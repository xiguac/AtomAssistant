package core

import (
	"errors"
	"fmt"
	"time"
)

// TaskState 是任务的生命周期状态。任务的真身是数据库里的状态机，
// 不是内存里的 goroutine，见 docs/架构方案.md §3.3。
type TaskState string

const (
	StateNew       TaskState = "new"
	StateReady     TaskState = "ready"
	StateRunning   TaskState = "running"
	StateWaiting   TaskState = "waiting"
	StateSuspended TaskState = "suspended"
	StateDone      TaskState = "done"
	StateFailed    TaskState = "failed"
	StateCancelled TaskState = "cancelled"
)

// Terminal 报告状态是否为终态。终态任务不再被调度。
func (s TaskState) Terminal() bool {
	switch s {
	case StateDone, StateFailed, StateCancelled:
		return true
	}
	return false
}

// allowedTransitions 是唯一的状态迁移真值表。
//
// running → ready 看着奇怪，但它是崩溃恢复的入口：实例宕机后，
// 心跳过期的 running 任务会被重新入队，从最后一个 done 的步骤续跑。
var allowedTransitions = map[TaskState][]TaskState{
	StateNew:       {StateReady, StateCancelled},
	StateReady:     {StateRunning, StateSuspended, StateCancelled},
	StateRunning:   {StateWaiting, StateDone, StateFailed, StateSuspended, StateCancelled, StateReady},
	StateWaiting:   {StateReady, StateSuspended, StateFailed, StateCancelled},
	StateSuspended: {StateReady, StateFailed, StateCancelled},
	StateDone:      nil,
	StateFailed:    nil,
	StateCancelled: nil,
}

// ErrInvalidTransition 表示一次非法的状态迁移。
var ErrInvalidTransition = errors.New("core: 非法的任务状态迁移")

// CanTransition 报告 from → to 是否是允许的迁移。
func CanTransition(from, to TaskState) bool {
	for _, s := range allowedTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// ValidateTransition 在迁移非法时返回带上下文的错误。
func ValidateTransition(from, to TaskState) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}
	return nil
}

// WakeKind 是等待中任务的唤醒条件类型。
// WAITING 是核心状态——长程任务 90% 的时间在等，等待中的任务不占运行时资源。
type WakeKind string

const (
	WakeTime     WakeKind = "time"     // 等到某个时间点
	WakeEvent    WakeKind = "event"    // 等某个事件订阅命中
	WakeApproval WakeKind = "approval" // 等用户在 App 上审批
	WakeCallback WakeKind = "callback" // 等外部回调（webhook）
	WakeSubtask  WakeKind = "subtask"  // 等子任务结束
)

// WakeCondition 描述任务为什么在等、什么条件下该被唤醒。
type WakeCondition struct {
	Kind WakeKind `json:"kind"`
	// At 用于 WakeTime；对其他类型也可作为兜底超时。
	At *time.Time `json:"at,omitempty"`
	// Expr 用于 WakeEvent，是事件订阅表达式。
	Expr string `json:"expr,omitempty"`
	// Ref 用于 WakeApproval / WakeCallback / WakeSubtask，指向对应记录的 ID。
	Ref string `json:"ref,omitempty"`
}

// Budget 是任务的三重上限。预算硬上限必须在 P0 就有，
// 否则一个跑飞的长任务能烧掉用户所有的模型额度。
type Budget struct {
	MaxTokens    int64         `json:"max_tokens"`
	MaxCostCents int64         `json:"max_cost_cents"` // 金额一律用分表示，不用浮点
	MaxWallClock time.Duration `json:"max_wall_clock"`
}

// Usage 是任务已消耗的资源。
type Usage struct {
	Tokens    int64 `json:"tokens"`
	CostCents int64 `json:"cost_cents"`
	// CostMicros 是不足一分的零头，单位为百万分之一元，满一分就进位到 CostCents。
	CostMicros int64 `json:"cost_micros"`
	// WallClock 只累计任务真正在执行的时间，不含等待。
	WallClock time.Duration `json:"wall_clock"`
}

// DefaultBudget 是没有特别指定时一个任务的预算。
//
// 墙钟只算执行时间，所以 24 小时是"真正在跑"的上限，等待多久都不占它。
func DefaultBudget() Budget {
	// token 上限按"要上网查十来件事的任务也能跑完"来定：一次联网搜索连同搜到的网页要两三万 token。
	return Budget{MaxTokens: 600_000, MaxCostCents: 500, MaxWallClock: 24 * time.Hour}
}

// Exceeded 报告用量是否已经超出预算，以及超的是哪一项。
// 任一项为 0 表示该项不限。
func (b Budget) Exceeded(u Usage) (bool, string) {
	if b.MaxTokens > 0 && u.Tokens >= b.MaxTokens {
		return true, "tokens"
	}
	if b.MaxCostCents > 0 && u.CostCents >= b.MaxCostCents {
		return true, "cost"
	}
	if b.MaxWallClock > 0 && u.WallClock >= b.MaxWallClock {
		return true, "wall_clock"
	}
	return false, ""
}

// Task 是任务控制块（TCB），对应操作系统里的 PCB。
type Task struct {
	ID       string
	ParentID string // 空表示顶层任务

	Title  string
	Goal   string
	State  TaskState
	Reason string // 进入当前状态的原因，用于 App 上向用户解释

	// Domain 与 Visibility 决定这个任务的产出能不能出实例。
	// 分类不确定时一律按 DomainPersonal 处理，见 docs/协作与联邦.md §4.1。
	Domain           Domain
	Visibility       Visibility
	VisibilityTarget string // 配合 VisibilityCircle / VisibilityUser 使用

	// Origin 是创建这个任务的来源信任等级。由截图、邮件等 untrusted 内容
	// 推导出的任务，其不可逆操作不适用任何自动放行。
	Origin TrustLevel

	Priority int
	Tags     []string

	Budget Budget
	Usage  Usage

	Deadline *time.Time
	Wake     *WakeCondition

	// NextSeq 是下一个步骤的序号。它同时是幂等键的来源，
	// 保证重放不会重复下单、重复打车。
	NextSeq int

	// Lease 是执行租约。实例崩溃后租约到期，任务被重新入队。
	LeaseOwner     string
	LeaseExpiresAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ErrInvalidTask 表示任务字段不合法。
var ErrInvalidTask = errors.New("core: 任务字段不合法")

// Validate 校验任务的必填字段与域/可见性约束。
func (t *Task) Validate() error {
	if t.ID == "" {
		return fmt.Errorf("%w: ID 为空", ErrInvalidTask)
	}
	if t.Title == "" {
		return fmt.Errorf("%w: Title 为空", ErrInvalidTask)
	}
	if !t.Domain.Valid() {
		return fmt.Errorf("%w: 未知的域 %q", ErrInvalidTask, t.Domain)
	}
	if !t.Visibility.Valid() {
		return fmt.Errorf("%w: 未知的可见性 %q", ErrInvalidTask, t.Visibility)
	}
	// 域约束排在用户显式设置之前：私人域的东西要共享，必须先改域再改可见性。
	if !t.Domain.Shareable() && t.Visibility != VisibilityPrivate {
		return fmt.Errorf("%w: %s 域的任务不能设置为 %s 可见",
			ErrInvalidTask, t.Domain, t.Visibility)
	}
	if t.Visibility != VisibilityPrivate && t.VisibilityTarget == "" {
		return fmt.Errorf("%w: 可见性 %s 缺少目标", ErrInvalidTask, t.Visibility)
	}
	if t.State == StateWaiting && t.Wake == nil {
		return fmt.Errorf("%w: waiting 状态必须带唤醒条件", ErrInvalidTask)
	}
	return nil
}

// NewTask 创建一个处于 new 状态的任务，默认私人域、私有可见。
// 默认值刻意选在最保守的一侧：分错方向的代价是不对称的。
func NewTask(title, goal string, origin TrustLevel) *Task {
	now := time.Now().UTC()
	return &Task{
		ID:         NewID(),
		Title:      title,
		Goal:       goal,
		State:      StateNew,
		Domain:     DomainPersonal,
		Visibility: VisibilityPrivate,
		Origin:     origin,
		NextSeq:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}
