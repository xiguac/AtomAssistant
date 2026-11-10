package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomassistant/internal/matter"
)

// MatterStore 是 matter.Store 的 Postgres 实现。
type MatterStore struct {
	pool *pgxpool.Pool
	z    sealer
}

// Matters 返回基于同一连接池的事项存储。
func (s *Store) Matters() *MatterStore { return &MatterStore{pool: s.pool, z: s.z} }

var _ matter.Store = (*MatterStore)(nil)

const matterColumns = `id, parent_id, kind, title, notes, channel, chat, last_active_at, asked_at, created_at`

func (s *MatterStore) Put(ctx context.Context, m *matter.Matter) error {
	notes, err := json.Marshal(m.Notes)
	if err != nil {
		return fmt.Errorf("pg: 序列化事项的进展失败: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO matters (`+matterColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (id) DO UPDATE SET parent_id = EXCLUDED.parent_id, kind = EXCLUDED.kind, title = EXCLUDED.title,
		    notes = EXCLUDED.notes, channel = EXCLUDED.channel, chat = EXCLUDED.chat,
		    last_active_at = EXCLUDED.last_active_at, asked_at = EXCLUDED.asked_at`,
		m.ID, m.ParentID, string(m.Kind), s.z.str("matters.title", m.Title), s.z.str("matters.notes", string(notes)),
		m.Channel, s.z.str("matters.chat", m.Chat), m.LastActiveAt, m.AskedAt, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("pg: 保存事项失败: %w", err)
	}
	return nil
}

func (s *MatterStore) scan(row pgx.Row) (*matter.Matter, error) {
	var m matter.Matter
	var kind, notes string
	if err := row.Scan(&m.ID, &m.ParentID, &kind, &m.Title, &notes, &m.Channel, &m.Chat, &m.LastActiveAt, &m.AskedAt, &m.CreatedAt); err != nil {
		return nil, err
	}
	if err := s.z.opens("matters.title", &m.Title, "matters.notes", &notes, "matters.chat", &m.Chat); err != nil {
		return nil, err
	}
	m.Kind = matter.Kind(kind)
	if notes != "" {
		if err := json.Unmarshal([]byte(notes), &m.Notes); err != nil {
			return nil, fmt.Errorf("事项 %s 的进展无法解析: %w", m.ID, err)
		}
	}
	return &m, nil
}

func (s *MatterStore) Get(ctx context.Context, id string) (*matter.Matter, error) {
	m, err := s.scan(s.pool.QueryRow(ctx, `SELECT `+matterColumns+` FROM matters WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, matter.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取事项失败: %w", err)
	}
	return m, nil
}

func (s *MatterStore) List(ctx context.Context) ([]*matter.Matter, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+matterColumns+` FROM matters ORDER BY last_active_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取事项清单失败: %w", err)
	}
	defer rows.Close()
	out := make([]*matter.Matter, 0)
	for rows.Next() {
		m, err := s.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: 读取事项清单失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 读取事项清单失败: %w", err)
	}
	return out, nil
}

func (s *MatterStore) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM matters WHERE id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("pg: 删除事项失败: %w", err)
	}
	return nil
}
