package files

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
)

// 生成 Word 文档的办法：让模型写它最拿手的 Markdown，这里把它排成一份规矩的 .docx。
// 支持标题、段落、加粗、斜体、行内代码、无序和有序列表、表格——日常的通知、纪要、报告够用了。
//
// .docx 是一个压缩包，里面是几份 XML。这里直接生成这几份 XML，不依赖任何办公软件。

// block 是文档里的一块内容。
type block struct {
	kind  string // heading / para / bullet / number / table
	level int
	text  string
	rows  [][]string
	// list 是有序列表的编号，同一个列表里的各项相同，用来让每个列表各自从 1 开始。
	list int
}

var (
	reHeading  = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*$`)
	reBullet   = regexp.MustCompile(`^\s*[-*+]\s+(.+)$`)
	reNumbered = regexp.MustCompile(`^\s*\d+[.)、]\s*(.+)$`)
	reTableSep = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	reInline   = regexp.MustCompile("(\\*\\*[^*\n]+\\*\\*|__[^_\n]+__|\\*[^*\n]+\\*|`[^`\n]+`)")
)

// parseMarkdown 把 Markdown 拆成内容块。认不出的写法按普通段落处理，不报错。
func parseMarkdown(md string) []block {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var out []block
	var para []string
	lists := 0
	flush := func() {
		if len(para) > 0 {
			out = append(out, block{kind: "para", text: strings.Join(para, " ")})
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flush()
		case reHeading.MatchString(trimmed):
			flush()
			m := reHeading.FindStringSubmatch(trimmed)
			out = append(out, block{kind: "heading", level: len(m[1]), text: m[2]})
		case strings.HasPrefix(trimmed, "|") && i+1 < len(lines) && reTableSep.MatchString(lines[i+1]):
			flush()
			var rows [][]string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				if reTableSep.MatchString(lines[i]) {
					continue
				}
				rows = append(rows, tableCells(lines[i]))
			}
			i--
			out = append(out, block{kind: "table", rows: rows})
		case reBullet.MatchString(line):
			flush()
			out = append(out, block{kind: "bullet", text: reBullet.FindStringSubmatch(line)[1]})
		case reNumbered.MatchString(line):
			flush()
			// 紧挨着上一项的是同一个列表，否则另起一个，编号从 1 重新开始。
			if n := len(out); n == 0 || out[n-1].kind != "number" {
				lists++
			}
			out = append(out, block{kind: "number", text: reNumbered.FindStringSubmatch(line)[1], list: lists})
		default:
			para = append(para, trimmed)
		}
	}
	flush()
	return out
}

func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func esc(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// runs 把一段带行内标记的文字变成 Word 的文字块。bold 为真时整段加粗。
func runs(text string, bold bool) string {
	var sb strings.Builder
	emit := func(s string, b, i, code bool) {
		if s == "" {
			return
		}
		sb.WriteString("<w:r>")
		if b || i || code {
			sb.WriteString("<w:rPr>")
			if code {
				sb.WriteString(`<w:rFonts w:ascii="Consolas" w:hAnsi="Consolas"/>`)
			}
			if b {
				sb.WriteString("<w:b/><w:bCs/>")
			}
			if i {
				sb.WriteString("<w:i/><w:iCs/>")
			}
			sb.WriteString("</w:rPr>")
		}
		sb.WriteString(`<w:t xml:space="preserve">` + esc(s) + "</w:t></w:r>")
	}
	last := 0
	for _, loc := range reInline.FindAllStringIndex(text, -1) {
		emit(text[last:loc[0]], bold, false, false)
		tok := text[loc[0]:loc[1]]
		switch {
		case strings.HasPrefix(tok, "**"), strings.HasPrefix(tok, "__"):
			emit(tok[2:len(tok)-2], true, false, false)
		case strings.HasPrefix(tok, "`"):
			emit(tok[1:len(tok)-1], bold, false, true)
		default:
			emit(tok[1:len(tok)-1], bold, true, false)
		}
		last = loc[1]
	}
	emit(text[last:], bold, false, false)
	return sb.String()
}

const (
	docNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`
	// 无序列表用编号定义 1；每个有序列表各用一个，从 2 往后排。
	bulletNum = 1
)

// documentXML 生成正文。
func documentXML(blocks []block) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	sb.WriteString(`<w:document ` + docNS + `><w:body>`)
	for _, b := range blocks {
		switch b.kind {
		case "heading":
			fmt.Fprintf(&sb, `<w:p><w:pPr><w:pStyle w:val="Heading%d"/></w:pPr>%s</w:p>`, b.level, runs(b.text, false))
		case "bullet":
			fmt.Fprintf(&sb, `<w:p><w:pPr><w:pStyle w:val="ListParagraph"/><w:numPr><w:ilvl w:val="0"/><w:numId w:val="%d"/></w:numPr></w:pPr>%s</w:p>`,
				bulletNum, runs(b.text, false))
		case "number":
			fmt.Fprintf(&sb, `<w:p><w:pPr><w:pStyle w:val="ListParagraph"/><w:numPr><w:ilvl w:val="0"/><w:numId w:val="%d"/></w:numPr></w:pPr>%s</w:p>`,
				bulletNum+b.list, runs(b.text, false))
		case "table":
			sb.WriteString(tableXML(b.rows))
		default:
			sb.WriteString(`<w:p>` + runs(b.text, false) + `</w:p>`)
		}
	}
	// A4 纸，四边各 2.54 厘米。
	sb.WriteString(`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/>` +
		`<w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="851" w:footer="992" w:gutter="0"/></w:sectPr>`)
	sb.WriteString(`</w:body></w:document>`)
	return sb.String()
}

func tableXML(rows [][]string) string {
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return ""
	}
	// 版心宽 9026（A4 减去两边页边距），各列均分。
	width := 9026 / cols
	var sb strings.Builder
	sb.WriteString(`<w:tbl><w:tblPr><w:tblStyle w:val="TableGrid"/><w:tblW w:w="0" w:type="auto"/></w:tblPr><w:tblGrid>`)
	for i := 0; i < cols; i++ {
		fmt.Fprintf(&sb, `<w:gridCol w:w="%d"/>`, width)
	}
	sb.WriteString(`</w:tblGrid>`)
	for r, row := range rows {
		sb.WriteString(`<w:tr>`)
		for c := 0; c < cols; c++ {
			text := ""
			if c < len(row) {
				text = row[c]
			}
			// 第一行是表头，加粗。
			fmt.Fprintf(&sb, `<w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/></w:tcPr><w:p>%s</w:p></w:tc>`, width, runs(text, r == 0))
		}
		sb.WriteString(`</w:tr>`)
	}
	// 表格后面跟一个空段落：两张表挨在一起时 Word 会把它们并成一张。
	sb.WriteString(`</w:tbl><w:p/>`)
	return sb.String()
}

// numberingXML 生成列表的编号定义。lists 是有序列表的个数。
func numberingXML(lists int) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	sb.WriteString(`<w:numbering ` + docNS + `>`)
	sb.WriteString(`<w:abstractNum w:abstractNumId="0"><w:multiLevelType w:val="hybridMultilevel"/>` +
		`<w:lvl w:ilvl="0"><w:start w:val="1"/><w:numFmt w:val="bullet"/><w:lvlText w:val="•"/><w:lvlJc w:val="left"/>` +
		`<w:pPr><w:ind w:left="420" w:hanging="420"/></w:pPr></w:lvl></w:abstractNum>`)
	sb.WriteString(`<w:abstractNum w:abstractNumId="1"><w:multiLevelType w:val="hybridMultilevel"/>` +
		`<w:lvl w:ilvl="0"><w:start w:val="1"/><w:numFmt w:val="decimal"/><w:lvlText w:val="%1."/><w:lvlJc w:val="left"/>` +
		`<w:pPr><w:ind w:left="420" w:hanging="420"/></w:pPr></w:lvl></w:abstractNum>`)
	fmt.Fprintf(&sb, `<w:num w:numId="%d"><w:abstractNumId w:val="0"/></w:num>`, bulletNum)
	for i := 1; i <= lists; i++ {
		fmt.Fprintf(&sb, `<w:num w:numId="%d"><w:abstractNumId w:val="1"/>`+
			`<w:lvlOverride w:ilvl="0"><w:startOverride w:val="1"/></w:lvlOverride></w:num>`, bulletNum+i)
	}
	sb.WriteString(`</w:numbering>`)
	return sb.String()
}

// stylesXML 是文档的样式：正文宋体五号，标题黑体，表格带框线——中文公文和报告最常见的样子。
func stylesXML() string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	sb.WriteString(`<w:styles ` + docNS + `>`)
	sb.WriteString(`<w:docDefaults><w:rPrDefault><w:rPr>` +
		`<w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="宋体" w:cs="Times New Roman"/>` +
		`<w:sz w:val="21"/><w:szCs w:val="21"/><w:lang w:val="en-US" w:eastAsia="zh-CN"/></w:rPr></w:rPrDefault>` +
		`<w:pPrDefault><w:pPr><w:spacing w:after="120" w:line="360" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults>`)
	sb.WriteString(`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/></w:style>`)
	sizes := []int{44, 32, 28, 24, 21, 21}
	for i, size := range sizes {
		fmt.Fprintf(&sb, `<w:style w:type="paragraph" w:styleId="Heading%d"><w:name w:val="heading %d"/><w:basedOn w:val="Normal"/>`+
			`<w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="240" w:after="120"/><w:outlineLvl w:val="%d"/></w:pPr>`+
			`<w:rPr><w:rFonts w:ascii="Arial" w:hAnsi="Arial" w:eastAsia="黑体"/><w:b/><w:bCs/><w:sz w:val="%d"/><w:szCs w:val="%d"/></w:rPr></w:style>`,
			i+1, i+1, i, size, size)
	}
	sb.WriteString(`<w:style w:type="paragraph" w:styleId="ListParagraph"><w:name w:val="List Paragraph"/><w:basedOn w:val="Normal"/><w:qFormat/>` +
		`<w:pPr><w:spacing w:after="60"/></w:pPr></w:style>`)
	sb.WriteString(`<w:style w:type="table" w:default="1" w:styleId="TableNormal"><w:name w:val="Normal Table"/><w:tblPr><w:tblInd w:w="0" w:type="dxa"/>` +
		`<w:tblCellMar><w:top w:w="0" w:type="dxa"/><w:left w:w="108" w:type="dxa"/><w:bottom w:w="0" w:type="dxa"/><w:right w:w="108" w:type="dxa"/></w:tblCellMar></w:tblPr></w:style>`)
	border := func(side string) string {
		return `<w:` + side + ` w:val="single" w:sz="4" w:space="0" w:color="auto"/>`
	}
	sb.WriteString(`<w:style w:type="table" w:styleId="TableGrid"><w:name w:val="Table Grid"/><w:basedOn w:val="TableNormal"/><w:tblPr><w:tblBorders>` +
		border("top") + border("left") + border("bottom") + border("right") + border("insideH") + border("insideV") +
		`</w:tblBorders></w:tblPr></w:style>`)
	sb.WriteString(`</w:styles>`)
	return sb.String()
}

const (
	docxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
		`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>` +
		`<Override PartName="/word/numbering.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/>` +
		`</Types>`
	relsNS       = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
	officeRelNS  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	docxRootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships ` + relsNS + `><Relationship Id="rId1" Type="` + officeRelNS + `/officeDocument" Target="word/document.xml"/></Relationships>`
	docxDocRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships ` + relsNS + `>` +
		`<Relationship Id="rId1" Type="` + officeRelNS + `/styles" Target="styles.xml"/>` +
		`<Relationship Id="rId2" Type="` + officeRelNS + `/numbering" Target="numbering.xml"/>` +
		`</Relationships>`
)

// zipPart 是要写进压缩包的一个部件。
type zipPart struct {
	name, body string
}

// writeZip 把若干部件打成一个压缩包，返回它的内容。
func writeZip(parts []zipPart) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		w, err := zw.Create(p.name)
		if err != nil {
			return nil, fmt.Errorf("files: 生成文件失败: %w", err)
		}
		if _, err := w.Write([]byte(p.body)); err != nil {
			return nil, fmt.Errorf("files: 生成文件失败: %w", err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("files: 生成文件失败: %w", err)
	}
	return buf.Bytes(), nil
}

// DocWrite 把一段 Markdown 排成 Word 文档。
func (w *Workspace) DocWrite(rel, markdown string) (Saved, error) {
	if _, norm, err := w.resolve(rel, true); err != nil {
		return Saved{}, err
	} else if ext(norm) != "docx" {
		return Saved{}, fmt.Errorf("%w: Word 文档只能存成 .docx", ErrInvalid)
	}
	blocks := parseMarkdown(markdown)
	if len(blocks) == 0 {
		return Saved{}, fmt.Errorf("%w: 文档内容是空的", ErrInvalid)
	}
	lists := 0
	for _, b := range blocks {
		if b.list > lists {
			lists = b.list
		}
	}
	return w.save(rel, byAssistant, func() ([]byte, error) {
		return writeZip([]zipPart{
			{"[Content_Types].xml", docxContentTypes},
			{"_rels/.rels", docxRootRels},
			{"word/document.xml", documentXML(blocks)},
			{"word/_rels/document.xml.rels", docxDocRels},
			{"word/styles.xml", stylesXML()},
			{"word/numbering.xml", numberingXML(lists)},
		})
	})
}
