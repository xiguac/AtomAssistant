package pg_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// TestEventStore 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestEventStore(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过事件存储测试")
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
	bus := event.NewBus(s.Events(), s)

	// 把一个任务推进到等待事件。
	task := core.NewTask("等回信", "验证事件唤醒", core.TrustUser)
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if err := s.Transition(ctx, task.ID, core.StateNew, core.StateReady, "入队"); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if _, err := s.ClaimReady(ctx, "w1", time.Minute, 1); err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	at := time.Now().UTC().Add(time.Hour)
	task.Wake = &core.WakeCondition{
		Kind: core.WakeEvent, At: &at,
		Expr: `topic == "mail.received" AND payload.from CONTAINS "张总"`,
	}
	if err := bus.Subscribe(ctx, task.ID, task.Wake); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	if err := s.UpdateTaskRuntime(ctx, task); err != nil {
		t.Fatalf("写入唤醒条件失败: %v", err)
	}
	if err := s.Transition(ctx, task.ID, core.StateRunning, core.StateWaiting, "等事件"); err != nil {
		t.Fatalf("进入等待失败: %v", err)
	}

	subs, err := s.Events().ActiveSubscriptions(ctx, time.Now().UTC())
	if err != nil || len(subs) != 1 || subs[0].TaskID != task.ID || !subs[0].Once {
		t.Fatalf("订阅未正确读回: %v, %+v", err, subs)
	}

	// 不相关的事件：记录但不唤醒。
	if woken, err := bus.Publish(ctx, event.New("calendar.event.created", nil, core.TrustConnector, "caldav")); err != nil || woken != 0 {
		t.Fatalf("不相关事件不应唤醒任务: %d, %v", woken, err)
	}

	woken, err := bus.Publish(ctx, event.New("mail.received",
		json.RawMessage(`{"from":"客户张总","subject":"周三见"}`), core.TrustExternal, "imap:收件箱"))
	if err != nil || woken != 1 {
		t.Fatalf("期望唤醒 1 个任务，实际 %d，err=%v", woken, err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil || got.State != core.StateReady {
		t.Fatalf("任务应被唤醒: %v, %+v", err, got)
	}
	steps, err := s.ListSteps(ctx, task.ID)
	if err != nil || len(steps) != 1 || steps[0].OutputTrust != core.TrustExternal {
		t.Fatalf("事件步骤未正确写入: %v, %+v", err, steps)
	}
	var out struct {
		Topic   string
		Payload struct{ Subject string }
	}
	if err := json.Unmarshal(steps[0].Output, &out); err != nil || out.Topic != "mail.received" || out.Payload.Subject != "周三见" {
		t.Fatalf("事件内容丢失: %s (%v)", steps[0].Output, err)
	}

	// 一次性订阅命中后失效；作废后不再返回。
	if subs, _ := s.Events().ActiveSubscriptions(ctx, time.Now().UTC()); len(subs) != 0 {
		t.Fatalf("命中后订阅应失效，实际 %d 条", len(subs))
	}
}
