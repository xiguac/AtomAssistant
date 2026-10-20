package channel

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeASR 按语音内容给出事先定好的文字。
type fakeASR struct {
	texts map[string]string
	err   error
	heard []string
}

func (f *fakeASR) Transcribe(_ context.Context, audio []byte) (string, error) {
	f.heard = append(f.heard, string(audio))
	return f.texts[string(audio)], f.err
}

// voiceHub 是一个接了语音识别的中枢：语音按地址取回，内容就是地址本身。
func voiceHub(t *testing.T, asr *fakeASR) (*Hub, *fakeAdapter, *fakeTasks) {
	h, qq, tasks := hub(t)
	h = h.WithASR(asr)
	h.fetch = func(_ context.Context, a Attachment) (Picture, error) { return Picture{Data: []byte(a.URL)}, nil }
	return h, qq, tasks
}

func TestRoute_语音转成文字和打字一样处理(t *testing.T) {
	asr := &fakeASR{texts: map[string]string{"v1": "明天上午十点提醒我开会"}}
	h, _, tasks := voiceHub(t, asr)
	m := from("OWNER", "", "m1")
	m.Voices = []Attachment{{URL: "v1"}}
	h.Handle(context.Background(), "qq", m)
	// 标题是转出来的话；目标前面带着说明，告诉模型这是语音转的。不交给任务任何语音。
	want := "create:明天上午十点提醒我开会|" + voiceNote + "明天上午十点提醒我开会"
	if len(tasks.log) != 1 || tasks.log[0] != want || len(tasks.attached) != 0 {
		t.Fatalf("语音应转成文字建任务: %q", tasks.log)
	}
}

func TestRoute_语音说同意也放行(t *testing.T) {
	asr := &fakeASR{texts: map[string]string{"v1": "好的"}}
	h, _, tasks := voiceHub(t, asr)
	task := tasks.add("回邮件", Tag("qq", "c2c:OWNER"))
	awaitApproval(tasks, task, "a1", "mail.send", `{}`, "等你确认")
	m := from("OWNER", "", "m1")
	m.Voices = []Attachment{{URL: "v1"}}
	h.Handle(context.Background(), "qq", m)
	if len(tasks.log) != 1 || tasks.log[0] != "approve:回邮件" {
		t.Fatalf("语音说的同意应放行: %v", tasks.log)
	}
}

func TestRoute_语音转不成时告诉用户(t *testing.T) {
	ctx := context.Background()

	// 没有配置语音识别。
	h, qq, tasks := hub(t)
	m := from("OWNER", "", "m1")
	m.Voices = []Attachment{{URL: "v1"}}
	h.Handle(ctx, "qq", m)
	if len(tasks.log) != 0 || len(qq.sent) != 1 || !strings.Contains(qq.sent[0].Text, "还没有配置语音识别") {
		t.Fatalf("没配置时应告诉用户: %v %+v", tasks.log, qq.sent)
	}

	// 识别服务出错：告诉用户，不建任务。
	h, qq, tasks = voiceHub(t, &fakeASR{err: errors.New("连不上")})
	h.Handle(ctx, "qq", m)
	if len(tasks.log) != 0 || len(qq.sent) != 1 || !strings.Contains(qq.sent[0].Text, "没转成文字") {
		t.Fatalf("出错时应告诉用户: %v %+v", tasks.log, qq.sent)
	}

	// 听不出内容：告诉用户，不建任务。
	h, qq, tasks = voiceHub(t, &fakeASR{})
	h.Handle(ctx, "qq", m)
	if len(tasks.log) != 0 || len(qq.sent) != 1 || !strings.Contains(qq.sent[0].Text, "没听出内容") {
		t.Fatalf("听不出内容时应告诉用户: %v %+v", tasks.log, qq.sent)
	}

	// 同时打了字：字照常办。
	h, _, tasks = voiceHub(t, &fakeASR{err: errors.New("连不上")})
	m = from("OWNER", "查一下天气", "m2")
	m.Voices = []Attachment{{URL: "v1"}}
	h.Handle(ctx, "qq", m)
	if len(tasks.log) != 1 || tasks.log[0] != "create:查一下天气|查一下天气" {
		t.Fatalf("打的字应照常办: %v", tasks.log)
	}
}
