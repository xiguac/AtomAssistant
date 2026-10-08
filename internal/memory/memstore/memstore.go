// Package memstore 是 memory.Store 的内存实现。
//
// 用途是让记忆的仲裁、写入管线和检索逻辑能在没有 Postgres 的情况下被完整测试。
// 生产环境用 memory/pgmem。
package memstore

import (
	"context"
	"sort"
	"sync"

	"github.com/xiguac/atomhelper/internal/memory"
)

// Store 是并发安全的内存实现。
type Store struct {
	mu      sync.Mutex
	byID    map[string]memory.Memory
	byKey   map[string]string
	pending []memory.Pending
}

var _ memory.Store = (*Store)(nil)

// New 创建空存储。
func New() *Store {
	return &Store{
		byID:  make(map[string]memory.Memory),
		byKey: make(map[string]string),
	}
}

func (s *Store) Get(_ context.Context, id string) (*memory.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok {
		return nil, memory.ErrNotFound
	}
	return clone(m), nil
}

func (s *Store) FindByKey(_ context.Context, key string) (*memory.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byKey[key]
	if !ok {
		return nil, memory.ErrNotFound
	}
	m := s.byID[id]
	return clone(m), nil
}

func (s *Store) Put(_ context.Context, m *memory.Memory) error {
	if err := m.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 身份键可能随内容变化（如偏好条件被修正），旧键要清掉。
	for k, id := range s.byKey {
		if id == m.ID && k != m.Key() {
			delete(s.byKey, k)
		}
	}
	s.byID[m.ID] = *clone(*m)
	s.byKey[m.Key()] = m.ID
	return nil
}

func (s *Store) List(_ context.Context, q memory.Query) ([]*memory.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*memory.Memory, 0, len(s.byID))
	for _, m := range s.byID {
		if !matches(m, q) {
			continue
		}
		out = append(out, clone(m))
	}
	// 稳定顺序：先按更新时间倒序，再按 ID，避免 map 遍历顺序影响结果。
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (s *Store) PutPending(_ context.Context, p memory.Pending) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, p)
	return nil
}

func (s *Store) ListPending(_ context.Context, subject string) ([]memory.Pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]memory.Pending, 0, len(s.pending))
	for _, p := range s.pending {
		if subject == "" || p.Candidate.Subject == subject {
			out = append(out, p)
		}
	}
	return out, nil
}

func matches(m memory.Memory, q memory.Query) bool {
	if q.Subject != "" && m.Subject != q.Subject {
		return false
	}
	if q.Domain != "" && m.Domain != q.Domain {
		return false
	}
	if q.SafetyOnly && !m.SafetyCritical {
		return false
	}
	if !q.IncludeInactive &&
		(m.Status == memory.StatusArchived || m.Status == memory.StatusExpired) {
		return false
	}
	if len(q.Kinds) > 0 {
		hit := false
		for _, k := range q.Kinds {
			if m.Kind == k {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for _, want := range q.Tags {
		found := false
		for _, have := range m.Tags {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// clone 深拷贝，避免调用方拿到指向内部状态的指针。
func clone(m memory.Memory) *memory.Memory {
	cp := m
	if m.Tags != nil {
		cp.Tags = append([]string(nil), m.Tags...)
	}
	if m.Condition != nil {
		cp.Condition = make(memory.Condition, len(m.Condition))
		for k, v := range m.Condition {
			cp.Condition[k] = v
		}
	}
	if m.History != nil {
		cp.History = append([]memory.Revision(nil), m.History...)
	}
	if m.Current.Evidence != nil {
		cp.Current.Evidence = append([]string(nil), m.Current.Evidence...)
	}
	if m.ValidUntil != nil {
		v := *m.ValidUntil
		cp.ValidUntil = &v
	}
	if m.LastHitAt != nil {
		v := *m.LastHitAt
		cp.LastHitAt = &v
	}
	return &cp
}
