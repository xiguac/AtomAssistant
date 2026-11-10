package agent

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/matter"
	"github.com/xiguac/atomassistant/internal/session"
)

func TestPlanner_任务带上它所属那件事此前的进展(t *testing.T) {
	ctx := context.Background()
	store := matter.NewMemStore()
	m := &matter.Matter{ID: "m1", Title: "订周五去苏州的高铁", Kind: matter.KindOnce, LastActiveAt: time.Now(), CreatedAt: time.Now()}
	m.AddNote(matter.Note{At: time.Now(), Ask: "帮我订周五去苏州的高铁", Result: "已订 G7001"})
	if err := store.Put(ctx, m); err != nil {
		t.Fatal(err)
	}
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithMatters(matter.Reader{Store: store})

	task := core.NewTask("订周五去苏州的高铁", "那改成周六呢", core.TrustUser)
	task.Tags = []string{matter.Tag("m1")}
	bg, err := p.background(ctx, task)
	// 用法说明在围栏外，进展本身在围栏里并且钉住：它决定这句话指的是什么，不能被预算裁掉。
	if err != nil || len(bg) != 2 || !bg[0].Trust.CanInstruct() || bg[1].Trust.CanInstruct() || !bg[1].Pinned ||
		bg[1].Source != matterSource || !strings.Contains(bg[0].Content, matterSource) || !strings.Contains(bg[1].Content, "G7001") {
		t.Fatalf("背景里应有这件事此前的进展: %v %+v", err, bg)
	}
	// 不属于任何事的任务没有这一段。
	if bg, err := p.background(ctx, core.NewTask("x", "y", core.TrustUser)); err != nil || len(bg) != 0 {
		t.Fatalf("不属于任何事的任务不应有这一段: %v %+v", err, bg)
	}
}

func TestDigest_接了事项清单时摘要取自各件事的进展(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)
	sessions := &memSessions{}
	sessions.say(now.Add(-26*time.Hour), session.RoleUser, "帮我订周五去苏州的高铁")
	sessions.say(now.Add(-50*time.Hour), session.RoleUser, "随便聊聊")
	matters := matter.NewMemStore()
	m := &matter.Matter{ID: "m1", Title: "订周五去苏州的高铁", Kind: matter.KindOnce, LastActiveAt: now, CreatedAt: now}
	m.AddNote(matter.Note{At: now.Add(-26 * time.Hour), Ask: "帮我订周五去苏州的高铁", Result: "已订 G7001"})
	if err := matters.Put(ctx, m); err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{replies: []string{"聊了哪些事：闲聊"}}
	d := NewDigest(model, sessions, slog.New(slog.NewTextHandler(io.Discard, nil))).WithMatters(matter.Reader{Store: matters})
	d.now = func() time.Time { return now }
	if err := d.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range sessions.sums {
		got[s.Day] = s.Text
	}
	// 有事办的那天：摘要是那天各件事的进展，不调模型。只是闲聊的那天：照旧让模型写。
	if !strings.Contains(got["2026-10-20"], "【订周五去苏州的高铁】") || !strings.Contains(got["2026-10-20"], "G7001") ||
		got["2026-10-19"] != "聊了哪些事：闲聊" || model.calls != 1 {
		t.Fatalf("摘要不对（调了模型 %d 次）: %+v", model.calls, got)
	}
}

func TestTriager_清单里的事怎么列给模型怎么认回答(t *testing.T) {
	cands := []channel.Candidate{
		{ID: matter.Tag("course"), Title: "操作系统课", Matter: true, Ongoing: true, Status: "没有在办的任务", Idle: 40 * 24 * time.Hour, Asked: true},
		{ID: matter.Tag("hw"), Title: "收第三次作业", Matter: true, Under: "操作系统课", Status: "没有在办的任务",
			Last: "帮我收作业 → 收齐了 32 份", Idle: 9 * 24 * time.Hour, Dormant: "low"},
		{ID: "T1", Title: "订高铁", Goal: "周五去苏州", Status: "正在办", Idle: time.Minute},
	}
	ask := func(reply string) (channel.Verdict, string) {
		t.Helper()
		m := &fakeModel{replies: []string{reply}}
		v, err := NewTriager(m).Triage(context.Background(), channel.TriageInput{Text: "x", Candidates: cands})
		if err != nil {
			t.Fatal(err)
		}
		return v, m.lastReq.Messages[len(m.lastReq.Messages)-1].Content
	}

	v, user := ask(`{"target":"new","title":"收第四次作业","kind":"once","under":"1","ack":"好，我来收。"}`)
	for _, want := range []string{
		`1. 操作系统课 — 长期的事；`, "【等用户回答】",
		`2. 收第三次作业 — 一次性的事，属于「操作系统课」；`, "一周多没动静", "最近的进展：帮我收作业 → 收齐了 32 份",
		"3. 订高铁 — 任务，正在办；", "用户当时的要求：周五去苏州",
	} {
		if !strings.Contains(user, want) {
			t.Fatalf("清单应写明 %q:\n%s", want, user)
		}
	}
	if !v.New || v.Title != "收第四次作业" || v.Ongoing || v.Under != matter.Tag("course") || v.Ack != "好，我来收。" {
		t.Fatalf("新的事应带上标题、种类和挂在哪件事下面: %+v", v)
	}
	// 只能挂在长期的事下面：写了一次性的事或者任务的序号，不挂。
	if v, _ = ask(`{"target":"new","title":"别的","kind":"ongoing","under":2}`); v.Under != "" || !v.Ongoing {
		t.Fatalf("不能挂在一次性的事下面: %+v", v)
	}
	// 留着还是不要了，只对问过用户的那件事算数。
	if v, _ = ask(`{"target":"1","decision":"close"}`); v.TaskID != matter.Tag("course") || !v.Close || v.Keep {
		t.Fatalf("问过的事应认用户的回答: %+v", v)
	}
	if v, _ = ask(`{"target":1,"decision":"keep"}`); !v.Keep || v.Close {
		t.Fatalf("问过的事应认用户的回答: %+v", v)
	}
	if v, _ = ask(`{"target":"2","decision":"close","ack":"好。"}`); v.TaskID != matter.Tag("hw") || v.Close || v.Keep {
		t.Fatalf("没问过的事不认要删的回答: %+v", v)
	}
}
