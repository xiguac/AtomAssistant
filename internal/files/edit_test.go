package files

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func editWS(t *testing.T) *Workspace {
	t.Helper()
	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// docText 取出 docx 里正文 XML 的内容（docx 是压缩的，不能直接在字节里找字）。
func docText(t *testing.T, raw []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, _ := f.Open()
			defer rc.Close()
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(rc)
			return buf.String()
		}
	}
	t.Fatal("docx 里没有 word/document.xml")
	return ""
}

// docxWith 把一份 docx 里的某段文字换掉，返回新的 docx。
func docxWith(t *testing.T, raw []byte, from, to string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		rc, _ := f.Open()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(rc)
		rc.Close()
		data := buf.Bytes()
		if f.Name == "word/document.xml" {
			data = bytes.ReplaceAll(data, []byte(from), []byte(to))
		}
		wr, _ := zw.Create(f.Name)
		_, _ = wr.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestOverwrite_存回改好的Word_留底且算用户的(t *testing.T) {
	w := editWS(t)
	if _, err := w.DocWrite("报告.docx", "# 季度报告\n\n本季度销量增长。"); err != nil {
		t.Fatal(err)
	}
	raw, sum, err := w.Fetch("报告.docx")
	if err != nil {
		t.Fatal(err)
	}
	edited := docxWith(t, raw, "销量增长", "销量大幅增长")
	saved, err := w.Overwrite("报告.docx", edited, sum)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Replaced == "" {
		t.Error("被覆盖的旧版本应当留底")
	}
	got, _, _ := w.Fetch("报告.docx")
	if !strings.Contains(docText(t, got), "销量大幅增长") {
		t.Error("改过的内容没有存进去")
	}
	// 来源改成用户的：助手不能直接把它对外分享。
	newSum := Digest(got)
	if by, ok, _ := w.originOf("报告.docx", newSum); !ok || by != byUser {
		t.Errorf("改过的文件应当记为用户的，得到 %v %v", by, ok)
	}
}

func TestOverwrite_助手优先_打开之后被改过就拒绝(t *testing.T) {
	w := editWS(t)
	if _, err := w.DocWrite("a.docx", "第一版"); err != nil {
		t.Fatal(err)
	}
	raw, sum, _ := w.Fetch("a.docx")
	// 用户还开着页面的时候，助手把文件改了。
	if _, err := w.DocWrite("a.docx", "助手改的第二版"); err != nil {
		t.Fatal(err)
	}
	_, err := w.Overwrite("a.docx", docxWith(t, raw, "第一版", "我改的"), sum)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("应当报冲突，得到 %v", err)
	}
	cur, _, _ := w.Fetch("a.docx")
	if !strings.Contains(docText(t, cur), "助手改的第二版") {
		t.Error("助手的修改不能被覆盖")
	}
}

func TestOverwrite_参数不合法(t *testing.T) {
	w := editWS(t)
	if _, err := w.DocWrite("a.docx", "x"); err != nil {
		t.Fatal(err)
	}
	raw, sum, _ := w.Fetch("a.docx")
	if _, err := w.Overwrite("a.docx", raw, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("没带摘要应当被拒: %v", err)
	}
	if _, err := w.Overwrite("a.docx", []byte("不是 zip"), sum); !errors.Is(err, ErrInvalid) {
		t.Errorf("不是 zip 应当被拒: %v", err)
	}
	if _, err := w.Overwrite("a.docx", nil, sum); !errors.Is(err, ErrInvalid) {
		t.Errorf("空内容应当被拒: %v", err)
	}
	if _, err := w.Overwrite("不存在.docx", raw, sum); !errors.Is(err, ErrNotFound) {
		t.Errorf("文件不存在: %v", err)
	}
	if _, err := w.Overwrite("../x.docx", raw, sum); err == nil {
		t.Error("路径不能跳出工作区")
	}
	if _, err := w.SheetWrite("t.xlsx", []SheetEdit{{Cells: []CellEdit{{Cell: "A1", Value: 1}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Overwrite("t.xlsx", raw, sum); !errors.Is(err, ErrInvalid) {
		t.Errorf("表格不走这个入口: %v", err)
	}
}

// styledSheet 造一张带样式的表：粗体红字、黄底、合并、列宽、公式。
func styledSheet(t *testing.T, w *Workspace) {
	t.Helper()
	f := excelize.NewFile()
	bold, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FF0000", Size: 14},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFFF00"}},
		Border:    []excelize.Border{{Type: "bottom", Color: "000000", Style: 1}},
		Alignment: &excelize.Alignment{Horizontal: "center", WrapText: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = f.SetCellValue("Sheet1", "A1", "标题")
	_ = f.SetCellStyle("Sheet1", "A1", "A1", bold)
	_ = f.MergeCell("Sheet1", "A1", "C1")
	_ = f.SetCellValue("Sheet1", "A2", 10)
	_ = f.SetCellValue("Sheet1", "B2", 20)
	_ = f.SetCellFormula("Sheet1", "C2", "A2+B2")
	_ = f.SetColWidth("Sheet1", "A", "A", 24)
	_ = f.SetPanes("Sheet1", &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.save("表.xlsx", byUser, func() ([]byte, error) { return buf.Bytes(), nil }); err != nil {
		t.Fatal(err)
	}
}

func TestSheetView_样式合并列宽冻结都带出来(t *testing.T) {
	w := editWS(t)
	styledSheet(t, w)
	v, err := w.SheetView("表.xlsx", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Sheet != "Sheet1" || len(v.Sheets) != 1 || v.SHA256 == "" {
		t.Fatalf("基本信息不对: %+v", v)
	}
	if v.Cells[0][0].V != "标题" || v.Cells[0][0].S == 0 {
		t.Fatalf("A1 应当有内容和样式: %+v", v.Cells[0][0])
	}
	st := v.Styles[v.Cells[0][0].S]
	if !st.Bold || st.Color != "#ff0000" || st.Fill != "#ffff00" || st.Align != "center" || !st.Wrap || st.Size != 14 {
		t.Errorf("样式没读对: %+v", st)
	}
	if st.Borders["bottom"] != "1px solid #000000" {
		t.Errorf("边框没读对: %+v", st.Borders)
	}
	if len(v.Merges) != 1 || v.Merges[0] != "A1:C1" {
		t.Errorf("合并 = %v", v.Merges)
	}
	if v.Cells[1][2].F != "=A2+B2" {
		t.Errorf("公式 = %q", v.Cells[1][2].F)
	}
	if v.Cells[1][2].V != "30" {
		t.Errorf("没有缓存值的公式应当现算出结果，得到 %q", v.Cells[1][2].V)
	}
	if v.ColWidths[0] != 24 {
		t.Errorf("列宽 = %v", v.ColWidths)
	}
	if v.FrozenRows != 1 {
		t.Errorf("冻结行数 = %d", v.FrozenRows)
	}
	if _, err := w.SheetView("表.xlsx", "没有这张表"); !errors.Is(err, ErrInvalid) {
		t.Errorf("没有的工作表应当报错: %v", err)
	}
}

func TestSheetUserEdit_只改被改的格_样式不丢_助手优先(t *testing.T) {
	w := editWS(t)
	styledSheet(t, w)
	v, _ := w.SheetView("表.xlsx", "")
	res, err := w.SheetUserEdit("表.xlsx", v.SHA256, []SheetCellEdit{
		{Sheet: "Sheet1", Cell: "A2", Value: 100},
		{Sheet: "Sheet1", Cell: "D2", Value: "=A2*2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Replaced == "" || res.Formulas != 2 {
		t.Errorf("应当留底、共 2 个公式: %+v", res)
	}
	v2, _ := w.SheetView("表.xlsx", "")
	if v2.Cells[1][0].V != "100" {
		t.Errorf("A2 = %q", v2.Cells[1][0].V)
	}
	if v2.Cells[1][3].F != "=A2*2" {
		t.Errorf("D2 公式 = %q", v2.Cells[1][3].F)
	}
	// A1 的样式和合并都还在。
	if st := v2.Styles[v2.Cells[0][0].S]; !st.Bold || st.Fill != "#ffff00" {
		t.Errorf("没动的单元格样式丢了: %+v", st)
	}
	if len(v2.Merges) != 1 {
		t.Errorf("合并丢了: %v", v2.Merges)
	}
	// 用旧摘要再改：被拒。
	if _, err := w.SheetUserEdit("表.xlsx", v.SHA256, []SheetCellEdit{{Sheet: "Sheet1", Cell: "A2", Value: 1}}); !errors.Is(err, ErrConflict) {
		t.Errorf("旧摘要应当报冲突: %v", err)
	}
	// 助手改了之后，用户拿着打开时的摘要保存：被拒，助手的修改保留。
	if _, err := w.SheetWrite("表.xlsx", []SheetEdit{{Cells: []CellEdit{{Cell: "A3", Value: "助手写的"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.SheetUserEdit("表.xlsx", v2.SHA256, []SheetCellEdit{{Sheet: "Sheet1", Cell: "A2", Value: 7}}); !errors.Is(err, ErrConflict) {
		t.Errorf("助手改过之后应当报冲突: %v", err)
	}
	v3, _ := w.SheetView("表.xlsx", "")
	if v3.Cells[2][0].V != "助手写的" {
		t.Error("助手的修改不能被覆盖")
	}
}

func TestSheetUserEdit_参数不合法(t *testing.T) {
	w := editWS(t)
	styledSheet(t, w)
	v, _ := w.SheetView("表.xlsx", "")
	for name, tc := range map[string]struct {
		sha   string
		edits []SheetCellEdit
	}{
		"没有摘要":  {"", []SheetCellEdit{{Sheet: "Sheet1", Cell: "A1", Value: 1}}},
		"没有修改":  {v.SHA256, nil},
		"坏的地址":  {v.SHA256, []SheetCellEdit{{Sheet: "Sheet1", Cell: "ZZZZ0", Value: 1}}},
		"没有这张表": {v.SHA256, []SheetCellEdit{{Sheet: "不存在", Cell: "A1", Value: 1}}},
	} {
		if _, err := w.SheetUserEdit("表.xlsx", tc.sha, tc.edits); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s 应当被拒: %v", name, err)
		}
	}
}

func TestApplyTint(t *testing.T) {
	if got := applyTint("000000", 0.5); got != "808080" {
		t.Errorf("黑色调亮一半 = %s", got)
	}
	if got := applyTint("ffffff", -0.5); got != "808080" {
		t.Errorf("白色调暗一半 = %s", got)
	}
	theme := 4
	if got := cssColor("", &theme, 0); got != "#4472c4" {
		t.Errorf("主题色 4 = %s", got)
	}
	if got := cssColor("FFFF0000", nil, 0); got != "#ff0000" {
		t.Errorf("带透明度前缀的颜色 = %s", got)
	}
}

func TestOverwrite_XML损坏的文件拒绝保存(t *testing.T) {
	w := editWS(t)
	if _, err := w.DocWrite("a.docx", "第一版"); err != nil {
		t.Fatal(err)
	}
	raw, sum, _ := w.Fetch("a.docx")
	// 模拟浏览器重复加了 XML 声明。
	bad := docxWith(t, raw, "<w:document", `<?xml version="1.0"?><w:document`)
	if _, err := w.Overwrite("a.docx", bad, sum); !errors.Is(err, ErrInvalid) {
		t.Fatalf("损坏的 XML 应被拒绝: %v", err)
	}
	cur, _, _ := w.Fetch("a.docx")
	if !bytes.Equal(cur, raw) {
		t.Error("被拒绝之后原文件不该变")
	}
}
