package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/vault"
)

// ToolCatalog 提供可用工具列表。
type ToolCatalog interface {
	Catalog() []mcpgw.ToolInfo
	Lookup(name string) (mcpgw.ToolInfo, error)
}

const plannerSystemPrompt = `你是原子助手的任务规划器。你的职责是决定任务的下一步做什么。

必须输出一个 JSON 对象，不要输出任何其他内容：
{
  "action": "tool" | "ask" | "wait" | "finish" | "fail",
  "tool": "工具全名，action=tool 时必填",
  "params": {},
  "question": "action=ask 时必填，要问用户的一个问题",
  "wait_seconds": 数字，action=wait 时必填,
  "wait_event": "可选。事件订阅表达式，action=wait 时填写表示等这个事件",
  "reason": "一句话说明理由，会展示给用户；action=finish 时写完整的结论或建议"
}

向用户提问（ask）：
- 只在缺少的信息会改变结论、且记忆和历史里都找不到时才问。能从记忆里知道的不要再问。
- 一次只问一个问题，问得具体。用户的回答会作为一条步骤输出出现在历史里。

等待有两种：
- 只填 wait_seconds：等这么多秒后继续。
- 同时填 wait_event：等到事件发生就继续，wait_seconds 是等不到时的超时。
  事件发生时它的内容会作为一条步骤输出出现在历史里；历史里没有它，说明是超时醒来的。
  表达式语法：字段只有 topic、source、payload.字段路径；
  运算符 == != > >= < <= CONTAINS，用 AND / OR / NOT 和括号组合；字符串用双引号。
  例：topic == "calendar.event.created" AND payload.attendees CONTAINS "客户张总"

给建议之前要综合研判，不要只看单一因素：
- 先想清楚这个决定受哪些因素影响，再逐个取证，最后综合给出结论。
  以出行为例：从哪出发、什么时候有空、交通（机票/火车票的价格与耗时）、
  当地天气、是否节假日与调休、人流量、预算、用户自己的出行习惯。
- 结论要具体到可以执行：什么时候去、去哪些地方、怎么去，以及为什么这样选。
- 每个因素的依据必须来自工具结果、记忆或用户的话。某个因素没有工具能查，
  就在结论里明说"这一项没有数据"，并说明它可能怎样影响结论——绝不编造票价、天气或人流。
- 上下文里如果给了适用的标准流程（SOP），按它的步骤做；跳过哪一步要说明原因。

对外只披露必要的信息：
- 工具参数里只放这次调用必需的信息。记忆里的住址、健康、家人、财务、联系人等个人信息，
  与这次调用无关的一律不带。
- 可给可不给的信息先权衡：给了能明显改善结果才给，并且只给到够用的粒度
  （查天气给城市就够了，不要给详细住址）。
- 拿不准该不该给，就不给，或者先问用户。
- 读取也一样克制：只调用与任务直接相关的工具。不要为了找线索去翻用户的邮件、
  消息、相册这类私人数据——缺信息就问用户。

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
	// ReplyStyle 是回复风格：concise / balanced / detailed，空值按 concise。
	ReplyStyle string
	// ReplyCustom 是用户自己写的额外风格要求。
	ReplyCustom string
}

// styleRule 返回写进系统提示的回复风格要求。
//
// 默认简洁：多数任务用户要的是结论，不是一篇文章。
func (o PlannerOptions) styleRule() string {
	var rule string
	switch o.ReplyStyle {
	case "detailed":
		rule = "回复风格：详细。结论放在最前面，然后展开依据、备选方案和注意事项。"
	case "balanced":
		rule = "回复风格：适中。先给结论，再用几句话交代关键依据；不要铺陈背景。"
	default:
		rule = "回复风格：简洁。直接给结论和必要的依据，能一句话说清就不用两句；" +
			"不要客套、不要复述问题、不要罗列用户没问的东西。" +
			"只有任务本身需要完整方案（如行程规划）时才展开，展开也只写用得上的。"
	}
	if c := strings.TrimSpace(o.ReplyCustom); c != "" {
		rule += "\n用户对回复的额外要求：" + c
	}
	return rule + "\n这条要求约束你写给用户看的 reason 和 question。"
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

// VaultLister 列出保险库条目的公开信息，由 *vault.Vault 提供。
type VaultLister interface {
	List(ctx context.Context) ([]vault.Meta, error)
}

// Planner 用大模型决定任务的下一步。
type Planner struct {
	model   Completer
	catalog ToolCatalog
	memory  MemoryProvider
	vault   VaultLister
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

// WithVault 接入保险库。接入后模型能看到条目的名字与用途，并用句柄引用它们；
// 它看不到、也没有任何办法看到条目的值。
func (p *Planner) WithVault(v VaultLister) *Planner {
	p.vault = v
	return p
}

// vaultSegment 把保险库条目的名字列给模型。
func (p *Planner) vaultSegment(ctx context.Context) (llm.Segment, bool, error) {
	if p.vault == nil {
		return llm.Segment{}, false, nil
	}
	metas, err := p.vault.List(ctx)
	if err != nil {
		return llm.Segment{}, false, fmt.Errorf("agent: 读取保险库目录失败: %w", err)
	}
	if len(metas) == 0 {
		return llm.Segment{}, false, nil
	}
	var sb strings.Builder
	sb.WriteString("保险库里存着下面这些敏感信息。你只能看到名字，看不到内容，也不要向用户索要内容。\n" +
		"工具参数需要其中某一项时，把那个参数的值**整个**写成对应的句柄，系统会在调用的瞬间替换成真实内容；\n" +
		"句柄只能用于它后面列出的工具，用在别的工具上调用会被拒绝。不要把句柄写进长文本里，那样不会被替换。\n")
	for _, m := range metas {
		fmt.Fprintf(&sb, "- %s%s", vault.HandlePrefix, m.Label)
		if m.Note != "" {
			fmt.Fprintf(&sb, "（%s）", m.Note)
		}
		if len(m.AllowedTools) == 0 {
			sb.WriteString(" 可用工具：无")
		} else {
			fmt.Fprintf(&sb, " 可用工具：%s", strings.Join(m.AllowedTools, "、"))
		}
		sb.WriteString("\n")
	}
	// 这份目录是系统自己生成的：名字与用途由用户亲手填写，其中没有任何外部内容。
	return llm.Segment{
		Role: llm.RoleUser, Content: sb.String(),
		Trust: core.TrustSystem, Source: "保险库目录", Pinned: true,
	}, true, nil
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
	Question    string          `json:"question"`
	Reason      string          `json:"reason"`
}

// Decide 询问模型下一步做什么，并把回答校验成内核能执行的决策。
func (p *Planner) Decide(ctx context.Context, t *core.Task, history []*core.Step) (kernel.Decision, error) {
	segs, err := p.contextSegments(ctx, t, history)
	if err != nil {
		return kernel.Decision{}, err
	}
	msgs, err := llm.Build(segs, llm.BuildOptions{
		// 没有当前时间，"下周三""国庆前后"这类说法模型无从换算。
		SystemPrompt: plannerSystemPrompt +
			"\n\n" + p.opts.styleRule() +
			"\n\n当前时间：" + p.now().Local().Format("2006-01-02 15:04 Monday") +
			"\n\n可用工具：\n" + p.toolList(),
		MaxChars: p.opts.MaxChars,
	})
	if err != nil {
		return kernel.Decision{}, err
	}

	d, firstErr := p.ask(ctx, t, msgs)
	if firstErr == nil || ctx.Err() != nil || !errors.Is(firstErr, errBadOutput) {
		return d, firstErr
	}
	// 模型答非所问（空输出、不是对象、选了不存在的工具）时当场再问一次，把错在哪告诉它。
	// 这类错多半一提醒就改；交给内核的话要先退避十秒，还白占一次失败额度。
	retry := append(append([]llm.Message{}, msgs...), llm.Message{
		Role: llm.RoleUser,
		Content: "你上一次的输出无法使用：" + firstErr.Error() +
			"\n请重新决定，只输出一个符合格式的 JSON 对象。",
	})
	d2, err := p.ask(ctx, t, retry)
	// 两次调用都花了钱，都要记账。
	d2.TokensIn += d.TokensIn
	d2.TokensOut += d.TokensOut
	d2.CostMicros += d.CostMicros
	return d2, err
}

// errBadOutput 标记"模型给了回答，但回答不能用"。上游不可达之类的错误不属于这一类。
var errBadOutput = errors.New("agent: 模型输出不可用")

// ask 调一次模型并把回答校验成决策。
//
// 回答不可用时返回的 Decision 只带用量，供调用方记账。
func (p *Planner) ask(ctx context.Context, t *core.Task, msgs []llm.Message) (kernel.Decision, error) {
	resp, err := p.model.Complete(ctx, llm.PurposePlan, llm.Request{
		Messages: msgs,
		JSONMode: true,
	})
	if err != nil {
		return kernel.Decision{}, err
	}
	spent := kernel.Decision{
		TokensIn:   resp.Usage.PromptTokens,
		TokensOut:  resp.Usage.CompletionTokens,
		CostMicros: resp.CostMicros,
	}

	// 只取第一个 JSON 值：模型偶尔会在对象后面再跟一段解释，
	// 为这种尾巴整次重试不值得。
	var md modelDecision
	if err := json.NewDecoder(strings.NewReader(stripFence(resp.Content))).Decode(&md); err != nil {
		return spent, fmt.Errorf("%w: 不是合法 JSON: %v (原文: %s)",
			errBadOutput, err, truncate(strings.TrimSpace(resp.Content), 200))
	}
	d, err := p.validate(t, md)
	if err != nil {
		return spent, fmt.Errorf("%w: %v", errBadOutput, err)
	}
	// 规划本身的用量交给内核记账，否则任务预算管不到最贵的那部分开销。
	d.TokensIn, d.TokensOut, d.CostMicros = spent.TokensIn, spent.TokensOut, spent.CostMicros
	return d, nil
}

// validate 把模型的回答校验成决策。
//
// 这里是"模型不可信"原则的落点：工具必须在目录里，副作用等级取自目录而不是模型自报，
// 等待时长有上限。模型能影响做什么，但不能影响能做什么。
func (p *Planner) validate(t *core.Task, md modelDecision) (kernel.Decision, error) {
	switch md.Action {
	case "ask":
		q := strings.TrimSpace(md.Question)
		if q == "" {
			return kernel.Decision{}, fmt.Errorf("agent: action=ask 但没有问题")
		}
		record, err := json.Marshal(map[string]string{"asked_user": q})
		if err != nil {
			return kernel.Decision{}, fmt.Errorf("agent: 序列化提问失败: %w", err)
		}
		at := p.now().Add(p.opts.MaxWait)
		return kernel.Decision{
			Kind: kernel.DecideWait,
			// 只等发给这个任务的用户消息。任务 ID 是我们自己生成的，拼进表达式是安全的。
			Wake: &core.WakeCondition{
				Kind: core.WakeEvent, At: &at,
				Expr: fmt.Sprintf(`topic == %q AND payload.task_id == %q`, event.TopicUserMessage, t.ID),
			},
			// 问题要留在日志里：醒来之后模型得知道用户的回答是在答什么。
			Input:  record,
			Reason: "等待你的回复：" + q,
		}, nil
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
	if seg, ok, err := p.vaultSegment(ctx); err != nil {
		return nil, err
	} else if ok {
		// 紧跟在任务定义之后。
		segs = append(segs[:1], append([]llm.Segment{seg}, segs[1:]...)...)
	}
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
//
// 空值表示不限主体：一个实例只服务一个用户，关于他本人的记忆和关于
// 他身边的人（客户、家人）的记忆都属于他，都该参与检索。
func memorySubject(*core.Task) string { return "" }

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

	// 最新的计划清单钉在最前面：它记录着这个任务已经拿到什么、还差什么，
	// 不能随着步骤增多滑出窗口。
	if plan := latestPlan(history); plan != "" {
		segs = append(segs, llm.Segment{
			Role:    llm.RoleUser,
			Content: "当前计划清单（你自己维护的，用 " + ToolPlan + " 更新）：\n" + plan,
			Trust:   core.TrustToolOutput,
			Source:  "计划清单",
			Pinned:  true,
		})
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
		fmt.Fprintf(&sb, "- 步骤 %d %s %s %s → %s\n",
			s.Seq, s.Type, s.ToolName, truncate(string(s.Input), 200), stepOutcome(s))
	}
	return sb.String()
}

func describeStep(s *core.Step) string {
	// 参数必须带上：只看到"调过 book_ride 且成功"而看不到订的是什么，
	// 模型无从判断这一步是否已经满足目标，会把同一件事再做一遍。
	return fmt.Sprintf("步骤 %d（%s %s）%s\n参数: %s\n输出: %s",
		s.Seq, s.Type, s.ToolName, stepOutcome(s),
		truncate(string(s.Input), 1000), truncate(string(s.Output), 2000))
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
	// 退到字符边界再切，否则中文会被切出半个字符。
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}
