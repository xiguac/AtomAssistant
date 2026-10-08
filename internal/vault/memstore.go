package vault

import (
	"context"
	"sort"
	"sync"
)

// MemStore 是 Store 的内存实现，供测试使用。生产环境用 store/pg。
type MemStore struct {
	mu    sync.Mutex
	items map[string]Item
}

// NewMemStore 创建空的内存存储。
func NewMemStore() *MemStore { return &MemStore{items: make(map[string]Item)} }

var _ Store = (*MemStore)(nil)

func (s *MemStore) PutItem(_ context.Context, it *Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[it.Label] = *it
	return nil
}

func (s *MemStore) GetItem(_ context.Context, label string) (*Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[label]
	if !ok {
		return nil, ErrNotFound
	}
	return &it, nil
}

func (s *MemStore) ListItems(_ context.Context) ([]*Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Item, 0, len(s.items))
	for _, it := range s.items {
		cp := it
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

func (s *MemStore) DeleteItem(_ context.Context, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[label]; !ok {
		return ErrNotFound
	}
	delete(s.items, label)
	return nil
}
