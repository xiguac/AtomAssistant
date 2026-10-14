package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// workspace 返回一个测试用的工作区。设置了 ATOM_FILES_OUT 时用那个目录并保留产物，
// 方便拿真正的 Office 打开检查生成的文件。
func workspace(t *testing.T) *Workspace {
	t.Helper()
	dir := os.Getenv("ATOM_FILES_OUT")
	if dir == "" {
		dir = t.TempDir()
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("打开工作区失败: %v", err)
	}
	return w
}

func TestWorkspace_出不了工作区(t *testing.T) {
	w := workspace(t)
	outside := filepath.Join(filepath.Dir(w.Root), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("机密"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	for _, bad := range []string{
		"../outside-secret.txt", "a/../../outside-secret.txt", `..\outside-secret.txt`,
		outside, "/etc/passwd", `C:\Windows\win.ini`, "a\x00b.txt",
	} {
		if _, err := w.Read(bad, 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("读 %q: 期望 ErrInvalid，实际 %v", bad, err)
		}
		if _, err := w.DocWrite(bad+".docx", "# x"); !errors.Is(err, ErrInvalid) {
			t.Errorf("写 %q: 期望 ErrInvalid，实际 %v", bad, err)
		}
	}
	// 留底目录不许写，免得把别的文件的旧版本冲掉。
	if _, err := w.DocWrite(".versions/x.docx", "# x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("留底目录应不可写: %v", err)
	}
	if _, err := w.Read("没有这个文件.txt", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的文件应报 ErrNotFound: %v", err)
	}

	// 工作区里一个指向外面的符号链接：字面上在里面，实际在外面。
	link := filepath.Join(w.Root, "link.txt")
	if err := os.Symlink(outside, link); err == nil {
		defer os.Remove(link)
		if _, err := w.Read("link.txt", 0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("经符号链接绕出工作区应被拒绝: %v", err)
		}
	}
}

func TestSave_覆盖时旧文件留底(t *testing.T) {
	w := workspace(t)
	first, err := w.DocWrite("版本/纪要.docx", "# 第一版\n\n内容一")
	if err != nil || first.Replaced != "" {
		t.Fatalf("首次写入不应有旧版本: %+v %v", first, err)
	}
	second, err := w.DocWrite("版本/纪要.docx", "# 第二版\n\n内容二")
	if err != nil || second.Replaced == "" {
		t.Fatalf("覆盖时应把旧文件留底: %+v %v", second, err)
	}
	now, _ := w.Read("版本/纪要.docx", 0)
	old, err := w.Read(second.Replaced, 0)
	if err != nil || !strings.Contains(old.Text, "第一版") || !strings.Contains(now.Text, "第二版") {
		t.Fatalf("留底的应是旧内容，当前的应是新内容: %v %q %q", err, old.Text, now.Text)
	}

	// 列目录时看不到留底目录和临时文件。
	root, err := w.List("")
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	for _, e := range root {
		if strings.HasPrefix(e.Path, ".") {
			t.Fatalf("隐藏的条目不应列出: %+v", e)
		}
	}
	sub, err := w.List("版本")
	if err != nil || len(sub) != 1 || sub[0].Path != "版本/纪要.docx" || sub[0].Size == 0 {
		t.Fatalf("子目录内容不对: %+v %v", sub, err)
	}
}

const sampleDoc = `# 项目周报

本周完成了 **结算模块** 的联调，*下周* 提测。接口文档见 ` + "`docs/api.md`" + `。

## 进展

- 结算接口联调完成
- 修复 3 个对账差异
- 性能压测 <通过> & 达标

## 下周计划

1. 提测
2. 准备上线方案

## 风险

| 事项 | 负责人 | 状态 |
|---|---|---|
| 第三方回调延迟 | 李伟 | 跟进中 |
| 压测环境不稳定 | 王磊 | 已解决 |

1. 另一个列表也应从 1 开始
`

func TestDocWrite_Markdown排成Word再读回来(t *testing.T) {
	w := workspace(t)
	if _, err := w.DocWrite("周报.docx", sampleDoc); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	got, err := w.Read("周报.docx", 0)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Kind != "docx" {
		t.Fatalf("种类不对: %s", got.Kind)
	}
	for _, want := range []string{
		"# 项目周报", "## 进展", "## 下周计划",
		"本周完成了 结算模块 的联调，下周 提测。接口文档见 docs/api.md。",
		"- 结算接口联调完成", "- 性能压测 <通过> & 达标", "- 提测", "- 准备上线方案",
		"| 事项 | 负责人 | 状态 |", "| 第三方回调延迟 | 李伟 | 跟进中 |",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("读回的内容里应有 %q:\n%s", want, got.Text)
		}
	}
	// 行内标记是排版，不该原样留在文档里。
	if strings.Contains(got.Text, "**") || strings.Contains(got.Text, "`") {
		t.Fatalf("Markdown 标记不应留在文档里:\n%s", got.Text)
	}

	if _, err := w.DocWrite("空.docx", "  \n\n"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空文档应被拒绝: %v", err)
	}
	if _, err := w.DocWrite("周报.doc", "# x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("只能存成 docx: %v", err)
	}
}

func TestSlidesWrite_生成演示文稿再读回来(t *testing.T) {
	w := workspace(t)
	deck := []Slide{
		{Title: "Q4 方案评审", Subtitle: "结算项目组 · 2026 年 10 月"},
		{Title: "背景", Bullets: []string{"对账差异长期靠人工处理", "  每月约 40 人时", "第三方回调 <不稳定> & 延迟"}},
		{Title: "方案", Bullets: []string{"- 引入对账任务队列", "- 回调失败自动重试"}},
	}
	if _, err := w.SlidesWrite("评审.pptx", deck); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	got, err := w.Read("评审.pptx", 0)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	want := "## 第 1 页\nQ4 方案评审\n结算项目组 · 2026 年 10 月\n\n" +
		"## 第 2 页\n背景\n对账差异长期靠人工处理\n每月约 40 人时\n第三方回调 <不稳定> & 延迟\n\n" +
		"## 第 3 页\n方案\n引入对账任务队列\n回调失败自动重试"
	if got.Text != want {
		t.Fatalf("读回的内容不对:\n期望\n%s\n实际\n%s", want, got.Text)
	}

	for name, bad := range map[string][]Slide{
		"没有页":   {},
		"没有标题":  {{Bullets: []string{"x"}}},
		"要点太多":  {{Title: "x", Bullets: make([]string, maxBullets+1)}},
		"页数太多":  make([]Slide, maxSlidesWrite+1),
		"扩展名不对": nil,
	} {
		path := "bad.pptx"
		if name == "扩展名不对" {
			path, bad = "bad.ppt", deck
		}
		if _, err := w.SlidesWrite(path, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: 期望 ErrInvalid，实际 %v", name, err)
		}
	}
}

func TestSheet_写公式核算读回(t *testing.T) {
	w := workspace(t)
	res, err := w.SheetWrite("报销.xlsx", []SheetEdit{{
		Sheet: "十月",
		Rows: &Block{Header: true, Values: [][]any{
			{"日期", "事项", "金额"},
			{"2026-10-08", "打车", 46.5},
			{"2026-10-09", "午餐", 38.0},
			{"2026-10-09", "高铁", 219.0},
		}},
		Cells:  []CellEdit{{Cell: "b5", Value: "合计"}, {Cell: "C5", Value: "=SUM(C2:C4)"}, {Cell: "C6", Value: "=C5/COUNT(C2:C4)"}},
		Widths: map[string]float64{"A": 14, "B": 20},
	}})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if res.Formulas != 2 || len(res.Issues) != 0 || res.Path != "报销.xlsx" {
		t.Fatalf("应有两个公式且没有问题: %+v", res)
	}

	got, err := w.SheetRead("报销.xlsx", "", "")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Sheet != "十月" || len(got.Sheets) != 1 || got.Range != "A1:C6" {
		t.Fatalf("新文件自带的空表应改成指定的名字: %+v", got)
	}
	// 公式单元格既给出公式，也给出算出来的值。
	if got.Formulas["C5"] != "=SUM(C2:C4)" || got.Rows[4][2] != "303.5" || got.Rows[4][1] != "合计" {
		t.Fatalf("合计不对: %v %v", got.Formulas, got.Rows[4])
	}
	if got.Rows[1][2] != "46.5" || got.Rows[0][0] != "日期" {
		t.Fatalf("数据不对: %v", got.Rows)
	}

	// 改已有的表：加一行、新开一个工作表、把一个公式格改成数值。
	res, err = w.SheetWrite("报销.xlsx", []SheetEdit{
		{Sheet: "十月", Cells: []CellEdit{{Cell: "C6", Value: 100}, {Cell: "A5", Value: nil}}},
		{Sheet: "汇总", Cells: []CellEdit{{Cell: "A1", Value: "十月合计"}, {Cell: "B1", Value: "='十月'!C5"}}},
	})
	if err != nil || res.Replaced == "" || res.Formulas != 2 {
		t.Fatalf("修改失败或没有给旧表留底: %+v %v", res, err)
	}
	sum, err := w.SheetRead("报销.xlsx", "汇总", "A1:B1")
	if err != nil || sum.Rows[0][1] != "303.5" || len(sum.Sheets) != 2 {
		t.Fatalf("跨表引用不对: %v %+v", err, sum)
	}
	oct, _ := w.SheetRead("报销.xlsx", "十月", "C6")
	if oct.Rows[0][0] != "100" || len(oct.Formulas) != 0 {
		t.Fatalf("公式格改成数值后不应还留着公式: %+v", oct)
	}

	// 通读预览。
	preview, err := w.Read("报销.xlsx", 0)
	if err != nil || !strings.Contains(preview.Text, "## 工作表: 十月") || !strings.Contains(preview.Text, "2026-10-08\t打车\t46.5") ||
		!strings.Contains(preview.Text, "## 工作表: 汇总") || preview.Note == "" {
		t.Fatalf("通读预览不对: %v\n%s", err, preview.Text)
	}

	// 公式有错：照样保存，但把错处报出来，交给调用方改。
	bad, err := w.SheetWrite("出错.xlsx", []SheetEdit{{Cells: []CellEdit{
		{Cell: "A1", Value: 10}, {Cell: "A2", Value: 0},
		{Cell: "B1", Value: "=A1/A2"}, {Cell: "B2", Value: "=SUM(不存在的表!A1:A3)"}, {Cell: "B3", Value: "=A1*2"},
	}}})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	problems := map[string]string{}
	for _, is := range bad.Issues {
		problems[is.Cell] = is.Problem
	}
	if bad.Formulas != 3 || problems["B1"] != "#DIV/0!" || problems["B2"] == "" || problems["B3"] != "" {
		t.Fatalf("应报出除零和无效引用，正常的公式不报: %+v", bad)
	}

	for name, fn := range map[string]func() error{
		"扩展名不对": func() error {
			_, err := w.SheetWrite("x.csv", []SheetEdit{{Cells: []CellEdit{{Cell: "A1", Value: 1}}}})
			return err
		},
		"没有内容": func() error { _, err := w.SheetWrite("x.xlsx", []SheetEdit{{Sheet: "a"}}); return err },
		"地址不合法": func() error {
			_, err := w.SheetWrite("x.xlsx", []SheetEdit{{Cells: []CellEdit{{Cell: "1A", Value: 1}}}})
			return err
		},
		"范围不合法": func() error { _, err := w.SheetRead("报销.xlsx", "十月", "C6:A1"); return err },
		"范围太大":  func() error { _, err := w.SheetRead("报销.xlsx", "十月", "A1:ZZ9999"); return err },
	} {
		if err := fn(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: 期望 ErrInvalid，实际 %v", name, err)
		}
	}
	if _, err := w.SheetRead("报销.xlsx", "没有这个表", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的工作表应报 ErrNotFound: %v", err)
	}
}

func TestRead_文本编码与分段(t *testing.T) {
	w := workspace(t)
	put := func(name string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(w.Root, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Excel 另存为 CSV 默认是 GBK。
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("姓名,部门\n张三,结算组\n"))
	if err != nil {
		t.Fatal(err)
	}
	put("名单.csv", gbk)
	got, err := w.Read("名单.csv", 0)
	if err != nil || got.Kind != "text" || got.Text != "姓名,部门\n张三,结算组\n" {
		t.Fatalf("GBK 的 CSV 应读得出来: %v %q", err, got.Text)
	}
	put("bom.txt", append([]byte{0xEF, 0xBB, 0xBF}, []byte("带 BOM 的文本")...))
	if got, _ := w.Read("bom.txt", 0); got.Text != "带 BOM 的文本" {
		t.Fatalf("BOM 应去掉: %q", got.Text)
	}
	put("utf16.txt", []byte{0xFF, 0xFE, 0x60, 0x4F, 0x7D, 0x59})
	if got, _ := w.Read("utf16.txt", 0); got.Text != "你好" {
		t.Fatalf("UTF-16 应读得出来: %q", got.Text)
	}

	long := strings.Repeat("这是一段很长的文字。", 600)
	put("长文.md", []byte(long))
	first, err := w.Read("长文.md", 0)
	if err != nil || first.NextOffset == 0 || len(first.Text) > ChunkBytes || first.Total != len(long) {
		t.Fatalf("长文件应分段: %v next=%d", err, first.NextOffset)
	}
	rest, err := w.Read("长文.md", first.NextOffset)
	if err != nil || strings.ContainsRune(first.Text+rest.Text, '\uFFFD') || !strings.HasPrefix(long, first.Text+rest.Text) {
		t.Fatalf("接着读应从字符边界继续，拼起来和原文一致: %v", err)
	}

	put("图.png", []byte{0x89, 'P', 'N', 'G', 0, 1, 2})
	put("老.doc", []byte{0xD0, 0xCF, 0x11, 0xE0})
	put("二进制.dat", []byte{0, 1, 2, 3, 0xFF, 0xFE, 0})
	put("假的.docx", []byte("这不是一个压缩包"))
	for _, name := range []string{"图.png", "老.doc", "二进制.dat", "假的.docx"} {
		if _, err := w.Read(name, 0); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: 期望 ErrUnsupported，实际 %v", name, err)
		}
	}
}

func TestReceive_收下聊天里发来的文件同名的不覆盖(t *testing.T) {
	ws, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := ws.Receive("报价单.xlsx", []byte("v1"))
	if err != nil || first != "收到的文件/报价单.xlsx" {
		t.Fatalf("收文件失败: %q %v", first, err)
	}
	second, err := ws.Receive("报价单.xlsx", []byte("v2"))
	if err != nil || second != "收到的文件/报价单 (2).xlsx" {
		t.Fatalf("同名的文件应另起一个名字: %q %v", second, err)
	}
	if got, _ := ws.Bytes(first); string(got) != "v1" {
		t.Fatalf("先收的文件不应被覆盖: %q", got)
	}
	// 文件名是别人起的：路径、隐藏文件、奇怪的字符都不能让它落到别处。
	for _, name := range []string{"../../etc/passwd", ".bashrc", "a:b*c?.txt", ""} {
		p, err := ws.Receive(name, []byte("x"))
		if err != nil || !strings.HasPrefix(p, "收到的文件/") || strings.Contains(strings.TrimPrefix(p, "收到的文件/"), "/") ||
			strings.HasPrefix(strings.TrimPrefix(p, "收到的文件/"), ".") {
			t.Fatalf("文件名 %q 应被整理后收在「收到的文件」里: %q %v", name, p, err)
		}
	}
}
