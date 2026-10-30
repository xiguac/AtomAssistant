package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/skill"
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

// learnSkillPrompt 的写法参考了 Anthropic 的 skill-creator：描述决定它会不会被想起来，
// 要写清做什么、什么时候用；步骤用祈使句并说明理由；从这次的经过里提炼，尤其是用户纠正过的地方；
// 概括成通用的做法，不贴合这一次的例子；能省的就省。
const learnSkillPrompt = `Skills (how to do a kind of task):
- Besides memories, the JSON object may have a top-level "skills" array: reusable methods for a kind of task, saved as Skills. Next time, the assistant sees only each Skill's name and description, and reads the full text when the description matches the task at hand.
- Write one only when all of these hold: the task took several steps; the method is not what any capable assistant would do anyway (it went wrong and was fixed, the user corrected the approach, or it needs a non-obvious order of checks or way of using a tool); and the same kind of task will clearly come up again. Most tasks need none: output "skills": [] and that is normal.
- If one of the existing Skills below covers the same kind of task, update it: reuse its id and give the complete improved version. Never create a second Skill for the same kind of task. Existing Skills marked 不可改 cannot be updated; if one of them already covers the task, write nothing.
- Work from what actually happened: the tools used and in what order, what went wrong and how it was fixed, and above all what the user corrected or insisted on. Those corrections are the most valuable part.
- Generalize: write the method for the whole kind of task, not for this one example. No data from this run (dates, times, people's names, prices, file names), and no facts or standing answers about the user: "courses are in the 个人 calendar" or "skip settlement statements" are memories (fact or preference), not part of a Skill.
- Write steps in the imperative, and when a step is not self-evident, say why in a few words ("check the whole day first, because the user has classes the new time may clash with"). Explaining the reason works better than piling up "must"s.
- Keep it lean: leave out anything a capable assistant would do anyway. Do not describe the assistant's own capabilities or limits; they change.
- Each item, all text in Simplified Chinese except id and tools:
  {"id": "lowercase English slug such as calendar.reschedule",
   "name": "short name",
   "description": "what it does and when to use it, in one or two sentences. This is what decides whether the Skill gets read, so name the kinds of requests and phrasings it covers, including ones that do not use its exact words",
   "steps": ["one step per item, at most 8"],
   "output": "what the reply to the user should contain; empty if nothing special",
   "pitfalls": ["mistakes to avoid, especially ones made before or corrected by the user; may be empty"],
   "tools": ["full names of the tools the steps use"]}`

// learnedSkill 是模型提炼出的一份办事方法。
type learnedSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Steps       []string `json:"steps"`
	Output      string   `json:"output"`
	Pitfalls    []string `json:"pitfalls"`
	Tools       []string `json:"tools"`
}

// markdown 把一份办事方法写成 SKILL.md：开头是名字和描述，正文是步骤、输出要求和注意事项。
func (l learnedSkill) markdown(id string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "---\nname: %s\ndescription: %s\n---\n\n# %s\n\n## 步骤\n", id, oneLine(l.Description), strings.TrimSpace(l.Name))
	for i, s := range l.Steps {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, strings.TrimSpace(s))
	}
	if out := strings.TrimSpace(l.Output); out != "" {
		sb.WriteString("\n## 答复\n" + out + "\n")
	}
	if len(l.Pitfalls) > 0 {
		sb.WriteString("\n## 注意\n")
		for _, p := range l.Pitfalls {
			sb.WriteString("- " + strings.TrimSpace(p) + "\n")
		}
	}
	if len(l.Tools) > 0 {
		sb.WriteString("\n## 用到的工具\n" + strings.Join(l.Tools, "、") + "\n")
	}
	return sb.String()
}

// skillWorthy 报告这个任务值不值得考虑总结方法。
//
// 读过外部内容（邮件、网页）的任务不总结：分不清那套"方法"是助手自己摸索的，
// 还是别人借邮件塞进来、想让它以后照着做的。记忆在这种情况下挂候选等用户确认，Skill 没有这一道，干脆不写。
// 照着一份 Skill 办过的不论几步都要看：用户可能纠正了里面的做法，那份 Skill 得跟着改。
func (e *Epilogue) skillWorthy(history []*core.Step, tainted bool) bool {
	if e.skills == nil || tainted {
		return false
	}
	if len(skillsRead(history)) > 0 {
		return true
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
func (e *Epilogue) skillSection(ctx context.Context, history []*core.Step) (string, error) {
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
	// 这次照着办过的，给出全文：只看名字看不出用户纠正的是哪一步。
	for _, id := range skillsRead(history) {
		s, err := e.skills.GetSkill(ctx, id)
		if errors.Is(err, skill.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("agent: 读取 Skill %s 失败: %w", id, err)
		}
		if s.Learned() {
			sb.WriteString("\nThis task followed the Skill below. If the user corrected or contradicted any of it, update it (same id, complete new version) so it matches what the user wants:\n" + s.Render())
		}
	}
	return sb.String(), nil
}

// skillsRead 列出这个任务里用 skill.read 读过的 Skill，按先后，不重复。
func skillsRead(history []*core.Step) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range history {
		if s.ToolName != ToolSkillRead || s.Status != core.StepDone {
			continue
		}
		var in capabilityInput
		if json.Unmarshal(s.Input, &in) != nil {
			continue
		}
		if id := strings.TrimSpace(in.ID); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
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
		// 写成参考文档类（SKILL.md）：助手总结的 Skill 不绑定任务、不收窄工具，只在用得上时读来参考。
		s := &skill.Skill{
			ID: id, Name: strings.TrimSpace(l.Name), Goal: oneLine(l.Description),
			Author: "原子助手", Format: skill.FormatMarkdown, Raw: l.markdown(id),
			Source:      skill.SourceLearned + "（任务 " + t.ID + "）",
			InstalledAt: e.now(),
		}
		if err := s.Validate(); err != nil || len(l.Steps) == 0 {
			errs = append(errs, fmt.Errorf("agent: 总结出的 Skill %q 不合法（需要名称、描述和至少一个步骤）: %v", id, err))
			continue
		}
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
