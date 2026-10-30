package pg_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/oplog"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/store"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// TestOpLogs 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestOpLogs(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN")
	}
	ctx := context.Background()
	s, err := pg.Open(ctx, dsn, seal.TestBox())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ResetForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC().Add(-time.Hour)
	var batch []oplog.Entry
	for i := 0; i < 5; i++ {
		batch = append(batch, oplog.Entry{At: base.Add(time.Duration(i) * time.Minute), Level: slog.LevelInfo,
			Category: oplog.Admin, Message: "后台操作", Attrs: "操作=保存模型服务 第=" + string(rune('0'+i))})
	}
	batch = append(batch, oplog.Entry{At: base.Add(10 * time.Minute), Level: slog.LevelError, Category: oplog.Message,
		Message: "发送消息失败", Attrs: "渠道=qq"})
	if err := s.AppendLogs(ctx, batch); err != nil {
		t.Fatal(err)
	}

	// 新的在前；内容原样取回。
	all, err := s.ListLogs(ctx, oplog.Query{})
	if err != nil || len(all) != 6 || all[0].Message != "发送消息失败" || all[0].Category != oplog.Message || all[0].Level != slog.LevelError {
		t.Fatalf("取全部日志不对: %+v %v", all, err)
	}
	// 按类别、按级别筛。
	if got, _ := s.ListLogs(ctx, oplog.Query{Category: oplog.Admin}); len(got) != 5 || got[0].Attrs != "操作=保存模型服务 第=4" {
		t.Fatalf("按类别筛不对: %+v", got)
	}
	if got, _ := s.ListLogs(ctx, oplog.Query{MinLevel: slog.LevelWarn}); len(got) != 1 {
		t.Fatalf("按级别筛不对: %+v", got)
	}
	// 往前翻页。
	page, _ := s.ListLogs(ctx, oplog.Query{Category: oplog.Admin, Limit: 2})
	older, _ := s.ListLogs(ctx, oplog.Query{Category: oplog.Admin, Limit: 2, Before: page[1].At})
	if len(page) != 2 || len(older) != 2 || !older[0].At.Before(page[1].At) {
		t.Fatalf("翻页不对: %+v %+v", page, older)
	}

	// 审计同时记进操作日志：工具调用归"工具调用"，其余归"任务"。
	task := core.NewTask("t", "g", core.TrustUser)
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, store.AuditEntry{Actor: task.ID, Action: "tool.call", TaskID: task.ID, Detail: "calendar.create: 成功"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, store.AuditEntry{Actor: "user", Action: "approval.granted", TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	tools, _ := s.ListLogs(ctx, oplog.Query{Category: oplog.Tool})
	tasks, _ := s.ListLogs(ctx, oplog.Query{Category: oplog.Task})
	if len(tools) != 1 || tools[0].Message != "调用工具" || len(tasks) != 1 || tasks[0].Message != "已批准" {
		t.Fatalf("审计没有记进操作日志: %+v %+v", tools, tasks)
	}

	// 过期的删掉：后台操作类按自己的期限算。
	if n, err := s.PruneLogs(ctx, base.Add(time.Hour), base.Add(3*time.Minute)); err != nil || n != 4 {
		t.Fatalf("应删掉 3 条过期的后台操作和 1 条过期的消息: %d %v", n, err)
	}

	// 按范围删除：后台操作类只是不再显示，其余的真删掉。
	if err := s.AppendLogs(ctx, []oplog.Entry{{At: base.Add(5 * time.Minute), Level: slog.LevelInfo, Category: oplog.Message, Message: "收到消息"}}); err != nil {
		t.Fatal(err)
	}
	if d, h, err := s.DeleteLogs(ctx, oplog.Message, base, base.Add(11*time.Minute)); err != nil || d != 1 || h != 0 {
		t.Fatalf("只删消息类: %d %d %v", d, h, err)
	}
	if d, h, err := s.DeleteLogs(ctx, "", base, base.Add(11*time.Minute)); err != nil || d != 0 || h != 2 {
		t.Fatalf("后台操作类应只隐藏: %d %d %v", d, h, err)
	}
	if got, _ := s.ListLogs(ctx, oplog.Query{Category: oplog.Admin}); len(got) != 0 {
		t.Fatalf("隐藏的不应再列出: %+v", got)
	}
	if got, _ := s.ListLogs(ctx, oplog.Query{}); len(got) != 2 {
		t.Fatalf("范围外的不受影响: %+v", got)
	}
	// 隐藏的仍在库里，期满才删。
	if n, err := s.PruneLogs(ctx, base, base.Add(6*time.Minute)); err != nil || n != 2 {
		t.Fatalf("隐藏的后台操作应留到期满再删: %d %v", n, err)
	}
}
