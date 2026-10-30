package kernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/store"
)

// SchedulerConfig 是调度器的运行参数。
type SchedulerConfig struct {
	// Owner 是本实例的标识，写进租约。同一台机器重启后应保持稳定。
	Owner string
	// Workers 是并发执行任务的 goroutine 数。
	Workers int
	// PollInterval 是空闲时的轮询间隔。
	PollInterval time.Duration
	// LeaseDuration 是执行租约时长。它应当明显大于单步耗时，
	// 又不能太长——崩溃后要等这么久任务才会被回收。
	LeaseDuration time.Duration
	// MaxStepsPerClaim 限制一次领取内连续推进的步数，超过后把任务放回队列。
	// 这是为了公平：一个长任务不能独占 worker 让别的任务饿死。
	MaxStepsPerClaim int
}

func (c *SchedulerConfig) applyDefaults() {
	if c.Owner == "" {
		c.Owner = "atomd-" + core.NewID()[:8]
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 2 * time.Minute
	}
	if c.MaxStepsPerClaim <= 0 {
		c.MaxStepsPerClaim = 16
	}
}

// Scheduler 驱动任务的执行。
//
// 它由三部分组成：
//   - dispatcher：领取 ready 任务，分发给 worker
//   - worker：连续推进一个任务，直到它离开 running 或用完步数配额
//   - janitor：回收过期租约、唤醒到点的等待任务
type Scheduler struct {
	store  store.TaskStore
	engine *Engine
	cfg    SchedulerConfig
	log    *slog.Logger
	cron   Cron
}

// Cron 检查并触发到点的定时任务，返回新建的任务数。由 schedule.Runner 实现。
type Cron interface {
	Tick(ctx context.Context, now time.Time) (int, error)
}

// WithCron 接入定时任务。每个轮询周期检查一次。
func (s *Scheduler) WithCron(c Cron) *Scheduler {
	s.cron = c
	return s
}

// NewScheduler 构造调度器。
func NewScheduler(s store.TaskStore, e *Engine, cfg SchedulerConfig, log *slog.Logger) *Scheduler {
	cfg.applyDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{store: s, engine: e, cfg: cfg, log: log}
}

// Run 阻塞运行调度器，直到 ctx 结束。
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("调度器启动",
		"owner", s.cfg.Owner,
		"workers", s.cfg.Workers,
		"lease", s.cfg.LeaseDuration)

	// 启动时先回收一次过期租约：上一次进程崩溃遗留的 running 任务在这里被救回来。
	if n, err := s.store.ReclaimExpired(ctx, time.Now().UTC()); err != nil {
		return fmt.Errorf("kernel: 启动回收失败: %w", err)
	} else if n > 0 {
		s.log.Warn("回收了上次运行遗留的任务", "数量", n)
	}

	queue := make(chan string, s.cfg.Workers)
	var wg sync.WaitGroup

	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s.worker(ctx, id, queue)
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.janitor(ctx)
	}()

	s.dispatch(ctx, queue)
	close(queue)
	wg.Wait()
	s.log.Info("调度器已停止")
	return nil
}

// dispatch 持续领取 ready 任务并投递给 worker。
func (s *Scheduler) dispatch(ctx context.Context, queue chan<- string) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// 一次最多领取与空闲 worker 数相当的任务，避免领了跑不动、白占租约。
		tasks, err := s.store.ClaimReady(ctx, s.cfg.Owner, s.cfg.LeaseDuration, s.cfg.Workers)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error("领取任务失败", "err", err)
			continue
		}
		for _, t := range tasks {
			select {
			case queue <- t.ID:
			case <-ctx.Done():
				// 已领取但没来得及执行的任务要放回队列，否则要等租约过期才能被别人拿走。
				s.release(context.WithoutCancel(ctx), t.ID, "进程停止，未开始执行")
				return
			}
		}
	}
}

// worker 连续推进一个任务。
func (s *Scheduler) worker(ctx context.Context, id int, queue <-chan string) {
	log := s.log.With("worker", id)
	for taskID := range queue {
		s.drive(ctx, log, taskID)
	}
}

// drive 推进单个任务，直到它离开 running、用完步数配额或进程退出。
func (s *Scheduler) drive(ctx context.Context, log *slog.Logger, taskID string) {
	for step := 0; step < s.cfg.MaxStepsPerClaim; step++ {
		if ctx.Err() != nil {
			// 优雅停机：主动把任务放回队列，下次启动立刻能接着跑，
			// 不必等租约过期。
			s.release(context.WithoutCancel(ctx), taskID, "进程停止，等待重新调度")
			return
		}

		// 每步之前续期。长任务连跑多步时租约不能过期，
		// 否则会被别的 worker 当成崩溃任务重新领取，造成并发执行同一任务。
		if err := s.store.RenewLease(ctx, taskID, s.cfg.Owner, s.cfg.LeaseDuration); err != nil {
			if errors.Is(err, store.ErrStateConflict) {
				log.Warn("租约已失效，停止推进", "task", taskID)
				return
			}
			log.Error("续期租约失败", "task", taskID, "err", err)
			return
		}

		err := s.advanceWithHeartbeat(ctx, log, taskID)
		switch {
		case err == nil:
		case errors.Is(err, ErrNeedsReconciliation):
			// 不是故障，是刻意的挂起：有不可逆操作状态不明，等用户裁决。
			log.Warn("任务挂起待人工确认", "task", taskID)
			return
		case errors.Is(err, ErrNotRunning):
			// 任务已经离开 running（完成、等待或被取消），正常结束。
			return
		case errors.Is(err, store.ErrStateConflict):
			log.Warn("任务状态被并发改变，停止推进", "task", taskID)
			return
		case ctx.Err() != nil:
			// 停机打断了正在执行的一步：放回队列，下次启动由 recoverPending 接手。
			s.release(context.WithoutCancel(ctx), taskID, "进程停止，等待重新调度")
			return
		default:
			log.Error("推进任务失败", "task", taskID, "err", err)
			return
		}

		t, err := s.store.GetTask(ctx, taskID)
		if err != nil {
			log.Error("读取任务失败", "task", taskID, "err", err)
			return
		}
		if t.State != core.StateRunning {
			log.Debug("任务离开运行态", "task", taskID, "state", t.State, "reason", t.Reason)
			return
		}
	}

	// 用完步数配额：放回队列重新排队，给其他任务让路。
	s.release(ctx, taskID, "步数配额用尽，重新排队")
}

// advanceWithHeartbeat 推进一步，并在这一步执行期间持续续期租约。
//
// 一步等于一次模型调用加一次工具调用，耗时可能超过租约时长。
// 只在步与步之间续期的话，慢的一步会让租约过期、任务被回收再领取，
// 结果是同一个任务被两个 worker 并发执行。
func (s *Scheduler) advanceWithHeartbeat(ctx context.Context, log *slog.Logger, taskID string) error {
	stepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.cfg.LeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stepCtx.Done():
				return
			case <-ticker.C:
			}
			err := s.store.RenewLease(stepCtx, taskID, s.cfg.Owner, s.cfg.LeaseDuration)
			switch {
			case err == nil || stepCtx.Err() != nil:
			case errors.Is(err, store.ErrStateConflict):
				// 租约已经不是我们的了，继续执行就是并发执行。立刻叫停这一步。
				log.Warn("执行中租约失效，中止当前步骤", "task", taskID)
				cancel()
				return
			default:
				log.Error("心跳续期失败", "task", taskID, "err", err)
			}
		}
	}()

	err := s.engine.Advance(stepCtx, taskID)
	cancel()
	<-done
	return err
}

// release 把一个仍在 running 的任务放回 ready。
func (s *Scheduler) release(ctx context.Context, taskID, reason string) {
	err := s.store.Transition(ctx, taskID, core.StateRunning, core.StateReady, reason)
	if err != nil && !errors.Is(err, store.ErrStateConflict) && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("放回队列失败", "task", taskID, "err", err)
	}
}

// janitor 做两件周期性工作：回收过期租约、唤醒到点的等待任务。
func (s *Scheduler) janitor(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now().UTC()

		if n, err := s.store.ReclaimExpired(ctx, now); err != nil {
			if ctx.Err() == nil {
				s.log.Error("回收过期租约失败", "err", err)
			}
		} else if n > 0 {
			s.log.Warn("回收了过期租约的任务", "数量", n)
		}

		s.wakeDue(ctx, now)

		if s.cron != nil {
			if n, err := s.cron.Tick(ctx, now); err != nil {
				if ctx.Err() == nil {
					s.log.Error("触发定时任务失败", "err", err)
				}
			} else if n > 0 {
				s.log.Info("定时任务触发", "新建任务数", n)
			}
		}
	}
}

// wakeDue 把唤醒时间已到的 waiting 任务放回 ready。
func (s *Scheduler) wakeDue(ctx context.Context, now time.Time) {
	tasks, err := s.store.DueWaiting(ctx, now, s.cfg.Workers*4)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("查询到期任务失败", "err", err)
		}
		return
	}
	for _, t := range tasks {
		err := s.store.Transition(ctx, t.ID, core.StateWaiting, core.StateReady, "唤醒时间已到")
		if err != nil && !errors.Is(err, store.ErrStateConflict) {
			s.log.Error("唤醒任务失败", "task", t.ID, "err", err)
			continue
		}
		if err == nil {
			s.log.Debug("唤醒等待任务", "task", t.ID)
		}
	}
}
