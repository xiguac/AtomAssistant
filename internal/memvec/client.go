package memvec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxEmbedBatch 是一次交给向量服务编码的条数上限，与 deploy/embed/embedd.py 的 MAX_TEXTS 一致。
const maxEmbedBatch = 64

// Embedder 是本机向量服务（deploy/embed/embedd.py）的客户端。
type Embedder struct {
	URL    string
	Client *http.Client
}

func (e *Embedder) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Model 返回向量服务用的模型名，用来判断已存的向量还能不能用。
func (e *Embedder) Model(ctx context.Context) (string, error) {
	var out struct {
		OK    bool   `json:"ok"`
		Model string `json:"model"`
	}
	if err := call(ctx, e.client(), http.MethodGet, strings.TrimRight(e.URL, "/")+"/healthz", "", nil, &out); err != nil {
		return "", fmt.Errorf("memvec: 向量服务不可用: %w", err)
	}
	if !out.OK || out.Model == "" {
		return "", fmt.Errorf("memvec: 向量服务没有就绪")
	}
	return out.Model, nil
}

// Embed 把几段文字编码成向量（已归一化）。
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	var all [][]float32
	for len(texts) > 0 {
		n := min(len(texts), maxEmbedBatch)
		var out struct {
			Dim     int         `json:"dim"`
			Vectors [][]float32 `json:"vectors"`
		}
		if err := call(ctx, e.client(), http.MethodPost, strings.TrimRight(e.URL, "/")+"/embed", "",
			map[string]any{"texts": texts[:n]}, &out); err != nil {
			return nil, fmt.Errorf("memvec: 编码失败: %w", err)
		}
		if len(out.Vectors) != n {
			return nil, fmt.Errorf("memvec: 向量服务返回了 %d 个向量，应为 %d 个", len(out.Vectors), n)
		}
		all = append(all, out.Vectors...)
		texts = texts[n:]
	}
	return all, nil
}

// Qdrant 是 Qdrant 向量库的 REST 客户端，只用到这里需要的几个接口。
type Qdrant struct {
	URL    string
	APIKey string
	Client *http.Client
}

func (q *Qdrant) client() *http.Client {
	if q.Client != nil {
		return q.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (q *Qdrant) do(ctx context.Context, method, path string, in, out any) error {
	return call(ctx, q.client(), method, strings.TrimRight(q.URL, "/")+path, q.APIKey, in, out)
}

// Point 是向量库里的一条：编号、向量、附带的少量字段。
type Point struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Hit 是一条检索结果。
type Hit struct {
	ID      string         `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

// EnsureCollection 没有这个集合就按 dim 维、余弦距离建一个。
func (q *Qdrant) EnsureCollection(ctx context.Context, name string, dim int) error {
	err := q.do(ctx, http.MethodGet, "/collections/"+name, nil, nil)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("memvec: 查询向量集合失败: %w", err)
	}
	if err := q.do(ctx, http.MethodPut, "/collections/"+name,
		map[string]any{"vectors": map[string]any{"size": dim, "distance": "Cosine"}}, nil); err != nil {
		return fmt.Errorf("memvec: 建立向量集合失败: %w", err)
	}
	return nil
}

// Upsert 写入或替换几条。
func (q *Qdrant) Upsert(ctx context.Context, name string, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	if err := q.do(ctx, http.MethodPut, "/collections/"+name+"/points?wait=true", map[string]any{"points": points}, nil); err != nil {
		return fmt.Errorf("memvec: 写入向量失败: %w", err)
	}
	return nil
}

// Delete 删掉几条。
func (q *Qdrant) Delete(ctx context.Context, name string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := q.do(ctx, http.MethodPost, "/collections/"+name+"/points/delete?wait=true", map[string]any{"points": ids}, nil); err != nil {
		return fmt.Errorf("memvec: 删除向量失败: %w", err)
	}
	return nil
}

// Search 找与 vector 最接近的 limit 条，只要分数不低于 threshold 的。
func (q *Qdrant) Search(ctx context.Context, name string, vector []float32, limit int, threshold float64) ([]Hit, error) {
	var out struct {
		Result []Hit `json:"result"`
	}
	if err := q.do(ctx, http.MethodPost, "/collections/"+name+"/points/search", map[string]any{
		"vector": vector, "limit": limit, "with_payload": true, "score_threshold": threshold,
	}, &out); err != nil {
		return nil, fmt.Errorf("memvec: 向量检索失败: %w", err)
	}
	return out.Result, nil
}

// All 翻出集合里的全部条目（不带向量）。
func (q *Qdrant) All(ctx context.Context, name string) ([]Point, error) {
	var all []Point
	var offset any
	for {
		var out struct {
			Result struct {
				Points []Point `json:"points"`
				Next   any     `json:"next_page_offset"`
			} `json:"result"`
		}
		body := map[string]any{"limit": 256, "with_payload": true, "with_vector": false}
		if offset != nil {
			body["offset"] = offset
		}
		if err := q.do(ctx, http.MethodPost, "/collections/"+name+"/points/scroll", body, &out); err != nil {
			return nil, fmt.Errorf("memvec: 读取向量集合失败: %w", err)
		}
		all = append(all, out.Result.Points...)
		if out.Result.Next == nil {
			return all, nil
		}
		offset = out.Result.Next
	}
}

// httpError 是对方返回的非 2xx 应答。
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

func isNotFound(err error) bool {
	he, ok := err.(*httpError)
	return ok && he.Status == http.StatusNotFound
}

func call(ctx context.Context, c *http.Client, method, url, apiKey string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("api-key", apiKey)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &httpError{Status: resp.StatusCode, Body: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("应答无法解析: %w", err)
	}
	return nil
}
