package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Setting 读出后台自己记的一项设置；没有这一项时返回空串。
//
// 这里存的是登录方式、动态验证码的种子、登记过的通行密钥这类东西，值是加密的。
// 它不在任何导出板块里：换了服务器，登录方式应当由用户重新设置。
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pg: 读取设置 %s 失败: %w", key, err)
	}
	if err := s.z.open("settings.value", &v); err != nil {
		return "", fmt.Errorf("pg: 读取设置 %s 失败: %w", key, err)
	}
	return v, nil
}

// PutSetting 写入一项设置。值为空表示删掉这一项。
func (s *Store) PutSetting(ctx context.Context, key, value string) error {
	var err error
	if value == "" {
		_, err = s.pool.Exec(ctx, `DELETE FROM settings WHERE key = $1`, key)
	} else {
		_, err = s.pool.Exec(ctx, `
			INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, now())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
			key, s.z.str("settings.value", value))
	}
	if err != nil {
		return fmt.Errorf("pg: 保存设置 %s 失败: %w", key, err)
	}
	return nil
}

// SettingsWithPrefix 读出名字以 prefix 开头的全部设置。
func (s *Store) SettingsWithPrefix(ctx context.Context, prefix string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM settings WHERE starts_with(key, $1)`, prefix)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取设置 %s* 失败: %w", prefix, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("pg: 读取设置 %s* 失败: %w", prefix, err)
		}
		if err := s.z.open("settings.value", &v); err != nil {
			return nil, fmt.Errorf("pg: 读取设置 %s 失败: %w", k, err)
		}
		out[k] = v
	}
	return out, rows.Err()
}
