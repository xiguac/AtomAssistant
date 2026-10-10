package team

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// MemStore 是 Store 的内存实现，供测试使用。生产环境用 store/pg。
type MemStore struct {
	mu   sync.Mutex
	work []Work
}

// NewMemStore 创建空的内存存储。
func NewMemStore() *MemStore { return &MemStore{} }

var _ Store = (*MemStore)(nil)

func (s *MemStore) PutWork(_ context.Context, w *Work) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.work {
		if s.work[i].ID == w.ID {
			w.Rating, w.RatedBy = s.work[i].Rating, s.work[i].RatedBy
			s.work[i] = *w
			return nil
		}
	}
	s.work = append(s.work, *w)
	return nil
}

func (s *MemStore) WorkOfTask(_ context.Context, taskID string) ([]Work, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Work
	for _, w := range s.work {
		if w.TaskID == taskID {
			out = append(out, w)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StepSeq != out[j].StepSeq {
			return out[i].StepSeq < out[j].StepSeq
		}
		return out[i].Index < out[j].Index
	})
	return out, nil
}

func (s *MemStore) LatestWork(ctx context.Context, taskIDs []string) ([]Work, error) {
	s.mu.Lock()
	latest := ""
	for _, w := range s.work {
		for _, id := range taskIDs {
			// 存入的顺序就是时间顺序，后面的覆盖前面的。
			if w.TaskID == id {
				latest = id
			}
		}
	}
	s.mu.Unlock()
	if latest == "" {
		return nil, nil
	}
	return s.WorkOfTask(ctx, latest)
}

func (s *MemStore) Rate(_ context.Context, id string, rating float64, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.work {
		if s.work[i].ID != id {
			continue
		}
		if by == RatedByInferred && s.work[i].RatedBy == RatedByUser {
			return nil
		}
		s.work[i].Rating, s.work[i].RatedBy = rating, by
		return nil
	}
	return fmt.Errorf("team: 没有编号为 %s 的活", id)
}

func (s *MemStore) Tally(_ context.Context) ([]Tally, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return tally(s.work), nil
}

// tally 把一批活按协作模型和类别累计起来。
func tally(work []Work) []Tally {
	type key struct {
		worker string
		cat    Category
	}
	acc := map[key]*Tally{}
	var order []key
	for _, w := range work {
		k := key{w.Worker, w.Category}
		t, ok := acc[k]
		if !ok {
			t = &Tally{Worker: w.Worker, Category: w.Category}
			acc[k] = t
			order = append(order, k)
		}
		t.Runs++
		if w.Rating > 0 {
			t.Good += w.Rating
		} else {
			t.Bad -= w.Rating
		}
	}
	out := make([]Tally, 0, len(order))
	for _, k := range order {
		out = append(out, *acc[k])
	}
	return out
}
