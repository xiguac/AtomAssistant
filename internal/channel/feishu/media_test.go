package feishu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/media"
)

func strp(s string) *string { return &s }

func TestConvert_语音和视频按文件的办法取(t *testing.T) {
	b := &Bot{Label: "feishu"}
	ev := func(msgType, content string) *larkim.P2MessageReceiveV1 {
		return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: strp("ou_1")}, SenderType: strp("user")},
			Message: &larkim.EventMessage{MessageId: strp("om_1"), ChatType: strp("p2p"),
				MessageType: strp(msgType), Content: strp(content)},
		}}
	}
	m, ok := b.convert(ev("audio", `{"file_key":"file_v2_voice","duration":2000}`))
	if !ok || len(m.Voices) != 1 || m.Voices[0].Fetch == nil || m.Text != "" {
		t.Fatalf("语音消息不对: %+v %v", m, ok)
	}
	m, ok = b.convert(ev("media", `{"file_key":"file_v2_video","image_key":"img_v2_cover","file_name":"a.mp4","duration":3000}`))
	if !ok || len(m.Videos) != 1 || m.Videos[0].Fetch == nil {
		t.Fatalf("视频消息不对: %+v %v", m, ok)
	}
}

// fakeLark 模拟飞书开放平台：换凭证、上传图片和文件、发消息。
type fakeLark struct {
	srv       *httptest.Server
	fileTypes []string // 上传文件时的 file_type
	durations []string
	images    int
	sent      []map[string]any
}

func newFakeLark(t *testing.T) *fakeLark {
	f := &fakeLark{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "tenant_access_token"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","tenant_access_token":"t-1","expire":7200}`)
		case r.URL.Path == "/open-apis/im/v1/images":
			if err := r.ParseMultipartForm(32 << 20); err != nil || r.FormValue("image_type") != "message" {
				_, _ = io.WriteString(w, `{"code":1,"msg":"bad image upload"}`)
				return
			}
			f.images++
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"image_key":"img_v2_1"}}`)
		case r.URL.Path == "/open-apis/im/v1/files":
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				_, _ = io.WriteString(w, `{"code":1,"msg":"bad file upload"}`)
				return
			}
			f.fileTypes = append(f.fileTypes, r.FormValue("file_type"))
			f.durations = append(f.durations, r.FormValue("duration"))
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"file_key":"file_v2_1"}}`)
		case r.URL.Path == "/open-apis/im/v1/messages":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.sent = append(f.sent, body)
			_, _ = io.WriteString(w, `{"code":0,"msg":"success","data":{"message_id":"om_2"}}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLark) bot() *Bot {
	return &Bot{Label: "feishu", AppID: "cli_1", APIBase: f.srv.URL,
		Secret: func(context.Context) (string, error) { return "secret-1", nil }}
}

func (f *fakeLark) content(i int) map[string]string {
	var c map[string]string
	_ = json.Unmarshal([]byte(f.sent[i]["content"].(string)), &c)
	return c
}

func TestSend_图片按图片消息发其余文件按文件发(t *testing.T) {
	f := newFakeLark(t)
	b := f.bot()
	ctx := context.Background()
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 40)...)
	if err := b.Send(ctx, channel.Outgoing{Chat: "p2p:ou_abc", File: &channel.File{Name: "图.png", Data: png, Kind: media.KindImage}}); err != nil {
		t.Fatalf("发图片失败: %v", err)
	}
	if f.images != 1 || f.sent[0]["msg_type"] != "image" || f.content(0)["image_key"] != "img_v2_1" {
		t.Fatalf("图片应上传后按 image 发: %v", f.sent)
	}
	if err := b.Send(ctx, channel.Outgoing{Chat: "p2p:ou_abc", File: &channel.File{Name: "表.xlsx", Data: []byte("xlsx"), Kind: media.KindFile}}); err != nil {
		t.Fatalf("发文件失败: %v", err)
	}
	if f.fileTypes[0] != "stream" || f.sent[1]["msg_type"] != "file" || f.content(1)["file_key"] != "file_v2_1" {
		t.Fatalf("文件应按 stream 上传、按 file 发: %v %v", f.fileTypes, f.sent)
	}
	// 不是 mp4 的视频飞书不收：交给中枢改成按文件发。
	if err := b.Send(ctx, channel.Outgoing{Chat: "p2p:ou_abc", File: &channel.File{Name: "a.mov", Data: []byte("mov"), Kind: media.KindVideo}}); err != channel.ErrUnsupportedKind {
		t.Fatalf("不是 mp4 的视频应退回中枢: %v", err)
	}
}

func TestSend_语音转opus视频带封面和时长(t *testing.T) {
	if !media.Default.HasFFmpeg() {
		t.Skip("没有 ffmpeg，跳过")
	}
	f := newFakeLark(t)
	b := f.bot()
	ctx := context.Background()
	wav := media.WAV(make([]byte, 16000*2*2), 16000) // 2 秒静音
	if err := b.Send(ctx, channel.Outgoing{Chat: "p2p:ou_abc", File: &channel.File{Name: "录音.wav", Data: wav, Kind: media.KindAudio}}); err != nil {
		t.Fatalf("发语音失败: %v", err)
	}
	if f.fileTypes[0] != "opus" || f.durations[0] != "2000" || f.sent[0]["msg_type"] != "audio" || f.content(0)["file_key"] != "file_v2_1" {
		t.Fatalf("语音应转成 opus、带时长、按 audio 发: %v %v %v", f.fileTypes, f.durations, f.sent)
	}

	bin, _ := exec.LookPath("ffmpeg")
	out := filepath.Join(t.TempDir(), "v.mp4")
	if msg, err := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "mpeg4", "-c:a", "aac", "-shortest", out).CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v %s", err, msg)
	}
	video, _ := os.ReadFile(out)
	if err := b.Send(ctx, channel.Outgoing{Chat: "p2p:ou_abc", File: &channel.File{Name: "片段.mp4", Data: video, Kind: media.KindVideo}}); err != nil {
		t.Fatalf("发视频失败: %v", err)
	}
	c := f.content(1)
	if f.fileTypes[1] != "mp4" || f.durations[1] != "2000" || f.images != 1 || f.sent[1]["msg_type"] != "media" ||
		c["file_key"] != "file_v2_1" || c["image_key"] != "img_v2_1" {
		t.Fatalf("视频应带封面和时长、按 media 发: %v %v %v", f.fileTypes, f.durations, f.sent)
	}
}
