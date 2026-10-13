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
	// ToolReview 记下主模型的一次退回意见。
	ToolReview = "task.review"
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
	return name == ToolAssign || name == ToolBrief || name == ToolReview
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
				d.brief, d.reviews = nil, nil
			}
		case ToolBrief:
			var b briefRecord
			if json.Unmarshal(s.Output, &b) == nil {
				d.brief = &b
			}
		case ToolReview:
			var r reviewRecord
			if json.Unmarshal(s.Output, &r) == nil {
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
	if len(d.reviews) > 0 {
		last := d.reviews[len(d.reviews)-1]
		pin("主模型的审核意见", fmt.Sprintf("主模型审核了上一次交回的结果，认为还没有达到要求（第 %d 次退回）。\n上一次交回的结果：\n%s\n\n修改意见：\n%s",
			last.Round, truncate(last.Proposed, 4000), last.Feedback))
	}
	return segs
}

// workerLanguageRule 是任务模型的语言要求。它排在基础约束之后，与基础约束里的语言一节不一致时以它为准。
const workerLanguageRule = `语言（任务模型适用，与前面"语言"一节不一致时以这里为准）：
- 用英文思考。你写的方案、理由（reason）、交回的结论，一律用英文：它们是给主模型看的，不直接给用户。
- 两个例外：question 会直接发给用户，用简体中文写；替用户写的东西（邮件、文档、消息）和工具参数里的内容
  （地名、搜索词、日程标题），按那份东西本身该用的语言写。
- 人名、地名、术语、引用的原文保留原样，不要翻译。`

// mainLanguageRule 是主模型做指派、评估方案、审核时的语言要求。
const mainLanguageRule = `语言：
- 用英文思考和推理。
- 写给任务模型看的内容（对方案的修改和说明、退回意见）用英文。
- 写给用户看的内容（title、answer）用简体中文，除非用户明确要求用别的语言。`

// delegateRule 是任务模型执行时加进系统提示的一节。
const delegateRule = `你是任务模型：
- 这件事由你来办。方案已经定了，见上下文里的「定下来的方案」：它是你起草、主模型评估过的。
  实际情况与方案设想的不一样时，按你自己的判断调整，并在结论里说明改了什么、为什么。
- 你输出 finish 时，reason 里的结论不会直接发给用户，而是先交给主模型审核，由主模型写给用户的答复。
  所以结论要写全：办了什么、结果是什么、依据来自哪一步、哪些没办成或没有数据。
- 主模型认为没达到要求时，会把修改意见放进上下文里的「主模型的审核意见」。
  按意见补查、补做、改正之后再 finish；意见里你认为不对的地方，在结论里说明理由，不要置之不理。`

const assignPrompt = `你是原子助手的主模型。用户交来一件事，你不亲自办，而是交给一个任务模型去办：
它先写方案给你评估，办完再把结果交给你审核。现在先判断这件事属于哪一类，系统据此选出任务模型。

必须输出一个 JSON 对象，不要输出任何其他内容：
{
  "category": "这件事的类别，从下面的类别里选一个",
  "title": "一句话说这件事是什么，用户会看到"
}

- 类别按这件事主要考验的能力来选。`

const draftPrompt = `你是任务模型。主模型把用户的一件事交给了你。动手之前，先写一份简略的方案交给主模型评估。

必须输出一个 JSON 对象，不要输出任何其他内容：
{
  "steps": ["打算拆成的小步骤，按先后排"],
  "tools": ["打算用到的工具全名，只能从下面的可用工具里选"],
  "considerations": ["需要考虑的因素、限制、容易出错的地方"],
  "done_when": "做到什么样算完成，交回的结果里会有什么"
}

规则：
- 方案要简略：每一步一句话，写清先做什么、后做什么。一两句话就能办完的小事，写一两步即可，不要硬拆。
- 现在只写方案，不要调用工具，也不要给出结论。
- 工具只列确实用得上的。这件事不需要工具就留空。
- 要考虑的东西写具体：以出行为例，出发地、时间、交通、天气、节假日、预算、用户自己的习惯。
- 不替用户假设他没说过的要求。有会影响结果的含糊之处，写进 considerations，并把"先向用户问清"列为一步。`

const planReviewPrompt = `你是原子助手的主模型。你把用户的一件事交给了任务模型，它写了一份简略的方案。
请评估这份方案：可以直接同意，也可以修改。

必须输出一个 JSON 对象，不要输出任何其他内容：
{
  "verdict": "approve" | "amend",
  "steps": ["verdict=amend 时填：修改后的完整步骤"],
  "tools": ["verdict=amend 时填：修改后的工具全名，只能从下面的可用工具里选"],
  "considerations": ["verdict=amend 时填：修改后的完整列表"],
  "done_when": "verdict=amend 时填：修改后的完成标准",
  "note": "verdict=amend 时填：你改了什么、为什么"
}

规则：
- 对照用户的原始要求看：步骤能不能办成这件事，有没有漏掉会影响结论的因素，有没有多余的、会白花钱或者越权的步骤，
  完成标准是不是用户真正要的。
- 方案没有问题就直接同意（approve），其余栏目留空。不要为了显得把过关而改。
- 要改就给出修改后的完整方案（amend），没改的条目照抄过来。
- 不替用户假设他没说过的要求。`

const reviewPrompt = `你是原子助手的主模型。你把用户的一件事交给了任务模型去办，它现在交回了结果。
请研判这份结果是否达到了用户的要求，再决定怎么处理。

必须输出一个 JSON 对象，不要输出任何其他内容：
{
  "verdict": %s,
  "answer": "verdict=accept 时必填：反馈给用户的最终答复",
  "feedback": "verdict 不是 accept 时必填：哪里没有达到要求，要补什么、改什么"
}

规则：
- 对照用户的原始要求和方案里的完成标准逐项看：要的有没有给到，结论站不站得住，有没有遗漏、自相矛盾。
- 结果里说办了的事、引用的数字和事实，要在上下文的步骤里找得到对应的成功结果。找不到的按没办、没依据处理。
- 措辞、格式、篇幅、语言这类小毛病不算没达到要求，你在 answer 里直接改好。
- 你自己没有工具，不能去查任何东西：answer 里不能出现结果和步骤里都没有的事实。
- answer 是直接给用户看的：任务模型的结果是英文的，你要用简体中文重新写给用户；
  不要提"任务模型""审核"这些内部过程，没办成、没有数据的部分如实说明。
%s`

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
func (p *Planner) askJSON(ctx context.Context, main bool, system string, segs []llm.Segment, spent *kernel.Decision, what string, out any) error {
	msgs, err := llm.Build(segs, llm.BuildOptions{SystemPrompt: system, MaxChars: p.opts.MaxChars + len(system)})
	if err != nil {
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
	return "\n\n当前时间：" + p.now().Local().Format("2006-01-02 15:04 Monday")
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
	base, _, err := p.promptTexts(ctx)
	if err != nil {
		return kernel.Decision{}, err
	}
	var cats []string
	for _, c := range team.Categories {
		cats = append(cats, fmt.Sprintf("- %s（%s）：%s", c.ID, c.Name, c.Hint))
	}
	system := base + "\n\n" + assignPrompt + "\n\n类别：\n" + strings.Join(cats, "\n") + "\n\n" + mainLanguageRule + p.clock()
	segs, err := p.contextSegments(ctx, t, history)
	if err != nil {
		return kernel.Decision{}, err
	}
	var spent kernel.Decision
	var out struct {
		Category string `json:"category"`
		Title    string `json:"title"`
	}
	if err := p.askJSON(ctx, true, system, segs, &spent, "指派结果", &out); err != nil {
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
	base, _, err := p.promptTexts(ctx)
	if err != nil {
		return kernel.Decision{}, err
	}
	tools := "\n\n可用工具：\n" + p.toolList()
	segs, err := p.contextSegments(ctx, t, history)
	if err != nil {
		return kernel.Decision{}, err
	}
	var spent kernel.Decision
	var plan briefRecord
	if err := p.askJSON(ctx, false, base+"\n\n"+draftPrompt+"\n\n"+workerLanguageRule+p.clock()+tools,
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
	if err := p.askJSON(ctx, true, base+"\n\n"+planReviewPrompt+"\n\n"+mainLanguageRule+p.clock()+tools,
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
	base, _, err := p.promptTexts(ctx)
	if err != nil {
		return spent, err
	}
	cur := state.current()
	round := len(state.reviews)
	canRevise := round < maxReviewRounds
	// 退回的次数用完了才谈得上换人。
	canReassign := !canRevise && len(state.assigns) <= maxReassign
	verdicts, extra := `"accept"`, "- 这份结果不能再退回，也不能再换人了：verdict 必须是 accept。由你写出最终答复，还没解决的问题如实告诉用户。"
	switch {
	case canRevise:
		verdicts = `"accept" | "revise"`
		extra = "- 只有缺了内容、结论站不住、需要补查补做时才退回（revise）。feedback 写具体，让任务模型知道下一步做什么。"
	case canReassign:
		verdicts = `"accept" | "reassign"`
		extra = fmt.Sprintf("- 这份结果已经退回过 %d 次，不能再退回给同一个任务模型了。"+
			"结果勉强可用就 accept，由你写出最终答复，没解决的问题如实告诉用户；"+
			"仍然不可用就改派给另一个任务模型（reassign），feedback 写清没有达到要求的地方，接手的模型会看到。", round)
	}
	style := p.opts.styleRule() + "\n这条要求同样约束 answer。"
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
		if !v.Empty() {
			style += "\n\n" + voiceRule
			segs = append(segs[:1], append([]llm.Segment{voiceSegment(v)}, segs[1:]...)...)
		}
	}
	segs = append(segs, llm.Segment{
		Role: llm.RoleUser, Content: "任务模型交回的结果：\n" + d.Reason,
		Trust: core.TrustToolOutput, Source: "任务模型的结果", Pinned: true,
	})
	system := base + "\n\n" + fmt.Sprintf(reviewPrompt, verdicts, extra) + "\n\n" + mainLanguageRule + "\n\n" + style + p.clock()
	var out struct {
		Verdict  string `json:"verdict"`
		Answer   string `json:"answer"`
		Feedback string `json:"feedback"`
	}
	if err := p.askJSON(ctx, true, system, segs, &spent, "审核结果", &out); err != nil {
		return spent, err
	}
	feedback := strings.TrimSpace(out.Feedback)
	switch out.Verdict {
	case "accept", "revise", "reassign":
	default:
		return spent, fmt.Errorf("%w: 审核结果的 verdict 不合法: %q", errBadOutput, out.Verdict)
	}
	if out.Verdict == "revise" && canRevise && feedback != "" {
		return internalStep(spent, ToolReview,
			reviewRecord{Round: round + 1, Proposed: d.Reason, Feedback: feedback}, "主模型审核：需要修改")
	}
	if out.Verdict == "reassign" && canReassign && feedback != "" {
		// 没有别的任务模型可换时，照下面的路收尾。
		if w, err := p.team.PickExcept(ctx, team.Category(cur.Category), state.tried()); err == nil {
			return internalStep(spent, ToolAssign,
				assignRecord{Worker: w.Name, Category: cur.Category, Title: cur.Title, Round: cur.Round + 1, Why: feedback},
				fmt.Sprintf("主模型审核：改派给 %s 办理", w.Name))
		}
	}

	// 答复要到用户手里了：记下这件事是谁办的，用户的评分会算在它的评分里。
	// 同一个编号再记一次不改变已有的评分，所以这一步重跑也没有关系。
	if err := p.team.Record(ctx, &team.Work{
		ID: team.WorkID(t.ID, state.assignSeq, 0), TaskID: t.ID, StepSeq: state.assignSeq,
		Worker: cur.Worker, Category: team.Category(cur.Category), Title: cur.Title,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
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
