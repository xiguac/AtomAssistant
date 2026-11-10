package channel

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// ownerAdapter 是能算出主人私聊会话的渠道（像 QQ 那样）。
type ownerAdapter struct{ fakeAdapter }

func (o *ownerAdapter) OwnerChat(owner string) string { return "c2c:" + owner }

func injectHub(t *testing.T, owners []string) (*Hub, *fakeTasks, *ownerAdapter) {
	t.Helper()
	qq := &ownerAdapter{fakeAdapter{name: "qq"}}
	tasks := &fakeTasks{}
	h, err := NewHub([]Binding{{Adapter: qq, Owners: owners}}, tasks, NewMemState(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h.runAsync = func(f func()) { f() }
	return h, tasks, qq
}

func TestInjectOwner_像主人亲自在渠道里发的一样建任务并沿用最近的来信编号(t *testing.T) {
	h, tasks, _ := injectHub(t, []string{"OWNER"})
	ctx := context.Background()
	// 主人先在 QQ 里真的说过话，留下了最近的来信编号。
	if err := h.state.PutChat(ctx, &ChatState{Channel: "qq", Chat: "c2c:OWNER", LastMsgID: "real-msg-1"}); err != nil {
		t.Fatal(err)
	}

	if err := h.InjectOwner(ctx, "qq", "帮我看下今天的日程", nil); err != nil {
		t.Fatal(err)
	}
	if len(tasks.log) != 1 || !strings.HasPrefix(tasks.log[0], "create:") {
		t.Fatalf("应当建成任务: %v", tasks.log)
	}
	// 任务记下它来自主人的私聊：进展会推回那里。
	if got := tasks.tasks[0].Tags; len(got) != 2 || got[0] != "chat:qq/c2c:OWNER" {
		t.Errorf("任务应记下主人的私聊会话: %v", got)
	}
	// 来信编号沿用真实的那条，没有被编造的值覆盖。
	st, _ := h.state.GetChat(ctx, "qq", "c2c:OWNER")
	if st == nil || st.LastMsgID != "real-msg-1" {
		t.Errorf("最近来信编号 = %+v，想要沿用 real-msg-1", st)
	}
}

func TestInjectOwner_带截屏时图片交给任务(t *testing.T) {
	h, tasks, _ := injectHub(t, []string{"OWNER"})
	img := Attachment{ContentType: "image/jpeg", Fetch: func(context.Context) (Picture, error) {
		return Picture{MIME: "image/jpeg", Data: []byte("jpegbytes")}, nil
	}}
	if err := h.InjectOwner(context.Background(), "qq", "这个页面是什么意思", []Attachment{img}); err != nil {
		t.Fatal(err)
	}
	if len(tasks.attached) != 1 || len(tasks.attached[0]) != 1 || string(tasks.attached[0][0].Data) != "jpegbytes" {
		t.Errorf("截屏应当交给任务: %+v", tasks.attached)
	}
}

func TestInjectOwner_配置有问题时立即报错(t *testing.T) {
	ctx := context.Background()
	h, _, _ := injectHub(t, []string{"OWNER"})
	if err := h.InjectOwner(ctx, "feishu", "hi", nil); err == nil || !strings.Contains(err.Error(), "没有名为") {
		t.Errorf("没有这个渠道: %v", err)
	}
	h2, _, _ := injectHub(t, nil)
	if err := h2.InjectOwner(ctx, "qq", "hi", nil); err == nil || !strings.Contains(err.Error(), "没有绑定主人") {
		t.Errorf("没绑主人: %v", err)
	}
	// 不支持从手机发起的渠道。
	plain := &fakeAdapter{name: "plain"}
	h3, err := NewHub([]Binding{{Adapter: plain, Owners: []string{"X"}}}, &fakeTasks{}, NewMemState(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := h3.InjectOwner(ctx, "plain", "hi", nil); err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Errorf("不支持的渠道: %v", err)
	}
}
