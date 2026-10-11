package pg_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/team"
)

// TestTeamStore 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestTeamStore(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过协作记录存储测试")
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
	store := s.Team()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	put := func(task string, seq, idx int, worker string, c team.Category, at time.Time) string {
		t.Helper()
		w := &team.Work{ID: team.WorkID(task, seq, idx), TaskID: task, StepSeq: seq, Index: idx,
			Worker: worker, Category: c, Title: "活", CreatedAt: at}
		if err := store.PutWork(ctx, w); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
		return w.ID
	}
	a := put("t1", 2, 0, "甲", team.CatWriting, base)
	b := put("t1", 2, 1, "乙", team.CatCode, base)
	c := put("t2", 1, 0, "甲", team.CatWriting, base.Add(time.Minute))

	if err := store.Rate(ctx, a, team.Verdict(false, team.RatedByInferred), team.RatedByInferred); err != nil {
		t.Fatal(err)
	}
	if err := store.Rate(ctx, a, team.Verdict(true, team.RatedByUser), team.RatedByUser); err != nil {
		t.Fatal(err)
	}
	// 推断不覆盖用户的；这不算错误。
	if err := store.Rate(ctx, a, team.Verdict(false, team.RatedByInferred), team.RatedByInferred); err != nil {
		t.Fatalf("推断遇到用户的评价应安静地让开: %v", err)
	}
	if err := store.Rate(ctx, c, team.Verdict(false, team.RatedByInferred), team.RatedByInferred); err != nil {
		t.Fatal(err)
	}
	if err := store.Rate(ctx, "没有:1:0", 1, team.RatedByUser); err == nil {
		t.Fatal("给不存在的活打分应报错")
	}
	// 同一步重跑：换了人，评价留着，不多算一份。
	put("t1", 2, 1, "丙", team.CatCode, base)

	works, err := store.WorkOfTask(ctx, "t1")
	if err != nil || len(works) != 2 || works[0].ID != a || works[0].Rating != team.WeightUser ||
		works[0].RatedBy != team.RatedByUser || works[1].ID != b || works[1].Worker != "丙" {
		t.Fatalf("任务里的活读得不对: %+v %v", works, err)
	}
	latest, err := store.LatestWork(ctx, []string{"t1", "t2", "别的"})
	if err != nil || len(latest) != 1 || latest[0].ID != c {
		t.Fatalf("最近派过活的应是 t2: %+v %v", latest, err)
	}
	if none, err := store.LatestWork(ctx, []string{"别的"}); err != nil || len(none) != 0 {
		t.Fatalf("没派过活的任务应返回空: %+v %v", none, err)
	}

	all, err := store.Tally(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]team.Tally{}
	for _, x := range all {
		got[x.Worker+"/"+string(x.Category)] = x
	}
	if w := got["甲/writing"]; w.Runs != 2 || w.Good != team.WeightUser || w.Bad != team.WeightInferred {
		t.Fatalf("甲的写作累计不对: %+v", w)
	}
	if w := got["丙/code"]; w.Runs != 1 || w.Good != 0 || w.Bad != 0 {
		t.Fatalf("丙的编程累计不对: %+v", w)
	}
}
