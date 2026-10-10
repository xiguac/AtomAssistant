package skillscan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Finding 是扫描报告里的一条发现。
type Finding struct {
	// ID 是扫描器的规则编号，如 E1。
	ID string
	// Category 是它归属的类别，如 "Data Exfiltration"。登记处条目的发现没有类别。
	Category string
	// Severity 统一成大写：INFO / LOW / MEDIUM / HIGH / CRITICAL。
	Severity string
	// Text 是命中的内容或扫描器的说明。
	Text string
	// Where 是它出现的位置，如 "SKILL.md 第 9 行"；没有则为空。
	Where string
}

// Outbound 报告这条发现是否属于"把数据传到外面去"。
func (f Finding) Outbound() bool {
	return strings.Contains(strings.ToLower(f.Category), "exfiltration")
}

// Severe 报告这条发现是否达到高危。
func (f Finding) Severe() bool {
	return f.Severity == "HIGH" || f.Severity == "CRITICAL"
}

// Inspect 扫描 path 处的 Skill 并要求扫描器给出结构化的报告，供 Findings 解析。
// 结论的含义与 Check 相同。
func (s Scanner) Inspect(ctx context.Context, path string) (Report, error) {
	return s.run(ctx, path, "--format", "json")
}

// report 是扫描器 JSON 报告里我们用到的字段。Skill 的发现在 issues 里，登记处条目的在 findings 里。
type report struct {
	Issues []struct {
		ID       string `json:"id"`
		Category string `json:"category"`
		Severity string `json:"severity"`
		Finding  string `json:"finding"`
		Location struct {
			File      string `json:"file"`
			StartLine int    `json:"start_line"`
		} `json:"location"`
	} `json:"issues"`
	Findings []struct {
		ID       string `json:"id"`
		Message  string `json:"message"`
		Severity string `json:"severity"`
	} `json:"findings"`
}

// Findings 解析 Inspect 或 CheckRegistry 得到的报告。
//
// 解析不出来时返回错误，调用方应当按"没扫成"处理：
// 看不懂报告就无从判断风险，不能当作没有风险。
func (r Report) Findings() ([]Finding, error) {
	// 扫描器的标准输出和错误输出是合在一起收的，报告前面可能有几行提示。
	start := strings.Index(r.Output, "{")
	if start < 0 {
		return nil, fmt.Errorf("%w: 扫描器没有给出结构化的报告", ErrScanFailed)
	}
	var rep report
	if err := json.NewDecoder(strings.NewReader(r.Output[start:])).Decode(&rep); err != nil {
		return nil, fmt.Errorf("%w: 扫描报告无法解析: %v", ErrScanFailed, err)
	}
	out := make([]Finding, 0, len(rep.Issues)+len(rep.Findings))
	for _, i := range rep.Issues {
		f := Finding{ID: i.ID, Category: i.Category, Severity: strings.ToUpper(i.Severity), Text: strings.TrimSpace(i.Finding)}
		if i.Location.File != "" {
			f.Where = i.Location.File
			if i.Location.StartLine > 0 {
				f.Where = fmt.Sprintf("%s 第 %d 行", i.Location.File, i.Location.StartLine)
			}
		}
		out = append(out, f)
	}
	for _, i := range rep.Findings {
		out = append(out, Finding{ID: i.ID, Severity: strings.ToUpper(i.Severity), Text: strings.TrimSpace(i.Message)})
	}
	return out, nil
}
