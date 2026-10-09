package memory

import (
	"fmt"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
)

// ConflictKind 是冲突的类型。分类型处置，因为它们该有不同的默认结果。
type ConflictKind string

const (
	// ConflictNone 新值与旧值一致，不是冲突——是佐证。
	ConflictNone ConflictKind = "none"
	// ConflictTemporalSuccession 时效更替：住址从 A 变 B。旧值曾经是对的。
	ConflictTemporalSuccession ConflictKind = "temporal_succession"
	// ConflictContradiction 真实矛盾：上月"张总忌海鲜"，本月"张总点了海鲜"。
	ConflictContradiction ConflictKind = "contradiction"
	// ConflictSourceAuthority 来源可信度不同：用户口述 vs 权威数据。
	ConflictSourceAuthority ConflictKind = "source_authority"
	// ConflictRefinement 新值是旧值的补充：旧的说法原样还在，只是多了细节。
	// "住在杭州" → "住在杭州，出行常从杭州东站走"。这不是矛盾。
	ConflictRefinement ConflictKind = "refinement"
	// ConflictObservationVsStatement 观察与陈述矛盾：
	// 用户说不吃辣，但订单记录连续点川菜。
	ConflictObservationVsStatement ConflictKind = "observation_vs_statement"
)

// Resolution 是仲裁结果。
type Resolution string

const (
	// ResolveReinforce 新值与旧值一致，提升置信度。
	ResolveReinforce Resolution = "reinforce"
	// ResolveUpdate 新证据充分，直接更新，事后在汇总里告知用户。
	ResolveUpdate Resolution = "update"
	// ResolveKeepBoth 证据相当，保留双值并标记分歧，下次相关任务前询问。
	ResolveKeepBoth Resolution = "keep_both"
	// ResolveChallenge 旧记忆有强佐证或涉及安全属性，主动向用户对质。
	ResolveChallenge Resolution = "challenge"
	// ResolveHoldAsCandidate 新值来源不可信，只能挂为候选，不能影响现状。
	ResolveHoldAsCandidate Resolution = "hold_as_candidate"
)

// Evidence 是交叉验证找到的一条佐证或反驳。
type Evidence struct {
	// Ref 指向证据来源，如任务 ID。
	Ref string
	// Summary 是人类可读的说明，会出现在对质话术里。
	Summary string
	// At 是证据产生的时间。
	At time.Time
	// Supports 为真表示支持既有记忆，为假表示支持新值。
	Supports bool
	// Strong 标记强证据，如"客户本人当场确认"。
	Strong bool
}

// Arbitration 是一次仲裁的完整结论。
type Arbitration struct {
	Kind       ConflictKind
	Resolution Resolution
	Reason     string
	// Evidence 是参与判断的交叉验证结果。
	Evidence []Evidence
	// Challenge 是需要向用户对质时的话术。必须点明矛盾和出处。
	Challenge string
}

// Candidate 是一条待写入的记忆候选。
type Candidate struct {
	Kind      Kind
	Subject   string
	Domain    string
	Attribute string
	Condition Condition
	Value     string

	Temporality Temporality
	ValidUntil  *time.Time

	SafetyCritical bool
	Tags           []string
	Source         Source
	Evidence       []string
}

// Inferred 报告一条候选是不是助手自己从任务里推断出来的。
//
// 来源是任务提炼、信任等级不低于工具输出的才算。读过邮件、网页的任务里提炼出来的东西
// 会被标成外部内容等级，不算推断——那可能是别人想让助手"记住"的话。
func (c *Candidate) Inferred() bool {
	return c.Source.Kind == SourceTask && c.Source.Trust >= core.TrustToolOutput
}

// Key 与 Memory.Key 保持一致，用于定位同一条记忆。
func (c *Candidate) Key() string {
	parts := []string{string(c.Kind), c.Subject, c.Domain, c.Attribute}
	if c.Kind == KindPreference {
		parts = append(parts, c.Condition.String())
	}
	return strings.Join(parts, "|")
}

// Arbitrate 判定新候选与既有记忆的关系。
//
// 判定顺序是刻意的：先看来源资格，再看是否一致，再看时效更替，
// 最后才进入真实矛盾的证据比较。安全属性在任何情况下都要对质。
func Arbitrate(existing *Memory, cand Candidate, evidence []Evidence, now time.Time) Arbitration {
	// 0. 助手自己的推断。它不是用户说的，但也不是外面来的东西——
	//    与旧记忆一致就是一次印证；不一致时，只许它改动同样出自推断的旧值，
	//    碰到用户说过的、权威数据同步来的，一律留给用户核实。
	if cand.Inferred() {
		same := normalize(existing.Current.Value) == normalize(cand.Value) || contains(existing.Current.Value, cand.Value)
		switch {
		case same:
			return Arbitration{Kind: ConflictNone, Resolution: ResolveReinforce, Evidence: evidence,
				Reason: "助手的推断与既有记忆一致，算作一次印证"}
		case existing.SafetyCritical || cand.SafetyCritical:
			return Arbitration{Kind: ConflictContradiction, Resolution: ResolveHoldAsCandidate, Evidence: evidence,
				Reason: "涉及安全相关属性，助手的推断必须由用户确认"}
		case existing.Inferred():
			return Arbitration{Kind: ConflictRefinement, Resolution: ResolveUpdate, Evidence: evidence,
				Reason: "新的推断取代旧的推断，旧值转入历史"}
		default:
			return Arbitration{Kind: ConflictContradiction, Resolution: ResolveHoldAsCandidate, Evidence: evidence,
				Reason:    "助手的推断与既有记忆不一致，需要向用户核实",
				Challenge: buildChallenge(existing, cand, evidence)}
		}
	}

	// 1. 不可信来源永远只能当候选。截图里写"张总不忌海鲜"不能改动记忆。
	if !cand.Source.Trust.CanWriteMemoryDirectly() {
		return Arbitration{
			Kind:       ConflictContradiction,
			Resolution: ResolveHoldAsCandidate,
			Evidence:   evidence,
			Reason: fmt.Sprintf("来源信任等级为 %s，只能作为候选进入人工确认",
				cand.Source.Trust),
		}
	}

	// 2. 值一致：这不是冲突，是佐证。
	if normalize(existing.Current.Value) == normalize(cand.Value) {
		return Arbitration{
			Kind:       ConflictNone,
			Resolution: ResolveReinforce,
			Evidence:   evidence,
			Reason:     "新观察与既有记忆一致，提升置信度",
		}
	}

	// 3. 安全属性永远对质。过敏、禁忌、疾病、财务红线判错一次就是事故，
	//    宁可多打断用户一次。
	if existing.SafetyCritical || cand.SafetyCritical {
		return Arbitration{
			Kind:       ConflictContradiction,
			Resolution: ResolveChallenge,
			Evidence:   evidence,
			Reason:     "涉及安全相关属性，必须由用户确认",
			Challenge:  buildChallenge(existing, cand, evidence),
		}
	}

	// 4. 补充而非矛盾。只比较文字是否相同的话，每一次"说得更细一点"
	//    都会被当成冲突挂起，用户很快就会被自己说过的话淹没。
	switch {
	case contains(existing.Current.Value, cand.Value):
		// 新说法只是旧说法的一部分，没有带来新东西。
		return Arbitration{
			Kind:       ConflictNone,
			Resolution: ResolveReinforce,
			Evidence:   evidence,
			Reason:     "新观察已包含在既有记忆里，提升置信度",
		}
	case contains(cand.Value, existing.Current.Value) &&
		authority(cand.Source.Kind) >= authority(existing.Current.Source.Kind):
		// 来源不如旧值权威的"补充"不走这条：不能让一次任务里的推断悄悄改写用户亲口说的话。
		return Arbitration{
			Kind:       ConflictRefinement,
			Resolution: ResolveUpdate,
			Evidence:   evidence,
			Reason:     "新值是对既有记忆的补充，旧值转入历史",
		}
	}

	// 5. 时效更替：旧值曾经是对的，只是过时了。这类不该被当成"矛盾"去打扰用户。
	if isSuccession(existing, cand, now) {
		return Arbitration{
			Kind:       ConflictTemporalSuccession,
			Resolution: ResolveUpdate,
			Evidence:   evidence,
			Reason:     "属于时效更替，旧值转入历史",
		}
	}

	// 6. 观察与陈述矛盾：两者都可能对，行为和口述本来就会不一致。
	if isObservationVsStatement(existing, cand) {
		return Arbitration{
			Kind:       ConflictObservationVsStatement,
			Resolution: ResolveKeepBoth,
			Evidence:   evidence,
			Reason:     "用户陈述与行为观察不一致，保留两条并在下次相关任务前求证",
			Challenge:  buildChallenge(existing, cand, evidence),
		}
	}

	// 7. 来源权威度差距明显时直接采信更权威的一方。
	switch {
	case authority(cand.Source.Kind) > authority(existing.Current.Source.Kind):
		return Arbitration{
			Kind:       ConflictSourceAuthority,
			Resolution: ResolveUpdate,
			Evidence:   evidence,
			Reason: fmt.Sprintf("新值来自更权威的来源（%s 优于 %s）",
				cand.Source.Kind, existing.Current.Source.Kind),
		}
	case authority(cand.Source.Kind) < authority(existing.Current.Source.Kind):
		return Arbitration{
			Kind:       ConflictSourceAuthority,
			Resolution: ResolveChallenge,
			Evidence:   evidence,
			Reason: fmt.Sprintf("新值来源（%s）不如既有记忆（%s）权威",
				cand.Source.Kind, existing.Current.Source.Kind),
			Challenge: buildChallenge(existing, cand, evidence),
		}
	}

	// 8. 同等权威的真实矛盾，交给交叉验证的证据强度决定。
	support, oppose, strongSupport := weigh(evidence)
	switch {
	case strongSupport:
		// 旧记忆有强佐证（如"客户本人当场确认"），不能默默被推翻。
		return Arbitration{
			Kind:       ConflictContradiction,
			Resolution: ResolveChallenge,
			Evidence:   evidence,
			Reason:     "既有记忆有强证据支持，需用户裁决",
			Challenge:  buildChallenge(existing, cand, evidence),
		}
	case oppose > support:
		return Arbitration{
			Kind:       ConflictContradiction,
			Resolution: ResolveUpdate,
			Evidence:   evidence,
			Reason:     "交叉验证更支持新值",
		}
	default:
		return Arbitration{
			Kind:       ConflictContradiction,
			Resolution: ResolveKeepBoth,
			Evidence:   evidence,
			Reason:     "证据相当，保留两条并标记分歧",
			Challenge:  buildChallenge(existing, cand, evidence),
		}
	}
}

// buildChallenge 生成对质话术。
//
// 关键是**点明矛盾和出处**，而不是含糊地问"你确定吗"。
// 用户需要知道助手是根据什么记住的，才能判断该不该推翻。
func buildChallenge(existing *Memory, cand Candidate, evidence []Evidence) string {
	var sb strings.Builder
	subject := existing.Subject
	if existing.Attribute != "" {
		subject += "的" + existing.Attribute
	}
	fmt.Fprintf(&sb, "记录里%s是「%s」", subject, existing.Current.Value)

	if src := describeSource(existing.Current); src != "" {
		fmt.Fprintf(&sb, "（%s）", src)
	}
	fmt.Fprintf(&sb, "。现在的信息是「%s」。", cand.Value)

	for _, e := range evidence {
		if e.Supports && e.Strong {
			fmt.Fprintf(&sb, "\n另外 %s 也支持原记录：%s。",
				e.At.Local().Format("1月2日"), e.Summary)
			break
		}
	}
	sb.WriteString("\n是情况变了，还是我记错了？")
	return sb.String()
}

func describeSource(r Revision) string {
	date := r.CreatedAt.Local().Format("2006年1月2日")
	switch r.Source.Kind {
	case SourceUser:
		return date + "你告诉我的"
	case SourceTask:
		return date + "助手自己推断的，未经你确认"
	case SourceConnector:
		return date + "从已授权数据同步的"
	case SourceObserved:
		return date + "从你的行为中观察到的"
	case SourceBuiltin:
		return "内置流程"
	}
	return date
}

// isSuccession 判断是否属于时效更替而非真实矛盾。
//
// 判据：旧值已经过期，或者旧值是"长期可变"类型且新值来自可靠来源。
// 住址变更、职位变动都走这条，它们不该打扰用户。
func isSuccession(existing *Memory, cand Candidate, now time.Time) bool {
	if existing.Expired(now) {
		return true
	}
	if existing.Temporality != TemporalDurable {
		return false
	}
	return cand.Source.Kind == SourceUser || cand.Source.Kind == SourceConnector
}

// isObservationVsStatement 判断是否属于"用户怎么说"与"用户怎么做"的矛盾。
//
// 只在口述与观察之间成立。权威数据与观察的矛盾不算这类——
// 日历里白纸黑字写着有会，就不该因为"通常这个时段空闲"而保留双值，
// 那种情况归权威度比较处理。
func isObservationVsStatement(existing *Memory, cand Candidate) bool {
	a, b := existing.Current.Source.Kind, cand.Source.Kind
	return (a == SourceObserved && b == SourceUser) ||
		(a == SourceUser && b == SourceObserved)
}

// authority 是来源的权威度。权威数据 > 用户口述 > 任务提炼 > 行为观察。
func authority(k SourceKind) int {
	switch k {
	case SourceConnector:
		return 4
	case SourceUser:
		return 3
	case SourceTask:
		return 2
	case SourceObserved:
		return 1
	}
	return 0
}

func weigh(evidence []Evidence) (support, oppose int, strongSupport bool) {
	for _, e := range evidence {
		if e.Supports {
			support++
			if e.Strong {
				strongSupport = true
			}
			continue
		}
		oppose++
	}
	return support, oppose, strongSupport
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// contains 报告 whole 是否原样包含 part（忽略大小写与空白）。
//
// 这是个保守的判据：它认得出"在原话后面加了细节"，认不出换了说法的补充——
// 后者仍会走到矛盾处理，由用户裁决。宁可多问一次，也不靠猜。
func contains(whole, part string) bool {
	squash := func(s string) string { return strings.Join(strings.Fields(normalize(s)), "") }
	w, p := squash(whole), squash(part)
	return p != "" && w != p && strings.Contains(w, p)
}

// UserOverride 处理用户在被对质后仍坚持新值的情形。
//
// 新值立即生效并覆盖，但旧值转入版本链而不是被删除；
// 用户显式指定的置信度置为 1.0，且此后同类冲突不再重复对质——
// 否则助手会变成复读机。
func UserOverride(m *Memory, value string, now time.Time) {
	m.Supersede(Revision{
		ID:         core.NewID(),
		Value:      value,
		Source:     Source{Kind: SourceUser, Trust: core.TrustUser},
		Confidence: 1.0,
		CreatedAt:  now,
	}, OverrideUserExplicit, now)
	m.Confidence = 1.0
}
