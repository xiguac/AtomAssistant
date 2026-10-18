package pg

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/llm"
)

// PutTaskImage 存下用户发给一个任务的一张图片（或一段语音、视频，按 mime 区分），返回它的编号。
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
	return s.TaskMedia(ctx, taskID, llm.InputImage, limit)
}

// mimePrefix 是各类媒体的 MIME 类型前缀。语音、视频和图片存在同一张表里，按类型分开取。
var mimePrefix = map[llm.Input]string{llm.InputImage: "image/", llm.InputAudio: "audio/", llm.InputVideo: "video/"}

// TaskMedia 返回一个任务最近收到的 limit 条某类媒体（图片、语音或视频），按收到的先后排。
func (s *Store) TaskMedia(ctx context.Context, taskID string, kind llm.Input, limit int) ([]llm.Image, error) {
	prefix, ok := mimePrefix[kind]
	if !ok {
		return nil, fmt.Errorf("pg: 没有 %q 这一类媒体", kind)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT mime, data FROM (
			SELECT mime, data, created_at, id FROM task_images
			 WHERE task_id = $1 AND starts_with(mime, $3) ORDER BY created_at DESC, id DESC LIMIT $2) recent
		 ORDER BY created_at, id`, taskID, limit, prefix)
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
