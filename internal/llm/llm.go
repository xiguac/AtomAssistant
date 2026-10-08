// Package llm 是大模型的 provider 抽象与路由。
//
// 密钥由用户持有，配置由用户填写；这一层不绑定任何一家厂商。
// 见 docs/架构方案.md §8。
package llm

import (
	"context"
	"errors"
	"fmt"
)

// Role 是消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 是发给模型的一条消息。
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// Request 是一次补全请求。
type Request struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float64
	// JSONMode 要求模型输出合法 JSON。
	JSONMode bool
}

// Usage 是一次调用的用量。
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
}

// Response 是模型返回。
type Response struct {
	Content string
	Usage   Usage
	Model   string
}

// Provider 是一家模型服务的适配器。
type Provider interface {
	// Name 是配置里用来引用这家 provider 的标识。
	Name() string
	// Complete 执行一次补全。
	Complete(ctx context.Context, req Request) (Response, error)
}

var (
	// ErrNoProvider 表示路由找不到可用的 provider。
	ErrNoProvider = errors.New("llm: 没有可用的 provider")
	// ErrUpstream 表示上游服务返回错误。
	ErrUpstream = errors.New("llm: 上游服务错误")
)

// Purpose 是调用意图，路由据此选择模型。
//
// 规划用强模型，打标/摘要/抽取用便宜模型——这是成本控制里最有效的一招，
// 因为后者的调用量通常是前者的几十倍。
type Purpose string

const (
	// PurposePlan 任务规划与决策，需要最强的模型。
	PurposePlan Purpose = "plan"
	// PurposeExtract 结构化抽取、打标、分类，便宜模型足够。
	PurposeExtract Purpose = "extract"
	// PurposeSummarize 摘要与压缩，需要长上下文但不需要强推理。
	PurposeSummarize Purpose = "summarize"
	// PurposeVision 截图理解，需要多模态模型。
	PurposeVision Purpose = "vision"
)

// Route 把一个用途绑定到具体的 provider 与模型。
type Route struct {
	Purpose  Purpose
	Provider string
	Model    string
	// MaxCostCentsPerCall 是单次调用的成本上限估算，0 表示不限。
	MaxTokens int
}

// Router 按用途把请求分发给不同的 provider。
type Router struct {
	providers map[string]Provider
	routes    map[Purpose]Route
	fallback  *Route
}

// NewRouter 构造路由。
func NewRouter() *Router {
	return &Router{
		providers: make(map[string]Provider),
		routes:    make(map[Purpose]Route),
	}
}

// AddProvider 注册一个 provider。
func (r *Router) AddProvider(p Provider) error {
	if p == nil || p.Name() == "" {
		return fmt.Errorf("llm: provider 必须有名字")
	}
	if _, dup := r.providers[p.Name()]; dup {
		return fmt.Errorf("llm: provider %s 重复注册", p.Name())
	}
	r.providers[p.Name()] = p
	return nil
}

// SetRoute 绑定一个用途到具体模型。
func (r *Router) SetRoute(route Route) error {
	if _, ok := r.providers[route.Provider]; !ok {
		return fmt.Errorf("llm: 路由引用了未注册的 provider %q", route.Provider)
	}
	if route.Model == "" {
		return fmt.Errorf("llm: 路由 %s 未指定模型", route.Purpose)
	}
	r.routes[route.Purpose] = route
	return nil
}

// SetFallback 设置兜底路由：没有为某个用途单独配置时使用。
func (r *Router) SetFallback(route Route) error {
	if _, ok := r.providers[route.Provider]; !ok {
		return fmt.Errorf("llm: 兜底路由引用了未注册的 provider %q", route.Provider)
	}
	if route.Model == "" {
		return fmt.Errorf("llm: 兜底路由未指定模型")
	}
	r.fallback = &route
	return nil
}

// Complete 按用途路由并执行一次补全。
func (r *Router) Complete(ctx context.Context, purpose Purpose, req Request) (Response, error) {
	route, ok := r.routes[purpose]
	if !ok {
		if r.fallback == nil {
			return Response{}, fmt.Errorf("%w: 用途 %s 未配置路由且无兜底", ErrNoProvider, purpose)
		}
		route = *r.fallback
	}
	p := r.providers[route.Provider]
	if p == nil {
		return Response{}, fmt.Errorf("%w: %s", ErrNoProvider, route.Provider)
	}

	req.Model = route.Model
	if req.MaxTokens == 0 {
		req.MaxTokens = route.MaxTokens
	}
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return Response{}, fmt.Errorf("llm: 用途 %s 调用 %s/%s 失败: %w",
			purpose, route.Provider, route.Model, err)
	}
	return resp, nil
}

// Routes 返回当前路由配置，供启动日志与诊断使用。
func (r *Router) Routes() map[Purpose]Route {
	out := make(map[Purpose]Route, len(r.routes))
	for k, v := range r.routes {
		out[k] = v
	}
	return out
}
