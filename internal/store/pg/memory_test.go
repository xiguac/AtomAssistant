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

// TestMemoryStore 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestMemoryStore(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过记忆存储测试")
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
	ms := s.Memories()
	now := time.Now().UTC().Truncate(time.Millisecond)

	src := memory.Source{Kind: memory.SourceUser, Ref: "app", Trust: core.TrustUser}
	m := memory.New(memory.KindPreference, "user", "出行/打车", "顺风车", src, now)
	m.Temporality = memory.TemporalDurable
	m.Condition = memory.Condition{"场景": "个人通勤"}
	m.Tags = []string{"domain:出行"}

	if err := ms.Put(ctx, m); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got, err := ms.Get(ctx, m.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Current.Value != "顺风车" || got.Condition["场景"] != "个人通勤" {
		t.Fatalf("字段丢失: %+v", got)
	}
	if got.Temporality != memory.TemporalDurable {
		t.Fatalf("时效丢失: %s", got.Temporality)
	}

	byKey, err := ms.FindByKey(ctx, m.Key())
	if err != nil || byKey.ID != m.ID {
		t.Fatalf("按身份键查找失败: %v", err)
	}

	// 版本链：旧值必须留在库里。
	memory.UserOverride(got, "豪华型", now.Add(time.Hour))
	if err := ms.Put(ctx, got); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	after, err := ms.Get(ctx, m.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if after.Current.Value != "豪华型" {
		t.Fatalf("新值未生效: %s", after.Current.Value)
	}
	if len(after.History) != 1 || after.History[0].Value != "顺风车" {
		t.Fatalf("版本链未持久化: %+v", after.History)
	}
	if after.History[0].OverrideReason != memory.OverrideUserExplicit {
		t.Fatalf("覆盖原因未持久化: %s", after.History[0].OverrideReason)
	}

	// 限时记忆缺少过期时间会被数据库约束挡住。
	bad := memory.New(memory.KindFact, "user", "财务", "预算 2000", src, now)
	bad.Temporality = memory.TemporalLimited
	bad.ValidUntil = &now
	bad.ValidUntil = nil
	if err := ms.Put(ctx, bad); err == nil {
		t.Fatal("限时记忆缺少过期时间应被拒绝")
	}

	// 待裁决候选。
	pend := memory.Pending{
		ID: core.NewID(),
		Candidate: memory.Candidate{
			Kind: memory.KindFact, Subject: "user", Value: "报销上限 5000",
			Temporality: memory.TemporalDurable, Source: src,
		},
		Arb:       memory.Arbitration{Kind: memory.ConflictNone, Resolution: memory.ResolveHoldAsCandidate},
		CreatedAt: now,
	}
	if err := ms.PutPending(ctx, pend); err != nil {
		t.Fatalf("写入候选失败: %v", err)
	}
	list, err := ms.ListPending(ctx, "user")
	if err != nil || len(list) != 1 {
		t.Fatalf("读取候选失败: %v, %d 条", err, len(list))
	}
	if list[0].Candidate.Value != "报销上限 5000" {
		t.Fatalf("候选内容丢失: %+v", list[0].Candidate)
	}
	if err := ms.ResolvePending(ctx, pend.ID, now); err != nil {
		t.Fatalf("裁决失败: %v", err)
	}
	if list, _ := ms.ListPending(ctx, "user"); len(list) != 0 {
		t.Fatal("已裁决的候选不应再出现")
	}

	// 检索过滤。
	all, err := ms.List(ctx, memory.Query{Subject: "user", Kinds: []memory.Kind{memory.KindPreference}})
	if err != nil || len(all) != 1 {
		t.Fatalf("按类型检索失败: %v, %d 条", err, len(all))
	}
	if tagged, _ := ms.List(ctx, memory.Query{Subject: "user", Tags: []string{"domain:出行"}}); len(tagged) != 1 {
		t.Fatalf("按标签检索失败，%d 条", len(tagged))
	}
	if none, _ := ms.List(ctx, memory.Query{Subject: "user", Tags: []string{"不存在"}}); len(none) != 0 {
		t.Fatal("不匹配的标签不应返回结果")
	}
}
