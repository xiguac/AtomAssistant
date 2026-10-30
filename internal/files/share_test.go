package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiguac/atomassistant/internal/seal"
)

func TestShareable_只有助手生成的文件能分享(t *testing.T) {
	w := workspace(t)
	if _, err := w.WriteFile("沙箱输出/汇总.csv", []byte("a,b\n1,2\n")); err != nil {
		t.Fatal(err)
	}
	norm, dir, pinned, err := w.Shareable(`沙箱输出\汇总.csv`)
	if err != nil || dir || norm != "沙箱输出/汇总.csv" || len(pinned) != 1 || pinned[0].Path != norm || pinned[0].Size != 8 {
		t.Fatalf("助手生成的文件应能分享: %v %v %+v", err, dir, pinned)
	}

	// 聊天里发来的。
	got, err := w.Receive("合同.pdf", []byte("%PDF 合同"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := w.Shareable(got); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "用户发来的文件") {
		t.Fatalf("用户发来的文件不能分享: %v", err)
	}
	// 用户用 atomctl file add 放到别处的。
	src := filepath.Join(t.TempDir(), "身份证.jpg")
	if err := os.WriteFile(src, []byte("JPEG 身份证"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Import(src, "资料/身份证.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := w.Shareable("资料/身份证.jpg"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("用户放进来的文件不能分享: %v", err)
	}
	// 直接拷进工作区、没有任何记录的。
	if err := os.WriteFile(filepath.Join(w.Root, "手放的.txt"), []byte("手放的"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := w.Shareable("手放的.txt"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("没有来源记录的文件不能分享: %v", err)
	}
	// 助手生成的文件之后被换了内容：不再算助手生成的。
	if err := os.WriteFile(filepath.Join(w.Root, "沙箱输出", "汇总.csv"), []byte("换掉了"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := w.Shareable("沙箱输出/汇总.csv"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("内容被换掉的文件不能分享: %v", err)
	}
	// 用户的文件被助手覆盖成新内容，新内容是助手生成的。
	if _, err := w.WriteFile("资料/身份证.jpg", []byte("助手画的图")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := w.Shareable("资料/身份证.jpg"); err != nil {
		t.Fatalf("覆盖之后的内容是助手生成的: %v", err)
	}
}

func TestShareable_原样转手的用户文件不能分享(t *testing.T) {
	for _, encrypt := range []bool{false, true} {
		w := workspace(t)
		if encrypt {
			w.WithSeal(seal.TestBox(), true)
		}
		got, err := w.Receive("体检报告.pdf", []byte("%PDF 体检报告"))
		if err != nil {
			t.Fatal(err)
		}
		// 在沙箱里原样复制了一份：路径上是助手生成的，内容是用户的。
		if _, err := w.WriteFile("沙箱输出/1/报告.pdf", []byte("%PDF 体检报告")); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := w.Shareable("沙箱输出/1/报告.pdf"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), got) {
			t.Fatalf("encrypt=%v 原样转手的用户文件不能分享: %v", encrypt, err)
		}
		// 用户批准后复制出的副本可以。
		copied, err := w.CopyReceived(got)
		if err != nil || !strings.HasPrefix(copied, CopiesDir+"/") {
			t.Fatalf("复制失败: %v %q", err, copied)
		}
		if _, _, pinned, err := w.Shareable(copied); err != nil || len(pinned) != 1 {
			t.Fatalf("encrypt=%v 批准过的副本应能分享: %v", encrypt, err)
		}
		// 再复制一次不覆盖。
		again, err := w.CopyReceived(got)
		if err != nil || again == copied {
			t.Fatalf("第二份副本不应覆盖第一份: %v %q", err, again)
		}
		// 助手生成的文件不需要副本。
		if _, err := w.CopyReceived("沙箱输出/1/报告.pdf"); err == nil {
			// 它的内容和用户文件一样，但来源记录是助手：不给复制。
			t.Fatal("助手生成的文件不应能复制成副本")
		}
	}
}

func TestShareable_目录(t *testing.T) {
	w := workspace(t)
	for name, body := range map[string]string{"图/a.png": "PNG a", "图/子/b.png": "PNG b"} {
		if _, err := w.WriteFile(name, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	// 以 . 开头的不算在内。
	if err := os.WriteFile(filepath.Join(w.Root, "图", ".DS_Store"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	norm, dir, pinned, err := w.Shareable("图/")
	if err != nil || !dir || norm != "图" || len(pinned) != 2 || pinned[0].Path != "图/a.png" || pinned[1].Path != "图/子/b.png" {
		t.Fatalf("目录应能分享: %v %v %+v", err, dir, pinned)
	}
	// 目录里混进一个用户的文件：整个拒绝，并说出是哪个。
	if err := os.WriteFile(filepath.Join(w.Root, "图", "用户的.png"), []byte("PNG 用户"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := w.Shareable("图"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "图/用户的.png") {
		t.Fatalf("目录里有用户的文件应整个拒绝: %v", err)
	}
	if err := os.Remove(filepath.Join(w.Root, "图", "用户的.png")); err != nil {
		t.Fatal(err)
	}
	// 目录里有符号链接：不顺着走，直接拒绝。
	if err := os.Symlink(filepath.Join(w.Root, ReceivedDir), filepath.Join(w.Root, "图", "链接")); err == nil {
		if _, _, _, err := w.Shareable("图"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("目录里有符号链接应拒绝: %v", err)
		}
	}
}

func TestShareable_路径(t *testing.T) {
	w := workspace(t)
	if _, err := w.WriteFile("a.txt", []byte("a")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"", ".", "../a.txt", "x/../../a.txt", `..\a.txt`, "/etc/passwd", `C:\Windows\win.ini`, "a\x00.txt",
		originsFile, ".versions", ReceivedDir, ReceivedDir + "/../" + ReceivedDir,
	} {
		if _, _, _, err := w.Shareable(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("分享 %q: 期望 ErrInvalid，实际 %v", bad, err)
		}
	}
	if _, _, _, err := w.Shareable("没有.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的文件应报 ErrNotFound: %v", err)
	}
	// 来源记录写不了：助手不能把用户的文件记成自己生成的。
	if _, err := w.DocWrite(originsFile, "# x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("来源记录不应可写: %v", err)
	}
	if _, err := w.DocWrite("子/.隐藏.docx", "# x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("以 . 开头的路径不应可写: %v", err)
	}
}

func TestSharedBytes_内容变了就不给(t *testing.T) {
	w := workspace(t)
	if _, err := w.WriteFile("报告.txt", []byte("第一版")); err != nil {
		t.Fatal(err)
	}
	_, _, pinned, err := w.Shareable("报告.txt")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := w.SharedBytes(pinned[0]); err != nil || string(data) != "第一版" {
		t.Fatalf("应取到建链接时的内容: %v %q", err, data)
	}
	if _, err := w.WriteFile("报告.txt", []byte("第二版")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.SharedBytes(pinned[0]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("内容变了应拒绝: %v", err)
	}
	// 清单里的路径被改成别的：照样经过解析和校验。
	bad := pinned[0]
	bad.Path = "../报告.txt"
	if _, err := w.SharedBytes(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("路径不对应拒绝: %v", err)
	}
}

func TestOrigins_重新打开工作区还记得(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.DocWrite("信.docx", "# 信"); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := again.Shareable("信.docx"); err != nil {
		t.Fatalf("来源记录应落盘: %v", err)
	}
	// 来源记录不列给助手看。
	entries, err := again.List("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Path, ".") {
			t.Fatalf("不应列出 %s", e.Path)
		}
	}
}
