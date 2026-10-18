package media

import (
	"bytes"
	"context"
	"math"
	"testing"
)

// tone 生成 rate 采样率、ms 毫秒长的一段正弦波，16 位单声道。
func tone(rate, ms int) []byte {
	n := rate * ms / 1000
	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		buf.WriteByte(byte(v))
		buf.WriteByte(byte(v >> 8))
	}
	return buf.Bytes()
}

func TestSilk_编出来的是腾讯格式能解回来(t *testing.T) {
	pcm := tone(silkRate, 1000)
	enc, err := encodeSilk(pcm, silkRate)
	if err != nil {
		t.Fatal(err)
	}
	if !isSilk(enc) || enc[0] != 0x02 {
		t.Fatalf("应是带 0x02 前缀的腾讯 silk: %q", enc[:12])
	}
	dec, err := decodeSilk(enc, silkRate)
	if err != nil {
		t.Fatal(err)
	}
	// 编解码有帧对齐，长度大致相同即可。
	if ms := durationMS(dec, silkRate); ms < 900 || ms > 1100 {
		t.Fatalf("解回来的时长不对: %d ms", ms)
	}
	// 解成别的采样率也行：给模型的是 16k。
	if dec16, err := decodeSilk(enc, 16000); err != nil || durationMS(dec16, 16000) < 900 {
		t.Fatalf("解成 16k 失败: %v", err)
	}
}

func TestSniff_认出常见的语音视频格式(t *testing.T) {
	enc, err := encodeSilk(tone(silkRate, 200), silkRate)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		MIMESilk: enc,
		MIMEAMR:  []byte("#!AMR\n\x3c\x00"),
		MIMEOgg:  []byte("OggS\x00\x02\x00\x00"),
		MIMEWAV:  WAV(tone(8000, 10), 8000),
		MIMEMP3:  []byte("ID3\x04\x00\x00\x00"),
		MIMEMP4:  []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00"),
		MIMEMP4A: []byte("\x00\x00\x00\x18ftypM4A \x00\x00\x02\x00"),
	}
	for want, data := range cases {
		if got := Sniff(data); got != want {
			t.Fatalf("应认出 %s，实际 %s", want, got)
		}
	}
	for name, want := range map[string]Kind{
		"a.png": KindImage, "语音.silk": KindAudio, "x.opus": KindAudio, "片段.mov": KindVideo, "报告.pdf": KindFile,
	} {
		if got := KindOf(name, []byte("not sniffable")); got != want {
			t.Fatalf("%s 应算 %s，实际 %s", name, want, got)
		}
	}
	// 内容优先于扩展名。
	if KindOf("说明.txt", cases[MIMEMP4]) != KindVideo {
		t.Fatal("内容是 MP4 就按视频算")
	}
}

func TestForModel_模型认的原样给其余的转成WAV(t *testing.T) {
	ctx := context.Background()
	c := &Codec{FFmpeg: "no-such-ffmpeg-binary"}
	wav := WAV(tone(16000, 100), 16000)
	if out, mime, err := c.ForModel(ctx, wav); err != nil || mime != MIMEWAV || !bytes.Equal(out, wav) {
		t.Fatalf("WAV 应原样给: %s %v", mime, err)
	}
	// silk 不用 ffmpeg 也能转。
	enc, _ := encodeSilk(tone(silkRate, 500), silkRate)
	out, mime, err := c.ForModel(ctx, enc)
	if err != nil || mime != MIMEWAV || Sniff(out) != MIMEWAV {
		t.Fatalf("silk 应转成 WAV: %s %v", mime, err)
	}
	// opus 要 ffmpeg：没有时说清楚。
	if _, _, err := c.ForModel(ctx, []byte("OggS\x00\x02\x00\x00rest")); err == nil {
		t.Fatal("没有 ffmpeg 时转不了 opus，应报错")
	}
	// QQ 发语音：silk 原样用，量出时长。
	if got, ms, err := c.Silk(ctx, enc); err != nil || !bytes.Equal(got, enc) || ms < 400 || ms > 600 {
		t.Fatalf("已经是 silk 的应原样用并量出时长: %d %v", ms, err)
	}
}
