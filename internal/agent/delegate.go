package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/team"
)

// 主模型与任务模型的分工：配置了协作模型（任务模型）之后，一件事这样办。
//
//  1. 指派（主模型）：判断这件事属于哪一类，据此选出任务模型。
//  2. 方案（任务模型 → 主模型）：任务模型先写一份简略的方案——拆成哪些小步骤、用哪些工具、
//     要考虑哪些东西、做到什么样算完。主模型评估，可以直接同意，也可以改。
//  3. 执行（任务模型）：按定下来的方案去办，调工具、做分析，直到给出结果。
//  4. 审核（主模型）：研判结果有没有达到要求。可以，就由主模型写出答复反馈给用户；
//     要改，就把意见交回任务模型，它改完再交。
//  5. 换人（主模型）：退回了几轮仍不满意，主模型可以把这件事改派给另一个任务模型，
//     从方案开始重来。
//
// 任务模型选谁不由主模型点名：主模型只判断类别，人选由评分决定——用户对回答的评分记在
// 办这件事的那个模型名下，同类的事优先交给评分高的，见 internal/team。
// 只有答复最终到了用户手里的那个模型才记一份可以评分的活；中途被换下去的不记。
//
// 多了一个模型，"能做什么"的边界没有变：任务模型选的每一个工具照旧要在目录里，
// 副作用等级照旧取自授权而不是它的自报，照旧要过出口管控和审批。
// 变的是"谁看得到"：除了用户的表达风格，任务模型看到的上下文和主模型一样，包括记忆。
// 它所在的那家模型服务因此也看得到这些内容，这是用户配置协作模型时作的选择。
//
// 与用户有关的事都归主模型：给用户的答复由它写，用户的表达风格只给它看，
// 记忆、流程和表达风格的学习也都由它做（见 Epilogue 与 internal/style）。
//
// 语言：主模型用英文思考，写给用户看的用简体中文；任务模型的方案、理由、结论一律用英文，
// 只有直接问用户的问题和替用户写的东西例外。

// 规划器自己写进日志的三种步骤。它们不是给模型选的工具。
const (
	// ToolAssign 记下这件事交给了哪个任务模型。改派时再记一条。
	ToolAssign = "task.assign"
	// ToolBrief 记下定下来的方案：任务模型起草、主模型评估过的。
	ToolBrief = "task.brief"
	// ToolReview 记下主模型的一次退回意见，或者它决定自己接手。
	ToolReview = "task.review"
	// ToolVeto 记下主模型否决的一个动作：要修改、删除已有东西的动作先由它把关，见 guard。
	ToolVeto = "task.veto"
)

const (
	// maxReviewRounds 是主模型最多把同一个任务模型的结果退回去几次。
	maxReviewRounds = 2
	// maxReassign 是一件事最多改派几次。到头了由主模型自己收尾，把没解决的问题如实告诉用户——
	// 几个模型来回推不能没有尽头。
	maxReassign = 1
)

// TaskAssigner 按类别和评分选出任务模型，并记下谁办了哪件事。由 *team.Board 提供。
type TaskAssigner interface {
	PickExcept(ctx context.Context, c team.Category, exclude []string) (team.Worker, error)
	Record(ctx context.Context, w *team.Work) error
}

// assignRecord 是一次指派，存在 task.assign 这一步里。
type assignRecord struct {
	Worker   string `json:"worker"`
	Category string `json:"category"`
	Title    string `json:"title"`
	// Round 是第几次指派，从 1 起。Why 是改派的原因，第一次指派时为空。
	Round int    `json:"round"`
	Why   string `json:"why,omitempty"`
}

// briefRecord 是定下来的方案，存在 task.brief 这一步里。
type briefRecord struct {
	Steps          []string `json:"steps"`
	Tools          []string `json:"tools"`
	Considerations []string `json:"considerations"`
	DoneWhen       string   `json:"done_when"`
	// Amended 为真表示主模型改过任务模型起草的方案，Note 是它改了什么、为什么。
	Amended bool   `json:"amended,omitempty"`
	Note    string `json:"note,omitempty"`
}

// reviewRecord 是一次被退回的审核，存在 task.review 这一步里。
type reviewRecord struct {
	Round int `json:"round"`
	// Proposed 是任务模型交回的结果，Feedback 是主模型要它改什么。
	Proposed string `json:"proposed"`
	Feedback string `json:"feedback"`
	// Takeover 为真表示主模型没有退回，而是决定自己接手：之后的每一步由它来做。
	Takeover bool `json:"takeover,omitempty"`
}

// vetoRecord 是被主模型否决的一个动作，存在 task.veto 这一步里。
type vetoRecord struct {
	Tool   string          `json:"tool"`
	Params json.RawMessage `json:"params"`
	Reason string          `json:"reason"`
}

// WithTaskModels 接入任务模型。接入后每件事都按"指派—方案—执行—审核"办。
func (p *Planner) WithTaskModels(a TaskAssigner, m WorkerModel) *Planner {
	p.team, p.workers = a, m
	return p
}

// delegating 报告这个规划器是否把事情交给任务模型去办。
func (p *Planner) delegating() bool { return p.team != nil && p.workers != nil }

// isInternalTool 报告一个工具名是否是规划器自己写日志用的。
func isInternalTool(name string) bool {
	return name == ToolAssign || name == ToolBrief || name == ToolReview || name == ToolVeto
}

// delegation 是从步骤日志里读出来的分工进展。
type delegation struct {
	// assigns 是历次指派，最后一个是当前的；assignSeq 是当前这次指派所在的步骤号。
	assigns   []assignRecord
	assignSeq int
	// brief 是当前这次指派定下来的方案，还没有则为 nil。
	brief *briefRecord
	// reviews 是当前这次指派里已经退回过的各次审核。
	reviews []reviewRecord
	// takeover 非空表示主模型已经接手，里面是任务模型最后交回的结果和主模型认为的问题。
	takeover *reviewRecord
}

// current 返回当前的指派，还没有指派过则返回 nil。
func (d *delegation) current() *assignRecord {
	if len(d.assigns) == 0 {
		return nil
	}
	return &d.assigns[len(d.assigns)-1]
}

// tried 返回已经办过这件事的任务模型。
func (d *delegation) tried() []string {
	out := make([]string, 0, len(d.assigns))
	for _, a := range d.assigns {
		out = append(out, a.Worker)
	}
	return out
}

// readDelegation 从步骤日志里读出分工进展。一次新的指派会让之前的方案和退回记录作废。
func readDelegation(history []*core.Step) delegation {
	var d delegation
	for _, s := range history {
		if s.Status != core.StepDone {
			continue
		}
		switch s.ToolName {
		case ToolAssign:
			var a assignRecord
			if json.Unmarshal(s.Output, &a) == nil {
				d.assigns, d.assignSeq = append(d.assigns, a), s.Seq
				d.brief, d.reviews, d.takeover = nil, nil, nil
			}
		case ToolBrief:
			var b briefRecord
			if json.Unmarshal(s.Output, &b) == nil {
				d.brief = &b
			}
		case ToolReview:
			var r reviewRecord
			if json.Unmarshal(s.Output, &r) != nil {
				continue
			}
			if r.Takeover {
				d.takeover = &r
			} else {
				d.reviews = append(d.reviews, r)
			}
		}
	}
	return d
}

// render 把方案写成给模型看的文字。
func (b *briefRecord) render() string {
	var sb strings.Builder
	list := func(title string, items []string, numbered bool) {
		if len(items) == 0 {
			return
		}
		sb.WriteString(title + "\n")
		for i, it := range items {
			if numbered {
				fmt.Fprintf(&sb, "%d. %s\n", i+1, it)
			} else {
				fmt.Fprintf(&sb, "- %s\n", it)
			}
		}
	}
	list("步骤：", b.Steps, true)
	list("用到的工具：", b.Tools, false)
	list("需要考虑的：", b.Considerations, false)
	if b.DoneWhen != "" {
		sb.WriteString("完成标准：" + b.DoneWhen + "\n")
	}
	if b.Note != "" {
		sb.WriteString("主模型的修改说明：" + b.Note + "\n")
	}
	return sb.String()
}

// delegateSegments 把分工的进展钉在上下文里：定下来的方案、改派的原因、最近一次退回意见。
//
// 它们和计划清单一样不能随着步骤增多滑出窗口：任务模型做到第三十步时，仍然要知道方案是什么。
func delegateSegments(history []*core.Step) []llm.Segment {
	d := readDelegation(history)
	cur := d.current()
	if cur == nil {
		return nil
	}
	var segs []llm.Segment
	// 这些都是模型写的，信任等级与模型输出一致。
	pin := func(source, content string) {
		segs = append(segs, llm.Segment{Role: llm.RoleUser, Content: content,
			Trust: core.TrustToolOutput, Source: source, Pinned: true})
	}
	if cur.Why != "" {
		pin("改派说明", "这件事之前交给别的任务模型办过，结果没有达到要求，现在改由你来办。之前的步骤和查到的东西都在历史里，可以接着用。\n"+
			"没有达到要求的地方：\n"+cur.Why)
	}
	if d.brief != nil {
		pin("定下来的方案", "这件事的方案（任务模型起草，主模型评估过）。按它办，实际情况不一样时可以调整：\n"+d.brief.render())
	}
	if d.takeover != nil {
		pin("接手说明", "任务模型交回的结果没有达到要求，这件事现在由你（主模型）接手。之前的步骤和查到的东西都在历史里。\n"+
			"任务模型交回的结果：\n"+truncate(d.takeover.Proposed, 4000)+"\n\n你认为的问题：\n"+d.takeover.Feedback)
		return segs
	}
	if len(d.reviews) > 0 {
		last := d.reviews[len(d.reviews)-1]
		pin("主模型的审核意见", fmt.Sprintf("主模型审核了上一次交回的结果，认为还没有达到要求（第 %d 次退回）。\n上一次交回的结果：\n%s\n\n修改意见：\n%s",
			last.Round, truncate(last.Proposed, 4000), last.Feedback))
	}
	return segs
}

// decideDelegated 是接入任务模型之后的决策，按分工进展到了哪一步来定这一次该谁做什么。
func (p *Planner) decideDelegated(ctx context.Context, t *core.Task, history []*core.Step) (kernel.Decision, error) {
	state := readDelegation(history)
	cur := state.current()
	if cur == nil {
		return p.assign(ctx, t, history)
	}
	cp := *p
	cp.worker = cur.Worker
	if state.brief == nil {
		return cp.draft(ctx, t, history)
	}
	if state.takeover != nil {
		// 主模型接手之后自己一步步办，可以用全部工具；它说办完了就是办完了，不用再审自己。
		// 接手之后该怎么办写在规划说明里，认的是上下文里的「接手说明」。
		d, err := p.decide(ctx, t, history)
		if err != nil || d.Kind != kernel.DecideFinish {
			return d, err
		}
		return d, p.recordWork(ctx, t, state)
	}
	d, err := cp.decide(ctx, t, history)
	if err != nil || d.Kind != kernel.DecideFinish {
		// 提问、等待、调工具照常进行；办不下去（fail）也如实交给内核。
		return d, err
	}
	return p.review(ctx, t, history, state, d)
}

// complete 让当前负责决策的模型执行一次补全：任务模型，或者主模型。
func (p *Planner) complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if p.worker != "" && p.workers != nil {
		resp, err := p.workers.CompleteWith(ctx, p.worker, req)
		if !errors.Is(err, llm.ErrNoProvider) {
			return resp, err
		}
		// 任务办到一半，配置里已经没有这个任务模型了：主模型自己接着办，不让任务卡死。
	}
	return p.model.Complete(ctx, llm.PurposePlan, req)
}

// spend 把一次模型调用的用量加到决策上。
func spend(d *kernel.Decision, resp llm.Response) {
	d.TokensIn += resp.Usage.PromptTokens
	d.TokensOut += resp.Usage.CompletionTokens
	d.CostMicros += resp.CostMicros
}

// askJSON 让一个模型按给定的系统提示看一遍上下文，把它的回答解析进 out。
// main 为真问主模型，否则问 p.worker 指定的任务模型。用量记在 spent 上，不管回答能不能用。
func (p *Planner) askJSON(ctx context.Context, t *core.Task, main bool, system string, segs []llm.Segment, spent *kernel.Decision, what string, out any) error {
	msgs, err := llm.Build(segs, llm.BuildOptions{SystemPrompt: system, MaxChars: p.opts.MaxChars + len(system)})
	if err != nil {
		return err
	}
	if msgs, err = p.withImages(ctx, t, msgs); err != nil {
		return err
	}
	req := llm.Request{Messages: msgs, JSONMode: true}
	var resp llm.Response
	if main {
		resp, err = p.model.Complete(ctx, llm.PurposePlan, req)
	} else {
		resp, err = p.complete(ctx, req)
	}
	if err != nil {
		return err
	}
	spend(spent, resp)
	if err := json.NewDecoder(strings.NewReader(stripFence(resp.Content))).Decode(out); err != nil {
		return fmt.Errorf("%w: %s不是合法 JSON: %v (原文: %s)",
			errBadOutput, what, err, truncate(strings.TrimSpace(resp.Content), 200))
	}
	return nil
}

// internalStep 把一条规划器自己写进日志的记录做成决策。
func internalStep(spent kernel.Decision, tool string, record any, reason string) (kernel.Decision, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return spent, fmt.Errorf("agent: 序列化 %s 失败: %w", tool, err)
	}
	spent.Kind, spent.Type, spent.ToolName = kernel.DecideStep, core.StepTool, tool
	// 只写任务自己的日志，没有任何外部副作用。
	spent.SideEffect = core.SideEffectNone
	spent.Input, spent.Reason = raw, reason
	return spent, nil
}

func (p *Planner) clock() string {
	// 没有当前时间，"下周三""国庆前后"这类说法模型无从换算。
	return "\n\nCurrent time: " + p.now().Local().Format("2006-01-02 15:04 Monday")
}

// knownTools 只留下真的有的工具名：一个不存在的名字只会让任务模型白试一次。
func (p *Planner) knownTools(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, err := p.lookup(name); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// assign 让主模型判断这件事的类别，再按类别和评分选出任务模型。
func (p *Planner) assign(ctx context.Context, t *core.Task, history []*core.Step) (kernel.Decision, error) {
	ps, err := p.loadPrompts(ctx)
	if err != nil {
		return kernel.Decision{}, err
	}
	var cats []string
	for _, c := range team.Categories {
		cats = append(cats, fmt.Sprintf("- %s (%s): %s", c.ID, c.Name, c.Hint))
	}
	system := sections(ps.get(PromptBase), ps.get(PromptAssign), "Categories:\n"+strings.Join(cats, "\n")) + p.clock()
	segs, err := p.contextSegments(ctx, t, history)
	if err != nil {
		return kernel.Decision{}, err
	}
	var spent kernel.Decision
	var out struct {
		Category string `json:"category"`
		Title    string `json:"title"`
	}
	if err := p.askJSON(ctx, t, true, system, segs, &spent, "指派结果", &out); err != nil {
		return spent, err
	}
	// 类别写错了不值得为它重来一次：归到"其他"，照样有人接。
	if !team.ValidCategory(team.Category(out.Category)) {
		out.Category = string(team.CatOther)
	}
	if strings.TrimSpace(out.Title) == "" {
		out.Title = t.Title
	}
	w, err := p.team.PickExcept(ctx, team.Category(out.Category), nil)
	if err != nil {
		return spent, fmt.Errorf("agent: 选任务模型失败: %w", err)
	}
	return internalStep(spent, ToolAssign,
		assignRecord{Worker: w.Name, Category: out.Category, Title: out.Title, Round: 1},
		fmt.Sprintf("按「%s」交给 %s 办理", team.CategoryName(team.Category(out.Category)), w.Name))
}

// draft 让任务模型写一份简略的方案，再由主模型评估：同意，或者改。定下来的方案记进日志。
func (p *Planner) draft(ctx context.Context, t *core.Task, history []*core.Step) (kernel.Decision, error) {
	ps, err := p.loadPrompts(ctx)
	if err != nil {
		return kernel.Decision{}, err
	}
	tools := p.clock() + "\n\nAvailable tools:\n" + p.toolList()
	segs, err := p.contextSegments(ctx, t, history)
	if err != nil {
		return kernel.Decision{}, err
	}
	var spent kernel.Decision
	var plan briefRecord
	if err := p.askJSON(ctx, t, false, sections(ps.get(PromptBase), ps.get(PromptDraft))+tools,
		segs, &spent, "方案", &plan); err != nil {
		return spent, err
	}
	if len(plan.Steps) == 0 {
		return spent, fmt.Errorf("%w: 方案里没有步骤", errBadOutput)
	}

	var verdict struct {
		Verdict string `json:"verdict"`
		briefRecord
	}
	review := append(append([]llm.Segment{}, segs...), llm.Segment{
		Role: llm.RoleUser, Content: "任务模型起草的方案：\n" + plan.render(),
		Trust: core.TrustToolOutput, Source: "任务模型的方案", Pinned: true,
	})
	if err := p.askJSON(ctx, t, true, sections(ps.get(PromptBase), ps.get(PromptPlanReview))+tools,
		review, &spent, "方案评估结果", &verdict); err != nil {
		return spent, err
	}
	final, reason := plan, "方案已定：主模型同意任务模型的方案"
	switch verdict.Verdict {
	case "approve":
	case "amend":
		if len(verdict.Steps) == 0 {
			return spent, fmt.Errorf("%w: 主模型要改方案，却没有给出修改后的步骤", errBadOutput)
		}
		final, reason = verdict.briefRecord, "方案已定：主模型修改了任务模型的方案"
		final.Amended = true
	default:
		return spent, fmt.Errorf("%w: 方案评估结果的 verdict 不合法: %q", errBadOutput, verdict.Verdict)
	}
	final.Tools = p.knownTools(final.Tools)
	return internalStep(spent, ToolBrief, final, reason)
}

// review 让主模型研判任务模型交回的结果：可以就反馈给用户，要改就退回，退了几轮仍不行可以换人。
func (p *Planner) review(ctx context.Context, t *core.Task, history []*core.Step, state delegation, d kernel.Decision) (kernel.Decision, error) {
	// 任务模型那一次决策同样花了钱，不管审核成没成都要记账。
	spent := kernel.Decision{TokensIn: d.TokensIn, TokensOut: d.TokensOut, CostMicros: d.CostMicros}
	ps, err := p.loadPrompts(ctx)
	if err != nil {
		return spent, err
	}
	cur := state.current()
	round := len(state.reviews)
	canRevise := round < maxReviewRounds
	// 退回的次数用完了才谈得上换人。
	canReassign := !canRevise && len(state.assigns) <= maxReassign
	verdicts, extra := `"accept" | "takeover"`, "- This result can no longer be sent back or reassigned: either accept, writing the final reply yourself and telling the user honestly what remains unresolved, or takeover and finish it yourself."
	switch {
	case canRevise:
		verdicts = `"accept" | "revise" | "takeover"`
		extra = "- Send it back (revise) only when content is missing, the conclusion does not hold up, or more lookups or work are needed. Make feedback specific so the task model knows what to do next."
	case canReassign:
		verdicts = `"accept" | "reassign" | "takeover"`
		extra = fmt.Sprintf("- This result has already been sent back %d time(s) and cannot go back to the same task model again. "+
			"If it is barely usable, accept: write the final reply yourself and tell the user honestly what remains unresolved. "+
			"If it is still unusable, hand the task to another task model (reassign) and state in feedback what falls short; the model taking over will see it.", round)
	}
	extra += "\n- You cannot call tools in this review step. When you need to verify a key fact yourself, or only a little is missing and it is faster to add it yourself, choose takeover: " +
		"after taking over you can call every tool and reply to the user directly when done. Put in feedback what you intend to verify or complete."
	style := p.opts.styleRule() + "\nThis requirement also governs answer."
	segs, err := p.contextSegments(ctx, t, history)
	if err != nil {
		return spent, err
	}
	// 用户的表达风格只给主模型：给用户的答复是它写的。
	if p.voice != nil {
		v, err := p.voice.Voice(ctx)
		if err != nil {
			return spent, fmt.Errorf("agent: 读取表达风格失败: %w", err)
		}
		if rule := ps.get(PromptVoice); !v.Empty() && rule != "" {
			style += "\n\n" + rule
			segs = append(segs[:1], append([]llm.Segment{voiceSegment(v)}, segs[1:]...)...)
		}
	}
	segs = append(segs, llm.Segment{
		Role: llm.RoleUser, Content: "任务模型交回的结果：\n" + d.Reason,
		Trust: core.TrustToolOutput, Source: "任务模型的结果", Pinned: true,
	})
	body := ps.get(PromptReview)
	// 用户改写这一段时可能去掉了占位符：选项和这一轮的规则仍然要让模型看到，接在后面。
	if !strings.Contains(body, "{{verdicts}}") {
		body += "\nverdict must be one of: {{verdicts}}"
	}
	if !strings.Contains(body, "{{rules}}") {
		body += "\n{{rules}}"
	}
	body = strings.NewReplacer("{{verdicts}}", verdicts, "{{rules}}", extra).Replace(body)
	system := sections(ps.get(PromptBase), body, style) + p.clock()
	var out struct {
		Verdict  string `json:"verdict"`
		Answer   string `json:"answer"`
		Feedback string `json:"feedback"`
	}
	if err := p.askJSON(ctx, t, true, system, segs, &spent, "审核结果", &out); err != nil {
		return spent, err
	}
	feedback := strings.TrimSpace(out.Feedback)
	switch out.Verdict {
	case "accept", "revise", "reassign", "takeover":
	default:
		return spent, fmt.Errorf("%w: 审核结果的 verdict 不合法: %q", errBadOutput, out.Verdict)
	}
	if out.Verdict == "revise" && canRevise && feedback != "" {
		return internalStep(spent, ToolReview,
			reviewRecord{Round: round + 1, Proposed: d.Reason, Feedback: feedback}, "主模型审核：需要修改")
	}
	if out.Verdict == "takeover" && feedback != "" {
		return internalStep(spent, ToolReview,
			reviewRecord{Round: round + 1, Proposed: d.Reason, Feedback: feedback, Takeover: true}, "主模型审核：由主模型接手")
	}
	if out.Verdict == "reassign" && canReassign && feedback != "" {
		// 没有别的任务模型可换时，照下面的路收尾。
		if w, err := p.team.PickExcept(ctx, team.Category(cur.Category), state.tried()); err == nil {
			return internalStep(spent, ToolAssign,
				assignRecord{Worker: w.Name, Category: cur.Category, Title: cur.Title, Round: cur.Round + 1, Why: feedback},
				fmt.Sprintf("主模型审核：改派给 %s 办理", w.Name))
		}
	}

	if err := p.recordWork(ctx, t, state); err != nil {
		return spent, err
	}
	spent.Kind = kernel.DecideFinish
	spent.Reason = strings.TrimSpace(out.Answer)
	if spent.Reason == "" {
		// 主模型没有写出答复时，用任务模型的结论，总比没有强。
		spent.Reason = d.Reason
	}
	return spent, nil
}

// recordWork 在答复要到用户手里时记下这件事是谁办的：用户的评分会算在它的评分里。
// 同一个编号再记一次不改变已有的评分，所以这一步重跑也没有关系。
func (p *Planner) recordWork(ctx context.Context, t *core.Task, state delegation) error {
	cur := state.current()
	return p.team.Record(ctx, &team.Work{
		ID: team.WorkID(t.ID, state.assignSeq, 0), TaskID: t.ID, StepSeq: state.assignSeq,
		Worker: cur.Worker, Category: team.Category(cur.Category), Title: cur.Title,
		CreatedAt: time.Now().UTC(),
	})
}

// WithActionReview 让会修改、删除已有东西的动作在执行前先由主模型把关。
//
// 自主模式下用：那时这类动作不再等用户确认，把关的换成了主模型。
func (p *Planner) WithActionReview(enabled bool) *Planner {
	p.reviewActions = enabled
	return p
}

// needsMainReview 报告一个动作执行前要不要主模型把关。
//
// 主模型管的是内部的事。新写入的东西不用把关：记一条日程、写一个文件、装一个扫描通过的 Skill，
// 错了也只是多出一样东西。要把关的是动已有东西的：删日程，以及改已有的日程、挪动邮件这两种虽然撤得回来、
// 但动的是用户原有数据的。
// 对外发送的动作（发信、付款、通过 MCP 提交信息）不在这里：那一类由用户确认，不由模型判断，见 outbound。
func needsMainReview(d kernel.Decision) bool {
	if d.Kind != kernel.DecideStep || isInternalTool(d.ToolName) || outbound(d.ToolName, d.SideEffect) {
		return false
	}
	return d.SideEffect == core.SideEffectIrreversible ||
		d.ToolName == ToolCalendarUpdate || d.ToolName == ToolMailMove
}

// guard 让主模型给一个要修改、删除已有东西的动作把关：同意就照原样执行，不同意就把理由记进日志，
// 提出这个动作的模型下一步会看到。
func (p *Planner) guard(ctx context.Context, t *core.Task, history []*core.Step, d kernel.Decision) (kernel.Decision, error) {
	// 提出这个动作的那一次决策同样花了钱。
	spent := kernel.Decision{TokensIn: d.TokensIn, TokensOut: d.TokensOut, CostMicros: d.CostMicros}
	ps, err := p.loadPrompts(ctx)
	if err != nil {
		return spent, err
	}
	segs, err := p.contextSegments(ctx, t, history)
	if err != nil {
		return spent, err
	}
	segs = append(segs, llm.Segment{
		Role: llm.RoleUser,
		Content: fmt.Sprintf("待执行的动作：\n工具：%s\n副作用等级：%s\n参数：%s\n提出它的理由：%s",
			d.ToolName, d.SideEffect, truncate(string(d.Input), 4000), d.Reason),
		// 动作是模型提的，参数里可能带着它从外部内容抄来的话。
		Trust: core.TrustToolOutput, Source: "待执行的动作", Pinned: true,
	})
	var out struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := p.askJSON(ctx, t, true, sections(ps.get(PromptBase), ps.get(PromptGuard))+p.clock(), segs, &spent, "把关结果", &out); err != nil {
		return spent, err
	}
	switch out.Verdict {
	case "approve":
		d.TokensIn, d.TokensOut, d.CostMicros = spent.TokensIn, spent.TokensOut, spent.CostMicros
		// 附带的查询不受影响，照常跟着执行。
		return d, nil
	case "reject":
		return internalStep(spent, ToolVeto,
			vetoRecord{Tool: d.ToolName, Params: d.Input, Reason: strings.TrimSpace(out.Reason)},
			"主模型没有同意执行 "+d.ToolName)
	}
	return spent, fmt.Errorf("%w: 把关结果的 verdict 不合法: %q", errBadOutput, out.Verdict)
}
