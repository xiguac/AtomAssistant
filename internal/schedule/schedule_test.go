package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/store"
)

func TestNextAfter_停机后不补跑只回到节拍上(t *testing.T) {
	base := time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"还没到点", base.Add(-time.Hour), base},
		{"刚到点", base, base.Add(day)},
		{"晚了一小时", base.Add(time.Hour), base.Add(day)},
		// 机器停了三天：不补跑三次，下一次仍在早上八点。
		{"停了三天", base.Add(3*day + 5*time.Hour), base.Add(4 * day)},
	}
	for _, c := range cases {
		if got := NextAfter(base, day, c.now); !got.Equal(c.want) {
			t.Errorf("%s: 期望 %s，实际 %s", c.name, c.want, got)
		}
	}
}

// memStore 是 Store 的内存实现，只供本包测试。
type memStore struct {
	items map[string]*Schedule
	tasks map[string]*core.Task
}

func newMemStore() *memStore {
	return &memStore{items: map[string]*Schedule{}, tasks: map[string]*core.Task{}}
}

func (m *memStore) Add(_ context.Context, s *Schedule) error {
	if err := s.Validate(); err != nil {
		return err
	}
	cp := *s
	m.items[s.ID] = &cp
	return nil
}
func (m *memStore) List(context.Context) ([]*Schedule, error) { return nil, nil }
func (m *memStore) Remove(_ context.Context, id string) error {
	delete(m.items, id)
	return nil
}
func (m *memStore) Due(_ context.Context, now time.Time, _ int) ([]*Schedule, error) {
	var out []*Schedule
	for _, s := range m.items {
		if s.Enabled && !s.NextRunAt.After(now) {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (m *memStore) Fire(_ context.Context, id string, expected, next time.Time, task *core.Task) (bool, error) {
	s := m.items[id]
	if s == nil || !s.NextRunAt.Equal(expected) {
		return false, nil
	}
	s.NextRunAt = next
	if task != nil {
		if err := task.Validate(); err != nil {
			return false, err
		}
		s.LastTaskID = task.ID
		m.tasks[task.ID] = task
	}
	return true, nil
}
func (m *memStore) GetTask(_ context.Context, id string) (*core.Task, error) {
	if t, ok := m.tasks[id]; ok {
		return t, nil
	}
	return nil, store.ErrNotFound
}

func TestRunner_到点创建任务且上一次没结束时不叠加(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	start := time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC)
	sc := New("早报", "看一眼今天的天气和日程", 24*time.Hour, start)
	if err := st.Add(ctx, sc); err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	budget := core.Budget{MaxTokens: 1000}
	r := NewRunner(st, st, budget)

	// 没到点：什么都不做。
	if n, err := r.Tick(ctx, start.Add(-time.Minute)); err != nil || n != 0 {
		t.Fatalf("没到点不应触发: %d, %v", n, err)
	}

	n, err := r.Tick(ctx, start.Add(time.Second))
	if err != nil || n != 1 {
		t.Fatalf("到点应创建 1 个任务: %d, %v", n, err)
	}
	first := st.tasks[st.items[sc.ID].LastTaskID]
	if first == nil || first.State != core.StateReady || first.Title != "早报" {
		t.Fatalf("应建出一个可立即调度的任务: %+v", first)
	}
	// 任务沿用定时任务的来源等级与预算。
	if first.Origin != core.TrustUser || first.Budget != budget {
		t.Fatalf("来源或预算不对: origin=%s budget=%+v", first.Origin, first.Budget)
	}
	if !st.items[sc.ID].NextRunAt.Equal(start.Add(24 * time.Hour)) {
		t.Fatalf("触发后应推进到下一个周期: %s", st.items[sc.ID].NextRunAt)
	}
	// 同一个周期内再检查，不会重复触发。
	if n, _ := r.Tick(ctx, start.Add(time.Minute)); n != 0 {
		t.Fatalf("同一周期不应重复触发，实际 %d", n)
	}

	// 第二天到点，但昨天的任务还卡着没结束：推进时刻，不再叠一个。
	first.State = core.StateWaiting
	if n, _ := r.Tick(ctx, start.Add(24*time.Hour+time.Second)); n != 0 {
		t.Fatalf("上一次没结束时不应再建任务，实际 %d", n)
	}
	if !st.items[sc.ID].NextRunAt.Equal(start.Add(48 * time.Hour)) {
		t.Fatal("即使跳过这一次，触发时刻也要推进，否则每个轮询周期都会重查一遍")
	}

	// 昨天的任务结束了：下一个周期照常触发。
	first.State = core.StateDone
	if n, _ := r.Tick(ctx, start.Add(48*time.Hour+time.Second)); n != 1 {
		t.Fatalf("上一次结束后应恢复触发，实际 %d", n)
	}
}

func TestValidate_拒绝过密的间隔(t *testing.T) {
	sc := New("刷屏", "", 10*time.Second, time.Now())
	if err := sc.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("短于一分钟的间隔应被拒绝: %v", err)
	}
}
