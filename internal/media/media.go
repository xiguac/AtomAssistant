// Package media 认出聊天里收发的语音、视频是什么格式，并在各家要求的格式之间转换。
//
// 各家的格式不一样：微信、QQ 的语音是 silk，飞书的语音是 opus，钉钉收的语音要 amr；
// 模型服务认的是 wav、mp3、amr 这类常见格式（见阿里云百炼的说明）。
//
// silk 用纯 Go 的编解码器处理（见 silk.go）。其余的转换交给服务器上的 ffmpeg：
// 没装 ffmpeg 时这些转换做不了，调用方据此退回（比如把语音当文件发、告诉用户收不了）。
package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Kind 是一个文件按聊天里怎么发来分的类别。
type Kind string

const (
	KindImage Kind = "image"
	KindAudio Kind = "audio"
	KindVideo Kind = "video"
	KindFile  Kind = "file"
)

var (
	// ErrNoFFmpeg 表示服务器上没有 ffmpeg，这一项转换做不了。
	ErrNoFFmpeg = errors.New("media: 服务器上没有安装 ffmpeg，无法转换音视频格式")
	// ErrUnknown 表示认不出是什么格式。
	ErrUnknown = errors.New("media: 认不出音频格式")
)

// 常见音频格式的 MIME 类型。silk 没有登记过的类型，用 audio/silk 自己约定。
const (
	MIMESilk = "audio/silk"
	MIMEAMR  = "audio/amr"
	MIMEOgg  = "audio/ogg"
	MIMEWAV  = "audio/wav"
	MIMEMP3  = "audio/mpeg"
	MIMEMP4A = "audio/mp4"
	MIMEMP4  = "video/mp4"
)

// Sniff 按内容认出格式，返回 MIME 类型。认不出的音频返回空串，其余的交给 http.DetectContentType。
func Sniff(data []byte) string {
	switch {
	case isSilk(data):
		return MIMESilk
	case bytes.HasPrefix(data, []byte("#!AMR")):
		return MIMEAMR
	case bytes.HasPrefix(data, []byte("OggS")):
		return MIMEOgg
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")):
		return MIMEWAV
	case bytes.HasPrefix(data, []byte("ID3")) || (len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0):
		return MIMEMP3
	case len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")):
		// MP4 容器：M4A 是只有音轨的那种。
		if brand := string(data[8:12]); brand == "M4A " || brand == "M4B " {
			return MIMEMP4A
		}
		return MIMEMP4
	}
	return http.DetectContentType(data)
}

// isSilk 认出 silk v3。腾讯的 silk 在标准文件头前面多一个字节 0x02。
func isSilk(data []byte) bool {
	return bytes.HasPrefix(data, []byte("#!SILK_V3")) || (len(data) > 10 && data[0] == 0x02 && bytes.HasPrefix(data[1:], []byte("#!SILK_V3")))
}

// 按扩展名认的类别：内容认不出（比如 opus、mov）时用。
var extKinds = map[string]Kind{
	".png": KindImage, ".jpg": KindImage, ".jpeg": KindImage, ".gif": KindImage, ".webp": KindImage, ".bmp": KindImage,
	".mp3": KindAudio, ".wav": KindAudio, ".amr": KindAudio, ".silk": KindAudio, ".ogg": KindAudio, ".opus": KindAudio, ".m4a": KindAudio, ".aac": KindAudio,
	".mp4": KindVideo, ".mov": KindVideo, ".m4v": KindVideo,
}

// KindOf 定出一个文件发到聊天里时算哪一类：图片、语音、视频，其余的都按文件发。
func KindOf(name string, data []byte) Kind {
	mime := Sniff(data)
	switch {
	case strings.HasPrefix(mime, "image/"):
		return KindImage
	case strings.HasPrefix(mime, "audio/"):
		return KindAudio
	case mime == MIMEMP4:
		return KindVideo
	}
	if k, ok := extKinds[strings.ToLower(filepath.Ext(name))]; ok {
		return k
	}
	return KindFile
}

// Codec 做格式转换。FFmpeg 是 ffmpeg 程序的路径，为空时在 PATH 里找。
type Codec struct {
	FFmpeg string
	// Timeout 是一次转换最长多久，为空用一分钟。
	Timeout time.Duration
}

// Default 是在 PATH 里找 ffmpeg 的转换器。
var Default = &Codec{}

func (c *Codec) ffmpeg() (string, error) {
	if c.FFmpeg != "" {
		return c.FFmpeg, nil
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", ErrNoFFmpeg
	}
	return path, nil
}

// HasFFmpeg 报告 ffmpeg 能不能用。
func (c *Codec) HasFFmpeg() bool {
	_, err := c.ffmpeg()
	return err == nil
}

func (c *Codec) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return time.Minute
}

// run 用 ffmpeg 把 in 转成 outExt 格式的文件并读回来。输入输出都经临时文件：
// MP4 这类容器要能前后跳着读，管道做不到。
func (c *Codec) run(ctx context.Context, in []byte, outExt string, args ...string) ([]byte, error) {
	bin, err := c.ffmpeg()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "atomhelper-media-")
	if err != nil {
		return nil, fmt.Errorf("media: 建临时目录失败: %w", err)
	}
	defer os.RemoveAll(dir)
	src, dst := filepath.Join(dir, "in"), filepath.Join(dir, "out"+outExt)
	if err := os.WriteFile(src, in, 0o600); err != nil {
		return nil, fmt.Errorf("media: 写临时文件失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y", "-i", src}, args...)
	full = append(full, dst)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("media: ffmpeg 转换失败: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		return nil, fmt.Errorf("media: 读转换结果失败: %w", err)
	}
	return out, nil
}

// PCM 把一段语音解成 16 位、单声道、rate 采样率的原始采样。silk 自己解，其余的交给 ffmpeg。
func (c *Codec) PCM(ctx context.Context, audio []byte, rate int) ([]byte, error) {
	if isSilk(audio) {
		return decodeSilk(audio, rate)
	}
	return c.run(ctx, audio, ".pcm", "-vn", "-ac", "1", "-ar", strconv.Itoa(rate), "-f", "s16le")
}

// ForModel 把用户发来的语音换成模型服务认的格式，返回内容和 MIME 类型。
//
// wav、mp3、amr 原样给；silk 解成 wav；其余的（opus、m4a 等）用 ffmpeg 转成 wav。
func (c *Codec) ForModel(ctx context.Context, audio []byte) ([]byte, string, error) {
	switch mime := Sniff(audio); mime {
	case MIMEWAV, MIMEMP3, MIMEAMR:
		return audio, mime, nil
	}
	const rate = 16000
	pcm, err := c.PCM(ctx, audio, rate)
	if err != nil {
		return nil, "", err
	}
	return WAV(pcm, rate), MIMEWAV, nil
}

// Silk 把一段语音编成腾讯的 silk（QQ 发语音要它），同时返回时长（毫秒）。
func (c *Codec) Silk(ctx context.Context, audio []byte) ([]byte, int, error) {
	if isSilk(audio) {
		pcm, err := decodeSilk(audio, silkRate)
		if err != nil {
			return nil, 0, err
		}
		return audio, durationMS(pcm, silkRate), nil
	}
	pcm, err := c.PCM(ctx, audio, silkRate)
	if err != nil {
		return nil, 0, err
	}
	out, err := encodeSilk(pcm, silkRate)
	if err != nil {
		return nil, 0, err
	}
	return out, durationMS(pcm, silkRate), nil
}

// Opus 把一段语音编成飞书要的 opus（ogg 封装、单声道 16k，参数照飞书文档里给的 ffmpeg 命令），同时返回时长（毫秒）。
func (c *Codec) Opus(ctx context.Context, audio []byte) ([]byte, int, error) {
	const rate = 16000
	pcm, err := c.PCM(ctx, audio, rate)
	if err != nil {
		return nil, 0, err
	}
	out, err := c.run(ctx, WAV(pcm, rate), ".opus", "-acodec", "libopus", "-ac", "1", "-ar", strconv.Itoa(rate))
	if err != nil {
		return nil, 0, err
	}
	return out, durationMS(pcm, rate), nil
}

// AMR 把一段语音编成钉钉要的 amr（窄带，8k 单声道），同时返回时长（毫秒）。
func (c *Codec) AMR(ctx context.Context, audio []byte) ([]byte, int, error) {
	const rate = 8000
	pcm, err := c.PCM(ctx, audio, rate)
	if err != nil {
		return nil, 0, err
	}
	out, err := c.run(ctx, WAV(pcm, rate), ".amr", "-c:a", "libopencore_amrnb", "-ac", "1", "-ar", strconv.Itoa(rate), "-b:a", "12.2k")
	if err != nil {
		return nil, 0, err
	}
	return out, durationMS(pcm, rate), nil
}

// Cover 从视频里取第一秒附近的一帧做封面，JPEG 格式。
func (c *Codec) Cover(ctx context.Context, video []byte) ([]byte, error) {
	return c.run(ctx, video, ".jpg", "-ss", "0", "-frames:v", "1", "-q:v", "4")
}

// VideoSeconds 量出一段视频有多少秒（四舍五入：音轨的编码会多出一点点开头的填充）。
// 用 ffmpeg 把音轨解出来数采样：不另依赖 ffprobe。
// 没有音轨的视频量不出来，返回 0。
func (c *Codec) VideoSeconds(ctx context.Context, video []byte) (int, error) {
	const rate = 8000
	pcm, err := c.run(ctx, video, ".pcm", "-vn", "-ac", "1", "-ar", strconv.Itoa(rate), "-f", "s16le")
	if err != nil {
		return 0, err
	}
	ms := durationMS(pcm, rate)
	return (ms + 500) / 1000, nil
}

func durationMS(pcm []byte, rate int) int {
	return len(pcm) / 2 * 1000 / rate
}

// WAV 给 16 位单声道的原始采样加上 WAV 文件头。
func WAV(pcm []byte, rate int) []byte {
	var buf bytes.Buffer
	buf.Grow(44 + len(pcm))
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // 单声道
	_ = binary.Write(&buf, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}
