package pg

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5"

	"github.com/xiguac/atomassistant/internal/core"
)

// 每天的会话在任务列表里是一个"容器"任务：当天聊天里交代的事都挂在它下面（parent_id 指向它）。
//
// 它自己不办事，所以不进调度：建出来就是 waiting，唤醒条件是"等子任务"——没有哪段代码会唤醒它。
// 过完那一天由 FinishSession 直接收尾，不经过内核：内核的状态表只管真正在办事的任务。

// OpenSessionTask 返回带 tag 的那个会话任务，没有就按 title、goal 建一个，tags 里要含 tag。
// 同一天的两句话同时进来时只会建出一个。
func (s *Store) OpenSessionTask(ctx context.Context, tag, title, goal string, tags []string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	h := fnv.New64a()
	_, _ = h.Write([]byte("session-task/" + tag))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(h.Sum64())); err != nil {
		return "", fmt.Errorf("pg: 获取会话锁失败: %w", err)
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM tasks WHERE tags @> ARRAY[$1]::text[] ORDER BY created_at LIMIT 1`,
		s.z.eq("tasks.tags", tag)).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("pg: 查找会话任务失败: %w", err)
	}
	t := core.NewTask(title, goal, core.TrustUser)
	t.State = core.StateWaiting
	t.Wake = &core.WakeCondition{Kind: core.WakeSubtask}
	t.Reason = "当天的会话"
	t.Tags = tags
	if err := s.z.insertTask(ctx, tx, t); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("pg: 提交会话任务失败: %w", err)
	}
	return t.ID, nil
}

// ChildTasks 返回挂在 parent 下面的任务，新的在前。activeOnly 为真时不含已结束的。
func (s *Store) ChildTasks(ctx context.Context, parent string, activeOnly bool) ([]*core.Task, error) {
	sql := `SELECT ` + taskColumns + ` FROM tasks WHERE parent_id = $1`
	if activeOnly {
		sql += ` AND ` + "NOT " + endedStates
	}
	return s.queryTasks(ctx, sql+` ORDER BY created_at DESC`, parent)
}

// SetParent 把几个任务挂到 parent 下面。
func (s *Store) SetParent(ctx context.Context, ids []string, parent string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET parent_id = $2, updated_at = now() WHERE id = ANY($1)`, ids, parent); err != nil {
		return fmt.Errorf("pg: 迁移任务失败: %w", err)
	}
	return nil
}

// FinishSession 结束一天的会话任务。只动还在 waiting 的会话任务，已经结束的不再改。
func (s *Store) FinishSession(ctx context.Context, id, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET state = 'done', reason = $2, wake = NULL, wake_at = NULL, updated_at = now()
		 WHERE id = $1 AND state = 'waiting'`, id, s.z.str("tasks.reason", reason))
	if err != nil {
		return fmt.Errorf("pg: 结束会话任务失败: %w", err)
	}
	return nil
}
