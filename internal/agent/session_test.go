package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/session"
)

// memSessions 是放在内存里的会话记录。
type memSessions struct {
	msgs []session.Message
	sums []session.Summary
}

func (m *memSessions) Append(_ context.Context, msg session.Message) error {
	m.msgs = append(m.msgs, msg)
	return nil
}
func (m *memSessions) Messages(_ context.Context, day string, limit int) ([]session.Message, error) {
	var out []session.Message
	for _, msg := range m.msgs {
		if msg.Day == day {
			out = append(out, msg)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
func (m *memSessions) PutSummary(_ context.Context, s session.Summary) error {
	for i := range m.sums {
		if m.sums[i].Day == s.Day {
			m.sums[i] = s
			return nil
		}
	}
	m.sums = append(m.sums, s)
	return nil
}
func (m *memSessions) Summaries(_ context.Context, from, to string) ([]session.Summary, error) {
	var out []session.Summary
	for _, s := range m.sums {
		if s.Day >= from && s.Day <= to {
			out = append(out, s)
		}
	}
	return out, nil
}
func (m *memSessions) Unsummarized(_ context.Context, before string) ([]string, error) {
	seen := map[string]bool{}
	for _, s := range m.sums {
		seen[s.Day] = true
	}
	var out []string
	for _, msg := range m.msgs {
		if msg.Day < before && !seen[msg.Day] {
			seen[msg.Day] = true
			out = append(out, msg.Day)
		}
	}
	return out, nil
}

func (m *memSessions) say(at time.Time, role session.Role, text string) {
	m.msgs = append(m.msgs, session.Message{ID: core.NewID(), Day: session.Day(at), At: at, Role: role, Text: text})
}

func TestTriager_今天的会话按一来一回交给模型(t *testing.T) {
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.Local)
	today := []session.Message{
		{Role: session.RoleAssistant, Text: "（写周报）周报写好了", At: at},
		{Role: session.RoleUser, Text: "帮我订周五去苏州的高铁", At: at.Add(time.Minute)},
		{Role: session.RoleUser, Text: "要靠窗", At: at.Add(2 * time.Minute)},
		{Role: session.RoleAssistant, Text: "（订高铁）几点出发？", At: at.Add(3 * time.Minute)},
	}
	m := &fakeModel{replies: []string{`{"target": "1"}`}}
	in := channel.TriageInput{
		Text:       "上午的",
		Candidates: []channel.Candidate{{ID: "A", Title: "订高铁", Status: "正在等用户回答：几点出发？"}},
		Today:      today,
		Earlier:    &session.Summary{Day: "2026-10-20", Text: "聊了去苏州的行程"},
	}
	got, err := NewTriager(m).Triage(context.Background(), in)
	if err != nil || got.TaskID != "A" {
		t.Fatalf("应判给 A: %+v %v", got, err)
	}
	msgs := m.lastReq.Messages
	roles := make([]string, len(msgs))
	for i, msg := range msgs {
		roles[i] = string(msg.Role)
	}
	// 系统提示之后从用户开始、一来一回：第一条是助手说的就补一句用户的，同一个人连着说的并成一条。
	want := "system user assistant user assistant user"
	if strings.Join(roles, " ") != want {
		t.Fatalf("消息应一来一回: %s", strings.Join(roles, " "))
	}
	if !strings.Contains(msgs[3].Content, "帮我订周五去苏州的高铁") || !strings.Contains(msgs[3].Content, "要靠窗") {
		t.Fatalf("连着说的两句应并成一条: %q", msgs[3].Content)
	}
	last := msgs[len(msgs)-1].Content
	if !strings.Contains(last, "聊了去苏州的行程") || !strings.Contains(last, "订高铁") || !strings.Contains(last, "上午的") {
		t.Fatalf("最后一条应有摘要、任务清单和这句话: %s", last)
	}
}

func TestTranscript_太长时丢最早的(t *testing.T) {
	var today []session.Message
	for range 100 {
		today = append(today, session.Message{Role: session.RoleUser, Text: strings.Repeat("长", messageRunes+50)})
	}
	today = append(today, session.Message{Role: session.RoleAssistant, Text: "最后一句"})
	got := transcript(today)
	total := 0
	for _, m := range got {
		total += len([]rune(m.Content))
	}
	if total > transcriptRunes+500 || !strings.Contains(got[len(got)-1].Content, "最后一句") {
		t.Fatalf("应只留最近的、总长受限: 共 %d 字", total)
	}
}

func TestSessionSegment_新任务带上当天之前的会话和昨天的摘要(t *testing.T) {
	store := &memSessions{}
	created := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)
	store.sums = []session.Summary{{Day: "2026-10-20", Text: "聊了去苏州的行程"}}
	store.say(created.Add(-20*time.Hour), session.RoleUser, "前一天说的原话")
	store.say(created.Add(-30*time.Minute), session.RoleUser, "帮我看周五去苏州的高铁")
	store.say(created.Add(-29*time.Minute), session.RoleAssistant, "（看高铁）周五早上有 G7001")
	// 这一句就是这个任务本身，任务定义里已经有了。
	store.say(created.Add(-time.Second), session.RoleUser, "换成周日出发行不行")
	store.say(created.Add(time.Minute), session.RoleUser, "之后才说的话")

	task := core.NewTask("换成周日出发行不行", "换成周日出发行不行", core.TrustUser)
	task.CreatedAt, task.Tags = created, []string{"chat:qq/1", ConversationTag}
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithSessions(store)
	seg, ok, err := p.conversationSegment(context.Background(), task)
	if err != nil || !ok {
		t.Fatalf("应带上此前的对话: %v %v", ok, err)
	}
	text := seg.Content
	for _, want := range []string{"聊了去苏州的行程", "帮我看周五去苏州的高铁", "G7001", ToolRecall} {
		if !strings.Contains(text, want) {
			t.Fatalf("应包含 %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"前一天说的原话", "换成周日出发行不行", "之后才说的话"} {
		if strings.Contains(text, not) {
			t.Fatalf("不应包含 %q：前一天的原话只看摘要，这个任务自己和之后的话都不算:\n%s", not, text)
		}
	}
	if seg.Trust >= core.TrustUser {
		t.Fatalf("此前的对话不应按用户指令对待: %s", seg.Trust)
	}
	// 查以前对话的工具给了模型。
	found := false
	for _, tool := range p.builtins() {
		found = found || tool.Name == ToolRecall
	}
	if !found {
		t.Fatal("接入会话后应能查以前的对话")
	}
}

func TestRecall_查摘要和某一天的原话(t *testing.T) {
	store := &memSessions{}
	now := time.Now()
	d1, d2 := session.Day(now.AddDate(0, 0, -20)), session.Day(now.AddDate(0, 0, -10))
	old := session.Day(now.AddDate(0, 0, -45))
	store.sums = []session.Summary{
		{Day: old, Text: "很久以前订了去杭州的酒店"},
		{Day: d1, Text: "订了国庆去杭州的酒店"},
		{Day: d2, Text: "聊了周报"},
	}
	store.say(now.AddDate(0, 0, -10), session.RoleUser, "周报写好了吗")
	a := NewActuator(nil, nil).WithSessions(store)
	run := func(params string) (recallOutput, error) {
		out, err := a.runRecall(context.Background(), &core.Step{ToolName: ToolRecall, Input: json.RawMessage(params)})
		var r recallOutput
		if err == nil {
			_ = json.Unmarshal(out, &r)
		}
		return r, err
	}
	r, err := run(`{"from":"` + d1 + `","query":"杭州"}`)
	if err != nil || len(r.Summaries) != 1 || r.Summaries[0].Day != d1 {
		t.Fatalf("按词查摘要不对: %+v %v", r, err)
	}
	r, err = run(`{"day":"` + session.Day(now.AddDate(0, 0, -10)) + `"}`)
	if err != nil || !strings.Contains(r.Messages, "周报写好了吗") {
		t.Fatalf("翻某一天的原话不对: %+v %v", r, err)
	}
	// 超过 30 天的摘要和原话不能查：范围里太早的部分不给，并说明；单查那一天直接拒绝。
	r, err = run(`{"from":"` + old + `","query":"杭州"}`)
	if err != nil || len(r.Summaries) != 1 || r.Summaries[0].Day != d1 || !strings.Contains(r.Note, "30 天") {
		t.Fatalf("超过 30 天的摘要不应给出: %+v %v", r, err)
	}
	if _, err := run(`{"day":"` + old + `"}`); err == nil || !strings.Contains(err.Error(), "30 天") {
		t.Fatalf("超过 30 天的原话不应能查: %v", err)
	}
	if _, err := run(`{"from":"10月1日"}`); err == nil {
		t.Fatal("日子写法不对应报错")
	}
}

func TestDigest_过完的日子写成摘要今天的不写(t *testing.T) {
	store := &memSessions{}
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)
	store.say(now.Add(-26*time.Hour), session.RoleUser, "提醒我周五交作业")
	store.say(now.Add(-time.Hour), session.RoleUser, "今天说的")
	m := &fakeModel{replies: []string{"还没办完的：周五交作业"}}
	mem := &dayMemory{}
	d := NewDigest(m, store, slog.New(slog.NewTextHandler(io.Discard, nil))).WithMemory(mem)
	d.now = func() time.Time { return now }
	if err := d.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.sums) != 1 || store.sums[0].Day != "2026-10-20" || store.sums[0].Text != "还没办完的：周五交作业" {
		t.Fatalf("应只给过完的那天写摘要: %+v", store.sums)
	}
	// 写完摘要后，那天的对话交去提炼记忆：重要的存进记忆，不重要的留在会话里。
	if len(mem.days) != 1 || mem.days[0] != "2026-10-20|还没办完的：周五交作业|1" {
		t.Fatalf("写完摘要应把那天的对话交去提炼记忆: %v", mem.days)
	}
	if m.lastPurp != llm.PurposeSummarize || m.lastReq.JSONMode || !strings.Contains(m.lastReq.Messages[1].Content, "提醒我周五交作业") {
		t.Fatalf("写摘要的请求不对: %s %+v", m.lastPurp, m.lastReq)
	}
	// 写过的不再写；模型交了白卷要报错，留着下次再写。
	if err := d.RunOnce(context.Background()); err != nil || m.calls != 1 {
		t.Fatalf("写过的不应再写: %v（调了 %d 次）", err, m.calls)
	}
	store.say(now.Add(-50*time.Hour), session.RoleUser, "更早的一天")
	m.replies = []string{"  "}
	if err := d.RunOnce(context.Background()); err == nil {
		t.Fatal("空摘要应报错")
	}
}

// dayMemory 记下交来提炼记忆的日子：日子|摘要|消息条数。
type dayMemory struct{ days []string }

func (m *dayMemory) DayFinished(_ context.Context, day, summary string, msgs []session.Message) error {
	m.days = append(m.days, fmt.Sprintf("%s|%s|%d", day, summary, len(msgs)))
	return nil
}
