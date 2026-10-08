package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/store"
	"github.com/xiguac/atomhelper/internal/store/memory"
)

// scriptedPlanner 按预设脚本逐个返回决策，用完后返回 finish。
type scriptedPlanner struct {
	script []Decision
	calls  int
}

func (p *scriptedPlanner) Decide(context.Context, *core.Task, []*core.Step) (Decision, error) {
	if p.calls >= len(p.script) {
		return Decision{Kind: DecideFinish}, nil
	}
	d := p.script[p.calls]
	p.calls++
	return d, nil
}

// recordingActuator 记录每次执行，便于断言幂等键与执行次数。
type recordingActuator struct {
	keys   []string
	result Result
	err    error
}

func (a *recordingActuator) Execute(_ context.Context, _ *core.Task, s *core.Step) (Result, error) {
	a.keys = append(a.keys, s.IdempotencyKey)
	if a.err != nil {
		return Result{}, a.err
	}
	return a.result, nil
}

// runningTask 建一个已进入 running 状态的任务。
func runningTask(t *testing.T, st *memory.Store, origin core.TrustLevel, opts ...func(*core.Task)) *core.Task {
	t.Helper()
	ctx := context.Background()
	task := core.NewTask("测试任务", "验证内核", origin)
	for _, opt := range opts {
		opt(task)
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if err := st.Transition(ctx, task.ID, core.StateNew, core.StateReady, "入队"); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	claimed, err := st.ClaimReady(ctx, "worker-1", time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("领取任务失败: %v, 数量 %d", err, len(claimed))
	}
	return claimed[0]
}

func mustGet(t *testing.T, st *memory.Store, id string) *core.Task {
	t.Helper()
	got, err := st.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	return got
}

func TestAdvance_执行一步后落库并完成(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)

	planner := &scriptedPlanner{script: []Decision{{
		Kind:       DecideStep,
		Type:       core.StepTool,
		ToolName:   "calendar.query",
		SideEffect: core.SideEffectReadOnly,
		Input:      json.RawMessage(`{"range":"today"}`),
	}}}
	act := &recordingActuator{result: Result{
		Output: json.RawMessage(`{"events":0}`), Trust: core.TrustConnector, CostCents: 3,
	}}
	e := NewEngine(st, planner, act)

	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	steps, _ := st.ListSteps(ctx, task.ID)
	if len(steps) != 1 || steps[0].Status != core.StepDone {
		t.Fatalf("期望 1 个已完成步骤，实际 %+v", steps)
	}
	if got := mustGet(t, st, task.ID); got.Usage.CostCents != 3 {
		t.Fatalf("用量未累计，实际 %d 分", got.Usage.CostCents)
	}

	// 第二次推进：脚本用尽，Planner 返回 finish。
	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("第二次推进失败: %v", err)
	}
	if got := mustGet(t, st, task.ID); got.State != core.StateDone {
		t.Fatalf("期望 done，实际 %s", got.State)
	}
}

func TestAdvance_崩溃后可重放的步骤用同一幂等键续跑(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)

	// 模拟崩溃现场：pending 步骤已落库，但没有执行结果。
	orphan := core.NewStep(task.ID, 1, core.StepTool, json.RawMessage(`{}`))
	orphan.ToolName = "calendar.query"
	orphan.SideEffect = core.SideEffectReadOnly
	if err := st.AppendStep(ctx, orphan); err != nil {
		t.Fatalf("写入 pending 步骤失败: %v", err)
	}

	act := &recordingActuator{result: Result{Output: json.RawMessage(`{"ok":true}`), Trust: core.TrustConnector}}
	e := NewEngine(st, &scriptedPlanner{}, act)

	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("恢复执行失败: %v", err)
	}
	if len(act.keys) != 1 {
		t.Fatalf("期望执行 1 次，实际 %d 次", len(act.keys))
	}
	// 幂等键必须与崩溃前一致，否则外部服务无法识别这是同一次操作。
	want := core.IdempotencyKeyFor(task.ID, 1)
	if act.keys[0] != want {
		t.Fatalf("幂等键变了: 期望 %s，实际 %s", want, act.keys[0])
	}
	steps, _ := st.ListSteps(ctx, task.ID)
	if len(steps) != 1 || steps[0].Status != core.StepDone {
		t.Fatalf("期望原步骤被补完而不是新增，实际 %+v", steps)
	}
}

func TestAdvance_崩溃后不可逆步骤挂起而不是重试(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)

	orphan := core.NewStep(task.ID, 1, core.StepTool, json.RawMessage(`{"amount":8800}`))
	orphan.ToolName = "ride.book"
	orphan.SideEffect = core.SideEffectIrreversible
	if err := st.AppendStep(ctx, orphan); err != nil {
		t.Fatalf("写入 pending 步骤失败: %v", err)
	}

	act := &recordingActuator{}
	e := NewEngine(st, &scriptedPlanner{}, act)

	err := e.Advance(ctx, task.ID)
	if !errors.Is(err, ErrNeedsReconciliation) {
		t.Fatalf("期望 ErrNeedsReconciliation，实际 %v", err)
	}
	if len(act.keys) != 0 {
		t.Fatalf("不可逆步骤绝不能被自动重放，实际执行了 %d 次", len(act.keys))
	}
	if got := mustGet(t, st, task.ID); got.State != core.StateSuspended {
		t.Fatalf("期望 suspended，实际 %s", got.State)
	}
}

func TestAdvance_untrusted来源的不可逆操作强制走审批(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	// 来源是截图/邮件这类外部内容。
	task := runningTask(t, st, core.TrustExternal)

	planner := &scriptedPlanner{script: []Decision{{
		Kind:       DecideStep,
		Type:       core.StepTool,
		ToolName:   "ride.book",
		SideEffect: core.SideEffectIrreversible,
		Input:      json.RawMessage(`{"amount":100}`),
	}}}
	act := &recordingActuator{}
	e := NewEngine(st, planner, act)

	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	if len(act.keys) != 0 {
		t.Fatalf("untrusted 来源的不可逆操作不得直接执行，实际执行了 %d 次", len(act.keys))
	}
	got := mustGet(t, st, task.ID)
	if got.State != core.StateWaiting {
		t.Fatalf("期望 waiting，实际 %s", got.State)
	}
	if got.Wake == nil || got.Wake.Kind != core.WakeApproval {
		t.Fatalf("期望等待审批，实际 %+v", got.Wake)
	}
	// 待审批的动作必须原样留在日志里，批准后执行的才是用户看过的那一个。
	steps, _ := st.ListSteps(ctx, task.ID)
	if len(steps) != 1 || steps[0].Status != core.StepAwaiting || steps[0].ToolName != "ride.book" {
		t.Fatalf("期望 1 条 awaiting 步骤，实际 %+v", steps)
	}
	if got.Wake.Ref != steps[0].ID {
		t.Fatalf("唤醒条件应指向待审批步骤，实际 %q", got.Wake.Ref)
	}
}

// awaitingApproval 把一个 untrusted 来源的不可逆动作推进到等待审批。
func awaitingApproval(t *testing.T, st *memory.Store, planner *scriptedPlanner, act *recordingActuator) (*Engine, *core.Task) {
	t.Helper()
	task := runningTask(t, st, core.TrustExternal)
	planner.script = append([]Decision{{
		Kind:       DecideStep,
		Type:       core.StepTool,
		ToolName:   "ride.book",
		SideEffect: core.SideEffectIrreversible,
		Input:      json.RawMessage(`{"amount":100}`),
	}}, planner.script...)
	e := NewEngine(st, planner, act)
	if err := e.Advance(context.Background(), task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	return e, task
}

// reclaim 模拟调度器重新领取一个 ready 任务。
func reclaim(t *testing.T, st *memory.Store) {
	t.Helper()
	claimed, err := st.ClaimReady(context.Background(), "worker-1", time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("重新领取任务失败: %v, 数量 %d", err, len(claimed))
	}
}

func TestApprove_批准后按原参数执行且不再询问Planner(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	planner := &scriptedPlanner{}
	act := &recordingActuator{result: Result{Output: json.RawMessage(`{"ok":true}`), Trust: core.TrustToolOutput}}
	e, task := awaitingApproval(t, st, planner, act)

	if err := Approve(ctx, st, task.ID); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	// 重复批准不应报错，也不应产生第二次执行。
	if err := Approve(ctx, st, task.ID); err != nil {
		t.Fatalf("重复批准应幂等: %v", err)
	}
	if got := mustGet(t, st, task.ID); got.State != core.StateReady {
		t.Fatalf("批准后期望 ready，实际 %s", got.State)
	}

	reclaim(t, st)
	callsBefore := planner.calls
	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("执行已批准步骤失败: %v", err)
	}
	if planner.calls != callsBefore {
		t.Fatalf("执行已批准的动作不应再询问 Planner")
	}
	if len(act.keys) != 1 || act.keys[0] != core.IdempotencyKeyFor(task.ID, 1) {
		t.Fatalf("期望用步骤 1 的幂等键执行 1 次，实际 %v", act.keys)
	}
	steps, _ := st.ListSteps(ctx, task.ID)
	if len(steps) != 1 || steps[0].Status != core.StepDone || string(steps[0].Input) != `{"amount":100}` {
		t.Fatalf("期望原步骤以原参数完成，实际 %+v", steps)
	}
}

func TestReject_拒绝后步骤跳过并交回Planner(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	planner := &scriptedPlanner{}
	act := &recordingActuator{}
	e, task := awaitingApproval(t, st, planner, act)

	if err := Reject(ctx, st, task.ID, "太贵了"); err != nil {
		t.Fatalf("拒绝失败: %v", err)
	}
	steps, _ := st.ListSteps(ctx, task.ID)
	if steps[0].Status != core.StepSkipped || steps[0].Err == "" {
		t.Fatalf("被拒绝的步骤应为 skipped 且带理由，实际 %+v", steps[0])
	}
	if err := Reject(ctx, st, task.ID, ""); !errors.Is(err, ErrNothingToDecide) {
		t.Fatalf("没有待审批步骤时应返回 ErrNothingToDecide，实际 %v", err)
	}

	reclaim(t, st)
	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	if len(act.keys) != 0 {
		t.Fatalf("被拒绝的动作绝不能执行，实际执行了 %d 次", len(act.keys))
	}
	// 脚本用尽，Planner 返回 finish。
	if got := mustGet(t, st, task.ID); got.State != core.StateDone {
		t.Fatalf("期望 Planner 接手后结束任务，实际 %s", got.State)
	}
}

func TestAdvance_仍在等审批的任务被提前唤醒时回到等待(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	planner := &scriptedPlanner{}
	act := &recordingActuator{}
	e, task := awaitingApproval(t, st, planner, act)

	// 没有批准，任务却被放回了队列（例如人工改了状态）。
	if err := st.Transition(ctx, task.ID, core.StateWaiting, core.StateReady, "误唤醒"); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	reclaim(t, st)
	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	if len(act.keys) != 0 || planner.calls != 1 {
		t.Fatalf("未批准的动作不得执行也不得被重新规划，执行 %d 次，规划 %d 次", len(act.keys), planner.calls)
	}
	if got := mustGet(t, st, task.ID); got.State != core.StateWaiting {
		t.Fatalf("期望回到 waiting，实际 %s", got.State)
	}
}

func TestResolve_用户裁决状态不明的不可逆步骤(t *testing.T) {
	for _, happened := range []bool{true, false} {
		ctx := context.Background()
		st := memory.New()
		task := runningTask(t, st, core.TrustUser)

		orphan := core.NewStep(task.ID, 1, core.StepTool, json.RawMessage(`{"amount":8800}`))
		orphan.ToolName = "ride.book"
		orphan.SideEffect = core.SideEffectIrreversible
		if err := st.AppendStep(ctx, orphan); err != nil {
			t.Fatalf("写入 pending 步骤失败: %v", err)
		}
		act := &recordingActuator{}
		e := NewEngine(st, &scriptedPlanner{}, act)
		if err := e.Advance(ctx, task.ID); !errors.Is(err, ErrNeedsReconciliation) {
			t.Fatalf("期望 ErrNeedsReconciliation，实际 %v", err)
		}

		if err := Resolve(ctx, st, task.ID, happened); err != nil {
			t.Fatalf("裁决失败: %v", err)
		}
		steps, _ := st.ListSteps(ctx, task.ID)
		want := core.StepFailed
		if happened {
			want = core.StepDone
		}
		if steps[0].Status != want {
			t.Fatalf("happened=%v 时期望步骤 %s，实际 %s", happened, want, steps[0].Status)
		}
		if got := mustGet(t, st, task.ID); got.State != core.StateReady {
			t.Fatalf("裁决后期望 ready，实际 %s", got.State)
		}
		if len(act.keys) != 0 {
			t.Fatalf("裁决本身不得触发执行")
		}
	}
}

func TestAdvance_决策用量计入预算(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser, func(tk *core.Task) {
		tk.Budget = core.Budget{MaxTokens: 100}
	})

	step := Decision{
		Kind: DecideStep, Type: core.StepTool, ToolName: "calendar.query",
		SideEffect: core.SideEffectReadOnly, TokensIn: 70, TokensOut: 30,
	}
	planner := &scriptedPlanner{script: []Decision{step, step}}
	act := &recordingActuator{result: Result{Output: json.RawMessage(`{}`)}}
	e := NewEngine(st, planner, act)

	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("第一步应成功: %v", err)
	}
	if got := mustGet(t, st, task.ID); got.Usage.Tokens != 100 {
		t.Fatalf("决策用量未计入，实际 %d tokens", got.Usage.Tokens)
	}
	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("第二次推进不应报错: %v", err)
	}
	if planner.calls != 1 {
		t.Fatalf("超预算后不应再调用 Planner，实际 %d 次", planner.calls)
	}
	if got := mustGet(t, st, task.ID); got.State != core.StateSuspended {
		t.Fatalf("期望 suspended，实际 %s", got.State)
	}
}

// failingPlanner 每次决策都失败。
type failingPlanner struct{ calls int }

func (p *failingPlanner) Decide(context.Context, *core.Task, []*core.Step) (Decision, error) {
	p.calls++
	return Decision{}, errors.New("模型输出不是合法 JSON")
}

func TestAdvance_决策失败记入日志并退避重试直到上限(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)
	planner := &failingPlanner{}
	e := NewEngine(st, planner, &recordingActuator{})

	for i := 1; i <= maxConsecutiveFailures; i++ {
		if err := e.Advance(ctx, task.ID); err != nil {
			t.Fatalf("第 %d 次推进不应报错: %v", i, err)
		}
		got := mustGet(t, st, task.ID)
		if i < maxConsecutiveFailures {
			if got.State != core.StateWaiting || got.Wake == nil || got.Wake.Kind != core.WakeTime {
				t.Fatalf("第 %d 次失败后期望定时等待，实际 state=%s wake=%+v", i, got.State, got.Wake)
			}
			if err := st.Transition(ctx, task.ID, core.StateWaiting, core.StateReady, "到点"); err != nil {
				t.Fatalf("唤醒失败: %v", err)
			}
			reclaim(t, st)
			continue
		}
		if got.State != core.StateFailed {
			t.Fatalf("连续失败 %d 次后期望 failed，实际 %s", i, got.State)
		}
	}
	steps, _ := st.ListSteps(ctx, task.ID)
	if len(steps) != maxConsecutiveFailures {
		t.Fatalf("每次决策失败都应留痕，实际 %d 条", len(steps))
	}
}

func TestAdvance_被取消的执行保持pending而不是记为失败(t *testing.T) {
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)
	planner := &scriptedPlanner{script: []Decision{{
		Kind: DecideStep, Type: core.StepTool, ToolName: "calendar.query",
		SideEffect: core.SideEffectReadOnly,
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	act := &cancellingActuator{cancel: cancel}
	e := NewEngine(st, planner, act)

	if err := e.Advance(ctx, task.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("期望返回 context.Canceled，实际 %v", err)
	}
	steps, _ := st.ListSteps(context.Background(), task.ID)
	if len(steps) != 1 || steps[0].Status != core.StepPending {
		t.Fatalf("被打断的步骤应保持 pending，实际 %+v", steps)
	}
	if got := mustGet(t, st, task.ID); got.State != core.StateRunning {
		t.Fatalf("被打断不应改变任务状态，实际 %s", got.State)
	}
}

// cancellingActuator 模拟执行途中进程收到停机信号。
type cancellingActuator struct{ cancel context.CancelFunc }

func (a *cancellingActuator) Execute(ctx context.Context, _ *core.Task, _ *core.Step) (Result, error) {
	a.cancel()
	return Result{}, ctx.Err()
}

func TestAdvance_预算超限时挂起且不再执行任何步骤(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser, func(tk *core.Task) {
		tk.Budget = core.Budget{MaxCostCents: 10}
	})

	planner := &scriptedPlanner{script: []Decision{
		{Kind: DecideStep, Type: core.StepLLM, SideEffect: core.SideEffectNone},
		{Kind: DecideStep, Type: core.StepLLM, SideEffect: core.SideEffectNone},
	}}
	act := &recordingActuator{result: Result{Output: json.RawMessage(`{}`), CostCents: 10}}
	e := NewEngine(st, planner, act)

	// 第一步把预算刚好用满。
	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("第一步应成功: %v", err)
	}
	if len(act.keys) != 1 {
		t.Fatalf("期望执行 1 次，实际 %d 次", len(act.keys))
	}

	// 第二步在决策之前就应被预算拦住。
	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("第二次推进不应报错: %v", err)
	}
	if len(act.keys) != 1 {
		t.Fatalf("超预算后不得再执行，实际执行了 %d 次", len(act.keys))
	}
	if planner.calls != 1 {
		t.Fatalf("超预算后不应再消耗模型调用，Planner 被调用了 %d 次", planner.calls)
	}
	got := mustGet(t, st, task.ID)
	if got.State != core.StateSuspended {
		t.Fatalf("期望 suspended，实际 %s", got.State)
	}
}

func TestAdvance_序号以步骤日志为准而不是TCB缓存(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)

	// 模拟"AppendStep 成功但 UpdateTaskRuntime 前崩溃"：日志有 seq=1，TCB 仍是 1。
	first := core.NewStep(task.ID, 1, core.StepLLM, json.RawMessage(`{}`))
	first.SideEffect = core.SideEffectNone
	first.Complete(json.RawMessage(`{}`), core.TrustSystem)
	if err := st.AppendStep(ctx, first); err != nil {
		t.Fatalf("写入步骤失败: %v", err)
	}
	if got := mustGet(t, st, task.ID); got.NextSeq != 1 {
		t.Fatalf("前置条件不成立：NextSeq 应为陈旧值 1，实际 %d", got.NextSeq)
	}

	planner := &scriptedPlanner{script: []Decision{{
		Kind: DecideStep, Type: core.StepLLM, SideEffect: core.SideEffectNone,
	}}}
	e := NewEngine(st, planner, &recordingActuator{})
	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	steps, _ := st.ListSteps(ctx, task.ID)
	if len(steps) != 2 || steps[1].Seq != 2 {
		t.Fatalf("期望新步骤序号为 2，实际 %+v", steps)
	}
}

func TestAdvance_执行失败时记录原因并退避重试而不是终结任务(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)

	query := Decision{
		Kind: DecideStep, Type: core.StepTool, ToolName: "calendar.query",
		SideEffect: core.SideEffectReadOnly,
	}
	planner := &scriptedPlanner{script: []Decision{query, query}}
	act := &recordingActuator{err: errors.New("网络不可达")}
	e := NewEngine(st, planner, act)

	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("推进本身不应报错: %v", err)
	}
	got := mustGet(t, st, task.ID)
	if got.State != core.StateWaiting || got.Wake == nil || got.Wake.At == nil {
		t.Fatalf("单次失败后期望定时等待重试，实际 state=%s wake=%+v", got.State, got.Wake)
	}
	steps, _ := st.ListSteps(ctx, task.ID)
	if steps[0].Status != core.StepFailed || steps[0].Err == "" {
		t.Fatalf("失败步骤必须记录原因，实际 %+v", steps[0])
	}

	// 恢复后的一次成功会清零连续失败计数。
	if err := st.Transition(ctx, task.ID, core.StateWaiting, core.StateReady, "到点"); err != nil {
		t.Fatalf("唤醒失败: %v", err)
	}
	reclaim(t, st)
	act.err = nil
	act.result = Result{Output: json.RawMessage(`{}`), Trust: core.TrustConnector}
	if err := e.Advance(ctx, task.ID); err != nil {
		t.Fatalf("重试应成功: %v", err)
	}
	if got := mustGet(t, st, task.ID); got.State != core.StateRunning {
		t.Fatalf("重试成功后任务应继续运行，实际 %s", got.State)
	}
}

func TestReclaimExpired_租约过期的任务重新入队(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)

	n, err := st.ReclaimExpired(ctx, time.Now().UTC().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("期望回收 1 个任务，实际 %d", n)
	}
	got := mustGet(t, st, task.ID)
	if got.State != core.StateReady || got.LeaseOwner != "" {
		t.Fatalf("期望回到 ready 且释放租约，实际 state=%s owner=%q", got.State, got.LeaseOwner)
	}
}

func TestTransition_并发抢占只有一个成功(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	task := runningTask(t, st, core.TrustUser)

	if err := st.Transition(ctx, task.ID, core.StateRunning, core.StateDone, "第一个"); err != nil {
		t.Fatalf("首次迁移应成功: %v", err)
	}
	err := st.Transition(ctx, task.ID, core.StateRunning, core.StateFailed, "第二个")
	if !errors.Is(err, store.ErrStateConflict) {
		t.Fatalf("期望 ErrStateConflict，实际 %v", err)
	}
}
