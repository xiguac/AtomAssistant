package files

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/xuri/excelize/v2"
)

// 管理后台里在线查看和修改文件。
//
// 渲染和大部分编辑在浏览器里做（Word、PPT 在浏览器里解压、改 XML、重新打包），服务器只负责两件事：
// 把文件原样给出去、把改好的文件收回来。表格是例外：浏览器拿不到单元格的样式，由这里读出来；
// 改表格时只改动被改的那些单元格，其余（样式、公式、图表）一概不动。
//
// 三条规矩：
//  1. 助手优先。打开文件时拿到内容的摘要，保存时带回来；摘要对不上，说明打开之后文件被别人（多半是助手）改过了，
//     一律拒绝保存，不覆盖助手的修改。
//  2. 不丢旧版本。保存走和助手写文件同一个入口，被覆盖的旧版本照样留底。
//  3. 改过的文件算用户的。内容里可能有用户新加的东西，所以不再算「助手生成的」，不能被助手直接对外分享。

// ErrConflict 表示文件在打开之后被改过了。
var ErrConflict = errors.New("files: 文件在你打开之后被改过了")

// Digest 是文件内容的摘要。打开文件时给浏览器，保存时由浏览器带回来，用来发现中途被改过。
func Digest(data []byte) string { return digest(data) }

// Fetch 读出一个文件的内容和它的摘要。加密存放的文件在这里解开。
func (w *Workspace) Fetch(rel string) ([]byte, string, error) {
	data, err := w.Bytes(rel)
	if err != nil {
		return nil, "", err
	}
	return data, digest(data), nil
}

// Overwrite 把浏览器里改好的 Word 或 PPT 存回去。baseSHA 是打开时拿到的摘要，必须带上。
func (w *Workspace) Overwrite(rel string, data []byte, baseSHA string) (Saved, error) {
	abs, norm, err := w.existing(rel)
	if err != nil {
		return Saved{}, err
	}
	if e := ext(norm); e != "docx" && e != "pptx" {
		return Saved{}, fmt.Errorf("%w: 只能在线保存 .docx 和 .pptx（表格走单元格修改）", ErrInvalid)
	}
	if strings.TrimSpace(baseSHA) == "" {
		return Saved{}, fmt.Errorf("%w: 缺少打开时的摘要，没法确认文件有没有被别人改过", ErrInvalid)
	}
	if len(data) == 0 || len(data) > MaxFileBytes {
		return Saved{}, fmt.Errorf("%w: 文件是空的，或超过 %d MB", ErrInvalid, MaxFileBytes>>20)
	}
	// docx、pptx 都是 zip：不是的话一定是传坏了，别把一个打不开的文件存成用户的文档。
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return Saved{}, fmt.Errorf("%w: 提交的内容不是一个有效的 %s 文件", ErrInvalid, ext(norm))
	}
	if err := checkOfficeXML(data); err != nil {
		return Saved{}, fmt.Errorf("%w: 提交的文件已损坏，没有保存（%v）", ErrInvalid, err)
	}
	return w.save(norm, byUser, func() ([]byte, error) {
		// 在落盘之前最后核对一次：打开之后文件被改过就拒绝。
		cur, err := w.content(abs)
		if err != nil {
			return nil, err
		}
		if digest(cur) != baseSHA {
			return nil, ErrConflict
		}
		return data, nil
	})
}

// checkOfficeXML 确认 docx、pptx 里的每个 XML 部件都是合法的 XML。浏览器里改过再打包，
// 万一哪里出了错，存下去的就是一个 Word 或 PowerPoint 打不开的文件：宁可拒绝，也不覆盖好的那份。
func checkOfficeXML(data []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("不是有效的压缩包: %w", err)
	}
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".xml") && !strings.HasSuffix(f.Name, ".rels") {
			continue
		}
		if f.UncompressedSize64 > maxXMLBytes {
			return fmt.Errorf("%s 过大", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("%s 打不开: %w", f.Name, err)
		}
		dec := xml.NewDecoder(io.LimitReader(rc, maxXMLBytes+1))
		first := true
		for {
			tok, err := dec.Token()
			if err != nil {
				if err != io.EOF {
					rc.Close()
					return fmt.Errorf("%s 不是合法的 XML: %w", f.Name, err)
				}
				break
			}
			// Go 的解析器对位置错误的声明很宽松，但 Word、浏览器都不认：声明只能在最开头。
			if pi, ok := tok.(xml.ProcInst); ok && pi.Target == "xml" && !first {
				rc.Close()
				return fmt.Errorf("%s 里的 XML 声明不在文件开头", f.Name)
			}
			first = false
		}
		rc.Close()
	}
	return nil
}

// ---- 表格 ----

const (
	// 一次最多给浏览器多少行、多少列。再多的分次看。
	maxViewRows = 1000
	maxViewCols = 60
)

// ViewCell 是表格里的一个单元格。
type ViewCell struct {
	// V 是显示出来的值（已按数字格式整理），F 是公式（带等号），S 是样式的编号（0 表示默认样式）。
	V string `json:"v,omitempty"`
	F string `json:"f,omitempty"`
	S int    `json:"s,omitempty"`
}

// ViewStyle 是一种单元格样式，字段都是现成的 CSS 取值。
type ViewStyle struct {
	Bold      bool              `json:"bold,omitempty"`
	Italic    bool              `json:"italic,omitempty"`
	Underline bool              `json:"underline,omitempty"`
	Strike    bool              `json:"strike,omitempty"`
	Color     string            `json:"color,omitempty"`
	Size      float64           `json:"size,omitempty"`
	Font      string            `json:"font,omitempty"`
	Fill      string            `json:"fill,omitempty"`
	Align     string            `json:"align,omitempty"`
	VAlign    string            `json:"valign,omitempty"`
	Wrap      bool              `json:"wrap,omitempty"`
	Borders   map[string]string `json:"borders,omitempty"`
}

// SheetView 是一个工作表给浏览器画的内容：单元格、样式、合并、列宽行高、冻结窗格。
type SheetView struct {
	Sheets []string `json:"sheets"`
	Sheet  string   `json:"sheet"`
	// Rows、Cols 是返回的行列数；Truncated 为真表示表比这更大，只给了前面这些。
	Rows       int          `json:"rows"`
	Cols       int          `json:"cols"`
	Truncated  bool         `json:"truncated,omitempty"`
	Cells      [][]ViewCell `json:"cells"`
	Styles     []ViewStyle  `json:"styles"`
	Merges     []string     `json:"merges,omitempty"`
	ColWidths  []float64    `json:"col_widths"`
	RowHeights []float64    `json:"row_heights,omitempty"`
	FrozenRows int          `json:"frozen_rows,omitempty"`
	FrozenCols int          `json:"frozen_cols,omitempty"`
	// SHA256 是文件内容的摘要，保存时带回来。
	SHA256 string `json:"sha256"`
}

// SheetView 读出一个工作表给浏览器画。sheet 为空用第一个。
func (w *Workspace) SheetView(rel, sheet string) (SheetView, error) {
	data, sum, err := w.Fetch(rel)
	if err != nil {
		return SheetView{}, err
	}
	if _, norm, _ := w.existing(rel); ext(norm) != "xlsx" && ext(norm) != "xlsm" {
		return SheetView{}, fmt.Errorf("%w: 只能查看 .xlsx 表格", ErrInvalid)
	}
	f, err := openXlsx(data)
	if err != nil {
		return SheetView{}, err
	}
	defer f.Close()
	out := SheetView{Sheets: f.GetSheetList(), SHA256: sum}
	if len(out.Sheets) == 0 {
		return SheetView{}, fmt.Errorf("%w: 表格里没有工作表", ErrInvalid)
	}
	if sheet == "" {
		sheet = out.Sheets[0]
	}
	if idx, _ := f.GetSheetIndex(sheet); idx < 0 {
		return SheetView{}, fmt.Errorf("%w: 没有名为 %q 的工作表", ErrInvalid, sheet)
	}
	out.Sheet = sheet

	// 数据范围：取有内容的最大行列，再和合并区域、列宽设置一起考虑太啰嗦，只看内容。
	rows, err := f.GetRows(sheet)
	if err != nil {
		return SheetView{}, fmt.Errorf("files: 读取工作表失败: %w", err)
	}
	nRows, nCols := len(rows), 0
	for _, r := range rows {
		nCols = max(nCols, len(r))
	}
	if nRows > maxViewRows {
		nRows, out.Truncated = maxViewRows, true
	}
	if nCols > maxViewCols {
		nCols, out.Truncated = maxViewCols, true
	}
	out.Rows, out.Cols = nRows, nCols

	styleIdx := map[int]int{} // 表格里的样式编号 → 返回的样式列表里的位置
	out.Styles = []ViewStyle{{}}
	out.Cells = make([][]ViewCell, nRows)
	for r := range nRows {
		out.Cells[r] = make([]ViewCell, nCols)
		for c := 0; c < nCols; c++ {
			cell, err := excelize.CoordinatesToCellName(c+1, r+1)
			if err != nil {
				continue
			}
			vc := ViewCell{}
			if r < len(rows) && c < len(rows[r]) {
				vc.V = rows[r][c]
			}
			if formula, err := f.GetCellFormula(sheet, cell); err == nil && formula != "" {
				vc.F = "=" + formula
				// 助手或程序写的公式没有存算好的结果，现算一个给人看；算不了的留空（本机不支持的函数）。
				if vc.V == "" {
					if v, err := f.CalcCellValue(sheet, cell); err == nil {
						vc.V = v
					}
				}
			}
			if sid, err := f.GetCellStyle(sheet, cell); err == nil && sid != 0 {
				pos, ok := styleIdx[sid]
				if !ok {
					st, err := f.GetStyle(sid)
					if err == nil && st != nil {
						pos = len(out.Styles)
						out.Styles = append(out.Styles, viewStyle(st))
					}
					styleIdx[sid] = pos
				}
				vc.S = pos
			}
			out.Cells[r][c] = vc
		}
	}

	if merges, err := f.GetMergeCells(sheet, true); err == nil {
		for _, m := range merges {
			if len(m) > 0 {
				out.Merges = append(out.Merges, m[0])
			}
		}
	}
	out.ColWidths = make([]float64, nCols)
	for c := 0; c < nCols; c++ {
		name, _ := excelize.ColumnNumberToName(c + 1)
		wd, err := f.GetColWidth(sheet, name)
		if err != nil || wd <= 0 {
			wd = 8.43
		}
		out.ColWidths[c] = math.Round(wd*100) / 100
	}
	for r := 0; r < nRows; r++ {
		h, err := f.GetRowHeight(sheet, r+1)
		if err != nil {
			h = 0
		}
		if out.RowHeights == nil && h > 0 && h != 15 {
			out.RowHeights = make([]float64, nRows)
		}
		if out.RowHeights != nil {
			out.RowHeights[r] = h
		}
	}
	if p, err := f.GetPanes(sheet); err == nil && p.Freeze {
		out.FrozenCols, out.FrozenRows = p.XSplit, p.YSplit
	}
	return out, nil
}

// Office 默认主题的 10 个颜色，按主题编号取。表格里的颜色常写成「第几号主题色加减多少亮度」。
var themeColors = []string{"FFFFFF", "000000", "E7E6E6", "44546A", "4472C4", "ED7D31", "A5A5A5", "FFC000", "5B9BD5", "70AD47"}

// cssColor 把表格里的颜色写法变成 CSS 颜色。
func cssColor(rgb string, theme *int, tint float64) string {
	hex := strings.TrimPrefix(strings.TrimSpace(rgb), "#")
	if len(hex) == 8 {
		hex = hex[2:]
	}
	if theme != nil && *theme >= 0 && *theme < len(themeColors) {
		hex = themeColors[*theme]
		hex = applyTint(hex, tint)
	}
	if len(hex) != 6 {
		return ""
	}
	return "#" + strings.ToLower(hex)
}

// applyTint 按 Excel 的规则把颜色调亮（tint>0）或调暗（tint<0）。
func applyTint(hex string, tint float64) string {
	if tint == 0 || len(hex) != 6 {
		return hex
	}
	var rgb [3]float64
	for i := 0; i < 3; i++ {
		var v int
		if _, err := fmt.Sscanf(hex[i*2:i*2+2], "%x", &v); err != nil {
			return hex
		}
		rgb[i] = float64(v)
	}
	for i := range rgb {
		if tint > 0 {
			rgb[i] = rgb[i]*(1-tint) + 255*tint
		} else {
			rgb[i] = rgb[i] * (1 + tint)
		}
	}
	return fmt.Sprintf("%02x%02x%02x", int(math.Round(rgb[0])), int(math.Round(rgb[1])), int(math.Round(rgb[2])))
}

func viewStyle(st *excelize.Style) ViewStyle {
	v := ViewStyle{}
	if f := st.Font; f != nil {
		v.Bold, v.Italic, v.Strike = f.Bold, f.Italic, f.Strike
		v.Underline = f.Underline != "" && f.Underline != "none"
		v.Size, v.Font = f.Size, f.Family
		v.Color = cssColor(f.Color, f.ColorTheme, f.ColorTint)
	}
	if st.Fill.Type == "pattern" && st.Fill.Pattern == 1 && len(st.Fill.Color) > 0 {
		v.Fill = cssColor(st.Fill.Color[0], nil, 0)
	}
	if a := st.Alignment; a != nil {
		v.Align, v.VAlign, v.Wrap = a.Horizontal, a.Vertical, a.WrapText
		if v.VAlign == "center" {
			v.VAlign = "middle"
		}
	}
	for _, b := range st.Border {
		css := borderCSS(b)
		if css == "" {
			continue
		}
		if v.Borders == nil {
			v.Borders = map[string]string{}
		}
		switch b.Type {
		case "left", "right", "top", "bottom":
			v.Borders[b.Type] = css
		}
	}
	return v
}

func borderCSS(b excelize.Border) string {
	color := cssColor(b.Color, nil, 0)
	if color == "" {
		color = "#000000"
	}
	switch b.Style {
	case 1, 7:
		return "1px solid " + color
	case 2, 8:
		return "2px solid " + color
	case 3, 9, 10, 11, 12, 13:
		return "1px dashed " + color
	case 4:
		return "1px dotted " + color
	case 5:
		return "3px solid " + color
	case 6:
		return "3px double " + color
	}
	return ""
}

// SheetCellEdit 是浏览器里对一个单元格的修改。Value 以 = 开头是公式，null 清空。
type SheetCellEdit struct {
	Sheet string `json:"sheet"`
	Cell  string `json:"cell"`
	Value any    `json:"value"`
}

// SheetUserEdit 把浏览器里改过的单元格写回表格。只动这些单元格，其余的样式、公式、图表都不动。
// 返回保存结果；公式核算出的问题一并带回，让用户看到（不拦着不让存：这是用户自己的表）。
func (w *Workspace) SheetUserEdit(rel, baseSHA string, edits []SheetCellEdit) (SheetResult, error) {
	abs, norm, err := w.existing(rel)
	if err != nil {
		return SheetResult{}, err
	}
	if e := ext(norm); e != "xlsx" {
		return SheetResult{}, fmt.Errorf("%w: 只能修改 .xlsx 表格", ErrInvalid)
	}
	if strings.TrimSpace(baseSHA) == "" {
		return SheetResult{}, fmt.Errorf("%w: 缺少打开时的摘要，没法确认文件有没有被别人改过", ErrInvalid)
	}
	if len(edits) == 0 {
		return SheetResult{}, fmt.Errorf("%w: 没有修改", ErrInvalid)
	}
	if len(edits) > maxRangeCells {
		return SheetResult{}, fmt.Errorf("%w: 一次最多改 %d 个单元格", ErrInvalid, maxRangeCells)
	}
	raw, err := w.content(abs)
	if err != nil {
		return SheetResult{}, err
	}
	if digest(raw) != baseSHA {
		return SheetResult{}, ErrConflict
	}
	f, err := openXlsx(raw)
	if err != nil {
		return SheetResult{}, err
	}
	defer f.Close()
	for _, e := range edits {
		if idx, _ := f.GetSheetIndex(e.Sheet); idx < 0 {
			return SheetResult{}, fmt.Errorf("%w: 没有名为 %q 的工作表", ErrInvalid, e.Sheet)
		}
		cell := strings.ToUpper(strings.TrimSpace(e.Cell))
		if _, _, err := excelize.CellNameToCoordinates(cell); err != nil {
			return SheetResult{}, fmt.Errorf("%w: 单元格地址 %q 不合法", ErrInvalid, e.Cell)
		}
		if err := setValue(f, e.Sheet, cell, e.Value); err != nil {
			return SheetResult{}, fmt.Errorf("%w: 写入 %s!%s 失败: %v", ErrInvalid, e.Sheet, cell, err)
		}
	}
	out := SheetResult{}
	out.Formulas, out.Issues = checkFormulas(f)
	if err := f.UpdateLinkedValue(); err != nil {
		return SheetResult{}, fmt.Errorf("files: 标记重算失败: %w", err)
	}
	saved, err := w.save(norm, byUser, func() ([]byte, error) {
		// 落盘前最后核对一次，和 Overwrite 一样。
		cur, err := w.content(abs)
		if err != nil {
			return nil, err
		}
		if digest(cur) != baseSHA {
			return nil, ErrConflict
		}
		buf, err := f.WriteToBuffer()
		if err != nil {
			return nil, fmt.Errorf("files: 保存表格失败: %w", err)
		}
		return buf.Bytes(), nil
	})
	if err != nil {
		return SheetResult{}, err
	}
	out.Saved = saved
	return out, nil
}
