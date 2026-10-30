package pg_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// TestTaskImages 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestTaskImages(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过图片存储测试")
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

	task := core.NewTask("看图", "看看这张图", core.TrustUser)
	other := core.NewTask("别的事", "g", core.TrustUser)
	for _, tk := range []*core.Task{task, other} {
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	// 原样存取：二进制内容一个字节都不能变。
	raw := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0x10}
	for i := 0; i < 5; i++ {
		if _, err := s.PutTaskImage(ctx, task.ID, "image/png", append(append([]byte{}, raw...), byte(i))); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := s.PutTaskImage(ctx, other.ID, "image/jpeg", []byte("JPEG")); err != nil {
		t.Fatal(err)
	}

	// 只取最近的几张，按收到的先后排；别的任务的图片取不到。
	got, err := s.TaskImages(ctx, task.ID, 3)
	if err != nil || len(got) != 3 {
		t.Fatalf("应取到最近 3 张: %d %v", len(got), err)
	}
	for i, img := range got {
		want := append(append([]byte{}, raw...), byte(i+2))
		if img.MIME != "image/png" || !bytes.Equal(img.Data, want) {
			t.Fatalf("第 %d 张不对: %s %v", i, img.MIME, img.Data)
		}
	}
	if none, err := s.TaskImages(ctx, core.NewID(), 4); err != nil || len(none) != 0 {
		t.Fatalf("没有图片的任务应取到空: %v %v", none, err)
	}

	// 语音、视频和图片存在一起，按类型分开取：取图片时不会混进语音。
	if _, err := s.PutTaskImage(ctx, task.ID, "audio/wav", []byte("RIFF-voice")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutTaskImage(ctx, task.ID, "video/mp4", []byte("mp4-video")); err != nil {
		t.Fatal(err)
	}
	if imgs, err := s.TaskImages(ctx, task.ID, 10); err != nil || len(imgs) != 5 {
		t.Fatalf("取图片时不应混进语音、视频: %d %v", len(imgs), err)
	}
	voices, err := s.TaskMedia(ctx, task.ID, llm.InputAudio, 10)
	if err != nil || len(voices) != 1 || voices[0].MIME != "audio/wav" || string(voices[0].Data) != "RIFF-voice" {
		t.Fatalf("语音取得不对: %+v %v", voices, err)
	}
	if videos, err := s.TaskMedia(ctx, task.ID, llm.InputVideo, 10); err != nil || len(videos) != 1 || videos[0].MIME != "video/mp4" {
		t.Fatalf("视频取得不对: %+v %v", videos, err)
	}
	if _, err := s.TaskMedia(ctx, task.ID, llm.InputText, 10); err == nil {
		t.Fatal("文字不是媒体，应报错")
	}
}
