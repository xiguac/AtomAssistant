package pg

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/llm"
)

// PutTaskImage 存下用户发给一个任务的一张图片，返回它的编号。
func (s *Store) PutTaskImage(ctx context.Context, taskID, mime string, data []byte) (string, error) {
	id := core.NewID()
	_, err := s.pool.Exec(ctx, `INSERT INTO task_images (id, task_id, mime, data) VALUES ($1,$2,$3,$4)`,
		id, taskID, mime, s.z.str("task_images.data", base64.StdEncoding.EncodeToString(data)))
	if err != nil {
		return "", fmt.Errorf("pg: 保存图片失败: %w", err)
	}
	return id, nil
}

// TaskImages 返回一个任务最近收到的 limit 张图片，按收到的先后排。
func (s *Store) TaskImages(ctx context.Context, taskID string, limit int) ([]llm.Image, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT mime, data FROM (
			SELECT mime, data, created_at, id FROM task_images
			 WHERE task_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2) recent
		 ORDER BY created_at, id`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取图片失败: %w", err)
	}
	defer rows.Close()
	var out []llm.Image
	for rows.Next() {
		var mime, data string
		if err := rows.Scan(&mime, &data); err != nil {
			return nil, fmt.Errorf("pg: 解析图片失败: %w", err)
		}
		if err := s.z.open("task_images.data", &data); err != nil {
			return nil, fmt.Errorf("pg: 解析图片失败: %w", err)
		}
		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("pg: 图片内容损坏: %w", err)
		}
		out = append(out, llm.Image{MIME: mime, Data: raw})
	}
	return out, rows.Err()
}
