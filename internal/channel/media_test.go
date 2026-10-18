package channel

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/media"
)

func TestRoute_语音视频交给支持它们的模型(t *testing.T) {
	h, qq, tasks := hub(t)
	h = h.WithMedia(true, true, true)
	ctx := context.Background()
	wav := media.WAV(make([]byte, 16000*2), 16000)
	mp4 := append([]byte{0, 0, 0, 0x18}, []byte("ftypisom-video-body")...)
	h.fetch = func(_ context.Context, a Attachment) (Picture, error) {
		if strings.HasSuffix(a.URL, ".mp4") {
			return Picture{MIME: media.Sniff(mp4), Data: mp4}, nil
		}
		return Picture{MIME: media.Sniff(wav), Data: wav}, nil
	}

	// 只发了一段语音：建一个"处理语音"的任务，语音交给它（wav 模型认，原样给）。
	m := from("OWNER", "", "m1")
	m.Voices = []Attachment{{URL: "https://cdn.example.com/a.wav"}}
	h.Handle(ctx, "qq", m)
	if len(tasks.log) != 2 || !strings.HasPrefix(tasks.log[0], "create:处理语音|用户发来了语音") || tasks.log[1] != "attach:处理语音|1 张|qq" {
		t.Fatalf("只发语音应建任务并交给它: %v", tasks.log)
	}
	if got := tasks.attached[0]; len(got) != 1 || got[0].MIME != media.MIMEWAV {
		t.Fatalf("交给任务的应是模型认的格式: %+v", got)
	}

	// 视频配一句话：话是目标，视频单独交给任务。
	m = from("OWNER", "这段视频里说了什么", "m2")
	m.Videos = []Attachment{{URL: "https://cdn.example.com/b.mp4"}}
	h.Handle(ctx, "qq", m)
	if last := tasks.attached[len(tasks.attached)-1]; len(last) != 1 || last[0].MIME != media.MIMEMP4 {
		t.Fatalf("视频应原样交给任务: %+v", last)
	}

	// 太大的视频：说明原因，不收。
	before, sent := len(tasks.log), len(qq.sent)
	big := append([]byte{0, 0, 0, 0x18}, append([]byte("ftypisom"), make([]byte, maxMediaBytes)...)...)
	h.fetch = func(context.Context, Attachment) (Picture, error) { return Picture{Data: big}, nil }
	m = from("OWNER", "", "m3")
	m.Videos = []Attachment{{URL: "https://cdn.example.com/c.mp4"}}
	h.Handle(ctx, "qq", m)
	if len(tasks.log) != before || len(qq.sent) != sent+1 || !strings.Contains(qq.sent[sent].Text, "视频超过") {
		t.Fatalf("太大的视频应说明后不收: %v", tasks.log[before:])
	}
}

func TestRoute_没有模型收得了语音时直接告诉用户(t *testing.T) {
	h, qq, tasks := hub(t)
	h = h.WithMedia(true, false, false)
	ctx := context.Background()
	m := from("OWNER", "", "m1")
	m.Voices = []Attachment{{URL: "https://cdn.example.com/a.silk"}}
	m.Videos = []Attachment{{URL: "https://cdn.example.com/b.mp4"}}
	h.Handle(ctx, "qq", m)
	if len(tasks.log) != 0 || len(qq.sent) != 1 || qq.sent[0].Text != unreadable+"（语音、视频）。" {
		t.Fatalf("应直接回复看不懂，不建任务: %v %+v", tasks.log, qq.sent)
	}
}

// pickyAdapter 只能按文件发：别的类别一律说发不了。
type pickyAdapter struct {
	fakeAdapter
}

func (p *pickyAdapter) Send(ctx context.Context, m Outgoing) error {
	if m.File != nil && m.File.Kind != media.KindFile {
		return ErrUnsupportedKind
	}
	return p.fakeAdapter.Send(ctx, m)
}

func TestSendFile_按类别发不了就改成按文件发(t *testing.T) {
	wx := &pickyAdapter{fakeAdapter{name: "weixin"}}
	h, err := NewHub([]Binding{{Adapter: wx, Owners: []string{"ME"}}}, &fakeTasks{}, NewMemState(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := h.SendFile(ctx, []string{Tag("weixin", "u:ME")}, "录音.mp3", []byte("ID3-audio")); err != nil {
		t.Fatalf("应改成按文件发出去: %v", err)
	}
	if len(wx.sent) != 1 || wx.sent[0].File.Kind != media.KindFile || wx.sent[0].File.Name != "录音.mp3" {
		t.Fatalf("应按文件发: %+v", wx.sent)
	}

	// 能按类别发的直接发，类别由内容定。
	qq := &fakeAdapter{name: "qq"}
	h, _ = NewHub([]Binding{{Adapter: qq, Owners: []string{"ME"}}}, &fakeTasks{}, NewMemState(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 20)...)
	if err := h.SendFile(ctx, []string{Tag("qq", "c2c:ME")}, "chart.png", png); err != nil || len(qq.sent) != 1 || qq.sent[0].File.Kind != media.KindImage {
		t.Fatalf("图片应按图片发: %+v %v", qq.sent, err)
	}
}
