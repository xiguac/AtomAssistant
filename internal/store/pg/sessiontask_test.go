package pg_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/event"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/session"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// TestSessionTasks 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestSessionTasks(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过会话任务测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := pg.Open(ctx, dsn, seal.TestBox())
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer s.Close()
	if err := s.ResetForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	k := &channel.Kernel{Store: s, Bus: event.NewBus(s.Events(), s), Budget: core.DefaultBudget()}
	tag := channel.Tag("qq", "c2c:A")
	chat := []string{tag, channel.ConversationTag}

	// 同一天在聊天里交代的两件事挂在同一个会话下面；会话本身不进调度，也不算等用户处理。
	a, err := k.Create(ctx, "订票", "帮我订周五的票", chat)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := k.Create(ctx, "查天气", "明天天气", chat)
	ta, _ := s.GetTask(ctx, a)
	tb, _ := s.GetTask(ctx, b)
	if ta.ParentID == "" || ta.ParentID != tb.ParentID {
		t.Fatalf("同一天的事应挂在同一个会话下: %q %q", ta.ParentID, tb.ParentID)
	}
	sess, err := s.GetTask(ctx, ta.ParentID)
	if err != nil || sess.State != core.StateWaiting || sess.Wake == nil || sess.Wake.Kind != core.WakeSubtask {
		t.Fatalf("会话任务应在等子任务，不进调度: %+v %v", sess, err)
	}
	if day, ok := channel.SessionDay(sess.Tags); !ok || day != session.Day(time.Now()) {
		t.Fatalf("会话任务应标出是哪一天: %v", sess.Tags)
	}
	if due, _ := s.DueWaiting(ctx, time.Now().Add(48*time.Hour), 10); len(due) != 0 {
		t.Fatalf("会话任务不应被唤醒: %d", len(due))
	}
	// 不是从聊天来的（后台建的）不挂。
	cli, _ := k.Create(ctx, "后台建的", "x", nil)
	if tc, _ := s.GetTask(ctx, cli); tc.ParentID != "" {
		t.Fatal("后台建的任务不该挂到会话下")
	}
	// 列表只列顶层：会话和后台建的那件，聊天里的两件在会话里。
	_, total, err := s.QueryTasks(ctx, pg.TaskQuery{Status: pg.TaskStatusAll})
	if err != nil || total != 2 {
		t.Fatalf("任务列表应只有顶层的两个: %d %v", total, err)
	}
	if kids, _ := s.ChildTasks(ctx, sess.ID, false); len(kids) != 2 {
		t.Fatalf("会话下应有两件事: %d", len(kids))
	}

	// 一件办完了，一件还在办。过了这一天：没办完的移到新一天的会话，旧的会话结束。
	_ = s.Transition(ctx, b, core.StateReady, core.StateRunning, "测试")
	_ = s.Transition(ctx, b, core.StateRunning, core.StateDone, "办完了")
	if err := k.RollSessions(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s2, _ := s.GetTask(ctx, sess.ID); s2.State != core.StateWaiting {
		t.Fatal("当天的会话还不该结束")
	}
	if err := k.RollSessions(ctx, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old, _ := s.GetTask(ctx, sess.ID)
	if old.State != core.StateDone || old.Reason == "" {
		t.Fatalf("过去那天的会话应结束: %+v", old)
	}
	moved, _ := s.GetTask(ctx, a)
	stay, _ := s.GetTask(ctx, b)
	if moved.ParentID == sess.ID || moved.ParentID == "" || stay.ParentID != sess.ID {
		t.Fatalf("没办完的应移到新一天，办完的留在原来那天: %q %q", moved.ParentID, stay.ParentID)
	}
	next, _ := s.GetTask(ctx, moved.ParentID)
	if day, _ := channel.SessionDay(next.Tags); day != session.Day(time.Now().Add(24*time.Hour)) {
		t.Fatalf("应移到新一天的会话: %v", next.Tags)
	}
	// 以前建的、没挂到会话下的聊天任务：按创建的日子挂进那天的会话，过去的那天随后收尾。
	legacy := core.NewTask("以前的事", "x", core.TrustUser)
	legacy.CreatedAt = time.Now().AddDate(0, 0, -3)
	legacy.Tags = chat
	if err := s.CreateTask(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := k.AdoptChatTasks(ctx); err != nil {
		t.Fatal(err)
	}
	lt, _ := s.GetTask(ctx, legacy.ID)
	lp, _ := s.GetTask(ctx, lt.ParentID)
	if day, _ := channel.SessionDay(lp.Tags); day != session.Day(legacy.CreatedAt) {
		t.Fatalf("以前的聊天任务应挂进它那天的会话: %v", lp.Tags)
	}
	// 重跑不会再动已经结束的会话。
	if err := k.RollSessions(ctx, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
}
