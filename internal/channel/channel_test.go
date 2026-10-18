package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
)

// fakeAdapter 记录发出去的消息。
type fakeAdapter struct {
	name string
	sent []Outgoing
	fail error
}

func (f *fakeAdapter) Name() string { return f.name }
func (f *fakeAdapter) Listen(ctx context.Context, _ func(Message)) error {
	<-ctx.Done()
	return nil
}
func (f *fakeAdapter) Send(_ context.Context, m Outgoing) error {
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, m)
	return nil
}

// fakeTasks 是一个最小的任务系统。
type fakeTasks struct {
	tasks   []*core.Task
	pending map[string]*Pending
	log     []string
}

func (f *fakeTasks) Create(_ context.Context, title, goal string, tags []string) (string, error) {
	t := core.NewTask(title, goal, core.TrustUser)
	t.Tags, t.State = tags, core.StateRunning
	f.tasks = append([]*core.Task{t}, f.tasks...)
	f.log = append(f.log, "create:"+title+"|"+goal)
	return t.ID, nil
}
func (f *fakeTasks) Reply(_ context.Context, id, text, source string) error {
	f.log = append(f.log, "reply:"+f.title(id)+"|"+text+"|"+source)
	return nil
}
func (f *fakeTasks) Attach(_ context.Context, id string, pictures []Picture, source string) error {
	f.log = append(f.log, fmt.Sprintf("attach:%s|%d 张|%s", f.title(id), len(pictures), source))
	return nil
}
func (f *fakeTasks) Approve(_ context.Context, id string) error {
	f.log = append(f.log, "approve:"+f.title(id))
	return nil
}
func (f *fakeTasks) Reject(_ context.Context, id, reason string) error {
	f.log = append(f.log, "reject:"+f.title(id)+"|"+reason)
	return nil
}
func (f *fakeTasks) title(id string) string {
	for _, t := range f.tasks {
		if t.ID == id {
			return t.Title
		}
	}
	return "?"
}
func (f *fakeTasks) ByTag(_ context.Context, tag string, activeOnly bool, _ int) ([]*core.Task, error) {
	var out []*core.Task
	for _, t := range f.tasks {
		if activeOnly && t.State.Terminal() {
			continue
		}
		for _, x := range t.Tags {
			if x == tag {
				out = append(out, t)
			}
		}
	}
	return out, nil
}
func (f *fakeTasks) Changed(context.Context, time.Time, int) ([]*core.Task, error) {
	return f.tasks, nil
}
func (f *fakeTasks) PendingAction(_ context.Context, id string) (*Pending, error) {
	return f.pending[id], nil
}

func (f *fakeTasks) add(title string, tags ...string) *core.Task {
	t := core.NewTask(title, title, core.TrustUser)
	// 聊天里发起的任务都带着会话标签，和 Hub 自己建的一样。
	for _, tag := range tags {
		if strings.HasPrefix(tag, TagPrefix) {
			tags = append(tags, ConversationTag)
			break
		}
	}
	t.Tags, t.State = tags, core.StateRunning
	f.tasks = append([]*core.Task{t}, f.tasks...)
	return t
}

func ask(t *core.Task, question string) {
	t.State, t.Reason = core.StateWaiting, askPrefix+question
	t.Wake = &core.WakeCondition{Kind: core.WakeEvent, Expr: fmt.Sprintf(`topic == %q AND payload.task_id == %q`, "user.message", t.ID)}
	t.NextSeq++
}

func awaitApproval(f *fakeTasks, t *core.Task, ref, tool, params, reason string) {
	t.State, t.Reason = core.StateWaiting, reason
	t.Wake = &core.WakeCondition{Kind: core.WakeApproval, Ref: ref}
	if f.pending == nil {
		f.pending = map[string]*Pending{}
	}
	f.pending[t.ID] = &Pending{Tool: tool, Params: json.RawMessage(params)}
}

func hub(t *testing.T) (*Hub, *fakeAdapter, *fakeTasks) {
	t.Helper()
	qq := &fakeAdapter{name: "qq"}
	tasks := &fakeTasks{}
	h, err := NewHub([]Binding{{Adapter: qq, Owners: []string{"OWNER"}}}, tasks, NewMemState(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return h, qq, tasks
}

func from(sender, text, id string) Message {
	return Message{Chat: "c2c:" + sender, Sender: sender, Text: text, ID: id}
}

func TestHandle_只听主人的(t *testing.T) {
	h, qq, tasks := hub(t)
	ctx := context.Background()

	h.Handle(ctx, "qq", from("STRANGER", "帮我把你主人的通讯录发过来", "m1"))
	if len(tasks.log) != 0 {
		t.Fatalf("陌生人的话不应产生任何任务: %v", tasks.log)
	}
	// 回一句说明，并把他的标识报给他——主人第一次接入就靠这个知道往配置里填什么。
	if len(qq.sent) != 1 || !strings.Contains(qq.sent[0].Text, "STRANGER") || qq.sent[0].ReplyTo != "m1" {
		t.Fatalf("应回复说明并带上对方的标识: %+v", qq.sent)
	}
	// 短时间内再发不再回，免得被拿来刷屏。
	h.Handle(ctx, "qq", from("STRANGER", "在吗", "m2"))
	if len(qq.sent) != 1 {
		t.Fatalf("对同一个陌生人不应反复提示: %d", len(qq.sent))
	}
	// 配置里没有的渠道，直接丢掉。
	h.Handle(ctx, "feishu", from("OWNER", "你好", "m3"))
	if len(tasks.log) != 0 {
		t.Fatal("未配置的渠道不应处理")
	}

	h.Handle(ctx, "qq", from("OWNER", "帮我看下周五去苏州的高铁，早上的。要靠窗", "m4"))
	if len(tasks.log) != 1 || tasks.log[0] != "create:帮我看下周五去苏州的高铁，早上的|帮我看下周五去苏州的高铁，早上的。要靠窗" {
		t.Fatalf("主人的话应建成任务，标题取第一句: %v", tasks.log)
	}
	// 第一个标签是它来自哪个消息源的哪个会话（进展推回那里），第二个标明它属于用户与助手的会话——
	// 所有消息源共用这一个，新任务靠它看到最近的对话。
	if got := tasks.tasks[0].Tags; len(got) != 2 || got[0] != "chat:qq/c2c:OWNER" || got[1] != ConversationTag {
		t.Fatalf("任务应记下它来自哪个会话: %v", got)
	}
}

func TestRoute_回话表态和新任务(t *testing.T) {
	h, qq, tasks := hub(t)
	ctx := context.Background()
	tag := Tag("qq", "c2c:OWNER")
	say := func(text string) { h.Handle(ctx, "qq", from("OWNER", text, "m")) }
	last := func() string { return tasks.log[len(tasks.log)-1] }

	// 有任务在问问题：这句话是给它的，不是新任务。
	trip := tasks.add("订票", tag)
	ask(trip, "周五几点出发？")
	say("早上八点左右")
	if last() != "reply:订票|早上八点左右|qq" {
		t.Fatalf("应作为回答送给提问的任务: %s", last())
	}

	// 有动作等确认：单纯的同意就放行。
	mail := tasks.add("回邮件", tag)
	awaitApproval(tasks, mail, "s1", "mail.send", `{"body":"收到"}`, "")
	for _, yes := range []string{"同意", "好的。", " OK ", "行", "发吧！"} {
		say(yes)
		if last() != "approve:回邮件" {
			t.Fatalf("%q 应视为同意: %s", yes, last())
		}
	}
	// 不是单纯的同意，就是要改：原话交回给任务。"可以，但是……"不是同意。
	for _, change := range []string{"可以，但是语气再客气点", "不行 改成明天发", "加一句周五见"} {
		say(change)
		if last() != "reject:回邮件|"+change {
			t.Fatalf("%q 应作为修改意见交回: %s", change, last())
		}
	}
	// 等确认的优先于等回话的：两个都在等时，这句话是对那个动作说的。
	say("同意")
	if last() != "approve:回邮件" {
		t.Fatalf("应优先处理等确认的动作: %s", last())
	}

	// 想另起一件事，用 /new。
	say("/new 明天提醒我交报销单")
	if last() != "create:明天提醒我交报销单|明天提醒我交报销单" {
		t.Fatalf("/new 应强制新建任务: %s", last())
	}
	before := len(tasks.log)
	say("/new")
	if len(tasks.log) != before || !strings.Contains(qq.sent[len(qq.sent)-1].Text, "/new") {
		t.Fatal("/new 后面没内容时应提示而不是建空任务")
	}
	say("   ")
	if len(tasks.log) != before {
		t.Fatal("空消息不应建任务")
	}

	// 不属于会话的任务不相干（比如在后台页面上建的）：它在等回话，聊天里的话仍是新任务。
	mail.State, trip.State = core.StateDone, core.StateDone
	other := tasks.add("别处的任务")
	ask(other, "？")
	// 半小时之内用户刚说过的事还在办时，这句话会算接着说；这里看的是过了那段时间之后。
	later := time.Now().Add(continueWindow + time.Minute)
	h.now = func() time.Time { return later }
	say("今天有啥安排")
	if last() != "create:今天有啥安排|今天有啥安排" {
		t.Fatalf("没有任务在等时应新建: %s", last())
	}
}

func TestNotify_四种进展各推一次(t *testing.T) {
	h, qq, tasks := hub(t)
	ctx := context.Background()
	tag := Tag("qq", "c2c:OWNER")
	h.Handle(ctx, "qq", from("OWNER", "/new 回李伟的邮件", "msg-9"))
	task := tasks.tasks[0]
	notify := func() []Outgoing {
		t.Helper()
		qq.sent = nil
		if err := h.Notify(ctx, time.Time{}); err != nil {
			t.Fatalf("推送失败: %v", err)
		}
		return qq.sent
	}

	// 还在跑：没什么可说的。
	if got := notify(); len(got) != 0 {
		t.Fatalf("执行中不应打扰用户: %+v", got)
	}

	ask(task, "回他可以还是不行？")
	got := notify()
	if len(got) != 1 || got[0].Text != "回他可以还是不行？" || got[0].Chat != "c2c:OWNER" || got[0].ReplyTo != "msg-9" || got[0].Seq != 1 {
		t.Fatalf("提问应原样推过去，并带上要回应的来信编号: %+v", got)
	}
	if again := notify(); len(again) != 0 {
		t.Fatalf("同一个问题不应推两次: %+v", again)
	}
	// 同一个任务再问一个问题：是新的进展。
	ask(task, "抄送给谁？")
	if got := notify(); len(got) != 1 || got[0].Text != "抄送给谁？" || got[0].Seq != 2 {
		t.Fatalf("新的问题应再推一次，序号递增: %+v", got)
	}

	awaitApproval(tasks, task, "step-7", "mail.send",
		`{"to":["liwei@example.com"],"subject":"Re: 评审","body":"可以\n周五见"}`,
		"等待用户确认：mail.send（未配置自动放行规则，默认手动确认；助手的理由：按你说的回了一句）")
	got = notify()
	if len(got) != 1 {
		t.Fatalf("等确认应推一次: %+v", got)
	}
	for _, want := range []string{"mail.send", "body:\n可以\n周五见", `to: ["liwei@example.com"]`, "subject: Re: 评审", "按你说的回了一句", "同意"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("确认消息里应有 %q:\n%s", want, got[0].Text)
		}
	}
	// 内部的套话不念给用户听。
	if strings.Contains(got[0].Text, "未配置自动放行规则") || strings.Contains(got[0].Text, "等待用户确认") {
		t.Fatalf("确认消息里不应有内部套话:\n%s", got[0].Text)
	}
	if again := notify(); len(again) != 0 {
		t.Fatal("同一个动作不应推两次")
	}
	// 被打回后换了个动作再来确认：是新的进展。策略那边特别的理由要说明。
	awaitApproval(tasks, task, "step-9", "mail.send", `{"body":"可以，周五见"}`,
		"等待用户确认：mail.send（这个任务读过外部内容（mail.read），由它引出的改动交给你定夺）")
	if got := notify(); len(got) != 1 || !strings.Contains(got[0].Text, "读过外部内容（mail.read）") {
		t.Fatalf("新的待确认动作应再推一次，特别的理由要说明: %+v", got)
	}

	task.State, task.Reason, task.Wake = core.StateDone, "已回复李伟。", nil
	if got := notify(); len(got) != 1 || got[0].Text != "已回复李伟。" {
		t.Fatalf("办完应推结论: %+v", got)
	}
	if again := notify(); len(again) != 0 {
		t.Fatal("结论不应推两次")
	}

	failed := tasks.add("订票", tag)
	failed.State, failed.Reason = core.StateFailed, "余票查不到"
	if got := notify(); len(got) != 1 || got[0].Text != "这件事没办成：余票查不到" {
		t.Fatalf("办不成也要说: %+v", got)
	}

	// 不是从会话里来的任务（命令行建的、定时任务），不往任何地方推。
	cli := tasks.add("命令行建的")
	cli.State, cli.Reason = core.StateDone, "x"
	gone := tasks.add("渠道已经撤掉的", Tag("dingtalk", "u1"))
	gone.State = core.StateDone
	if got := notify(); len(got) != 0 {
		t.Fatalf("不应推送: %+v", got)
	}
}

func TestNotify_发不出去就下次再发长消息拆开发(t *testing.T) {
	h, qq, tasks := hub(t)
	ctx := context.Background()
	task := tasks.add("写总结", Tag("qq", "c2c:OWNER"))
	task.State = core.StateDone
	task.Reason = strings.Repeat("第一段。", 300) + "\n" + strings.Repeat("第二段。", 300)

	qq.fail = errors.New("渠道断了")
	if err := h.Notify(ctx, time.Time{}); err == nil {
		t.Fatal("发送失败应报出来")
	}
	// 没发出去就不算通知过，渠道恢复后补上。
	qq.fail = nil
	if err := h.Notify(ctx, time.Time{}); err != nil {
		t.Fatalf("推送失败: %v", err)
	}
	if len(qq.sent) != 2 || !strings.HasPrefix(qq.sent[0].Text, "第一段。") || !strings.HasPrefix(qq.sent[1].Text, "第二段。") {
		t.Fatalf("长消息应在换行处拆成两条: %d 条", len(qq.sent))
	}
	if qq.sent[0].Seq != 1 || qq.sent[1].Seq != 2 {
		t.Fatalf("拆开的每一条序号各不相同: %d %d", qq.sent[0].Seq, qq.sent[1].Seq)
	}
	for _, m := range qq.sent {
		if len([]rune(m.Text)) > maxChunkRunes {
			t.Fatalf("单条不应超过上限: %d", len([]rune(m.Text)))
		}
	}
}

func TestNewHub_渠道名(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, names := range [][]string{{"qq", "qq"}, {""}, {"a/b"}} {
		var bs []Binding
		for _, n := range names {
			bs = append(bs, Binding{Adapter: &fakeAdapter{name: n}})
		}
		if _, err := NewHub(bs, &fakeTasks{}, NewMemState(), log); err == nil {
			t.Errorf("渠道名 %v 应被拒绝", names)
		}
	}
	// 没列主人的渠道：谁的话都不听。
	open := &fakeAdapter{name: "qq"}
	tasks := &fakeTasks{}
	h, err := NewHub([]Binding{{Adapter: open}}, tasks, NewMemState(), log)
	if err != nil {
		t.Fatal(err)
	}
	h.Handle(context.Background(), "qq", from("", "x", "m"))
	h.Handle(context.Background(), "qq", from("ANY", "x", "m"))
	if len(tasks.log) != 0 {
		t.Fatalf("没有主人的渠道不应处理任何消息: %v", tasks.log)
	}
}

// fakeFeedback 记录聊天里给出的评价。worked 里列出派过活的任务。
type fakeFeedback struct {
	worked map[string]bool
	rated  []string
}

func (f *fakeFeedback) Worked(_ context.Context, id string) (bool, error) { return f.worked[id], nil }
func (f *fakeFeedback) RateLatest(_ context.Context, ids []string, good bool) (int, error) {
	for _, id := range ids {
		if f.worked[id] {
			f.rated = append(f.rated, fmt.Sprintf("%s|%v", id, good))
			return 1, nil
		}
	}
	return 0, nil
}

func TestRoute_聊天里评价协作模型交的活(t *testing.T) {
	h, qq, tasks := hub(t)
	ctx := context.Background()
	last := func() string { return qq.sent[len(qq.sent)-1].Text }

	// 没配协作模型：口令不会被当成一件新事去办。
	h.Handle(ctx, "qq", from("OWNER", "/赞", "m1"))
	if len(tasks.log) != 0 || !strings.Contains(last(), "没有可评价的协作模型") {
		t.Fatalf("没配协作模型时应直接说明: %v %q", tasks.log, last())
	}

	fb := &fakeFeedback{worked: map[string]bool{}}
	h.WithFeedback(fb)
	h.Handle(ctx, "qq", from("OWNER", "帮我写一份方案", "m2"))
	task := tasks.tasks[0]
	// 最近的事里没有协作模型做的部分。
	h.Handle(ctx, "qq", from("OWNER", "/踩", "m3"))
	if len(fb.rated) != 0 || !strings.Contains(last(), "没什么可评的") {
		t.Fatalf("没有协作成果时不应记评价: %v %q", fb.rated, last())
	}

	// 办完之后，有协作模型参与的事会在结论后面提示可以评价。
	fb.worked[task.ID] = true
	task.State, task.Reason, task.UpdatedAt = core.StateDone, "方案写好了。", time.Now()
	if err := h.Notify(ctx, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last(), "方案写好了。") || !strings.Contains(last(), "/赞") {
		t.Fatalf("结论后面应提示可以评价: %q", last())
	}
	h.Handle(ctx, "qq", from("OWNER", "/踩", "m4"))
	if len(fb.rated) != 1 || fb.rated[0] != task.ID+"|false" || last() != "记下了。" {
		t.Fatalf("应给最近那件事记一个差评: %v %q", fb.rated, last())
	}
	// 口令必须是整句：带着别的话的是一件新事。
	h.Handle(ctx, "qq", from("OWNER", "/踩 的意思是什么", "m5"))
	if len(fb.rated) != 1 || !strings.HasPrefix(tasks.log[len(tasks.log)-1], "create:") {
		t.Fatalf("不是整句的口令不应被当成评价: %v %v", fb.rated, tasks.log)
	}
}

func TestSendFile_只发回任务所来自的会话(t *testing.T) {
	h, qq, _ := hub(t)
	ctx := context.Background()
	data := []byte("PK-docx")

	// 先有一条来信：回应它时要带上它的编号，序号接着往下排。
	h.Handle(ctx, "qq", Message{Chat: "c2c:OWNER", Sender: "OWNER", Text: "做个表", ID: "msg-9", At: time.Now()})
	before := len(qq.sent)
	if err := h.SendFile(ctx, []string{"别的标签", Tag("qq", "c2c:OWNER")}, "表.xlsx", data); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if len(qq.sent) != before+1 {
		t.Fatalf("应发出一条消息，实际多了 %d 条", len(qq.sent)-before)
	}
	got := qq.sent[len(qq.sent)-1]
	if got.Chat != "c2c:OWNER" || got.File == nil || got.File.Name != "表.xlsx" || string(got.File.Data) != "PK-docx" || got.ReplyTo != "msg-9" {
		t.Fatalf("发出的消息不对: %+v", got)
	}
	// 序号不能和先前的回应撞上。
	for _, m := range qq.sent[:len(qq.sent)-1] {
		if m.ReplyTo == "msg-9" && m.Seq >= got.Seq {
			t.Fatalf("序号应接着往下排: 之前 %d，这次 %d", m.Seq, got.Seq)
		}
	}

	// 不是从聊天里来的任务：没有地方可发。
	if err := h.SendFile(ctx, []string{"skill:x"}, "表.xlsx", data); !errors.Is(err, ErrNoChat) {
		t.Fatalf("没有会话标签时应报告无处可发: %v", err)
	}
	// 标签指向一个没接入的渠道、空文件、过大的文件：都不发。
	sent := len(qq.sent)
	if err := h.SendFile(ctx, []string{Tag("wechat", "u1")}, "表.xlsx", data); err == nil {
		t.Fatal("没接入的渠道应报错")
	}
	if err := h.SendFile(ctx, []string{Tag("qq", "c2c:OWNER")}, "空.xlsx", nil); err == nil {
		t.Fatal("空文件应被拒绝")
	}
	if err := h.SendFile(ctx, []string{Tag("qq", "c2c:OWNER")}, "大.bin", make([]byte, MaxFileBytes+1)); err == nil {
		t.Fatal("过大的文件应被拒绝")
	}
	if len(qq.sent) != sent {
		t.Fatal("被拒绝的文件不该发出去")
	}
}

func withPictures(m Message, n int) Message {
	for i := 0; i < n; i++ {
		m.Images = append(m.Images, Attachment{URL: fmt.Sprintf("https://img.example.com/%d.jpg", i), ContentType: "image/jpeg"})
	}
	return m
}

func TestHandle_图片原样交给任务与用户的话分开送(t *testing.T) {
	h, qq, tasks := hub(t)
	ctx := context.Background()
	h.fetch = func(context.Context, Attachment) (Picture, error) {
		return Picture{MIME: "image/jpeg", Data: []byte("x")}, nil
	}

	// 图片配一句话：话是任务的目标，图片单独交给任务。
	h.Handle(ctx, "qq", withPictures(from("OWNER", "把这个记到日历上", "m2"), 1))
	if len(tasks.log) != 2 || tasks.log[0] != "create:把这个记到日历上|把这个记到日历上" ||
		tasks.log[1] != "attach:把这个记到日历上|1 张|qq" {
		t.Fatalf("应新建任务并把图片交给它: %v", tasks.log)
	}
	// 只有图片没有话：照样建任务，由助手看了再说。超出张数上限的只收前几张，并告诉用户。
	tasks.log = nil
	h.Handle(ctx, "qq", withPictures(from("OWNER", "", "m3"), 6))
	if len(tasks.log) != 2 || !strings.HasPrefix(tasks.log[0], "create:处理图片|用户发来了图片") ||
		tasks.log[1] != fmt.Sprintf("attach:处理图片|%d 张|qq", maxPictures) {
		t.Fatalf("只发图片也应处理: %v", tasks.log)
	}
	if last := qq.sent[len(qq.sent)-1].Text; !strings.Contains(last, "只收下了前 4 张") {
		t.Fatalf("超出张数上限应告知用户: %s", last)
	}

	// 有任务在等回话：图片是给它的。先送图片，再送用户的话。
	tasks.log = nil
	waiting := tasks.add("订餐厅", Tag("qq", "c2c:OWNER"))
	ask(waiting, "订哪一家？")
	h.Handle(ctx, "qq", withPictures(from("OWNER", "就这家", "m4"), 1))
	if len(tasks.log) != 2 || tasks.log[0] != "attach:订餐厅|1 张|qq" || tasks.log[1] != "reply:订餐厅|就这家|qq" {
		t.Fatalf("图片应送给正在等回话的任务: %v", tasks.log)
	}

	// 取不回图片：告诉用户没处理成，不建任务。
	tasks.log = nil
	sent := len(qq.sent)
	h.fetch = func(context.Context, Attachment) (Picture, error) { return Picture{}, errors.New("超时") }
	h.Handle(ctx, "qq", withPictures(from("OWNER", "看看", "m5"), 1))
	if len(tasks.log) != 0 || len(qq.sent) != sent+1 || !strings.Contains(qq.sent[sent].Text, "取回图片失败") {
		t.Fatalf("取不回图片时应告知用户: %v", tasks.log)
	}
}

func TestFetchPicture_只认https和真正的图片(t *testing.T) {
	ctx := context.Background()
	if _, err := fetchPicture(ctx, Attachment{URL: "http://img.example.com/a.jpg"}); err == nil {
		t.Fatal("明文地址应被拒绝")
	}
	if _, err := fetchPicture(ctx, Attachment{URL: "file:///etc/passwd"}); err == nil {
		t.Fatal("不是 https 的地址应被拒绝")
	}
}

func TestRoute_换一个消息源也能回答进展跟着走(t *testing.T) {
	qq, wx := &fakeAdapter{name: "qq"}, &fakeAdapter{name: "weixin"}
	tasks := &fakeTasks{}
	h, err := NewHub([]Binding{{Adapter: qq, Owners: []string{"OWNER"}}, {Adapter: wx, Owners: []string{"WXME"}}},
		tasks, NewMemState(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	last := func() string { return tasks.log[len(tasks.log)-1] }

	// 在 QQ 里发起的任务问了个问题，问题推到 QQ。
	trip := tasks.add("订票", Tag("qq", "c2c:OWNER"))
	ask(trip, "周五几点出发？")
	if err := h.Notify(ctx, time.Time{}); err != nil || len(qq.sent) != 1 || len(wx.sent) != 0 {
		t.Fatalf("问题应推到发起的消息源: %v qq=%d wx=%d", err, len(qq.sent), len(wx.sent))
	}
	// 到微信里回答：算这个任务的回答，不是新任务。
	h.Handle(ctx, "weixin", from("WXME", "早上八点左右", "w1"))
	if last() != "reply:订票|早上八点左右|weixin" {
		t.Fatalf("换一个消息源回答也应送给提问的任务: %s", last())
	}
	// 之后的进展跟着用户走：推到微信，不再推到 QQ。
	trip.State, trip.Reason, trip.Wake = core.StateDone, "订好了 G7001", nil
	if err := h.Notify(ctx, time.Time{}); err != nil || len(qq.sent) != 1 || len(wx.sent) != 1 ||
		wx.sent[0].Chat != "c2c:WXME" || !strings.Contains(wx.sent[0].Text, "G7001") {
		t.Fatalf("进展应推到用户接着说话的消息源: %v qq=%d wx=%+v", err, len(qq.sent), wx.sent)
	}

	// 等确认的动作同理：在微信里说同意就放行。
	pay := tasks.add("付款", Tag("qq", "c2c:OWNER"))
	awaitApproval(tasks, pay, "s1", "pay.send", `{"amount":"10"}`, "")
	h.Handle(ctx, "weixin", from("WXME", "同意", "w2"))
	if last() != "approve:付款" {
		t.Fatalf("换一个消息源表态也应算数: %s", last())
	}
	// 陌生人在别的消息源里说话不算：主人名单各管各的。
	ask(tasks.add("查天气", Tag("qq", "c2c:OWNER")), "哪个城市？")
	before := len(tasks.log)
	h.Handle(ctx, "weixin", from("STRANGER", "北京", "w3"))
	if len(tasks.log) != before {
		t.Fatalf("不是主人的话不应送给任务: %v", tasks.log[before:])
	}
}

// fakeTriage 按预先写好的话回答：pick 是第几个候选（从 1 起），0 表示新的事，-1 表示拿不准。
type fakeTriage struct {
	pick int
	// id 非空时判给这个编号的任务，不看 pick。
	id    string
	err   error
	asked []string
	seen  []Candidate
}

func (f *fakeTriage) Triage(_ context.Context, text string, cands []Candidate) (Verdict, error) {
	f.asked, f.seen = append(f.asked, text), cands
	switch {
	case f.err != nil:
		return Verdict{}, f.err
	case f.id != "":
		return Verdict{TaskID: f.id}, nil
	case f.pick == 0:
		return Verdict{New: true}, nil
	case f.pick > 0 && f.pick <= len(cands):
		return Verdict{TaskID: cands[f.pick-1].ID}, nil
	}
	return Verdict{}, nil
}

func TestRoute_一句话归给哪件事由主模型判断拿不准就问用户(t *testing.T) {
	h, qq, tasks := hub(t)
	tri := &fakeTriage{}
	h = h.WithTriage(tri)
	ctx := context.Background()
	tag := Tag("qq", "c2c:OWNER")
	say := func(text string) { h.Handle(ctx, "qq", from("OWNER", text, "m")) }
	last := func() string { return tasks.log[len(tasks.log)-1] }

	// 手头没有事在办：直接是新的事，不用问模型。
	say("帮我订周五去苏州的高铁")
	if last() != "create:帮我订周五去苏州的高铁|帮我订周五去苏州的高铁" || len(tri.asked) != 0 {
		t.Fatalf("没有进行中的任务时应直接新建: %s（问了模型 %d 次）", last(), len(tri.asked))
	}
	trip := tasks.tasks[0]
	trip.State = core.StateRunning

	// 任务还在办的时候补了一句：模型判断是说它的，就作为补充送过去，不另开任务。
	tri.pick = 1
	say("要靠窗的")
	if last() != "reply:帮我订周五去苏州的高铁|要靠窗的|qq" {
		t.Fatalf("补充的话应送给正在办的任务: %s", last())
	}
	if len(tri.seen) != 1 || tri.seen[0].Status != "正在办" {
		t.Fatalf("交给模型的候选不对: %+v", tri.seen)
	}
	// 模型判断是另一件事：新建。
	tri.pick = 0
	say("今天天气怎么样")
	if last() != "create:今天天气怎么样|今天天气怎么样" {
		t.Fatalf("不相干的话应新建任务: %s", last())
	}
	weather := tasks.tasks[0]
	weather.State = core.StateRunning

	// 模型拿不准，而用户半小时之内刚说过一件事：多半是接着它说的，不去打扰用户。
	tri.pick = -1
	say("改成上午的")
	if last() != "reply:今天天气怎么样|改成上午的|qq" {
		t.Fatalf("拿不准时应归给刚说过的那件事: %s", last())
	}
	if len(tri.seen) != 2 || tri.seen[0].ID != weather.ID {
		t.Fatalf("候选应把最近说到的排在前面: %+v", tri.seen)
	}

	// 过了半小时还拿不准：问用户，这句话先不办。
	now := time.Now().Add(continueWindow + time.Minute)
	h.now = func() time.Time { return now }
	before, sent := len(tasks.log), len(qq.sent)
	say("再便宜点的")
	if len(tasks.log) != before || len(qq.sent) != sent+1 {
		t.Fatalf("拿不准时应先问用户，不建任务也不送给谁: %v", tasks.log[before:])
	}
	if q := qq.sent[len(qq.sent)-1].Text; !strings.Contains(q, "再便宜点的") || !strings.Contains(q, "1. ") || !strings.Contains(q, "2. ") || !strings.Contains(q, "0. ") {
		t.Fatalf("问题应列出进行中的事和\"新的事\"这个选项: %s", q)
	}
	// 用户回个序号：之前那句话送给选中的那件事。回答本身不问模型。
	asked := len(tri.asked)
	say("2")
	if last() != "reply:帮我订周五去苏州的高铁|再便宜点的|qq" || len(tri.asked) != asked {
		t.Fatalf("回序号后应把之前那句话送给选中的任务: %s", last())
	}
	// 回 0：之前那句话算一件新的事。又过了半小时，免得刚才那句把它算成接着说。
	now = now.Add(continueWindow + time.Minute)
	say("另外一件说不清的事")
	say("0")
	if last() != "create:另外一件说不清的事|另外一件说不清的事" {
		t.Fatalf("回 0 应新建任务: %s", last())
	}
	tasks.tasks[0].State = core.StateDone
	// 没回答、说了别的：之前那句话不能丢，按新的事办；这一句照常判断。
	say("又一件说不清的事")
	tri.pick = 1
	say("对了靠过道也行")
	n := len(tasks.log)
	if n < 2 || tasks.log[n-2] != "create:又一件说不清的事|又一件说不清的事" || !strings.HasPrefix(tasks.log[n-1], "reply:") {
		t.Fatalf("没回答时之前那句话应新建，这一句照常处理: %v", tasks.log[n-2:])
	}

	// 用户明确说了是新的事：不问模型。
	asked = len(tri.asked)
	say("/new 查一下明天的航班")
	if last() != "create:查一下明天的航班|查一下明天的航班" || len(tri.asked) != asked {
		t.Fatalf("明确说了是新的事就不用判断: %s", last())
	}
	// 等确认的动作：一句"同意"直接放行，不问模型；别的话交给模型，判给它就是"要这样改"。
	pay := tasks.add("付款", tag)
	awaitApproval(tasks, pay, "s1", "pay.send", `{"amount":"10"}`, "")
	say("同意")
	if last() != "approve:付款" || len(tri.asked) != asked {
		t.Fatalf("同意应直接放行: %s", last())
	}
	awaitApproval(tasks, pay, "s2", "pay.send", `{"amount":"10"}`, "")
	tri.id = pay.ID
	say("金额改成 8 块")
	if last() != "reject:付款|金额改成 8 块" {
		t.Fatalf("判给等确认的任务时这句话是修改意见: %s", last())
	}
	// 模型出错：话不能丢，退回按规则处理（有等确认的就归它）。
	awaitApproval(tasks, pay, "s3", "pay.send", `{"amount":"8"}`, "")
	tri.err = errors.New("模型不可用")
	say("还是算了")
	if last() != "reject:付款|还是算了" {
		t.Fatalf("模型出错时应按规则处理: %s", last())
	}
}

type memSink struct {
	saved map[string][]byte
	fail  error
}

func (m *memSink) Receive(name string, data []byte) (string, error) {
	if m.fail != nil {
		return "", m.fail
	}
	if m.saved == nil {
		m.saved = map[string][]byte{}
	}
	p := "收到的文件/" + name
	m.saved[p] = data
	return p, nil
}

func TestRoute_文件收进工作区换成一句话照常处理(t *testing.T) {
	h, qq, tasks := hub(t)
	ctx := context.Background()
	file := func(name, body string) Attachment {
		return Attachment{Name: name, Fetch: func(context.Context) (Picture, error) { return Picture{Data: []byte(body)}, nil }}
	}

	// 没启用文件处理：告诉用户，不建任务。
	h.Handle(ctx, "qq", Message{Chat: "c2c:OWNER", Sender: "OWNER", ID: "f0", Files: []Attachment{file("a.pdf", "x")}})
	if len(tasks.log) != 0 || !strings.Contains(qq.sent[len(qq.sent)-1].Text, "文件处理没有启用") {
		t.Fatalf("没启用文件处理时应说明: %v %+v", tasks.log, qq.sent)
	}

	sink := &memSink{}
	h = h.WithFiles(sink)
	// 只发了文件：存进工作区，换成"我发来了文件"建成任务。
	h.Handle(ctx, "qq", Message{Chat: "c2c:OWNER", Sender: "OWNER", ID: "f1", Files: []Attachment{file("合同.pdf", "pdf-body")}})
	if string(sink.saved["收到的文件/合同.pdf"]) != "pdf-body" {
		t.Fatalf("文件应存进工作区: %v", sink.saved)
	}
	if len(tasks.log) != 1 || !strings.Contains(tasks.log[0], "我发来了文件，已放进工作区：收到的文件/合同.pdf（8 字节）") {
		t.Fatalf("应换成一句话建任务: %v", tasks.log)
	}
	// 文件带着话：话在前，文件的说明在后，一起送给在等回话的任务。
	ask(tasks.tasks[0], "要我对合同做什么？")
	h.Handle(ctx, "qq", Message{Chat: "c2c:OWNER", Sender: "OWNER", ID: "f2", Text: "按这个改一下",
		Files: []Attachment{file("修改意见.docx", "doc")}})
	if last := tasks.log[len(tasks.log)-1]; !strings.HasPrefix(last, "reply:") || !strings.Contains(last, "按这个改一下") || !strings.Contains(last, "修改意见.docx") {
		t.Fatalf("文件带着的话应和文件说明一起送给在等回话的任务: %s", last)
	}
	// 取不回来的文件：告诉用户是哪个没收成。
	before := len(tasks.log)
	broken := Attachment{Name: "坏的.zip", Fetch: func(context.Context) (Picture, error) { return Picture{}, errors.New("下载失败") }}
	h.Handle(ctx, "qq", Message{Chat: "c2c:OWNER", Sender: "OWNER", ID: "f3", Files: []Attachment{broken}})
	if len(tasks.log) != before || !strings.Contains(qq.sent[len(qq.sent)-2].Text, "坏的.zip") {
		t.Fatalf("没收成的文件应告诉用户: %v %+v", tasks.log[before:], qq.sent[len(qq.sent)-2:])
	}
}

func TestRoute_没有模型看得了图片时直接告诉用户(t *testing.T) {
	h, qq, tasks := hub(t)
	h = h.WithoutImages()
	ctx := context.Background()
	img := Attachment{URL: "https://img.example.com/1.jpg", ContentType: "image/jpeg"}

	m := from("OWNER", "", "m1")
	m.Images = []Attachment{img}
	h.Handle(ctx, "qq", m)
	if len(tasks.log) != 0 || len(qq.sent) != 1 || !strings.Contains(qq.sent[0].Text, "我还看不懂这个类型的数据哦") {
		t.Fatalf("只发图片时应直接回复看不懂，不建任务: %v %+v", tasks.log, qq.sent)
	}

	// 带着文字的：图片不收，文字照常办。
	m = from("OWNER", "帮我记一下明天上午十点开会", "m2")
	m.Images = []Attachment{img}
	h.Handle(ctx, "qq", m)
	if len(tasks.log) != 1 || !strings.HasPrefix(tasks.log[0], "create:") {
		t.Fatalf("文字应照常建成任务: %v", tasks.log)
	}

	// 语音、视频这类消息源收不了的，到这里是空消息。
	h.Handle(ctx, "qq", from("OWNER", "", "m3"))
	if last := qq.sent[len(qq.sent)-1].Text; !strings.Contains(last, "我还看不懂这个类型的数据哦") {
		t.Fatalf("空消息应回复看不懂: %q", last)
	}
}

func TestRoute_接着刚办完的事说的话另起任务不错归给别的事(t *testing.T) {
	h, _, tasks := hub(t)
	tri := &fakeTriage{}
	h = h.WithTriage(tri)
	ctx := context.Background()
	tag := Tag("qq", "c2c:OWNER")
	say := func(text string) { h.Handle(ctx, "qq", from("OWNER", text, "m")) }
	last := func() string { return tasks.log[len(tasks.log)-1] }

	// 一件事在办（很早以前交代的），一件事刚办完。
	report := tasks.add("写周报", tag)
	report.CreatedAt = time.Now().Add(-3 * time.Hour)
	hotel := tasks.add("订酒店", tag)
	hotel.State, hotel.Reason, hotel.UpdatedAt = core.StateDone, "订好了观前街的全季", time.Now()

	// 用户接着说酒店的事：刚办完的那件在候选里，排在前面；判给它就另起一个任务接着办。
	tri.id = hotel.ID
	say("帮我把入住改成晚一天")
	if len(tri.seen) != 2 || tri.seen[0].ID != hotel.ID || !tri.seen[0].Finished || tri.seen[1].ID != report.ID {
		t.Fatalf("刚办完的事应列在候选里且排在前面: %+v", tri.seen)
	}
	if last() != "create:帮我把入住改成晚一天|帮我把入住改成晚一天" {
		t.Fatalf("判给刚办完的事时应另起任务: %s", last())
	}

	// 办完很久的不列。
	hotel.UpdatedAt = time.Now().Add(-2 * time.Hour)
	tri.id = ""
	tri.pick = 0
	say("今天有什么新闻")
	for _, c := range tri.seen {
		if c.ID == hotel.ID {
			t.Fatalf("办完很久的事不应列为候选: %+v", tri.seen)
		}
	}
}
