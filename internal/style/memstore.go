package style

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemStore 是 Store 的内存实现，供测试使用。生产环境用 store/pg。
type MemStore struct {
	mu       sync.Mutex
	samples  []Sample
	profiles map[string]Profile
}

// NewMemStore 创建空的内存存储。
func NewMemStore() *MemStore { return &MemStore{profiles: make(map[string]Profile)} }

var _ Store = (*MemStore)(nil)

func (s *MemStore) AddSample(_ context.Context, in *Sample) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.samples {
		if s.samples[i].Digest() == in.Digest() {
			return false, nil
		}
	}
	s.samples = append(s.samples, *in)
	return true, nil
}

// sorted 返回某个场景的样本，新的在前。同一时刻存入的按存入顺序，后存的在前。
func (s *MemStore) sorted(scene string) []Sample {
	var out []Sample
	for i := len(s.samples) - 1; i >= 0; i-- {
		if s.samples[i].Scene == scene {
			out = append(out, s.samples[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *MemStore) RecentSamples(_ context.Context, scene string, limit int) ([]Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sorted(scene)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemStore) CountSamples(_ context.Context, scene string, after time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, x := range s.samples {
		if x.Scene == scene && x.CreatedAt.After(after) {
			n++
		}
	}
	return n, nil
}

func (s *MemStore) TrimSamples(_ context.Context, scene string, keep int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ordered := s.sorted(scene)
	if len(ordered) <= keep {
		return nil
	}
	drop := make(map[string]bool)
	for _, x := range ordered[keep:] {
		drop[x.ID] = true
	}
	kept := s.samples[:0]
	for _, x := range s.samples {
		if !drop[x.ID] {
			kept = append(kept, x)
		}
	}
	s.samples = kept
	return nil
}

func (s *MemStore) GetProfile(_ context.Context, scene string) (*Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[scene]
	if !ok {
		return nil, nil
	}
	return &p, nil
}

func (s *MemStore) PutProfile(_ context.Context, p *Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles[p.Scene] = *p
	return nil
}

func (s *MemStore) Clear(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples, s.profiles = nil, make(map[string]Profile)
	return nil
}
