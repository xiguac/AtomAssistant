package pg

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/oplog"
	"github.com/xiguac/atomhelper/internal/store"
)

var _ oplog.Store = (*Store)(nil)

// AppendLogs 写入一批操作日志。
func (s *Store) AppendLogs(ctx context.Context, entries []oplog.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range entries {
		if e.ID == "" {
			e.ID = core.NewID()
		}
		batch.Queue(`INSERT INTO op_logs (id, at, level, category, message, attrs) VALUES ($1,$2,$3,$4,$5,$6)`,
			e.ID, e.At, int16(e.Level), string(e.Category),
			s.z.str("op_logs.message", e.Message), s.z.str("op_logs.attrs", e.Attrs))
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("pg: 写入操作日志失败: %w", err)
	}
	return nil
}

// ListLogs 按条件取操作日志，新的在前。
func (s *Store) ListLogs(ctx context.Context, q oplog.Query) ([]oplog.Entry, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	before := q.Before
	if before.IsZero() {
		before = time.Now().Add(time.Hour)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, at, level, category, message, attrs FROM op_logs
		 WHERE at < $1 AND level >= $2 AND ($3 = '' OR category = $3)
		 ORDER BY at DESC, id DESC LIMIT $4`, before, int16(q.MinLevel), string(q.Category), q.Limit)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取操作日志失败: %w", err)
	}
	defer rows.Close()
	var out []oplog.Entry
	for rows.Next() {
		var e oplog.Entry
		var level int16
		var category string
		if err := rows.Scan(&e.ID, &e.At, &level, &category, &e.Message, &e.Attrs); err != nil {
			return nil, fmt.Errorf("pg: 解析操作日志失败: %w", err)
		}
		if err := s.z.open("op_logs.message", &e.Message); err != nil {
			return nil, fmt.Errorf("pg: 解析操作日志失败: %w", err)
		}
		if err := s.z.open("op_logs.attrs", &e.Attrs); err != nil {
			return nil, fmt.Errorf("pg: 解析操作日志失败: %w", err)
		}
		e.Level, e.Category = slog.Level(level), oplog.Category(category)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneLogs 删掉 before 之前的操作日志，返回删了几条。
func (s *Store) PruneLogs(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM op_logs WHERE at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("pg: 清理操作日志失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// auditNames 是审计动作给人看的叫法。
var auditNames = map[string]string{
	"tool.call":               "调用工具",
	"task.transition":         "任务状态变化",
	"step.done":               "步骤完成",
	"step.reconcile_required": "步骤结果待核对",
	"step.reconciled":         "步骤结果已核对",
	"approval.requested":      "等待确认",
	"approval.granted":        "已批准",
	"approval.rejected":       "已驳回",
	"task.budget_raised":      "提高了任务预算",
	"task.epilogue_failed":    "任务收尾失败",
}

// auditLog 把一条审计也记进操作日志：工具调用归"工具调用"，其余的归"任务"。
func auditLog(e store.AuditEntry) oplog.Entry {
	cat := oplog.Task
	if strings.HasPrefix(e.Action, "tool.") {
		cat = oplog.Tool
	}
	msg := auditNames[e.Action]
	if msg == "" {
		msg = e.Action
	}
	level := slog.LevelInfo
	if strings.HasSuffix(e.Action, "_failed") || strings.HasSuffix(e.Action, "_required") {
		level = slog.LevelWarn
	}
	parts := []string{"任务=" + e.TaskID}
	if e.Actor != "" && e.Actor != e.TaskID {
		parts = append(parts, "操作者="+e.Actor)
	}
	if e.Detail != "" {
		parts = append(parts, fmt.Sprintf("详情=%q", e.Detail))
	}
	return oplog.Entry{ID: core.NewID(), At: e.At, Level: level, Category: cat, Message: msg, Attrs: strings.Join(parts, " ")}
}
