// Package llm 是大模型的 provider 抽象与路由。
//
// 密钥由用户持有，配置由用户填写；这一层不绑定任何一家厂商。
// 见 docs/架构方案.md §8。
package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
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
	// Audio 与 Video 是随这条消息给模型听、看的语音和视频，只有支持对应输入的模型收得了。
	Audio []Image `json:"-"`
	Video []Image `json:"-"`
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
	// Thinking 是这次调用的思考强度。路由配了而调用方没指定时，用路由的。
	Thinking Thinking
	// Tools 是这次可供模型调用的工具，见 tools.go。模型可以调用其中的一个或几个，也可以直接回答。
	Tools []Tool
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
	// CachedTokens 是 PromptTokens 里命中模型服务前缀缓存的部分。服务不报这一项时为 0。
	CachedTokens int64
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
	// Timing 是这次调用各阶段的耗时，用来查"为什么这么慢"。provider 量不到的项为 0。
	Timing Timing
	// ToolCalls 是模型发起的工具调用，只在请求带了 Tools 时可能有。
	ToolCalls []ToolCall
}

// Timing 是一次调用各阶段的耗时，都从发出请求算起。
type Timing struct {
	// FirstByte 是收到第一段数据的时间。
	FirstByte time.Duration
	// FirstContent 是收到第一段正文的时间。它比 FirstByte 晚得多，说明模型先想了很久。
	FirstContent time.Duration
	// ReasoningChars 是模型思考过程的字数。思考过程不交给调用方，只计数。
	ReasoningChars int
	// FinishReason 是模型服务报的结束原因（stop、length 等）。
	FinishReason string
	// ReasoningHasJSON 为真表示思考过程里出现过 JSON 对象。正文为空时用它判断答案是不是写进了思考里。
	// 只记这一点，不记思考的原文：里面可能有用户的隐私，日志不该有。
	ReasoningHasJSON bool
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
	// Thinking 是这个模型的思考强度，空值按模型服务的默认。
	Thinking Thinking
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
	log       *slog.Logger
	usage     func(Usage, int64)
}

// WithLogger 让每次模型调用都记一行耗时与用量。不设就不记。
func (r *Router) WithLogger(l *slog.Logger) *Router {
	r.log = l
	return r
}

// WithUsage 让每次成功的模型调用都把用量和开销（百万分之一元）交给 fn，后台按天统计 token 用它。
// fn 在调用方的 goroutine 里同步执行，要快。
func (r *Router) WithUsage(fn func(u Usage, costMicros int64)) *Router {
	r.usage = fn
	return r
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
	if req.Thinking == ThinkingDefault {
		req.Thinking = route.Thinking
	}
	var spent Usage
	var cost int64
	for attempt := 0; ; attempt++ {
		start := time.Now()
		resp, err := p.Complete(ctx, req)
		r.logCall(what, route, req, resp, time.Since(start), err)
		if err != nil {
			return Response{}, fmt.Errorf("llm: %s 调用 %s/%s 失败: %w",
				what, route.Provider, route.Model, err)
		}
		resp.CostMicros = route.costMicros(resp.Usage)
		if r.usage != nil {
			r.usage(resp.Usage, resp.CostMicros)
		}
		spent.PromptTokens += resp.Usage.PromptTokens
		spent.CompletionTokens += resp.Usage.CompletionTokens
		spent.CachedTokens += resp.Usage.CachedTokens
		cost += resp.CostMicros
		// JSON 模式下模型偶尔只吐一串空白就结束了（DeepSeek 上实测约每十几次一次）。
		// 当场再问，最后一次不用 JSON 模式：空白多半出在 JSON 模式上，调用方本来就会从正文里解析 JSON。
		// 这比交回调用方失败、再由内核退避 10 秒到 1 分钟重试快得多。
		// 带着工具的请求同理：既没有调用工具、正文又是空白，也当场再问一次。
		answered := strings.TrimSpace(resp.Content) != "" || len(resp.ToolCalls) > 0
		if (!req.JSONMode && len(req.Tools) == 0) || answered || attempt >= blankRetries || ctx.Err() != nil {
			resp.Usage, resp.CostMicros = spent, cost
			return resp, nil
		}
		if attempt == blankRetries-1 {
			req.JSONMode = false
		}
	}
}

// blankRetries 是 JSON 模式返回空白时当场重问的次数。
const blankRetries = 2

// logCall 记下一次调用花了多久、用了多少。只记数字，不记内容。
func (r *Router) logCall(what string, route Route, req Request, resp Response, total time.Duration, err error) {
	if r.log == nil {
		return
	}
	chars := 0
	for _, m := range req.Messages {
		chars += len([]rune(m.Content))
	}
	args := []any{
		"用途", what, "provider", route.Provider, "模型", route.Model,
		"总耗时", total.Round(time.Millisecond),
		"首段数据", resp.Timing.FirstByte.Round(time.Millisecond),
		"首段正文", resp.Timing.FirstContent.Round(time.Millisecond),
		"思考字数", resp.Timing.ReasoningChars,
		"输入字数", chars,
		"输入token", resp.Usage.PromptTokens, "缓存token", resp.Usage.CachedTokens,
		"输出token", resp.Usage.CompletionTokens,
		"结束原因", resp.Timing.FinishReason, "消息条数", len(req.Messages),
	}
	if len(req.Tools) > 0 {
		args = append(args, "可用工具数", len(req.Tools), "工具调用数", len(resp.ToolCalls))
	}
	if err != nil {
		r.log.Warn("模型调用失败", append(args, "err", err)...)
		return
	}
	// 正文为空的那一次要能查出原因：结束原因、正文里到底是什么（这时只有空白，不涉及内容）、
	// 答案是不是写进了思考里。
	if strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0 {
		raw := resp.Content
		if len(raw) > 40 {
			raw = raw[:40]
		}
		r.log.Warn("模型调用正文为空", append(args,
			"JSON模式", req.JSONMode, "正文原样", fmt.Sprintf("%q", raw),
			"思考里有JSON", resp.Timing.ReasoningHasJSON)...)
		return
	}
	r.log.Info("模型调用", args...)
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
