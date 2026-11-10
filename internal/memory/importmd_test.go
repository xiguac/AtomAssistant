package memory

import (
	"strings"
	"testing"
)

// 四种导出格式各取一小段，内容是编的，只保留版式：标题的写法、条目与证据的层级、占位行、来源落款。
const (
	sampleChatGPT = `## 1. 人口统计信息

- 用户常用昵称是 甲。
  - 证据：账户显示名称为“甲”。日期：[2026-10-31]。

## 4. 指令

- 用户要求回答先给结论。
  - 证据：用户明确要求。日期：[2026-07-13]。

导入来源：ChatGPT`

	sampleClaude = `**1. 人口统计信息**

（未记录常用名字、教育程度、常住地。）

**2. 兴趣和偏好**

* 用户喜欢长跑。
    * 证据：存储的记忆记录为“likes running”。日期：[2026-10-27]。

**5. 指令**

（存储的记忆中没有用户明确要求今后遵循的规则。）

导入来源：Claude`

	sampleGemini = `人口统计信息

* 用户的教育程度是某大学在校学生。
* 证据：提交申请材料。日期：2026-09-01


---

兴趣和偏好

* 用户持续学习高等数学。
* 证据：进行多章节习题推导。日期：2026-05-10


---

指令
无存储的全局偏好指令记忆。

导入来源：Gemini`

	// 豆包的证据与日期是单独的行，没有项目符号；整份内容连着写了两遍，第二遍紧跟在落款后面。
	sampleDoubao = `1. 基本信息
- 用户身份：在校大学生。
证据：对话中提及。
日期：日期未明确

3. 关系与协作
- 无明确提及长期合作关系、协作人员相关信息。

4. 重要事件、项目和计划
- 身体情况：某某症状。
证据：用户自述症状。
日期：2026-10-30

导入来源：豆包1. 基本信息
- 用户身份：在校大学生。
证据：对话中提及。
日期：日期未明确
- 常住地：某市。
证据：用户查询邮编。
日期：2026-10-26

导入来源：豆包`
)

func values(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Value
	}
	return out
}

func TestParseExport_各家格式(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
		skipped    int
	}{
		{"ChatGPT", sampleChatGPT, []string{
			"用户常用昵称是 甲。（证据：账户显示名称为“甲”。日期：[2026-10-31]。）",
			"用户要求回答先给结论。（证据：用户明确要求。日期：[2026-07-13]。）",
		}, 0},
		{"Claude", sampleClaude, []string{
			"用户喜欢长跑。（证据：存储的记忆记录为“likes running”。日期：[2026-10-27]。）",
		}, 2},
		{"Gemini", sampleGemini, []string{
			"用户的教育程度是某大学在校学生。（证据：提交申请材料。日期：2026-09-01）",
			"用户持续学习高等数学。（证据：进行多章节习题推导。日期：2026-05-10）",
		}, 1},
		{"豆包", sampleDoubao, []string{
			"用户身份：在校大学生。（证据：对话中提及。）",
			"身体情况：某某症状。（证据：用户自述症状。 日期：2026-10-30）",
			"常住地：某市。（证据：用户查询邮编。 日期：2026-10-26）",
		}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ParseExports(c.text)
			got := values(r.Candidates)
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Fatalf("条目不对:\n得到 %q\n应为 %q", got, c.want)
			}
			if r.Skipped != c.skipped {
				t.Errorf("占位行应跳过 %d 行，实际 %d", c.skipped, r.Skipped)
			}
		})
	}
}

func TestParseExport_缩进的补充说明跟着上一条(t *testing.T) {
	text := `## 1. 人口统计信息

- 用户常住甲地。
  - 证据：多次提到。日期：[2026-10-02]。
  - 注：近期定位在乙地，不能据此确认搬家。
`
	r := ParseExports(text)
	want := "用户常住甲地。（证据：多次提到。日期：[2026-10-02]。 注：近期定位在乙地，不能据此确认搬家。）"
	if r.Found != 1 || len(r.Candidates) != 1 || r.Candidates[0].Value != want {
		t.Fatalf("补充说明应并进上一条: 找到 %d，得到 %q", r.Found, values(r.Candidates))
	}
}

func TestParseExport_账目对得上(t *testing.T) {
	// 找到的 = 留下的 + 重复的 + 占位的：每一行要么进了记忆，要么有说法，不会悄悄丢掉。
	r := ParseExports(sampleDoubao)
	if r.Found != len(r.Candidates)+r.Duplicates+r.Skipped {
		t.Fatalf("账对不上: 找到 %d，留下 %d，重复 %d，占位 %d", r.Found, len(r.Candidates), r.Duplicates, r.Skipped)
	}
	if r.Duplicates != 1 {
		t.Errorf("第二遍里重复的一条应被去掉，实际重复 %d", r.Duplicates)
	}
}

func TestParseExport_合并多份并去重(t *testing.T) {
	r := ParseExports(sampleChatGPT, sampleChatGPT, sampleClaude)
	if len(r.Candidates) != 3 || r.Duplicates != 2 {
		t.Fatalf("两份相同的只留一份: 留下 %d，重复 %d", len(r.Candidates), r.Duplicates)
	}
}

func TestParseExport_候选可以写入(t *testing.T) {
	r := ParseExports(sampleChatGPT, sampleClaude, sampleGemini, sampleDoubao)
	seen := map[string]bool{}
	for _, c := range r.Candidates {
		if err := c.Validate(); err != nil {
			t.Fatalf("候选不合法: %v (%q)", err, c.Value)
		}
		if c.Subject != "user" || c.Source.Kind != SourceUser {
			t.Errorf("导入的按用户本人写的对待: %+v", c)
		}
		if seen[c.Key()] {
			t.Errorf("身份键重复，写入时后一条会覆盖前一条: %s", c.Key())
		}
		seen[c.Key()] = true
	}
	var pref int
	for _, c := range r.Candidates {
		if c.Kind == KindPreference {
			pref++
		}
	}
	if pref != 1 {
		t.Errorf("「指令」下的条目应记为偏好，其余记为事实；得到 %d 条偏好", pref)
	}
}
