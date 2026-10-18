// Package deepseek 调用 DeepSeek 开放平台的联网搜索与网页抓取接口。
//
// 见 https://api-docs.deepseek.com/zh-cn/guides/search 。两个接口都是独立的 REST 接口，
// 不是 OpenAI 兼容的补全接口：
//   - POST /v1/tools/search：按关键词搜索，返回网页的标题、网址、摘要。
//   - POST /v1/tools/fetch：给一个网址，返回网页正文（Markdown）。
//
// 鉴权和模型接口一样，用 DeepSeek 的 API Key 放在 Authorization: Bearer 里。
// 网页是 DeepSeek 的服务器去取的，这台机器只和 api.deepseek.com 通信。
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrInvalid 表示参数或配置不合法。
	ErrInvalid = errors.New("deepseek: 参数不合法")
	// ErrUnavailable 表示接口调用失败。
	ErrUnavailable = errors.New("deepseek: 搜索服务暂时不可用")
)

const (
	// MaxQueryRunes 是搜索词的长度上限（接口规定 1～128 个字符）。
	MaxQueryRunes = 128
	// MaxCount 是一次搜索最多返回的条数（接口规定 1～50）。
	MaxCount = 50
	// MaxURLBytes 是抓取的网址长度上限（接口规定）。
	MaxURLBytes = 8192
	// CostMicrosPerRequest 是一次计费请求的价格，单位百万分之一元。
	// 两个接口都是每千次 5 元，只有成功返回结果的请求计费（2026-10 文档价格）。
	CostMicrosPerRequest = 5000
	// maxBytes 是一次返回内容的大小上限。
	maxBytes = 8 << 20
)

// DefaultBaseURL 是 DeepSeek 开放平台的地址。
const DefaultBaseURL = "https://api.deepseek.com"

// Client 是 DeepSeek 搜索与抓取接口的客户端。
type Client struct {
	// BaseURL 为空时用 DefaultBaseURL。
	BaseURL string
	// Secret 在每次请求时给出 DeepSeek 的 API Key。
	Secret func(ctx context.Context) (string, error)
	// HTTP 为空时使用带超时的默认客户端。
	HTTP *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// Result 是一条搜索结果。
type Result struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet,omitempty"`
	Summary  string `json:"summary,omitempty"`
	SiteName string `json:"site_name,omitempty"`
	Date     string `json:"date,omitempty"`
}

// Page 是抓取到的一个网页。
type Page struct {
	Title string `json:"title"`
	// Content 是网页正文，Markdown 格式。
	Content string `json:"content"`
}

// Search 按关键词搜索，最多返回 count 条。
func (c *Client) Search(ctx context.Context, query string, count int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("%w: 缺少搜索词", ErrInvalid)
	}
	if n := len([]rune(query)); n > MaxQueryRunes {
		return nil, fmt.Errorf("%w: 搜索词有 %d 个字符，超过上限 %d", ErrInvalid, n, MaxQueryRunes)
	}
	if count < 1 || count > MaxCount {
		return nil, fmt.Errorf("%w: 条数应在 1～%d 之间", ErrInvalid, MaxCount)
	}
	var out struct {
		Results []Result `json:"results"`
	}
	if err := c.post(ctx, "/v1/tools/search", map[string]any{"query": query, "count": count}, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Fetch 抓取一个网页的正文。
func (c *Client) Fetch(ctx context.Context, rawURL string) (Page, error) {
	rawURL = strings.TrimSpace(rawURL)
	if err := CheckURL(rawURL); err != nil {
		return Page{}, err
	}
	var page Page
	if err := c.post(ctx, "/v1/tools/fetch", map[string]any{"url": rawURL}, &page); err != nil {
		return Page{}, err
	}
	return page, nil
}

// CheckURL 校验要抓取的网址：只能是 http/https，不带账号密码，长度不超过接口上限。
func CheckURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("%w: 缺少网址", ErrInvalid)
	}
	if len(rawURL) > MaxURLBytes {
		return fmt.Errorf("%w: 网址超过 %d 字节", ErrInvalid, MaxURLBytes)
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: 只能抓取 http 或 https 网址", ErrInvalid)
	}
	if u.User != nil {
		return fmt.Errorf("%w: 网址里不能带账号密码", ErrInvalid)
	}
	return nil
}

// problem 是出错时返回的内容，格式与 OpenAI 的错误格式相同。
type problem struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	if c.Secret == nil {
		return fmt.Errorf("%w: 缺少 API Key", ErrInvalid)
	}
	key, err := c.Secret(ctx)
	if err != nil {
		return fmt.Errorf("deepseek: 取 API Key 失败: %w", err)
	}
	if key == "" {
		return fmt.Errorf("%w: 缺少 API Key", ErrInvalid)
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("%w: 读取返回内容失败: %v", ErrUnavailable, err)
	}
	if len(raw) > maxBytes {
		return fmt.Errorf("%w: 返回内容超过 %d MB", ErrUnavailable, maxBytes>>20)
	}
	if resp.StatusCode != http.StatusOK {
		var p problem
		_ = json.Unmarshal(raw, &p)
		var hint string
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			hint = "，请检查 API Key"
		case http.StatusPaymentRequired:
			hint = "，账户余额不足"
		case http.StatusTooManyRequests:
			hint = "，请求过于频繁"
		}
		if p.Error.Message != "" {
			return fmt.Errorf("%w: DeepSeek 返回 %s%s：%s", ErrUnavailable, resp.Status, hint, p.Error.Message)
		}
		return fmt.Errorf("%w: DeepSeek 返回 %s%s", ErrUnavailable, resp.Status, hint)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: 返回内容不是合法 JSON: %v", ErrUnavailable, err)
	}
	return nil
}
