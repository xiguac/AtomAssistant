package kernel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/store/memory"
)

// slowPlanner 第一次决策耗时远超租约时长，之后直接结束任务。
type slowPlanner struct {
	delay time.Duration
	calls atomic.Int32
}

func (p *slowPlanner) Decide(ctx context.Context, _ *core.Task, _ []*core.Step) (Decision, error) {
	p.calls.Add(1)
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	}
	return Decision{Kind: DecideFinish}, nil
}

func TestScheduler_慢步骤期间心跳续期不会被重复领取(t *testing.T) {
	st := memory.New()
	task := core.NewTask("慢任务", "一步耗时超过租约", core.TrustUser)
	bg := context.Background()
	if err := st.CreateTask(bg, task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if err := st.Transition(bg, task.ID, core.StateNew, core.StateReady, "入队"); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	planner := &slowPlanner{delay: 600 * time.Millisecond}
	sched := NewScheduler(st, NewEngine(st, planner, &recordingActuator{}), SchedulerConfig{
		Owner:         "test",
		Workers:       2,
		PollInterval:  10 * time.Millisecond,
		LeaseDuration: 150 * time.Millisecond,
	}, nil)

	ctx, cancel := context.WithCancel(bg)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = sched.Run(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := st.GetTask(bg, task.ID)
		if err != nil {
			t.Fatalf("读取任务失败: %v", err)
		}
		if got.State == core.StateDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务未在限时内完成，当前 %s", got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-stopped

	// 没有心跳的话，租约在 150ms 时过期，任务会被回收并第二次进入 Planner。
	if n := planner.calls.Load(); n != 1 {
		t.Fatalf("同一步被执行了 %d 次，说明租约在执行中过期了", n)
	}
}
