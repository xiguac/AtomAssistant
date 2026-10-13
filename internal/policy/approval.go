package policy

import (
	"fmt"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
)

// Outcome 是一次审批判定的结果。
type Outcome string

const (
	// OutcomeAuto 允许直接执行。
	OutcomeAuto Outcome = "auto"
	// OutcomeManual 需要用户在 App 上确认。
	OutcomeManual Outcome = "manual"
	// OutcomeDeny 直接拒绝，不进审批队列。
	OutcomeDeny Outcome = "deny"
)

// ReasonNoRule 是没有为某个工具配置放行规则时的理由。它是最常见的一种，
// 展示给用户时可以省略——每次确认都看到这句话没有信息量。
const ReasonNoRule = "未配置自动放行规则，默认手动确认"

// Decision 是判定结果及其理由。理由要能直接展示给用户。
type Decision struct {
	Outcome Outcome
	Reason  string
}

// Condition 是自动放行的条件。零值表示该维度不作要求。
type Condition struct {
	// AmountBelowCents 要求金额低于该值。
	AmountBelowCents int64
	// Domain 要求任务属于指定域。
	Domain core.Domain
	// LeadTimeAbove 要求距事件发生还有足够时间（改明天的日程比改一小时后的安全）。
	LeadTimeAbove time.Duration
}

// Rule 是针对某个工具的放行规则。
type Rule struct {
	Tool string
	// Never 为真时该工具永远需要手动确认，忽略 AutoIf。
	Never bool
	// AutoIf 全部满足时自动放行。
	AutoIf Condition
}

// Caps 是累计上限。
//
// 只设单次阈值是个洞：100 元以下自动放行，一个跑飞的任务可以拆成 50 次。
// 所以单日总额、单日次数、单任务总额都必须有帽子。
type Caps struct {
	DailyAutoAmountCents int64
	DailyAutoCount       int
	PerTaskAmountCents   int64
}

// ReasonOutbound 是自主模式下一个对外发送的动作要用户确认的理由。
const ReasonOutbound = "这个动作会把内容发到外部，由你确认"

// ReasonNoCap 是自主模式下一笔花钱的操作因为没有设置金额上限而转手动的理由。
const ReasonNoCap = "未设置金额上限（单任务或单日），涉及金额的操作需手动确认"

// Policy 是用户配置的审批策略。默认一律手动确认。
type Policy struct {
	Rules []Rule
	Caps  Caps
	// Autonomous 为真是自主模式：取数据和在用户自己地盘上的处理（记日程、写文件、整理邮件）直接执行，
	// 不再默认手动确认。
	//
	// 长程任务要调几十次工具，每一次都等人点确认的话，人就得一直盯着。自主模式放开的只是内部的事；
	// 对外发送的动作（Request.Outbound）仍然要用户确认，除非用户为那个工具写了放行规则。
	// 下面这些也照旧：绝密数据不外传；不是用户发起的任务不能改动东西；用户写了 never 的工具始终确认；
	// 用户写了条件的按条件；累计帽子；以及花钱的操作必须有金额上限罩着，没设上限就仍然要确认。
	Autonomous bool
}

// Request 是一次待判定的操作。
type Request struct {
	Tool        string
	SideEffect  core.SideEffect
	AmountCents int64
	// Origin 是驱动这次操作的内容信任等级。
	Origin core.TrustLevel
	Domain core.Domain
	// EventAt 是这次操作影响的事件发生时间，用于 LeadTimeAbove 判断。
	EventAt *time.Time
	// MaxPrivacy 是参数里出现的最高隐私等级。
	MaxPrivacy core.PrivacyLevel
	// Outbound 为真表示这个动作会把内容发到外部：发信、付款、向外部服务提交信息。
	// 查询不算——它带出去的只是查询条件，而且能带多敏感的数据已经由工具的隐私上限管着。
	Outbound bool
}

// Usage 是当前的累计用量。
type Usage struct {
	DailyAutoAmountCents int64
	DailyAutoCount       int
	TaskAmountCents      int64
}

// Decide 判定一次操作该自动执行、请用户确认，还是直接拒绝。
//
// 判定顺序是刻意的：硬性拒绝 → 无需审批 → 强制手动 → 用户规则 → 累计帽子。
// 任何一步得出"手动"就不再往下看是否能放行——放行条件永远不能覆盖收紧条件。
func (p *Policy) Decide(req Request, usage Usage, now time.Time) Decision {
	// 1. 硬性拒绝：绝密数据不参与任何对外操作，连审批机会都不给。
	//    给了审批机会，用户点确认就泄露了。
	if req.MaxPrivacy.NeverLeavesInstance() {
		return Decision{OutcomeDeny, "参数含绝密数据（L3），此类操作不可执行"}
	}

	// 2. 无副作用或只读操作不需要审批。例外是自主模式下被认定为对外发送的：
	//    它不改动东西，但会把模型写的内容带到外部，走下面对外发送的那一条。
	readOnly := req.SideEffect == core.SideEffectNone || req.SideEffect == core.SideEffectReadOnly
	if readOnly && !(p.Autonomous && req.Outbound) {
		return Decision{OutcomeAuto, "只读操作"}
	}

	// 3. untrusted 来源驱动的写操作，无论金额多小一律手动确认。
	//    这条挡的是"截图/邮件里埋一句话让助手去下单"。
	if !req.Origin.CanTriggerIrreversible() {
		return Decision{OutcomeManual,
			fmt.Sprintf("操作由 %s 级内容驱动，不适用任何自动放行", req.Origin)}
	}

	// 4. 单任务金额帽子。
	if p.Caps.PerTaskAmountCents > 0 &&
		usage.TaskAmountCents+req.AmountCents > p.Caps.PerTaskAmountCents {
		return Decision{OutcomeManual, fmt.Sprintf("超出单任务金额上限 %s",
			yuan(p.Caps.PerTaskAmountCents))}
	}

	rule, found := p.ruleFor(req.Tool)
	switch {
	case !found && !p.Autonomous:
		return Decision{OutcomeManual, ReasonNoRule}
	case !found && req.Outbound:
		// 对外发出去的东西收不回来，而且任务读过的网页、邮件里可能埋着让助手往外发东西的话。
		// 这一类不交给模型判断，由用户确认。
		if req.MaxPrivacy >= core.PrivacySensitive {
			return Decision{OutcomeManual, ReasonOutbound + "（其中含有敏感信息）"}
		}
		return Decision{OutcomeManual, ReasonOutbound}
	case !found:
		// 自主模式下没有规则的工具直接执行，但花钱必须有上限罩着：
		// 没有任何金额上限时，一个跑飞的任务能花多少就没有边了。
		if req.AmountCents > 0 && p.Caps.PerTaskAmountCents <= 0 && p.Caps.DailyAutoAmountCents <= 0 {
			return Decision{OutcomeManual, ReasonNoCap}
		}
	case rule.Never:
		return Decision{OutcomeManual, fmt.Sprintf("%s 被设置为始终手动确认", req.Tool)}
	default:
		if reason, ok := matches(rule.AutoIf, req, now); !ok {
			return Decision{OutcomeManual, reason}
		}
	}

	// 5. 累计帽子放在最后：即使单次条件满足，也可能因为今天已经自动执行太多次而转手动。
	if p.Caps.DailyAutoCount > 0 && usage.DailyAutoCount+1 > p.Caps.DailyAutoCount {
		return Decision{OutcomeManual, fmt.Sprintf("已达今日自动执行次数上限 %d 次",
			p.Caps.DailyAutoCount)}
	}
	if p.Caps.DailyAutoAmountCents > 0 &&
		usage.DailyAutoAmountCents+req.AmountCents > p.Caps.DailyAutoAmountCents {
		return Decision{OutcomeManual, fmt.Sprintf("超出今日自动执行金额上限 %s",
			yuan(p.Caps.DailyAutoAmountCents))}
	}

	if !found {
		return Decision{OutcomeAuto, "自主模式：直接执行"}
	}
	return Decision{OutcomeAuto, fmt.Sprintf("满足 %s 的自动放行条件", req.Tool)}
}

func (p *Policy) ruleFor(tool string) (Rule, bool) {
	for _, r := range p.Rules {
		if r.Tool == tool {
			return r, true
		}
	}
	return Rule{}, false
}

// matches 检查是否满足全部自动放行条件，不满足时返回可展示给用户的理由。
func matches(c Condition, req Request, now time.Time) (string, bool) {
	if c.AmountBelowCents > 0 && req.AmountCents >= c.AmountBelowCents {
		return fmt.Sprintf("金额 %s 超过自动放行阈值 %s",
			yuan(req.AmountCents), yuan(c.AmountBelowCents)), false
	}
	if c.Domain != "" && req.Domain != c.Domain {
		return fmt.Sprintf("仅 %s 域可自动放行，当前为 %s 域", c.Domain, req.Domain), false
	}
	if c.LeadTimeAbove > 0 {
		if req.EventAt == nil {
			return "缺少事件时间，无法判断提前量", false
		}
		if req.EventAt.Sub(now) < c.LeadTimeAbove {
			return fmt.Sprintf("距事件发生不足 %s，需手动确认", c.LeadTimeAbove), false
		}
	}
	return "", true
}

func yuan(cents int64) string {
	return fmt.Sprintf("%.2f 元", float64(cents)/100)
}
