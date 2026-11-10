package admin

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/xiguac/atomassistant/internal/files"
)

// 文件的在线查看与修改。渲染和 Word、PPT 的改动都在浏览器里完成（见 static/fileview.js），
// 这里只做四件事：给出文件原样的字节和它的摘要、给出表格的单元格与样式、收回改好的 Word 或 PPT、收回表格里改过的单元格。
// 保存时必须带上打开时的摘要：文件在这之后被改过（多半是助手改的）就拒绝，不覆盖助手的修改。

// vendorFS 是浏览器端用的第三方库。后台的安全策略不许加载外面的脚本，所以随程序一起带着。
//
//go:embed static/vendor/*
var vendorFS embed.FS

// viewKinds 是能在线打开的文件类型。
var viewKinds = map[string]string{"docx": "docx", "xlsx": "xlsx", "pptx": "pptx", "pdf": "pdf"}

// viewKind 返回一个文件名对应的在线查看类型，不支持返回空。
func viewKind(name string) string {
	return viewKinds[strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))]
}

// vendorFile 提供 /static/vendor/ 下的库文件。库的版本写在文件名里，内容不会变，可以缓存。
func (s *Server) vendorFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	raw, err := vendorFS.ReadFile("static/vendor/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "application/octet-stream"
	if strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".mjs") {
		ct = "text/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(raw)
}

// fileViewData 是查看页要的数据。
type fileViewData struct {
	Path string
	Name string
	Kind string
	// Dir 是返回文件列表时回到哪一层。
	Dir string
}

func (s *Server) fileViewPage(w http.ResponseWriter, r *http.Request, v *view) {
	rel := r.URL.Query().Get("path")
	kind := viewKind(rel)
	if kind == "" {
		http.Error(w, "这种文件不能在线打开，只支持 Word、Excel、PPT 和 PDF。", http.StatusBadRequest)
		return
	}
	d := fileViewData{Path: rel, Name: path.Base(strings.ReplaceAll(rel, `\`, "/")), Kind: kind}
	if dir := path.Dir(strings.ReplaceAll(rel, `\`, "/")); dir != "." && !strings.HasPrefix(dir, files.ReceivedDir) {
		d.Dir = dir
	}
	v.Data = d
	s.render(w, "fileview", v)
}

// fileRaw 给出文件原样的字节，摘要放在响应头里。
func (s *Server) fileRaw(w http.ResponseWriter, r *http.Request, _ *view) {
	ws, err := s.workspace()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	data, sum, err := ws.Fetch(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-File-SHA256", sum)
	_, _ = w.Write(data)
}

// fileSheet 给出表格一个工作表的单元格、样式、合并、列宽。
func (s *Server) fileSheet(w http.ResponseWriter, r *http.Request, _ *view) {
	ws, err := s.workspace()
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	sv, err := ws.SheetView(r.URL.Query().Get("path"), r.URL.Query().Get("sheet"))
	if err != nil {
		jsonError(w, fileErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sv)
}

// fileSave 收回浏览器里改好的 Word 或 PPT。路径、打开时的摘要和 csrf 在地址里，正文是文件的字节。
func (s *Server) fileSave(w http.ResponseWriter, r *http.Request, _ *view) {
	rel := r.URL.Query().Get("path")
	ws, err := s.workspace()
	var saved files.Saved
	if err == nil {
		var data []byte
		if data, err = io.ReadAll(r.Body); err == nil {
			saved, err = ws.Overwrite(rel, data, r.URL.Query().Get("sha"))
			if err == nil {
				s.reportFile(w, r, "已保存", rel, files.Digest(data), saved, nil)
				return
			}
		}
	}
	s.failFile(w, r, err)
}

// fileSheetSave 收回表格里改过的单元格。
func (s *Server) fileSheetSave(w http.ResponseWriter, r *http.Request, _ *view) {
	var in struct {
		Path  string                `json:"path"`
		SHA   string                `json:"sha"`
		Edits []files.SheetCellEdit `json:"edits"`
	}
	ws, err := s.workspace()
	if err == nil {
		err = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
	}
	if err == nil {
		var res files.SheetResult
		if res, err = ws.SheetUserEdit(in.Path, in.SHA, in.Edits); err == nil {
			s.reportFile(w, r, "已保存", in.Path, "", res.Saved, res.Issues)
			return
		}
	}
	s.failFile(w, r, err)
}

func fileErrStatus(err error) int {
	switch {
	case errors.Is(err, files.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, files.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, files.ErrInvalid), errors.Is(err, files.ErrUnsupported):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) reportFile(w http.ResponseWriter, r *http.Request, msg, rel, sum string, saved files.Saved, issues []files.FormulaIssue) {
	s.noteOutcome(r, true, msg+"："+rel)
	s.opts.Log.Info("工作区的文件已在后台修改", "路径", rel, "留底", saved.Replaced, "来自", r.RemoteAddr)
	out := map[string]any{"ok": true, "message": msg, "sha256": sum, "previous_version": saved.Replaced}
	if len(issues) > 0 {
		out["formula_issues"] = issues
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) failFile(w http.ResponseWriter, r *http.Request, err error) {
	s.noteOutcome(r, false, err.Error())
	msg := err.Error()
	if errors.Is(err, files.ErrConflict) {
		msg = "文件在你打开之后被助手或别处改过了，为了不覆盖那次修改，这次没有保存。请刷新页面看看最新内容，再把你的修改重做一遍。"
	}
	writeJSON(w, fileErrStatus(err), map[string]any{
		"ok": false, "message": msg, "conflict": errors.Is(err, files.ErrConflict),
	})
}

// noteOutcome 把这次操作的结果记进操作日志，和 back 做的一样。
func (s *Server) noteOutcome(r *http.Request, ok bool, msg string) {
	if o, found := r.Context().Value(outcomeKey{}).(*outcome); found {
		o.set, o.ok, o.msg = true, ok, msg
	}
}
