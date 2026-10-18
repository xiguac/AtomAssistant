package dingtalk

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"

	"github.com/xiguac/atomhelper/internal/channel"
	"github.com/xiguac/atomhelper/internal/media"
)

func TestConvert_语音和视频带下载码(t *testing.T) {
	b := &Bot{Label: "ding", AppID: "app-1"}
	m, ok := b.convert(&chatbot.BotCallbackDataModel{
		ConversationType: "1", SenderStaffId: "staff-7", MsgId: "msg-a", Msgtype: "audio",
		Content: map[string]any{"duration": 4000, "downloadCode": "code-voice", "recognition": "钉钉，让进步发生"},
	})
	// 平台给的识别文字不用：语音原样交给支持语音的模型。
	if !ok || len(m.Voices) != 1 || m.Voices[0].Fetch == nil || m.Text != "" {
		t.Fatalf("语音消息不对: %+v %v", m, ok)
	}
	m, ok = b.convert(&chatbot.BotCallbackDataModel{
		ConversationType: "1", SenderStaffId: "staff-7", MsgId: "msg-v", Msgtype: "video",
		Content: map[string]any{"duration": 4000, "downloadCode": "code-video", "videoType": "mp4"},
	})
	if !ok || len(m.Videos) != 1 || m.Videos[0].Fetch == nil {
		t.Fatalf("视频消息不对: %+v %v", m, ok)
	}
}

// fakeDing 模拟钉钉的新旧两套接口：换凭证、上传媒体文件、发单聊消息。
type fakeDing struct {
	srv     *httptest.Server
	uploads []string // 每次上传的 type 和文件名
	files   [][]byte
	sent    []map[string]string
	keys    []string
}

func newFakeDing(t *testing.T) *fakeDing {
	f := &fakeDing{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			_, _ = io.WriteString(w, `{"accessToken":"tok-new","expireIn":7200}`)
		case "/gettoken":
			if r.URL.Query().Get("appkey") != "app-1" || r.URL.Query().Get("appsecret") != "secret-1" {
				_, _ = io.WriteString(w, `{"errcode":40089,"errmsg":"invalid appkey"}`)
				return
			}
			_, _ = io.WriteString(w, `{"errcode":0,"access_token":"tok-old","expires_in":7200}`)
		case "/media/upload":
			if r.URL.Query().Get("access_token") != "tok-old" {
				_, _ = io.WriteString(w, `{"errcode":40014,"errmsg":"invalid token"}`)
				return
			}
			file, header, err := r.FormFile("media")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			data, _ := io.ReadAll(file)
			f.uploads = append(f.uploads, r.FormValue("type")+" "+header.Filename)
			f.files = append(f.files, data)
			_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok","media_id":"@media-`+r.FormValue("type")+`","type":"`+r.FormValue("type")+`"}`)
		case "/v1.0/robot/oToMessages/batchSend":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			var param map[string]string
			_ = json.Unmarshal([]byte(body["msgParam"].(string)), &param)
			f.keys = append(f.keys, body["msgKey"].(string))
			f.sent = append(f.sent, param)
			_, _ = io.WriteString(w, `{"processQueryKey":"q1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDing) bot() *Bot {
	return &Bot{Label: "ding", AppID: "app-1", APIBase: f.srv.URL, OAPIBase: f.srv.URL,
		Secret: func(context.Context) (string, error) { return "secret-1", nil }}
}

func TestSend_图片和文件先上传再按各自的模板发(t *testing.T) {
	f := newFakeDing(t)
	b := f.bot()
	ctx := context.Background()
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 40)...)

	if err := b.Send(ctx, channel.Outgoing{Chat: "dm:staff-7", File: &channel.File{Name: "图.png", Data: png, Kind: media.KindImage}}); err != nil {
		t.Fatalf("发图片失败: %v", err)
	}
	if f.uploads[0] != "image 图.png" || f.keys[0] != "sampleImageMsg" || f.sent[0]["photoURL"] != "@media-image" {
		t.Fatalf("图片应上传后按 sampleImageMsg 发: %v %v %v", f.uploads, f.keys, f.sent)
	}
	if err := b.Send(ctx, channel.Outgoing{Chat: "dm:staff-7", Text: "报告在这", File: &channel.File{Name: "周报.pdf", Data: []byte("%PDF-1.7"), Kind: media.KindFile}}); err != nil {
		t.Fatalf("发文件失败: %v", err)
	}
	if f.uploads[1] != "file 周报.pdf" || f.keys[1] != "sampleFile" || f.sent[1]["mediaId"] != "@media-file" ||
		f.sent[1]["fileName"] != "周报.pdf" || f.sent[1]["fileType"] != "pdf" {
		t.Fatalf("文件应上传后按 sampleFile 发: %v %v", f.keys, f.sent)
	}
	// 附带的话另发一条文字。
	if f.keys[2] != "sampleText" || f.sent[2]["content"] != "报告在这" {
		t.Fatalf("附带的话应另发: %v %v", f.keys, f.sent)
	}
	// 钉钉不收的文件类型：说清楚，不上传。
	before := len(f.uploads)
	err := b.Send(ctx, channel.Outgoing{Chat: "dm:staff-7", File: &channel.File{Name: "a.txt", Data: []byte("x"), Kind: media.KindFile}})
	if err == nil || !strings.Contains(err.Error(), "xlsx") || len(f.uploads) != before {
		t.Fatalf("钉钉不收的文件类型应报错且不上传: %v", err)
	}
	// 钉钉不收的图片格式（webp）：交给中枢改成按文件发。
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 20)...)
	if err := b.Send(ctx, channel.Outgoing{Chat: "dm:staff-7", File: &channel.File{Name: "图.webp", Data: webp, Kind: media.KindImage}}); err != channel.ErrUnsupportedKind {
		t.Fatalf("不收的图片格式应退回中枢: %v", err)
	}
}

func TestSend_语音转成amr并带毫秒时长(t *testing.T) {
	if !media.Default.HasFFmpeg() {
		t.Skip("没有 ffmpeg，跳过")
	}
	f := newFakeDing(t)
	b := f.bot()
	wav := media.WAV(make([]byte, 16000*2*2), 16000) // 2 秒静音
	if err := b.Send(context.Background(), channel.Outgoing{Chat: "dm:staff-7", File: &channel.File{Name: "录音.wav", Data: wav, Kind: media.KindAudio}}); err != nil {
		t.Fatalf("发语音失败: %v", err)
	}
	if f.uploads[0] != "voice 录音.amr" || media.Sniff(f.files[0]) != media.MIMEAMR || f.keys[0] != "sampleAudio" ||
		f.sent[0]["mediaId"] != "@media-voice" || f.sent[0]["duration"] != "2000" {
		t.Fatalf("语音应转成 amr 后按 sampleAudio 发: %v %v %v", f.uploads, f.keys, f.sent)
	}
}
