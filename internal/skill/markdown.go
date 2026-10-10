package skill

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MarkdownName 是公开 Skill 库里说明文件的名字。
const MarkdownName = "SKILL.md"

// frontMatter 是 SKILL.md 开头那段元数据里我们用到的字段，其余的忽略。
type frontMatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// ParseMarkdown 把一份 SKILL.md 解析成 Skill。id 由调用方给出（它来自哪里就叫什么）。
//
// 开头的元数据只取名字和说明；写坏了或者没有，就用 id 当名字——
// 这段元数据是别人写的，不值得为它拒绝一份正文可用的说明。
func ParseMarkdown(raw []byte, id string) (*Skill, error) {
	if len(raw) > MaxManifestBytes {
		return nil, fmt.Errorf("%w: SKILL.md %d 字节，超过上限 %d", ErrInvalid, len(raw), MaxManifestBytes)
	}
	s := &Skill{ID: id, Format: FormatMarkdown, Raw: string(raw)}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		if head, _, ok := strings.Cut(rest, "\n---"); ok {
			var fm frontMatter
			if err := yaml.Unmarshal([]byte(head), &fm); err == nil {
				s.Name = strings.TrimSpace(fm.Name)
				s.Goal = strings.TrimSpace(fm.Description)
			}
		}
	}
	if s.Name == "" {
		s.Name = id
	}
	if s.Goal == "" {
		s.Goal = s.Name
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}
