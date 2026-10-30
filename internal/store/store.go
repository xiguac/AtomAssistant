// Package store 定义内核的持久化契约。
//
// 任务运行时的所有状态都必须能从这里恢复——内存里不保存任何决定性状态，
// 这是"任务的真身是数据库里的状态机"这条设计的落点。
package store

import (
	"context"
	"errors"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
)

var (
	// ErrNotFound 表示记录不存在。
	ErrNotFound = errors.New("store: 记录不存在")
	// ErrStateConflict 表示状态迁移的前置条件不满足，通常是并发抢占导致。
	ErrStateConflict = errors.New("store: 任务状态已被其他执行者改变")
	// ErrDuplicateSeq 表示同一任务的同一序号被重复写入。
	ErrDuplicateSeq = errors.New("store: 步骤序号重复")
)

// AuditEntry 是一条审计记录。一切留痕：状态迁移、工具调用、授权变更、数据外发。
type AuditEntry struct {
	ID        string
	At        time.Time
	Actor     string // "system" / "user" / 任务 ID
	Action    string // 例如 "task.transition" / "tool.call"
	TaskID    string
	Detail    string
	TrustAtOp core.TrustLevel // 触发这次操作的内容信任等级
}

// TaskStore 是任务与步骤日志的持久化接口。
type TaskStore interface {
	CreateTask(ctx context.Context, t *core.Task) error
	GetTask(ctx context.Context, id string) (*core.Task, error)

	// Transition 以 CAS 方式迁移任务状态：仅当当前状态等于 from 时才写入 to。
	// 并发执行者只有一个能成功，其余得到 ErrStateConflict。
	Transition(ctx context.Context, id string, from, to core.TaskState, reason string) error

	// UpdateTaskRuntime 保存执行过程中变化的运行时字段
	//（用量、下一个序号、唤醒条件、截止时间）。
	UpdateTaskRuntime(ctx context.Context, t *core.Task) error

	// AppendStep 在执行副作用**之前**写入 pending 步骤。
	// 同一 (TaskID, Seq) 重复写入返回 ErrDuplicateSeq。
	AppendStep(ctx context.Context, s *core.Step) error
	// SaveStep 更新一个已存在步骤的终态。
	SaveStep(ctx context.Context, s *core.Step) error
	// ListSteps 按 Seq 升序返回任务的全部步骤。
	ListSteps(ctx context.Context, taskID string) ([]*core.Step, error)

	// ClaimReady 原子地领取若干 ready 任务并加上执行租约。
	ClaimReady(ctx context.Context, owner string, lease time.Duration, limit int) ([]*core.Task, error)
	// RenewLease 续期执行租约。长任务连续跑多步时必须续期，
	// 否则租约到期会被其他 worker 当成崩溃任务重新领取，造成同一任务被并发执行。
	RenewLease(ctx context.Context, taskID, owner string, lease time.Duration) error
	// ReclaimExpired 把租约过期的 running 任务放回 ready，这是崩溃恢复的入口。
	ReclaimExpired(ctx context.Context, now time.Time) (int, error)
	// DueWaiting 返回唤醒时间已到的 waiting 任务。
	DueWaiting(ctx context.Context, now time.Time, limit int) ([]*core.Task, error)
	// WakeWithStep 原子地把一个 waiting 任务放回 ready，并在它的日志末尾追加步骤 s。
	//
	// 用于把"任务为什么醒了"（事件内容）送进任务：序号由存储分配并回填到 s，
	// 调用方传入的 Seq 会被覆盖。任务不在 waiting 时返回 ErrStateConflict 且不写入任何东西——
	// 两件事必须同成同败，否则会出现任务醒了却看不到事件，或者事件写进了一个正在跑的任务。
	WakeWithStep(ctx context.Context, taskID, reason string, s *core.Step) error

	Audit(ctx context.Context, e AuditEntry) error
}
