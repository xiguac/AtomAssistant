package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/schedule"
	"github.com/xiguac/atomhelper/internal/store"
)

// ScheduleStore 是 schedule.Store 的 Postgres 实现，与任务存储共用连接池。
type ScheduleStore struct {
	pool *pgxpool.Pool
}

// Schedules 返回基于同一连接池的定时任务存储。
func (s *Store) Schedules() *ScheduleStore { return &ScheduleStore{pool: s.pool} }

var _ schedule.Store = (*ScheduleStore)(nil)

const scheduleColumns = `id, title, goal, every_ns, next_run_at, enabled,
	origin_trust, domain, last_task_id, created_at`

func (s *ScheduleStore) Add(ctx context.Context, sc *schedule.Schedule) error {
	if err := sc.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO schedules (`+scheduleColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		sc.ID, sc.Title, sc.Goal, int64(sc.Every), sc.NextRunAt, sc.Enabled,
		int16(sc.Origin), string(sc.Domain), sc.LastTaskID, sc.CreatedAt)
	if err != nil {
		return fmt.Errorf("pg: 写入定时任务失败: %w", err)
	}
	return nil
}

func (s *ScheduleStore) List(ctx context.Context) ([]*schedule.Schedule, error) {
	return s.query(ctx, `SELECT `+scheduleColumns+` FROM schedules ORDER BY created_at`)
}

func (s *ScheduleStore) Due(ctx context.Context, now time.Time, limit int) ([]*schedule.Schedule, error) {
	return s.query(ctx, `
		SELECT `+scheduleColumns+` FROM schedules
		 WHERE enabled AND next_run_at <= $1
		 ORDER BY next_run_at
		 LIMIT $2`, now, limit)
}

func (s *ScheduleStore) query(ctx context.Context, sql string, args ...any) ([]*schedule.Schedule, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取定时任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]*schedule.Schedule, 0)
	for rows.Next() {
		var (
			sc     schedule.Schedule
			every  int64
			origin int16
			domain string
		)
		if err := rows.Scan(&sc.ID, &sc.Title, &sc.Goal, &every, &sc.NextRunAt, &sc.Enabled,
			&origin, &domain, &sc.LastTaskID, &sc.CreatedAt); err != nil {
			return nil, fmt.Errorf("pg: 解析定时任务失败: %w", err)
		}
		sc.Every = time.Duration(every)
		sc.Origin = core.TrustLevel(origin)
		sc.Domain = core.Domain(domain)
		out = append(out, &sc)
	}
	return out, rows.Err()
}

func (s *ScheduleStore) Remove(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM schedules WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("pg: 删除定时任务失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Fire 在一个事务里推进触发时刻并创建任务。
//
// UPDATE 带着 next_run_at = expected 的条件，相当于一次比较并交换：
// 两个实例同时看到同一条到点的定时任务，只有一个能推进成功、建出任务。
func (s *ScheduleStore) Fire(ctx context.Context, id string, expected, next time.Time, task *core.Task) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	taskID := ""
	if task != nil {
		taskID = task.ID
	}
	tag, err := tx.Exec(ctx, `
		UPDATE schedules
		   SET next_run_at = $3,
		       last_task_id = CASE WHEN $4 = '' THEN last_task_id ELSE $4 END
		 WHERE id = $1 AND next_run_at = $2 AND enabled`,
		id, expected, next, taskID)
	if err != nil {
		return false, fmt.Errorf("pg: 推进定时任务失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if task != nil {
		if err := insertTask(ctx, tx, task); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("pg: 提交定时任务触发失败: %w", err)
	}
	return true, nil
}
