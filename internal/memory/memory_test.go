package memory_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/memory/memstore"
)

func ctx() context.Context { return context.Background() }

func userSource() memory.Source {
	return memory.Source{Kind: memory.SourceUser, Ref: "app", Trust: core.TrustUser}
}

func taskSource() memory.Source {
	return memory.Source{Kind: memory.SourceTask, Ref: "task#1", Trust: core.TrustConnector}
}

// ---- 偏好的条件化建模 ----

func TestCondition_特异度与匹配(t *testing.T) {
	def := memory.Condition{}
	commute := memory.Condition{"场景": "个人通勤"}
	host := memory.Condition{"场景": "接待客户", "时段": "晚上"}

	scene := memory.Condition{"场景": "接待客户", "时段": "晚上", "城市": "上海"}
	if !def.Matches(scene) {
		t.Fatal("空条件是默认规则，应匹配任何场景")
	}
	if commute.Matches(scene) {
		t.Fatal("场景不符的条件不应匹配")
	}
	if !host.Matches(scene) {
		t.Fatal("场景多出的维度不应影响匹配")
	}
	if host.Specificity() <= commute.Specificity() {
		t.Fatal("条件越多特异度越高")
	}
}

func TestResolvePreference_接待客户盖过默认便宜(t *testing.T) {
	st := memstore.New()
	now := time.Now().UTC()

	// 这是那个打车例子：不能存成"用户喜欢便宜的车"。
	cheap := memory.New(memory.KindPreference, "user", "出行/打车", "顺风车，优先价格", userSource(), now)
	cheap.Temporality = memory.TemporalDurable
	cheap.Condition = memory.Condition{}

	luxury := memory.New(memory.KindPreference, "user", "出行/打车", "豪华型，优先体验", userSource(), now)
	luxury.Temporality = memory.TemporalDurable
	luxury.Condition = memory.Condition{"场景": "接待客户"}

	for _, m := range []*memory.Memory{cheap, luxury} {
		if err := st.Put(ctx(), m); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	r := memory.NewRetriever(st, nil)

	got, err := r.ResolvePreference(ctx(), "user", "出行/打车", memory.Condition{"场景": "个人通勤"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(got.Current.Value, "顺风车") {
		t.Fatalf("通勤场景应命中默认规则，实际 %s", got.Current.Value)
	}

	got, err = r.ResolvePreference(ctx(), "user", "出行/打车", memory.Condition{"场景": "接待客户"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 关键：更具体的条件必须盖过默认规则，而不是被"便宜"压住。
	if !strings.Contains(got.Current.Value, "豪华") {
		t.Fatalf("接待客户应命中豪华型，实际 %s", got.Current.Value)
	}
}

func TestPreference_不同条件是并存而非冲突(t *testing.T) {
	cheap := memory.Candidate{
		Kind: memory.KindPreference, Subject: "user", Domain: "出行/打车",
		Condition: memory.Condition{"场景": "个人通勤"}, Value: "顺风车",
		Temporality: memory.TemporalDurable, Source: userSource(),
	}
	luxury := cheap
	luxury.Condition = memory.Condition{"场景": "接待客户"}
	luxury.Value = "豪华型"

	// 身份键不同 → 是两条记忆，不会互相覆盖。
	if cheap.Key() == luxury.Key() {
		t.Fatal("条件不同的偏好应有不同的身份键")
	}

	st := memstore.New()
	p := memory.NewPipeline(st, nil)
	if _, err := p.Ingest(ctx(), []memory.Candidate{cheap, luxury}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	all, _ := st.List(ctx(), memory.Query{Subject: "user"})
	if len(all) != 2 {
		t.Fatalf("应并存两条偏好，实际 %d 条", len(all))
	}
}

// ---- 时效性 ----

func TestValidate_限时记忆必须有过期时间(t *testing.T) {
	c := memory.Candidate{
		Kind: memory.KindFact, Subject: "user", Value: "本月预算还剩 2000",
		Temporality: memory.TemporalLimited, Source: userSource(),
	}
	if err := c.Validate(); err == nil {
		t.Fatal("限时记忆缺少过期时间应被拒绝")
	}
	at := time.Now().UTC().Add(720 * time.Hour)
	c.ValidUntil = &at
	if err := c.Validate(); err != nil {
		t.Fatalf("补上过期时间后应通过: %v", err)
	}
}

func TestValidate_未判定时效被拒(t *testing.T) {
	c := memory.Candidate{
		Kind: memory.KindFact, Subject: "user", Value: "x", Source: userSource(),
	}
	if err := c.Validate(); !errors.Is(err, memory.ErrInvalidCandidate) {
		t.Fatalf("没有默认永久这回事，应报错，实际 %v", err)
	}
}

func TestMaintain_过期记忆退出检索但可回溯(t *testing.T) {
	st := memstore.New()
	past := time.Now().UTC().Add(-time.Hour)
	m := memory.New(memory.KindFact, "user", "财务", "本月预算还剩 2000", userSource(), past.Add(-time.Hour))
	m.Temporality = memory.TemporalLimited
	m.ValidUntil = &past
	if err := st.Put(ctx(), m); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	p := memory.NewPipeline(st, nil)
	rep, err := p.Maintain(ctx(), "user", memory.DecayOptions{})
	if err != nil {
		t.Fatalf("维护失败: %v", err)
	}
	if rep.Expired != 1 {
		t.Fatalf("应标记 1 条过期，实际 %d", rep.Expired)
	}

	active, _ := st.List(ctx(), memory.Query{Subject: "user"})
	if len(active) != 0 {
		t.Fatal("过期记忆不应出现在主检索里")
	}
	// 过期不等于删除：还要能回答"我上个月预算是多少"。
	all, _ := st.List(ctx(), memory.Query{Subject: "user", IncludeInactive: true})
	if len(all) != 1 {
		t.Fatal("过期记忆应保留以供回溯")
	}
}

func TestMaintain_安全属性永不归档(t *testing.T) {
	st := memstore.New()
	old := time.Now().UTC().Add(-3 * 365 * 24 * time.Hour)

	safety := memory.New(memory.KindEntity, "客户张总", "餐饮", "海鲜过敏", taskSource(), old)
	safety.Attribute = "忌口"
	safety.Temporality = memory.TemporalPermanent
	safety.SafetyCritical = true
	safety.Confidence = 0.1
	safety.UpdatedAt = old

	junk := memory.New(memory.KindFact, "user", "杂项", "某个无关紧要的事", taskSource(), old)
	junk.Temporality = memory.TemporalDurable
	junk.Confidence = 0.1
	junk.UpdatedAt = old

	for _, m := range []*memory.Memory{safety, junk} {
		if err := st.Put(ctx(), m); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	p := memory.NewPipeline(st, nil)
	if _, err := p.Maintain(ctx(), "user", memory.DecayOptions{}); err != nil {
		t.Fatalf("维护失败: %v", err)
	}
	if _, err := p.Maintain(ctx(), "客户张总", memory.DecayOptions{}); err != nil {
		t.Fatalf("维护失败: %v", err)
	}

	got, _ := st.Get(ctx(), safety.ID)
	if got.Status == memory.StatusArchived {
		t.Fatal("安全相关属性永不归档")
	}
	gotJunk, _ := st.Get(ctx(), junk.ID)
	if gotJunk.Status != memory.StatusArchived {
		t.Fatalf("低置信长期未命中的记忆应被归档，实际 %s", gotJunk.Status)
	}
}

// ---- 冲突仲裁 ----

func newAllergyMemory(now time.Time) *memory.Memory {
	m := memory.New(memory.KindEntity, "客户张总", "餐饮", "海鲜过敏", taskSource(), now)
	m.Attribute = "忌口"
	m.Temporality = memory.TemporalPermanent
	m.SafetyCritical = true
	return m
}

func TestArbitrate_安全属性矛盾必须对质(t *testing.T) {
	now := time.Now().UTC()
	existing := newAllergyMemory(now.Add(-180 * 24 * time.Hour))

	cand := memory.Candidate{
		Kind: memory.KindEntity, Subject: "客户张总", Domain: "餐饮", Attribute: "忌口",
		Value: "无忌口", Temporality: memory.TemporalPermanent, Source: userSource(),
	}
	evidence := []memory.Evidence{{
		Ref: "task#88", Summary: "因客户海鲜过敏改订川菜，客户当场确认",
		At: now.Add(-150 * 24 * time.Hour), Supports: true, Strong: true,
	}}

	arb := memory.Arbitrate(existing, cand, evidence, now)
	if arb.Resolution != memory.ResolveChallenge {
		t.Fatalf("安全属性矛盾必须对质，实际 %s", arb.Resolution)
	}
	// 对质话术必须点明矛盾和出处，不能含糊地问"你确定吗"。
	if !strings.Contains(arb.Challenge, "海鲜过敏") {
		t.Fatalf("对质应说出原记录: %s", arb.Challenge)
	}
	if !strings.Contains(arb.Challenge, "当场确认") {
		t.Fatalf("对质应带上交叉验证到的强证据: %s", arb.Challenge)
	}
	if !strings.Contains(arb.Challenge, "记错") {
		t.Fatalf("对质应给用户纠正的机会: %s", arb.Challenge)
	}
}

func TestArbitrate_untrusted来源只能挂候选(t *testing.T) {
	now := time.Now().UTC()
	existing := newAllergyMemory(now)

	// 截图里写着"张总不忌海鲜"。
	cand := memory.Candidate{
		Kind: memory.KindEntity, Subject: "客户张总", Domain: "餐饮", Attribute: "忌口",
		Value: "无忌口", Temporality: memory.TemporalPermanent,
		Source: memory.Source{Kind: memory.SourceTask, Trust: core.TrustExternal},
	}
	arb := memory.Arbitrate(existing, cand, nil, now)
	if arb.Resolution != memory.ResolveHoldAsCandidate {
		t.Fatalf("不可信来源不能改动记忆，实际 %s", arb.Resolution)
	}
}

func TestArbitrate_值一致时是佐证不是冲突(t *testing.T) {
	now := time.Now().UTC()
	existing := newAllergyMemory(now)
	cand := memory.Candidate{
		Kind: memory.KindEntity, Subject: "客户张总", Domain: "餐饮", Attribute: "忌口",
		Value: "海鲜过敏", Temporality: memory.TemporalPermanent, Source: taskSource(),
	}
	arb := memory.Arbitrate(existing, cand, nil, now)
	if arb.Kind != memory.ConflictNone || arb.Resolution != memory.ResolveReinforce {
		t.Fatalf("一致的新观察应提升置信度，实际 %s/%s", arb.Kind, arb.Resolution)
	}
}

func TestArbitrate_时效更替不打扰用户(t *testing.T) {
	now := time.Now().UTC()
	m := memory.New(memory.KindFact, "user", "个人信息", "上海市浦东新区", userSource(), now.Add(-365*24*time.Hour))
	m.Attribute = "住址"
	m.Temporality = memory.TemporalDurable

	cand := memory.Candidate{
		Kind: memory.KindFact, Subject: "user", Domain: "个人信息", Attribute: "住址",
		Value: "杭州市西湖区", Temporality: memory.TemporalDurable, Source: userSource(),
	}
	arb := memory.Arbitrate(m, cand, nil, now)
	if arb.Kind != memory.ConflictTemporalSuccession || arb.Resolution != memory.ResolveUpdate {
		t.Fatalf("住址变更属于时效更替，应直接更新，实际 %s/%s", arb.Kind, arb.Resolution)
	}
}

func TestArbitrate_观察与陈述矛盾保留双值(t *testing.T) {
	now := time.Now().UTC()
	m := memory.New(memory.KindPreference, "user", "餐饮", "不吃辣",
		memory.Source{Kind: memory.SourceUser, Trust: core.TrustUser}, now)
	m.Temporality = memory.TemporalDurable

	cand := memory.Candidate{
		Kind: memory.KindPreference, Subject: "user", Domain: "餐饮",
		Value: "常点川菜", Temporality: memory.TemporalDurable,
		Source: memory.Source{Kind: memory.SourceObserved, Trust: core.TrustConnector},
	}
	arb := memory.Arbitrate(m, cand, nil, now)
	// 用户说不吃辣但订单记录连续点川菜——两者都可能对。
	if arb.Kind != memory.ConflictObservationVsStatement || arb.Resolution != memory.ResolveKeepBoth {
		t.Fatalf("观察与陈述矛盾应保留双值，实际 %s/%s", arb.Kind, arb.Resolution)
	}
}

func TestArbitrate_权威来源胜出(t *testing.T) {
	now := time.Now().UTC()
	m := memory.New(memory.KindFact, "user", "日程", "周三下午空闲",
		memory.Source{Kind: memory.SourceObserved, Trust: core.TrustConnector}, now)
	m.Temporality = memory.TemporalPermanent

	cand := memory.Candidate{
		Kind: memory.KindFact, Subject: "user", Domain: "日程", Value: "周三下午有会",
		Temporality: memory.TemporalPermanent,
		Source:      memory.Source{Kind: memory.SourceConnector, Trust: core.TrustConnector},
	}
	arb := memory.Arbitrate(m, cand, nil, now)
	if arb.Resolution != memory.ResolveUpdate {
		t.Fatalf("权威数据应胜过行为观察，实际 %s", arb.Resolution)
	}
}

// ---- 版本链与用户覆盖 ----

func TestUserOverride_旧值转入版本链而非删除(t *testing.T) {
	now := time.Now().UTC()
	m := newAllergyMemory(now.Add(-24 * time.Hour))
	原值 := m.Current.Value

	memory.UserOverride(m, "无忌口", now)

	if m.Current.Value != "无忌口" {
		t.Fatalf("新值应立即生效，实际 %s", m.Current.Value)
	}
	if m.Confidence != 1.0 {
		t.Fatalf("用户显式指定的置信度应为 1.0，实际 %v", m.Confidence)
	}
	if len(m.History) != 1 {
		t.Fatalf("旧值应转入版本链，实际 %d 条历史", len(m.History))
	}
	// 不物理删除，才可能回溯"助手当时为什么那么判断"，也才可能一键回滚误覆盖。
	if m.History[0].Value != 原值 {
		t.Fatalf("历史里应保留原值，实际 %s", m.History[0].Value)
	}
	if m.History[0].OverrideReason != memory.OverrideUserExplicit {
		t.Fatalf("应标注覆盖原因，实际 %s", m.History[0].OverrideReason)
	}
	if m.History[0].SupersededBy != m.Current.ID {
		t.Fatal("版本链应能从旧值指向新值")
	}
}

// ---- 写入管线 ----

func TestIngest_不可信来源的新记忆挂为候选(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)

	// 从一张截图里抽出来的"事实"。
	out, err := p.Ingest(ctx(), []memory.Candidate{{
		Kind: memory.KindFact, Subject: "user", Domain: "财务", Value: "报销上限 5000",
		Temporality: memory.TemporalDurable,
		Source:      memory.Source{Kind: memory.SourceTask, Trust: core.TrustExternal},
	}})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if out[0].Resolution != memory.ResolveHoldAsCandidate || out[0].PendingID == "" {
		t.Fatalf("应挂为候选，实际 %+v", out[0])
	}
	if all, _ := st.List(ctx(), memory.Query{Subject: "user"}); len(all) != 0 {
		t.Fatal("未经确认的候选不应进入记忆库")
	}
	if pend, _ := st.ListPending(ctx(), "user"); len(pend) != 1 {
		t.Fatal("候选应可被用户在 App 上看到并裁决")
	}
}

func TestIngest_一次性记忆不进长期记忆(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)
	if _, err := p.Ingest(ctx(), []memory.Candidate{{
		Kind: memory.KindFact, Subject: "user", Value: "帮我订今晚的餐厅",
		Temporality: memory.TemporalOneShot, Source: userSource(),
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if all, _ := st.List(ctx(), memory.Query{Subject: "user"}); len(all) != 0 {
		t.Fatal("一次性记忆只在任务生命周期内有意义")
	}
}

func TestIngest_重复写入提升置信度但有上限(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)
	cand := memory.Candidate{
		Kind: memory.KindPreference, Subject: "user", Domain: "出行", Value: "偏好高铁",
		Temporality: memory.TemporalDurable, Source: taskSource(),
	}
	if _, err := p.Ingest(ctx(), []memory.Candidate{cand}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	first, _ := st.FindByKey(ctx(), cand.Key())

	for i := 0; i < 20; i++ {
		// 每一次都是另一个任务带来的佐证；同一个任务说二十遍不算。
		again := cand
		again.Evidence = []string{fmt.Sprintf("task:%d", i)}
		if _, err := p.Ingest(ctx(), []memory.Candidate{again}); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	after, _ := st.FindByKey(ctx(), cand.Key())
	if after.Confidence <= first.Confidence {
		t.Fatal("重复佐证应提升置信度")
	}
	// 重复听到一件事不等于它是真的：推断靠印证涨不过上限。
	if after.Confidence > memory.InferredCeiling {
		t.Fatalf("置信度必须有上限，实际 %v", after.Confidence)
	}
}

func TestIngest_同批次按身份键去重(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)
	base := memory.Candidate{
		Kind: memory.KindFact, Subject: "user", Domain: "财务", Value: "旧值",
		Temporality: memory.TemporalDurable, Source: userSource(),
	}
	later := base
	later.Value = "新值"

	out, err := p.Ingest(ctx(), []memory.Candidate{base, later})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("同键候选应被去重，实际 %d 条", len(out))
	}
	m, _ := st.FindByKey(ctx(), base.Key())
	if m.Current.Value != "新值" {
		t.Fatalf("同键应保留最后一条，实际 %s", m.Current.Value)
	}
}

func TestIngest_冲突时标记分歧并挂候选(t *testing.T) {
	st := memstore.New()
	now := time.Now().UTC()
	existing := newAllergyMemory(now)
	if err := st.Put(ctx(), existing); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	p := memory.NewPipeline(st, nil)
	out, err := p.Ingest(ctx(), []memory.Candidate{{
		Kind: memory.KindEntity, Subject: "客户张总", Domain: "餐饮", Attribute: "忌口",
		Value: "无忌口", Temporality: memory.TemporalPermanent, Source: userSource(),
	}})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if out[0].PendingID == "" {
		t.Fatal("冲突应挂起等待用户裁决")
	}
	m, _ := st.Get(ctx(), existing.ID)
	if m.Status != memory.StatusDisputed {
		t.Fatalf("冲突中的记忆应标记分歧，实际 %s", m.Status)
	}
	// 原值在裁决前不能被改掉。
	if m.Current.Value != "海鲜过敏" {
		t.Fatalf("裁决前不应改动原值，实际 %s", m.Current.Value)
	}
}

// ---- 渐进式披露 ----

func TestAssemble_安全属性强制注入不受预算影响(t *testing.T) {
	st := memstore.New()
	now := time.Now().UTC()

	safety := newAllergyMemory(now)
	if err := st.Put(ctx(), safety); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	for i := 0; i < 50; i++ {
		m := memory.New(memory.KindFact, "客户张总", "杂项", strings.Repeat("填充", 40), taskSource(), now)
		m.Temporality = memory.TemporalDurable
		if err := st.Put(ctx(), m); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	r := memory.NewRetriever(st, nil)
	// 预算极小，且场景文本里完全没有"过敏"字样。
	asm, err := r.Assemble(ctx(), "客户张总", memory.Scene{Text: "安排一次商务晚餐"}, memory.Budget{Total: 200})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if len(asm.Critical) != 1 {
		t.Fatalf("安全属性必须强制注入，实际 %d 条", len(asm.Critical))
	}
	// 相似度召回失败一次就是事故，所以它根本不参与打分和裁剪。
	if !strings.Contains(memory.RenderFull(asm.Critical[0].Memory), "海鲜过敏") {
		t.Fatal("必达环应带原文")
	}
}

func TestAssemble_低关联降级为索引而不是丢弃(t *testing.T) {
	st := memstore.New()
	now := time.Now().UTC()
	for i := 0; i < 30; i++ {
		m := memory.New(memory.KindFact, "user", "杂项", strings.Repeat("内容", 30), taskSource(), now)
		m.Temporality = memory.TemporalDurable
		if err := st.Put(ctx(), m); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	r := memory.NewRetriever(st, nil)
	asm, err := r.Assemble(ctx(), "user", memory.Scene{Text: "随便做点什么"}, memory.Budget{Total: 2000})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if len(asm.High) == 0 {
		t.Fatal("高关联环应有内容")
	}
	// 关键：塞不下的不是被丢掉，而是降级成索引行让模型按需展开。
	if len(asm.Index) == 0 {
		t.Fatalf("应有内容降级到索引环: high=%d medium=%d index=%d dropped=%d",
			len(asm.High), len(asm.Medium), len(asm.Index), asm.Dropped)
	}
	for _, it := range asm.Index {
		line := memory.RenderIndex(it.Memory)
		if !strings.Contains(line, it.Memory.ID) {
			t.Fatalf("索引行必须带 ID，否则模型无法展开: %s", line)
		}
	}
}

func TestAssemble_实体命中排在前面(t *testing.T) {
	st := memstore.New()
	now := time.Now().UTC()

	about := memory.New(memory.KindEntity, "客户张总", "产品", "关注工业级传感器", taskSource(), now)
	about.Attribute = "关注产品线"
	about.Temporality = memory.TemporalDurable
	other := memory.New(memory.KindFact, "客户张总", "杂项", "无关紧要的事", taskSource(), now)
	other.Temporality = memory.TemporalDurable
	other.Confidence = 0.3

	for _, m := range []*memory.Memory{about, other} {
		if err := st.Put(ctx(), m); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	r := memory.NewRetriever(st, nil)
	asm, err := r.Assemble(ctx(), "客户张总", memory.Scene{
		Text: "给张总推荐产品", Entities: []string{"客户张总"}, Domain: "产品",
	}, memory.Budget{Total: 4000})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if len(asm.High) < 2 {
		t.Fatalf("两条都应进高关联环，实际 %d", len(asm.High))
	}
	if asm.High[0].Memory.ID != about.ID {
		t.Fatalf("同领域且实体命中的应排前面，实际 %s", asm.High[0].Memory.Current.Value)
	}
}

func TestExpand_展开记一次命中(t *testing.T) {
	st := memstore.New()
	now := time.Now().UTC()
	m := memory.New(memory.KindFact, "user", "杂项", "某个事实", taskSource(), now)
	m.Temporality = memory.TemporalDurable
	if err := st.Put(ctx(), m); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	r := memory.NewRetriever(st, nil)
	got, err := r.Expand(ctx(), m.ID)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if got.HitCount != 1 {
		t.Fatalf("展开应记一次命中，实际 %d", got.HitCount)
	}
	// 被证明有用的记忆下次更容易被召回。
	again, _ := st.Get(ctx(), m.ID)
	if again.HitCount != 1 {
		t.Fatalf("命中应被持久化，实际 %d", again.HitCount)
	}
}

func TestAssemble_已归档与已过期不进上下文(t *testing.T) {
	st := memstore.New()
	now := time.Now().UTC()

	archived := memory.New(memory.KindFact, "user", "杂项", "陈年旧事", taskSource(), now)
	archived.Temporality = memory.TemporalDurable
	archived.Status = memory.StatusArchived
	if err := st.Put(ctx(), archived); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	r := memory.NewRetriever(st, nil)
	asm, err := r.Assemble(ctx(), "user", memory.Scene{Text: "陈年旧事"}, memory.Budget{})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if len(asm.High)+len(asm.Medium)+len(asm.Index)+len(asm.Critical) != 0 {
		t.Fatal("已归档的记忆不应进入上下文")
	}
}

// prepared 模拟向量检索：Prepare 时按场景文字算好，给指定的那条记忆满分。
type prepared struct {
	want     string
	prepared []string
}

func (p *prepared) Score(string, *memory.Memory) float64 { return 0 }

func (p *prepared) Prepare(_ context.Context, text string) memory.Similarity {
	p.prepared = append(p.prepared, text)
	return byID(p.want)
}

type byID string

func (b byID) Score(_ string, m *memory.Memory) float64 {
	if m.ID == string(b) {
		return 1
	}
	return 0
}

func TestAssemble_相似度按场景准备一次(t *testing.T) {
	st := memstore.New()
	now := time.Now().UTC()
	var target string
	for i := 0; i < 5; i++ {
		m := memory.New(memory.KindFact, "user", "杂项"+strconv.Itoa(i), "内容"+strconv.Itoa(i), taskSource(), now)
		m.Temporality = memory.TemporalDurable
		if err := st.Put(ctx(), m); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
		if i == 3 {
			target = m.ID
		}
	}
	p := &prepared{want: target}
	asm, err := memory.NewRetriever(st, p).Assemble(ctx(), "user", memory.Scene{Text: "给我妈准备生日礼物"}, memory.Budget{Total: 2000})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if len(p.prepared) != 1 || p.prepared[0] != "给我妈准备生日礼物" {
		t.Fatalf("场景文字应只准备一次: %v", p.prepared)
	}
	if len(asm.High) == 0 || asm.High[0].Memory.ID != target {
		t.Fatal("准备好的相似度应决定排序")
	}
}
