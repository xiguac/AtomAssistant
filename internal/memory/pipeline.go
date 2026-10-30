package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
)

// Query 是记忆检索条件。
type Query struct {
	Subject string
	Kinds   []Kind
	Domain  string
	Tags    []string
	// SafetyOnly 只返回安全相关属性，用于强制注入。
	SafetyOnly bool
	// IncludeInactive 包含已过期和已归档的记忆，用于回溯。
	IncludeInactive bool
	Limit           int
}

// Store 是记忆的持久化契约。
type Store interface {
	Get(ctx context.Context, id string) (*Memory, error)
	// FindByKey 按身份键查找。同一个键上的新值构成版本演进，而不是新增一条。
	FindByKey(ctx context.Context, key string) (*Memory, error)
	Put(ctx context.Context, m *Memory) error
	List(ctx context.Context, q Query) ([]*Memory, error)
	// PutPending 保存一条待用户裁决的候选。
	PutPending(ctx context.Context, p Pending) error
	ListPending(ctx context.Context, subject string) ([]Pending, error)
	// ResolvePending 标记一条候选已被用户裁决，此后不再出现在待裁决列表里。
	ResolvePending(ctx context.Context, id string, at time.Time) error
}

// Pending 是一条需要用户裁决后才能生效的候选。
type Pending struct {
	ID string
	// MemoryID 非空表示与某条既有记忆冲突。
	MemoryID  string
	Candidate Candidate
	Arb       Arbitration
	CreatedAt time.Time
}

// Corroborator 为一条冲突做交叉验证，从历史任务和其他记忆里找佐证或反驳。
//
// 这是"结合其他记忆做判断"的落点：新说法"张总不忌海鲜"出现时，
// 应当能翻出三月那次"因客户海鲜过敏改订川菜，客户当场确认"。
type Corroborator interface {
	Corroborate(ctx context.Context, existing *Memory, cand Candidate) ([]Evidence, error)
}

// noCorroborator 是没有配置交叉验证时的退化实现：不提供任何证据。
type noCorroborator struct{}

func (noCorroborator) Corroborate(context.Context, *Memory, Candidate) ([]Evidence, error) {
	return nil, nil
}

// Outcome 是一条候选的处理结果。
type Outcome struct {
	Candidate Candidate
	// MemoryID 非空表示落到了某条记忆上。
	MemoryID   string
	Resolution Resolution
	Arb        Arbitration
	// Pending 非空表示挂起等待用户裁决。
	PendingID string
}

// Pipeline 是记忆写入管线。
//
// 不做无脑追加：候选要经过校验、去重、冲突检测、置信度评估，
// 才可能落库。记忆污染是这类产品的头号杀手——错误记忆会持续影响
// 所有后续决策，且很难被发现。
type Pipeline struct {
	store Store
	corr  Corroborator
	now   func() time.Time
}

// NewPipeline 构造写入管线。corr 为 nil 时不做交叉验证。
func NewPipeline(s Store, corr Corroborator) *Pipeline {
	if corr == nil {
		corr = noCorroborator{}
	}
	return &Pipeline{store: s, corr: corr, now: func() time.Time { return time.Now().UTC() }}
}

// ErrInvalidCandidate 表示候选不合法。
var ErrInvalidCandidate = errors.New("memory: 候选不合法")

// Validate 校验一条候选。
func (c *Candidate) Validate() error {
	if !c.Kind.Valid() {
		return fmt.Errorf("%w: 未知类型 %q", ErrInvalidCandidate, c.Kind)
	}
	if c.Subject == "" || c.Value == "" {
		return fmt.Errorf("%w: Subject 与 Value 不能为空", ErrInvalidCandidate)
	}
	// 抽取时就必须输出时效字段，不能事后补——事后没人知道当时的语境。
	if !c.Temporality.Valid() {
		return fmt.Errorf("%w: 未判定时效类型", ErrInvalidCandidate)
	}
	if c.Temporality == TemporalLimited && c.ValidUntil == nil {
		return fmt.Errorf("%w: 限时记忆必须给出绝对的过期时间", ErrInvalidCandidate)
	}
	if c.Kind == KindEntity && c.Attribute == "" {
		return fmt.Errorf("%w: 实体档案必须指明属性名", ErrInvalidCandidate)
	}
	return nil
}

// Ingest 处理一批候选。
func (p *Pipeline) Ingest(ctx context.Context, cands []Candidate) ([]Outcome, error) {
	now := p.now()
	out := make([]Outcome, 0, len(cands))

	for _, cand := range dedupe(cands) {
		if err := cand.Validate(); err != nil {
			return nil, err
		}
		// 一次性记忆不进长期记忆，它只在任务生命周期内有意义。
		if cand.Temporality == TemporalOneShot {
			continue
		}

		existing, err := p.store.FindByKey(ctx, cand.Key())
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}

		if existing == nil {
			o, err := p.insert(ctx, cand, now)
			if err != nil {
				return nil, err
			}
			out = append(out, o)
			continue
		}

		evidence, err := p.corr.Corroborate(ctx, existing, cand)
		if err != nil {
			return nil, fmt.Errorf("memory: 交叉验证失败: %w", err)
		}
		arb := Arbitrate(existing, cand, evidence, now)

		o, err := p.apply(ctx, existing, cand, arb, now)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// insert 写入一条全新的记忆。
func (p *Pipeline) insert(ctx context.Context, cand Candidate, now time.Time) (Outcome, error) {
	// 不可信来源连新建都不允许：截图里出现的"事实"必须经用户确认。
	// 助手自己的推断是例外：先以较轻的分量记下，不拿它去打扰用户；
	// 之后被印证了再加重，印证不了就随时间淡掉。安全相关的推断不在此列——
	// 过敏、禁忌记错一次就是事故，那一类仍然要用户点头。
	if !cand.Source.Trust.CanWriteMemoryDirectly() && (!cand.Inferred() || cand.SafetyCritical) {
		pending := Pending{
			ID:        core.NewID(),
			Candidate: cand,
			Arb: Arbitration{
				Kind:       ConflictNone,
				Resolution: ResolveHoldAsCandidate,
				Reason: fmt.Sprintf("来源信任等级为 %s，需用户确认后才能记住",
					cand.Source.Trust),
			},
			CreatedAt: now,
		}
		if err := p.store.PutPending(ctx, pending); err != nil {
			return Outcome{}, err
		}
		return Outcome{Candidate: cand, Resolution: ResolveHoldAsCandidate, PendingID: pending.ID}, nil
	}

	m := New(cand.Kind, cand.Subject, cand.Domain, cand.Value, cand.Source, now)
	m.Attribute = cand.Attribute
	m.Condition = cand.Condition
	m.Temporality = cand.Temporality
	m.ValidUntil = cand.ValidUntil
	m.SafetyCritical = cand.SafetyCritical
	m.Tags = cand.Tags
	m.Current.Evidence = cand.Evidence

	if err := m.Validate(); err != nil {
		return Outcome{}, err
	}
	if err := p.store.Put(ctx, m); err != nil {
		return Outcome{}, err
	}
	return Outcome{Candidate: cand, MemoryID: m.ID, Resolution: ResolveUpdate}, nil
}

// apply 按仲裁结论落地。
func (p *Pipeline) apply(ctx context.Context, m *Memory, cand Candidate, arb Arbitration, now time.Time) (Outcome, error) {
	o := Outcome{Candidate: cand, MemoryID: m.ID, Resolution: arb.Resolution, Arb: arb}

	switch arb.Resolution {
	case ResolveReinforce:
		reinforce(m, cand)
		m.UpdatedAt = now
		if err := p.store.Put(ctx, m); err != nil {
			return Outcome{}, err
		}

	case ResolveUpdate:
		reason := OverrideNewEvidence
		if arb.Kind == ConflictTemporalSuccession {
			reason = OverrideExpired
		}
		m.Supersede(Revision{
			ID:         core.NewID(),
			Value:      cand.Value,
			Source:     cand.Source,
			Evidence:   cand.Evidence,
			Confidence: initialConfidence(cand.Source),
			CreatedAt:  now,
		}, reason, now)
		// 更新时效：新值可能换了时效类型（临时住址 → 长期住址）。
		m.Temporality = cand.Temporality
		m.ValidUntil = cand.ValidUntil
		m.Status = StatusActive
		if err := p.store.Put(ctx, m); err != nil {
			return Outcome{}, err
		}

	case ResolveKeepBoth, ResolveChallenge, ResolveHoldAsCandidate:
		if arb.Resolution != ResolveHoldAsCandidate {
			// 标记分歧：这条记忆仍可被检索到，但必须带着分歧提示，
			// 让模型知道这里存在未裁决的矛盾。
			m.Status = StatusDisputed
			m.UpdatedAt = now
			if err := p.store.Put(ctx, m); err != nil {
				return Outcome{}, err
			}
		}
		pending := Pending{
			ID:        core.NewID(),
			MemoryID:  m.ID,
			Candidate: cand,
			Arb:       arb,
			CreatedAt: now,
		}
		if err := p.store.PutPending(ctx, pending); err != nil {
			return Outcome{}, err
		}
		o.PendingID = pending.ID
	}
	return o, nil
}

// reinforce 用一条与既有记忆一致的候选给它加分量。这就是"交叉印证"的落点。
func reinforce(m *Memory, cand Candidate) {
	// 只有带来新证据的才算数：同一个任务把同一件事说两遍，不是两次印证。
	fresh := false
	have := make(map[string]bool, len(m.Current.Evidence))
	for _, e := range m.Current.Evidence {
		have[e] = true
	}
	for _, e := range cand.Evidence {
		if !have[e] {
			fresh = true
			m.Current.Evidence = append(m.Current.Evidence, e)
		}
	}
	switch {
	case authority(cand.Source.Kind) > authority(m.Current.Source.Kind):
		// 更权威的来源说了同样的话（最常见的是用户亲口说中了助手的推断）：
		// 这条记忆从此算在更权威的来源名下。
		m.Current.Source = cand.Source
		m.Confidence = math.Max(m.Confidence, initialConfidence(cand.Source))
	case !fresh:
		return
	case m.Inferred():
		if m.Confidence < InferredCeiling {
			m.Confidence = math.Min(InferredCeiling, m.Confidence+corroborationStep)
		}
	default:
		// 同一事实反复出现就提升置信度，但有上限：重复听到一件事不等于它是真的。
		m.Confidence = math.Min(0.99, m.Confidence+0.05)
	}
	m.Current.Confidence = m.Confidence
}

// Affirm 是用户对一条已经记下的记忆说"对，就是这样"。
// 助手推断出来的记忆由此转到用户名下，分量与用户亲口说的相同。
func (p *Pipeline) Affirm(ctx context.Context, memoryID string) error {
	m, err := p.store.Get(ctx, memoryID)
	if err != nil {
		return err
	}
	now := p.now()
	m.Current.Source = Source{Kind: SourceUser, Ref: m.Current.Source.Ref, Trust: core.TrustUser}
	m.Confidence = math.Max(m.Confidence, initialConfidence(m.Current.Source))
	m.Current.Confidence = m.Confidence
	m.Status = StatusActive
	m.UpdatedAt = now
	return p.store.Put(ctx, m)
}

// Revise 是用户把一条已经记下的记忆改成 value。旧的内容留在历史里，新的内容算用户亲口说的。
func (p *Pipeline) Revise(ctx context.Context, memoryID, value string) error {
	m, err := p.store.Get(ctx, memoryID)
	if err != nil {
		return err
	}
	now := p.now()
	src := Source{Kind: SourceUser, Ref: m.Current.Source.Ref, Trust: core.TrustUser}
	m.Supersede(Revision{ID: core.NewID(), Value: value, Source: src, Confidence: initialConfidence(src), CreatedAt: now},
		OverrideUserExplicit, now)
	m.Status = StatusActive
	return p.store.Put(ctx, m)
}

// Forget 是用户对一条已经记下的记忆说"不对，忘掉"。
// 记忆退出检索但不物理删除：之后能回溯助手当时为什么那样判断。
func (p *Pipeline) Forget(ctx context.Context, memoryID string) error {
	m, err := p.store.Get(ctx, memoryID)
	if err != nil {
		return err
	}
	m.Status = StatusArchived
	m.UpdatedAt = p.now()
	return p.store.Put(ctx, m)
}

// ErrPendingNotFound 表示没有这条待裁决的候选。
var ErrPendingNotFound = errors.New("memory: 没有这条待裁决的候选")

func (p *Pipeline) findPending(ctx context.Context, id string) (Pending, error) {
	list, err := p.store.ListPending(ctx, "")
	if err != nil {
		return Pending{}, err
	}
	for _, pd := range list {
		if pd.ID == id {
			return pd, nil
		}
	}
	return Pending{}, fmt.Errorf("%w: %s", ErrPendingNotFound, id)
}

// Confirm 是用户对一条候选说"对，记住它"。
//
// 候选从此以用户本人的名义生效：新记忆直接落库；与既有记忆冲突的，
// 走 UserOverride——旧值进版本链，不删除。返回落到的记忆 ID。
func (p *Pipeline) Confirm(ctx context.Context, pendingID string) (string, error) {
	pd, err := p.findPending(ctx, pendingID)
	if err != nil {
		return "", err
	}
	now := p.now()
	cand := pd.Candidate
	cand.Source = Source{Kind: SourceUser, Ref: cand.Source.Ref, Trust: core.TrustUser}

	existing, err := p.store.FindByKey(ctx, cand.Key())
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	var id string
	if existing == nil {
		o, err := p.insert(ctx, cand, now)
		if err != nil {
			return "", err
		}
		id = o.MemoryID
	} else {
		UserOverride(existing, cand.Value, now)
		existing.Temporality = cand.Temporality
		existing.ValidUntil = cand.ValidUntil
		existing.Status = StatusActive
		if err := p.store.Put(ctx, existing); err != nil {
			return "", err
		}
		id = existing.ID
	}
	return id, p.store.ResolvePending(ctx, pendingID, now)
}

// Dismiss 是用户对一条候选说"不对，别记"。既有记忆若因它被标为有分歧，恢复为生效。
func (p *Pipeline) Dismiss(ctx context.Context, pendingID string) error {
	pd, err := p.findPending(ctx, pendingID)
	if err != nil {
		return err
	}
	now := p.now()
	if pd.MemoryID != "" {
		m, err := p.store.Get(ctx, pd.MemoryID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if m != nil && m.Status == StatusDisputed {
			m.Status = StatusActive
			m.UpdatedAt = now
			if err := p.store.Put(ctx, m); err != nil {
				return err
			}
		}
	}
	return p.store.ResolvePending(ctx, pendingID, now)
}

// dedupe 按身份键去重，同键保留最后一条。
func dedupe(cands []Candidate) []Candidate {
	seen := make(map[string]int, len(cands))
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		key := c.Key()
		if i, ok := seen[key]; ok {
			out[i] = c
			continue
		}
		seen[key] = len(out)
		out = append(out, c)
	}
	return out
}

// MaintenanceReport 是一次维护的结果。
type MaintenanceReport struct {
	Expired  int
	Decayed  int
	Archived int
	// DueForReview 是置信度衰减到阈值、建议向用户复核的记忆。
	DueForReview []string
}

// DecayOptions 控制遗忘机制。
type DecayOptions struct {
	// HalfLife 是长期可变记忆的置信度半衰期。
	HalfLife time.Duration
	// ArchiveBelow 是归档阈值：置信度低于它且长期未命中就归档。
	ArchiveBelow float64
	// IdleBeforeArchive 是"长期未命中"的时长门槛。
	IdleBeforeArchive time.Duration
	// ReviewBelow 是建议复核的置信度阈值。
	ReviewBelow float64
}

func (o *DecayOptions) applyDefaults() {
	if o.HalfLife <= 0 {
		o.HalfLife = 365 * 24 * time.Hour
	}
	if o.ArchiveBelow <= 0 {
		o.ArchiveBelow = 0.25
	}
	if o.IdleBeforeArchive <= 0 {
		o.IdleBeforeArchive = 180 * 24 * time.Hour
	}
	if o.ReviewBelow <= 0 {
		o.ReviewBelow = 0.45
	}
}

// Maintain 执行过期、衰减与归档。
//
// 没有遗忘机制的记忆库会无限膨胀，最终把检索淹没在陈年旧事里——
// 这和记错事一样会毁掉决策质量。
func (p *Pipeline) Maintain(ctx context.Context, subject string, opts DecayOptions) (MaintenanceReport, error) {
	opts.applyDefaults()
	now := p.now()

	all, err := p.store.List(ctx, Query{Subject: subject, IncludeInactive: true})
	if err != nil {
		return MaintenanceReport{}, err
	}

	var rep MaintenanceReport
	for _, m := range all {
		if m.Status == StatusArchived {
			continue
		}
		changed := false

		// 1. 过期不等于删除：退出主检索，但留着供"我上个月预算是多少"这类回溯。
		if m.Expired(now) && m.Status != StatusExpired {
			m.Status = StatusExpired
			rep.Expired++
			changed = true
		}

		// 2. 长期可变记忆随时间衰减。永久记忆（血型、过敏）不衰减。
		if m.Status == StatusActive && m.Temporality == TemporalDurable {
			age := now.Sub(lastTouch(m))
			if age > 0 {
				decayed := m.Confidence * math.Pow(0.5, age.Seconds()/opts.HalfLife.Seconds())
				if decayed < m.Confidence-0.001 {
					m.Confidence = decayed
					rep.Decayed++
					changed = true
				}
			}
			if m.Confidence < opts.ReviewBelow {
				rep.DueForReview = append(rep.DueForReview, m.ID)
			}
		}

		// 3. 低置信 + 长期未命中 → 归档。安全属性永不归档。
		if !m.SafetyCritical && m.Confidence < opts.ArchiveBelow &&
			now.Sub(lastTouch(m)) > opts.IdleBeforeArchive {
			m.Status = StatusArchived
			rep.Archived++
			changed = true
		}

		if changed {
			m.UpdatedAt = now
			if err := p.store.Put(ctx, m); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

func lastTouch(m *Memory) time.Time {
	if m.LastHitAt != nil && m.LastHitAt.After(m.UpdatedAt) {
		return *m.LastHitAt
	}
	return m.UpdatedAt
}

// ErrNotFound 表示记忆不存在。
var ErrNotFound = errors.New("memory: 记忆不存在")
