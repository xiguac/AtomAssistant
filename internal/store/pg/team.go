package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomhelper/internal/team"
)

// TeamStore 是 team.Store 的 Postgres 实现。
type TeamStore struct {
	pool *pgxpool.Pool
}

// Team 返回基于同一连接池的协作记录存储。
func (s *Store) Team() *TeamStore { return &TeamStore{pool: s.pool} }

var _ team.Store = (*TeamStore)(nil)

func (s *TeamStore) PutWork(ctx context.Context, w *team.Work) error {
	// 同一步重跑时可能换了协作模型，记录跟着换；评价留着不动。
	_, err := s.pool.Exec(ctx, `
		INSERT INTO team_work (id, task_id, step_seq, idx, worker, category, title, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (id) DO UPDATE
		   SET worker = EXCLUDED.worker, category = EXCLUDED.category, title = EXCLUDED.title`,
		w.ID, w.TaskID, w.StepSeq, w.Index, w.Worker, string(w.Category), w.Title, w.CreatedAt)
	if err != nil {
		return fmt.Errorf("pg: 记录派出去的活失败: %w", err)
	}
	return nil
}

const teamWorkColumns = `id, task_id, step_seq, idx, worker, category, title, rating, rated_by, created_at`

func (s *TeamStore) queryWork(ctx context.Context, sql string, args ...any) ([]team.Work, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取派出去的活失败: %w", err)
	}
	defer rows.Close()
	var out []team.Work
	for rows.Next() {
		var w team.Work
		var cat string
		if err := rows.Scan(&w.ID, &w.TaskID, &w.StepSeq, &w.Index, &w.Worker, &cat, &w.Title,
			&w.Rating, &w.RatedBy, &w.CreatedAt); err != nil {
			return nil, fmt.Errorf("pg: 读取派出去的活失败: %w", err)
		}
		w.Category = team.Category(cat)
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 读取派出去的活失败: %w", err)
	}
	return out, nil
}

func (s *TeamStore) WorkOfTask(ctx context.Context, taskID string) ([]team.Work, error) {
	return s.queryWork(ctx, `SELECT `+teamWorkColumns+` FROM team_work
		 WHERE task_id = $1 ORDER BY step_seq, idx`, taskID)
}

func (s *TeamStore) LatestWork(ctx context.Context, taskIDs []string) ([]team.Work, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	return s.queryWork(ctx, `SELECT `+teamWorkColumns+` FROM team_work
		 WHERE task_id = (SELECT task_id FROM team_work WHERE task_id = ANY($1)
		                   ORDER BY created_at DESC, id DESC LIMIT 1)
		 ORDER BY step_seq, idx`, taskIDs)
}

func (s *TeamStore) Rate(ctx context.Context, id string, rating float64, by string) error {
	// 推断出来的评价不覆盖用户亲口给的。
	tag, err := s.pool.Exec(ctx, `
		UPDATE team_work SET rating = $2, rated_by = $3
		 WHERE id = $1 AND NOT ($3 = 'inferred' AND rated_by = 'user')`, id, rating, by)
	if err != nil {
		return fmt.Errorf("pg: 保存评价失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM team_work WHERE id = $1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("pg: 保存评价失败: %w", err)
		}
		if !exists {
			return fmt.Errorf("pg: 没有编号为 %s 的活", id)
		}
	}
	return nil
}

func (s *TeamStore) Tally(ctx context.Context) ([]team.Tally, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT worker, category, count(*),
		       coalesce(sum(rating) FILTER (WHERE rating > 0), 0),
		       coalesce(-sum(rating) FILTER (WHERE rating < 0), 0)
		  FROM team_work GROUP BY worker, category`)
	if err != nil {
		return nil, fmt.Errorf("pg: 统计协作成绩失败: %w", err)
	}
	defer rows.Close()
	var out []team.Tally
	for rows.Next() {
		var t team.Tally
		var cat string
		if err := rows.Scan(&t.Worker, &cat, &t.Runs, &t.Good, &t.Bad); err != nil {
			return nil, fmt.Errorf("pg: 统计协作成绩失败: %w", err)
		}
		t.Category = team.Category(cat)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 统计协作成绩失败: %w", err)
	}
	return out, nil
}
