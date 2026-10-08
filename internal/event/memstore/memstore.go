// Package memstore 是 event.Store 的内存实现，供测试使用。生产环境用 store/pg。
package memstore

import (
	"context"
	"sync"
	"time"

	"github.com/xiguac/atomhelper/internal/event"
)

// Store 是并发安全的内存实现。
type Store struct {
	mu     sync.Mutex
	events []event.Event
	subs   []event.Subscription
}

// New 创建一个空的内存存储。
func New() *Store { return &Store{} }

var _ event.Store = (*Store)(nil)

func (s *Store) SaveEvent(_ context.Context, e *event.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, *e)
	return nil
}

func (s *Store) AddSubscription(_ context.Context, sub *event.Subscription) error {
	if err := sub.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, event.Subscription{
		ID: sub.ID, TaskID: sub.TaskID, TopicPrefix: sub.TopicPrefix, Expr: sub.Expr,
		Once: sub.Once, ExpiresAt: sub.ExpiresAt, Active: sub.Active, CreatedAt: sub.CreatedAt,
	})
	return nil
}

func (s *Store) ActiveSubscriptions(_ context.Context, now time.Time) ([]*event.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*event.Subscription, 0)
	for i := range s.subs {
		sub := s.subs[i]
		if !sub.Active || (sub.ExpiresAt != nil && !now.Before(*sub.ExpiresAt)) {
			continue
		}
		out = append(out, &sub)
	}
	return out, nil
}

func (s *Store) Deactivate(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.subs {
		if s.subs[i].ID == id {
			s.subs[i].Active = false
		}
	}
	return nil
}

func (s *Store) DeactivateForTask(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.subs {
		if s.subs[i].TaskID == taskID {
			s.subs[i].Active = false
		}
	}
	return nil
}

// Events 返回已记录事件的副本，供测试断言。
func (s *Store) Events() []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event.Event(nil), s.events...)
}
