package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/memory"
)

// ToolCatalog 提供可用工具列表。
type ToolCatalog interface {
	Catalog() []mcpgw.ToolInfo
	Lookup(name string) (mcpgw.ToolInfo, error)
}

const plannerSystemPrompt = `你是原子助手的任务规划器。你的职责是决定任务的下一步做什么。

必须输出一个 JSON 对象，不要输出任何其他内容：
{
  "action": "tool" | "wait" | "finish" | "fail",
  "tool": "工具全名，action=tool 时必填",
  "params": {},
  "wait_seconds": 数字，action=wait 时必填,
  "wait_event": "可选。事件订阅表达式，action=wait 时填写表示等这个事件",
  "reason": "一句话说明理由，会展示给用户"
}

等待有两种：
- 只填 wait_seconds：等这么多秒后继续。
- 同时填 wait_event：等到事件发生就继续，wait_seconds 是等不到时的超时。
  事件发生时它的内容会作为一条步骤输出出现在历史里；历史里没有它，说明是超时醒来的。
  表达式语法：字段只有 topic、source、payload.字段路径；
  运算符 == != > >= < <= CONTAINS，用 AND / OR / NOT 和括号组合；字符串用双引号。
  例：topic == "calendar.event.created" AND payload.attendees CONTAINS "客户张总"

规则：
- 只能使用下面列出的工具，名字必须完全一致。
- 不要声明工具的副作用等级，那由系统根据用户授权决定，你说了不算。
- 已经做过的事不要重复做；已经排除的方案不要再试。
- 目标达成就输出 finish；无法继续就输出 fail 并说明原因。`

// PlannerOptions 控制规划器行为。
type PlannerOptions struct {
	// RecentSteps 是保留原文的最近步骤数，更早的压缩成一行摘要。
	// 长任务跑几十步后，全量历史必然超出任何上下文窗口。
	RecentSteps int
	// MaxChars 是上下文字符预算。
	MaxChars int
	// MaxWait 限制单次等待时长，防止模型给出一个荒谬的等待时间。
	MaxWait time.Duration
	// MemoryChars 是记忆环的字符预算。
	MemoryChars int
}

func (o *PlannerOptions) applyDefaults() {
	if o.RecentSteps <= 0 {
		o.RecentSteps = 6
	}
	if o.MaxChars <= 0 {
		o.MaxChars = 24000
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 7 * 24 * time.Hour
	}
	if o.MemoryChars <= 0 {
		o.MemoryChars = o.MaxChars / 3
	}
}

// Planner 用大模型决定任务的下一步。
type Planner struct {
	model   Completer
	catalog ToolCatalog
	memory  MemoryProvider
	opts    PlannerOptions
	now     func() time.Time
}

var _ kernel.Planner = (*Planner)(nil)

// NewPlanner 构造规划器。
func NewPlanner(model Completer, catalog ToolCatalog, opts PlannerOptions) *Planner {
	opts.applyDefaults()
	return &Planner{
		model:   model,
		catalog: catalog,
		opts:    opts,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithMemory 接入记忆系统。接入后上下文会带上分环的记忆，
// 并向模型开放 memory.expand 用于按需展开索引行。
func (p *Planner) WithMemory(m MemoryProvider) *Planner {
	p.memory = m
	return p
}

// lookup 在内置工具与 MCP 目录里查找。
func (p *Planner) lookup(name string) (mcpgw.ToolInfo, error) {
	for _, t := range builtinTools(p.memory != nil) {
		if t.Name == name {
			return t, nil
		}
	}
	return p.catalog.Lookup(name)
}

// modelDecision 是模型被要求输出的结构。
type modelDecision struct {
	Action      string          `json:"action"`
	Tool        string          `json:"tool"`
	Params      json.RawMessage `json:"params"`
	WaitSeconds int64           `json:"wait_seconds"`
	WaitEvent   string          `json:"wait_event"`
	Reason      string          `json:"reason"`
}

// Decide 询问模型下一步做什么，并把回答校验成内核能执行的决策。
func (p *Planner) Decide(ctx context.Context, t *core.Task, history []*core.Step) (kernel.Decision, error) {
	segs, err := p.contextSegments(ctx, t, history)
	if err != nil {
		return kernel.Decision{}, err
	}
	msgs, err := llm.Build(segs, llm.BuildOptions{
		SystemPrompt: plannerSystemPrompt + "\n\n可用工具：\n" + p.toolList(),
		MaxChars:     p.opts.MaxChars,
	})
	if err != nil {
		return kernel.Decision{}, err
	}

	resp, err := p.model.Complete(ctx, llm.PurposePlan, llm.Request{
		Messages: msgs,
		JSONMode: true,
	})
	if err != nil {
		return kernel.Decision{}, err
	}

	var md modelDecision
	if err := json.Unmarshal([]byte(stripFence(resp.Content)), &md); err != nil {
		return kernel.Decision{}, fmt.Errorf("agent: 模型输出不是合法 JSON: %w (原文: %s)",
			err, truncate(resp.Content, 200))
	}
	d, err := p.validate(md)
	if err != nil {
		return kernel.Decision{}, err
	}
	// 规划本身的用量交给内核记账，否则任务预算管不到最贵的那部分开销。
	d.TokensIn = resp.Usage.PromptTokens
	d.TokensOut = resp.Usage.CompletionTokens
	return d, nil
}

// validate 把模型的回答校验成决策。
//
// 这里是"模型不可信"原则的落点：工具必须在目录里，副作用等级取自目录而不是模型自报，
// 等待时长有上限。模型能影响做什么，但不能影响能做什么。
func (p *Planner) validate(md modelDecision) (kernel.Decision, error) {
	switch md.Action {
	case "finish":
		return kernel.Decision{Kind: kernel.DecideFinish, Reason: md.Reason}, nil
	case "fail":
		return kernel.Decision{Kind: kernel.DecideFail, Reason: md.Reason}, nil
	case "wait":
		if md.WaitSeconds <= 0 {
			return kernel.Decision{}, fmt.Errorf("agent: wait 缺少正的 wait_seconds")
		}
		d := time.Duration(md.WaitSeconds) * time.Second
		if d > p.opts.MaxWait {
			d = p.opts.MaxWait
		}
		at := p.now().Add(d)
		if expr := strings.TrimSpace(md.WaitEvent); expr != "" {
			// 表达式是模型写的，语法错误要在这里拦下并反馈给它，
			// 而不是登记一条永远匹配不上的订阅。
			if _, err := event.Parse(expr); err != nil {
				return kernel.Decision{}, fmt.Errorf("agent: wait_event 不合法: %w", err)
			}
			return kernel.Decision{
				Kind:   kernel.DecideWait,
				Wake:   &core.WakeCondition{Kind: core.WakeEvent, Expr: expr, At: &at},
				Reason: md.Reason,
			}, nil
		}
		return kernel.Decision{
			Kind:   kernel.DecideWait,
			Wake:   &core.WakeCondition{Kind: core.WakeTime, At: &at},
			Reason: md.Reason,
		}, nil
	case "tool":
		if md.Tool == "" {
			return kernel.Decision{}, fmt.Errorf("agent: action=tool 但未指定工具")
		}
		info, err := p.lookup(md.Tool)
		if err != nil {
			return kernel.Decision{}, fmt.Errorf("agent: 模型选了不存在的工具 %q: %w", md.Tool, err)
		}
		params := md.Params
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		return kernel.Decision{
			Kind:     kernel.DecideStep,
			Type:     core.StepTool,
			ToolName: info.Name,
			// 副作用等级来自用户授权时的声明，不采信模型自报。
			SideEffect: info.SideEffect,
			Input:      params,
			Reason:     md.Reason,
		}, nil
	default:
		return kernel.Decision{}, fmt.Errorf("agent: 未知的 action %q", md.Action)
	}
}

// contextSegments 装配完整上下文：任务目标 + 分环记忆 + 压缩历史 + 最近步骤。
func (p *Planner) contextSegments(ctx context.Context, t *core.Task, history []*core.Step) ([]llm.Segment, error) {
	segs := p.segments(t, history)
	if p.memory == nil {
		return segs, nil
	}
	asm, err := p.memory.Assemble(ctx, memorySubject(t), sceneOf(t), memory.Budget{
		Total: p.opts.MemoryChars,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: 装配记忆上下文失败: %w", err)
	}
	// 记忆放在任务定义之后、历史之前：先知道"用户是谁、在乎什么"，
	// 再看"这个任务做到哪一步了"。
	out := make([]llm.Segment, 0, len(segs)+4)
	out = append(out, segs[0])
	out = append(out, memorySegments(asm)...)
	out = append(out, segs[1:]...)
	return out, nil
}

// memorySubject 决定检索哪个主体的记忆。
func memorySubject(*core.Task) string { return "user" }

// segments 装配上下文：任务目标 + 压缩的历史 + 最近步骤原文。
func (p *Planner) segments(t *core.Task, history []*core.Step) []llm.Segment {
	segs := []llm.Segment{{
		Role: llm.RoleUser,
		Content: fmt.Sprintf("任务: %s\n目标: %s\n域: %s\n已花费: %d 分",
			t.Title, t.Goal, t.Domain, t.Usage.CostCents),
		// 任务目标的信任等级等于创建它的来源：截图创建的任务，其目标同样不可信。
		Trust:  t.Origin,
		Source: "任务定义",
		// 目标的信任等级可以很低，但它不能被预算裁掉——没有目标的上下文毫无意义。
		Pinned: true,
	}}

	if len(history) == 0 {
		return segs
	}

	cut := len(history) - p.opts.RecentSteps
	if cut < 0 {
		cut = 0
	}
	if cut > 0 {
		segs = append(segs, llm.Segment{
			Role:    llm.RoleUser,
			Content: "较早步骤摘要：\n" + summarize(history[:cut]),
			Trust:   core.TrustSystem,
			Source:  "步骤日志摘要",
		})
	}
	for _, s := range history[cut:] {
		segs = append(segs, llm.Segment{
			Role:    llm.RoleUser,
			Content: describeStep(s),
			// 每一步的输出带着自己的信任等级进上下文——
			// 工具返回的文本在这里依然是 untrusted。
			Trust:  s.OutputTrust,
			Source: fmt.Sprintf("步骤 %d 输出", s.Seq),
		})
	}
	return segs
}

func (p *Planner) toolList() string {
	// 显式复制：直接 append 到 Catalog() 的返回值上会依赖它的容量，
	// 实现一改就可能写坏调用方的切片。
	catalog := p.catalog.Catalog()
	tools := make([]mcpgw.ToolInfo, 0, len(catalog)+1)
	tools = append(tools, catalog...)
	tools = append(tools, builtinTools(p.memory != nil)...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	if len(tools) == 0 {
		return "（当前没有可用工具，只能输出 finish 或 fail）"
	}
	var sb strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&sb, "- %s: %s\n", t.Name, t.Description)
		if len(t.InputSchema) > 0 {
			fmt.Fprintf(&sb, "  入参: %s\n", truncate(string(t.InputSchema), 400))
		}
	}
	return sb.String()
}

func summarize(steps []*core.Step) string {
	var sb strings.Builder
	for _, s := range steps {
		fmt.Fprintf(&sb, "- 步骤 %d %s %s → %s\n",
			s.Seq, s.Type, s.ToolName, stepOutcome(s))
	}
	return sb.String()
}

func describeStep(s *core.Step) string {
	return fmt.Sprintf("步骤 %d（%s %s）%s\n输出: %s",
		s.Seq, s.Type, s.ToolName, stepOutcome(s), truncate(string(s.Output), 2000))
}

func stepOutcome(s *core.Step) string {
	switch s.Status {
	case core.StepFailed:
		return "失败: " + s.Err
	case core.StepSkipped:
		// 被用户拒绝的动作要把理由带给模型，否则它会原样再提一次。
		return "未执行: " + s.Err
	}
	return string(s.Status)
}

// stripFence 去掉模型有时会加上的 ```json 代码围栏。
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
