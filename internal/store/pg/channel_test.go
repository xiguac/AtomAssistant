package pg_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/event"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// TestChannelStore 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestChannelStore(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过渠道存储测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := pg.Open(ctx, dsn, seal.TestBox())
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer s.Close()
	if err := s.ResetForTest(ctx); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 会话状态与通知记录。
	state := s.Channels()
	if c, err := state.GetChat(ctx, "qq", "c2c:A"); c != nil || err != nil {
		t.Fatalf("没有记录时应返回 nil: %v %v", c, err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := state.PutChat(ctx, &channel.ChatState{Channel: "qq", Chat: "c2c:A", LastMsgID: "m1", LastMsgAt: at, Seq: 2}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := state.PutChat(ctx, &channel.ChatState{Channel: "qq", Chat: "c2c:A", LastMsgID: "m2", LastMsgAt: at}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	c, err := state.GetChat(ctx, "qq", "c2c:A")
	if err != nil || c.LastMsgID != "m2" || c.Seq != 0 || !c.LastMsgAt.Equal(at) {
		t.Fatalf("新来信应整条替换，序号归零: %+v %v", c, err)
	}
	if fp, err := state.GetNotice(ctx, "t1"); fp != "" || err != nil {
		t.Fatalf("没通知过时应返回空: %q %v", fp, err)
	}
	_ = state.PutNotice(ctx, "t1", "ask|3")
	_ = state.PutNotice(ctx, "t1", "done")
	if fp, _ := state.GetNotice(ctx, "t1"); fp != "done" {
		t.Fatalf("指纹应被替换: %q", fp)
	}

	// 用真实的任务存储和事件总线把渠道层的几个动作走一遍。
	k := &channel.Kernel{Store: s, Bus: event.NewBus(s.Events(), s), Budget: core.DefaultBudget()}
	tag := channel.Tag("qq", "c2c:A")
	id, err := k.Create(ctx, "订票", "帮我订周五去苏州的票", []string{tag})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	task, err := s.GetTask(ctx, id)
	if err != nil || task.State != core.StateReady || task.Origin != core.TrustUser || task.Budget.MaxTokens == 0 {
		t.Fatalf("主人发起的任务应是用户等级、带预算、已入队: %+v %v", task, err)
	}
	other, _ := k.Create(ctx, "别的会话", "x", []string{channel.Tag("qq", "c2c:B")})
	cli := core.NewTask("命令行建的", "x", core.TrustUser)
	if err := s.CreateTask(ctx, cli); err != nil {
		t.Fatalf("建任务失败: %v", err)
	}

	mine, err := k.ByTag(ctx, tag, true, 10)
	if err != nil || len(mine) != 1 || mine[0].ID != id {
		t.Fatalf("应只查到这个会话的任务: %v %+v", err, mine)
	}
	// 标签是整个匹配的：c2c:A 不该把 c2c:AB 的任务也带出来。
	ab, _ := k.Create(ctx, "相似的会话", "x", []string{channel.Tag("qq", "c2c:AB")})
	if mine, _ := k.ByTag(ctx, tag, true, 10); len(mine) != 1 {
		t.Fatalf("标签应整个匹配: %+v", mine)
	}

	changed, err := k.Changed(ctx, time.Now().Add(-time.Minute), 50)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	ids := map[string]bool{}
	for _, c := range changed {
		ids[c.ID] = true
	}
	if !ids[id] || !ids[other] || !ids[ab] || ids[cli.ID] || len(changed) != 3 {
		t.Fatalf("应只返回来自会话的任务: %v", ids)
	}
	if none, _ := k.Changed(ctx, time.Now().Add(time.Minute), 50); len(none) != 0 {
		t.Fatalf("之后没有变化的不应返回: %+v", none)
	}

	// 没有动作在等确认。
	if p, err := k.PendingAction(ctx, id); p != nil || err != nil {
		t.Fatalf("没有待确认动作时应返回 nil: %v %v", p, err)
	}
	step := core.NewStep(id, 1, core.StepTool, json.RawMessage(`{"body":"收到"}`))
	step.ToolName, step.Status = "mail.send", core.StepAwaiting
	if err := s.AppendStep(ctx, step); err != nil {
		t.Fatalf("写步骤失败: %v", err)
	}
	p, err := k.PendingAction(ctx, id)
	if err != nil || p == nil || p.Tool != "mail.send" || string(p.Params) == "" {
		t.Fatalf("应返回等确认的动作: %+v %v", p, err)
	}

	// 回话进了任务的收件箱，带着用户等级。
	if err := k.Reply(ctx, id, "早上八点", "qq"); err != nil {
		t.Fatalf("回话失败: %v", err)
	}
	pending, err := event.NewBus(s.Events(), s).Pending(ctx, id)
	if err != nil || len(pending) != 1 || pending[0].Trust != core.TrustUser {
		t.Fatalf("回话应以用户等级进入收件箱: %+v %v", pending, err)
	}
}
