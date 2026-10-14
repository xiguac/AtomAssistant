package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// OpenAICompatible 适配任何 OpenAI Chat Completions 兼容的接口。
//
// 选它作为第一个实现，是因为国内主流服务（DeepSeek、月之暗面、智谱、
// 通义的兼容模式等）大多提供 OpenAI 兼容端点，一个适配器就能覆盖大部分场景。
// 不兼容的厂商再单独写 Provider。
//
// 端点约定：POST {BaseURL}/chat/completions
type OpenAICompatible struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

var _ Provider = (*OpenAICompatible)(nil)

// OpenAIConfig 是 provider 的配置，全部由用户填写。
type OpenAIConfig struct {
	// Name 是配置里引用这家 provider 的标识，如 "deepseek"。
	Name string
	// BaseURL 形如 https://api.example.com/v1，不含 /chat/completions。
	BaseURL string
	// APIKey 由用户持有。我们的任何服务都不接触它。
	APIKey  string
	Timeout time.Duration
}

// NewOpenAICompatible 构造 provider。
func NewOpenAICompatible(cfg OpenAIConfig) (*OpenAICompatible, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("llm: provider 名称不能为空")
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("llm: provider %s 缺少 BaseURL", cfg.Name)
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("llm: provider %s 缺少 API Key", cfg.Name)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	return &OpenAICompatible{
		name:    cfg.Name,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		client:  &http.Client{Timeout: cfg.Timeout},
	}, nil
}

func (p *OpenAICompatible) Name() string { return p.name }

type chatRequest struct {
	Model          string   `json:"model"`
	Messages       []any    `json:"messages"`
	MaxTokens      int      `json:"max_tokens,omitempty"`
	Temperature    float64  `json:"temperature,omitempty"`
	ResponseFormat *respFmt `json:"response_format,omitempty"`
	// 以下两项是阿里云百炼的联网搜索开关，只在请求联网搜索时带上。
	EnableSearch  bool           `json:"enable_search,omitempty"`
	SearchOptions *searchOptions `json:"search_options,omitempty"`
}

type respFmt struct {
	Type string `json:"type"`
}

type searchOptions struct {
	// ForcedSearch 让模型一定去搜，而不是自己判断要不要搜。
	ForcedSearch bool `json:"forced_search"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		// 命中前缀缓存的输入：DeepSeek 报在 prompt_cache_hit_tokens，
		// OpenAI 和多数兼容它的服务报在 prompt_tokens_details.cached_tokens。
		PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
		PromptTokensDetails  struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete 执行一次补全。
func (p *OpenAICompatible) Complete(ctx context.Context, req Request) (Response, error) {
	if len(req.Messages) == 0 {
		return Response{}, fmt.Errorf("llm: 消息列表为空")
	}
	body := chatRequest{
		Model:       req.Model,
		Messages:    openAIMessages(req.Messages),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if req.JSONMode {
		body.ResponseFormat = &respFmt{Type: "json_object"}
	}
	if req.WebSearch {
		body.EnableSearch = true
		body.SearchOptions = &searchOptions{ForcedSearch: true}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: 序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %s 不可达: %w", ErrUpstream, p.name, err)
	}
	defer resp.Body.Close()

	// 限制读取大小：上游异常时可能返回巨大的响应体，不能让它吃光内存。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, fmt.Errorf("%w: 读取 %s 响应失败: %w", ErrUpstream, p.name, err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("%w: %s 返回了非 JSON 内容（HTTP %d）: %s",
			ErrUpstream, p.name, resp.StatusCode, truncate(string(raw), 200))
	}
	if resp.StatusCode != http.StatusOK {
		msg := truncate(string(raw), 200)
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return Response{}, fmt.Errorf("%w: %s 返回 HTTP %d: %s",
			ErrUpstream, p.name, resp.StatusCode, msg)
	}
	if parsed.Error != nil {
		return Response{}, fmt.Errorf("%w: %s: %s", ErrUpstream, p.name, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("%w: %s 返回了空结果", ErrUpstream, p.name)
	}

	model := parsed.Model
	if model == "" {
		model = req.Model
	}
	return Response{
		Content: parsed.Choices[0].Message.Content,
		Model:   model,
		Usage: Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			CachedTokens:     max(parsed.Usage.PromptCacheHitTokens, parsed.Usage.PromptTokensDetails.CachedTokens),
		},
	}, nil
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// 退到字符边界再切，否则中文会被切出半个字符。
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}
