package pg_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/style"
)

// TestStyleStore 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestStyleStore(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过风格存储测试")
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
	store := s.Style()

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	add := func(scene, text string, at time.Time) bool {
		t.Helper()
		ok, err := store.AddSample(ctx, &style.Sample{
			ID: fmt.Sprintf("%s-%d", scene, at.UnixNano()), Scene: scene, Text: text, Ref: "task:x", CreatedAt: at,
		})
		if err != nil {
			t.Fatalf("写入失败: %v", err)
		}
		return ok
	}
	for i := 0; i < 5; i++ {
		if !add(style.SceneChat, fmt.Sprintf("第 %d 句", i), base.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("第 %d 句应写入", i)
		}
	}
	// 一模一样的话不重复存；换个场景则各算各的。
	if add(style.SceneChat, "第 0 句", base.Add(time.Minute)) {
		t.Fatal("同一场景里重复的话不应再存一条")
	}
	if !add(style.SceneMail, "第 0 句", base.Add(time.Minute)) {
		t.Fatal("不同场景的同一句话应各存一条")
	}

	recent, err := store.RecentSamples(ctx, style.SceneChat, 2)
	if err != nil || len(recent) != 2 || recent[0].Text != "第 4 句" || recent[1].Text != "第 3 句" || recent[0].Ref != "task:x" {
		t.Fatalf("应按时间倒序返回最近的: %v %+v", err, recent)
	}
	if n, err := store.CountSamples(ctx, style.SceneChat, base.Add(2*time.Second)); err != nil || n != 2 {
		t.Fatalf("之后的样本数应为 2: %d %v", n, err)
	}

	if err := store.TrimSamples(ctx, style.SceneChat, 3); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	left, _ := store.RecentSamples(ctx, style.SceneChat, 10)
	if len(left) != 3 || left[2].Text != "第 2 句" {
		t.Fatalf("应只留最近三条: %+v", left)
	}
	if n, _ := store.CountSamples(ctx, style.SceneMail, time.Time{}); n != 1 {
		t.Fatal("清理一个场景不应动另一个场景")
	}

	if p, err := store.GetProfile(ctx, style.SceneChat); p != nil || err != nil {
		t.Fatalf("没提炼过时应返回 nil: %v %v", p, err)
	}
	p := &style.Profile{Scene: style.SceneChat, Description: "句子很短", UpTo: base, UpdatedAt: base}
	if err := store.PutProfile(ctx, p); err != nil {
		t.Fatalf("写入概述失败: %v", err)
	}
	p.Description, p.UpTo = "句子很短，不寒暄", base.Add(time.Minute)
	if err := store.PutProfile(ctx, p); err != nil {
		t.Fatalf("更新概述失败: %v", err)
	}
	got, err := store.GetProfile(ctx, style.SceneChat)
	if err != nil || got.Description != "句子很短，不寒暄" || !got.UpTo.Equal(base.Add(time.Minute)) {
		t.Fatalf("概述应被整条替换: %+v %v", got, err)
	}

	// 学到的风格随记忆板块一起导出导入。
	var buf bytes.Buffer
	if err := s.Export(ctx, []pg.Section{pg.SectionMemory}, &buf, "export passphrase"); err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if err := store.Clear(ctx); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if n, _ := store.CountSamples(ctx, style.SceneChat, time.Time{}); n != 0 {
		t.Fatal("清空后不应还有样本")
	}
	if p, _ := store.GetProfile(ctx, style.SceneChat); p != nil {
		t.Fatal("清空后不应还有概述")
	}
	rep, err := s.Import(ctx, &buf, "export passphrase")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if rep.Inserted["style_samples"] != 4 || rep.Inserted["style_profiles"] != 1 {
		t.Fatalf("风格数据应原样导回: %+v", rep.Inserted)
	}
	if back, _ := store.GetProfile(ctx, style.SceneChat); back == nil || back.Description != "句子很短，不寒暄" {
		t.Fatalf("导回的概述不对: %+v", back)
	}
}
