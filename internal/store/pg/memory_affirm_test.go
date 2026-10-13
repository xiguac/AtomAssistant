package pg_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// 用户确认一条推断之后，它的来源要真的存下来：否则读出来仍是"推断"，后台的确认按钮永远消不掉。
// 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestMemory_确认与合并之后的来源存得下来(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过记忆确认测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := pg.Open(ctx, dsn, seal.TestBox())
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer s.Close()
	if err := s.ResetForTest(ctx); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	store := s.Memories()
	pipe := memory.NewPipeline(store, nil)
	add := func(domain, value string) string {
		t.Helper()
		out, err := pipe.Ingest(ctx, []memory.Candidate{{
			Kind: memory.KindFact, Subject: "user", Domain: domain, Value: value, Temporality: memory.TemporalDurable,
			Source: memory.Source{Kind: memory.SourceTask, Ref: "t1", Trust: core.TrustToolOutput}, Evidence: []string{"task:" + domain},
		}})
		if err != nil || len(out) != 1 || out[0].MemoryID == "" {
			t.Fatalf("写入失败: %+v %v", out, err)
		}
		return out[0].MemoryID
	}
	a, b, c := add("饮食", "不太吃辣"), add("饮食/口味", "点菜时让少放辣"), add("作息", "习惯早起")

	if m, err := store.Get(ctx, c); err != nil || !m.Inferred() {
		t.Fatalf("刚记下的应是推断: %v", err)
	}
	if err := pipe.Affirm(ctx, c); err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	// 从库里重新读出来，而不是看内存里的那一份。
	m, err := store.Get(ctx, c)
	if err != nil || m.Inferred() || m.Current.Source.Kind != memory.SourceUser || m.Current.Source.Trust != core.TrustUser || m.Confidence < 0.9 {
		t.Fatalf("确认之后应转到用户名下并存下来: %+v %v", m, err)
	}
	// 确认过的不再参与合并；剩下两条并成一条之后仍是推断，被并掉的不再出现在列表里。
	if list, err := pipe.Mergeable(ctx); err != nil || len(list) != 2 {
		t.Fatalf("确认过的不应再可合并: %d %v", len(list), err)
	}
	keep, err := pipe.MergeInferred(ctx, []string{a, b}, "不太吃辣，点菜时会让少放辣")
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	m, err = store.Get(ctx, keep)
	if err != nil || !m.Inferred() || m.Current.Value != "不太吃辣，点菜时会让少放辣" || len(m.History) != 1 || len(m.Current.Evidence) != 2 {
		t.Fatalf("合并后的记忆没有存对: %+v %v", m, err)
	}
	if list, err := store.List(ctx, memory.Query{}); err != nil || len(list) != 2 {
		t.Fatalf("被并掉的应退出列表: %d %v", len(list), err)
	}
}
