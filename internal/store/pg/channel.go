package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomhelper/internal/channel"
	"github.com/xiguac/atomhelper/internal/core"
)

// ChannelState 是 channel.State 的 Postgres 实现。
type ChannelState struct {
	pool *pgxpool.Pool
}

// Channels 返回基于同一连接池的渠道状态存储。
func (s *Store) Channels() *ChannelState { return &ChannelState{pool: s.pool} }

var (
	_ channel.State     = (*ChannelState)(nil)
	_ channel.TaskQuery = (*Store)(nil)
)

func (s *ChannelState) GetChat(ctx context.Context, name, chat string) (*channel.ChatState, error) {
	var c channel.ChatState
	err := s.pool.QueryRow(ctx, `
		SELECT channel, chat, last_msg_id, last_msg_at, seq FROM channel_chats
		 WHERE channel = $1 AND chat = $2`, name, chat).
		Scan(&c.Channel, &c.Chat, &c.LastMsgID, &c.LastMsgAt, &c.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取会话状态失败: %w", err)
	}
	return &c, nil
}

func (s *ChannelState) PutChat(ctx context.Context, c *channel.ChatState) error {
	at := c.LastMsgAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO channel_chats (channel, chat, last_msg_id, last_msg_at, seq)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (channel, chat) DO UPDATE
		   SET last_msg_id = EXCLUDED.last_msg_id, last_msg_at = EXCLUDED.last_msg_at, seq = EXCLUDED.seq`,
		c.Channel, c.Chat, c.LastMsgID, at, c.Seq)
	if err != nil {
		return fmt.Errorf("pg: 保存会话状态失败: %w", err)
	}
	return nil
}

func (s *ChannelState) GetNotice(ctx context.Context, taskID string) (string, error) {
	var fp string
	err := s.pool.QueryRow(ctx, `SELECT fingerprint FROM channel_notices WHERE task_id = $1`, taskID).Scan(&fp)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pg: 读取通知记录失败: %w", err)
	}
	return fp, nil
}

func (s *ChannelState) PutNotice(ctx context.Context, taskID, fingerprint string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO channel_notices (task_id, fingerprint, notified_at) VALUES ($1,$2, now())
		ON CONFLICT (task_id) DO UPDATE SET fingerprint = EXCLUDED.fingerprint, notified_at = now()`,
		taskID, fingerprint)
	if err != nil {
		return fmt.Errorf("pg: 保存通知记录失败: %w", err)
	}
	return nil
}

// TasksByTag 返回带有某个标签的任务，新的在前。
func (s *Store) TasksByTag(ctx context.Context, tag string, activeOnly bool, limit int) ([]*core.Task, error) {
	sql := `SELECT ` + taskColumns + ` FROM tasks WHERE tags @> ARRAY[$1]::text[]`
	if activeOnly {
		sql += ` AND state NOT IN ('done', 'failed', 'cancelled')`
	}
	sql += ` ORDER BY created_at DESC LIMIT $2`
	return s.queryTasks(ctx, sql, tag, limit)
}

// TasksChangedWithTagPrefix 返回 since 之后有过变化、且带有某个前缀的标签的任务。
func (s *Store) TasksChangedWithTagPrefix(ctx context.Context, prefix string, since time.Time, limit int) ([]*core.Task, error) {
	// 前缀来自调用方的常量，但仍把 LIKE 的通配符转义掉，不让它被当成模式。
	pattern := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix) + "%"
	sql := `SELECT ` + taskColumns + ` FROM tasks
	         WHERE updated_at > $1
	           AND EXISTS (SELECT 1 FROM unnest(tags) AS tag WHERE tag LIKE $2)
	         ORDER BY updated_at ASC LIMIT $3`
	return s.queryTasks(ctx, sql, since, pattern, limit)
}

// queryTasks 执行一条返回任务行的查询。
func (s *Store) queryTasks(ctx context.Context, sql string, args ...any) ([]*core.Task, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("pg: 查询任务失败: %w", err)
	}
	defer rows.Close()
	out := make([]*core.Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: 解析任务失败: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 查询任务失败: %w", err)
	}
	return out, nil
}
