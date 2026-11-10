package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiguac/atomassistant/internal/files"
)

// viewServer 造一个只带工作区配置的后台，直接调文件查看的处理函数（不经过登录，登录与防伪由 authed 负责，另有测试）。
func viewServer(t *testing.T) (*Server, *files.Workspace) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	work := filepath.Join(dir, "files")
	if err := os.WriteFile(cfg, []byte("files:\n  dir: "+filepath.ToSlash(work)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{opts: Options{ConfigPath: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	ws, err := s.workspace()
	if err != nil {
		t.Fatal(err)
	}
	return s, ws
}

func TestViewKind(t *testing.T) {
	for name, want := range map[string]string{"a.docx": "docx", "B.XLSX": "xlsx", "c.pptx": "pptx", "d.pdf": "pdf", "e.csv": "", "f.doc": "", "g": ""} {
		if got := viewKind(name); got != want {
			t.Errorf("viewKind(%q) = %q，应为 %q", name, got, want)
		}
	}
}

func TestVendorFile_只给库文件(t *testing.T) {
	s := &Server{}
	for name, want := range map[string]int{
		"jszip-3.10.2.min.js":       http.StatusOK,
		"pdfjs-6.4.299.min.mjs":     http.StatusOK,
		"不存在.js":                    http.StatusNotFound,
		"..%2F..%2Fadmin.go":        http.StatusNotFound,
		".hidden":                   http.StatusNotFound,
		"pptx-preview-1.0.7.umd.js": http.StatusOK,
	} {
		r := httptest.NewRequest("GET", "/static/vendor/x", nil)
		n, _ := url.PathUnescape(name)
		r.SetPathValue("name", n)
		w := httptest.NewRecorder()
		s.vendorFile(w, r)
		if w.Code != want {
			t.Errorf("%s: %d，应为 %d", name, w.Code, want)
		}
		if want == http.StatusOK && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
			t.Errorf("%s: 脚本的类型不对: %s", name, w.Header().Get("Content-Type"))
		}
	}
}

func TestFileRaw_带摘要_路径不能跳出(t *testing.T) {
	s, ws := viewServer(t)
	if _, err := ws.DocWrite("报告.docx", "你好"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.fileRaw(w, httptest.NewRequest("GET", "/files/raw?path="+url.QueryEscape("报告.docx"), nil), nil)
	if w.Code != http.StatusOK || w.Header().Get("X-File-SHA256") != files.Digest(w.Body.Bytes()) || w.Body.Len() == 0 {
		t.Fatalf("应返回文件和它的摘要: %d %q", w.Code, w.Header().Get("X-File-SHA256"))
	}
	w = httptest.NewRecorder()
	s.fileRaw(w, httptest.NewRequest("GET", "/files/raw?path="+url.QueryEscape("../config.yaml"), nil), nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("跳出工作区应被拒: %d", w.Code)
	}
}

func TestFileSave_存回_冲突返回409且不覆盖(t *testing.T) {
	s, ws := viewServer(t)
	if _, err := ws.DocWrite("a.docx", "第一版"); err != nil {
		t.Fatal(err)
	}
	raw, sum, _ := ws.Fetch("a.docx")
	save := func(sha string, body []byte) (*httptest.ResponseRecorder, map[string]any) {
		w := httptest.NewRecorder()
		s.fileSave(w, httptest.NewRequest("POST", "/files/save?path=a.docx&sha="+sha, bytes.NewReader(body)), nil)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w, out
	}
	// 助手在用户打开之后改了文件。
	if _, err := ws.DocWrite("a.docx", "助手改的"); err != nil {
		t.Fatal(err)
	}
	w, out := save(sum, raw)
	if w.Code != http.StatusConflict || out["conflict"] != true || !strings.Contains(out["message"].(string), "助手") {
		t.Fatalf("应返回 409 和冲突提示: %d %v", w.Code, out)
	}
	cur, curSum, _ := ws.Fetch("a.docx")
	if bytes.Equal(cur, raw) {
		t.Fatal("助手的修改被覆盖了")
	}
	// 用最新的摘要保存成功，返回新摘要。
	w, out = save(curSum, cur)
	if w.Code != http.StatusOK || out["ok"] != true || out["sha256"] != files.Digest(cur) {
		t.Fatalf("应保存成功并给出新摘要: %d %v", w.Code, out)
	}
	// 缺摘要、不是 zip：400。
	if w, _ = save("", cur); w.Code != http.StatusBadRequest {
		t.Errorf("缺摘要应为 400: %d", w.Code)
	}
	if w, _ = save(curSum, []byte("不是 zip")); w.Code != http.StatusBadRequest {
		t.Errorf("不是 zip 应为 400: %d", w.Code)
	}
}

func TestFileSheet_查看与修改(t *testing.T) {
	s, ws := viewServer(t)
	if _, err := ws.SheetWrite("t.xlsx", []files.SheetEdit{{Cells: []files.CellEdit{{Cell: "A1", Value: 5}, {Cell: "B1", Value: "=A1*2"}}}}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.fileSheet(w, httptest.NewRequest("GET", "/files/sheet?path=t.xlsx", nil), nil)
	var sv files.SheetView
	if err := json.Unmarshal(w.Body.Bytes(), &sv); err != nil || w.Code != http.StatusOK {
		t.Fatalf("查看表格: %d %v %s", w.Code, err, w.Body.String())
	}
	if sv.Cells[0][1].F != "=A1*2" || sv.Cells[0][1].V != "10" {
		t.Errorf("公式与现算的结果: %+v", sv.Cells[0][1])
	}
	body, _ := json.Marshal(map[string]any{"path": "t.xlsx", "sha": sv.SHA256, "edits": []map[string]any{{"sheet": sv.Sheet, "cell": "A1", "value": 7}}})
	w = httptest.NewRecorder()
	s.fileSheetSave(w, httptest.NewRequest("POST", "/files/sheet/save", bytes.NewReader(body)), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("保存表格: %d %s", w.Code, w.Body.String())
	}
	// 同一个旧摘要再存：冲突。
	w = httptest.NewRecorder()
	s.fileSheetSave(w, httptest.NewRequest("POST", "/files/sheet/save", bytes.NewReader(body)), nil)
	if w.Code != http.StatusConflict {
		t.Errorf("旧摘要应为 409: %d", w.Code)
	}
	// 找不到文件、坏请求。
	w = httptest.NewRecorder()
	s.fileSheet(w, httptest.NewRequest("GET", "/files/sheet?path=没有.xlsx", nil), nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("没有的文件应为 404: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.fileSheetSave(w, httptest.NewRequest("POST", "/files/sheet/save", strings.NewReader("{坏")), nil)
	if w.Code == http.StatusOK {
		t.Error("坏的 JSON 不该成功")
	}
}
