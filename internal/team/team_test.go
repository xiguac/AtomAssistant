package team

import (
	"context"
	"testing"
	"time"
)

func put(t *testing.T, s Store, task string, seq int, worker string, c Category) string {
	t.Helper()
	w := &Work{ID: WorkID(task, seq, 0), TaskID: task, StepSeq: seq, Worker: worker, Category: c, CreatedAt: time.Now()}
	if err := s.PutWork(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	return w.ID
}

func pick(t *testing.T, b *Board, c Category) string {
	t.Helper()
	w, err := b.Pick(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return w.Name
}

func TestBoard_先试后比再按用户的评价派活(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	b := NewBoard(s, []Worker{{Name: "甲"}, {Name: "乙"}, {Name: "译员", GoodAt: []Category{CatTranslate}}})

	// 谁都没做过：按配置的顺序先给第一个。划了范围的不接范围之外的活。
	if got := pick(t, b, CatWriting); got != "甲" {
		t.Fatalf("都没做过时应派给配置里靠前的，实际 %s", got)
	}
	first := put(t, s, "t1", 1, "甲", CatWriting)
	// 甲做过了、乙还没有：让乙也试一次，哪怕甲刚拿了好评。
	if err := s.Rate(ctx, first, Verdict(true, RatedByUser), RatedByUser); err != nil {
		t.Fatal(err)
	}
	if got := pick(t, b, CatWriting); got != "乙" {
		t.Fatalf("没做过这一类的应先试一次，实际 %s", got)
	}
	second := put(t, s, "t2", 1, "乙", CatWriting)
	// 都做过之后按成绩：甲有好评，乙没人评价。
	if got := pick(t, b, CatWriting); got != "甲" {
		t.Fatalf("应派给成绩高的，实际 %s", got)
	}
	// 成绩是分类别算的：写作上的好评不影响编程。
	if got := pick(t, b, CatCode); got != "甲" {
		t.Fatalf("另一个类别应从头算起，实际 %s", got)
	}

	// 甲后来连着两次被说不行，乙拿了好评：换人。
	for i, task := range []string{"t3", "t4"} {
		id := put(t, s, task, i+1, "甲", CatWriting)
		if err := s.Rate(ctx, id, Verdict(false, RatedByUser), RatedByUser); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Rate(ctx, second, Verdict(true, RatedByUser), RatedByUser); err != nil {
		t.Fatal(err)
	}
	if got := pick(t, b, CatWriting); got != "乙" {
		t.Fatalf("差评多了应换成成绩高的，实际 %s", got)
	}

	// 翻译有专门接它的，也有什么都接的；没做过的先试。
	rows, err := b.Standings(ctx, CatTranslate)
	if err != nil || len(rows) != 3 || !rows[0].Next {
		t.Fatalf("翻译应有三个候选: %+v %v", rows, err)
	}
	if rows, _ := b.Standings(ctx, CatWriting); len(rows) != 2 {
		t.Fatalf("只接翻译的不应出现在写作里: %+v", rows)
	}
	if _, err := b.Pick(ctx, "胡写的类别"); err == nil {
		t.Fatal("不存在的类别应报错")
	}
}

func TestBoard_同一类按难度派给不同的模型(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	b := NewBoard(s, []Worker{
		{Name: "快手", Tier: TierSimple},
		{Name: "强手", Tier: TierComplex},
		{Name: "写手", GoodAt: []Category{CatWriting}, Tier: TierSimple},
	})
	pickTier := func(c Category, tier Tier, exclude ...string) string {
		t.Helper()
		w, err := b.PickExcept(ctx, c, tier, exclude)
		if err != nil {
			t.Fatal(err)
		}
		return w.Name
	}
	if got := pickTier(CatAnalysis, TierSimple); got != "快手" {
		t.Fatalf("简单的应派给接简单的，实际 %s", got)
	}
	if got := pickTier(CatAnalysis, TierComplex); got != "强手" {
		t.Fatalf("复杂的应派给接复杂的，实际 %s", got)
	}
	// 这一档的人都试过了：由这一类里别的模型顶上。
	if got := pickTier(CatAnalysis, TierComplex, "强手"); got != "快手" {
		t.Fatalf("这一档没人了应由同类别的别的模型接，实际 %s", got)
	}
	// 成绩榜上两档各标出一个优先的。
	rows, err := b.Standings(ctx, CatAnalysis)
	if err != nil {
		t.Fatal(err)
	}
	next := map[string][]Tier{}
	for _, r := range rows {
		if r.Next {
			next[r.Worker.Name] = r.NextFor
		}
	}
	if len(next) != 2 || len(next["快手"]) != 1 || next["快手"][0] != TierSimple || next["强手"][0] != TierComplex {
		t.Fatalf("两档应各有一个优先的: %+v", next)
	}
	if _, err := b.PickExcept(ctx, CatAnalysis, "medium", nil); err == nil {
		t.Fatal("不存在的难度应报错")
	}
}

func TestStore_推断的评价分量轻且不覆盖用户的(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id := put(t, s, "t1", 1, "甲", CatWriting)

	if err := s.Rate(ctx, id, Verdict(false, RatedByInferred), RatedByInferred); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Tally(ctx)
	if len(all) != 1 || all[0].Bad != WeightInferred || all[0].Runs != 1 {
		t.Fatalf("推断的差评应只算半票: %+v", all)
	}
	// 用户明确表态：以用户的为准。
	if err := s.Rate(ctx, id, Verdict(true, RatedByUser), RatedByUser); err != nil {
		t.Fatal(err)
	}
	// 之后再推断出别的，不能把用户的评价盖掉。
	if err := s.Rate(ctx, id, Verdict(false, RatedByInferred), RatedByInferred); err != nil {
		t.Fatal(err)
	}
	all, _ = s.Tally(ctx)
	if all[0].Good != WeightUser || all[0].Bad != 0 {
		t.Fatalf("用户的评价不应被推断覆盖: %+v", all)
	}
	// 同一步重跑再记一次：不多算一份，也不丢评价。
	put(t, s, "t1", 1, "甲", CatWriting)
	all, _ = s.Tally(ctx)
	if all[0].Runs != 1 || all[0].Good != WeightUser {
		t.Fatalf("重复记录不应重复计数: %+v", all)
	}
	if got := (Tally{}).Score(); got != 0.5 {
		t.Fatalf("没有评价时成绩应为 0.5，实际 %v", got)
	}
	if err := s.Rate(ctx, "不存在", 1, RatedByUser); err == nil {
		t.Fatal("给不存在的活打分应报错")
	}
}
