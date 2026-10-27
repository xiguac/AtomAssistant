package pg_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/files"
	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/share"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// TestShareStore 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestShareStore(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过分享链接存储测试")
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

	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	link := func(id string, ttl time.Duration) *share.Link {
		return &share.Link{ID: id, TaskID: "task-1", Path: "图", Dir: true,
			Files:     []files.Pinned{{Path: "图/a.png", Size: 5, SHA256: "aa"}, {Path: "图/b.png", Size: 6, SHA256: "bb"}},
			CreatedAt: now, ExpiresAt: now.Add(ttl)}
	}
	if err := s.CreateShare(ctx, link("s1", time.Hour), "h1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateShare(ctx, link("s2", 2*time.Hour), "h2"); err != nil {
		t.Fatal(err)
	}
	// 库里也挡着超过 24 小时的。
	if err := s.CreateShare(ctx, link("s3", 25*time.Hour), "h3"); err == nil {
		t.Fatal("超过 24 小时的链接应存不进去")
	}

	got, err := s.ShareByToken(ctx, "h1", now)
	if err != nil || got.ID != "s1" || got.Path != "图" || !got.Dir || len(got.Files) != 2 || got.Files[1].SHA256 != "bb" {
		t.Fatalf("按令牌找不对: %v %+v", err, got)
	}
	if _, err := s.ShareByToken(ctx, "没有", now); !errors.Is(err, share.ErrNotFound) {
		t.Fatalf("没有的令牌应报 ErrNotFound: %v", err)
	}
	if _, err := s.ShareByToken(ctx, "h1", now.Add(time.Hour)); !errors.Is(err, share.ErrNotFound) {
		t.Fatalf("到期的链接应找不到: %v", err)
	}
	if ok, err := s.RevokeShare(ctx, "s2", now); err != nil || !ok {
		t.Fatalf("撤销失败: %v %v", ok, err)
	}
	if ok, err := s.RevokeShare(ctx, "s2", now); err != nil || ok {
		t.Fatalf("撤销过的不应再报有效: %v %v", ok, err)
	}
	if _, err := s.ShareByToken(ctx, "h2", now); !errors.Is(err, share.ErrNotFound) {
		t.Fatalf("撤销的链接应找不到: %v", err)
	}
	active, err := s.ActiveShares(ctx, now)
	if err != nil || len(active) != 1 || active[0].ID != "s1" {
		t.Fatalf("有效链接不对: %v %d", err, len(active))
	}
	// 清理：撤销的和到期的都删掉。
	if err := s.DeleteExpiredShares(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if active, err := s.ActiveShares(ctx, now); err != nil || len(active) != 0 {
		t.Fatalf("清理之后不应还有: %v %d", err, len(active))
	}
}
