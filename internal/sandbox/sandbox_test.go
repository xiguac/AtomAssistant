package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type memWorkspace struct {
	root  string
	files map[string][]byte
}

func (m *memWorkspace) Mountable() string            { return m.root }
func (m *memWorkspace) Snapshot(string, int64) error { return nil }
func (m *memWorkspace) WriteFile(rel string, data []byte) (string, error) {
	if m.files == nil {
		m.files = map[string][]byte{}
	}
	m.files[rel] = data
	return rel, nil
}

func TestArgs_断网只读看不到服务的密钥(t *testing.T) {
	r := &Runner{Python: "/opt/atomhelper-sandbox/bin/python3", Timeout: time.Minute, MemoryMB: 512}
	args := strings.Join(r.args("/var/lib/atomhelper/files", "/tmp/x/out", "/tmp/x/main.py"), " ")
	for _, want := range []string{
		"--unshare-all", "--die-with-parent", "--clearenv",
		"--ro-bind /var/lib/atomhelper/files /work", "--bind /tmp/x/out /out",
		"--ro-bind /opt/atomhelper-sandbox /opt/atomhelper-sandbox",
		"--as=536870912", "-I /sandbox/main.py",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("参数里应有 %q:\n%s", want, args)
		}
	}
	// /etc 只给运行需要的几个文件：服务的密钥文件在 /etc/atomhelper，不能挂进去。
	if strings.Contains(args, "--ro-bind /etc /etc") || strings.Contains(args, "/etc/atomhelper") || strings.Contains(args, "--share-net") {
		t.Fatalf("不应把整个 /etc 或网络交给沙箱:\n%s", args)
	}
	// 工作区只读：/work 不能是可写挂载。
	if strings.Contains(args, "--bind /var/lib/atomhelper/files") {
		t.Fatalf("工作区应只读挂载:\n%s", args)
	}
}

func TestCollect_输出文件收进工作区有上限(t *testing.T) {
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "子目录"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(out, "汇总.xlsx"), []byte("xlsx"), 0o600)
	_ = os.WriteFile(filepath.Join(out, "子目录", "图.png"), []byte("png"), 0o600)
	// 符号链接不收：它可能指向沙箱外面。
	_ = os.Symlink("/etc/passwd", filepath.Join(out, "link"))
	ws := &memWorkspace{}
	r := &Runner{Workspace: ws, now: func() time.Time { return time.Date(2026, 10, 14, 9, 30, 0, 0, time.Local) }}
	var res Result
	if err := r.collect(out, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || string(ws.files["沙箱输出/20261014-093000/汇总.xlsx"]) != "xlsx" ||
		string(ws.files["沙箱输出/20261014-093000/子目录/图.png"]) != "png" {
		t.Fatalf("输出文件应收进「沙箱输出/<时间>」: %v %v", res.Files, ws.files)
	}
	for k := range ws.files {
		if strings.Contains(k, "link") {
			t.Fatal("符号链接不应收进工作区")
		}
	}
	// 太多的不收，并且说明。
	many := t.TempDir()
	for i := 0; i < maxOutputFiles+3; i++ {
		_ = os.WriteFile(filepath.Join(many, strings.Repeat("a", i+1)), []byte("x"), 0o600)
	}
	res = Result{}
	if err := (&Runner{Workspace: &memWorkspace{}}).collect(many, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != maxOutputFiles || !strings.Contains(res.Note, "有 3 个输出文件没有收下") {
		t.Fatalf("超出上限的应说明: %d %s", len(res.Files), res.Note)
	}
}

func TestRun_跑不了的时候说明白(t *testing.T) {
	r := &Runner{Python: "/no/such/python3", Workspace: &memWorkspace{root: t.TempDir()}}
	if _, err := r.Run(context.Background(), ""); err == nil {
		t.Fatal("没有代码应报错")
	}
	if _, err := r.Run(context.Background(), strings.Repeat("x", maxCodeBytes+1)); err == nil {
		t.Fatal("代码太大应报错")
	}
	_, err := r.Run(context.Background(), "print(1)")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("解释器不存在（或不是 Linux）时应报 ErrUnavailable: %v", err)
	}
	if runtime.GOOS != "linux" && !strings.Contains(err.Error(), "只支持 Linux") {
		t.Fatalf("不是 Linux 时应说明: %v", err)
	}
}

func TestLimitedBuffer_只留前面一段(t *testing.T) {
	b := limitedBuffer{max: 8}
	_, _ = b.Write([]byte("12345"))
	_, _ = b.Write([]byte("67890"))
	if s := b.String(); !strings.HasPrefix(s, "12345678") || !strings.Contains(s, "省略") {
		t.Fatalf("应只留前 8 个字节并注明: %q", s)
	}
}
