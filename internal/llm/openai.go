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
	dialect Dialect
	idle    time.Duration
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
	APIKey string
	// Timeout 是首字超时：发出请求后多久没收到任何数据就放弃，之后两段数据之间也按它算。
	// 不写按 10 分钟。整次请求没有总时限。
	Timeout time.Duration
	// Dialect 是这家服务特有的参数写法，决定思考强度怎么传。
	Dialect Dialect
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
		cfg.Timeout = defaultIdleTimeout
	}
	return &OpenAICompatible{
		name:    cfg.Name,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		dialect: cfg.Dialect,
		idle:    cfg.Timeout,
		// 不给 client 设 Timeout：它管的是整次请求，长回答流到一半会被它掐断。时限交给看门狗。
		client: &http.Client{},
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
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	// 以下是思考强度，各家写法不同，见 openAIThinking。
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Thinking        *thinkingSwitch `json:"thinking,omitempty"`
	EnableThinking  *bool           `json:"enable_thinking,omitempty"`
	ThinkingBudget  int             `json:"thinking_budget,omitempty"`
}

type streamOptions struct {
	// IncludeUsage 让流的最后一段带上用量，不开的话流式返回里没有用量。
	IncludeUsage bool `json:"include_usage"`
}

type respFmt struct {
	Type string `json:"type"`
}

type searchOptions struct {
	// ForcedSearch 让模型一定去搜，而不是自己判断要不要搜。
	ForcedSearch bool `json:"forced_search"`
}

type chatResponse struct {
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   struct {
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

	// 以下两项只在流式返回里由 readOpenAIStream 填：第一段正文到达的时间、思考过程的字数。
	firstContent   time.Time
	reasoningChars int
}

// Complete 执行一次补全。
func (p *OpenAICompatible) Complete(ctx context.Context, req Request) (Response, error) {
	if len(req.Messages) == 0 {
		return Response{}, fmt.Errorf("llm: 消息列表为空")
	}
	body := chatRequest{
		Model:       req.Model,
		Messages:    openAIMessages(p.dialect, req.Messages),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if req.JSONMode {
		body.ResponseFormat = &respFmt{Type: "json_object"}
	}
	openAIThinking(p.dialect, req.Thinking, &body)
	// 流式取回：强模型想得久，一次性返回要等它全部写完才有响应头，容易超时。
	body.Stream = true
	body.StreamOptions = &streamOptions{IncludeUsage: true}
	if req.WebSearch {
		body.EnableSearch = true
		body.SearchOptions = &searchOptions{ForcedSearch: true}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: 序列化请求失败: %w", err)
	}

	ctx, dog := startWatchdog(ctx, p.idle)
	defer dog.stop()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return Response{}, dog.explain(ctx, p.name, fmt.Errorf("%w: %s 不可达: %w", ErrUpstream, p.name, err))
	}
	defer resp.Body.Close()
	respBody := dog.body(resp.Body)

	var parsed chatResponse
	if isEventStream(resp) {
		if parsed, err = readOpenAIStream(respBody); err != nil {
			return Response{}, dog.explain(ctx, p.name, fmt.Errorf("%w: 读取 %s 响应失败: %w", ErrUpstream, p.name, err))
		}
		out, err := p.finish(parsed, req)
		out.Timing = Timing{
			FirstByte:      dog.since(dog.first),
			FirstContent:   dog.since(parsed.firstContent),
			ReasoningChars: parsed.reasoningChars,
		}
		return out, err
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
		return Response{}, fmt.Errorf("%w: %s 返回 HTTP %d: %s",
			ErrUpstream, p.name, resp.StatusCode, msg)
	}
	return p.finish(parsed, req)
}

// finish 从完整的一份返回里取出答案。流式返回先拼成同样的形状再交给它。
func (p *OpenAICompatible) finish(parsed chatResponse, req Request) (Response, error) {
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

type chatChoice struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

// chatChunk 是流式返回里的一段。用量只在最后一段里有，那一段的 choices 为空。
type chatChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens         int64 `json:"prompt_tokens"`
		CompletionTokens     int64 `json:"completion_tokens"`
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

// readOpenAIStream 把流式返回拼成一份完整的返回。思考过程（reasoning_content）不要。
func readOpenAIStream(r io.Reader) (chatResponse, error) {
	var out chatResponse
	var text strings.Builder
	gotChoice := false
	err := readEvents(r, func(data []byte) error {
		var c chatChunk
		if err := json.Unmarshal(data, &c); err != nil {
			return fmt.Errorf("流里有一段不是 JSON: %s", truncate(string(data), 200))
		}
		if c.Error != nil {
			out.Error = c.Error
			return nil
		}
		if c.Model != "" {
			out.Model = c.Model
		}
		for _, ch := range c.Choices {
			gotChoice = true
			text.WriteString(ch.Delta.Content)
			out.reasoningChars += utf8.RuneCountInString(ch.Delta.ReasoningContent)
			if ch.Delta.Content != "" && out.firstContent.IsZero() {
				out.firstContent = time.Now()
			}
		}
		if c.Usage != nil {
			out.Usage.PromptTokens = c.Usage.PromptTokens
			out.Usage.CompletionTokens = c.Usage.CompletionTokens
			out.Usage.PromptCacheHitTokens = c.Usage.PromptCacheHitTokens
			out.Usage.PromptTokensDetails.CachedTokens = c.Usage.PromptTokensDetails.CachedTokens
		}
		return nil
	})
	if err != nil {
		return chatResponse{}, err
	}
	if gotChoice {
		var c chatChoice
		c.Message.Content = text.String()
		out.Choices = []chatChoice{c}
	}
	return out, nil
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
