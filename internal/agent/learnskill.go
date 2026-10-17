package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/skill"
)

// 办一类事的方法写成 Skill，不写进记忆。
//
// 记忆只记关于用户的事：住哪、习惯、相关的人，每个相关任务都整段带着。方法不一样：
// 只有再遇到同一类事才用得上，而且往往很长。写成 Skill 之后，平时上下文里只列一行名字和用途，
// 用到时才读全文；用户也能在 Skill 页上看、改、删。

// SkillWriter 是收尾写 Skill 所需的能力，由 skill.Store 提供。
type SkillWriter interface {
	ListSkills(ctx context.Context) ([]*skill.Skill, error)
	GetSkill(ctx context.Context, id string) (*skill.Skill, error)
	PutSkill(ctx context.Context, s *skill.Skill) error
}

// WithSkills 让收尾时把值得复用的办事方法写成 Skill。不接的话收尾只提炼记忆。
func (e *Epilogue) WithSkills(w SkillWriter) *Epilogue {
	e.skills = w
	return e
}

// minSkillSteps 是一个任务至少调了几次工具才考虑总结方法。一两步就办完的事，模型本来就会办，
// 总结出来只是一段谁都知道的话，还要占一行目录。
const minSkillSteps = 3

const learnSkillPrompt = `Skills (how to do a kind of task):
- Besides memories, the JSON object may have a top-level "skills" array: reusable methods for a kind of task, saved as Skills that the assistant reads only when that kind of task comes up again.
- Write one only when all of these hold: the task took several steps; the method is not what any capable assistant would do anyway (it went wrong and was fixed, the user corrected the approach, or it needs a non-obvious order of checks or way of using a tool); and the same kind of task will clearly come up again. Most tasks need none: output "skills": [] and that is normal.
- If one of the existing Skills below covers the same kind of task, update it: reuse its id and give the complete improved version. Never create a second Skill for the same kind of task. Existing Skills marked 不可改 cannot be updated; if one of them already covers the task, write nothing.
- A Skill holds only the method. No data from this run (dates, times, people's names, prices, file names), and no facts or standing answers about the user: "courses are in the 个人 calendar" or "skip settlement statements" are memories (fact or preference), not part of a Skill.
- Do not describe the assistant's own capabilities or limits; they change.
- Each item: {"id": "lowercase English slug such as calendar.reschedule", "name": "short name in Simplified Chinese", "goal": "one sentence in Simplified Chinese saying which tasks it applies to", "steps": ["Simplified Chinese, one step per item, at most 8"], "tools": ["full names of the tools the steps use"]}`

// learnedSkill 是模型提炼出的一份办事方法。
type learnedSkill struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Goal  string   `json:"goal"`
	Steps []string `json:"steps"`
	Tools []string `json:"tools"`
}

// skillWorthy 报告这个任务值不值得考虑总结方法。
//
// 读过外部内容（邮件、网页）的任务不总结：分不清那套"方法"是助手自己摸索的，
// 还是别人借邮件塞进来、想让它以后照着做的。记忆在这种情况下挂候选等用户确认，Skill 没有这一道，干脆不写。
func (e *Epilogue) skillWorthy(history []*core.Step, tainted bool) bool {
	if e.skills == nil || tainted {
		return false
	}
	n := 0
	for _, s := range history {
		if s.Type == core.StepTool && s.Status == core.StepDone && !isInternalTool(s.ToolName) &&
			s.ToolName != ToolPlan && s.ToolName != ToolLoad {
			n++
		}
	}
	return n >= minSkillSteps
}

// skillSection 是提炼时附在系统提示后面的一节：怎么写 Skill，以及已经有哪些。
func (e *Epilogue) skillSection(ctx context.Context) (string, error) {
	items, err := e.skills.ListSkills(ctx)
	if err != nil {
		return "", fmt.Errorf("agent: 读取已有的 Skill 失败: %w", err)
	}
	var sb strings.Builder
	sb.WriteString(learnSkillPrompt + "\n\nExisting Skills:\n")
	if len(items) == 0 {
		sb.WriteString("(none)\n")
	}
	for i, s := range items {
		if i == maxListedSkills {
			break
		}
		mark := ""
		if !s.Learned() {
			mark = "（不可改）"
		}
		fmt.Fprintf(&sb, "- %s%s: %s —— %s\n", s.ID, mark, truncate(s.Name, 60), truncate(oneLine(s.Goal), 200))
	}
	return sb.String(), nil
}

// saveSkills 把提炼出的方法写成 Skill。同 ID 已有的：助手自己总结的整份换成新的，别的不动。
func (e *Epilogue) saveSkills(ctx context.Context, t *core.Task, learned []learnedSkill) error {
	var errs []error
	for _, l := range learned {
		id := strings.ToLower(strings.TrimSpace(l.ID))
		existing, err := e.skills.GetSkill(ctx, id)
		if err != nil && !errors.Is(err, skill.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		if existing != nil && !existing.Learned() {
			errs = append(errs, fmt.Errorf("agent: Skill %s 不是助手总结的，不能改写", id))
			continue
		}
		s, err := skill.Compose(&skill.Skill{
			ID: id, Name: strings.TrimSpace(l.Name), Goal: strings.TrimSpace(l.Goal),
			Steps: l.Steps, RequiredTools: l.Tools, Author: "原子助手",
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("agent: 总结出的 Skill %q 不合法: %w", id, err))
			continue
		}
		s.Source = skill.SourceLearned + "（任务 " + t.ID + "）"
		s.InstalledAt = e.now()
		if err := e.skills.PutSkill(ctx, s); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// MovePlaybooks 把记忆里的"流程"挪成 Skill，原记忆归档（不删，还查得到）。启动时调用，可重复调用。
//
// 出厂自带的流程已经由 skill.SeedBuiltin 写成 Skill，这里只归档。
// 其余的是以往任务里总结的，原样写成一份参考文档类的 Skill。
func MovePlaybooks(ctx context.Context, mem memory.Store, skills SkillWriter, now time.Time) (int, error) {
	list, err := mem.List(ctx, memory.Query{Kinds: []memory.Kind{memory.KindPlaybook}})
	if err != nil {
		return 0, fmt.Errorf("agent: 读取流程类记忆失败: %w", err)
	}
	moved := 0
	for _, m := range list {
		if m.Current.Source.Kind != memory.SourceBuiltin {
			// 先写 Skill 再归档：中途失败的话，下次启动还会再挪一次，不会两头都没有。
			s := &skill.Skill{
				ID: "memo." + strings.ToLower(m.ID), Name: m.Domain,
				Goal:   "以往任务里总结的「" + m.Domain + "」做法",
				Author: "原子助手", Format: skill.FormatMarkdown, Raw: m.Current.Value,
				Source:      skill.SourceLearned + "（由记忆迁移，原记忆 " + m.ID + "）",
				InstalledAt: now,
			}
			if err := s.Validate(); err != nil {
				return moved, fmt.Errorf("agent: 记忆 %s 转成 Skill 不合法: %w", m.ID, err)
			}
			if err := skills.PutSkill(ctx, s); err != nil {
				return moved, err
			}
		}
		m.Status, m.UpdatedAt = memory.StatusArchived, now
		if err := mem.Put(ctx, m); err != nil {
			return moved, fmt.Errorf("agent: 归档记忆 %s 失败: %w", m.ID, err)
		}
		moved++
	}
	return moved, nil
}
