package files

import (
	"fmt"
	"strings"
)

// 生成演示文稿的办法和 Word 一样：直接写出 .pptx 压缩包里的那几份 XML，不依赖任何办公软件。
// 版式只有两种——封面（标题加副标题）和内容页（标题加若干要点），16:9，白底深色字。
// 这是"把内容整理成能拿去讲的一套幻灯片"，不是排版工具：要精修样式，在 PowerPoint 或 WPS 里接着改。

// Slide 是一页幻灯片。
type Slide struct {
	Title string `json:"title"`
	// Bullets 是这一页的要点。以两个空格或一个制表符开头的是下一级要点。
	Bullets []string `json:"bullets"`
	// Subtitle 只用于封面页（没有要点的页）。
	Subtitle string `json:"subtitle"`
}

const (
	maxSlidesWrite = 100
	maxBullets     = 12

	// 16:9 的页面尺寸，单位是 EMU（914400 EMU = 1 英寸）。
	slideW = 12192000
	slideH = 6858000

	pNS = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" ` +
		`xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`
	xmlHead = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
	pptCT   = "application/vnd.openxmlformats-officedocument.presentationml."
	// 一个空的组合属性块，每棵形状树的开头都要有。
	grpHead = `<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>` +
		`<p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr>`
)

// placeholder 生成一个占位符形状。geom 为空表示位置和大小沿用版式里的。
func placeholder(id int, name, phType string, idx int, geom, body string) string {
	ph := `<p:ph`
	if phType != "" {
		ph += ` type="` + phType + `"`
	}
	if idx > 0 {
		ph += fmt.Sprintf(` idx="%d"`, idx)
	}
	ph += `/>`
	return fmt.Sprintf(`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="%s"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr>%s</p:nvPr></p:nvSpPr>`+
		`<p:spPr>%s</p:spPr><p:txBody><a:bodyPr/><a:lstStyle/>%s</p:txBody></p:sp>`, id, name, ph, geom, body)
}

func xfrm(x, y, cx, cy int) string {
	return fmt.Sprintf(`<a:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></a:xfrm>`, x, y, cx, cy)
}

// para 生成一个段落。level 是缩进的级数。
func para(text string, level int) string {
	if strings.TrimSpace(text) == "" {
		return `<a:p><a:endParaRPr lang="zh-CN"/></a:p>`
	}
	pPr := ""
	if level > 0 {
		pPr = fmt.Sprintf(`<a:pPr lvl="%d"/>`, level)
	}
	return `<a:p>` + pPr + `<a:r><a:rPr lang="zh-CN" altLang="en-US"/><a:t>` + esc(text) + `</a:t></a:r></a:p>`
}

// slideXML 生成一页幻灯片。
func slideXML(s Slide) string {
	var body strings.Builder
	if len(s.Bullets) == 0 {
		body.WriteString(placeholder(2, "标题", "ctrTitle", 0, "", para(s.Title, 0)))
		body.WriteString(placeholder(3, "副标题", "subTitle", 1, "", para(s.Subtitle, 0)))
	} else {
		body.WriteString(placeholder(2, "标题", "title", 0, "", para(s.Title, 0)))
		var paras strings.Builder
		for _, b := range s.Bullets {
			level := 0
			if strings.HasPrefix(b, "\t") || strings.HasPrefix(b, "  ") {
				level = 1
			}
			paras.WriteString(para(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(b), "-*•·")), level))
		}
		body.WriteString(placeholder(3, "内容", "", 1, "", paras.String()))
	}
	return xmlHead + `<p:sld ` + pNS + `><p:cSld><p:spTree>` + grpHead + body.String() +
		`</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>`
}

// 各级文字的样式：字号（百分之一磅）与缩进。
func levelStyle(tag string, size, indent int, bullet bool) string {
	bu := `<a:buNone/>`
	margin := ""
	if bullet {
		bu = `<a:buFont typeface="Arial"/><a:buChar char="•"/>`
		margin = fmt.Sprintf(` marL="%d" indent="-342900"`, indent)
	}
	return fmt.Sprintf(`<a:%s%s algn="l" rtl="0" eaLnBrk="1" latinLnBrk="0" hangingPunct="1">`+
		`<a:lnSpc><a:spcPct val="100000"/></a:lnSpc><a:spcBef><a:spcPts val="1000"/></a:spcBef>%s`+
		`<a:defRPr sz="%d" kern="1200"><a:solidFill><a:schemeClr val="tx1"/></a:solidFill>`+
		`<a:latin typeface="+mn-lt"/><a:ea typeface="+mn-ea"/><a:cs typeface="+mn-cs"/></a:defRPr></a:%s>`,
		tag, margin, bu, size, tag)
}

func masterXML() string {
	title := placeholder(2, "标题占位符", "title", 0, xfrm(838200, 365125, 10515600, 1325563),
		`<a:p><a:r><a:rPr lang="zh-CN" altLang="en-US"/><a:t>标题</a:t></a:r></a:p>`)
	body := placeholder(3, "内容占位符", "body", 1, xfrm(838200, 1825625, 10515600, 4351338),
		`<a:p><a:pPr lvl="0"/><a:r><a:rPr lang="zh-CN" altLang="en-US"/><a:t>正文</a:t></a:r></a:p>`)
	return xmlHead + `<p:sldMaster ` + pNS + `><p:cSld>` +
		`<p:bg><p:bgRef idx="1001"><a:schemeClr val="bg1"/></p:bgRef></p:bg>` +
		`<p:spTree>` + grpHead + title + body + `</p:spTree></p:cSld>` +
		`<p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" ` +
		`accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/>` +
		`<p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/><p:sldLayoutId id="2147483650" r:id="rId2"/></p:sldLayoutIdLst>` +
		`<p:txStyles><p:titleStyle><a:lvl1pPr algn="l" rtl="0" eaLnBrk="1" latinLnBrk="0" hangingPunct="1">` +
		`<a:lnSpc><a:spcPct val="90000"/></a:lnSpc><a:spcBef><a:spcPct val="0"/></a:spcBef><a:buNone/>` +
		`<a:defRPr sz="4000" b="1" kern="1200"><a:solidFill><a:schemeClr val="tx1"/></a:solidFill>` +
		`<a:latin typeface="+mj-lt"/><a:ea typeface="+mj-ea"/><a:cs typeface="+mj-cs"/></a:defRPr></a:lvl1pPr></p:titleStyle>` +
		`<p:bodyStyle>` + levelStyle("lvl1pPr", 2400, 342900, true) + levelStyle("lvl2pPr", 2000, 800100, true) +
		levelStyle("lvl3pPr", 1800, 1257300, true) + `</p:bodyStyle>` +
		`<p:otherStyle>` + levelStyle("lvl1pPr", 1800, 0, false) + `</p:otherStyle></p:txStyles></p:sldMaster>`
}

// layoutXML 生成一个版式。cover 为真是封面，否则是内容页。
func layoutXML(cover bool) string {
	if cover {
		title := placeholder(2, "标题", "ctrTitle", 0, xfrm(1524000, 1122363, 9144000, 2387600),
			`<a:p><a:r><a:rPr lang="zh-CN" altLang="en-US"/><a:t>标题</a:t></a:r></a:p>`)
		sub := placeholder(3, "副标题", "subTitle", 1, xfrm(1524000, 3602038, 9144000, 1655762),
			`<a:p><a:r><a:rPr lang="zh-CN" altLang="en-US"/><a:t>副标题</a:t></a:r></a:p>`)
		// 封面的标题居中、放大；副标题不带项目符号。
		title = strings.Replace(title, `<a:bodyPr/><a:lstStyle/>`,
			`<a:bodyPr anchor="b"/><a:lstStyle><a:lvl1pPr algn="ctr"><a:defRPr sz="5400"/></a:lvl1pPr></a:lstStyle>`, 1)
		sub = strings.Replace(sub, `<a:bodyPr/><a:lstStyle/>`,
			`<a:bodyPr/><a:lstStyle><a:lvl1pPr marL="0" indent="0" algn="ctr"><a:buNone/><a:defRPr sz="2400"/></a:lvl1pPr></a:lstStyle>`, 1)
		return xmlHead + `<p:sldLayout ` + pNS + ` type="title" preserve="1"><p:cSld name="封面"><p:spTree>` + grpHead + title + sub +
			`</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sldLayout>`
	}
	title := placeholder(2, "标题", "title", 0, "", `<a:p><a:r><a:rPr lang="zh-CN" altLang="en-US"/><a:t>标题</a:t></a:r></a:p>`)
	body := placeholder(3, "内容", "", 1, "", `<a:p><a:pPr lvl="0"/><a:r><a:rPr lang="zh-CN" altLang="en-US"/><a:t>正文</a:t></a:r></a:p>`)
	return xmlHead + `<p:sldLayout ` + pNS + ` type="obj" preserve="1"><p:cSld name="标题和内容"><p:spTree>` + grpHead + title + body +
		`</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sldLayout>`
}

// themeXML 是主题：配色和字体。中文用微软雅黑，西文用 Arial——几乎每台机器上都有。
func themeXML() string {
	clr := func(name, hex string) string {
		return `<a:` + name + `><a:srgbClr val="` + hex + `"/></a:` + name + `>`
	}
	fonts := func(tag string) string {
		return `<a:` + tag + `><a:latin typeface="Arial"/><a:ea typeface="微软雅黑"/><a:cs typeface="Arial"/></a:` + tag + `>`
	}
	solid := `<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>`
	line := func(w int) string {
		return fmt.Sprintf(`<a:ln w="%d" cap="flat" cmpd="sng" algn="ctr">%s<a:prstDash val="solid"/><a:miter lim="800000"/></a:ln>`, w, solid)
	}
	effect := `<a:effectStyle><a:effectLst/></a:effectStyle>`
	return xmlHead + `<a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="原子助手"><a:themeElements>` +
		`<a:clrScheme name="原子助手">` +
		`<a:dk1><a:sysClr val="windowText" lastClr="000000"/></a:dk1><a:lt1><a:sysClr val="window" lastClr="FFFFFF"/></a:lt1>` +
		clr("dk2", "1F2D3D") + clr("lt2", "F2F4F7") + clr("accent1", "2F6FED") + clr("accent2", "ED7D31") + clr("accent3", "2EA36A") +
		clr("accent4", "FFC000") + clr("accent5", "8064A2") + clr("accent6", "D9534F") + clr("hlink", "0563C1") + clr("folHlink", "954F72") +
		`</a:clrScheme>` +
		`<a:fontScheme name="原子助手">` + fonts("majorFont") + fonts("minorFont") + `</a:fontScheme>` +
		`<a:fmtScheme name="原子助手">` +
		`<a:fillStyleLst>` + solid + solid + solid + `</a:fillStyleLst>` +
		`<a:lnStyleLst>` + line(6350) + line(12700) + line(19050) + `</a:lnStyleLst>` +
		`<a:effectStyleLst>` + effect + effect + effect + `</a:effectStyleLst>` +
		`<a:bgFillStyleLst>` + solid + solid + solid + `</a:bgFillStyleLst>` +
		`</a:fmtScheme></a:themeElements><a:objectDefaults/><a:extraClrSchemeLst/></a:theme>`
}

func rels(items ...[2]string) string {
	var sb strings.Builder
	sb.WriteString(xmlHead + `<Relationships ` + relsNS + `>`)
	for i, it := range items {
		fmt.Fprintf(&sb, `<Relationship Id="rId%d" Type="%s/%s" Target="%s"/>`, i+1, officeRelNS, it[0], it[1])
	}
	sb.WriteString(`</Relationships>`)
	return sb.String()
}

// SlidesWrite 生成一份演示文稿。
func (w *Workspace) SlidesWrite(rel string, slides []Slide) (Saved, error) {
	if _, norm, err := w.resolve(rel, true); err != nil {
		return Saved{}, err
	} else if ext(norm) != "pptx" {
		return Saved{}, fmt.Errorf("%w: 演示文稿只能存成 .pptx", ErrInvalid)
	}
	if len(slides) == 0 {
		return Saved{}, fmt.Errorf("%w: 一页幻灯片都没有", ErrInvalid)
	}
	if len(slides) > maxSlidesWrite {
		return Saved{}, fmt.Errorf("%w: 一次最多生成 %d 页", ErrInvalid, maxSlidesWrite)
	}
	for i, s := range slides {
		if strings.TrimSpace(s.Title) == "" {
			return Saved{}, fmt.Errorf("%w: 第 %d 页没有标题", ErrInvalid, i+1)
		}
		// 一页塞太多要点，字会小到看不清或者溢出页面。让调用方自己拆页，比悄悄截掉好。
		if len(s.Bullets) > maxBullets {
			return Saved{}, fmt.Errorf("%w: 第 %d 页有 %d 条要点，一页最多 %d 条，请拆成几页", ErrInvalid, i+1, len(s.Bullets), maxBullets)
		}
	}

	types := xmlHead + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/ppt/presentation.xml" ContentType="` + pptCT + `presentation.main+xml"/>` +
		`<Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="` + pptCT + `slideMaster+xml"/>` +
		`<Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="` + pptCT + `slideLayout+xml"/>` +
		`<Override PartName="/ppt/slideLayouts/slideLayout2.xml" ContentType="` + pptCT + `slideLayout+xml"/>` +
		`<Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>`
	// 演示文稿的关系表：第 1 项是母版，之后每页一项，最后是主题。
	presRels := [][2]string{{"slideMaster", "slideMasters/slideMaster1.xml"}}
	var ids strings.Builder
	parts := []zipPart{}
	for i, s := range slides {
		n := i + 1
		types += fmt.Sprintf(`<Override PartName="/ppt/slides/slide%d.xml" ContentType="%sslide+xml"/>`, n, pptCT)
		presRels = append(presRels, [2]string{"slide", fmt.Sprintf("slides/slide%d.xml", n)})
		fmt.Fprintf(&ids, `<p:sldId id="%d" r:id="rId%d"/>`, 255+n, n+1)
		layout := "../slideLayouts/slideLayout2.xml"
		if len(s.Bullets) == 0 {
			layout = "../slideLayouts/slideLayout1.xml"
		}
		parts = append(parts,
			zipPart{fmt.Sprintf("ppt/slides/slide%d.xml", n), slideXML(s)},
			zipPart{fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", n), rels([2]string{"slideLayout", layout})})
	}
	types += `</Types>`
	presRels = append(presRels, [2]string{"theme", "theme/theme1.xml"})

	presentation := xmlHead + `<p:presentation ` + pNS + ` saveSubsetFonts="1">` +
		`<p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst>` +
		`<p:sldIdLst>` + ids.String() + `</p:sldIdLst>` +
		fmt.Sprintf(`<p:sldSz cx="%d" cy="%d"/><p:notesSz cx="%d" cy="%d"/>`, slideW, slideH, slideH, 9144000) +
		`</p:presentation>`

	all := append([]zipPart{
		{"[Content_Types].xml", types},
		{"_rels/.rels", rels([2]string{"officeDocument", "ppt/presentation.xml"})},
		{"ppt/presentation.xml", presentation},
		{"ppt/_rels/presentation.xml.rels", rels(presRels...)},
		{"ppt/slideMasters/slideMaster1.xml", masterXML()},
		{"ppt/slideMasters/_rels/slideMaster1.xml.rels", rels(
			[2]string{"slideLayout", "../slideLayouts/slideLayout1.xml"},
			[2]string{"slideLayout", "../slideLayouts/slideLayout2.xml"},
			[2]string{"theme", "../theme/theme1.xml"})},
		{"ppt/slideLayouts/slideLayout1.xml", layoutXML(true)},
		{"ppt/slideLayouts/_rels/slideLayout1.xml.rels", rels([2]string{"slideMaster", "../slideMasters/slideMaster1.xml"})},
		{"ppt/slideLayouts/slideLayout2.xml", layoutXML(false)},
		{"ppt/slideLayouts/_rels/slideLayout2.xml.rels", rels([2]string{"slideMaster", "../slideMasters/slideMaster1.xml"})},
		{"ppt/theme/theme1.xml", themeXML()},
	}, parts...)
	return w.save(rel, func(tmp string) error { return writeZip(tmp, all) })
}
