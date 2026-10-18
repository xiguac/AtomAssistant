package media

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// needFFmpeg 在没有 ffmpeg 的机器上跳过：这些转换要靠它。
func needFFmpeg(t *testing.T) {
	t.Helper()
	if !Default.HasFFmpeg() {
		t.Skip("没有 ffmpeg，跳过音视频转换测试")
	}
}

// sampleVideo 用 ffmpeg 生成一段 2 秒、带音轨的 mp4。
func sampleVideo(t *testing.T) []byte {
	t.Helper()
	bin, _ := Default.ffmpeg()
	out := filepath.Join(t.TempDir(), "v.mp4")
	cmd := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "mpeg4", "-c:a", "aac", "-shortest", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v %s", err, msg)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFFmpeg_各家要的语音格式都转得出来(t *testing.T) {
	needFFmpeg(t)
	ctx := context.Background()
	wav := WAV(tone(16000, 1500), 16000)

	opus, ms, err := Default.Opus(ctx, wav)
	if err != nil || Sniff(opus) != MIMEOgg || ms < 1400 || ms > 1600 {
		t.Fatalf("飞书要的 opus: %s %d %v", Sniff(opus), ms, err)
	}
	amr, ms, err := Default.AMR(ctx, wav)
	if err != nil || Sniff(amr) != MIMEAMR || ms < 1400 || ms > 1600 {
		t.Fatalf("钉钉要的 amr: %s %d %v", Sniff(amr), ms, err)
	}
	silk, ms, err := Default.Silk(ctx, wav)
	if err != nil || Sniff(silk) != MIMESilk || ms < 1400 || ms > 1600 {
		t.Fatalf("QQ 要的 silk: %s %d %v", Sniff(silk), ms, err)
	}
	// 飞书发来的 opus 交给模型前转成 wav；转出来的 silk 也能再解回来。
	for name, in := range map[string][]byte{"opus": opus, "silk": silk} {
		out, mime, err := Default.ForModel(ctx, in)
		if err != nil || mime != MIMEWAV || Sniff(out) != MIMEWAV {
			t.Fatalf("%s 应转成 wav 给模型: %s %v", name, mime, err)
		}
	}
	// amr 模型直接认，原样给。
	if out, mime, err := Default.ForModel(ctx, amr); err != nil || mime != MIMEAMR || !bytes.Equal(out, amr) {
		t.Fatalf("amr 应原样给模型: %s %v", mime, err)
	}
}

func TestFFmpeg_视频取得到封面和时长(t *testing.T) {
	needFFmpeg(t)
	ctx := context.Background()
	video := sampleVideo(t)
	if Sniff(video) != MIMEMP4 || KindOf("x.bin", video) != KindVideo {
		t.Fatalf("应认出是 mp4 视频: %s", Sniff(video))
	}
	cover, err := Default.Cover(ctx, video)
	if err != nil || Sniff(cover) != "image/jpeg" {
		t.Fatalf("封面应是 jpeg: %v", err)
	}
	if secs, err := Default.VideoSeconds(ctx, video); err != nil || secs != 2 {
		t.Fatalf("时长应是 2 秒: %d %v", secs, err)
	}
}
