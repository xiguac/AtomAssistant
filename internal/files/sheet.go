package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/xuri/excelize/v2"
)

const (
	// maxRangeCells 是一次读取或写入的单元格数上限。
	maxRangeCells = 5000
	// maxFormulaChecks 是保存后最多核算多少个公式。
	maxFormulaChecks = 20000
)

// CellEdit 是对一个单元格的写入。
type CellEdit struct {
	// Cell 是单元格地址，如 B10。
	Cell string `json:"cell"`
	// Value 是要写的值。以 = 开头的字符串是公式。null 表示清空。
	Value any `json:"value"`
}

// Block 是从某个单元格开始、按行列铺开的一块数据。
type Block struct {
	// Start 是左上角的单元格，不写按 A1。
	Start  string  `json:"start"`
	Values [][]any `json:"values"`
	// Header 为真时把第一行设成粗体。
	Header bool `json:"header"`
}

// SheetEdit 是对一个工作表的一组写入。
type SheetEdit struct {
	// Sheet 是工作表的名字，不存在就新建。不写按第一个工作表。
	Sheet string     `json:"sheet"`
	Rows  *Block     `json:"rows"`
	Cells []CellEdit `json:"cells"`
	// Widths 是列宽，键是列字母，如 {"A": 24}。
	Widths map[string]float64 `json:"widths"`
}

// FormulaIssue 是保存后核算公式时发现的一个问题。
type FormulaIssue struct {
	Sheet string `json:"sheet"`
	Cell  string `json:"cell"`
	// Problem 是出错的结果（#REF!、#DIV/0! 等），或者没法在本机核算的原因。
	Problem string `json:"problem"`
	// Unverified 为真表示不是算错了，而是这个公式用到的函数本机算不了，要在 Excel 里打开确认。
	Unverified bool `json:"unverified,omitempty"`
}

// SheetResult 是一次写表格的结果。
type SheetResult struct {
	Saved
	// Formulas 是写完之后表里的公式总数。
	Formulas int            `json:"formulas"`
	Issues   []FormulaIssue `json:"formula_issues,omitempty"`
}

// setValue 往一个单元格写值。以 = 开头的字符串写成公式。
func setValue(f *excelize.File, sheet, cell string, v any) error {
	if s, ok := v.(string); ok && strings.HasPrefix(s, "=") && len(s) > 1 {
		// 先清掉原来的值：否则旧的缓存结果会留在公式旁边，别的软件读到的是旧数。
		if err := f.SetCellValue(sheet, cell, nil); err != nil {
			return err
		}
		return f.SetCellFormula(sheet, cell, s[1:])
	}
	// 写值的时候把原来可能存在的公式去掉，不然单元格里公式和值并存。
	if err := f.SetCellFormula(sheet, cell, ""); err != nil {
		return err
	}
	return f.SetCellValue(sheet, cell, v)
}

// SheetWrite 新建或修改一个 Excel 表格。
//
// 要算的数写成公式而不是算好的结果：表格之后还要能跟着数据变。
// 保存之后会把表里的公式全部核算一遍，算出错误（#REF!、#DIV/0! 等）的列在结果里，
// 交给调用方改正——一张带着公式错误的表不该交到用户手上。
func (w *Workspace) SheetWrite(rel string, edits []SheetEdit) (SheetResult, error) {
	abs, norm, err := w.resolve(rel, true)
	if err != nil {
		return SheetResult{}, err
	}
	if ext(norm) != "xlsx" {
		return SheetResult{}, fmt.Errorf("%w: 表格只能存成 .xlsx", ErrInvalid)
	}
	if len(edits) == 0 {
		return SheetResult{}, fmt.Errorf("%w: 没有说要写什么", ErrInvalid)
	}

	var f *excelize.File
	fresh := false
	if _, statErr := os.Stat(abs); errors.Is(statErr, fs.ErrNotExist) {
		f, fresh = excelize.NewFile(), true
	} else {
		if _, _, err := w.existing(rel); err != nil {
			return SheetResult{}, err
		}
		raw, err := w.content(abs)
		if err != nil {
			return SheetResult{}, err
		}
		if f, err = openXlsx(raw); err != nil {
			return SheetResult{}, err
		}
	}
	defer f.Close()

	bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return SheetResult{}, fmt.Errorf("files: 创建样式失败: %w", err)
	}
	written := 0
	for i, e := range edits {
		sheet := strings.TrimSpace(e.Sheet)
		list := f.GetSheetList()
		switch {
		case sheet == "":
			sheet = list[0]
		case fresh && i == 0 && sheet != list[0]:
			// 新文件自带一个叫 Sheet1 的空表，第一组写入就用它，按调用方的名字改名。
			if err := f.SetSheetName(list[0], sheet); err != nil {
				return SheetResult{}, fmt.Errorf("%w: 工作表名 %q 不合法: %v", ErrInvalid, sheet, err)
			}
		default:
			if idx, _ := f.GetSheetIndex(sheet); idx < 0 {
				if _, err := f.NewSheet(sheet); err != nil {
					return SheetResult{}, fmt.Errorf("%w: 工作表名 %q 不合法: %v", ErrInvalid, sheet, err)
				}
			}
		}

		if e.Rows != nil {
			start := strings.TrimSpace(e.Rows.Start)
			if start == "" {
				start = "A1"
			}
			col0, row0, err := excelize.CellNameToCoordinates(start)
			if err != nil {
				return SheetResult{}, fmt.Errorf("%w: 起始单元格 %q 不合法", ErrInvalid, start)
			}
			width := 0
			for r, row := range e.Rows.Values {
				if len(row) > width {
					width = len(row)
				}
				for c, v := range row {
					if written++; written > maxRangeCells {
						return SheetResult{}, fmt.Errorf("%w: 一次最多写 %d 个单元格，分几次写", ErrInvalid, maxRangeCells)
					}
					cell, err := excelize.CoordinatesToCellName(col0+c, row0+r)
					if err != nil {
						return SheetResult{}, fmt.Errorf("%w: 数据超出了表格的范围", ErrInvalid)
					}
					if err := setValue(f, sheet, cell, v); err != nil {
						return SheetResult{}, fmt.Errorf("%w: 写入 %s!%s 失败: %v", ErrInvalid, sheet, cell, err)
					}
				}
			}
			if e.Rows.Header && width > 0 && len(e.Rows.Values) > 0 {
				first, _ := excelize.CoordinatesToCellName(col0, row0)
				last, _ := excelize.CoordinatesToCellName(col0+width-1, row0)
				if err := f.SetCellStyle(sheet, first, last, bold); err != nil {
					return SheetResult{}, fmt.Errorf("files: 设置表头样式失败: %w", err)
				}
			}
		}
		for _, c := range e.Cells {
			cell := strings.ToUpper(strings.TrimSpace(c.Cell))
			if _, _, err := excelize.CellNameToCoordinates(cell); err != nil {
				return SheetResult{}, fmt.Errorf("%w: 单元格地址 %q 不合法", ErrInvalid, c.Cell)
			}
			if written++; written > maxRangeCells {
				return SheetResult{}, fmt.Errorf("%w: 一次最多写 %d 个单元格，分几次写", ErrInvalid, maxRangeCells)
			}
			if err := setValue(f, sheet, cell, c.Value); err != nil {
				return SheetResult{}, fmt.Errorf("%w: 写入 %s!%s 失败: %v", ErrInvalid, sheet, cell, err)
			}
		}
		for col, width := range e.Widths {
			col = strings.ToUpper(strings.TrimSpace(col))
			if width <= 0 || width > 255 {
				return SheetResult{}, fmt.Errorf("%w: 列宽 %v 不合法", ErrInvalid, width)
			}
			if err := f.SetColWidth(sheet, col, col, width); err != nil {
				return SheetResult{}, fmt.Errorf("%w: 列 %q 不合法", ErrInvalid, col)
			}
		}
	}
	if written == 0 {
		return SheetResult{}, fmt.Errorf("%w: 没有任何单元格要写", ErrInvalid)
	}

	out := SheetResult{}
	out.Formulas, out.Issues = checkFormulas(f)
	// 让 Excel 打开时把公式重算一遍：我们没有往文件里存算好的结果。
	if err := f.UpdateLinkedValue(); err != nil {
		return SheetResult{}, fmt.Errorf("files: 标记重算失败: %w", err)
	}
	saved, err := w.save(rel, byAssistant, func() ([]byte, error) {
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

// checkFormulas 把表里的公式全部核算一遍，返回公式总数和发现的问题。
func checkFormulas(f *excelize.File) (int, []FormulaIssue) {
	total := 0
	var issues []FormulaIssue
	for _, sheet := range f.GetSheetList() {
		rows, err := f.Rows(sheet)
		if err != nil {
			continue
		}
		r := 0
		for rows.Next() {
			r++
			cols, err := rows.Columns()
			if err != nil {
				break
			}
			for c := range cols {
				cell, err := excelize.CoordinatesToCellName(c+1, r)
				if err != nil {
					continue
				}
				formula, err := f.GetCellFormula(sheet, cell)
				if err != nil || formula == "" {
					continue
				}
				if total++; total > maxFormulaChecks {
					continue
				}
				result, err := f.CalcCellValue(sheet, cell)
				switch {
				case err != nil && strings.HasPrefix(err.Error(), "#"):
					issues = append(issues, FormulaIssue{Sheet: sheet, Cell: cell, Problem: err.Error()})
				case err != nil:
					issues = append(issues, FormulaIssue{Sheet: sheet, Cell: cell, Problem: err.Error(), Unverified: true})
				case strings.HasPrefix(result, "#"):
					issues = append(issues, FormulaIssue{Sheet: sheet, Cell: cell, Problem: result})
				}
			}
		}
		_ = rows.Close()
	}
	sort.SliceStable(issues, func(i, j int) bool { return !issues[i].Unverified && issues[j].Unverified })
	if len(issues) > 50 {
		issues = issues[:50]
	}
	return total, issues
}

// SheetRange 是表格里的一块区域。
type SheetRange struct {
	Path  string `json:"path"`
	Sheet string `json:"sheet"`
	Range string `json:"range"`
	// Rows 是各单元格显示出来的值。
	Rows [][]string `json:"rows"`
	// Formulas 是区域内带公式的单元格，键是地址，值是公式（带等号）。
	Formulas map[string]string `json:"formulas,omitempty"`
	Sheets   []string          `json:"sheets"`
}

// SheetRead 读取表格里指定的一块区域，带上其中的公式。
// sheet 为空读第一个工作表；rng 为空读这个工作表有内容的整个范围。
func (w *Workspace) SheetRead(rel, sheet, rng string) (SheetRange, error) {
	abs, norm, err := w.existing(rel)
	if err != nil {
		return SheetRange{}, err
	}
	if e := ext(norm); e != "xlsx" && e != "xlsm" {
		return SheetRange{}, fmt.Errorf("%w: 这个工具只读 .xlsx，其他文件用通读工具", ErrInvalid)
	}
	raw, err := w.content(abs)
	if err != nil {
		return SheetRange{}, err
	}
	f, err := openXlsx(raw)
	if err != nil {
		return SheetRange{}, err
	}
	defer f.Close()
	out := SheetRange{Path: norm, Sheets: f.GetSheetList()}
	if sheet = strings.TrimSpace(sheet); sheet == "" {
		sheet = out.Sheets[0]
	}
	if idx, _ := f.GetSheetIndex(sheet); idx < 0 {
		return out, fmt.Errorf("%w: 没有名为 %q 的工作表（现有：%s）", ErrNotFound, sheet, strings.Join(out.Sheets, "、"))
	}
	out.Sheet = sheet
	if rng = strings.ToUpper(strings.TrimSpace(rng)); rng == "" {
		rng = usedRange(f, sheet)
	}
	from, to, ok := strings.Cut(rng, ":")
	if !ok {
		to = from
	}
	c1, r1, err1 := excelize.CellNameToCoordinates(from)
	c2, r2, err2 := excelize.CellNameToCoordinates(to)
	if err1 != nil || err2 != nil || c2 < c1 || r2 < r1 {
		return out, fmt.Errorf("%w: 范围 %q 不合法，要写成 A1:D20 这样", ErrInvalid, rng)
	}
	if (c2-c1+1)*(r2-r1+1) > maxRangeCells {
		return out, fmt.Errorf("%w: 这块区域有 %d 个单元格，一次最多读 %d 个，缩小范围再读",
			ErrInvalid, (c2-c1+1)*(r2-r1+1), maxRangeCells)
	}
	out.Range = from + ":" + to
	out.Formulas = map[string]string{}
	for r := r1; r <= r2; r++ {
		row := make([]string, 0, c2-c1+1)
		for c := c1; c <= c2; c++ {
			cell, _ := excelize.CoordinatesToCellName(c, r)
			value, err := f.GetCellValue(sheet, cell)
			if err != nil {
				return out, fmt.Errorf("files: 读取 %s!%s 失败: %w", sheet, cell, err)
			}
			if formula, _ := f.GetCellFormula(sheet, cell); formula != "" {
				out.Formulas[cell] = "=" + formula
				// 文件里没存算好的结果时现算一个，免得公式单元格看起来是空的。
				if value == "" {
					if calc, err := f.CalcCellValue(sheet, cell); err == nil {
						value = calc
					}
				}
			}
			row = append(row, value)
		}
		out.Rows = append(out.Rows, row)
	}
	// 去掉末尾整行为空的行，省得占地方。
	for len(out.Rows) > 0 {
		last := out.Rows[len(out.Rows)-1]
		if strings.Join(last, "") != "" {
			break
		}
		out.Rows = out.Rows[:len(out.Rows)-1]
	}
	return out, nil
}

// usedRange 数出一个工作表里有内容的范围。
//
// 不用文件里记的范围：那个数是写文件的软件填的，刚改过还没保存的表里它是旧的，
// 有的软件干脆不填。
func usedRange(f *excelize.File, sheet string) string {
	rows, err := f.Rows(sheet)
	if err != nil {
		return "A1"
	}
	defer rows.Close()
	maxRow, maxCol, r := 1, 1, 0
	for rows.Next() {
		r++
		cols, err := rows.Columns()
		if err != nil {
			break
		}
		if len(cols) > 0 {
			maxRow = r
		}
		if len(cols) > maxCol {
			maxCol = len(cols)
		}
	}
	last, err := excelize.CoordinatesToCellName(maxCol, maxRow)
	if err != nil {
		return "A1"
	}
	return "A1:" + last
}
