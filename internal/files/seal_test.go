package files

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/seal"
)

func onDisk(t *testing.T, ws *Workspace, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWorkspace_开了加密落盘就是密文(t *testing.T) {
	ws, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws.WithSeal(seal.TestBox(), true)

	if _, err := ws.DocWrite("信.docx", "# 道歉信\n\n非常抱歉，周五的会议我去不了。"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SheetWrite("账.xlsx", []SheetEdit{{Rows: &Block{Values: [][]any{{"项目", "金额"}, {"房租", 4200}}}}}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "备忘.txt")
	if err := os.WriteFile(src, []byte("护照号 E12345678"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Import(src, "备忘.txt"); err != nil {
		t.Fatal(err)
	}

	// 磁盘上的每一个文件都是密文，认不出原来的格式，也找不到内容。
	for _, rel := range []string{"信.docx", "账.xlsx", "备忘.txt"} {
		raw := onDisk(t, ws, rel)
		if !seal.IsSealedFile(raw) || bytes.Contains(raw, []byte("PK\x03\x04")) || bytes.Contains(raw, []byte("E12345678")) {
			t.Fatalf("%s 落盘后不是密文", rel)
		}
	}
	// 工作区里没有留下明文的临时文件。
	items, err := os.ReadDir(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if strings.HasPrefix(it.Name(), ".tmp-") {
			t.Fatalf("留下了临时文件 %s", it.Name())
		}
	}

	// 读它的时候是明文。
	if doc, err := ws.Read("信.docx", 0); err != nil || !strings.Contains(doc.Text, "周五的会议我去不了") {
		t.Fatalf("加密存放的文档读不出来: %v, %q", err, doc.Text)
	}
	if rng, err := ws.SheetRead("账.xlsx", "", ""); err != nil || len(rng.Rows) != 2 || rng.Rows[1][0] != "房租" {
		t.Fatalf("加密存放的表格读不出来: %v, %+v", err, rng.Rows)
	}
	if txt, err := ws.Read("备忘.txt", 0); err != nil || txt.Text != "护照号 E12345678" {
		t.Fatalf("加密存放的文本读不出来: %v", err)
	}
	// 发给用户的是解开之后的原文件。
	if raw, err := ws.Bytes("信.docx"); err != nil || !bytes.HasPrefix(raw, []byte("PK\x03\x04")) {
		t.Fatalf("取出来发给用户的应当是明文的 docx: %v", err)
	}

	// 改一个加密存放的表格：读得进来，写回去仍是密文，旧的那份留底也是密文。
	res, err := ws.SheetWrite("账.xlsx", []SheetEdit{{Rows: &Block{Start: "A3", Values: [][]any{{"水电", 300}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !seal.IsSealedFile(onDisk(t, ws, "账.xlsx")) || !seal.IsSealedFile(onDisk(t, ws, res.Saved.Replaced)) {
		t.Fatal("改写之后的文件或者留底的旧文件不是密文")
	}
	if rng, err := ws.SheetRead("账.xlsx", "", ""); err != nil || len(rng.Rows) != 3 {
		t.Fatalf("改写后的表格不对: %v, %+v", err, rng.Rows)
	}
}

func TestWorkspace_加密可以中途开关(t *testing.T) {
	ws, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 还没开加密时放进来的文件是明文。
	if _, err := ws.DocWrite("旧.docx", "# 旧文件\n\n开加密之前写的。"); err != nil {
		t.Fatal(err)
	}
	if seal.IsSealedFile(onDisk(t, ws, "旧.docx")) {
		t.Fatal("没开加密时不应加密")
	}

	// 开了之后：旧的明文文件照常能读，新写的是密文。
	ws.WithSeal(seal.TestBox(), true)
	if doc, err := ws.Read("旧.docx", 0); err != nil || !strings.Contains(doc.Text, "开加密之前写的") {
		t.Fatalf("开加密之后旧的明文文件应当照常能读: %v", err)
	}
	if _, err := ws.DocWrite("新.docx", "# 新文件\n\n开加密之后写的。"); err != nil {
		t.Fatal(err)
	}

	// 又关掉：之前加密的文件仍然读得出来，新写的回到明文。
	ws.WithSeal(seal.TestBox(), false)
	if doc, err := ws.Read("新.docx", 0); err != nil || !strings.Contains(doc.Text, "开加密之后写的") {
		t.Fatalf("关掉加密之后，之前加密的文件应当仍然能读: %v", err)
	}
	if _, err := ws.DocWrite("再.docx", "# 再写一个"); err != nil {
		t.Fatal(err)
	}
	if seal.IsSealedFile(onDisk(t, ws, "再.docx")) {
		t.Fatal("关掉加密之后新写的文件不应加密")
	}

	// 没有密钥：加密的文件读不了，并且说得清原因；要求加密却没有密钥时拒绝写入，不会悄悄写成明文。
	ws.WithSeal(nil, false)
	if _, err := ws.Read("新.docx", 0); err == nil || !strings.Contains(err.Error(), "加密存放") {
		t.Fatalf("没有密钥时读加密文件应当报错: %v", err)
	}
	ws.WithSeal(nil, true)
	if _, err := ws.DocWrite("漏.docx", "# 不该写成明文"); err == nil {
		t.Fatal("要求加密却没有密钥时应当拒绝写入")
	}
	if _, err := os.Stat(filepath.Join(ws.Root, "漏.docx")); err == nil {
		t.Fatal("拒绝写入时不应留下文件")
	}
}
