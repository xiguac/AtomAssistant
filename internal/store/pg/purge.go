package pg

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PurgeSpec 指明要彻底清掉的记录，都按编号给。
type PurgeSpec struct {
	// Tasks 是任务的编号。只清已经结束的；里面有没结束的，整次清理不做。
	Tasks []string
	// Memories 是记忆的编号，Pending 是等用户确认的候选的编号。
	Memories []string
	Pending  []string
}

// PurgeResult 是各类记录清掉（或者将会清掉）了多少条。
type PurgeResult struct {
	Tasks, Steps, Images, Audit, Works, Notices, Samples int
	Memories, Revisions, Pending                         int
}

// Purge 彻底清掉指定的任务、记忆和候选，连同只属于它们的附带记录。dryRun 为真时只数不删。
//
// 这和平常的"删除已结束的任务"不是一回事。那个只删任务自己的记录，任务办完时落下的东西都留着；
// 这个是要把一件事在库里的痕迹清干净，好让同一件事从头再来一遍：
// 任务的步骤、图片、通知记录，它的审计日志和评分记录，从它的对话里学到的说话样本，
// 以及点名要清的记忆（连同版本链）和候选。
//
// 全部在一个事务里做：要么都清掉，要么一条都不动。
func (s *Store) Purge(ctx context.Context, spec PurgeSpec, dryRun bool) (PurgeResult, error) {
	var res PurgeResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	count := func(dst *int, sql string, args ...any) error {
		if err := tx.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			return fmt.Errorf("pg: 清点记录失败: %w", err)
		}
		return nil
	}
	del := func(sql string, args ...any) error {
		if dryRun {
			return nil
		}
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			return fmt.Errorf("pg: 清理记录失败: %w", err)
		}
		return nil
	}

	if len(spec.Tasks) > 0 {
		// 先看清对象：编号都得在，而且都得是结束了的。
		rows, err := tx.Query(ctx, `SELECT id, state FROM tasks WHERE id = ANY($1)`, spec.Tasks)
		if err != nil {
			return res, fmt.Errorf("pg: 读取任务失败: %w", err)
		}
		found := map[string]string{}
		for rows.Next() {
			var id, state string
			if err := rows.Scan(&id, &state); err != nil {
				rows.Close()
				return res, fmt.Errorf("pg: 读取任务失败: %w", err)
			}
			found[id] = state
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, fmt.Errorf("pg: 读取任务失败: %w", err)
		}
		for _, id := range spec.Tasks {
			switch state, ok := found[id]; {
			case !ok:
				return res, fmt.Errorf("pg: 没有编号为 %s 的任务", id)
			case state != "done" && state != "failed" && state != "cancelled":
				return res, fmt.Errorf("pg: 任务 %s 还没有结束（%s），不清理", id, state)
			}
		}
		res.Tasks = len(found)
		for _, c := range []struct {
			dst *int
			sql string
		}{
			{&res.Steps, `SELECT count(*) FROM task_steps WHERE task_id = ANY($1)`},
			{&res.Images, `SELECT count(*) FROM task_images WHERE task_id = ANY($1)`},
			{&res.Audit, `SELECT count(*) FROM audit_log WHERE task_id = ANY($1)`},
			{&res.Works, `SELECT count(*) FROM team_work WHERE task_id = ANY($1)`},
			{&res.Notices, `SELECT count(*) FROM channel_notices WHERE task_id = ANY($1)`},
		} {
			if err := count(c.dst, c.sql, spec.Tasks); err != nil {
				return res, err
			}
		}
		// 说话样本的来源是加密存的，没法在库里按值筛，取出来一条条对。
		samples, err := s.samplesOf(ctx, tx, spec.Tasks)
		if err != nil {
			return res, err
		}
		res.Samples = len(samples)
		// 步骤、图片、收件箱里的消息和事件订阅有外键，随任务一起删。
		for _, sql := range []string{
			`DELETE FROM audit_log WHERE task_id = ANY($1)`,
			`DELETE FROM team_work WHERE task_id = ANY($1)`,
			`DELETE FROM channel_notices WHERE task_id = ANY($1)`,
			`DELETE FROM tasks WHERE id = ANY($1)`,
		} {
			if err := del(sql, spec.Tasks); err != nil {
				return res, err
			}
		}
		if len(samples) > 0 {
			if err := del(`DELETE FROM style_samples WHERE id = ANY($1)`, samples); err != nil {
				return res, err
			}
		}
	}

	if len(spec.Memories) > 0 {
		if err := count(&res.Memories, `SELECT count(*) FROM memories WHERE id = ANY($1)`, spec.Memories); err != nil {
			return res, err
		}
		if res.Memories != len(spec.Memories) {
			return res, fmt.Errorf("pg: 要清的记忆有 %d 条，库里只找到 %d 条，编号有误", len(spec.Memories), res.Memories)
		}
		if err := count(&res.Revisions, `SELECT count(*) FROM memory_revisions WHERE memory_id = ANY($1)`, spec.Memories); err != nil {
			return res, err
		}
		// 版本链和与它冲突的候选有外键，随记忆一起删。
		if err := del(`DELETE FROM memories WHERE id = ANY($1)`, spec.Memories); err != nil {
			return res, err
		}
	}

	if len(spec.Pending) > 0 {
		if err := count(&res.Pending, `SELECT count(*) FROM memory_pending WHERE id = ANY($1)`, spec.Pending); err != nil {
			return res, err
		}
		if res.Pending != len(spec.Pending) {
			return res, fmt.Errorf("pg: 要清的候选有 %d 条，库里只找到 %d 条，编号有误", len(spec.Pending), res.Pending)
		}
		if err := del(`DELETE FROM memory_pending WHERE id = ANY($1)`, spec.Pending); err != nil {
			return res, err
		}
	}

	if dryRun {
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("pg: 提交清理失败: %w", err)
	}
	return res, nil
}

// samplesOf 找出从这些任务的对话里学来的说话样本的编号。
func (s *Store) samplesOf(ctx context.Context, tx pgx.Tx, tasks []string) ([]string, error) {
	want := make(map[string]bool, len(tasks))
	for _, id := range tasks {
		want[id] = true
	}
	rows, err := tx.Query(ctx, `SELECT id, ref FROM style_samples`)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取说话样本失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, ref string
		if err := rows.Scan(&id, &ref); err != nil {
			return nil, fmt.Errorf("pg: 读取说话样本失败: %w", err)
		}
		if err := s.z.open("style_samples.ref", &ref); err != nil {
			return nil, fmt.Errorf("pg: 读取说话样本失败: %w", err)
		}
		// 来源形如 task:任务ID。
		if task, ok := strings.CutPrefix(ref, "task:"); ok && want[task] {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}
