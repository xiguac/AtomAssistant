package memory_test

import (
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/memory/memstore"
)

func TestArchiveExpired_限时记忆过期一阵后归档_其余不动(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)
	day := 24 * time.Hour
	now := time.Now().UTC()
	src := memory.Source{Kind: memory.SourceUser, Trust: core.TrustUser}
	limited := func(attr string, until time.Time) memory.Candidate {
		return memory.Candidate{Kind: memory.KindFact, Subject: "user", Domain: "行程", Attribute: attr, Value: attr,
			Temporality: memory.TemporalLimited, ValidUntil: &until, Source: src}
	}
	out, err := p.Ingest(ctx(), []memory.Candidate{
		limited("早已过期", now.Add(-60*day)),
		limited("刚过期", now.Add(-10*day)),
		limited("还没到期", now.Add(10*day)),
		{Kind: memory.KindFact, Subject: "user", Domain: "居住", Value: "常住南京", Temporality: memory.TemporalDurable, Source: src},
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := p.ArchiveExpired(ctx(), 30*day)
	if err != nil || n != 1 {
		t.Fatalf("只有过期超过 30 天的该归档: n=%d err=%v", n, err)
	}
	want := []memory.Status{memory.StatusArchived, memory.StatusActive, memory.StatusActive, memory.StatusActive}
	for i, o := range out {
		m, _ := st.Get(ctx(), o.MemoryID)
		if m.Status != want[i] {
			t.Errorf("第 %d 条状态应为 %s，实际 %s", i, want[i], m.Status)
		}
	}
	// 归档不是删除：原文还在，查得到。
	if m, _ := st.Get(ctx(), out[0].MemoryID); m.Current.Value != "早已过期" {
		t.Error("归档后原文应保留")
	}
}
