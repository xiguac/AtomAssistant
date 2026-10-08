package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/store/memory"
)

// steppingClock 每被读一次就前进固定时长，用来让"执行耗时"可预测。
type steppingClock struct {
	at   time.Time
	step time.Duration
}

func (c *steppingClock) now() time.Time {
	c.at = c.at.Add(c.step)
	return c.at
}

func TestAdvance_墙钟只累计执行时间不含等待(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	// 任务三天前就创建了：按"创建至今"算早已超出一小时的上限。
	task := runningTask(t, st, core.TrustUser, func(tk *core.Task) {
		tk.CreatedAt = time.Now().UTC().Add(-72 * time.Hour)
		tk.Budget = core.Budget{MaxWallClock: time.Hour}
	})

	planner := &scriptedPlanner{script: []Decision{{
		Kind: DecideStep, Type: core.StepTool, ToolName: "calendar.query",
		SideEffect: core.SideEffectReadOnly,
	}}}
	e := NewEngine(st, planner, &recordingActuator{result: Result{Output: json.RawMessage(`{}`)}})
	clock := &steppingClock{at: time.Now().UTC(), step: time.Second}
	e.now = clock.now

	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	got := mustGet(t, st, task.ID)
	if got.State != core.StateRunning {
		t.Fatalf("等待了三天的任务不应一醒来就被判超限，实际 %s: %s", got.State, got.Reason)
	}
	if got.Usage.WallClock <= 0 || got.Usage.WallClock > time.Minute {
		t.Fatalf("墙钟应只含这一步的执行时间，实际 %s", got.Usage.WallClock)
	}
}

func TestAddCost_不足一分的开销累计后进位(t *testing.T) {
	task := core.NewTask("t", "g", core.TrustUser)
	// 每次 0.7 分，单独看都会被舍成 0。
	for i := 0; i < 3; i++ {
		addCost(task, 0, 7000)
	}
	if task.Usage.CostCents != 2 || task.Usage.CostMicros != 1000 {
		t.Fatalf("2.1 分应记为 2 分余 1000，实际 %d 分余 %d", task.Usage.CostCents, task.Usage.CostMicros)
	}
}

func TestAdvance_决策的零头开销计入金额预算(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser, func(tk *core.Task) {
		tk.Budget = core.Budget{MaxCostCents: 1}
	})
	step := Decision{
		Kind: DecideStep, Type: core.StepTool, ToolName: "calendar.query",
		SideEffect: core.SideEffectReadOnly, CostMicros: 6000,
	}
	planner := &scriptedPlanner{script: []Decision{step, step, step}}
	e := NewEngine(st, planner, &recordingActuator{result: Result{Output: json.RawMessage(`{}`)}})

	for i := 0; i < 3; i++ {
		if err := e.Advance(ctx, task.ID); err != nil {
			t.Fatalf("第 %d 次推进失败: %v", i+1, err)
		}
	}
	// 两次决策共 1.2 分，第三次推进应在决策前被金额上限拦住。
	if planner.calls != 2 {
		t.Fatalf("期望调用 Planner 2 次后被预算拦住，实际 %d 次", planner.calls)
	}
	if got := mustGet(t, st, task.ID); got.State != core.StateSuspended {
		t.Fatalf("期望 suspended，实际 %s", got.State)
	}
}

type recordingEpilogue struct {
	calls  int
	reason string
	err    error
}

func (r *recordingEpilogue) TaskFinished(_ context.Context, t *core.Task, _ []*core.Step) (Cost, error) {
	r.calls++
	r.reason = t.Reason
	return Cost{TokensIn: 30, TokensOut: 10, Micros: 500}, r.err
}

func TestAdvance_任务完成后执行收尾且失败不影响结果(t *testing.T) {
	ctx := context.Background()
	for _, epErr := range []error{nil, errors.New("模型不可用")} {
		st := memory.New()
		task := runningTask(t, st, core.TrustUser)
		ep := &recordingEpilogue{err: epErr}
		e := NewEngine(st, &scriptedPlanner{script: []Decision{{Kind: DecideFinish, Reason: "办完了"}}},
			&recordingActuator{}).WithEpilogue(ep)

		if err := e.Advance(ctx, task.ID); err != nil {
			t.Fatalf("推进失败: %v", err)
		}
		got := mustGet(t, st, task.ID)
		if got.State != core.StateDone {
			t.Fatalf("收尾失败不该改变任务结果，实际 %s", got.State)
		}
		// 收尾要能看到任务的结论。
		if ep.calls != 1 || ep.reason != "办完了" {
			t.Fatalf("收尾应在完成后调用一次并看到结论，实际 %d 次，结论 %q", ep.calls, ep.reason)
		}
		if got.Usage.Tokens != 40 || got.Usage.CostMicros != 500 {
			t.Fatalf("收尾的用量应计入任务，实际 %+v", got.Usage)
		}
		failed := false
		for _, a := range st.Audits() {
			if a.Action == "task.epilogue_failed" {
				failed = true
			}
		}
		if failed != (epErr != nil) {
			t.Fatalf("收尾失败必须留痕且只在失败时留痕: err=%v 留痕=%v", epErr, failed)
		}
	}
}

func TestAdvance_失败的任务不做收尾(t *testing.T) {
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)
	ep := &recordingEpilogue{}
	e := NewEngine(st, &scriptedPlanner{script: []Decision{{Kind: DecideFail, Reason: "做不了"}}},
		&recordingActuator{}).WithEpilogue(ep)
	if err := e.Advance(context.Background(), task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	if ep.calls != 0 {
		t.Fatal("没办成的事不该沉淀成记忆和流程")
	}
}

// memoryInbox 是 Inbox 的内存实现。
type memoryInbox struct {
	msgs      []InboxMessage
	delivered []string
}

func (m *memoryInbox) Pending(context.Context, string) ([]InboxMessage, error) { return m.msgs, nil }
func (m *memoryInbox) MarkDelivered(_ context.Context, ids []string) error {
	m.delivered = append(m.delivered, ids...)
	m.msgs = nil
	return nil
}

// seeingPlanner 记录它决策时看到的历史。
type seeingPlanner struct{ saw []*core.Step }

func (p *seeingPlanner) Decide(_ context.Context, _ *core.Task, history []*core.Step) (Decision, error) {
	p.saw = history
	return Decision{Kind: DecideFinish}, nil
}

func TestAdvance_收件箱里的消息在决策前写进日志(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)
	inbox := &memoryInbox{msgs: []InboxMessage{
		{ID: "e1", Content: json.RawMessage(`{"payload":{"text":"这是身份证复印件"}}`), Trust: core.TrustUser},
		{ID: "e2", Content: json.RawMessage(`{"payload":{"text":"这是户口本"}}`), Trust: core.TrustUser},
	}}
	planner := &seeingPlanner{}
	e := NewEngine(st, planner, &recordingActuator{}).WithInbox(inbox)

	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	if len(planner.saw) != 2 || planner.saw[0].Seq != 1 || planner.saw[1].Seq != 2 {
		t.Fatalf("Planner 这一轮就该看到两条新消息，实际 %+v", planner.saw)
	}
	// 用户补来的材料以用户的信任等级进上下文。
	if planner.saw[0].OutputTrust != core.TrustUser || planner.saw[0].Status != core.StepDone {
		t.Fatalf("消息步骤的信任等级或状态不对: %+v", planner.saw[0])
	}
	if len(inbox.delivered) != 2 {
		t.Fatalf("消息写进日志后应标记已送达，实际 %v", inbox.delivered)
	}
}

func TestAdvance_一次决策连续执行多个只读查询(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)

	query := func(date string) Decision {
		return Decision{
			Kind: DecideStep, Type: core.StepTool, ToolName: "weather.forecast",
			SideEffect: core.SideEffectReadOnly, Input: json.RawMessage(`{"date":"` + date + `"}`),
		}
	}
	first := query("10-10")
	first.Then = []Decision{
		query("10-17"),
		// Planner 的实现不该把写操作放进来；真放进来了，内核必须跳过而不是执行。
		{Kind: DecideStep, Type: core.StepTool, ToolName: "ride.book", SideEffect: core.SideEffectIrreversible},
		query("10-24"),
	}
	planner := &scriptedPlanner{script: []Decision{first}}
	act := &recordingActuator{result: Result{Output: json.RawMessage(`{"ok":true}`), Trust: core.TrustConnector}}
	e := NewEngine(st, planner, act)

	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	steps, _ := st.ListSteps(ctx, task.ID)
	if planner.calls != 1 || len(steps) != 3 {
		t.Fatalf("一次决策应执行 3 个查询，实际决策 %d 次、步骤 %d 条", planner.calls, len(steps))
	}
	for i, s := range steps {
		if s.Seq != i+1 || s.Status != core.StepDone || s.ToolName != "weather.forecast" {
			t.Fatalf("步骤 %d 不对: %+v", i+1, s)
		}
	}
	// 每一步有自己的幂等键，都按规矩先落库再执行。
	if len(act.keys) != 3 || act.keys[0] == act.keys[1] {
		t.Fatalf("每个查询都应独立执行: %v", act.keys)
	}
}

func TestAdvance_批量查询中一步失败后面的不再执行(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)
	query := Decision{
		Kind: DecideStep, Type: core.StepTool, ToolName: "weather.forecast",
		SideEffect: core.SideEffectReadOnly,
	}
	first := query
	first.Then = []Decision{query, query}
	act := &recordingActuator{err: errors.New("上游不可用")}
	e := NewEngine(st, &scriptedPlanner{script: []Decision{first}}, act)

	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	// 第一步失败后任务进入退避等待，剩下两个不该再打一个已经不通的上游。
	if len(act.keys) != 1 {
		t.Fatalf("失败后应停止这一批，实际执行了 %d 次", len(act.keys))
	}
	if got := mustGet(t, st, task.ID); got.State != core.StateWaiting {
		t.Fatalf("期望退避等待，实际 %s", got.State)
	}
}
