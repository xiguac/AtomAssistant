package pg

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomassistant/internal/vault"
)

// VaultStore 是 vault.Store 的 Postgres 实现。它只经手密文。
type VaultStore struct {
	pool *pgxpool.Pool
	z    sealer
}

// Vault 返回基于同一连接池的保险库存储。
func (s *Store) Vault() *VaultStore { return &VaultStore{pool: s.pool, z: s.z} }

var _ vault.Store = (*VaultStore)(nil)

// PutItem 写入一条，同名则整条替换。
func (s *VaultStore) PutItem(ctx context.Context, it *vault.Item) error {
	tools := it.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO vault_items (id, label, note, allowed_tools, sealed, nonce, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (label) DO UPDATE
		   SET note = EXCLUDED.note, allowed_tools = EXCLUDED.allowed_tools,
		       sealed = EXCLUDED.sealed, nonce = EXCLUDED.nonce, created_at = EXCLUDED.created_at`,
		it.ID, s.z.eq("vault_items.label", it.Label), s.z.str("vault_items.note", it.Note), tools, it.Sealed, it.Nonce, it.CreatedAt)
	if err != nil {
		return fmt.Errorf("pg: 写入保险库失败: %w", err)
	}
	return nil
}

func (s *VaultStore) GetItem(ctx context.Context, label string) (*vault.Item, error) {
	var it vault.Item
	err := s.pool.QueryRow(ctx, `
		SELECT id, label, note, allowed_tools, sealed, nonce, created_at
		  FROM vault_items WHERE label = $1`, s.z.eq("vault_items.label", label)).
		Scan(&it.ID, &it.Label, &it.Note, &it.AllowedTools, &it.Sealed, &it.Nonce, &it.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, vault.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取保险库失败: %w", err)
	}
	if err := s.z.opens("vault_items.label", &it.Label, "vault_items.note", &it.Note); err != nil {
		return nil, fmt.Errorf("pg: 读取保险库失败: %w", err)
	}
	return &it, nil
}

func (s *VaultStore) ListItems(ctx context.Context) ([]*vault.Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, label, note, allowed_tools, sealed, nonce, created_at
		  FROM vault_items`)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取保险库失败: %w", err)
	}
	defer rows.Close()

	out := make([]*vault.Item, 0)
	for rows.Next() {
		var it vault.Item
		if err := rows.Scan(&it.ID, &it.Label, &it.Note, &it.AllowedTools,
			&it.Sealed, &it.Nonce, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("pg: 解析保险库条目失败: %w", err)
		}
		if err := s.z.opens("vault_items.label", &it.Label, "vault_items.note", &it.Note); err != nil {
			return nil, fmt.Errorf("pg: 解析保险库条目失败: %w", err)
		}
		out = append(out, &it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 名字是加密的，数据库排不了序，读出来再排。
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

func (s *VaultStore) DeleteItem(ctx context.Context, label string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM vault_items WHERE label = $1`, s.z.eq("vault_items.label", label))
	if err != nil {
		return fmt.Errorf("pg: 删除保险库条目失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return vault.ErrNotFound
	}
	return nil
}
