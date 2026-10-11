package pg_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/schedule"
	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// TestScheduleStore 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestScheduleStore(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过定时任务存储测试")
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
	store := s.Schedules()

	start := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	sc := schedule.New("早报", "看一眼今天的天气和日程", 24*time.Hour, start)
	if err := store.Add(ctx, sc); err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	// 过密的间隔在数据库这一层也写不进去。
	dense := schedule.New("刷屏", "", time.Second, start)
	if err := store.Add(ctx, dense); err == nil {
		t.Fatal("短于一分钟的间隔应被拒绝")
	}

	runner := schedule.NewRunner(store, s, core.DefaultBudget())
	n, err := runner.Tick(ctx, time.Now().UTC())
	if err != nil || n != 1 {
		t.Fatalf("到点应创建 1 个任务: %d, %v", n, err)
	}
	list, err := store.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("列出失败: %v, %d 条", err, len(list))
	}
	got := list[0]
	if got.LastTaskID == "" || !got.NextRunAt.Equal(start.Add(24*time.Hour)) || got.Every != 24*time.Hour {
		t.Fatalf("触发后的状态不对: %+v", got)
	}
	task, err := s.GetTask(ctx, got.LastTaskID)
	if err != nil {
		t.Fatalf("任务未创建: %v", err)
	}
	if task.State != core.StateReady || task.Origin != core.TrustUser || task.Title != "早报" {
		t.Fatalf("建出的任务不对: %+v", task)
	}
	// 建出来的任务能被调度器正常领走。
	claimed, err := s.ClaimReady(ctx, "w1", time.Minute, 5)
	if err != nil || len(claimed) != 1 || claimed[0].ID != task.ID {
		t.Fatalf("任务应可被领取: %v, %d 个", err, len(claimed))
	}

	// 两个实例看到同一条到点的定时任务：只有拿着正确旧时刻的那个能推进。
	stale := core.NewTask("早报", "", core.TrustUser)
	stale.State = core.StateReady
	ok, err := store.Fire(ctx, sc.ID, start, start.Add(48*time.Hour), stale)
	if err != nil || ok {
		t.Fatalf("时刻已被推进时不应再触发: %v, %v", ok, err)
	}
	if _, err := s.GetTask(ctx, stale.ID); err == nil {
		t.Fatal("没推进成功就不该留下任务")
	}

	if err := store.Remove(ctx, sc.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if list, _ := store.List(ctx); len(list) != 0 {
		t.Fatal("删除后不应再列出")
	}
}
