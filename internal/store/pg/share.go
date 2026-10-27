package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/xiguac/atomhelper/internal/share"
)

var _ share.Store = (*Store)(nil)

// CreateShare 存下一个下载链接。
func (s *Store) CreateShare(ctx context.Context, l *share.Link, tokenHash string) error {
	list, err := json.Marshal(l.Files)
	if err != nil {
		return fmt.Errorf("pg: 保存分享链接失败: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO share_links (id, token_hash, task_id, path, is_dir, files, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		l.ID, tokenHash, l.TaskID, s.z.str("share_links.path", l.Path), l.Dir,
		s.z.str("share_links.files", string(list)), l.CreatedAt, l.ExpiresAt)
	if err != nil {
		return fmt.Errorf("pg: 保存分享链接失败: %w", err)
	}
	return nil
}

const shareColumns = `id, task_id, path, is_dir, files, created_at, expires_at`

// ShareByToken 按令牌摘要找一个此刻有效的链接。
func (s *Store) ShareByToken(ctx context.Context, tokenHash string, now time.Time) (*share.Link, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+shareColumns+` FROM share_links
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2`, tokenHash, now)
	l, err := s.scanShare(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, share.ErrNotFound
	}
	return l, err
}

// ActiveShares 列出此刻有效的链接，新的在前。
func (s *Store) ActiveShares(ctx context.Context, now time.Time) ([]*share.Link, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+shareColumns+` FROM share_links
		WHERE revoked_at IS NULL AND expires_at > $1 ORDER BY created_at DESC, id`, now)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取分享链接失败: %w", err)
	}
	defer rows.Close()
	var out []*share.Link
	for rows.Next() {
		l, err := s.scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 读取分享链接失败: %w", err)
	}
	return out, nil
}

// RevokeShare 撤销一个链接，返回它此前是否有效。
func (s *Store) RevokeShare(ctx context.Context, id string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE share_links SET revoked_at = $2
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > $2`, id, now)
	if err != nil {
		return false, fmt.Errorf("pg: 撤销分享链接失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteExpiredShares 删掉 before 之前就已经失效（过期或撤销）的链接。
func (s *Store) DeleteExpiredShares(ctx context.Context, before time.Time) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM share_links WHERE expires_at <= $1 OR revoked_at <= $1`, before); err != nil {
		return fmt.Errorf("pg: 清理失效的分享链接失败: %w", err)
	}
	return nil
}

func (s *Store) scanShare(row pgx.Row) (*share.Link, error) {
	var l share.Link
	var list string
	if err := row.Scan(&l.ID, &l.TaskID, &l.Path, &l.Dir, &list, &l.CreatedAt, &l.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("pg: 读取分享链接失败: %w", err)
	}
	if err := s.z.opens("share_links.path", &l.Path, "share_links.files", &list); err != nil {
		return nil, fmt.Errorf("pg: 解开分享链接失败: %w", err)
	}
	if err := json.Unmarshal([]byte(list), &l.Files); err != nil {
		return nil, fmt.Errorf("pg: 分享链接的文件清单已损坏: %w", err)
	}
	if len(l.Files) == 0 {
		return nil, fmt.Errorf("pg: 分享链接 %s 没有文件", l.ID)
	}
	return &l, nil
}
