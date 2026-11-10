package pg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
)

// 后台任务列表的筛选、分页与归档。

const (
	// TaskStatusAll 是全部没归档的任务。
	TaskStatusAll = ""
	// TaskStatusActive 是还没结束的任务，包括等用户处理的。
	TaskStatusActive = "active"
	// TaskStatusEnded 是已经结束的任务：完成、失败、取消，没归档的。
	TaskStatusEnded = "ended"
	// TaskStatusArchived 是归档了的任务。
	TaskStatusArchived = "archived"
)

const endedStates = `state IN ('done', 'failed', 'cancelled')`

// TaskQuery 是一次任务列表查询。
type TaskQuery struct {
	Status string
	// From、To 按创建时间筛选，范围是 [From, To)。零值表示不限。
	From, To      time.Time
	Limit, Offset int
	// Flat 为真时连子任务一起列：聊天里（QQ、Siri 等）交代的事挂在当天的会话下面，是子任务。
	// 网页后台的列表只列顶层、在会话里看那些事；手机 App 的列表要直接看到它们，所以要打开这个。
	Flat bool
}

// QueryTasks 按条件列出任务，新建的在前，同时返回符合条件的总数。
func (s *Store) QueryTasks(ctx context.Context, q TaskQuery) ([]*core.Task, int, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	switch q.Status {
	case TaskStatusAll:
		where = append(where, `archived_at IS NULL`)
	case TaskStatusActive:
		where = append(where, `archived_at IS NULL`, `NOT `+endedStates)
	case TaskStatusEnded:
		where = append(where, `archived_at IS NULL`, endedStates)
	case TaskStatusArchived:
		where = append(where, `archived_at IS NOT NULL`)
	default:
		return nil, 0, fmt.Errorf("pg: 未知的任务筛选 %q", q.Status)
	}
	if !q.From.IsZero() {
		where = append(where, `created_at >= `+arg(q.From))
	}
	if !q.To.IsZero() {
		where = append(where, `created_at < `+arg(q.To))
	}
	// 列表只列顶层：聊天里交代的事挂在当天的会话下面，在会话里看。Flat 时不限。
	if !q.Flat {
		where = append(where, `parent_id IS NULL`)
	}
	cond := ` WHERE ` + strings.Join(where, ` AND `)

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tasks`+cond, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("pg: 统计任务失败: %w", err)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	sql := `SELECT ` + taskColumns + ` FROM tasks` + cond +
		` ORDER BY created_at DESC, id DESC LIMIT ` + arg(limit) + ` OFFSET ` + arg(max(q.Offset, 0))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("pg: 列出任务失败: %w", err)
	}
	defer rows.Close()
	out := make([]*core.Task, 0)
	for rows.Next() {
		t, err := s.z.scanTask(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("pg: 解析任务失败: %w", err)
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// ArchiveTasks 归档几个已经结束的任务（archive 为假时取消归档），返回实际改了几个。
// 没结束的任务不能归档：还在跑的事被收起来，就看不到它在等你处理了。
func (s *Store) ArchiveTasks(ctx context.Context, ids []string, archive bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	sql := `UPDATE tasks SET archived_at = now() WHERE id = ANY($1) AND archived_at IS NULL AND ` + endedStates
	if !archive {
		sql = `UPDATE tasks SET archived_at = NULL WHERE id = ANY($1) AND archived_at IS NOT NULL`
	}
	tag, err := s.pool.Exec(ctx, sql, ids)
	if err != nil {
		return 0, fmt.Errorf("pg: 归档任务失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
