// Package llm 是大模型的 provider 抽象与路由。
//
// 密钥由用户持有，配置由用户填写；这一层不绑定任何一家厂商。
// 见 docs/架构方案.md §8。
package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	// Images 是随这条消息一起给模型看的图片，只有多模态模型看得懂。绝大多数消息没有。
	Images []Image `json:"-"`
}

// Request 是一次补全请求。
type Request struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float64
	// JSONMode 要求模型输出合法 JSON。
	JSONMode bool
	// WebSearch 要求模型用它自带的联网搜索来回答。只有 Router.Search 会设置它：
	// 不是每家模型都有这个能力，发给没有的那些只会得到一段凭记忆写的回答。
	WebSearch bool
}

// Source 是模型联网搜索时引用的一个网页。
type Source struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url"`
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
	// CostMicros 是这次调用的开销，单位为百万分之一元。路由没配单价时为 0。
	CostMicros int64
	// Sources 是联网搜索时引用的网页。有的服务不返回这一项。
	Sources []Source
	// NoSearch 为真表示请求了联网搜索、而服务明确表明这次没有搜——回答来自模型自己的记忆。
	NoSearch bool
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
	// PurposeSearch 联网搜索，要用自带搜索能力的模型。它不走兜底路由：
	// 兜底的模型不一定能联网，拿它顶上只会得到一段看起来像查过的回答。
	PurposeSearch Purpose = "search"
)

// Route 把一个用途绑定到具体的 provider 与模型。
type Route struct {
	Purpose   Purpose
	Provider  string
	Model     string
	MaxTokens int
	// InputYuanPerMTok / OutputYuanPerMTok 是每百万 token 的单价（元）。
	// 不配就不计费——金额预算也就对这条路由不起作用。
	InputYuanPerMTok  float64
	OutputYuanPerMTok float64
}

// costMicros 按单价算出一次调用的开销。
//
// 单价是"元/百万 token"，结果是"百万分之一元"，两个百万正好约掉。
func (r Route) costMicros(u Usage) int64 {
	return int64(math.Round(float64(u.PromptTokens)*r.InputYuanPerMTok +
		float64(u.CompletionTokens)*r.OutputYuanPerMTok))
}

// Router 按用途把请求分发给不同的 provider。
type Router struct {
	providers map[string]Provider
	routes    map[Purpose]Route
	fallback  *Route
	workers   map[string]Route
}

// NewRouter 构造路由。
func NewRouter() *Router {
	return &Router{
		providers: make(map[string]Provider),
		routes:    make(map[Purpose]Route),
		workers:   make(map[string]Route),
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
	return r.run(ctx, "用途 "+string(purpose), route, req)
}

// CanSearch 报告是否配置了联网搜索用的模型。
func (r *Router) CanSearch() bool {
	_, ok := r.routes[PurposeSearch]
	return ok
}

// Search 让联网搜索用的模型带着搜索能力回答一次。
func (r *Router) Search(ctx context.Context, req Request) (Response, error) {
	route, ok := r.routes[PurposeSearch]
	if !ok {
		return Response{}, fmt.Errorf("%w: 没有配置能联网搜索的模型", ErrNoProvider)
	}
	req.WebSearch = true
	return r.run(ctx, "联网搜索", route, req)
}

// run 按一条路由执行补全并算出开销。
func (r *Router) run(ctx context.Context, what string, route Route, req Request) (Response, error) {
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
		return Response{}, fmt.Errorf("llm: %s 调用 %s/%s 失败: %w",
			what, route.Provider, route.Model, err)
	}
	resp.CostMicros = route.costMicros(resp.Usage)
	return resp, nil
}

// AddWorker 登记一个协作模型：主 AI 可以把一块活派给它。名字是用户起的，不能重复。
func (r *Router) AddWorker(name string, route Route) error {
	if name == "" {
		return fmt.Errorf("llm: 协作模型必须有名字")
	}
	if _, dup := r.workers[name]; dup {
		return fmt.Errorf("llm: 协作模型 %s 重复", name)
	}
	if _, ok := r.providers[route.Provider]; !ok {
		return fmt.Errorf("llm: 协作模型 %s 引用了未注册的 provider %q", name, route.Provider)
	}
	if route.Model == "" {
		return fmt.Errorf("llm: 协作模型 %s 未指定模型", name)
	}
	r.workers[name] = route
	return nil
}

// CompleteWith 让指定的协作模型执行一次补全。
func (r *Router) CompleteWith(ctx context.Context, worker string, req Request) (Response, error) {
	route, ok := r.workers[worker]
	if !ok {
		return Response{}, fmt.Errorf("%w: 没有名叫 %s 的协作模型", ErrNoProvider, worker)
	}
	return r.run(ctx, "协作模型 "+worker, route, req)
}

// Routes 返回当前路由配置，供启动日志与诊断使用。
func (r *Router) Routes() map[Purpose]Route {
	out := make(map[Purpose]Route, len(r.routes))
	for k, v := range r.routes {
		out[k] = v
	}
	return out
}
