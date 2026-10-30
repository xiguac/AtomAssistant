package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/core"
)

// ChannelState 是 channel.State 的 Postgres 实现。
type ChannelState struct {
	pool *pgxpool.Pool
	z    sealer
}

// Channels 返回基于同一连接池的渠道状态存储。
func (s *Store) Channels() *ChannelState { return &ChannelState{pool: s.pool, z: s.z} }

var (
	_ channel.State     = (*ChannelState)(nil)
	_ channel.TaskQuery = (*Store)(nil)
)

func (s *ChannelState) GetChat(ctx context.Context, name, chat string) (*channel.ChatState, error) {
	var c channel.ChatState
	err := s.pool.QueryRow(ctx, `
		SELECT channel, chat, last_msg_id, last_msg_at, seq FROM channel_chats
		 WHERE channel = $1 AND chat = $2`, name, s.z.eq("channel_chats.chat", chat)).
		Scan(&c.Channel, &c.Chat, &c.LastMsgID, &c.LastMsgAt, &c.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取会话状态失败: %w", err)
	}
	if err := s.z.opens("channel_chats.chat", &c.Chat, "channel_chats.last_msg_id", &c.LastMsgID); err != nil {
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
		c.Channel, s.z.eq("channel_chats.chat", c.Chat), s.z.str("channel_chats.last_msg_id", c.LastMsgID), at, c.Seq)
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
	if err := s.z.open("channel_notices.fingerprint", &fp); err != nil {
		return "", fmt.Errorf("pg: 读取通知记录失败: %w", err)
	}
	return fp, nil
}

func (s *ChannelState) PutNotice(ctx context.Context, taskID, fingerprint string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO channel_notices (task_id, fingerprint, notified_at) VALUES ($1,$2, now())
		ON CONFLICT (task_id) DO UPDATE SET fingerprint = EXCLUDED.fingerprint, notified_at = now()`,
		taskID, s.z.str("channel_notices.fingerprint", fingerprint))
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
	return s.queryTasks(ctx, sql, s.z.eq("tasks.tags", tag), limit)
}

// TasksChangedWithTagPrefix 返回 since 之后有过变化、且带有某个前缀的标签的任务。
func (s *Store) TasksChangedWithTagPrefix(ctx context.Context, prefix string, since time.Time, limit int) ([]*core.Task, error) {
	// 标签是加密的，数据库没法按前缀匹配：把这段时间里变过的任务按时间顺序分批读出来，
	// 解密后在这里筛。调用方的 since 跟着处理进度往前走，要翻的批数通常只有一两批。
	const page = 200
	sql := `SELECT ` + taskColumns + ` FROM tasks
	         WHERE updated_at > $1
	         ORDER BY updated_at ASC, id LIMIT $2 OFFSET $3`
	out := make([]*core.Task, 0)
	for offset := 0; ; offset += page {
		batch, err := s.queryTasks(ctx, sql, since, page, offset)
		if err != nil {
			return nil, err
		}
		for _, t := range batch {
			for _, tag := range t.Tags {
				if strings.HasPrefix(tag, prefix) {
					out = append(out, t)
					break
				}
			}
			if len(out) == limit {
				return out, nil
			}
		}
		if len(batch) < page {
			return out, nil
		}
	}
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
		t, err := s.z.scanTask(rows)
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
