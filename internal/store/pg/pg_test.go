package pg_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/store"
	"github.com/xiguac/atomassistant/internal/store/pg"
	"github.com/xiguac/atomassistant/internal/store/storetest"
)

// TestPostgresStore契约 需要一个真实数据库。
//
// 设置 ATOM_TEST_DSN 后运行，例如：
//
//	docker run -d --name atomassistant-pg -e POSTGRES_PASSWORD=atom \
//	  -e POSTGRES_DB=atomassistant -p 55432:5432 postgres:16-alpine
//	ATOM_TEST_DSN='postgres://postgres:atom@localhost:55432/atomassistant' go test ./...
//
// 每个子用例用独立 schema，互不干扰。
func TestPostgresStore契约(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过 Postgres 契约测试")
	}

	storetest.RunSuite(t, func(t *testing.T) store.TaskStore {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		t.Cleanup(cancel)

		s, err := pg.Open(ctx, dsn, seal.TestBox())
		if err != nil {
			t.Fatalf("连接数据库失败: %v", err)
		}
		t.Cleanup(s.Close)

		if err := s.ResetForTest(ctx); err != nil {
			t.Fatalf("重置数据库失败: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		return s
	})
}
