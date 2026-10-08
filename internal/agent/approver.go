// Package agent 把内核、LLM 路由、MCP 网关和安全策略接在一起。
//
// 它是唯一知道这四者同时存在的地方；下层互相不感知。
package agent

import (
	"context"
	"encoding/json"
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
	now        func() time.Time
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

	// 参数分级失败时按最保守处理：分不清就不能放行。
	_, maxPrivacy, err := a.classifier.ClassifyJSON(d.Input)
	if err != nil {
		return kernel.ApprovalManual, "参数无法完成隐私分级，转人工确认", nil
	}

	usage, err := a.usage.Usage(ctx, t.ID, now)
	if err != nil {
		return kernel.ApprovalManual, "无法读取累计用量，转人工确认", nil
	}

	amount, eventAt := extractIntent(d.Input)
	decision := a.policy.Decide(policy.Request{
		Tool:        d.ToolName,
		SideEffect:  d.SideEffect,
		AmountCents: amount,
		Origin:      t.Origin,
		Domain:      t.Domain,
		EventAt:     eventAt,
		MaxPrivacy:  maxPrivacy,
	}, usage, now)

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
