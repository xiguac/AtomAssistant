package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// StepType 是步骤的种类。
type StepType string

const (
	StepLLM     StepType = "llm"     // 一次模型推理
	StepTool    StepType = "tool"    // 一次 MCP 工具调用（查询或执行）
	StepWait    StepType = "wait"    // 进入等待
	StepBranch  StepType = "branch"  // 分支决策
	StepApprove StepType = "approve" // 请求用户审批
)

// SideEffect 是一个步骤对外部世界的影响程度，决定崩溃恢复时能否自动重放。
//
// 这是持久化执行里最关键的一个判断：进程在写入 pending 之后、写入 done 之前崩溃，
// 我们**无法知道副作用是否已经发生**。对只读和可逆操作可以带幂等键重放；
// 对不可逆操作（已经下单、已经发消息）绝不能赌，只能挂起交给用户裁决。
type SideEffect string

const (
	SideEffectNone         SideEffect = "none"         // 纯推理、纯计算
	SideEffectReadOnly     SideEffect = "read_only"    // 查询，重放安全
	SideEffectReversible   SideEffect = "reversible"   // 可撤销的写（建日程、改备注）
	SideEffectIrreversible SideEffect = "irreversible" // 花钱、发消息、签合同
)

// ReplaySafe 报告崩溃恢复时能否自动重放这一步。
func (e SideEffect) ReplaySafe() bool {
	return e == SideEffectNone || e == SideEffectReadOnly || e == SideEffectReversible
}

// StepStatus 是步骤的执行状态。
type StepStatus string

const (
	StepPending StepStatus = "pending" // 已写入但未完成，崩溃恢复时从这里续跑
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
	// StepAwaiting 表示动作已定但在等用户确认。把它写进日志而不是只留一行审计，
	// 是为了批准后能按**原参数**执行，而不是再问一次模型、得到一个没被审过的新动作。
	StepAwaiting StepStatus = "awaiting"
	// StepApproved 表示用户已确认、尚未开始执行。
	StepApproved StepStatus = "approved"
)

// Step 是 append-only 的步骤日志记录。
//
// 每个步骤执行前写 pending，成功后写 done。进程重启后调度器从最后一个 done 的
// seq 续跑，这是整个持久化执行机制的核心。见 docs/架构方案.md §3.3。
type Step struct {
	ID     string
	TaskID string
	Seq    int

	Type   StepType
	Status StepStatus

	// ToolName 仅对 StepTool 有意义。
	ToolName string

	// SideEffect 决定这一步崩溃后能否自动重放。默认取最保守的一档。
	SideEffect SideEffect

	// InputHash 用于检测重放时输入是否发生变化。若续跑时输入变了，
	// 说明上游状态已改变，不能直接复用旧结果。
	InputHash string
	Input     json.RawMessage
	Output    json.RawMessage

	// OutputTrust 是这一步产出内容的信任等级。
	// 工具返回值默认不可信，自由文本字段一律降级为 TrustExternal。
	OutputTrust TrustLevel

	// IdempotencyKey 由 taskID + seq 派生，交给外部工具用于去重。
	// 它保证崩溃重放不会造成重复下单、重复打车。
	IdempotencyKey string

	TokensIn  int64
	TokensOut int64
	CostCents int64

	// Err 记录失败原因。错误必须可见，不允许静默失败。
	Err string

	StartedAt time.Time
	EndedAt   *time.Time
}

// ErrInvalidStep 表示步骤字段不合法。
var ErrInvalidStep = errors.New("core: 步骤字段不合法")

// IdempotencyKeyFor 由任务 ID 与序号派生幂等键。
//
// 用哈希而不是直接拼接，是为了不把内部任务 ID 泄露给第三方工具——
// 幂等键会随请求发给外部服务，见 docs/安全模型.md §4.2 最小化裁剪。
func IdempotencyKeyFor(taskID string, seq int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("atomhelper/idem/%s/%d", taskID, seq)))
	return hex.EncodeToString(sum[:16])
}

// HashInput 计算步骤输入的哈希，用于续跑时的一致性校验。
func HashInput(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:])
}

// NewStep 构造一个 pending 状态的步骤。调用方必须在执行动作**之前**把它落库。
func NewStep(taskID string, seq int, typ StepType, input json.RawMessage) *Step {
	return &Step{
		ID:             NewID(),
		TaskID:         taskID,
		Seq:            seq,
		Type:           typ,
		Status:         StepPending,
		Input:          input,
		InputHash:      HashInput(input),
		IdempotencyKey: IdempotencyKeyFor(taskID, seq),
		OutputTrust:    TrustExternal,
		SideEffect:     SideEffectIrreversible, // 默认最保守，由调用方按工具声明下调
		StartedAt:      time.Now().UTC(),
	}
}

// Complete 把步骤标记为完成。outputTrust 由调用方根据工具的信任上限决定。
func (s *Step) Complete(output json.RawMessage, outputTrust TrustLevel) {
	now := time.Now().UTC()
	s.Status = StepDone
	s.Output = output
	s.OutputTrust = outputTrust
	s.EndedAt = &now
}

// Fail 把步骤标记为失败。err 不能为 nil——静默失败是被禁止的。
func (s *Step) Fail(err error) {
	now := time.Now().UTC()
	s.Status = StepFailed
	if err != nil {
		s.Err = err.Error()
	} else {
		s.Err = "未提供失败原因"
	}
	s.EndedAt = &now
}

// Validate 校验步骤的必填字段。
func (s *Step) Validate() error {
	if s.ID == "" || s.TaskID == "" {
		return fmt.Errorf("%w: ID 或 TaskID 为空", ErrInvalidStep)
	}
	if s.Seq < 1 {
		return fmt.Errorf("%w: Seq 必须从 1 开始，当前 %d", ErrInvalidStep, s.Seq)
	}
	if s.Type == StepTool && s.ToolName == "" {
		return fmt.Errorf("%w: 工具调用步骤缺少 ToolName", ErrInvalidStep)
	}
	if s.Status == StepFailed && s.Err == "" {
		return fmt.Errorf("%w: 失败步骤必须记录原因", ErrInvalidStep)
	}
	if s.SideEffect == "" {
		return fmt.Errorf("%w: 缺少 SideEffect，无法判断能否重放", ErrInvalidStep)
	}
	return nil
}
