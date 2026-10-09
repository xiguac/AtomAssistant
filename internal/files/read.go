package files

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	// ChunkBytes 是一次读取返回的文本长度。内容要进模型的上下文，长文件分段读。
	ChunkBytes = 3000
	// maxXMLBytes 是从压缩包里解出单个部件的大小上限，挡住压缩炸弹。
	maxXMLBytes = 32 << 20
	// maxSlides 与 maxPages 是幻灯片和 PDF 的页数上限。
	maxSlides = 500
	maxPages  = 500
	// previewRows 与 previewCols 是通读表格时每个工作表展示的行数和列数。
	previewRows = 60
	previewCols = 30
)

// Excerpt 是一个文件里的一段文字。
type Excerpt struct {
	Path string `json:"path"`
	// Kind 是文件的种类：docx / pptx / xlsx / pdf / text。
	Kind string `json:"kind"`
	Text string `json:"text"`
	// NextOffset 非零表示后面还有，下一次从这里接着读。
	NextOffset int `json:"next_offset,omitempty"`
	// Total 是全文的字节数，让调用方知道读到哪了。
	Total int    `json:"total_bytes"`
	Note  string `json:"note,omitempty"`
}

// Read 把一个文件读成文字，从 offset 字节处开始取一段。
func (w *Workspace) Read(rel string, offset int) (Excerpt, error) {
	abs, norm, err := w.existing(rel)
	if err != nil {
		return Excerpt{}, err
	}
	out := Excerpt{Path: norm}
	var text string
	switch e := ext(norm); e {
	case "docx":
		out.Kind = "docx"
		text, err = readDocx(abs)
	case "pptx":
		out.Kind = "pptx"
		text, err = readPptx(abs)
	case "xlsx", "xlsm":
		out.Kind = "xlsx"
		text, err = readXlsx(abs)
		out.Note = "这是通读用的预览，每个工作表只列了开头一部分。要看具体的单元格或公式，用表格读取工具指明范围。"
	case "pdf":
		out.Kind = "pdf"
		text, err = readPDF(abs)
	case "doc", "xls", "ppt":
		return out, fmt.Errorf("%w: .%s 是老版本的 Office 格式。请先在 Office 或 WPS 里另存为 .%sx 再放进来", ErrUnsupported, e, e)
	case "png", "jpg", "jpeg", "gif", "webp", "bmp", "zip", "rar", "7z", "tar", "gz", "exe", "dll", "mp3", "mp4", "mov":
		return out, fmt.Errorf("%w: .%s 文件读不成文字", ErrUnsupported, e)
	default:
		out.Kind = "text"
		text, err = readText(abs)
	}
	if err != nil {
		return out, err
	}
	out.Total = len(text)
	out.Text, out.NextOffset = slice(text, offset, ChunkBytes)
	return out, nil
}

// slice 从 offset 处取一段不超过 size 字节的文字，两头都落在字符边界上。
func slice(text string, offset, size int) (part string, next int) {
	if offset < 0 || offset > len(text) {
		offset = len(text)
	}
	for offset > 0 && offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset--
	}
	end := offset + size
	if end >= len(text) {
		return text[offset:], 0
	}
	for end > offset && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[offset:end], end
}

// readText 读取纯文本，认得 UTF-8、UTF-16 和 GBK。
//
// 国内的 CSV 和文本文件大量是 GBK 编码的（Excel 另存为 CSV 默认就是），
// 当成 UTF-8 读出来是一屏乱码。
func readText(abs string) (string, error) {
	raw, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("files: 读取文件失败: %w", err)
	}
	return decodeText(raw)
}

func decodeText(raw []byte) (string, error) {
	switch {
	case bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}):
		raw = raw[3:]
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}), bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		big := raw[0] == 0xFE
		raw = raw[2:]
		units := make([]uint16, 0, len(raw)/2)
		for i := 0; i+1 < len(raw); i += 2 {
			if big {
				units = append(units, uint16(raw[i])<<8|uint16(raw[i+1]))
			} else {
				units = append(units, uint16(raw[i+1])<<8|uint16(raw[i]))
			}
		}
		return string(utf16.Decode(units)), nil
	}
	if utf8.Valid(raw) {
		if bytes.IndexByte(raw, 0) >= 0 {
			return "", fmt.Errorf("%w: 这不是文本文件", ErrUnsupported)
		}
		return string(raw), nil
	}
	out, err := simplifiedchinese.GB18030.NewDecoder().Bytes(raw)
	if err != nil || bytes.IndexByte(out, 0) >= 0 || bytes.ContainsRune(out, utf8.RuneError) {
		return "", fmt.Errorf("%w: 这不是文本文件，或者用了认不出的编码", ErrUnsupported)
	}
	return string(out), nil
}

// part 从 Office 文件（一个压缩包）里读出一个部件。
func part(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("files: 打开文件内部的 %s 失败: %w", name, err)
		}
		defer rc.Close()
		raw, err := io.ReadAll(io.LimitReader(rc, maxXMLBytes+1))
		if err != nil {
			return nil, fmt.Errorf("files: 读取文件内部的 %s 失败: %w", name, err)
		}
		if len(raw) > maxXMLBytes {
			return nil, fmt.Errorf("%w: 文件内部的 %s 大得不正常", ErrUnsupported, name)
		}
		return raw, nil
	}
	return nil, fmt.Errorf("%w: 文件里没有 %s，它可能不是一个正常的 Office 文件", ErrUnsupported, name)
}

func openZip(abs string) (*zip.ReadCloser, error) {
	zr, err := zip.OpenReader(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: 文件打不开，可能已损坏或者加了密码", ErrUnsupported)
	}
	return zr, nil
}

// attr 取一个 XML 属性的值，不管命名空间。
func attr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// readDocx 把 Word 文档读成带一点结构的文字：标题前加 #，列表项前加 -，表格按行用 | 隔开。
func readDocx(abs string) (string, error) {
	zr, err := openZip(abs)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	raw, err := part(&zr.Reader, "word/document.xml")
	if err != nil {
		return "", err
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var out strings.Builder
	var para strings.Builder
	var cells []string
	var cell strings.Builder
	inTable, inText := 0, false
	style, isList := "", false

	flush := func() {
		text := strings.TrimSpace(para.String())
		para.Reset()
		if inTable > 0 {
			if text != "" {
				if cell.Len() > 0 {
					cell.WriteString(" ")
				}
				cell.WriteString(text)
			}
			return
		}
		if text == "" {
			return
		}
		switch level := headingLevel(style); {
		case level > 0:
			out.WriteString(strings.Repeat("#", level) + " " + text + "\n\n")
		case isList:
			out.WriteString("- " + text + "\n")
		default:
			out.WriteString(text + "\n\n")
		}
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: 文档内容解析失败，文件可能已损坏", ErrUnsupported)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				style, isList = "", false
			case "pStyle":
				style = attr(t, "val")
			case "numPr":
				isList = true
			case "t":
				inText = true
			case "tab":
				para.WriteString("\t")
			case "br", "cr":
				para.WriteString("\n")
			case "tbl":
				inTable++
			case "tr":
				cells = nil
			case "tc":
				cell.Reset()
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				flush()
			case "t":
				inText = false
			case "tc":
				cells = append(cells, strings.TrimSpace(cell.String()))
				cell.Reset()
			case "tr":
				if inTable == 1 {
					out.WriteString("| " + strings.Join(cells, " | ") + " |\n")
				}
			case "tbl":
				inTable--
				if inTable == 0 {
					out.WriteString("\n")
				}
			}
		case xml.CharData:
			if inText {
				para.Write(t)
			}
		}
	}
	return strings.TrimSpace(out.String()), nil
}

// headingLevel 从段落样式名看出它是几级标题。认不出返回 0。
//
// 英文版 Word 的样式叫 Heading1，中文版里常常就是 1、2、3；Title 是文档标题。
func headingLevel(style string) int {
	s := strings.ToLower(strings.TrimSpace(style))
	if s == "title" {
		return 1
	}
	s = strings.TrimPrefix(s, "heading")
	if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 6 {
		return n
	}
	return 0
}

// readPptx 把演示文稿读成文字：每页一节，页内每个段落一行。
func readPptx(abs string) (string, error) {
	zr, err := openZip(abs)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	names := slideOrder(&zr.Reader)
	if len(names) == 0 {
		return "", fmt.Errorf("%w: 文件里没有幻灯片，它可能不是一个正常的 PowerPoint 文件", ErrUnsupported)
	}
	if len(names) > maxSlides {
		names = names[:maxSlides]
	}
	var out strings.Builder
	for i, name := range names {
		raw, err := part(&zr.Reader, name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "## 第 %d 页\n", i+1)
		for _, line := range drawingText(raw) {
			out.WriteString(line + "\n")
		}
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String()), nil
}

// slideOrder 按放映顺序给出各页幻灯片在压缩包里的路径。
//
// 文件名里的编号不是顺序：把第 5 页拖到最前面，它的文件名仍然是 slide5.xml。
// 真正的顺序记在 presentation.xml 里。读不出来时退回按文件名编号排。
func slideOrder(zr *zip.Reader) []string {
	if ordered := slideOrderFromPresentation(zr); len(ordered) > 0 {
		return ordered
	}
	type numbered struct {
		n    int
		name string
	}
	var found []numbered
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "ppt/slides/slide") || !strings.HasSuffix(f.Name, ".xml") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(f.Name, "ppt/slides/slide"), ".xml"))
		if err != nil {
			continue
		}
		found = append(found, numbered{n, f.Name})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].n < found[j].n })
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.name)
	}
	return out
}

func slideOrderFromPresentation(zr *zip.Reader) []string {
	relsRaw, err := part(zr, "ppt/_rels/presentation.xml.rels")
	if err != nil {
		return nil
	}
	var rels struct {
		Items []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if xml.Unmarshal(relsRaw, &rels) != nil {
		return nil
	}
	targets := make(map[string]string, len(rels.Items))
	for _, r := range rels.Items {
		targets[r.ID] = r.Target
	}
	presRaw, err := part(zr, "ppt/presentation.xml")
	if err != nil {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(presRaw))
	var out []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sldId" {
			continue
		}
		// r:id 与 id 两个属性的本地名都是 id，要的是带关系命名空间的那个。
		target := ""
		for _, a := range se.Attr {
			if a.Name.Local == "id" && strings.Contains(a.Name.Space, "relationships") {
				target = targets[a.Value]
			}
		}
		if target == "" {
			return nil
		}
		out = append(out, path.Clean("ppt/"+strings.TrimPrefix(target, "/ppt/")))
	}
	return out
}

// drawingText 取出一页幻灯片里的文字，每个段落一行。
func drawingText(raw []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var lines []string
	var para strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "br":
				para.WriteString(" ")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if line := strings.TrimSpace(para.String()); line != "" {
					lines = append(lines, line)
				}
				para.Reset()
			}
		case xml.CharData:
			if inText {
				para.Write(t)
			}
		}
	}
	return lines
}

// openXlsx 打开一个表格文件，带上解压大小的限制。
func openXlsx(abs string) (*excelize.File, error) {
	f, err := excelize.OpenFile(abs, excelize.Options{UnzipSizeLimit: 512 << 20, UnzipXMLSizeLimit: maxXMLBytes})
	if err != nil {
		return nil, fmt.Errorf("%w: 表格打不开，可能已损坏或者加了密码: %v", ErrUnsupported, err)
	}
	return f, nil
}

// readXlsx 给出表格的通读预览：每个工作表的范围和开头若干行。
func readXlsx(abs string) (string, error) {
	f, err := openXlsx(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var out strings.Builder
	for _, sheet := range f.GetSheetList() {
		fmt.Fprintf(&out, "## 工作表: %s（范围 %s）\n", sheet, usedRange(f, sheet))
		rows, err := f.GetRows(sheet)
		if err != nil {
			fmt.Fprintf(&out, "（读取失败: %v）\n\n", err)
			continue
		}
		for i, row := range rows {
			if i >= previewRows {
				fmt.Fprintf(&out, "…（共 %d 行，这里只列了前 %d 行）\n", len(rows), previewRows)
				break
			}
			if len(row) > previewCols {
				row = append(row[:previewCols:previewCols], "…")
			}
			for j, v := range row {
				// 单元格里的换行和制表符会打乱行列，换成空格。
				row[j] = strings.Join(strings.Fields(v), " ")
			}
			out.WriteString(strings.Join(row, "\t") + "\n")
		}
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String()), nil
}

// readPDF 取出 PDF 里的文字。扫描件（整页是图片）取不出文字，会如实说明。
func readPDF(abs string) (text string, err error) {
	// 这个库遇到不规范的 PDF 会 panic 而不是返回错误。
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("%w: 这个 PDF 解析不了: %v", ErrUnsupported, r)
		}
	}()
	file, reader, err := pdf.Open(abs)
	if err != nil {
		return "", fmt.Errorf("%w: PDF 打不开，可能已损坏或者加了密码", ErrUnsupported)
	}
	defer file.Close()
	pages := reader.NumPage()
	if pages > maxPages {
		pages = maxPages
	}
	var out strings.Builder
	empty := 0
	for i := 1; i <= pages; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		content, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}
		content = strings.TrimSpace(content)
		if content == "" {
			empty++
			continue
		}
		fmt.Fprintf(&out, "## 第 %d 页\n%s\n\n", i, content)
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("%w: 这个 PDF 里取不出文字，多半是扫描件（每一页都是图片）", ErrUnsupported)
	}
	if empty > 0 {
		fmt.Fprintf(&out, "（另有 %d 页没有可提取的文字，可能是图片）\n", empty)
	}
	return strings.TrimSpace(out.String()), nil
}
