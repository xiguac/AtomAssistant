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
)

// AnthropicCompatible 适配 Anthropic Messages 接口，以及按它的格式提供服务的其他厂商。
//
// 端点约定：POST {BaseURL}/v1/messages。BaseURL 已经以 /v1 结尾时不再重复加。
//
// 和 OpenAI 兼容接口相比有三处不同，都在这里抹平，上层感觉不到：
// 系统提示不是一条消息而是单独的字段；max_tokens 必填；没有"只输出 JSON"的开关——
// JSONMode 在这里靠提示词保证，所以默认协议仍然是 OpenAI 兼容。
type AnthropicCompatible struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

var _ Provider = (*AnthropicCompatible)(nil)

const (
	anthropicVersion = "2023-06-01"
	// anthropicDefaultMaxTokens 是调用方没有指定长度上限时用的值：这个接口不允许不填。
	anthropicDefaultMaxTokens = 4096
)

// NewAnthropicCompatible 构造 provider。配置项的含义与 OpenAIConfig 相同。
func NewAnthropicCompatible(cfg OpenAIConfig) (*AnthropicCompatible, error) {
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
	return &AnthropicCompatible{
		name:    cfg.Name,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		client:  &http.Client{Timeout: cfg.Timeout},
	}, nil
}

func (p *AnthropicCompatible) Name() string { return p.name }

func (p *AnthropicCompatible) endpoint() string {
	if strings.HasSuffix(p.baseURL, "/v1") {
		return p.baseURL + "/messages"
	}
	return p.baseURL + "/v1/messages"
}

type anthropicRequest struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	Tools       []any     `json:"tools,omitempty"`
}

// webSearchTool 是服务端执行的联网搜索工具：搜索由模型服务自己完成，我们只拿结果。
var webSearchTool = map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 3}

type anthropicResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// Content 只在 web_search_tool_result 块里有：一组搜到的网页，或一个错误对象。
		Content json.RawMessage `json:"content"`
	} `json:"content"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete 执行一次补全。
func (p *AnthropicCompatible) Complete(ctx context.Context, req Request) (Response, error) {
	if len(req.Messages) == 0 {
		return Response{}, fmt.Errorf("llm: 消息列表为空")
	}
	body := anthropicRequest{Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature}
	if body.MaxTokens <= 0 {
		body.MaxTokens = anthropicDefaultMaxTokens
	}
	// 系统提示单独成一个字段；其余的只有 user 和 assistant 两种角色。
	var system []string
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			system = append(system, m.Content)
		case RoleAssistant:
			body.Messages = append(body.Messages, Message{Role: RoleAssistant, Content: m.Content})
		default:
			body.Messages = append(body.Messages, Message{Role: RoleUser, Content: m.Content})
		}
	}
	body.System = strings.Join(system, "\n\n")
	if len(body.Messages) == 0 {
		return Response{}, fmt.Errorf("llm: 除了系统提示之外没有任何消息")
	}
	if req.WebSearch {
		body.Tools = []any{webSearchTool}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: 序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

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
	var parsed anthropicResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("%w: %s 返回了非 JSON 内容（HTTP %d）: %s",
			ErrUpstream, p.name, resp.StatusCode, truncate(string(raw), 200))
	}
	if resp.StatusCode != http.StatusOK {
		msg := truncate(string(raw), 200)
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return Response{}, fmt.Errorf("%w: %s 返回 HTTP %d: %s", ErrUpstream, p.name, resp.StatusCode, msg)
	}
	if parsed.Error != nil {
		return Response{}, fmt.Errorf("%w: %s: %s", ErrUpstream, p.name, parsed.Error.Message)
	}
	// 只取文字块。思考过程（thinking）是另一种块，不是答案的一部分。
	var text strings.Builder
	var sources []Source
	searched := false
	seen := map[string]bool{}
	for _, c := range parsed.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "server_tool_use":
			searched = true
		case "web_search_tool_result":
			searched = true
			// 搜索出错时这里是一个对象而不是数组，解不出来就当没有来源。
			var results []Source
			if err := json.Unmarshal(c.Content, &results); err != nil {
				continue
			}
			for _, s := range results {
				if s.URL != "" && !seen[s.URL] {
					seen[s.URL] = true
					sources = append(sources, s)
				}
			}
		}
	}
	if len(parsed.Content) == 0 {
		return Response{}, fmt.Errorf("%w: %s 返回了空结果", ErrUpstream, p.name)
	}
	model := parsed.Model
	if model == "" {
		model = req.Model
	}
	return Response{
		Content: text.String(),
		Model:   model,
		Usage:   Usage{PromptTokens: parsed.Usage.InputTokens, CompletionTokens: parsed.Usage.OutputTokens},
		Sources: sources,
		// 这个协议下搜没搜是看得见的：一次搜索都没有，回答就只是模型的记忆。
		NoSearch: req.WebSearch && !searched,
	}, nil
}
