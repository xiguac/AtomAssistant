package channel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/session"
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
func (m *memSessions) Unsummarized(context.Context, string) ([]string, error) { return nil, nil }

func TestSession_说过的话都记进当天的会话判断归属时带上(t *testing.T) {
	h, qq, tasks := hub(t)
	tri := &fakeTriage{}
	store := &memSessions{}
	h = h.WithTriage(tri).WithSession(store)
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)
	h.now = func() time.Time { return now }
	yesterday := session.Day(now.AddDate(0, 0, -1))
	store.sums = []session.Summary{{Day: yesterday, Text: "聊了去苏州的行程"}}
	say := func(text string) { h.Handle(ctx, "qq", from("OWNER", text, "m")) }

	// 第一句话：没有在办的事，直接新建，会话里记下用户说的。
	say("帮我订周五去苏州的高铁")
	trip := tasks.tasks[0]
	if len(store.msgs) != 1 || store.msgs[0].Role != session.RoleUser || store.msgs[0].Text != "帮我订周五去苏州的高铁" ||
		store.msgs[0].Day != session.Day(now) {
		t.Fatalf("用户说的话应记进当天的会话: %+v", store.msgs)
	}

	// 任务问了问题，推给用户：记成助手说的，标上是哪件事。
	ask(trip, "几点出发？")
	if err := h.Notify(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	last := store.msgs[len(store.msgs)-1]
	if last.Role != session.RoleAssistant || last.TaskID != trip.ID || !strings.Contains(last.Text, "几点出发") ||
		!strings.Contains(last.Text, trip.Title) {
		t.Fatalf("推给用户的进展应记成助手说的并标上任务: %+v", last)
	}

	// 下一句交给模型判断：看得到今天前面说过的（不含这一句本身）和昨天的摘要。
	now = now.Add(time.Minute)
	tri.pick = 1
	say("上午的")
	in := tri.last
	if len(in.Today) != 2 || in.Today[0].Text != "帮我订周五去苏州的高铁" || in.Today[1].Role != session.RoleAssistant {
		t.Fatalf("判断时应带上今天前面说过的: %+v", in.Today)
	}
	if in.Earlier == nil || in.Earlier.Text != "聊了去苏州的行程" {
		t.Fatalf("判断时应带上最近一天的摘要: %+v", in.Earlier)
	}

	// 不用办事的话：直接回，不建任务也不送给谁，回的话也记进会话。
	trip.State = core.StateRunning
	tri.pick, tri.reply = 0, "不客气"
	before, sent := len(tasks.log), len(qq.sent)
	say("谢谢")
	if len(tasks.log) != before || len(qq.sent) != sent+1 || qq.sent[len(qq.sent)-1].Text != "不客气" {
		t.Fatalf("不用办事时应直接回一句: %v %+v", tasks.log[before:], qq.sent[sent:])
	}
	if last := store.msgs[len(store.msgs)-1]; last.Role != session.RoleAssistant || last.Text != "不客气" {
		t.Fatalf("直接回的话应记进会话: %+v", last)
	}
}
