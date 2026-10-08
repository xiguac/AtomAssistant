// Package kernel 是任务运行时。
//
// 它的职责边界：负责持久化、恢复、幂等、预算与安全策略；
// **不负责决定任务该做什么**——那是 Planner 的事。
package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/store"
)

// DecisionKind 是 Planner 给出的下一步动作类型。
type DecisionKind string

const (
	DecideStep   DecisionKind = "step"   // 执行一个步骤
	DecideWait   DecisionKind = "wait"   // 进入等待
	DecideFinish DecisionKind = "finish" // 任务完成
	DecideFail   DecisionKind = "fail"   // 任务失败
)

// Decision 是 Planner 的输出。它只描述意图，不产生任何副作用。
type Decision struct {
	Kind       DecisionKind
	Type       core.StepType
	ToolName   string
	SideEffect core.SideEffect
	Input      json.RawMessage
	Wake       *core.WakeCondition
	Reason     string

	// TokensIn / TokensOut / CostCents 是**做出这次决策本身**消耗的模型用量。
	// 规划是长任务里最贵的开销，不计入预算的话硬上限就是摆设。
	TokensIn  int64
	TokensOut int64
	CostCents int64
}

// Planner 决定任务的下一步。实现可以是一次 LLM 推理，也可以是固定流程。
type Planner interface {
	Decide(ctx context.Context, t *core.Task, history []*core.Step) (Decision, error)
}

// Result 是一次实际执行的结果。
type Result struct {
	Output    json.RawMessage
	Trust     core.TrustLevel
	TokensIn  int64
	TokensOut int64
	CostCents int64
}

// ApprovalOutcome 是一次动作的审批判定。
type ApprovalOutcome string

const (
	// ApprovalAuto 允许直接执行。
	ApprovalAuto ApprovalOutcome = "auto"
	// ApprovalManual 需要用户在 App 上确认。
	ApprovalManual ApprovalOutcome = "manual"
	// ApprovalDeny 直接拒绝，连审批机会都不给。
	ApprovalDeny ApprovalOutcome = "deny"
)

// Approver 判定一个待执行动作是否需要用户确认。
//
// 内核只认这三种结果，具体规则（金额阈值、提前量、累计帽子）属于 policy 层。
// Approver 为 nil 时内核退回内置的保守规则，见 builtinApproval。
type Approver interface {
	CheckAction(ctx context.Context, t *core.Task, d Decision) (ApprovalOutcome, string, error)
}

// Actuator 执行步骤的实际副作用（调模型、调 MCP 工具、调内建适配器）。
//
// 实现必须把 step.IdempotencyKey 透传给外部服务，
// 否则崩溃重放会造成重复下单、重复打车。
type Actuator interface {
	Execute(ctx context.Context, t *core.Task, s *core.Step) (Result, error)
}

// Subscriber 为等待事件的任务登记订阅，由事件总线实现。
//
// Subscriber 为 nil 时任务不能等待事件，只能等时间。
type Subscriber interface {
	Subscribe(ctx context.Context, taskID string, w *core.WakeCondition) error
}

var (
	// ErrNeedsReconciliation 表示有一个不可逆步骤状态不明，必须由用户裁决。
	ErrNeedsReconciliation = errors.New("kernel: 不可逆步骤状态不明，需人工确认")
	// ErrNotRunning 表示任务不在可推进的状态。
	ErrNotRunning = errors.New("kernel: 任务不处于 running 状态")
)

// Engine 推进任务的执行。
type Engine struct {
	store    store.TaskStore
	planner  Planner
	act      Actuator
	approver Approver
	subs     Subscriber
	now      func() time.Time
}

// NewEngine 构造引擎。approver 为 nil 时使用内置的保守规则。
func NewEngine(s store.TaskStore, p Planner, a Actuator) *Engine {
	return &Engine{store: s, planner: p, act: a, now: func() time.Time { return time.Now().UTC() }}
}

// WithApprover 接入审批策略。
func (e *Engine) WithApprover(a Approver) *Engine {
	e.approver = a
	return e
}

// WithSubscriber 接入事件总线，允许任务等待事件。
func (e *Engine) WithSubscriber(s Subscriber) *Engine {
	e.subs = s
	return e
}

// Advance 把一个 running 任务向前推进一步。
//
// 每次只推进一步是刻意的：每两步之间都有持久化边界，
// 进程可以在任意位置崩溃而不丢状态。
func (e *Engine) Advance(ctx context.Context, taskID string) error {
	t, err := e.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.State != core.StateRunning {
		return fmt.Errorf("%w: 当前 %s", ErrNotRunning, t.State)
	}

	history, err := e.store.ListSteps(ctx, taskID)
	if err != nil {
		return err
	}

	// 1. 先处理上一次崩溃遗留的 pending 步骤。
	if pending := lastPending(history); pending != nil {
		return e.recoverPending(ctx, t, pending)
	}

	// 2. 再处理审批相关的步骤：已批准的按原参数执行，仍在等的回到等待。
	//    这两种情况都不能再问 Planner——用户批准的是那一个动作，不是"下一个动作"。
	if s := lastWithStatus(history, core.StepApproved); s != nil {
		s.Status = core.StepPending
		if err := e.store.SaveStep(ctx, s); err != nil {
			return err
		}
		return e.execute(ctx, t, s)
	}
	if s := lastWithStatus(history, core.StepAwaiting); s != nil {
		t.Wake = &core.WakeCondition{Kind: core.WakeApproval, Ref: s.ID}
		if err := e.store.UpdateTaskRuntime(ctx, t); err != nil {
			return err
		}
		return e.transition(ctx, t, core.StateWaiting,
			fmt.Sprintf("等待用户确认：%s", s.ToolName))
	}

	// 3. 预算与截止时间检查，先于任何新动作。
	done, err := e.checkLimits(ctx, t)
	if err != nil || done {
		return err
	}

	// 4. 询问 Planner 下一步做什么。
	d, err := e.planner.Decide(ctx, t, history)
	if err != nil {
		if ctx.Err() != nil {
			// 进程停止或租约丢失导致的中断不是决策失败，不能记到任务头上。
			return ctx.Err()
		}
		return e.recordPlannerFailure(ctx, t, history, err)
	}
	if err := e.chargeDecision(ctx, t, d); err != nil {
		return err
	}

	switch d.Kind {
	case DecideFinish:
		return e.transition(ctx, t, core.StateDone, orDefault(d.Reason, "任务完成"))
	case DecideFail:
		return e.failTask(ctx, t, orDefault(d.Reason, "Planner 判定失败"))
	case DecideWait:
		if d.Wake == nil {
			return e.failTask(ctx, t, "Planner 要求等待但未给出唤醒条件")
		}
		if d.Wake.Kind == core.WakeEvent {
			if e.subs == nil {
				return e.failTask(ctx, t, "Planner 要求等待事件，但未接入事件总线")
			}
			// 订阅先于进入等待：反过来的话，任务已经睡了而订阅还没建，
			// 这段时间里到来的事件叫不醒任何人。
			if err := e.subs.Subscribe(ctx, t.ID, d.Wake); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return e.recordPlannerFailure(ctx, t, history, fmt.Errorf("登记事件订阅失败: %w", err))
			}
		}
		t.Wake = d.Wake
		if err := e.store.UpdateTaskRuntime(ctx, t); err != nil {
			return err
		}
		return e.transition(ctx, t, core.StateWaiting, orDefault(d.Reason, "等待外部条件"))
	case DecideStep:
		return e.runStep(ctx, t, history, d)
	default:
		return e.failTask(ctx, t, fmt.Sprintf("未知的决策类型 %q", d.Kind))
	}
}

// runStep 执行一个新步骤：先落 pending，再执行，最后落终态。
func (e *Engine) runStep(ctx context.Context, t *core.Task, history []*core.Step, d Decision) error {
	if d.SideEffect == "" {
		return e.failTask(ctx, t, "Planner 未声明步骤的副作用等级")
	}

	outcome, reason, err := e.approve(ctx, t, d)
	if err != nil {
		return e.failTask(ctx, t, fmt.Sprintf("审批判定失败: %v", err))
	}
	switch outcome {
	case ApprovalDeny:
		return e.failTask(ctx, t, "操作被安全策略拒绝: "+reason)
	case ApprovalManual:
		return e.requestApproval(ctx, t, history, d, reason)
	}

	// 序号以步骤日志为准，而不是以 TCB 的缓存字段为准：
	// 若在 AppendStep 之后、UpdateTaskRuntime 之前崩溃，缓存字段会是旧值，
	// 用它分配序号会撞上已存在的记录。
	seq := nextSeq(history)
	s := core.NewStep(t.ID, seq, d.Type, d.Input)
	s.ToolName = d.ToolName
	s.SideEffect = d.SideEffect

	// 关键顺序：执行副作用**之前**必须先把 pending 落库，
	// 否则崩溃后我们连"这一步开始过"都不知道。
	if err := e.store.AppendStep(ctx, s); err != nil {
		return err
	}
	t.NextSeq = seq + 1
	if err := e.store.UpdateTaskRuntime(ctx, t); err != nil {
		return err
	}

	return e.execute(ctx, t, s)
}

// execute 实际执行一个已落库的 pending 步骤，并记录结果与用量。
func (e *Engine) execute(ctx context.Context, t *core.Task, s *core.Step) error {
	res, execErr := e.act.Execute(ctx, t, s)
	if execErr != nil {
		if ctx.Err() != nil {
			// 执行被打断时副作用是否发生是未知的。保持 pending，
			// 交给 recoverPending 按副作用等级决定重放还是挂起。
			return ctx.Err()
		}
		s.Fail(execErr)
		if err := e.store.SaveStep(ctx, s); err != nil {
			return err
		}
		return e.afterFailure(ctx, t, fmt.Sprintf("步骤 %d 执行失败: %v", s.Seq, execErr))
	}

	s.TokensIn, s.TokensOut, s.CostCents = res.TokensIn, res.TokensOut, res.CostCents
	s.Complete(res.Output, res.Trust)
	if err := e.store.SaveStep(ctx, s); err != nil {
		return err
	}

	t.Usage.Tokens += res.TokensIn + res.TokensOut
	t.Usage.CostCents += res.CostCents
	t.Usage.WallClock = e.now().Sub(t.CreatedAt)
	if err := e.store.UpdateTaskRuntime(ctx, t); err != nil {
		return err
	}

	return e.store.Audit(ctx, store.AuditEntry{
		Actor:     t.ID,
		Action:    "step.done",
		TaskID:    t.ID,
		Detail:    fmt.Sprintf("seq=%d type=%s tool=%s effect=%s", s.Seq, s.Type, s.ToolName, s.SideEffect),
		TrustAtOp: s.OutputTrust,
	})
}

// recoverPending 处理崩溃遗留的 pending 步骤。
//
// 我们无法知道副作用是否已经发生。可重放的带幂等键重跑；
// 不可逆的绝不赌——挂起任务，交给用户裁决。
func (e *Engine) recoverPending(ctx context.Context, t *core.Task, s *core.Step) error {
	if !s.SideEffect.ReplaySafe() {
		if err := e.store.Audit(ctx, store.AuditEntry{
			Actor:  "system",
			Action: "step.reconcile_required",
			TaskID: t.ID,
			Detail: fmt.Sprintf("seq=%d tool=%s 执行中断，无法确认是否已生效", s.Seq, s.ToolName),
		}); err != nil {
			return err
		}
		if err := e.transition(ctx, t, core.StateSuspended,
			fmt.Sprintf("步骤 %d（%s）执行中断，需确认是否已生效", s.Seq, s.ToolName)); err != nil {
			return err
		}
		return ErrNeedsReconciliation
	}
	return e.execute(ctx, t, s)
}

// approve 判定一个动作该自动执行、请用户确认还是拒绝。
func (e *Engine) approve(ctx context.Context, t *core.Task, d Decision) (ApprovalOutcome, string, error) {
	if e.approver != nil {
		return e.approver.CheckAction(ctx, t, d)
	}
	return builtinApproval(t, d)
}

// builtinApproval 是没有接入 policy 时的兜底规则。
//
// 它只保留一条不可让渡的硬约束：由 untrusted 来源（截图、邮件、网页）驱动的
// 不可逆操作必须人工确认。见 docs/截图理解与连接器.md §5.2。
func builtinApproval(t *core.Task, d Decision) (ApprovalOutcome, string, error) {
	if d.SideEffect == core.SideEffectIrreversible && !t.Origin.CanTriggerIrreversible() {
		return ApprovalManual,
			fmt.Sprintf("操作由 %s 级内容驱动，不适用任何自动放行", t.Origin), nil
	}
	return ApprovalAuto, "", nil
}

// maxConsecutiveFailures 是任务连续失败多少次后判定为失败。
const maxConsecutiveFailures = 3

// retryBackoff 是第 n 次连续失败后的等待时长。
//
// 网络抖动、上游限流这类故障立刻重试没有意义，等一会儿再让 Planner 看着失败记录重新决策。
func retryBackoff(n int) time.Duration {
	if n <= 1 {
		return 10 * time.Second
	}
	return time.Minute
}

// afterFailure 在一个步骤失败并落库后决定任务的去向。
//
// 单次失败不终结任务：失败步骤留在日志里，Planner 下次决策时能看到并换路。
// 连续失败达到上限才判失败，否则一个持续不可用的依赖会把预算烧光。
func (e *Engine) afterFailure(ctx context.Context, t *core.Task, msg string) error {
	steps, err := e.store.ListSteps(ctx, t.ID)
	if err != nil {
		return err
	}
	n := 0
	for i := len(steps) - 1; i >= 0 && steps[i].Status == core.StepFailed; i-- {
		n++
	}
	if n >= maxConsecutiveFailures {
		return e.failTask(ctx, t, fmt.Sprintf("连续失败 %d 次，最后一次: %s", n, msg))
	}
	backoff := retryBackoff(n)
	at := e.now().Add(backoff)
	t.Wake = &core.WakeCondition{Kind: core.WakeTime, At: &at}
	if err := e.store.UpdateTaskRuntime(ctx, t); err != nil {
		return err
	}
	return e.transition(ctx, t, core.StateWaiting,
		fmt.Sprintf("%s；%s 后重试（第 %d 次失败）", msg, backoff, n))
}

// recordPlannerFailure 把一次决策失败记成失败步骤。
//
// 写进日志有两个作用：失败次数因此可持久化、可累计；
// 用户在时间线上也能看到"模型在这里答非所问"，而不是任务凭空卡住。
func (e *Engine) recordPlannerFailure(ctx context.Context, t *core.Task, history []*core.Step, cause error) error {
	seq := nextSeq(history)
	s := core.NewStep(t.ID, seq, core.StepLLM, nil)
	s.SideEffect = core.SideEffectNone
	s.Fail(fmt.Errorf("决策失败: %w", cause))
	if err := e.store.AppendStep(ctx, s); err != nil {
		return err
	}
	t.NextSeq = seq + 1
	if err := e.store.UpdateTaskRuntime(ctx, t); err != nil {
		return err
	}
	return e.afterFailure(ctx, t, s.Err)
}

// chargeDecision 把决策本身的模型用量计入任务。
func (e *Engine) chargeDecision(ctx context.Context, t *core.Task, d Decision) error {
	if d.TokensIn == 0 && d.TokensOut == 0 && d.CostCents == 0 {
		return nil
	}
	t.Usage.Tokens += d.TokensIn + d.TokensOut
	t.Usage.CostCents += d.CostCents
	t.Usage.WallClock = e.now().Sub(t.CreatedAt)
	return e.store.UpdateTaskRuntime(ctx, t)
}

// requestApproval 把一个待执行的动作转成等待用户审批。
//
// 动作以 awaiting 步骤的形式落库，工具名与参数原样保留。
// 用户批准后引擎执行的就是这一条，见 Advance 与 Approve。
func (e *Engine) requestApproval(ctx context.Context, t *core.Task, history []*core.Step, d Decision, reason string) error {
	seq := nextSeq(history)
	s := core.NewStep(t.ID, seq, d.Type, d.Input)
	s.ToolName = d.ToolName
	s.SideEffect = d.SideEffect
	s.Status = core.StepAwaiting
	if err := e.store.AppendStep(ctx, s); err != nil {
		return err
	}
	t.NextSeq = seq + 1
	approvalID := s.ID
	if err := e.store.Audit(ctx, store.AuditEntry{
		Actor:     t.ID,
		Action:    "approval.requested",
		TaskID:    t.ID,
		Detail:    fmt.Sprintf("tool=%s: %s", d.ToolName, reason),
		TrustAtOp: t.Origin,
	}); err != nil {
		return err
	}
	t.Wake = &core.WakeCondition{Kind: core.WakeApproval, Ref: approvalID}
	if err := e.store.UpdateTaskRuntime(ctx, t); err != nil {
		return err
	}
	return e.transition(ctx, t, core.StateWaiting,
		fmt.Sprintf("等待用户确认：%s（%s）", d.ToolName, reason))
}

// checkLimits 检查预算与截止时间。返回 true 表示任务已因超限而离开 running 状态。
func (e *Engine) checkLimits(ctx context.Context, t *core.Task) (bool, error) {
	usage := t.Usage
	usage.WallClock = e.now().Sub(t.CreatedAt)
	if over, which := t.Budget.Exceeded(usage); over {
		return true, e.transition(ctx, t, core.StateSuspended, fmt.Sprintf("预算超限: %s", which))
	}
	if t.Deadline != nil && e.now().After(*t.Deadline) {
		return true, e.failTask(ctx, t, "已超过截止时间")
	}
	return false, nil
}

func (e *Engine) failTask(ctx context.Context, t *core.Task, reason string) error {
	return e.transition(ctx, t, core.StateFailed, reason)
}

func (e *Engine) transition(ctx context.Context, t *core.Task, to core.TaskState, reason string) error {
	from := t.State
	if err := e.store.Transition(ctx, t.ID, from, to, reason); err != nil {
		return err
	}
	t.State = to
	t.Reason = reason
	return e.store.Audit(ctx, store.AuditEntry{
		Actor:     "system",
		Action:    "task.transition",
		TaskID:    t.ID,
		Detail:    fmt.Sprintf("%s → %s: %s", from, to, reason),
		TrustAtOp: t.Origin,
	})
}

func lastPending(steps []*core.Step) *core.Step {
	return lastWithStatus(steps, core.StepPending)
}

func lastWithStatus(steps []*core.Step, status core.StepStatus) *core.Step {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Status == status {
			return steps[i]
		}
	}
	return nil
}

func nextSeq(steps []*core.Step) int {
	max := 0
	for _, s := range steps {
		if s.Seq > max {
			max = s.Seq
		}
	}
	return max + 1
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
