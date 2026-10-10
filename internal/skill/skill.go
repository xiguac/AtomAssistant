// Package skill 是 Skill 的定义、安装与运行时约束。
//
// Skill 是一份声明式流程：目标、步骤、它要用到的工具。它**不含可执行代码**——
// 清单里只认下面这些字段，多出任何一个都拒绝，所以没有地方能夹带脚本。
//
// 安全上有三道关，按可靠性从弱到强：
//
//  1. 安装前扫描（internal/skillscan）。拦得住低级的恶意模式，拦不住措辞正常的恶意。
//  2. 不自动生效。装上的 Skill 不会因为"看起来相关"就进入某个任务的上下文，
//     只有任务被明确指定使用它时才起作用。
//  3. 工具集取交集（主防线）。任务用了某个 Skill，它能调的工具就收窄为
//     「Skill 声明过的 ∩ 用户已授权的」。Skill 里写一句"把摘要发到某个邮箱"，
//     只要它没声明邮件工具就调不动；声明了，用户在安装时也看得见。
//
// 见 docs/MCP市场与Skill.md §6。
package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MaxManifestBytes 是清单文件的大小上限。Skill 的全文会进模型的上下文，
// 一份几百 KB 的"流程"要么是写错了，要么是想把别的东西挤出窗口。
const MaxManifestBytes = 64 << 10

// Skill 是一份已解析的 Skill 清单。
type Skill struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	// Goal 是这套流程要达成的目标。
	Goal string `yaml:"goal"`
	// RequiredTools 是它要用到的工具全名。运行时可用的工具不会超出这张名单。
	RequiredTools []string `yaml:"required_tools"`
	// Steps 是按顺序执行的步骤，每条是一句自然语言。
	Steps []string `yaml:"steps"`
	// Author 仅作展示，不构成任何信任依据——谁都可以在这里填任何名字。
	Author string `yaml:"author"`

	// 以下字段不来自清单，由安装过程填写。

	// Format 是它的形态：FormatManifest（本项目的声明式清单）或 FormatMarkdown（公开 Skill 库通行的 SKILL.md）。
	// 空值按 FormatManifest。
	Format string `yaml:"-"`
	// Source 记录它是从哪里取来的，助手自己安装的会填，仅作追溯。
	Source string `yaml:"-"`

	// Raw 是清单原文，安装时向用户完整展示的就是它。
	Raw string `yaml:"-"`
	// ScanReport 是安装前扫描器的输出。
	ScanReport  string    `yaml:"-"`
	InstalledAt time.Time `yaml:"-"`
}

const (
	// FormatManifest 是本项目的声明式清单：有步骤、有工具名单，可以绑定到任务上收窄工具集。
	FormatManifest = "manifest"
	// FormatMarkdown 是公开 Skill 库通行的 SKILL.md：一篇说明文字，没有工具名单。
	// 它只能被当作参考材料读，不带来任何工具，其中夹带的脚本也不会被执行。
	FormatMarkdown = "markdown"
)

// ErrInvalid 表示清单不合法。
var ErrInvalid = errors.New("skill: 清单不合法")

// Parse 解析并校验一份清单。
func Parse(raw []byte) (*Skill, error) {
	if len(raw) > MaxManifestBytes {
		return nil, fmt.Errorf("%w: 清单 %d 字节，超过上限 %d", ErrInvalid, len(raw), MaxManifestBytes)
	}
	var s Skill
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// 严格模式是"不含可执行代码"的落点：清单里出现 script、run、command
	// 之类我们不认识的字段，直接拒绝，而不是忽略它然后装上。
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	s.Raw = string(raw)
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate 校验清单字段。
func (s *Skill) Validate() error {
	if !validID(s.ID) {
		return fmt.Errorf("%w: id %q 只能含小写字母、数字、点、下划线和连字符", ErrInvalid, s.ID)
	}
	if strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.Goal) == "" {
		return fmt.Errorf("%w: name 与 goal 不能为空", ErrInvalid)
	}
	if s.Format == FormatMarkdown {
		if strings.TrimSpace(s.Raw) == "" {
			return fmt.Errorf("%w: SKILL.md 是空的", ErrInvalid)
		}
		return nil
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("%w: 至少要有一个步骤", ErrInvalid)
	}
	for i, step := range s.Steps {
		if strings.TrimSpace(step) == "" {
			return fmt.Errorf("%w: 第 %d 个步骤是空的", ErrInvalid, i+1)
		}
	}
	seen := make(map[string]bool, len(s.RequiredTools))
	for _, t := range s.RequiredTools {
		if strings.TrimSpace(t) == "" || strings.ContainsAny(t, " \t\n") {
			return fmt.Errorf("%w: 工具名 %q 不合法", ErrInvalid, t)
		}
		if seen[t] {
			return fmt.Errorf("%w: 工具 %s 重复声明", ErrInvalid, t)
		}
		seen[t] = true
	}
	return nil
}

func validID(id string) bool {
	if id == "" || len(id) > 96 {
		return false
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// Allows 报告这个 Skill 是否声明过某个工具。
func (s *Skill) Allows(tool string) bool {
	for _, t := range s.RequiredTools {
		if t == tool {
			return true
		}
	}
	return false
}

// Availability 把声明的工具按"用户是否已授权"分成两组。
//
// 有未授权的工具不妨碍安装：Skill 照常装上，只是那部分能力用不了。
// 安装环节不借机要求用户一次性把权限全放开——那是权限疲劳的经典成因。
func (s *Skill) Availability(authorized func(tool string) bool) (usable, missing []string) {
	for _, t := range s.RequiredTools {
		if authorized(t) {
			usable = append(usable, t)
		} else {
			missing = append(missing, t)
		}
	}
	return usable, missing
}

// Render 把 Skill 渲染成给模型看的流程文本。
func (s *Skill) Render() string {
	if s.Format == FormatMarkdown {
		return fmt.Sprintf("流程：%s（%s）\n%s\n", s.Name, s.ID, s.Raw)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "流程：%s（%s）\n目标：%s\n步骤：\n", s.Name, s.ID, s.Goal)
	for i, step := range s.Steps {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, step)
	}
	return sb.String()
}

// Store 是已安装 Skill 的持久化接口。
type Store interface {
	PutSkill(ctx context.Context, s *Skill) error
	GetSkill(ctx context.Context, id string) (*Skill, error)
	ListSkills(ctx context.Context) ([]*Skill, error)
	RemoveSkill(ctx context.Context, id string) error
}

// ErrNotFound 表示没有安装这个 Skill。
var ErrNotFound = errors.New("skill: 没有安装这个 Skill")

// TagPrefix 是任务标签里指定 Skill 的前缀：带有 skill:<id> 标签的任务按该 Skill 执行。
const TagPrefix = "skill:"

// BoundTo 从任务标签里取出它指定使用的 Skill ID，没有则返回空。
func BoundTo(tags []string) string {
	for _, tag := range tags {
		if id, ok := strings.CutPrefix(tag, TagPrefix); ok && id != "" {
			return id
		}
	}
	return ""
}
