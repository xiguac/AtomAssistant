package pg_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// TestExportImport 模拟迁移服务器：导出、清库、导入，数据应原样回来。
// 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestExportImport(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过导出导入测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer s.Close()
	reset := func() {
		t.Helper()
		if err := s.ResetForTest(ctx); err != nil {
			t.Fatalf("重置失败: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
	}
	reset()

	// ---- 旧服务器上的数据 ----
	now := time.Now().UTC().Truncate(time.Millisecond)
	m := memory.New(memory.KindPreference, "user", "出行", "优先高铁",
		memory.Source{Kind: memory.SourceUser, Ref: "app", Trust: core.TrustUser}, now)
	m.Temporality = memory.TemporalDurable
	m.Condition = memory.Condition{"场景": "国内出行"}
	if err := s.Memories().Put(ctx, m); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}

	task := core.NewTask("收集签证材料", "分几天收齐", core.TrustUser)
	task.Usage = core.Usage{Tokens: 1234, CostCents: 3, CostMicros: 4200, WallClock: 90 * time.Second}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	step := core.NewStep(task.ID, 1, core.StepTool, json.RawMessage(`{"items":[{"title":"身份证","status":"done"}]}`))
	step.ToolName = "plan.update"
	step.SideEffect = core.SideEffectNone
	step.Complete(json.RawMessage(`{"ok":true}`), core.TrustToolOutput)
	if err := s.AppendStep(ctx, step); err != nil {
		t.Fatalf("写入步骤失败: %v", err)
	}
	inboxed := event.New(event.TopicUserMessage, json.RawMessage(`{"text":"户口本稍后给你"}`), core.TrustUser, "atomctl")
	if err := s.Events().SaveForTask(ctx, inboxed, task.ID); err != nil {
		t.Fatalf("写入收件箱失败: %v", err)
	}

	export := func(sections ...pg.Section) []byte {
		t.Helper()
		var buf bytes.Buffer
		if err := s.Export(ctx, sections, &buf); err != nil {
			t.Fatalf("导出失败: %v", err)
		}
		return buf.Bytes()
	}
	memFile := export(pg.SectionMemory)
	allFile := export(pg.Sections()...)
	if strings.Contains(string(memFile), task.ID) {
		t.Fatal("只导出记忆板块时不应带出任务数据")
	}

	// ---- 新服务器：空库，只导入记忆板块 ----
	reset()
	rep, err := s.Import(ctx, bytes.NewReader(memFile))
	if err != nil {
		t.Fatalf("导入记忆失败: %v", err)
	}
	if rep.Inserted["memories"] != 1 || rep.Inserted["memory_revisions"] != 1 {
		t.Fatalf("记忆板块应导入 1 条记忆 1 个版本: %+v", rep.Inserted)
	}
	got, err := s.Memories().Get(ctx, m.ID)
	if err != nil || got.Current.Value != "优先高铁" || got.Condition["场景"] != "国内出行" {
		t.Fatalf("记忆未原样恢复: %v, %+v", err, got)
	}
	if _, err := s.GetTask(ctx, task.ID); err == nil {
		t.Fatal("没导入任务板块，任务不应出现")
	}

	// ---- 再导入全部：已有的记忆跳过，其余补齐 ----
	rep, err = s.Import(ctx, bytes.NewReader(allFile))
	if err != nil {
		t.Fatalf("导入全部失败: %v", err)
	}
	if rep.Inserted["memories"] != 0 || rep.Inserted["tasks"] != 1 || rep.Inserted["task_steps"] != 1 {
		t.Fatalf("已存在的应跳过、缺的应补齐: %+v", rep.Inserted)
	}
	gotTask, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("任务未恢复: %v", err)
	}
	if gotTask.Usage != task.Usage || gotTask.Title != task.Title || gotTask.Origin != core.TrustUser {
		t.Fatalf("任务字段未原样恢复: %+v", gotTask)
	}
	steps, err := s.ListSteps(ctx, task.ID)
	if err != nil || len(steps) != 1 || steps[0].ToolName != "plan.update" || steps[0].Status != core.StepDone {
		t.Fatalf("步骤日志未恢复: %v, %+v", err, steps)
	}
	// 迁移途中用户补的材料不能丢：收件箱要跟着任务一起过来。
	inbox, err := s.Events().InboxOf(ctx, task.ID)
	if err != nil || len(inbox) != 1 || inbox[0].ID != inboxed.ID {
		t.Fatalf("收件箱未恢复: %v, %d 条", err, len(inbox))
	}

	// 同一个文件导两次是安全的。
	rep, err = s.Import(ctx, bytes.NewReader(allFile))
	if err != nil {
		t.Fatalf("重复导入失败: %v", err)
	}
	for table, n := range rep.Inserted {
		if n != 0 {
			t.Fatalf("重复导入不应新增数据，%s 新增了 %d 行", table, n)
		}
	}

	// 不是我们的文件，或者目标库比导出时旧，都必须拒绝。
	if _, err := s.Import(ctx, strings.NewReader(`{"format":"别的东西"}`)); err == nil {
		t.Fatal("格式不对的文件应被拒绝")
	}
	newer := strings.Replace(string(allFile), `"migrations":[`, `"migrations":["9999_future.sql",`, 1)
	if _, err := s.Import(ctx, strings.NewReader(newer)); err == nil || !strings.Contains(err.Error(), "9999_future.sql") {
		t.Fatalf("目标库缺迁移时应拒绝导入并指出缺哪个: %v", err)
	}
}
