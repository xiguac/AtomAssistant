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
	dialect Dialect
	idle    time.Duration
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
		cfg.Timeout = defaultIdleTimeout
	}
	return &AnthropicCompatible{
		name:    cfg.Name,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		dialect: cfg.Dialect,
		idle:    cfg.Timeout,
		// 同 OpenAICompatible：时限交给看门狗，不给 client 设总时限。
		client: &http.Client{},
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
	Model       string  `json:"model"`
	MaxTokens   int     `json:"max_tokens"`
	System      string  `json:"system,omitempty"`
	Messages    []any   `json:"messages"`
	Temperature float64 `json:"temperature,omitempty"`
	Tools       []any   `json:"tools,omitempty"`
	Stream      bool    `json:"stream,omitempty"`
	// Reasoning 是 DeepSeek 的 Anthropic 兼容接口上的思考强度，见 anthropicThinking。
	Reasoning *reasoningEffort `json:"reasoning,omitempty"`
}

// webSearchTool 是服务端执行的联网搜索工具：搜索由模型服务自己完成，我们只拿结果。
var webSearchTool = map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 3}

type anthropicResponse struct {
	Model   string           `json:"model"`
	Content []anthropicBlock `json:"content"`
	Usage   anthropicUsage   `json:"usage"`
	Error   *anthropicError  `json:"error"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Content 只在 web_search_tool_result 块里有：一组搜到的网页，或一个错误对象。
	Content json.RawMessage `json:"content"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// anthropicEvent 是流式返回里的一个事件，各种事件的字段合在一起。
type anthropicEvent struct {
	Type string `json:"type"`
	// message_start 带着模型名和输入用量。
	Message *struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	// content_block_start 带着一个新块；搜索结果块在这里一次给全。
	Index        int             `json:"index"`
	ContentBlock *anthropicBlock `json:"content_block"`
	// content_block_delta 带着块的增量。
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	// message_delta 带着累计的输出用量。
	Usage *anthropicUsage `json:"usage"`
	Error *anthropicError `json:"error"`
}

// readAnthropicStream 把流式返回拼成一份完整的返回。
func readAnthropicStream(r io.Reader) (anthropicResponse, error) {
	var out anthropicResponse
	err := readEvents(r, func(data []byte) error {
		var e anthropicEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return fmt.Errorf("流里有一段不是 JSON: %s", truncate(string(data), 200))
		}
		switch e.Type {
		case "message_start":
			if e.Message != nil {
				out.Model = e.Message.Model
				out.Usage = e.Message.Usage
			}
		case "content_block_start":
			if e.ContentBlock != nil && e.Index == len(out.Content) {
				out.Content = append(out.Content, *e.ContentBlock)
			}
		case "content_block_delta":
			// 只拼文字；思考过程和工具参数的增量用不上。
			if e.Delta != nil && e.Delta.Type == "text_delta" && e.Index < len(out.Content) {
				out.Content[e.Index].Text += e.Delta.Text
			}
		case "message_delta":
			if e.Usage != nil {
				out.Usage.OutputTokens = e.Usage.OutputTokens
				if e.Usage.InputTokens > 0 {
					out.Usage.InputTokens = e.Usage.InputTokens
				}
			}
		case "error":
			out.Error = e.Error
		}
		return nil
	})
	return out, err
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
			msg, err := anthropicMessage(RoleAssistant, m)
			if err != nil {
				return Response{}, err
			}
			body.Messages = append(body.Messages, msg)
		default:
			msg, err := anthropicMessage(RoleUser, m)
			if err != nil {
				return Response{}, err
			}
			body.Messages = append(body.Messages, msg)
		}
	}
	body.System = strings.Join(system, "\n\n")
	if len(body.Messages) == 0 {
		return Response{}, fmt.Errorf("llm: 除了系统提示之外没有任何消息")
	}
	if req.WebSearch {
		body.Tools = []any{webSearchTool}
	}
	anthropicThinking(p.dialect, req.Thinking, &body)
	// 流式取回，理由同 OpenAICompatible。
	body.Stream = true
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: 序列化请求失败: %w", err)
	}

	ctx, dog := startWatchdog(ctx, p.idle)
	defer dog.stop()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return Response{}, dog.explain(ctx, p.name, fmt.Errorf("%w: %s 不可达: %w", ErrUpstream, p.name, err))
	}
	defer resp.Body.Close()
	respBody := dog.body(resp.Body)

	var parsed anthropicResponse
	if isEventStream(resp) {
		if parsed, err = readAnthropicStream(respBody); err != nil {
			return Response{}, dog.explain(ctx, p.name, fmt.Errorf("%w: 读取 %s 响应失败: %w", ErrUpstream, p.name, err))
		}
		return p.finish(parsed, req)
	}

	// 限制读取大小：上游异常时可能返回巨大的响应体，不能让它吃光内存。
	raw, err := io.ReadAll(io.LimitReader(respBody, 8<<20))
	if err != nil {
		return Response{}, dog.explain(ctx, p.name, fmt.Errorf("%w: 读取 %s 响应失败: %w", ErrUpstream, p.name, err))
	}
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
	return p.finish(parsed, req)
}

// finish 从完整的一份返回里取出答案。流式返回先拼成同样的形状再交给它。
func (p *AnthropicCompatible) finish(parsed anthropicResponse, req Request) (Response, error) {
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
