package pg_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/session"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// TestSessionStore 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestSessionStore(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过会话存储测试")
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
	store := s.Sessions()
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	put := func(day string, at time.Time, role session.Role, text string) {
		t.Helper()
		if err := store.Append(ctx, session.Message{Day: day, At: at, Role: role, Channel: "qq", Text: text}); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
	}
	put("2026-10-19", base.Add(-24*time.Hour), session.RoleUser, "前天说的")
	put("2026-10-20", base, session.RoleUser, "订周六的票")
	put("2026-10-20", base.Add(time.Minute), session.RoleAssistant, "订好了")
	put("2026-10-20", base.Add(2*time.Minute), session.RoleUser, "谢谢")

	t.Run("按时间读回一天的消息，只取最后几条", func(t *testing.T) {
		all, err := store.Messages(ctx, "2026-10-20", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 3 || all[0].Text != "订周六的票" || all[1].Role != session.RoleAssistant || all[2].Text != "谢谢" {
			t.Fatalf("读回的不对: %+v", all)
		}
		last, err := store.Messages(ctx, "2026-10-20", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(last) != 2 || last[0].Text != "订好了" || last[1].Text != "谢谢" {
			t.Fatalf("应只取最后两条，早的在前: %+v", last)
		}
	})

	t.Run("没写摘要的日子写完就不再列出", func(t *testing.T) {
		days, err := store.Unsummarized(ctx, "2026-10-21")
		if err != nil {
			t.Fatal(err)
		}
		if len(days) != 2 || days[0] != "2026-10-19" || days[1] != "2026-10-20" {
			t.Fatalf("没写摘要的日子不对: %v", days)
		}
		if err := store.PutSummary(ctx, session.Summary{Day: "2026-10-19", Text: "聊了天气"}); err != nil {
			t.Fatal(err)
		}
		// 当天还没过完，不列。
		days, err = store.Unsummarized(ctx, "2026-10-20")
		if err != nil {
			t.Fatal(err)
		}
		if len(days) != 0 {
			t.Fatalf("当天和写过摘要的都不该列出: %v", days)
		}
		if err := store.PutSummary(ctx, session.Summary{Day: "2026-10-19", Text: "聊了天气和车票"}); err != nil {
			t.Fatal(err)
		}
		sums, err := store.Summaries(ctx, "2026-10-01", "2026-10-31")
		if err != nil {
			t.Fatal(err)
		}
		if len(sums) != 1 || sums[0].Text != "聊了天气和车票" {
			t.Fatalf("摘要应被覆盖: %+v", sums)
		}
	})
}
