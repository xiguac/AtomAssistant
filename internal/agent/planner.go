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
	"github.com/xiguac/atomhelper/internal/files"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/skill"
	"github.com/xiguac/atomhelper/internal/vault"
)

// ToolCatalog 提供可用工具列表。
type ToolCatalog interface {
	Catalog() []mcpgw.ToolInfo
	Lookup(name string) (mcpgw.ToolInfo, error)
}

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
		rule = "Reply style: detailed. Conclusion first, then the evidence, the alternatives and the caveats."
	case "balanced":
		rule = "Reply style: balanced. Conclusion first, then a few sentences on the key evidence; no background padding."
	default:
		rule = "Reply style: concise. Give the conclusion and the necessary evidence directly; one sentence where one will do. " +
			"No pleasantries, no restating the question, nothing the user did not ask about. " +
			"Length: ordinary matters in at most three sentences; matters that need a full plan (such as an itinerary) in at most 300 Chinese characters, " +
			"containing only the conclusion, the key evidence and what could not be found. No item-by-item account of the lookups, no itinerary details the user did not ask for."
	}
	if c := strings.TrimSpace(o.ReplyCustom); c != "" {
		rule += "\nThe user's additional requirements for replies: " + c
	}
	return rule + "\nThis requirement governs the reason and question you write for the user."
}

func (o *PlannerOptions) applyDefaults() {
	if o.RecentSteps <= 0 {
		o.RecentSteps = 6
	}
	if o.MaxChars <= 0 {
		o.MaxChars = 36000
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
	skills  SkillProvider
	voice   VoiceProvider
	// prompts 非空时，基础约束与规划说明以用户在后台改写的为准。
	prompts PromptSource
	// images 非空时，用户发给任务的图片随上下文一起交给模型看。
	images ImageSource
	// pay 为真表示接入了付款能力，付款工具对模型可见。
	pay bool
	// paidResources 为真表示允许请求付费资源。
	paidResources bool
	// 已接入的内建连接器。canSend 为真表示邮件连接器配了发信服务器。
	hasCalendar, hasMail, canSend bool
	// hasHolidays 为真表示接入了节假日数据。
	hasHolidays bool
	// conversation 非空时，属于会话的任务看得到同一个会话里最近的对话，见 conversation.go。
	conversation ConversationStore
	// canRequestSecrets 为真表示助手可以在后台登记待用户填写的环境变量。
	canRequestSecrets bool
	// hasSandbox 为真表示启用了沙箱。
	hasSandbox bool
	// hasWeather 为真表示接入了天气数据。
	hasWeather bool
	// hasFiles 为真表示启用了文件处理。
	hasFiles bool
	// hasTeam 为真表示配置了协作模型，可以派活。
	hasTeam bool
	// team 与 workers 非空时，事情交给任务模型去办，主模型负责指派、评估方案和审核，见 delegate.go。
	team    TaskAssigner
	workers WorkerModel
	// worker 是当前这次决策由哪个任务模型来做。它只存在于 decideDelegated 内部的副本上，
	// 共享的 Planner 上永远是空的。
	worker string
	// reviewActions 为真时，会修改、删除已有东西的动作在执行前先由主模型把关，见 guard。
	reviewActions bool
	// canSendFiles 为真表示接了消息渠道，可以把文件发到聊天窗口里。
	canSendFiles bool
	// hasWeb 为真表示接入了联网搜索。
	hasWeb bool
	// library 非空表示助手可以自己查找、安装能力，并能读已安装的 Skill。
	library SkillLibrary
	// scope 是当前这次决策所绑定的 Skill。它只存在于 Decide 内部的副本上，
	// 共享的 Planner 上永远是 nil。
	scope *skill.Skill
	opts  PlannerOptions
	now   func() time.Time
	// contexts 是各个任务冻结的背景与固定的围栏标记，见 taskctx.go。指针：Planner 会按值复制。
	contexts *taskContexts
}

var _ kernel.Planner = (*Planner)(nil)

// NewPlanner 构造规划器。
func NewPlanner(model Completer, catalog ToolCatalog, opts PlannerOptions) *Planner {
	opts.applyDefaults()
	return &Planner{
		model:    model,
		catalog:  catalog,
		opts:     opts,
		now:      func() time.Time { return time.Now().UTC() },
		contexts: newTaskContexts(),
	}
}

// WithMemory 接入记忆系统。接入后上下文会带上分环的记忆，
// 并向模型开放 memory.expand 用于按需展开索引行。
func (p *Planner) WithMemory(m MemoryProvider) *Planner {
	p.memory = m
	return p
}

// WithVoice 接入表达风格。接入后助手替用户写东西、对用户说话时会向他本人的说话方式靠拢。
func (p *Planner) WithVoice(v VoiceProvider) *Planner {
	p.voice = v
	return p
}

// SkillProvider 按 ID 取出已安装的 Skill，由 skill.Store 提供。
type SkillProvider interface {
	GetSkill(ctx context.Context, id string) (*skill.Skill, error)
}

// WithSkills 接入 Skill。带有 skill:<id> 标签的任务会按对应的 Skill 执行，
// 可用工具收窄为"Skill 声明过的 ∩ 用户已授权的"。
func (p *Planner) WithSkills(s SkillProvider) *Planner {
	p.skills = s
	return p
}

// scoped 返回一个绑定了任务所指定 Skill 的 Planner 副本；任务没有指定 Skill 时返回自身。
func (p *Planner) scoped(ctx context.Context, t *core.Task) (*Planner, error) {
	id := skill.BoundTo(t.Tags)
	if id == "" {
		return p, nil
	}
	// 指定了 Skill 却加载不出来，绝不能退化成"不受限地继续跑"——
	// 那等于把收窄工具集这道防线悄悄拿掉。
	if p.skills == nil {
		return nil, fmt.Errorf("agent: 任务指定了 Skill %s，但未启用 Skill", id)
	}
	sk, err := p.skills.GetSkill(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("agent: 加载任务指定的 Skill %s 失败: %w", id, err)
	}
	cp := *p
	cp.scope = sk
	return &cp, nil
}

// permits 报告当前这次决策能否使用某个 MCP 工具。
func (p *Planner) permits(tool string) bool {
	return p.scope == nil || p.scope.Allows(tool)
}

// WithPay 向模型开放付款工具。实际执行由 Actuator 上的 Payer 完成。
func (p *Planner) WithPay(enabled bool) *Planner {
	p.pay = enabled
	return p
}

// WithPaidResources 向模型开放付费资源工具。
func (p *Planner) WithPaidResources(enabled bool) *Planner {
	p.paidResources = enabled
	return p
}

// WithCalendar 向模型开放日历工具。实际执行由 Actuator 上的 CalendarService 完成。
func (p *Planner) WithCalendar(enabled bool) *Planner {
	p.hasCalendar = enabled
	return p
}

// WithFiles 向模型开放文件工具。
func (p *Planner) WithFiles(enabled bool) *Planner {
	p.hasFiles = enabled
	return p
}

// WithChatFiles 向模型开放"把文件发到聊天窗口"。要同时启用了文件处理才有意义。
func (p *Planner) WithChatFiles(enabled bool) *Planner {
	p.canSendFiles = enabled
	return p
}

// WithHolidays 向模型开放节假日查询。
func (p *Planner) WithHolidays(enabled bool) *Planner {
	p.hasHolidays = enabled
	return p
}

// WithSecretRequests 向模型开放"在后台登记待填写的环境变量"。
func (p *Planner) WithSecretRequests(enabled bool) *Planner {
	p.canRequestSecrets = enabled
	return p
}

// WithSandbox 向模型开放沙箱。
func (p *Planner) WithSandbox(enabled bool) *Planner {
	p.hasSandbox = enabled
	return p
}

// WithWeather 向模型开放天气查询。
func (p *Planner) WithWeather(enabled bool) *Planner {
	p.hasWeather = enabled
	return p
}

// WithMail 向模型开放邮件工具。canSend 为假时只能读和整理，不能发信。
func (p *Planner) WithMail(enabled, canSend bool) *Planner {
	p.hasMail, p.canSend = enabled, canSend
	return p
}

// builtins 返回当前这次决策可用的内置工具。
//
// 清单和记忆展开只读写任务自己的东西，任何任务都能用。付款不一样：
// 任务绑定了 Skill 时，只有 Skill 明确声明了付款工具才给——
// 否则一个别人写的流程，装上之后就凭空有了花钱的能力。
// 日历和邮件同理：它们读的是用户最私人的数据，流程没声明就碰不到。
func (p *Planner) builtins() []mcpgw.ToolInfo {
	tools := builtinTools(p.memory != nil)
	// 节假日是公开信息，不涉及用户的任何数据，绑定了 Skill 的任务也能查。
	if p.hasHolidays {
		tools = append(tools, holidayTool)
	}
	// 天气同理：带出去的只有一个地名。
	if p.hasWeather {
		tools = append(tools, weatherTool)
	}
	gated := append(payTools(p.pay), resourceTools(p.pay && p.paidResources)...)
	gated = append(gated, connectorTools(p.hasCalendar, p.hasMail, p.canSend)...)
	if p.hasFiles {
		gated = append(gated, fileToolset...)
		if p.canSendFiles {
			gated = append(gated, fileSendTool)
		}
	}
	// 派活会把材料交给另一家模型服务，别人写的流程没声明就不能用。
	if p.hasTeam {
		gated = append(gated, dispatchTool())
	}
	// 联网搜索会把问题交给模型服务去网上查，别人写的流程没声明就不能用。
	if p.hasWeb {
		gated = append(gated, webSearchTool)
	}
	// 别人写的流程不能自己再去装东西、读别的流程。
	if p.library != nil {
		gated = append(gated, capabilityToolset...)
	}
	// 在沙箱里跑代码能读整个工作区，别人写的流程没声明就不能用。
	if p.hasSandbox {
		gated = append(gated, sandboxTool)
	}
	// 别人写的流程不能让后台多出一个向用户要密钥的输入框。
	if p.canRequestSecrets {
		gated = append(gated, vaultRequestTool)
	}
	for _, t := range gated {
		if p.permits(t.Name) {
			tools = append(tools, t)
		}
	}
	return tools
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
	// 服务凭据（模型密钥、渠道凭据、MCP 密钥）是系统自己连接时用的，模型用不上，不列给它。
	usable := metas[:0:0]
	for _, m := range metas {
		if !m.Service() {
			usable = append(usable, m)
		}
	}
	if metas = usable; len(metas) == 0 {
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
	for _, t := range p.builtins() {
		if t.Name == name {
			return t, nil
		}
	}
	// 这是"工具集取交集"的落点。目录里有的是用户已授权的工具，
	// 任务绑定了 Skill 时再和 Skill 声明的名单取交集。
	// 内置工具（清单、展开记忆）只读写任务自己的东西，不在收窄范围内。
	if !p.permits(name) {
		return mcpgw.ToolInfo{}, fmt.Errorf("agent: 这个任务按流程「%s」执行，它没有声明工具 %s",
			p.scope.Name, name)
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
	More        []modelCall     `json:"more"`
	Question    string          `json:"question"`
	Reason      string          `json:"reason"`
}

// modelCall 是 more 里的一个附带查询。
type modelCall struct {
	Tool   string          `json:"tool"`
	Params json.RawMessage `json:"params"`
}

// toolDecision 把一次工具调用校验成决策。副作用等级取自目录，不采信模型自报。
func (p *Planner) toolDecision(name string, params json.RawMessage, reason string) (kernel.Decision, error) {
	if name == "" {
		return kernel.Decision{}, fmt.Errorf("agent: 没有指定工具")
	}
	info, err := p.lookup(name)
	if err != nil {
		return kernel.Decision{}, fmt.Errorf("agent: 模型选了不存在的工具 %q: %w", name, err)
	}
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
		Reason:     reason,
	}, nil
}

// Decide 询问模型下一步做什么，并把回答校验成内核能执行的决策。
func (p *Planner) Decide(ctx context.Context, t *core.Task, history []*core.Step) (kernel.Decision, error) {
	p, err := p.scoped(ctx, t)
	if err != nil {
		return kernel.Decision{}, err
	}
	if p.delegating() {
		return p.decideDelegated(ctx, t, history)
	}
	return p.decide(ctx, t, history)
}

// decide 让当前负责决策的模型（主模型，或者 p.worker 指定的任务模型）决定下一步。
// 它选的动作如果会修改、删除已有的东西，先交主模型把关。
func (p *Planner) decide(ctx context.Context, t *core.Task, history []*core.Step) (kernel.Decision, error) {
	d, err := p.propose(ctx, t, history)
	if err != nil || !p.reviewActions || !needsMainReview(d) {
		return d, err
	}
	return p.guard(ctx, t, history, d)
}

// propose 问一次模型下一步做什么，并把回答校验成决策。
func (p *Planner) propose(ctx context.Context, t *core.Task, history []*core.Step) (kernel.Decision, error) {
	segs, err := p.contextSegments(ctx, t, history)
	if err != nil {
		return kernel.Decision{}, err
	}
	ps, err := p.loadPrompts(ctx)
	if err != nil {
		return kernel.Decision{}, err
	}
	style := p.opts.styleRule()
	// 用户的表达风格只给主模型：任务模型的结论不直接给用户，给用户的答复由主模型在审核时写。
	if p.voice != nil && p.worker == "" {
		v, err := p.voice.Voice(ctx)
		if err != nil {
			return kernel.Decision{}, fmt.Errorf("agent: 读取表达风格失败: %w", err)
		}
		// 还什么都没学到、或者用户停用了这一段时不提这回事，免得模型去找一段并不存在的材料。
		if rule := ps.get(PromptVoice); !v.Empty() && rule != "" {
			style += "\n\n" + rule
			// 紧跟在任务定义之后：先知道"替谁说话"，再看任务做到哪一步了。
			segs = append(segs[:1], append([]llm.Segment{voiceSegment(v)}, segs[1:]...)...)
		}
	}
	// 基础约束永远排在最前面，后面才是这一步具体怎么做。
	// 没有当前时间，"下周三""国庆前后"这类说法模型无从换算。
	parts := []string{ps.get(PromptBase), ps.get(PromptPlanner)}
	if p.hasWeb && p.permits(ToolWebSearch) {
		parts = append(parts, ps.get(PromptWeb))
	}
	if p.library != nil && p.scope == nil {
		parts = append(parts, ps.get(PromptCapability))
	}
	if p.worker != "" {
		parts = append(parts, ps.get(PromptWorker))
	}
	// 当前时间不放在这里：它每次都变，放在前面会让后面整段都命中不了缓存。见 taskctx.go。
	system := sections(append(parts, style)...) + "\n\nAvailable tools:\n" + p.toolList()
	msgs, err := p.build(t, system, segs)
	if err != nil {
		return kernel.Decision{}, err
	}
	if msgs, err = p.withImages(ctx, t, msgs); err != nil {
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
	d2.TokensCached += d.TokensCached
	return d2, err
}

// errBadOutput 标记"模型给了回答，但回答不能用"。上游不可达之类的错误不属于这一类。
var errBadOutput = errors.New("agent: 模型输出不可用")

// ask 调一次模型并把回答校验成决策。
//
// 回答不可用时返回的 Decision 只带用量，供调用方记账。
func (p *Planner) ask(ctx context.Context, t *core.Task, msgs []llm.Message) (kernel.Decision, error) {
	resp, err := p.complete(ctx, llm.Request{
		Messages: msgs,
		JSONMode: true,
	})
	if err != nil {
		return kernel.Decision{}, err
	}
	spent := kernel.Decision{
		TokensIn:     resp.Usage.PromptTokens,
		TokensOut:    resp.Usage.CompletionTokens,
		CostMicros:   resp.CostMicros,
		TokensCached: resp.Usage.CachedTokens,
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
	d.TokensIn, d.TokensOut, d.CostMicros, d.TokensCached = spent.TokensIn, spent.TokensOut, spent.CostMicros, spent.TokensCached
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
		d, err := p.toolDecision(md.Tool, md.Params, md.Reason)
		if err != nil {
			return kernel.Decision{}, err
		}
		if len(md.More) > kernel.MaxBatch {
			return kernel.Decision{}, fmt.Errorf("agent: more 最多 %d 项，实际 %d 项", kernel.MaxBatch, len(md.More))
		}
		for _, c := range md.More {
			next, err := p.toolDecision(c.Tool, c.Params, md.Reason)
			if err != nil {
				return kernel.Decision{}, err
			}
			// 这是硬规则，不是建议：写操作放进批量里，模型就能在没看到前一步结果的情况下连着改东西。
			if next.SideEffect != core.SideEffectNone && next.SideEffect != core.SideEffectReadOnly {
				return kernel.Decision{}, fmt.Errorf(
					"agent: more 里只能放查询，%s 会产生副作用（%s），必须单独作为一步", next.ToolName, next.SideEffect)
			}
			d.Then = append(d.Then, next)
		}
		return d, nil
	default:
		return kernel.Decision{}, fmt.Errorf("agent: 未知的 action %q", md.Action)
	}
}

// contextSegments 装配完整上下文：任务目标 + 分环记忆 + 压缩历史 + 最近步骤。
func (p *Planner) contextSegments(ctx context.Context, t *core.Task, history []*core.Step) ([]llm.Segment, error) {
	segs := p.segments(t, history)
	bg, err := p.background(ctx, t)
	if err != nil {
		return nil, err
	}
	// 背景紧跟在任务定义之后，再往后是步骤历史和末尾那些每步都变的东西。
	out := make([]llm.Segment, 0, len(segs)+len(bg))
	out = append(out, segs[0])
	out = append(out, bg...)
	return append(out, segs[1:]...), nil
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
		// 已花费每一步都在变，不放在这里，放在末尾（见 Planner.tail）。
		Content: fmt.Sprintf("任务: %s\n目标: %s\n域: %s", t.Title, t.Goal, t.Domain),
		// 任务目标的信任等级等于创建它的来源：截图创建的任务，其目标同样不可信。
		Trust:  t.Origin,
		Source: "任务定义",
		// 目标的信任等级可以很低，但它不能被预算裁掉——没有目标的上下文毫无意义。
		Pinned: true,
	}}

	if len(history) == 0 {
		return segs
	}

	// 更早的步骤压成摘要。分界每攒满 compactEvery 步才挪一次：两次之间摘要和它后面的原文都不变，
	// 同一个任务的连续几步发出去的内容开头一致，能命中模型服务的前缀缓存。
	cut := len(history) - p.opts.RecentSteps
	if cut < 0 {
		cut = 0
	}
	cut = cut / compactEvery * compactEvery
	if cut > 0 {
		segs = append(segs, llm.Segment{
			Role:    llm.RoleUser,
			Content: "较早步骤摘要：\n" + summarize(history[:cut]),
			Trust:   core.TrustSystem,
			Source:  "步骤日志摘要",
		})
	}
	recent := history[cut:]
	for i, s := range recent {
		segs = append(segs, llm.Segment{
			Role:    llm.RoleUser,
			Content: describeStep(s),
			// 每一步的输出带着自己的信任等级进上下文——
			// 工具返回的文本在这里依然是 untrusted。
			Trust:  s.OutputTrust,
			Source: fmt.Sprintf("步骤 %d 输出", s.Seq),
			// 最近几步无论预算多紧都留着：看不到刚才那一步的结果，下一步就无从决定。
			// 钉住只是不裁剪，该进围栏的照样在围栏里。
			Pinned: i >= len(recent)-pinnedSteps,
		})
	}

	// 下面这些每一步都可能变，放在历史后面。

	// 最新的计划清单：它记录着这个任务已经拿到什么、还差什么，不能随着步骤增多滑出窗口。
	if plan := latestPlan(history); plan != "" {
		segs = append(segs, llm.Segment{
			Role:    llm.RoleUser,
			Content: "当前计划清单（你自己维护的，用 " + ToolPlan + " 更新）：\n" + plan,
			Trust:   core.TrustToolOutput,
			Source:  "计划清单",
			Pinned:  true,
		})
	}

	// 主模型的预处理和最近一次审核意见同样钉着。
	segs = append(segs, delegateSegments(history)...)

	// 搜索结果是外部内容，上下文一紧最先被裁掉的就是它——哪怕它就在最近几步里。
	// 模型看不到查到了什么，就会把同一件事再查一遍，每查一次都是两三万 token。
	// 所以除了钉住的最近几步，之前每次搜索的结论都留一份钉住的短摘要。
	if older := len(history) - pinnedSteps; older > 0 {
		if found := earlierFindings(history[:older]); found != "" {
			segs = append(segs, llm.Segment{
				Role:    llm.RoleUser,
				Content: "较早的联网搜索已经查到下面这些，不要再查同样的事：\n" + found,
				// 网上的内容，照旧在围栏里。
				Trust:  core.TrustExternal,
				Source: "较早的联网搜索结果",
				Pinned: true,
			})
		}
	}
	return segs
}

// pinnedSteps 是不参与预算裁剪的最近步骤数。
const pinnedSteps = 3

func (p *Planner) toolList() string {
	// 显式复制：直接 append 到 Catalog() 的返回值上会依赖它的容量，
	// 实现一改就可能写坏调用方的切片。
	catalog := p.catalog.Catalog()
	tools := make([]mcpgw.ToolInfo, 0, len(catalog)+1)
	for _, t := range catalog {
		// 绑定了 Skill 的任务，模型根本看不到名单之外的工具。
		if p.permits(t.Name) {
			tools = append(tools, t)
		}
	}
	tools = append(tools, p.builtins()...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	if len(tools) == 0 {
		return "（当前没有可用工具，只能输出 finish 或 fail）"
	}
	var sb strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&sb, "- %s: %s\n", t.Name, t.Description)
		if len(t.InputSchema) > 0 {
			fmt.Fprintf(&sb, "  入参: %s\n", truncate(string(t.InputSchema), 1500))
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
		truncate(string(s.Input), 1000), truncate(string(s.Output), outputChars(s)))
}

// outputChars 是一步的输出进上下文时保留的长度。
func outputChars(s *core.Step) int {
	if s.ToolName == ToolDispatch {
		return dispatchOutputChars
	}
	// 搜索结果就是这一步的全部价值，截得太短等于白查；但也不能太长，
	// 几步搜索就把上下文占满的话，别的内容会被挤出去。
	if s.ToolName == ToolWebSearch {
		return 4000
	}
	// 读文件一次返回一段，后面跟着"下一段从哪读"。截断了模型就看不到这一段的结尾和续读位置，
	// 只会在同一个文件上来回读。
	if s.ToolName == ToolFileRead || s.ToolName == ToolSheetRead {
		return 2*files.ChunkBytes + 1000
	}
	// 一段 Skill 正文被截断的话，模型拿到的就是半篇说明。
	if s.ToolName == ToolSkillRead {
		return 4 * skillReadChars
	}
	return 2000
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
