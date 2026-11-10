// Package pg 是 store 契约的 PostgreSQL 实现。
//
// 任务状态与业务数据同库，这是不引入 Temporal 而自研步骤日志换来的最大好处：
// 出问题时可以直接用 SQL 排障，不需要另一套控制面。
package pg

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/oplog"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/store"
	"log/slog"
)

//go:embed all:migrations
var migrationFS embed.FS

// Store 用连接池访问 Postgres。
type Store struct {
	pool *pgxpool.Pool
	// z 负责内容列的加解密，见 sealed.go。
	z sealer
	// memHook 在一条记忆写入后被调用，见 OnMemoryChange。
	memHook *atomic.Pointer[func(id string)]
}

// OnMemoryChange 登记一个回调：每写入一条记忆（新增、修改、归档）就以它的编号调用一次。
// 回调在写库的那个调用里执行，不能阻塞，要做的事自己放到后台去做。
// 同一时间只有一个回调；返回的函数取消登记，若已被后来者换掉则什么都不做。
func (s *Store) OnMemoryChange(f func(id string)) (cancel func()) {
	p := &f
	s.memHook.Store(p)
	return func() { s.memHook.CompareAndSwap(p, nil) }
}

var _ store.TaskStore = (*Store)(nil)

// Open 建立连接池并验证连通性。
//
// box 是内容列加解密用的密钥，必须提供：没有它，写进去的就是明文。
func Open(ctx context.Context, dsn string, box *seal.Box) (*Store, error) {
	if box == nil {
		return nil, fmt.Errorf("pg: 没有提供数据密钥，拒绝以明文方式打开数据库")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: 解析 DSN 失败: %w", err)
	}
	// 自部署目标机器可能只有 2C4G，连接数要克制。
	cfg.MaxConns = 8
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: 建立连接池失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: 数据库不可达: %w", err)
	}
	return &Store{pool: pool, z: sealer{box: box}, memHook: new(atomic.Pointer[func(string)])}, nil
}

// Close 关闭连接池。
func (s *Store) Close() { s.pool.Close() }

// Migrate 顺序执行内嵌的迁移脚本。
//
// 迁移记录表本身用 IF NOT EXISTS 创建，所以首次运行也安全。
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("pg: 创建迁移记录表失败: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("pg: 读取迁移目录失败: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&exists)
		if err != nil {
			return fmt.Errorf("pg: 查询迁移状态失败: %w", err)
		}
		if exists {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("pg: 读取迁移 %s 失败: %w", name, err)
		}
		// 每个迁移单独一个事务：失败时不会留下半套 schema。
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("pg: 开启事务失败: %w", err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("pg: 执行迁移 %s 失败: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("pg: 记录迁移 %s 失败: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("pg: 提交迁移 %s 失败: %w", name, err)
		}
	}
	// 表结构就绪之后，确认库里的内容是用当前这把密钥加密的；升级前留下的明文在这里补加密。
	return s.ensureSealed(ctx)
}

// ResetForTest 删除全部表与数据。
//
// **仅供测试使用。** 它会不可逆地清空整个 schema，
// 任何生产代码路径都不得调用。之所以放在非 _test.go 文件里，
// 是因为契约测试位于外部测试包，只能访问导出标识符。
func (s *Store) ResetForTest(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		DROP TABLE IF EXISTS audit_log, task_steps, tasks,
			memory_pending, memory_revisions, memories,
			event_subscriptions, events, vault_items, schedules, skills, mcp_installs,
			style_samples, style_profiles, channel_chats, channel_notices, team_work,
			seal_state, settings, task_images, op_logs, session_messages, session_summaries, matters, share_links, llm_usage, device_data, device_requests, devices, schema_migrations CASCADE`)
	if err != nil {
		return fmt.Errorf("pg: 重置数据库失败: %w", err)
	}
	return nil
}

// DeleteFinishedTasks 删除已经结束的任务，返回删掉了几个。ids 为空表示删除全部已结束的。
//
// 只删结束了的（完成、失败、取消）：还在跑、还在等的任务不会被删，哪怕它的编号在 ids 里。
// 跟着任务一起删掉的是它自己的记录——步骤日志、收到的图片、没送达的消息、事件订阅、通知指纹。
// 留下的是任务办完时已经落下的东西，它们从那一刻起就不依赖任务了：
// 提炼出的记忆和流程、学到的表达风格、任务模型的评分记录，以及审计日志。
func (s *Store) DeleteFinishedTasks(ctx context.Context, ids []string) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const finished = `state IN ('done', 'failed', 'cancelled')`
	sql, args := `DELETE FROM tasks WHERE `+finished+` RETURNING id`, []any{}
	if len(ids) > 0 {
		sql, args = `DELETE FROM tasks WHERE id = ANY($1) AND `+finished+` RETURNING id`, []any{ids}
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("pg: 删除任务失败: %w", err)
	}
	var gone []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("pg: 删除任务失败: %w", err)
		}
		gone = append(gone, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("pg: 删除任务失败: %w", err)
	}
	// 步骤、图片、收件箱和订阅有外键，随任务一起删了；通知指纹没有外键，在这里删。
	if len(gone) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM channel_notices WHERE task_id = ANY($1)`, gone); err != nil {
			return 0, fmt.Errorf("pg: 清理任务通知记录失败: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pg: 提交删除失败: %w", err)
	}
	return len(gone), nil
}

const taskColumns = `
	id, parent_id, title, goal, state, reason,
	domain, visibility, visibility_target, origin_trust,
	priority, tags,
	budget_max_tokens, budget_max_cost_cents, budget_max_wall_ns,
	usage_tokens, usage_cost_cents, usage_wall_ns,
	deadline, wake, next_seq, lease_owner, lease_expires_at,
	created_at, updated_at, usage_cost_micros, usage_cached_tokens`

func (s *Store) CreateTask(ctx context.Context, t *core.Task) error {
	return s.z.insertTask(ctx, s.pool, t)
}

// ListTasks 按创建时间倒序列出任务。activeOnly 为真时不含已结束的。
//
// 它不在 store.TaskStore 契约里：内核按 ID 和状态工作，从不需要"列出全部"，
// 这只是给运维 CLI 用的视图。
func (s *Store) ListTasks(ctx context.Context, activeOnly bool, limit int) ([]*core.Task, error) {
	sql := `SELECT ` + taskColumns + ` FROM tasks`
	if activeOnly {
		sql += ` WHERE state NOT IN ('done', 'failed', 'cancelled')`
	}
	sql += ` ORDER BY created_at DESC LIMIT $1`
	rows, err := s.pool.Query(ctx, sql, limit)
	if err != nil {
		return nil, fmt.Errorf("pg: 列出任务失败: %w", err)
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
	return out, rows.Err()
}

func (z sealer) insertTask(ctx context.Context, db execer, t *core.Task) error {
	if err := t.Validate(); err != nil {
		return err
	}
	wake, err := marshalWake(t.Wake)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO tasks (`+taskColumns+`, wake_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)`,
		t.ID, nullString(t.ParentID), z.str("tasks.title", t.Title), z.str("tasks.goal", t.Goal),
		string(t.State), z.str("tasks.reason", t.Reason),
		string(t.Domain), string(t.Visibility), z.str("tasks.visibility_target", t.VisibilityTarget), int16(t.Origin),
		t.Priority, z.eqs("tasks.tags", t.Tags),
		t.Budget.MaxTokens, t.Budget.MaxCostCents, int64(t.Budget.MaxWallClock),
		t.Usage.Tokens, t.Usage.CostCents, int64(t.Usage.WallClock),
		t.Deadline, z.json("tasks.wake", wake), t.NextSeq, t.LeaseOwner, t.LeaseExpiresAt,
		t.CreatedAt, t.UpdatedAt, t.Usage.CostMicros, t.Usage.CachedTokens, wakeAt(t.Wake))
	if err != nil {
		return fmt.Errorf("pg: 创建任务失败: %w", err)
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, id string) (*core.Task, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1`, id)
	t, err := s.z.scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取任务失败: %w", err)
	}
	return t, nil
}

func (s *Store) Transition(ctx context.Context, id string, from, to core.TaskState, reason string) error {
	if err := core.ValidateTransition(from, to); err != nil {
		return err
	}
	// 离开执行态时释放租约，否则重启后这个任务会被误判为仍在运行。
	releaseLease := to.Terminal() || to == core.StateReady

	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		   SET state = $3,
		       reason = $4,
		       updated_at = now(),
		       lease_owner = CASE WHEN $5 THEN '' ELSE lease_owner END,
		       lease_expires_at = CASE WHEN $5 THEN NULL ELSE lease_expires_at END
		 WHERE id = $1 AND state = $2`,
		id, string(from), string(to), s.z.str("tasks.reason", reason), releaseLease)
	if err != nil {
		return fmt.Errorf("pg: 状态迁移失败: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// 没有命中：要么任务不存在，要么已被其他执行者改掉了状态。
	var actual string
	err = s.pool.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1`, id).Scan(&actual)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("pg: 查询任务状态失败: %w", err)
	}
	return fmt.Errorf("%w: 期望 %s，实际 %s", store.ErrStateConflict, from, actual)
}

// SetTaskBudget 换掉一个已挂起的任务的预算。只对挂起的任务生效：正在跑的任务的预算由执行者持有，
// 在这里改了也会被它下一次写回时盖掉。
func (s *Store) SetTaskBudget(ctx context.Context, id string, b core.Budget) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		   SET budget_max_tokens = $2, budget_max_cost_cents = $3, budget_max_wall_ns = $4, updated_at = now()
		 WHERE id = $1 AND state = $5`,
		id, b.MaxTokens, b.MaxCostCents, int64(b.MaxWallClock), string(core.StateSuspended))
	if err != nil {
		return fmt.Errorf("pg: 更新任务预算失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 任务不存在或不在挂起状态", store.ErrStateConflict)
	}
	return nil
}

func (s *Store) UpdateTaskRuntime(ctx context.Context, t *core.Task) error {
	wake, err := marshalWake(t.Wake)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		   SET usage_tokens = $2, usage_cost_cents = $3, usage_wall_ns = $4,
		       next_seq = $5, wake = $6, deadline = $7, priority = $8,
		       usage_cost_micros = $9, wake_at = $10, usage_cached_tokens = $11,
		       updated_at = now()
		 WHERE id = $1`,
		t.ID, t.Usage.Tokens, t.Usage.CostCents, int64(t.Usage.WallClock),
		t.NextSeq, s.z.json("tasks.wake", wake), t.Deadline, t.Priority, t.Usage.CostMicros, wakeAt(t.Wake), t.Usage.CachedTokens)
	if err != nil {
		return fmt.Errorf("pg: 更新任务运行时失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

const stepColumns = `
	id, task_id, seq, type, status, tool_name, side_effect,
	input_hash, input, output, output_trust, idempotency_key,
	tokens_in, tokens_out, cost_cents, err, started_at, ended_at`

// execer 同时适配连接池与事务。
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (s *Store) AppendStep(ctx context.Context, st *core.Step) error {
	return s.z.insertStep(ctx, s.pool, st)
}

// WakeWithStep 在一个事务里完成唤醒与写步骤。
//
// UPDATE 拿到的行锁一直持有到提交，期间调度器领不走这个任务，
// 所以随后读到的 MAX(seq) 不会被并发的步骤写入抢先。
func (s *Store) WakeWithStep(ctx context.Context, taskID, reason string, st *core.Step) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE tasks
		   SET state = 'ready', reason = $2, updated_at = now(),
		       lease_owner = '', lease_expires_at = NULL
		 WHERE id = $1 AND state = 'waiting'`, taskID, s.z.str("tasks.reason", reason))
	if err != nil {
		return fmt.Errorf("pg: 唤醒任务失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var actual string
		err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1`, taskID).Scan(&actual)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("pg: 查询任务状态失败: %w", err)
		}
		return fmt.Errorf("%w: 期望 %s，实际 %s", store.ErrStateConflict, core.StateWaiting, actual)
	}

	var seq int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM task_steps WHERE task_id = $1`, taskID).Scan(&seq); err != nil {
		return fmt.Errorf("pg: 分配步骤序号失败: %w", err)
	}
	st.TaskID = taskID
	st.Seq = seq
	st.IdempotencyKey = core.IdempotencyKeyFor(taskID, seq)
	if err := s.z.insertStep(ctx, tx, st); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET next_seq = $2 WHERE id = $1`, taskID, seq+1); err != nil {
		return fmt.Errorf("pg: 更新步骤序号失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: 提交唤醒失败: %w", err)
	}
	return nil
}

func (z sealer) insertStep(ctx context.Context, db execer, st *core.Step) error {
	if err := st.Validate(); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `
		INSERT INTO task_steps (`+stepColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		st.ID, st.TaskID, st.Seq, string(st.Type), string(st.Status), st.ToolName,
		string(st.SideEffect), z.str("task_steps.input_hash", st.InputHash),
		z.json("task_steps.input", st.Input), z.json("task_steps.output", st.Output),
		int16(st.OutputTrust), st.IdempotencyKey,
		st.TokensIn, st.TokensOut, st.CostCents, z.str("task_steps.err", st.Err), st.StartedAt, st.EndedAt)
	if err != nil {
		if isUniqueViolation(err, "task_steps_seq_unique") {
			return fmt.Errorf("%w: 任务 %s seq=%d", store.ErrDuplicateSeq, st.TaskID, st.Seq)
		}
		return fmt.Errorf("pg: 写入步骤失败: %w", err)
	}
	return nil
}

func (s *Store) SaveStep(ctx context.Context, st *core.Step) error {
	if err := st.Validate(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE task_steps
		   SET status = $3, output = $4, output_trust = $5,
		       tokens_in = $6, tokens_out = $7, cost_cents = $8,
		       err = $9, ended_at = $10
		 WHERE task_id = $1 AND seq = $2`,
		st.TaskID, st.Seq, string(st.Status), s.z.json("task_steps.output", st.Output), int16(st.OutputTrust),
		st.TokensIn, st.TokensOut, st.CostCents, s.z.str("task_steps.err", st.Err), st.EndedAt)
	if err != nil {
		return fmt.Errorf("pg: 更新步骤失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) ListSteps(ctx context.Context, taskID string) ([]*core.Step, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+stepColumns+` FROM task_steps WHERE task_id = $1 ORDER BY seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取步骤日志失败: %w", err)
	}
	defer rows.Close()

	out := make([]*core.Step, 0)
	for rows.Next() {
		st, err := s.z.scanStep(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: 解析步骤失败: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ClaimReady 用 FOR UPDATE SKIP LOCKED 领取任务。
//
// 这是 Postgres 做队列的标准手法：多个 worker 并发领取时互不阻塞，
// 每个任务只会被一个 worker 拿到。
func (s *Store) ClaimReady(ctx context.Context, owner string, lease time.Duration, limit int) ([]*core.Task, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE tasks
		   SET state = 'running',
		       lease_owner = $1,
		       lease_expires_at = now() + $2::interval,
		       updated_at = now()
		 WHERE id IN (
		       SELECT id FROM tasks
		        WHERE state = 'ready'
		        ORDER BY priority DESC, created_at
		        LIMIT $3
		        FOR UPDATE SKIP LOCKED
		 )
		RETURNING `+taskColumns,
		owner, lease.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("pg: 领取任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]*core.Task, 0, limit)
	for rows.Next() {
		t, err := s.z.scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: 解析任务失败: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RenewLease 续期执行租约，仅当租约仍属于该 owner 时才成功。
func (s *Store) RenewLease(ctx context.Context, taskID, owner string, lease time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		   SET lease_expires_at = now() + $3::interval,
		       updated_at = now()
		 WHERE id = $1 AND lease_owner = $2 AND state = 'running'`,
		taskID, owner, lease.String())
	if err != nil {
		return fmt.Errorf("pg: 续期租约失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 租约不再属于 %s", store.ErrStateConflict, owner)
	}
	return nil
}

// ReclaimExpired 把租约过期的 running 任务放回 ready，这是崩溃恢复的入口。
func (s *Store) ReclaimExpired(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		   SET state = 'ready',
		       reason = $2,
		       lease_owner = '',
		       lease_expires_at = NULL,
		       updated_at = now()
		 WHERE state = 'running'
		   AND lease_expires_at IS NOT NULL
		   AND lease_expires_at <= $1`, now, s.z.str("tasks.reason", "执行租约过期，重新入队"))
	if err != nil {
		return 0, fmt.Errorf("pg: 回收过期租约失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) DueWaiting(ctx context.Context, now time.Time, limit int) ([]*core.Task, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+taskColumns+`
		  FROM tasks
		 WHERE state = 'waiting'
		   AND wake_at IS NOT NULL
		   AND wake_at <= $1
		 ORDER BY wake_at
		 LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("pg: 查询到期任务失败: %w", err)
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
	return out, rows.Err()
}

func (s *Store) Audit(ctx context.Context, e store.AuditEntry) error {
	if e.ID == "" {
		e.ID = core.NewID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log (id, at, actor, action, task_id, detail, trust_at_op)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.ID, e.At, e.Actor, e.Action, e.TaskID, s.z.str("audit_log.detail", e.Detail), int16(e.TrustAtOp))
	if err != nil {
		return fmt.Errorf("pg: 写入审计失败: %w", err)
	}
	// 审计同时记进操作日志，在后台的日志页里和别的操作放在一起看。审计本身已经写成，这一步失败不算审计失败。
	if err := s.AppendLogs(ctx, []oplog.Entry{auditLog(e)}); err != nil {
		slog.Default().Warn("审计没能记进操作日志", "动作", e.Action, "err", err)
	}
	return nil
}

// scanner 同时适配 pgx.Row 与 pgx.Rows。
type scanner interface {
	Scan(dest ...any) error
}

func (z sealer) scanTask(sc scanner) (*core.Task, error) {
	var (
		t          core.Task
		parentID   *string
		state      string
		domain     string
		visibility string
		origin     int16
		maxWall    int64
		usedWall   int64
		wake       []byte
	)
	err := sc.Scan(
		&t.ID, &parentID, &t.Title, &t.Goal, &state, &t.Reason,
		&domain, &visibility, &t.VisibilityTarget, &origin,
		&t.Priority, &t.Tags,
		&t.Budget.MaxTokens, &t.Budget.MaxCostCents, &maxWall,
		&t.Usage.Tokens, &t.Usage.CostCents, &usedWall,
		&t.Deadline, &wake, &t.NextSeq, &t.LeaseOwner, &t.LeaseExpiresAt,
		&t.CreatedAt, &t.UpdatedAt, &t.Usage.CostMicros, &t.Usage.CachedTokens)
	if err != nil {
		return nil, err
	}
	if parentID != nil {
		t.ParentID = *parentID
	}
	t.State = core.TaskState(state)
	t.Domain = core.Domain(domain)
	t.Visibility = core.Visibility(visibility)
	t.Origin = core.TrustLevel(origin)
	t.Budget.MaxWallClock = time.Duration(maxWall)
	t.Usage.WallClock = time.Duration(usedWall)
	if err := z.opens("tasks.title", &t.Title, "tasks.goal", &t.Goal, "tasks.reason", &t.Reason,
		"tasks.visibility_target", &t.VisibilityTarget); err != nil {
		return nil, err
	}
	if err := z.openAll("tasks.tags", t.Tags); err != nil {
		return nil, err
	}
	if wake, err = z.openJSON("tasks.wake", wake); err != nil {
		return nil, err
	}
	if len(wake) > 0 {
		var w core.WakeCondition
		if err := json.Unmarshal(wake, &w); err != nil {
			return nil, fmt.Errorf("解析唤醒条件失败: %w", err)
		}
		t.Wake = &w
	}
	return &t, nil
}

func (z sealer) scanStep(sc scanner) (*core.Step, error) {
	var (
		s          core.Step
		typ        string
		status     string
		sideEffect string
		trust      int16
		input      []byte
		output     []byte
	)
	err := sc.Scan(
		&s.ID, &s.TaskID, &s.Seq, &typ, &status, &s.ToolName, &sideEffect,
		&s.InputHash, &input, &output, &trust, &s.IdempotencyKey,
		&s.TokensIn, &s.TokensOut, &s.CostCents, &s.Err, &s.StartedAt, &s.EndedAt)
	if err != nil {
		return nil, err
	}
	s.Type = core.StepType(typ)
	s.Status = core.StepStatus(status)
	s.SideEffect = core.SideEffect(sideEffect)
	s.OutputTrust = core.TrustLevel(trust)
	if err := z.opens("task_steps.input_hash", &s.InputHash, "task_steps.err", &s.Err); err != nil {
		return nil, err
	}
	if input, err = z.openJSON("task_steps.input", input); err != nil {
		return nil, err
	}
	if output, err = z.openJSON("task_steps.output", output); err != nil {
		return nil, err
	}
	s.Input = json.RawMessage(input)
	s.Output = json.RawMessage(output)
	return &s, nil
}

// wakeAt 取出唤醒条件里的时间，单独存一列：调度按它找到点的任务，而 wake 本身是加密的。
func wakeAt(w *core.WakeCondition) *time.Time {
	if w == nil {
		return nil
	}
	return w.At
}

func marshalWake(w *core.WakeCondition) ([]byte, error) {
	if w == nil {
		return nil, nil
	}
	b, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("pg: 序列化唤醒条件失败: %w", err)
	}
	return b, nil
}

// nonNilTags 保证写入的是空数组而不是 NULL，tags 列是 NOT NULL。
func nonNilTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// isUniqueViolation 判断错误是否为指定约束的唯一性冲突。
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
