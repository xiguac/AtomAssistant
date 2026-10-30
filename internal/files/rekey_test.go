package files

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xiguac/atomassistant/internal/seal"
)

func TestRekeyFiles(t *testing.T) {
	ws, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := seal.TestBox()
	ws.WithSeal(old, true)
	src := filepath.Join(t.TempDir(), "备忘.txt")
	if err := os.WriteFile(src, []byte("护照号 E12345678"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Import(src, "备忘.txt"); err != nil {
		t.Fatal(err)
	}
	// 再写一次，让旧的那份进 .versions 留底：留底的也要换。
	if _, err := ws.Import(src, "备忘.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root, "明文.txt"), []byte("没加密"), 0o600); err != nil {
		t.Fatal(err)
	}

	cur, err := seal.NewBox([]byte("another-key-another-key-another!"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := ws.RekeyFiles(old, cur)
	if err != nil || n < 2 {
		t.Fatalf("应当至少换了正文和留底两个文件: n=%d err=%v", n, err)
	}
	ws.WithSeal(cur, true)
	got, err := ws.content(filepath.Join(ws.Root, "备忘.txt"))
	if err != nil || string(got) != "护照号 E12345678" {
		t.Fatalf("换了之后用新密钥读不出来: %q, %v", got, err)
	}
	if plain := onDisk(t, ws, "明文.txt"); string(plain) != "没加密" {
		t.Fatalf("明文文件不该被动: %q", plain)
	}
	if again, err := ws.RekeyFiles(old, cur); err != nil || again != 0 {
		t.Fatalf("重跑应当什么都不做: n=%d err=%v", again, err)
	}
}
