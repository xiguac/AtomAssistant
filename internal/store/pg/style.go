package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomassistant/internal/style"
)

// StyleStore 是 style.Store 的 Postgres 实现。
type StyleStore struct {
	pool *pgxpool.Pool
	z    sealer
}

// Style 返回基于同一连接池的风格存储。
func (s *Store) Style() *StyleStore { return &StyleStore{pool: s.pool, z: s.z} }

var _ style.Store = (*StyleStore)(nil)

func (s *StyleStore) AddSample(ctx context.Context, in *style.Sample) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO style_samples (id, scene, text, digest, ref, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (digest) DO NOTHING`,
		in.ID, in.Scene, s.z.str("style_samples.text", in.Text), s.z.eq("style_samples.digest", in.Digest()),
		s.z.str("style_samples.ref", in.Ref), in.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("pg: 写入风格样本失败: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *StyleStore) RecentSamples(ctx context.Context, scene string, limit int) ([]style.Sample, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, scene, text, ref, created_at FROM style_samples
		 WHERE scene = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, scene, limit)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取风格样本失败: %w", err)
	}
	defer rows.Close()
	var out []style.Sample
	for rows.Next() {
		var x style.Sample
		if err := rows.Scan(&x.ID, &x.Scene, &x.Text, &x.Ref, &x.CreatedAt); err != nil {
			return nil, fmt.Errorf("pg: 读取风格样本失败: %w", err)
		}
		if err := s.z.opens("style_samples.text", &x.Text, "style_samples.ref", &x.Ref); err != nil {
			return nil, fmt.Errorf("pg: 读取风格样本失败: %w", err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 读取风格样本失败: %w", err)
	}
	return out, nil
}

func (s *StyleStore) CountSamples(ctx context.Context, scene string, after time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM style_samples WHERE scene = $1 AND created_at > $2`, scene, after).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("pg: 统计风格样本失败: %w", err)
	}
	return n, nil
}

func (s *StyleStore) TrimSamples(ctx context.Context, scene string, keep int) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM style_samples
		 WHERE scene = $1 AND id NOT IN (
		       SELECT id FROM style_samples WHERE scene = $1
		        ORDER BY created_at DESC, id DESC LIMIT $2)`, scene, keep)
	if err != nil {
		return fmt.Errorf("pg: 清理旧的风格样本失败: %w", err)
	}
	return nil
}

func (s *StyleStore) GetProfile(ctx context.Context, scene string) (*style.Profile, error) {
	var p style.Profile
	err := s.pool.QueryRow(ctx,
		`SELECT scene, description, up_to, updated_at FROM style_profiles WHERE scene = $1`, scene).
		Scan(&p.Scene, &p.Description, &p.UpTo, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取风格概述失败: %w", err)
	}
	if err := s.z.open("style_profiles.description", &p.Description); err != nil {
		return nil, fmt.Errorf("pg: 读取风格概述失败: %w", err)
	}
	return &p, nil
}

func (s *StyleStore) PutProfile(ctx context.Context, p *style.Profile) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO style_profiles (scene, description, up_to, updated_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (scene) DO UPDATE
		   SET description = EXCLUDED.description, up_to = EXCLUDED.up_to, updated_at = EXCLUDED.updated_at`,
		p.Scene, s.z.str("style_profiles.description", p.Description), p.UpTo, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("pg: 写入风格概述失败: %w", err)
	}
	return nil
}

// Clear 删掉全部样本和概述。两张表一起清，不会留下"概述还在、样本没了"的中间状态。
func (s *StyleStore) Clear(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM style_samples`); err != nil {
		return fmt.Errorf("pg: 清空风格样本失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM style_profiles`); err != nil {
		return fmt.Errorf("pg: 清空风格概述失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: 提交失败: %w", err)
	}
	return nil
}

// AssistantSentMail 返回助手发出过的邮件的编号（Message-ID，不带尖括号）。
//
// 学习用户写邮件的口气时要靠它把助手代写的信剔出去：那些信在"已发送"里和用户亲手写的
// 混在一起，不剔掉的话助手就是在跟自己学，越学越像它自己。
func (s *Store) AssistantSentMail(ctx context.Context) (map[string]bool, error) {
	// 步骤的输出是加密的，数据库没法从里面取字段：把发信步骤的输出读出来，解密后自己取。
	rows, err := s.pool.Query(ctx, `
		SELECT output FROM task_steps
		 WHERE tool_name = 'mail.send' AND status = 'done' AND output IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取助手发出的邮件失败: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("pg: 读取助手发出的邮件失败: %w", err)
		}
		if raw, err = s.z.openJSON("task_steps.output", raw); err != nil {
			return nil, fmt.Errorf("pg: 读取助手发出的邮件失败: %w", err)
		}
		var sent struct {
			MessageID string `json:"message_id"`
		}
		// 输出不是对象（比如只是一句话）就没有编号可取，跳过。
		if json.Unmarshal(raw, &sent) == nil && sent.MessageID != "" {
			out[sent.MessageID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 读取助手发出的邮件失败: %w", err)
	}
	return out, nil
}
