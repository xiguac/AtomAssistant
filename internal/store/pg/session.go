package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/session"
)

// SessionStore 是 session.Store 的 Postgres 实现。
type SessionStore struct {
	pool *pgxpool.Pool
	z    sealer
}

// Sessions 返回基于同一连接池的会话存储。
func (s *Store) Sessions() *SessionStore { return &SessionStore{pool: s.pool, z: s.z} }

var _ session.Store = (*SessionStore)(nil)

func (s *SessionStore) Append(ctx context.Context, m session.Message) error {
	if m.ID == "" {
		m.ID = core.NewID()
	}
	if m.At.IsZero() {
		m.At = time.Now().UTC()
	}
	if m.Day == "" {
		m.Day = session.Day(m.At)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_messages (id, day, at, role, channel, task_id, text)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		m.ID, m.Day, m.At, string(m.Role), m.Channel, m.TaskID, s.z.str("session_messages.text", m.Text))
	if err != nil {
		return fmt.Errorf("pg: 记录会话消息失败: %w", err)
	}
	return nil
}

func (s *SessionStore) Messages(ctx context.Context, day string, limit int) ([]session.Message, error) {
	// 先倒着取最后 limit 条，再按时间正过来。
	rows, err := s.pool.Query(ctx, `
		SELECT id, day, at, role, channel, task_id, text FROM (
		    SELECT * FROM session_messages WHERE day = $1 ORDER BY at DESC, id DESC LIMIT $2
		) last ORDER BY at, id`, day, limit)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取会话消息失败: %w", err)
	}
	defer rows.Close()
	out := make([]session.Message, 0)
	for rows.Next() {
		var m session.Message
		var role string
		if err := rows.Scan(&m.ID, &m.Day, &m.At, &role, &m.Channel, &m.TaskID, &m.Text); err != nil {
			return nil, fmt.Errorf("pg: 读取会话消息失败: %w", err)
		}
		if err := s.z.open("session_messages.text", &m.Text); err != nil {
			return nil, fmt.Errorf("pg: 读取会话消息失败: %w", err)
		}
		m.Role = session.Role(role)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 读取会话消息失败: %w", err)
	}
	return out, nil
}

func (s *SessionStore) PutSummary(ctx context.Context, sum session.Summary) error {
	at := sum.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_summaries (day, summary, created_at) VALUES ($1,$2,$3)
		ON CONFLICT (day) DO UPDATE SET summary = EXCLUDED.summary, created_at = EXCLUDED.created_at`,
		sum.Day, s.z.str("session_summaries.summary", sum.Text), at)
	if err != nil {
		return fmt.Errorf("pg: 保存会话摘要失败: %w", err)
	}
	return nil
}

func (s *SessionStore) Summaries(ctx context.Context, from, to string) ([]session.Summary, error) {
	// 日子都写成 2026-10-21 这样等长的文字，按文字比较就是按日期比较。
	rows, err := s.pool.Query(ctx, `
		SELECT day, summary, created_at FROM session_summaries
		 WHERE day >= $1 AND day <= $2 ORDER BY day`, from, to)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取会话摘要失败: %w", err)
	}
	defer rows.Close()
	out := make([]session.Summary, 0)
	for rows.Next() {
		var sum session.Summary
		if err := rows.Scan(&sum.Day, &sum.Text, &sum.At); err != nil {
			return nil, fmt.Errorf("pg: 读取会话摘要失败: %w", err)
		}
		if err := s.z.open("session_summaries.summary", &sum.Text); err != nil {
			return nil, fmt.Errorf("pg: 读取会话摘要失败: %w", err)
		}
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 读取会话摘要失败: %w", err)
	}
	return out, nil
}

func (s *SessionStore) Unsummarized(ctx context.Context, before string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT m.day FROM session_messages m
		 WHERE m.day < $1 AND NOT EXISTS (SELECT 1 FROM session_summaries s WHERE s.day = m.day)
		 ORDER BY m.day`, before)
	if err != nil {
		return nil, fmt.Errorf("pg: 查找没写摘要的会话失败: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			return nil, fmt.Errorf("pg: 查找没写摘要的会话失败: %w", err)
		}
		out = append(out, day)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 查找没写摘要的会话失败: %w", err)
	}
	return out, nil
}
