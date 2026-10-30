package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/event"
)

// EventStore 是 event.Store 的 Postgres 实现，与任务存储共用连接池。
type EventStore struct {
	pool *pgxpool.Pool
	z    sealer
}

// Events 返回基于同一连接池的事件存储。
func (s *Store) Events() *EventStore { return &EventStore{pool: s.pool, z: s.z} }

var _ event.Store = (*EventStore)(nil)

func (s *EventStore) SaveEvent(ctx context.Context, e *event.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO events (id, topic, payload, trust, source, occurred_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.ID, e.Topic, s.z.json("events.payload", e.Payload), int16(e.Trust), s.z.str("events.source", e.Source), e.OccurredAt, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("pg: 写入事件失败: %w", err)
	}
	return nil
}

func (s *EventStore) SaveForTask(ctx context.Context, e *event.Event, taskID string) error {
	if err := e.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO events (id, topic, payload, trust, source, occurred_at, created_at, inbox_task_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.ID, e.Topic, s.z.json("events.payload", e.Payload), int16(e.Trust), s.z.str("events.source", e.Source), e.OccurredAt, e.CreatedAt, taskID)
	if err != nil {
		return fmt.Errorf("pg: 写入收件箱失败: %w", err)
	}
	return nil
}

func (s *EventStore) InboxOf(ctx context.Context, taskID string) ([]*event.Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, topic, payload, trust, source, occurred_at, created_at
		  FROM events
		 WHERE inbox_task_id = $1
		 ORDER BY occurred_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取收件箱失败: %w", err)
	}
	defer rows.Close()

	out := make([]*event.Event, 0)
	for rows.Next() {
		var (
			e       event.Event
			payload []byte
			trust   int16
		)
		if err := rows.Scan(&e.ID, &e.Topic, &payload, &trust, &e.Source, &e.OccurredAt, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("pg: 解析收件箱失败: %w", err)
		}
		var err error
		if payload, err = s.z.openJSON("events.payload", payload); err != nil {
			return nil, fmt.Errorf("pg: 解析收件箱失败: %w", err)
		}
		if err := s.z.open("events.source", &e.Source); err != nil {
			return nil, fmt.Errorf("pg: 解析收件箱失败: %w", err)
		}
		e.Payload = payload
		e.Trust = core.TrustLevel(trust)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *EventStore) ClearInbox(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE events SET inbox_task_id = NULL WHERE id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("pg: 清理收件箱失败: %w", err)
	}
	return nil
}

func (s *EventStore) AddSubscription(ctx context.Context, sub *event.Subscription) error {
	if err := sub.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO event_subscriptions
		       (id, task_id, topic_prefix, expr, once, expires_at, active, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		sub.ID, sub.TaskID, sub.TopicPrefix, s.z.str("event_subscriptions.expr", sub.Expr), sub.Once, sub.ExpiresAt, sub.Active, sub.CreatedAt)
	if err != nil {
		return fmt.Errorf("pg: 写入订阅失败: %w", err)
	}
	return nil
}

func (s *EventStore) ActiveSubscriptions(ctx context.Context, now time.Time) ([]*event.Subscription, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, task_id, topic_prefix, expr, once, expires_at, active, created_at
		  FROM event_subscriptions
		 WHERE active AND (expires_at IS NULL OR expires_at > $1)
		 ORDER BY created_at`, now)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取订阅失败: %w", err)
	}
	defer rows.Close()

	out := make([]*event.Subscription, 0)
	for rows.Next() {
		var sub event.Subscription
		if err := rows.Scan(&sub.ID, &sub.TaskID, &sub.TopicPrefix, &sub.Expr,
			&sub.Once, &sub.ExpiresAt, &sub.Active, &sub.CreatedAt); err != nil {
			return nil, fmt.Errorf("pg: 解析订阅失败: %w", err)
		}
		if err := s.z.open("event_subscriptions.expr", &sub.Expr); err != nil {
			return nil, fmt.Errorf("pg: 解析订阅失败: %w", err)
		}
		out = append(out, &sub)
	}
	return out, rows.Err()
}

func (s *EventStore) Deactivate(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE event_subscriptions SET active = false WHERE id = $1`, id); err != nil {
		return fmt.Errorf("pg: 作废订阅失败: %w", err)
	}
	return nil
}

func (s *EventStore) DeactivateForTask(ctx context.Context, taskID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE event_subscriptions SET active = false WHERE task_id = $1 AND active`, taskID); err != nil {
		return fmt.Errorf("pg: 作废任务订阅失败: %w", err)
	}
	return nil
}

// 编译期确认任务存储满足事件总线的唤醒接口。
var _ event.TaskWaker = (*Store)(nil)
