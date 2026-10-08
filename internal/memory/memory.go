// Package memory 是长期记忆系统。
//
// 三条贯穿全包的设计：
//  1. 分类型建模，不把所有东西一股脑塞进向量库——偏好、实体档案、事实、
//     流程、情景各有各的检索策略。
//  2. 偏好必须是**条件化规则**："用户打车喜欢便宜的"是错的写法，
//     正确的是 {场景:个人通勤}→顺风车 / {场景:接待客户}→豪华型。
//  3. 每条记忆写入时必须判定时效，没有"默认永久"这回事。
//
// 见 docs/架构方案.md §4。
package memory

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
)

// Kind 是长期记忆的类型。
type Kind string

const (
	// KindPreference 偏好记忆，必须条件化。
	KindPreference Kind = "preference"
	// KindEntity 实体档案：客户、同事、家人、常去地点。
	KindEntity Kind = "entity"
	// KindFact 语义事实："公司报销上限 500"。
	KindFact Kind = "fact"
	// KindPlaybook 程序记忆：从成功任务沉淀的可复用流程。
	KindPlaybook Kind = "playbook"
	// KindEpisode 情景摘要：任务归档的索引投影。
	KindEpisode Kind = "episode"
)

func (k Kind) Valid() bool {
	switch k {
	case KindPreference, KindEntity, KindFact, KindPlaybook, KindEpisode:
		return true
	}
	return false
}

// Temporality 是记忆的时效类型。写入时必须判定，不允许留空。
type Temporality string

const (
	// TemporalPermanent 永久：血型、海鲜过敏、生日。变更需要强证据。
	TemporalPermanent Temporality = "permanent"
	// TemporalDurable 长期可变：住址、职位、常用车型偏好。
	// 无硬过期，但置信度随时间缓慢衰减，低到阈值就触发复核。
	TemporalDurable Temporality = "durable"
	// TemporalLimited 限时：本月预算余额、下周在上海出差。必须有 ValidUntil。
	TemporalLimited Temporality = "limited"
	// TemporalOneShot 一次性：只在单个任务内有意义，任务结束即失效。
	TemporalOneShot Temporality = "one_shot"
)

func (t Temporality) Valid() bool {
	switch t {
	case TemporalPermanent, TemporalDurable, TemporalLimited, TemporalOneShot:
		return true
	}
	return false
}

// Status 是记忆的当前状态。
type Status string

const (
	// StatusActive 生效中，参与检索。
	StatusActive Status = "active"
	// StatusDisputed 存在未裁决的矛盾，参与检索但必须带上分歧提示。
	StatusDisputed Status = "disputed"
	// StatusExpired 已过期，退出主检索但保留以供回溯。
	StatusExpired Status = "expired"
	// StatusArchived 已归档：长期未命中且置信度低，被遗忘机制降权。
	StatusArchived Status = "archived"
)

// Condition 是偏好生效的场景条件，维度化的键值对。
//
// 空条件表示默认规则。条件越具体（键越多）优先级越高，
// 所以「{场景:接待客户}→豪华型」会盖过「{}→顺风车」。
type Condition map[string]string

// Specificity 是条件的特异度，用于冲突排序。
func (c Condition) Specificity() int { return len(c) }

// Matches 报告当前场景是否满足这个条件。
//
// 条件里的每一项都必须在场景中出现且取值相同；场景里多余的维度不影响匹配。
func (c Condition) Matches(scene Condition) bool {
	for k, v := range c {
		if scene[k] != v {
			return false
		}
	}
	return true
}

// String 返回稳定的文本表示，用于去重与展示。
func (c Condition) String() string {
	if len(c) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+c[k])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// SourceKind 是记忆的来源类别。
type SourceKind string

const (
	SourceUser      SourceKind = "user"      // 用户直接告知
	SourceTask      SourceKind = "task"      // 从任务执行中提炼
	SourceConnector SourceKind = "connector" // 权威数据同步
	SourceObserved  SourceKind = "observed"  // 从行为中观察推断
)

// Source 记录一条记忆是怎么来的。
//
// 来源不只是元数据：仲裁冲突时，权威数据优先于用户口述，
// 用户口述优先于行为观察。
type Source struct {
	Kind SourceKind
	// Ref 指向具体来源，如任务 ID。
	Ref string
	// Trust 是产生这条记忆的内容信任等级。
	// untrusted 内容永远只能产生候选，不能直接落库。
	Trust core.TrustLevel
}

// Revision 是记忆值的一个版本。
//
// 用户执意覆盖旧值时，旧值不被物理删除而是转入版本链——
// 这样可以回溯"助手当时为什么那么判断"，也可以一键回滚误覆盖。
type Revision struct {
	ID       string
	MemoryID string
	Value    string
	Source   Source
	// Evidence 是支持这个值的证据引用（任务 ID、步骤引用）。
	Evidence   []string
	Confidence float64
	CreatedAt  time.Time
	// SupersededBy 指向取代它的版本，非空表示已失效。
	SupersededBy string
	// OverrideReason 说明为什么被取代，如 user_explicit。
	OverrideReason string
}

// 覆盖原因的取值。
const (
	// OverrideUserExplicit 用户在被提示矛盾后仍坚持使用新值。
	OverrideUserExplicit = "user_explicit"
	// OverrideNewEvidence 新证据充分，自动更新。
	OverrideNewEvidence = "new_evidence"
	// OverrideExpired 旧值已过时效。
	OverrideExpired = "expired"
)

// Memory 是一条长期记忆。ID 稳定，值随 Revision 演进。
type Memory struct {
	ID string

	Kind Kind
	// Subject 是记忆的主体："user" 或某个实体 ID。
	Subject string
	// Domain 是领域，如 "出行/打车"。
	Domain string
	// Attribute 是属性名，实体档案用，如 "忌口"。
	Attribute string
	// Condition 仅对偏好记忆有意义。
	Condition Condition

	Status      Status
	Temporality Temporality
	ValidFrom   time.Time
	ValidUntil  *time.Time

	Confidence float64
	HitCount   int
	LastHitAt  *time.Time

	// SafetyCritical 标记安全相关属性（过敏、禁忌、疾病、财务红线）。
	//
	// 这类记忆走旁路强制注入，不参与相似度打分也不参与预算裁剪——
	// 相似度召回失败一次就是事故。
	SafetyCritical bool

	Tags []string

	// Current 是当前生效的版本。
	Current Revision
	// History 是被取代的版本，按时间升序。
	History []Revision

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ErrInvalidMemory 表示记忆字段不合法。
var ErrInvalidMemory = errors.New("memory: 记忆字段不合法")

// Validate 校验一条记忆。
func (m *Memory) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("%w: ID 为空", ErrInvalidMemory)
	}
	if !m.Kind.Valid() {
		return fmt.Errorf("%w: 未知的记忆类型 %q", ErrInvalidMemory, m.Kind)
	}
	if m.Subject == "" {
		return fmt.Errorf("%w: Subject 为空", ErrInvalidMemory)
	}
	if !m.Temporality.Valid() {
		return fmt.Errorf("%w: 未判定时效类型——不存在"+
			"默认永久这回事", ErrInvalidMemory)
	}
	// 限时记忆没有过期时间等于永久记忆，三个月后它就是一条错误信息。
	if m.Temporality == TemporalLimited && m.ValidUntil == nil {
		return fmt.Errorf("%w: 限时记忆必须有 ValidUntil", ErrInvalidMemory)
	}
	if m.Confidence < 0 || m.Confidence > 1 {
		return fmt.Errorf("%w: 置信度必须在 [0,1]，实际 %v", ErrInvalidMemory, m.Confidence)
	}
	if m.Current.Value == "" {
		return fmt.Errorf("%w: 当前版本没有值", ErrInvalidMemory)
	}
	if m.Kind == KindEntity && m.Attribute == "" {
		return fmt.Errorf("%w: 实体档案必须指明属性名", ErrInvalidMemory)
	}
	return nil
}

// Expired 报告记忆在给定时刻是否已过期。
func (m *Memory) Expired(now time.Time) bool {
	return m.ValidUntil != nil && !now.Before(*m.ValidUntil)
}

// Retrievable 报告这条记忆在给定时刻是否应参与检索。
func (m *Memory) Retrievable(now time.Time) bool {
	if m.Status == StatusArchived || m.Status == StatusExpired {
		return false
	}
	if m.Expired(now) {
		return false
	}
	return !now.Before(m.ValidFrom)
}

// Key 是记忆的身份键：同一个键上的新值构成版本演进，而不是新增一条。
//
// 偏好的条件参与身份：{场景:通勤}→顺风车 和 {场景:接待客户}→豪华型
// 是两条并存的记忆，不是互相冲突的一条。
func (m *Memory) Key() string {
	parts := []string{string(m.Kind), m.Subject, m.Domain, m.Attribute}
	if m.Kind == KindPreference {
		parts = append(parts, m.Condition.String())
	}
	return strings.Join(parts, "|")
}

// Supersede 用新值取代当前版本，旧版本转入历史。
//
// 刻意不提供"删除旧值"的路径：用户可以推翻助手的记忆，
// 但推翻过程本身必须可回溯，否则误覆盖就再也救不回来了。
func (m *Memory) Supersede(next Revision, reason string, now time.Time) {
	old := m.Current
	old.SupersededBy = next.ID
	old.OverrideReason = reason
	m.History = append(m.History, old)

	next.MemoryID = m.ID
	if next.CreatedAt.IsZero() {
		next.CreatedAt = now
	}
	m.Current = next
	m.Confidence = next.Confidence
	m.UpdatedAt = now
	if m.Status == StatusDisputed {
		m.Status = StatusActive
	}
}

// RecordHit 记录一次命中，用于检索打分与遗忘机制。
func (m *Memory) RecordHit(now time.Time) {
	m.HitCount++
	m.LastHitAt = &now
}

// New 构造一条记忆。调用方仍需设置 Temporality 等字段并调用 Validate。
func New(kind Kind, subject, domain string, value string, src Source, now time.Time) *Memory {
	id := core.NewID()
	return &Memory{
		ID:         id,
		Kind:       kind,
		Subject:    subject,
		Domain:     domain,
		Status:     StatusActive,
		ValidFrom:  now,
		Confidence: initialConfidence(src),
		Current: Revision{
			ID:         core.NewID(),
			MemoryID:   id,
			Value:      value,
			Source:     src,
			Confidence: initialConfidence(src),
			CreatedAt:  now,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// initialConfidence 按来源给出初始置信度。
//
// 用户亲口说的最可信；从行为观察推断的最不可信——
// 连续点了三次川菜不代表"喜欢川菜"，可能只是公司楼下就那一家。
func initialConfidence(src Source) float64 {
	switch src.Kind {
	case SourceUser:
		return 0.9
	case SourceConnector:
		return 0.85
	case SourceTask:
		return 0.7
	case SourceObserved:
		return 0.4
	default:
		return 0.5
	}
}
