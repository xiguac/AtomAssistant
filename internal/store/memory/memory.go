// Package memory 是 store 契约的内存实现。
//
// 它的用途是让内核逻辑（状态机、崩溃恢复、幂等重放）能在没有 Postgres 的情况下
// 被完整测试。生产环境用 store/pg。
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/store"
)

// Store 是并发安全的内存实现。
type Store struct {
	mu     sync.Mutex
	tasks  map[string]core.Task
	steps  map[string][]core.Step // taskID -> steps，按 Seq 有序
	audits []store.AuditEntry
}

// New 创建一个空的内存存储。
func New() *Store {
	return &Store{
		tasks: make(map[string]core.Task),
		steps: make(map[string][]core.Step),
	}
}

var _ store.TaskStore = (*Store)(nil)

func (s *Store) CreateTask(_ context.Context, t *core.Task) error {
	if err := t.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[t.ID]; ok {
		return fmt.Errorf("store: 任务 %s 已存在", t.ID)
	}
	s.tasks[t.ID] = *t
	return nil
}

func (s *Store) GetTask(_ context.Context, id string) (*core.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyTask(t), nil
}

func (s *Store) Transition(_ context.Context, id string, from, to core.TaskState, reason string) error {
	if err := core.ValidateTransition(from, to); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	if t.State != from {
		return fmt.Errorf("%w: 期望 %s，实际 %s", store.ErrStateConflict, from, t.State)
	}
	t.State = to
	t.Reason = reason
	t.UpdatedAt = time.Now().UTC()
	if to.Terminal() || to == core.StateReady {
		// 离开执行态时释放租约，否则重启后这个任务会被误判为仍在运行。
		t.LeaseOwner = ""
		t.LeaseExpiresAt = nil
	}
	s.tasks[id] = t
	return nil
}

func (s *Store) UpdateTaskRuntime(_ context.Context, t *core.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.tasks[t.ID]
	if !ok {
		return store.ErrNotFound
	}
	cur.Usage = t.Usage
	cur.NextSeq = t.NextSeq
	cur.Wake = t.Wake
	cur.Deadline = t.Deadline
	cur.Priority = t.Priority
	cur.UpdatedAt = time.Now().UTC()
	s.tasks[t.ID] = cur
	return nil
}

func (s *Store) AppendStep(_ context.Context, st *core.Step) error {
	if err := st.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.steps[st.TaskID] {
		if existing.Seq == st.Seq {
			return fmt.Errorf("%w: 任务 %s seq=%d", store.ErrDuplicateSeq, st.TaskID, st.Seq)
		}
	}
	s.steps[st.TaskID] = append(s.steps[st.TaskID], *st)
	sort.Slice(s.steps[st.TaskID], func(i, j int) bool {
		return s.steps[st.TaskID][i].Seq < s.steps[st.TaskID][j].Seq
	})
	return nil
}

func (s *Store) SaveStep(_ context.Context, st *core.Step) error {
	if err := st.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.steps[st.TaskID]
	for i := range list {
		if list[i].Seq == st.Seq {
			list[i] = *st
			return nil
		}
	}
	return store.ErrNotFound
}

func (s *Store) ListSteps(_ context.Context, taskID string) ([]*core.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.steps[taskID]
	out := make([]*core.Step, 0, len(list))
	for i := range list {
		cp := list[i]
		out = append(out, &cp)
	}
	return out, nil
}

func (s *Store) ClaimReady(_ context.Context, owner string, lease time.Duration, limit int) ([]*core.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	expires := now.Add(lease)

	ready := make([]core.Task, 0)
	for _, t := range s.tasks {
		if t.State == core.StateReady {
			ready = append(ready, t)
		}
	}
	// 优先级高的先跑；同优先级按创建时间，保证不会饿死老任务。
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Priority != ready[j].Priority {
			return ready[i].Priority > ready[j].Priority
		}
		return ready[i].CreatedAt.Before(ready[j].CreatedAt)
	})

	out := make([]*core.Task, 0, limit)
	for i := range ready {
		if len(out) >= limit {
			break
		}
		t := ready[i]
		t.State = core.StateRunning
		t.LeaseOwner = owner
		t.LeaseExpiresAt = &expires
		t.UpdatedAt = now
		s.tasks[t.ID] = t
		out = append(out, copyTask(t))
	}
	return out, nil
}

func (s *Store) RenewLease(_ context.Context, taskID, owner string, lease time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return store.ErrNotFound
	}
	// 只有持有租约的 worker 能续期。租约已被别人抢走时必须让当前 worker 停手。
	if t.State != core.StateRunning || t.LeaseOwner != owner {
		return fmt.Errorf("%w: 租约不再属于 %s", store.ErrStateConflict, owner)
	}
	expires := time.Now().UTC().Add(lease)
	t.LeaseExpiresAt = &expires
	s.tasks[taskID] = t
	return nil
}

func (s *Store) ReclaimExpired(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, t := range s.tasks {
		if t.State != core.StateRunning || t.LeaseExpiresAt == nil {
			continue
		}
		if t.LeaseExpiresAt.After(now) {
			continue
		}
		t.State = core.StateReady
		t.Reason = "执行租约过期，重新入队"
		t.LeaseOwner = ""
		t.LeaseExpiresAt = nil
		t.UpdatedAt = now
		s.tasks[id] = t
		n++
	}
	return n, nil
}

func (s *Store) DueWaiting(_ context.Context, now time.Time, limit int) ([]*core.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*core.Task, 0)
	for _, t := range s.tasks {
		if t.State != core.StateWaiting || t.Wake == nil || t.Wake.At == nil {
			continue
		}
		if t.Wake.At.After(now) {
			continue
		}
		out = append(out, copyTask(t))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Store) WakeWithStep(_ context.Context, taskID, reason string, st *core.Step) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return store.ErrNotFound
	}
	if t.State != core.StateWaiting {
		return fmt.Errorf("%w: 期望 %s，实际 %s", store.ErrStateConflict, core.StateWaiting, t.State)
	}
	seq := 1
	if list := s.steps[taskID]; len(list) > 0 {
		seq = list[len(list)-1].Seq + 1
	}
	st.TaskID = taskID
	st.Seq = seq
	st.IdempotencyKey = core.IdempotencyKeyFor(taskID, seq)
	if err := st.Validate(); err != nil {
		return err
	}
	s.steps[taskID] = append(s.steps[taskID], *st)

	t.State = core.StateReady
	t.Reason = reason
	t.NextSeq = seq + 1
	t.LeaseOwner = ""
	t.LeaseExpiresAt = nil
	t.UpdatedAt = time.Now().UTC()
	s.tasks[taskID] = t
	return nil
}

func (s *Store) Audit(_ context.Context, e store.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == "" {
		e.ID = core.NewID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	s.audits = append(s.audits, e)
	return nil
}

// Audits 返回审计记录副本，供测试断言。
func (s *Store) Audits() []store.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.AuditEntry(nil), s.audits...)
}

// copyTask 深拷贝任务，避免调用方拿到指向内部状态的指针。
func copyTask(t core.Task) *core.Task {
	cp := t
	if t.Tags != nil {
		cp.Tags = append([]string(nil), t.Tags...)
	}
	if t.Wake != nil {
		w := *t.Wake
		if t.Wake.At != nil {
			at := *t.Wake.At
			w.At = &at
		}
		cp.Wake = &w
	}
	if t.Deadline != nil {
		d := *t.Deadline
		cp.Deadline = &d
	}
	if t.LeaseExpiresAt != nil {
		l := *t.LeaseExpiresAt
		cp.LeaseExpiresAt = &l
	}
	return &cp
}
