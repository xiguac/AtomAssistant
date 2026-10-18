// Package agent 把内核、LLM 路由、MCP 网关和安全策略接在一起。
//
// 它是唯一知道这四者同时存在的地方；下层互相不感知。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/policy"
)

// UsageTracker 提供审批策略所需的累计用量。
//
// 累计帽子是防拆单的关键：只设单次阈值时，100 元以下自动放行可以被拆成 50 次。
type UsageTracker interface {
	Usage(ctx context.Context, taskID string, day time.Time) (policy.Usage, error)
	RecordAuto(ctx context.Context, taskID string, amountCents int64, at time.Time) error
}

// Approver 用 policy.Policy 判定动作是否需要用户确认。
type Approver struct {
	policy     *policy.Policy
	classifier *policy.Classifier
	usage      UsageTracker
	history    StepLister
	risks      InstallRisks
	now        func() time.Time
}

// StepLister 读取任务已经走过的步骤，由任务存储提供。
type StepLister interface {
	ListSteps(ctx context.Context, taskID string) ([]*core.Step, error)
}

// WithHistory 让审批看得到任务走过的步骤，从而判断这次改动是不是被外部内容引出来的。
func (a *Approver) WithHistory(s StepLister) *Approver {
	a.history = s
	return a
}

// InstallRisks 给出一次被搁置的安装的风险点，由 *market.Installer 提供。
type InstallRisks interface {
	PendingRisks(kind, id string) []string
}

// WithInstallRisks 让审批在请用户批准一次有风险的安装时，能把风险点一条条列给他。
func (a *Approver) WithInstallRisks(r InstallRisks) *Approver {
	a.risks = r
	return a
}

// installRisk 判断这个动作是不是"用户批准后才能装"的那种安装，是的话给出展示给用户的理由。
//
// 认的是参数里的 accept_risk：安装器只有在评估出风险、把安装搁置之后，才接受带这个参数的调用。
// 理由里的风险点取自安装器当时的评估，不取模型的转述。
func (a *Approver) installRisk(d kernel.Decision) (string, bool) {
	if d.ToolName != ToolCapabilityInstall {
		return "", false
	}
	var in capabilityInput
	if err := json.Unmarshal(d.Input, &in); err != nil || !in.AcceptRisk {
		return "", false
	}
	what := "Skill"
	if in.Kind == "mcp" {
		what = "MCP server"
	}
	var risks []string
	if a.risks != nil {
		risks = a.risks.PendingRisks(in.Kind, strings.TrimSpace(in.ID))
	}
	if len(risks) == 0 {
		return fmt.Sprintf("要安装的 %s %s 有把本机信息传到外部的风险，需要你批准", what, in.ID), true
	}
	return fmt.Sprintf("要安装的 %s %s 有下面的风险，需要你批准：\n- %s", what, in.ID, strings.Join(risks, "\n- ")), true
}

// CalendarDefaults 给日历的新建和修改加上默认放行：用户没有为它们单独写规则时，直接执行。
//
// 用户说"周五下午三点开会，记一下"，再弹一次确认是多此一举——点确认和自己动手记一样费事。
// 这只对用户亲口交代的事生效：任务读过邮件、网页这类外部内容之后，默认放行不再适用（见 drivenBy）。
// 删除不在其中：删掉的日程找不回来。用户想收紧，在配置里给对应工具写 never: true。
func CalendarDefaults(p *policy.Policy) {
	for _, tool := range []string{ToolCalendarCreate, ToolCalendarUpdate} {
		listed := false
		for _, r := range p.Rules {
			if r.Tool == tool {
				listed = true
			}
		}
		if !listed {
			p.Rules = append(p.Rules, policy.Rule{Tool: tool})
		}
	}
}

// drivenBy 定出这次动作是由什么等级的内容驱动的。
//
// 光看任务是谁创建的不够。用户说"看看邮件里有什么要记到日历上的"，任务是用户建的，
// 但要记什么是邮件说的——这种由助手自己取回来的外部内容引出的改动，应该交给用户定夺。
// 所以任务只要读过外部内容，之后的改动就按外部内容驱动对待，不适用任何自动放行。
//
// 用户亲手交给助手的东西不算：他转发来一段调课通知让助手记下，那是他自己的意思。
func (a *Approver) drivenBy(ctx context.Context, t *core.Task, d kernel.Decision) (core.TrustLevel, string, error) {
	if a.history == nil || !t.Origin.CanTriggerIrreversible() {
		return t.Origin, "", nil
	}
	// 自主模式下用户选择了不再为"任务读过外部内容"逐个确认：长程任务几乎都会查网页、读工具返回的文字，
	// 这条规则会让之后的每一次改动都停下来等人。只看任务是谁发起的——不是用户发起的任务照旧改不了东西。
	if a.policy.Autonomous {
		return t.Origin, "", nil
	}
	// 只读操作本来就不用审批，不必为它翻一遍历史。
	if d.SideEffect == core.SideEffectNone || d.SideEffect == core.SideEffectReadOnly {
		return t.Origin, "", nil
	}
	// 整理邮件（标已读、归档、移进废纸篓）天生就是对着外部内容做的，而且出不了邮箱、随时能撤回。
	// 对它套用这条规则，等于永远不许用户为它配置放行。
	if d.ToolName == ToolMailMark || d.ToolName == ToolMailMove {
		return t.Origin, "", nil
	}
	// 安装必然跟在搜索后面，搜索结果又必然是外部内容；对它套用这条规则，助手就永远不能自己装东西。
	// 安装该不该问用户，看的是装的东西有没有风险，不是看任务读过什么：
	// 低风险的直接装，有外传风险的一律要批准（见 CheckAction 里的 installRisk）。
	if d.ToolName == ToolCapabilityInstall {
		return t.Origin, "", nil
	}
	steps, err := a.history.ListSteps(ctx, t.ID)
	if err != nil {
		return t.Origin, "", err
	}
	for _, s := range steps {
		if s.Status != core.StepDone || s.OutputTrust != core.TrustExternal {
			continue
		}
		// 看自己的日历不算读外部内容：新建日程之前查一下有没有冲突是正常做法，
		// 把它算进去的话每一次新建都要确认。
		if s.ToolName == ToolCalendarEvents || s.ToolName == ToolCalendarList {
			continue
		}
		source := s.ToolName
		if source == "" {
			source = "外部事件"
		}
		return core.TrustExternal, source, nil
	}
	return t.Origin, "", nil
}

// strangerAddress 判断一次"按地址装 MCP server"用的地址是不是用户没有亲口说过的。
//
// 核对的办法是逐字比对：地址必须原样出现在用户说过的话里（任务的目标，或者他后来的回话）。
// 模型把地址改写过、或者任务不是用户发起的，都对不上，交给用户确认。
func (a *Approver) strangerAddress(ctx context.Context, t *core.Task, d kernel.Decision) (string, bool) {
	if d.ToolName != ToolCapabilityInstall {
		return "", false
	}
	var in capabilityInput
	if err := json.Unmarshal(d.Input, &in); err != nil {
		return "", false
	}
	addr := strings.TrimSpace(in.URL)
	if addr == "" {
		return "", false
	}
	const ask = "要按一个地址接入 MCP server，而这个地址不是你亲口给的（或者核对不了），是否接入由你确认"
	var history []*core.Step
	if a.history != nil {
		steps, err := a.history.ListSteps(ctx, t.ID)
		if err != nil {
			return ask, true
		}
		history = steps
	}
	for _, said := range userStatements(t, history) {
		if strings.Contains(said, addr) {
			return "", false
		}
	}
	return ask, true
}

// knownURL 报告 web.fetch 要读的网址是否原样出现在用户说过的话里，或者之前搜索、读网页取回的内容里。
// 核对不了时按不认得处理。
func (a *Approver) knownURL(ctx context.Context, t *core.Task, input json.RawMessage) bool {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return false
	}
	addr := strings.TrimSpace(in.URL)
	if addr == "" || a.history == nil {
		return false
	}
	steps, err := a.history.ListSteps(ctx, t.ID)
	if err != nil {
		return false
	}
	// 步骤的输出是 JSON，网址里的 & 之类会被转义，按转义后的样子也比一次。
	quoted, err := json.Marshal(addr)
	if err != nil {
		return false
	}
	escaped := string(quoted[1 : len(quoted)-1])
	texts := userStatements(t, steps)
	// 只认网上取回的内容。邮件、日历里的链接可能带着用户自己的凭据（退订、确认、免登录链接），
	// 打开一次就算办了一件事。
	for _, s := range steps {
		if s.Status == core.StepDone && (s.ToolName == ToolWebSearch || s.ToolName == ToolWebFetch) {
			texts = append(texts, string(s.Output))
		}
	}
	for _, text := range texts {
		if strings.Contains(text, addr) || strings.Contains(text, escaped) {
			return true
		}
	}
	return false
}

// inHouse 是会改动东西、但改的是用户自己地盘上的东西的内置工具：他的日历、他的邮箱、本机的工作区。
// 它们不把内容交给任何第三方。
var inHouse = map[string]bool{
	ToolCalendarCreate: true, ToolCalendarUpdate: true, ToolCalendarDelete: true,
	ToolMailMark: true, ToolMailMove: true,
	ToolSheetWrite: true, ToolDocWrite: true, ToolSlidesWrite: true,
	ToolCapabilityInstall: true,
	// 只在用户自己的后台里多出一个空的输入框，值由用户填。
	ToolVaultRequest: true,
	// 断网的沙箱，结果只写进本机工作区的一个新目录。
	ToolSandboxRun: true,
}

// outbound 报告一个动作是不是对外发送：发信、付款与付费请求、通过 MCP 工具向外部服务提交信息。
//
// 判断的办法是排除法：只读的不算；规划器自己的记录和上面那几个"自己地盘上"的工具不算；
// 其余会产生副作用的一律算。新接的 MCP 工具因此默认按对外发送对待——宁可多问一次，
// 也不要让一个没人认得的工具悄悄把东西带出去。
func outbound(tool string, side core.SideEffect) bool {
	// 这两个是助手自己拼出网址去发的 HTTP 请求。它们不改动任何东西，所以副作用等级是只读，
	// 但网址和参数是模型写的，能把任何东西带出去——不能当成普通的查询。
	if tool == ToolResourceRequest || tool == ToolResourceFetch {
		return true
	}
	if side == core.SideEffectNone || side == core.SideEffectReadOnly {
		return false
	}
	return !inHouse[tool] && !isInternalTool(tool)
}

var _ kernel.Approver = (*Approver)(nil)

// NewApprover 构造审批器。
func NewApprover(p *policy.Policy, c *policy.Classifier, u UsageTracker) *Approver {
	return &Approver{
		policy:     p,
		classifier: c,
		usage:      u,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// CheckAction 判定一个待执行动作。
func (a *Approver) CheckAction(ctx context.Context, t *core.Task, d kernel.Decision) (kernel.ApprovalOutcome, string, error) {
	now := a.now()

	// 有外传风险的安装必须由用户批准，没有任何规则能放行它。
	if reason, risky := a.installRisk(d); risky {
		return kernel.ApprovalManual, reason, nil
	}

	// 按地址装 MCP server 只对用户亲口给的地址直接放行。地址是助手从别处读来的（网页、邮件、搜索结果），
	// 或者核对不了，就交给用户确认——否则一封邮件写一句"装一下这个 MCP"就能让助手接上别人的服务。
	if reason, ask := a.strangerAddress(ctx, t, d); ask {
		return kernel.ApprovalManual, reason, nil
	}

	// 参数分级失败时按最保守处理：分不清就不能放行。
	fields, maxPrivacy, err := a.classifier.ClassifyJSON(classifiable(d))
	if err != nil {
		return kernel.ApprovalManual, "参数无法完成隐私分级，转人工确认", nil
	}

	// 写进用户自己的日历不算"把数据交给第三方"：那是他自己的地方，本来就是他记这些事的地方。
	// "周三手术复查"写不进日历的助手没法用。所以日历工具不适用"含绝密数据一律确认"，
	// 降一级按敏感数据对待——其余规则照旧：写操作默认要确认，外部内容驱动的一律要确认。
	// 这个例外只给日历。邮件是发给别人的，照常拦。
	// 写进本机工作区的文件同理：它连这台机器都没离开。
	if (isCalendarTool(d.ToolName) || isFileWrite(d.ToolName)) && maxPrivacy.NeverLeavesInstance() {
		maxPrivacy = core.PrivacySensitive
	}

	usage, err := a.usage.Usage(ctx, t.ID, now)
	if err != nil {
		return kernel.ApprovalManual, "无法读取累计用量，转人工确认", nil
	}

	origin, source, err := a.drivenBy(ctx, t, d)
	if err != nil {
		return kernel.ApprovalManual, "无法读取任务历史，转人工确认", nil
	}

	amount, eventAt := extractIntent(d.Input)
	out := outbound(d.ToolName, d.SideEffect)
	// 联网搜索是只读的，但搜索词会交给模型服务和搜索引擎：里面带着个人敏感信息时按对外发送对待。
	if d.ToolName == ToolWebSearch && a.classifier.ScanText(string(d.Input)).Level >= core.PrivacySensitive {
		out = true
		if maxPrivacy < core.PrivacySensitive {
			maxPrivacy = core.PrivacySensitive
		}
	}
	// 读网页也是只读的，但网址能带东西出去：网址是用户给的、或者原样出现在之前取回的内容里，
	// 就没有夹带什么；模型自己拼出来的网址按对外发送对待。
	if d.ToolName == ToolWebFetch && !a.knownURL(ctx, t, d.Input) {
		out = true
	}
	decision := a.policy.Decide(policy.Request{
		Outbound:    out,
		Tool:        d.ToolName,
		SideEffect:  d.SideEffect,
		AmountCents: amount,
		Origin:      origin,
		Domain:      t.Domain,
		EventAt:     eventAt,
		MaxPrivacy:  maxPrivacy,
		Secrets:     policy.SecretItems(fields),
	}, usage, now)
	secret := maxPrivacy.NeverLeavesInstance()

	// 金额无从核实的付款不适用任何自动放行规则。拒绝的结论不受影响。
	if unverifiableAmount(d.ToolName) && decision.Outcome == policy.OutcomeAuto {
		return kernel.ApprovalManual, "这笔付款的金额无法由系统核实，必须由你确认", nil
	}

	// 说清楚为什么这次要确认：同样的操作，平时是直接做的。
	// 含绝密数据时保留那条理由：用户要先知道交出去的是哪几项信息。
	if source != "" && decision.Outcome == policy.OutcomeManual && !secret {
		decision.Reason = fmt.Sprintf("这个任务读过外部内容（%s），由它引出的改动交给你定夺", source)
	}

	switch decision.Outcome {
	case policy.OutcomeAuto:
		// 自动放行的动作要计入累计，否则帽子形同虚设。
		if err := a.usage.RecordAuto(ctx, t.ID, amount, now); err != nil {
			return kernel.ApprovalManual, "无法记录累计用量，转人工确认", nil
		}
		return kernel.ApprovalAuto, decision.Reason, nil
	case policy.OutcomeDeny:
		return kernel.ApprovalDeny, decision.Reason, nil
	default:
		return kernel.ApprovalManual, decision.Reason, nil
	}
}

// intentFields 是参数里被识别为"金额"和"事件时间"的约定字段名。
//
// 靠约定字段而不是让模型自己申报金额：模型申报的数字不可信，
// 但这些字段是我们构造给工具的入参，来源是可控的。
type intentFields struct {
	AmountCents *int64  `json:"amount_cents"`
	Amount      *string `json:"amount"`
	EventAt     *string `json:"event_at"`
	StartAt     *string `json:"start_at"`
}

func extractIntent(raw json.RawMessage) (int64, *time.Time) {
	if len(raw) == 0 {
		return 0, nil
	}
	var f intentFields
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, nil
	}
	var amount int64
	if f.AmountCents != nil {
		amount = *f.AmountCents
	}
	for _, s := range []*string{f.EventAt, f.StartAt} {
		if s == nil {
			continue
		}
		// 相对时间在这里没有意义，必须是绝对时间。
		if ts, err := time.Parse(time.RFC3339, *s); err == nil {
			return amount, &ts
		}
	}
	return amount, nil
}

// MemoryUsageTracker 是 UsageTracker 的内存实现。
//
// 进程重启后累计清零——这是个已知缺口，P1 之后要落到 Postgres，
// 否则反复重启就能绕过每日帽子。
type MemoryUsageTracker struct {
	mu    sync.Mutex
	day   string
	count int
	spent int64
	tasks map[string]int64
}

var _ UsageTracker = (*MemoryUsageTracker)(nil)

// NewMemoryUsageTracker 构造内存用量跟踪器。
func NewMemoryUsageTracker() *MemoryUsageTracker {
	return &MemoryUsageTracker{tasks: make(map[string]int64)}
}

func (m *MemoryUsageTracker) Usage(_ context.Context, taskID string, day time.Time) (policy.Usage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollover(day)
	return policy.Usage{
		DailyAutoAmountCents: m.spent,
		DailyAutoCount:       m.count,
		TaskAmountCents:      m.tasks[taskID],
	}, nil
}

func (m *MemoryUsageTracker) RecordAuto(_ context.Context, taskID string, amountCents int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollover(at)
	m.count++
	m.spent += amountCents
	m.tasks[taskID] += amountCents
	return nil
}

func (m *MemoryUsageTracker) rollover(at time.Time) {
	d := at.UTC().Format("2006-01-02")
	if m.day != d {
		m.day = d
		m.count = 0
		m.spent = 0
	}
}
