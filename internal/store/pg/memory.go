package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/memory"
)

// MemoryStore 是 memory.Store 的 Postgres 实现。
//
// 它与任务存储共用同一个连接池：自部署目标机器可能只有 2C4G，
// 不该为了分层整洁多开一套连接。
type MemoryStore struct {
	pool *pgxpool.Pool
}

// Memories 返回基于同一连接池的记忆存储。
func (s *Store) Memories() *MemoryStore { return &MemoryStore{pool: s.pool} }

var _ memory.Store = (*MemoryStore)(nil)

const memoryColumns = `
	id, kind, subject, domain, attribute, condition, identity_key,
	status, temporality, valid_from, valid_until,
	confidence, hit_count, last_hit_at, safety_critical, tags,
	current_revision_id, created_at, updated_at`

func (s *MemoryStore) Get(ctx context.Context, id string) (*memory.Memory, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+memoryColumns+` FROM memories WHERE id = $1`, id)
	return s.scanAndLoad(ctx, row)
}

func (s *MemoryStore) FindByKey(ctx context.Context, key string) (*memory.Memory, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE identity_key = $1`, key)
	return s.scanAndLoad(ctx, row)
}

func (s *MemoryStore) scanAndLoad(ctx context.Context, row pgx.Row) (*memory.Memory, error) {
	m, currentID, err := scanMemory(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, memory.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取记忆失败: %w", err)
	}
	if err := s.loadRevisions(ctx, m, currentID); err != nil {
		return nil, err
	}
	return m, nil
}

// Put 以事务写入记忆与其全部版本。
//
// 版本用 upsert：版本链是 append-only 的，但 superseded_by 会在旧版本上回填。
func (s *MemoryStore) Put(ctx context.Context, m *memory.Memory) error {
	if err := m.Validate(); err != nil {
		return err
	}
	cond, err := json.Marshal(m.Condition)
	if err != nil {
		return fmt.Errorf("pg: 序列化偏好条件失败: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO memories (`+memoryColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (id) DO UPDATE SET
			domain = EXCLUDED.domain,
			attribute = EXCLUDED.attribute,
			condition = EXCLUDED.condition,
			identity_key = EXCLUDED.identity_key,
			status = EXCLUDED.status,
			temporality = EXCLUDED.temporality,
			valid_from = EXCLUDED.valid_from,
			valid_until = EXCLUDED.valid_until,
			confidence = EXCLUDED.confidence,
			hit_count = EXCLUDED.hit_count,
			last_hit_at = EXCLUDED.last_hit_at,
			safety_critical = EXCLUDED.safety_critical,
			tags = EXCLUDED.tags,
			current_revision_id = EXCLUDED.current_revision_id,
			updated_at = EXCLUDED.updated_at`,
		m.ID, string(m.Kind), m.Subject, m.Domain, m.Attribute, cond, m.Key(),
		string(m.Status), string(m.Temporality), m.ValidFrom, m.ValidUntil,
		m.Confidence, m.HitCount, m.LastHitAt, m.SafetyCritical, nonNilTags(m.Tags),
		m.Current.ID, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return fmt.Errorf("pg: 写入记忆失败: %w", err)
	}

	for _, r := range append(append([]memory.Revision{}, m.History...), m.Current) {
		if err := putRevision(ctx, tx, m.ID, r); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: 提交记忆写入失败: %w", err)
	}
	return nil
}

func putRevision(ctx context.Context, tx pgx.Tx, memoryID string, r memory.Revision) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO memory_revisions
			(id, memory_id, value, source_kind, source_ref, source_trust,
			 evidence, confidence, created_at, superseded_by, override_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (id) DO UPDATE SET
			superseded_by = EXCLUDED.superseded_by,
			override_reason = EXCLUDED.override_reason,
			evidence = EXCLUDED.evidence,
			confidence = EXCLUDED.confidence`,
		r.ID, memoryID, r.Value, string(r.Source.Kind), r.Source.Ref, int16(r.Source.Trust),
		nonNilTags(r.Evidence), r.Confidence, r.CreatedAt,
		nullString(r.SupersededBy), r.OverrideReason)
	if err != nil {
		return fmt.Errorf("pg: 写入记忆版本失败: %w", err)
	}
	return nil
}

func (s *MemoryStore) List(ctx context.Context, q memory.Query) ([]*memory.Memory, error) {
	sql := `SELECT ` + memoryColumns + ` FROM memories WHERE TRUE`
	args := make([]any, 0, 6)

	add := func(clause string, v any) {
		args = append(args, v)
		sql += fmt.Sprintf(clause, len(args))
	}
	if q.Subject != "" {
		add(" AND subject = $%d", q.Subject)
	}
	if q.Domain != "" {
		add(" AND domain = $%d", q.Domain)
	}
	if q.SafetyOnly {
		sql += " AND safety_critical"
	}
	if !q.IncludeInactive {
		sql += " AND status NOT IN ('archived','expired')"
	}
	if len(q.Kinds) > 0 {
		kinds := make([]string, 0, len(q.Kinds))
		for _, k := range q.Kinds {
			kinds = append(kinds, string(k))
		}
		add(" AND kind = ANY($%d)", kinds)
	}
	if len(q.Tags) > 0 {
		add(" AND tags @> $%d", q.Tags)
	}
	sql += " ORDER BY updated_at DESC, id"
	if q.Limit > 0 {
		add(" LIMIT $%d", q.Limit)
	}

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("pg: 检索记忆失败: %w", err)
	}
	defer rows.Close()

	out := make([]*memory.Memory, 0)
	currents := make([]string, 0)
	for rows.Next() {
		m, currentID, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: 解析记忆失败: %w", err)
		}
		out = append(out, m)
		currents = append(currents, currentID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, m := range out {
		if err := s.loadRevisions(ctx, m, currents[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// loadRevisions 读取一条记忆的全部版本，拆成当前版本与历史。
func (s *MemoryStore) loadRevisions(ctx context.Context, m *memory.Memory, currentID string) error {
	rows, err := s.pool.Query(ctx, `
		SELECT id, value, source_kind, source_ref, source_trust,
		       evidence, confidence, created_at, superseded_by, override_reason
		  FROM memory_revisions WHERE memory_id = $1 ORDER BY created_at, id`, m.ID)
	if err != nil {
		return fmt.Errorf("pg: 读取记忆版本失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			r          memory.Revision
			kind       string
			trust      int16
			superseded *string
		)
		if err := rows.Scan(&r.ID, &r.Value, &kind, &r.Source.Ref, &trust,
			&r.Evidence, &r.Confidence, &r.CreatedAt, &superseded, &r.OverrideReason); err != nil {
			return fmt.Errorf("pg: 解析记忆版本失败: %w", err)
		}
		r.MemoryID = m.ID
		r.Source.Kind = memory.SourceKind(kind)
		r.Source.Trust = core.TrustLevel(trust)
		if superseded != nil {
			r.SupersededBy = *superseded
		}
		if r.ID == currentID {
			m.Current = r
			continue
		}
		m.History = append(m.History, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if m.Current.ID == "" {
		return fmt.Errorf("pg: 记忆 %s 的当前版本 %s 缺失", m.ID, currentID)
	}
	return nil
}

func (s *MemoryStore) PutPending(ctx context.Context, p memory.Pending) error {
	cand, err := json.Marshal(p.Candidate)
	if err != nil {
		return fmt.Errorf("pg: 序列化候选失败: %w", err)
	}
	arb, err := json.Marshal(p.Arb)
	if err != nil {
		return fmt.Errorf("pg: 序列化仲裁结论失败: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO memory_pending (id, memory_id, subject, candidate, arbitration, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		p.ID, nullString(p.MemoryID), p.Candidate.Subject, cand, arb, p.CreatedAt)
	if err != nil {
		return fmt.Errorf("pg: 写入待裁决候选失败: %w", err)
	}
	return nil
}

func (s *MemoryStore) ListPending(ctx context.Context, subject string) ([]memory.Pending, error) {
	sql := `SELECT id, memory_id, candidate, arbitration, created_at
	          FROM memory_pending WHERE resolved_at IS NULL`
	args := make([]any, 0, 1)
	if subject != "" {
		args = append(args, subject)
		sql += " AND subject = $1"
	}
	sql += " ORDER BY created_at"

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取待裁决候选失败: %w", err)
	}
	defer rows.Close()

	out := make([]memory.Pending, 0)
	for rows.Next() {
		var (
			p         memory.Pending
			memoryID  *string
			cand, arb []byte
		)
		if err := rows.Scan(&p.ID, &memoryID, &cand, &arb, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("pg: 解析待裁决候选失败: %w", err)
		}
		if memoryID != nil {
			p.MemoryID = *memoryID
		}
		if err := json.Unmarshal(cand, &p.Candidate); err != nil {
			return nil, fmt.Errorf("pg: 解析候选内容失败: %w", err)
		}
		if err := json.Unmarshal(arb, &p.Arb); err != nil {
			return nil, fmt.Errorf("pg: 解析仲裁结论失败: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResolvePending 标记一条候选已被用户裁决。
func (s *MemoryStore) ResolvePending(ctx context.Context, id string, at time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE memory_pending SET resolved_at = $2 WHERE id = $1 AND resolved_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("pg: 标记候选已裁决失败: %w", err)
	}
	return nil
}

func scanMemory(sc scanner) (*memory.Memory, string, error) {
	var (
		m           memory.Memory
		kind        string
		status      string
		temporality string
		cond        []byte
		identityKey string
		currentID   string
	)
	err := sc.Scan(
		&m.ID, &kind, &m.Subject, &m.Domain, &m.Attribute, &cond, &identityKey,
		&status, &temporality, &m.ValidFrom, &m.ValidUntil,
		&m.Confidence, &m.HitCount, &m.LastHitAt, &m.SafetyCritical, &m.Tags,
		&currentID, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, "", err
	}
	m.Kind = memory.Kind(kind)
	m.Status = memory.Status(status)
	m.Temporality = memory.Temporality(temporality)
	if len(cond) > 0 {
		if err := json.Unmarshal(cond, &m.Condition); err != nil {
			return nil, "", fmt.Errorf("解析偏好条件失败: %w", err)
		}
	}
	return &m, currentID, nil
}
