// Package schedule 是定时任务：到点就按预设的标题与目标创建一个新任务。
//
// 助手是 7×24 常驻的，很多事不该等用户想起来才做——每天早上看一眼天气和日程、
// 每周一汇总上周的开销。定时任务是这类"主动做事"的最简单形态。
package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
)

// MinEvery 是最短的重复间隔。更密的触发基本都是配置写错了，
// 而每次触发都要花模型的钱。
const MinEvery = time.Minute

// Schedule 是一条定时任务。
type Schedule struct {
	ID    string
	Title string
	Goal  string

	// Every 是重复间隔，NextRunAt 是下一次触发的时刻。
	// "每天早上 8 点"就是 Every=24h、NextRunAt=明早 8 点。
	Every     time.Duration
	NextRunAt time.Time
	Enabled   bool

	// Origin 是创建这条定时任务的来源信任等级，它产生的任务沿用这个等级。
	// 用户亲手建的是 user；将来若允许助手代建，必须是更低的等级——
	// 否则注入内容只要诱使助手建一条定时任务，就得到了一个反复以用户身份执行的指令。
	Origin core.TrustLevel
	Domain core.Domain

	// LastTaskID 是最近一次触发创建的任务。
	LastTaskID string
	CreatedAt  time.Time
}

// ErrInvalid 表示定时任务不合法。
var ErrInvalid = errors.New("schedule: 定时任务不合法")

// Validate 校验一条定时任务。
func (s *Schedule) Validate() error {
	if s.ID == "" || s.Title == "" {
		return fmt.Errorf("%w: ID 与标题不能为空", ErrInvalid)
	}
	if s.Every < MinEvery {
		return fmt.Errorf("%w: 间隔不能短于 %s，实际 %s", ErrInvalid, MinEvery, s.Every)
	}
	if s.NextRunAt.IsZero() {
		return fmt.Errorf("%w: 缺少首次触发时间", ErrInvalid)
	}
	if !s.Domain.Valid() {
		return fmt.Errorf("%w: 未知的域 %q", ErrInvalid, s.Domain)
	}
	return nil
}

// New 构造一条用户创建的定时任务，默认私人域。
func New(title, goal string, every time.Duration, first time.Time) *Schedule {
	return &Schedule{
		ID:        core.NewID(),
		Title:     title,
		Goal:      goal,
		Every:     every,
		NextRunAt: first.UTC(),
		Enabled:   true,
		Origin:    core.TrustUser,
		Domain:    core.DomainPersonal,
		CreatedAt: time.Now().UTC(),
	}
}

// NextAfter 返回 next 之后、严格晚于 now 的第一个触发时刻，保持原有的节拍。
//
// 机器停了三天再起来，不会把错过的三次补跑一遍——
// 三份过期的"今日早报"没有任何用处。只跑最近该跑的那一次，然后回到节拍上。
func NextAfter(next time.Time, every time.Duration, now time.Time) time.Time {
	if every <= 0 {
		return now.Add(MinEvery)
	}
	if next.After(now) {
		return next
	}
	missed := now.Sub(next)/every + 1
	return next.Add(missed * every)
}

// Store 是定时任务的持久化接口。
type Store interface {
	Add(ctx context.Context, s *Schedule) error
	List(ctx context.Context) ([]*Schedule, error)
	Remove(ctx context.Context, id string) error
	// Due 返回已启用且到点的定时任务。
	Due(ctx context.Context, now time.Time, limit int) ([]*Schedule, error)
	// Fire 原子地把触发时刻从 expected 推进到 next，并在 task 非空时创建这个任务。
	//
	// 两件事同成同败：否则会出现"时刻推进了但任务没建"（漏跑），
	// 或者"任务建了但时刻没推进"（重复跑）。时刻已被别人推进时返回 false。
	Fire(ctx context.Context, id string, expected, next time.Time, task *core.Task) (bool, error)
}

// TaskReader 用来查看上一次触发的任务是否还没结束。
type TaskReader interface {
	GetTask(ctx context.Context, id string) (*core.Task, error)
}

// Runner 检查到点的定时任务并触发它们。
type Runner struct {
	store  Store
	tasks  TaskReader
	budget core.Budget
}

// NewRunner 构造触发器。budget 是每个被创建的任务的预算。
func NewRunner(s Store, tasks TaskReader, budget core.Budget) *Runner {
	return &Runner{store: s, tasks: tasks, budget: budget}
}

// Tick 触发所有到点的定时任务，返回创建的任务数。
func (r *Runner) Tick(ctx context.Context, now time.Time) (int, error) {
	due, err := r.store.Due(ctx, now, 32)
	if err != nil {
		return 0, err
	}
	created := 0
	var errs []error
	for _, s := range due {
		next := NextAfter(s.NextRunAt, s.Every, now)

		// 上一次的任务还没结束就不再叠一个：同一件事并发跑两份只会互相打架，
		// 一个卡住的定时任务也不该每个周期多烧一份钱。
		var task *core.Task
		if !r.stillRunning(ctx, s.LastTaskID) {
			task = core.NewTask(s.Title, s.Goal, s.Origin)
			task.Domain = s.Domain
			task.Budget = r.budget
			task.State = core.StateReady
			task.Reason = "定时任务触发"
			task.Tags = []string{"schedule:" + s.ID}
		}
		ok, err := r.store.Fire(ctx, s.ID, s.NextRunAt, next, task)
		if err != nil {
			errs = append(errs, fmt.Errorf("schedule: 触发 %s 失败: %w", s.ID, err))
			continue
		}
		if ok && task != nil {
			created++
		}
	}
	return created, errors.Join(errs...)
}

func (r *Runner) stillRunning(ctx context.Context, taskID string) bool {
	if taskID == "" {
		return false
	}
	t, err := r.tasks.GetTask(ctx, taskID)
	if err != nil {
		// 查不到（任务已被清理）就当它结束了。别的错误也按结束处理：
		// 宁可这一轮多跑一份，也不要让一次读库失败把定时任务永远卡住。
		return false
	}
	return !t.State.Terminal()
}
