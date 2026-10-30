package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/xiguac/atomassistant/internal/seal"
)

// RekeyFrom 把库里用 old 加密的内容全部解开，再用当前这把密钥重新加密。
//
// 只给改名迁移用（见 seal.MigrateLegacyKey），服务器迁移完成后删掉。要在 Migrate 之前调用：
// Migrate 会核对密钥指纹，旧密文还在时核对不过。整个过程在一个事务里，失败就什么都不改。
// 库里记的已经是当前密钥的指纹（上次已经换过），或者库里还没有加密过的内容，都什么也不做。
func (s *Store) RekeyFrom(ctx context.Context, old *seal.Box) (changed bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(sealStateLock)); err != nil {
		return false, fmt.Errorf("pg: 获取加密状态锁失败: %w", err)
	}
	var keyID string
	err = tx.QueryRow(ctx, `SELECT key_id FROM seal_state WHERE id = 1`).Scan(&keyID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pg: 读取加密状态失败: %w", err)
	case keyID == s.z.box.KeyID():
		return false, nil
	case keyID != old.KeyID():
		return false, fmt.Errorf("pg: 数据库里的内容不是用旧密钥加密的（库里记的是 %s，旧密钥是 %s）", keyID, old.KeyID())
	}

	from := sealer{box: old}
	tables := make([]string, 0, len(sealedColumns))
	for t := range sealedColumns {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, table := range tables {
		// 旧版本的库里还没有后来才加的表：它们没有数据，Migrate 会建出来，跳过。
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			return false, fmt.Errorf("pg: 检查表 %s 失败: %w", table, err)
		}
		if !exists {
			continue
		}
		reseal := func(row map[string]json.RawMessage) error {
			if err := from.openRow(table, row); err != nil {
				return err
			}
			return s.z.sealRow(table, row)
		}
		if err := s.rewriteTable(ctx, tx, table, reseal); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE seal_state SET key_id = $1 WHERE id = 1`, s.z.box.KeyID()); err != nil {
		return false, fmt.Errorf("pg: 记录加密状态失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("pg: 提交重新加密失败: %w", err)
	}
	return true, nil
}
