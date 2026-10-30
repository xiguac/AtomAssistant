package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/event"
	"github.com/xiguac/atomassistant/internal/event/memstore"
	"github.com/xiguac/atomassistant/internal/store/memory"
)

// waitingTask 建一个等待事件的任务，并通过总线登记订阅。
func waitingTask(t *testing.T, tasks *memory.Store, bus *event.Bus, expr string) *core.Task {
	t.Helper()
	ctx := context.Background()
	task := core.NewTask("等事件", "验证事件唤醒", core.TrustUser)
	if err := tasks.CreateTask(ctx, task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if err := tasks.Transition(ctx, task.ID, core.StateNew, core.StateReady, "入队"); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if _, err := tasks.ClaimReady(ctx, "w1", time.Minute, 10); err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	at := time.Now().UTC().Add(time.Hour)
	task.Wake = &core.WakeCondition{Kind: core.WakeEvent, Expr: expr, At: &at}
	if err := bus.Subscribe(ctx, task.ID, task.Wake); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	if err := tasks.UpdateTaskRuntime(ctx, task); err != nil {
		t.Fatalf("写入唤醒条件失败: %v", err)
	}
	if err := tasks.Transition(ctx, task.ID, core.StateRunning, core.StateWaiting, "等事件"); err != nil {
		t.Fatalf("进入等待失败: %v", err)
	}
	return task
}

func state(t *testing.T, tasks *memory.Store, id string) core.TaskState {
	t.Helper()
	got, err := tasks.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	return got.State
}

func TestPublish_命中的任务被唤醒并拿到事件内容(t *testing.T) {
	ctx := context.Background()
	tasks, events := memory.New(), memstore.New()
	bus := event.NewBus(events, tasks)

	hit := waitingTask(t, tasks, bus, `topic == "mail.received" AND payload.from CONTAINS "张总"`)
	miss := waitingTask(t, tasks, bus, `topic == "calendar.event.created"`)

	// 邮件是第三方写的，事件的信任等级是 external。
	e := event.New("mail.received", json.RawMessage(`{"from":"客户张总","subject":"周三见"}`),
		core.TrustExternal, "imap:收件箱")
	woken, err := bus.Publish(ctx, e)
	if err != nil || woken != 1 {
		t.Fatalf("期望唤醒 1 个任务，实际 %d，err=%v", woken, err)
	}
	if state(t, tasks, hit.ID) != core.StateReady {
		t.Fatal("命中的任务应被唤醒")
	}
	if state(t, tasks, miss.ID) != core.StateWaiting {
		t.Fatal("未命中的任务应继续等待")
	}

	steps, _ := tasks.ListSteps(ctx, hit.ID)
	if len(steps) != 1 || steps[0].Type != core.StepWait || steps[0].Status != core.StepDone {
		t.Fatalf("事件应作为一条已完成步骤写入，实际 %+v", steps)
	}
	// 能唤醒任务不等于内容可信。
	if steps[0].OutputTrust != core.TrustExternal {
		t.Fatalf("事件内容的信任等级应沿用事件自身的，实际 %s", steps[0].OutputTrust)
	}
	if !strings.Contains(string(steps[0].Output), "周三见") || !strings.Contains(string(steps[0].Output), e.ID) {
		t.Fatalf("步骤输出应带事件内容与 ID: %s", steps[0].Output)
	}
	if len(events.Events()) != 1 {
		t.Fatal("事件应被记录")
	}

	// 订阅是一次性的：同样的事件再来一次不应再唤醒任何人。
	again := event.New("mail.received", json.RawMessage(`{"from":"客户张总"}`), core.TrustExternal, "imap")
	if woken, _ := bus.Publish(ctx, again); woken != 0 {
		t.Fatalf("一次性订阅命中后应失效，实际又唤醒 %d 个", woken)
	}
}

func TestPublish_任务不在等待时订阅保持有效(t *testing.T) {
	ctx := context.Background()
	tasks, events := memory.New(), memstore.New()
	bus := event.NewBus(events, tasks)
	task := waitingTask(t, tasks, bus, `topic == "a.b"`)

	// 任务被定时器先一步唤醒并领走了。
	if err := tasks.Transition(ctx, task.ID, core.StateWaiting, core.StateReady, "到点"); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if woken, err := bus.Publish(ctx, event.New("a.b", nil, core.TrustUser, "test")); woken != 0 || err != nil {
		t.Fatalf("任务不在等待时不应计为唤醒，也不算错误: %d, %v", woken, err)
	}
	if steps, _ := tasks.ListSteps(ctx, task.ID); len(steps) != 0 {
		t.Fatal("不得往一个不在等待的任务日志里写事件")
	}
	subs, _ := events.ActiveSubscriptions(ctx, time.Now().UTC())
	if len(subs) != 1 {
		t.Fatalf("订阅应保持有效，实际 %d 条", len(subs))
	}
}

func TestSubscribe_拒绝非法订阅并作废旧订阅(t *testing.T) {
	ctx := context.Background()
	tasks, events := memory.New(), memstore.New()
	bus := event.NewBus(events, tasks)
	at := time.Now().UTC().Add(time.Hour)

	bad := []*core.WakeCondition{
		nil,
		{Kind: core.WakeTime, At: &at},
		{Kind: core.WakeEvent, Expr: `topic == "a"`}, // 缺兜底超时
		{Kind: core.WakeEvent, Expr: `topic ==`, At: &at},
	}
	for _, w := range bad {
		if err := bus.Subscribe(ctx, "t1", w); !errors.Is(err, event.ErrInvalidSubscription) {
			t.Errorf("%+v: 期望 ErrInvalidSubscription，实际 %v", w, err)
		}
	}

	for _, expr := range []string{`topic == "a"`, `topic == "b"`} {
		if err := bus.Subscribe(ctx, "t1", &core.WakeCondition{Kind: core.WakeEvent, Expr: expr, At: &at}); err != nil {
			t.Fatalf("订阅失败: %v", err)
		}
	}
	subs, _ := events.ActiveSubscriptions(ctx, time.Now().UTC())
	if len(subs) != 1 || subs[0].Expr != `topic == "b"` {
		t.Fatalf("同一任务只应保留最新的订阅，实际 %d 条", len(subs))
	}

	// 过期的订阅退出匹配。
	if subs, _ := events.ActiveSubscriptions(ctx, at.Add(time.Second)); len(subs) != 0 {
		t.Fatalf("过期订阅不应再返回，实际 %d 条", len(subs))
	}
}
