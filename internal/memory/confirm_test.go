package memory_test

import (
	"errors"
	"testing"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/memory/memstore"
)

// 从读过外部内容的任务里提炼出来的候选：分不清是助手的推断还是别人想让它记住的话，只能等用户确认。
func distilled(kind memory.Kind, domain, value string) memory.Candidate {
	return memory.Candidate{
		Kind: kind, Subject: "user", Domain: domain, Value: value,
		Temporality: memory.TemporalDurable,
		Source:      memory.Source{Kind: memory.SourceTask, Ref: "task#9", Trust: core.TrustExternal},
	}
}

func TestConfirm_候选经用户确认后以用户名义生效(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)

	out, err := p.Ingest(ctx(), []memory.Candidate{distilled(memory.KindPlaybook, "报销流程", "1. 收集发票 2. 填单")})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if out[0].PendingID == "" || out[0].MemoryID != "" {
		t.Fatalf("外部内容里提炼的流程应先挂为候选，实际 %+v", out[0])
	}
	if list, _ := st.List(ctx(), memory.Query{}); len(list) != 0 {
		t.Fatal("确认之前不应有任何记忆生效")
	}

	id, err := p.Confirm(ctx(), out[0].PendingID)
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	m, err := st.Get(ctx(), id)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if m.Kind != memory.KindPlaybook || m.Current.Source.Trust != core.TrustUser {
		t.Fatalf("确认后的流程应是 user 等级，实际 %+v", m.Current.Source)
	}
	if pend, _ := st.ListPending(ctx(), ""); len(pend) != 0 {
		t.Fatal("已确认的候选不应再出现在待确认列表")
	}
	if _, err := p.Confirm(ctx(), out[0].PendingID); !errors.Is(err, memory.ErrPendingNotFound) {
		t.Fatalf("重复确认应返回 ErrPendingNotFound，实际 %v", err)
	}
}

func TestDismiss_否决的候选不生效(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)
	out, err := p.Ingest(ctx(), []memory.Candidate{distilled(memory.KindFact, "居住", "住在杭州")})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := p.Dismiss(ctx(), out[0].PendingID); err != nil {
		t.Fatalf("否决失败: %v", err)
	}
	if list, _ := st.List(ctx(), memory.Query{}); len(list) != 0 {
		t.Fatal("被否决的候选不应生效")
	}
	if pend, _ := st.ListPending(ctx(), ""); len(pend) != 0 {
		t.Fatal("被否决的候选不应再待确认")
	}
}

func TestArbitrate_补充不是矛盾(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)
	fact := func(value string, src memory.Source) memory.Candidate {
		return memory.Candidate{
			Kind: memory.KindFact, Subject: "user", Domain: "居住", Value: value,
			Temporality: memory.TemporalDurable, Source: src,
		}
	}
	if _, err := p.Ingest(ctx(), []memory.Candidate{fact("住在杭州", userSource())}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 同样来自用户、在原话后面加了细节：直接更新，不打扰用户。
	out, err := p.Ingest(ctx(), []memory.Candidate{fact("住在杭州，出行常从杭州东站走", userSource())})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if out[0].Arb.Kind != memory.ConflictRefinement || out[0].PendingID != "" {
		t.Fatalf("补充应直接更新而不是挂起: %+v", out[0].Arb)
	}
	m, _ := st.Get(ctx(), out[0].MemoryID)
	if m.Current.Value != "住在杭州，出行常从杭州东站走" || m.Status != memory.StatusActive {
		t.Fatalf("应采用更详细的说法且保持生效: %q %s", m.Current.Value, m.Status)
	}
	if len(m.History) != 1 || m.History[0].Value != "住在杭州" {
		t.Fatalf("旧说法应留在版本链里: %+v", m.History)
	}

	// 新说法只是已知内容的一部分：算佐证。
	out, _ = p.Ingest(ctx(), []memory.Candidate{fact("住在杭州", userSource())})
	if out[0].Resolution != memory.ResolveReinforce {
		t.Fatalf("已包含的说法应算佐证: %+v", out[0].Arb)
	}

	// 来源不如旧值权威的"补充"不能悄悄改写用户亲口说的话。
	out, _ = p.Ingest(ctx(), []memory.Candidate{
		fact("住在杭州，出行常从杭州东站走，最近打算搬去上海", taskSource())})
	if out[0].Arb.Kind == memory.ConflictRefinement || out[0].PendingID == "" {
		t.Fatalf("任务推断的补充应交给用户裁决: %+v", out[0].Arb)
	}

	// 真正的矛盾照旧挂起。
	out, _ = p.Ingest(ctx(), []memory.Candidate{fact("住在北京", userSource())})
	if out[0].Arb.Kind == memory.ConflictRefinement {
		t.Fatalf("换了地方不是补充: %+v", out[0].Arb)
	}
}
