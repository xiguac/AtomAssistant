package matter

import (
	"context"
	"sync"
)

// MemStore 是放在内存里的 Store，测试用。
type MemStore struct {
	mu    sync.Mutex
	items map[string]*Matter
}

// NewMemStore 构造一个空的内存存储。
func NewMemStore() *MemStore { return &MemStore{items: make(map[string]*Matter)} }

var _ Store = (*MemStore)(nil)

func clone(m *Matter) *Matter {
	cp := *m
	cp.Notes = append([]Note(nil), m.Notes...)
	if m.AskedAt != nil {
		at := *m.AskedAt
		cp.AskedAt = &at
	}
	return &cp
}

func (s *MemStore) Put(_ context.Context, m *Matter) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[m.ID] = clone(m)
	return nil
}

func (s *MemStore) Get(_ context.Context, id string) (*Matter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.items[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(m), nil
}

func (s *MemStore) List(_ context.Context) ([]*Matter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Matter, 0, len(s.items))
	for _, m := range s.items {
		out = append(out, clone(m))
	}
	return out, nil
}

func (s *MemStore) Delete(_ context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.items, id)
	}
	return nil
}
