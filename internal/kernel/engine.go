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
	// CostMicros 是不足一分的开销，单位为百万分之一元。见 Cost.Micros。
	CostMicros int64
	// TokensCached 是 TokensIn 里命中模型服务前缀缓存的部分。
	TokensCached int64

	// Then 是紧接着要做的后续步骤，只允许无副作用或只读的。
	//
	// 查五个周末的天气不该问五次模型：每问一次都要把整个上下文重发一遍，
	// 那才是长任务里最贵的开销。写操作不能进这里——每一个写操作之前，
	// 模型都应该先看到前一步的结果再决定。
	Then []Decision
}

// MaxBatch 是一次决策里最多能带的后续步骤数。
const MaxBatch = 8

// Planner 决定任务的下一步。实现可以是一次 LLM 推理，也可以是固定流程。
type Planner interface {
	Decide(ctx context.Context, t *core.Task, history []*core.Step) (Decision, error)
}

// Result 是一次实际执行的结果。
type Result struct {
	Output     json.RawMessage
	Trust      core.TrustLevel
	TokensIn   int64
	TokensOut  int64
	CostCents  int64
	CostMicros int64
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

// Epilogue 在任务完成之后做收尾：提炼记忆、沉淀可复用的流程。
//
// 它在任务已经进入 done 之后调用，失败只记审计，不改变任务结果——
// 收尾没做成不该让一个已经办完的事变成"失败"。返回值是收尾本身消耗的模型用量。
type Epilogue interface {
	TaskFinished(ctx context.Context, t *core.Task, history []*core.Step) (Cost, error)
}

// Cost 是一次模型调用的开销。
type Cost struct {
	TokensIn  int64
	TokensOut int64
	// Micros 是金额，单位为百万分之一元。单次调用常常不到一分钱，
	// 用分记会被舍成 0，累计再多也触不到预算上限。
	Micros int64
}

// InboxMessage 是一条在任务没有等待时到达、尚未送进日志的消息。
type InboxMessage struct {
	ID      string
	Content json.RawMessage
	Trust   core.TrustLevel
}

// Inbox 保存任务不在等待时收到的消息。
//
// 长程任务是断断续续推进的：用户今天补一份材料、过几天再补一份，
// 不会挑任务正好在等的时候才说话。消息先进收件箱，任务下次推进时取走。
type Inbox interface {
	Pending(ctx context.Context, taskID string) ([]InboxMessage, error)
	MarkDelivered(ctx context.Context, ids []string) error
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
	epilogue Epilogue
	inbox    Inbox
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

	// 墙钟只累计真正在跑的时间。长程任务九成时间在等——等用户、等事件、等明天，
	// 把等待算进去的话，一个等了三天的任务一醒来就会被判超限。
	began := e.now()
	err = e.advance(ctx, t)
	t.Usage.WallClock += e.now().Sub(began)
	if uerr := e.store.UpdateTaskRuntime(context.WithoutCancel(ctx), t); uerr != nil && err == nil {
		err = uerr
	}
	return err
}

// WithEpilogue 接入任务完成后的收尾。
func (e *Engine) WithEpilogue(ep Epilogue) *Engine {
	e.epilogue = ep
	return e
}

// WithInbox 接入任务收件箱。
func (e *Engine) WithInbox(in Inbox) *Engine {
	e.inbox = in
	return e
}

func (e *Engine) advance(ctx context.Context, t *core.Task) error {
	taskID := t.ID
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

	// 3. 取走收件箱里的消息，让 Planner 这一轮就能看到。
	if history, err = e.drainInbox(ctx, t, history); err != nil {
		return err
	}

	// 4. 预算与截止时间检查，先于任何新动作。
	done, err := e.checkLimits(ctx, t)
	if err != nil || done {
		return err
	}

	// 5. 询问 Planner 下一步做什么。
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
		if err := e.transition(ctx, t, core.StateDone, orDefault(d.Reason, "任务完成")); err != nil {
			return err
		}
		return e.runEpilogue(ctx, t, history)
	case DecideFail:
		return e.failTask(ctx, t, orDefault(d.Reason, "Planner 判定失败"))
	case DecideWait:
		if d.Wake == nil {
			return e.failTask(ctx, t, "Planner 要求等待但未给出唤醒条件")
		}
		if len(d.Input) > 0 {
			// 等待之前有话要留在日志里（例如向用户提的问题）。
			note, err := e.recordNote(ctx, t, history, d.Input, core.TrustToolOutput)
			if err != nil {
				return err
			}
			history = append(history, note)
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
		if err := e.runStep(ctx, t, history, d); err != nil {
			return err
		}
		return e.runBatch(ctx, t, d.Then)
	default:
		return e.failTask(ctx, t, fmt.Sprintf("未知的决策类型 %q", d.Kind))
	}
}

// runBatch 依次执行一次决策里附带的后续只读步骤。
//
// 每一步照样先落 pending 再执行，所以中途崩溃不丢状态；剩下没跑的不会被记住，
// 重启后由 Planner 看着已有的结果重新决定——它们只是查询，重查没有代价。
// 任何一步让任务离开了 running（失败退避、超预算），后面的就不再做。
func (e *Engine) runBatch(ctx context.Context, t *core.Task, then []Decision) error {
	if len(then) > MaxBatch {
		then = then[:MaxBatch]
	}
	for _, d := range then {
		if t.State != core.StateRunning {
			return nil
		}
		if d.Kind != DecideStep || !readOnly(d.SideEffect) {
			// Planner 的实现有义务不把写操作放进来；真放进来了就跳过，绝不执行。
			continue
		}
		if done, err := e.checkLimits(ctx, t); err != nil || done {
			return err
		}
		history, err := e.store.ListSteps(ctx, t.ID)
		if err != nil {
			return err
		}
		if err := e.runStep(ctx, t, history, d); err != nil {
			return err
		}
	}
	return nil
}

func readOnly(s core.SideEffect) bool {
	return s == core.SideEffectNone || s == core.SideEffectReadOnly
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
		// 用户要看到的不只是"策略为什么拦下"，还有"助手为什么想这么做"。
		if d.Reason != "" {
			reason += assistantWhy + d.Reason
		}
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
	addCost(t, res.CostCents, res.CostMicros)
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

// recordNote 往日志里追加一条已完成的记录步骤，并返回它。
func (e *Engine) recordNote(ctx context.Context, t *core.Task, history []*core.Step, note json.RawMessage, trust core.TrustLevel) (*core.Step, error) {
	seq := nextSeq(history)
	s := core.NewStep(t.ID, seq, core.StepWait, nil)
	s.SideEffect = core.SideEffectNone
	s.Complete(note, trust)
	if err := e.store.AppendStep(ctx, s); err != nil {
		return nil, err
	}
	t.NextSeq = seq + 1
	return s, e.store.UpdateTaskRuntime(ctx, t)
}

// drainInbox 把收件箱里的消息写成步骤。
//
// 先写步骤再标记已送达：中间崩溃的话同一条消息会出现两次，
// 这比丢掉用户补来的材料好得多。
func (e *Engine) drainInbox(ctx context.Context, t *core.Task, history []*core.Step) ([]*core.Step, error) {
	if e.inbox == nil {
		return history, nil
	}
	msgs, err := e.inbox.Pending(ctx, t.ID)
	if err != nil || len(msgs) == 0 {
		return history, err
	}
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		s, err := e.recordNote(ctx, t, history, m.Content, m.Trust)
		if err != nil {
			return history, err
		}
		history = append(history, s)
		ids = append(ids, m.ID)
	}
	return history, e.inbox.MarkDelivered(ctx, ids)
}

// runEpilogue 在任务完成后做收尾。失败不影响任务结果，但必须留痕。
func (e *Engine) runEpilogue(ctx context.Context, t *core.Task, history []*core.Step) error {
	if e.epilogue == nil {
		return nil
	}
	cost, err := e.epilogue.TaskFinished(ctx, t, history)
	t.Usage.Tokens += cost.TokensIn + cost.TokensOut
	addCost(t, 0, cost.Micros)
	if err == nil {
		return nil
	}
	return e.store.Audit(ctx, store.AuditEntry{
		Actor:     "system",
		Action:    "task.epilogue_failed",
		TaskID:    t.ID,
		Detail:    err.Error(),
		TrustAtOp: t.Origin,
	})
}

// microsPerCent 是一分钱对应的百万分之一元数。
const microsPerCent = 10000

// addCost 把一笔开销计入任务。不足一分的部分留在 CostMicros 里继续累计。
func addCost(t *core.Task, cents, micros int64) {
	t.Usage.CostCents += cents
	t.Usage.CostMicros += micros
	t.Usage.CostCents += t.Usage.CostMicros / microsPerCent
	t.Usage.CostMicros %= microsPerCent
}

// chargeDecision 把决策本身的模型用量计入任务。
func (e *Engine) chargeDecision(ctx context.Context, t *core.Task, d Decision) error {
	if d.TokensIn == 0 && d.TokensOut == 0 && d.CostCents == 0 && d.CostMicros == 0 {
		return nil
	}
	t.Usage.Tokens += d.TokensIn + d.TokensOut
	t.Usage.CachedTokens += d.TokensCached
	addCost(t, d.CostCents, d.CostMicros)
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
		fmt.Sprintf("%s%s（%s）", approvalPrefix, d.ToolName, reason))
}

// checkLimits 检查预算与截止时间。返回 true 表示任务已因超限而离开 running 状态。
func (e *Engine) checkLimits(ctx context.Context, t *core.Task) (bool, error) {
	if over, which := t.Budget.Exceeded(t.Usage); over {
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
