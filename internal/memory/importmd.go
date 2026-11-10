package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/xiguac/atomassistant/internal/core"
)

// 导入其他助手导出的记忆。
//
// 各家导出的是同一种内容（人口统计、兴趣和偏好、关系、事件和项目、指令），版式不一样：
// 标题有的带编号、有的加粗、有的什么都不加；证据有的缩进在条目下面，有的和条目平级，有的只是单独一行；
// 整份内容还可能连着写了好几遍。这里按这几种版式切成一条一条的候选。
//
// 每一行都要有去处：留下的、和别处重复被去掉的、只是「没有记录」这类占位的，三者加起来等于找到的条目数，
// 由 ImportResult 报出来，导入前能核对有没有悄悄丢掉什么。

// ImportResult 是切分的结果。
type ImportResult struct {
	Candidates []Candidate
	// Found 是找到的条目数，等于 len(Candidates) + Duplicates + Skipped。
	Found int
	// Duplicates 是和前面某条一字不差而被去掉的条数。
	Duplicates int
	// Skipped 是「没有记录」「未记录」这类占位行，没有信息量，不进记忆。
	Skipped int
}

var (
	sourceTrailer = regexp.MustCompile(`导入来源：(?:ChatGPT|Claude|Gemini|豆包)`)
	headingMarks  = regexp.MustCompile(`^#{1,6}\s*`)
	headingNumber = regexp.MustCompile(`^\d+[.、]\s*`)
	bulletMark    = regexp.MustCompile(`^[-*•]\s+`)
)

// importSections 是各家用过的小节标题；没有标记的标题（如 Gemini 的）靠这份名单认出来。
var importSections = map[string]bool{
	"人口统计信息": true, "基本信息": true,
	"兴趣和偏好": true,
	"关系":    true, "关系与协作": true,
	"标注日期的事件、项目和计划": true, "重要事件、项目和计划": true,
	"指令": true, "长期指令和行为偏好": true,
}

// ParseExports 把一份或几份导出文本切成候选，几份之间的重复也一并去掉。
func ParseExports(texts ...string) ImportResult {
	var r ImportResult
	seen := map[string]bool{}
	for _, text := range texts {
		parseOne(text, &r, seen)
	}
	return r
}

type importEntry struct {
	section        string
	text, evidence string
	date           string
	// notes 是缩进在条目下面、不是证据的补充说明（如「注：……」），跟着条目走。
	notes []string
}

func parseOne(text string, r *ImportResult, seen map[string]bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	// 落款可能和下一份的第一行写在同一行（"导入来源：豆包1. 基本信息"），先把它换成换行。
	text = sourceTrailer.ReplaceAllString(text, "\n")

	var section string
	var cur *importEntry
	flush := func() {
		if cur == nil {
			return
		}
		e := cur
		cur = nil
		r.Found++
		if placeholder(e.text) {
			r.Skipped++
			return
		}
		value := e.value()
		if seen[value] {
			r.Duplicates++
			return
		}
		seen[value] = true
		r.Candidates = append(r.Candidates, e.candidate(value))
	}

	for raw := range strings.SplitSeq(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.Trim(line, "-—_ ") == "" {
			continue
		}
		body := strings.TrimSpace(bulletMark.ReplaceAllString(line, ""))
		isBullet := body != line

		// 证据和日期属于上一条，不管它缩进在下面、与条目平级还是单独一行。
		if rest, ok := strings.CutPrefix(body, "证据："); ok && cur != nil {
			cur.evidence = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(body, "日期："); ok && cur != nil && !isBullet {
			cur.date = strings.TrimSpace(rest)
			continue
		}
		if isBullet && cur != nil && (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) {
			cur.notes = append(cur.notes, body)
			continue
		}
		if !isBullet {
			if title, ok := heading(line); ok {
				flush()
				section = title
				continue
			}
		}
		flush()
		cur = &importEntry{section: section, text: body}
	}
	flush()
}

// heading 判断一行是不是小节标题，是的话返回去掉标记与编号的标题。
func heading(line string) (string, bool) {
	t := line
	marked := false
	if h := headingMarks.ReplaceAllString(t, ""); h != t {
		t, marked = h, true
	}
	if strings.HasPrefix(t, "**") && strings.HasSuffix(t, "**") && len(t) > 4 {
		t, marked = strings.TrimSpace(t[2:len(t)-2]), true
	}
	if n := headingNumber.ReplaceAllString(t, ""); n != t {
		t, marked = n, true
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return "", false
	}
	if importSections[t] || (marked && !strings.ContainsAny(t, "。：")) {
		return t, true
	}
	return "", false
}

// placeholder 报告一行是不是「没有记录」这类占位，而不是关于用户的信息。
func placeholder(s string) bool {
	return strings.HasPrefix(s, "（") || strings.HasPrefix(s, "(") ||
		strings.HasPrefix(s, "无存储") || strings.HasPrefix(s, "无明确提及")
}

// value 把条目、证据和日期写成一段话：原文都留着，证据和日期放在括号里。
func (e *importEntry) value() string {
	var extra []string
	if e.evidence != "" {
		extra = append(extra, "证据："+e.evidence)
	}
	if e.date != "" && !strings.Contains(e.date, "未明确") {
		extra = append(extra, "日期："+e.date)
	}
	for _, n := range e.notes {
		extra = append(extra, n)
	}
	if len(extra) == 0 {
		return e.text
	}
	inner := extra[0]
	for _, x := range extra[1:] {
		sep := "；"
		if strings.HasSuffix(inner, "。") || strings.HasSuffix(inner, "；") {
			sep = " "
		}
		inner += sep + x
	}
	return e.text + "（" + inner + "）"
}

func (e *importEntry) candidate(value string) Candidate {
	kind := KindFact
	if strings.Contains(e.section, "指令") {
		kind = KindPreference
	}
	sum := sha256.Sum256([]byte(value))
	return Candidate{
		Kind: kind, Subject: "user", Domain: e.section,
		// 身份键里要有区别于别的条目的东西，否则同一小节下的条目会互相当成同一条的新版本。
		Attribute:   "import-" + hex.EncodeToString(sum[:4]),
		Value:       value,
		Temporality: TemporalDurable,
		Source:      Source{Kind: SourceUser, Ref: "import", Trust: core.TrustUser},
	}
}
