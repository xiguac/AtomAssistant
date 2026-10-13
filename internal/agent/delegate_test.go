package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/style"
	"github.com/xiguac/atomhelper/internal/team"
)

// scriptedWorkers 模拟任务模型：按顺序给出事先写好的回答，记下是谁被叫到、看到了什么。
type scriptedWorkers struct {
	replies []string
	names   []string
	reqs    []llm.Request
	// gone 为真表示配置里已经没有这个模型了。
	gone bool
}

func (f *scriptedWorkers) CompleteWith(_ context.Context, worker string, req llm.Request) (llm.Response, error) {
	if f.gone {
		return llm.Response{}, llm.ErrNoProvider
	}
	i := len(f.reqs)
	f.names, f.reqs = append(f.names, worker), append(f.reqs, req)
	if i >= len(f.replies) {
		i = len(f.replies) - 1
	}
	return llm.Response{Content: f.replies[i], Model: "m-" + worker,
		Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20}, CostMicros: 3}, nil
}

// text 把一次请求的全部消息连成一段，方便断言模型看到了什么。
func text(req llm.Request) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(m.Content + "\n")
	}
	return sb.String()
}

const (
	assignReply = `{"category":"analysis","title":"周末出行建议"}`
	planReply   = `{"steps":["Check Saturday weather","Recommend a day"],"tools":["demo.weather","no.such_tool","task.review"],` +
		`"considerations":["Departure city is unknown"],"done_when":"Says which day and why"}`
)

// delegated 装出一套"主模型 + 任务模型"的规划器，以及一个帮测试把决策落成步骤的函数。
func delegated(t *testing.T, main *fakeModel, workers *scriptedWorkers) (*Planner, *team.MemStore, func(task *core.Task, history []*core.Step, d kernel.Decision) []*core.Step) {
	t.Helper()
	store := team.NewMemStore()
	board := team.NewBoard(store, []team.Worker{
		{Name: "写手", GoodAt: []team.Category{team.CatWriting}},
		{Name: "分析师", GoodAt: []team.Category{team.CatAnalysis}},
	})
	p := NewPlanner(main, catalogWith(core.SideEffectIrreversible), PlannerOptions{}).WithTaskModels(board, workers)
	a := NewActuator(nil, main)
	run := func(task *core.Task, history []*core.Step, d kernel.Decision) []*core.Step {
		t.Helper()
		if d.Kind != kernel.DecideStep || !isInternalTool(d.ToolName) || d.SideEffect != core.SideEffectNone {
			t.Fatalf("这里应是规划器自己写的一条记录: %+v", d)
		}
		s := core.NewStep(task.ID, len(history)+1, d.Type, d.Input)
		s.ToolName, s.SideEffect = d.ToolName, d.SideEffect
		res, err := a.Execute(context.Background(), task, s)
		if err != nil {
			t.Fatalf("执行 %s 失败: %v", d.ToolName, err)
		}
		s.Complete(res.Output, res.Trust)
		return append(history, s)
	}
	return p, store, run
}

func weatherStep(taskID string, seq int, tool, in, out string) *core.Step {
	s := core.NewStep(taskID, seq, core.StepTool, json.RawMessage(in))
	s.ToolName, s.SideEffect = tool, core.SideEffectReadOnly
	s.Complete(json.RawMessage(out), core.TrustToolOutput)
	return s
}

type shortVoice struct{}

func (shortVoice) Voice(context.Context) (style.Voice, error) {
	return style.Voice{Scenes: []style.SceneVoice{{Scene: style.SceneChat, Description: "说话很短，不用客套"}}}, nil
}

func TestDelegate_指派方案执行审核的完整流程(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("周末去哪", "这个周末适合出门吗", core.TrustUser)
	main := &fakeModel{replies: []string{
		assignReply,
		`{"verdict":"amend","steps":["Check Saturday and Sunday weather","Recommend a day"],"tools":["demo.weather"],` +
			`"considerations":["Departure city is unknown"],"done_when":"Says which day and why","note":"Sunday was missing"}`,
		`{"verdict":"revise","feedback":"Sunday weather was not checked"}`,
		`{"verdict":"accept","answer":"周六去：晴，21 度；周日有雨。"}`,
	}}
	workers := &scriptedWorkers{replies: []string{
		planReply,
		`{"action":"tool","tool":"demo.weather","params":{"city":"上海"},"reason":"Check Saturday weather"}`,
		`{"action":"finish","reason":"Saturday is sunny, go on Saturday"}`,
		`{"action":"finish","reason":"Saturday sunny 21C, Sunday rain; go on Saturday"}`,
	}}
	p, store, run := delegated(t, main, workers)
	p = p.WithVoice(shortVoice{})

	// 一、主模型判断类别，系统按类别选出任务模型。任务模型还没有被叫到。
	d, err := p.Decide(ctx, task, nil)
	if err != nil || d.ToolName != ToolAssign || len(workers.reqs) != 0 || d.TokensIn == 0 {
		t.Fatalf("第一步应是主模型的指派，用量要记账: %+v %v", d, err)
	}
	var assign assignRecord
	if json.Unmarshal(d.Input, &assign) != nil || assign.Worker != "分析师" || assign.Round != 1 || assign.Title != "周末出行建议" {
		t.Fatalf("指派不对: %+v", assign)
	}
	system := main.lastReq.Messages[0].Content
	if !strings.HasPrefix(system, baseRules) || !strings.Contains(system, "analysis（分析推理）") || !strings.Contains(system, "用英文思考和推理") {
		t.Fatal("指派时主模型应看到基础约束、类别和语言要求")
	}
	history := run(task, nil, d)

	// 二、任务模型起草方案，主模型评估后改了一处。记下来的是改过的那份。
	d, err = p.Decide(ctx, task, history)
	if err != nil || d.ToolName != ToolBrief {
		t.Fatalf("第二步应定下方案: %+v %v", d, err)
	}
	if workers.names[0] != "分析师" || !strings.Contains(text(workers.reqs[0]), "这个周末适合出门吗") ||
		!strings.Contains(workers.reqs[0].Messages[0].Content, "一律用英文") {
		t.Fatal("方案应由选出的任务模型起草，它看得到原始任务，并被要求用英文")
	}
	if !strings.Contains(text(main.lastReq), "Check Saturday weather") {
		t.Fatal("主模型评估时应看到任务模型起草的方案")
	}
	var brief briefRecord
	if json.Unmarshal(d.Input, &brief) != nil || !brief.Amended || brief.Note != "Sunday was missing" ||
		brief.Steps[0] != "Check Saturday and Sunday weather" || len(brief.Tools) != 1 {
		t.Fatalf("定下来的应是主模型改过的方案: %+v", brief)
	}
	// 两个模型的用量都要记上。
	if d.TokensIn != 110 || d.CostMicros != 3 {
		t.Fatalf("起草与评估的用量都应记账: %+v", d)
	}
	history = run(task, history, d)

	// 三、任务模型按方案去办，调工具。它看不到用户的表达风格。
	d, err = p.Decide(ctx, task, history)
	if err != nil || d.ToolName != "demo.weather" || d.SideEffect != core.SideEffectReadOnly {
		t.Fatalf("任务模型应调工具，副作用等级取自目录: %+v %v", d, err)
	}
	seen := text(workers.reqs[len(workers.reqs)-1])
	for _, want := range []string{"这个周末适合出门吗", "Check Saturday and Sunday weather", "Sunday was missing", "你是任务模型", "一律用英文"} {
		if !strings.Contains(seen, want) {
			t.Fatalf("任务模型应看到原始任务和定下来的方案，缺 %q", want)
		}
	}
	if strings.Contains(seen, "说话很短，不用客套") {
		t.Fatal("用户的表达风格不应交给任务模型")
	}
	history = append(history, weatherStep(task.ID, len(history)+1, "demo.weather", `{"city":"上海"}`, `{"temp":21}`))

	// 四、任务模型说办完了，主模型审核后退回。任务还没有结束，也还没有可评分的活。
	calls := main.calls
	d, err = p.Decide(ctx, task, history)
	if err != nil || d.ToolName != ToolReview || main.calls != calls+1 {
		t.Fatalf("主模型不满意时应退回，而不是结束: %+v %v", d, err)
	}
	reviewSeen := text(main.lastReq)
	if !strings.Contains(reviewSeen, "Saturday is sunny, go on Saturday") || !strings.Contains(reviewSeen, "说话很短，不用客套") {
		t.Fatal("主模型审核时应看到任务模型交回的结果和用户的表达风格")
	}
	if works, _ := store.WorkOfTask(ctx, task.ID); len(works) != 0 {
		t.Fatal("答复还没到用户手里，不应有可评分的活")
	}
	history = run(task, history, d)

	// 五、任务模型看到意见，改完再交；主模型认可，把自己用中文写的答复给用户。
	d, err = p.Decide(ctx, task, history)
	if err != nil || d.Kind != kernel.DecideFinish || d.Reason != "周六去：晴，21 度；周日有雨。" {
		t.Fatalf("审核通过后应以主模型的答复结束: %+v %v", d, err)
	}
	if seen = text(workers.reqs[len(workers.reqs)-1]); !strings.Contains(seen, "Sunday weather was not checked") {
		t.Fatal("任务模型应看到主模型的审核意见")
	}
	// 办这件事的模型记下一份活，等用户评分。
	works, _ := store.WorkOfTask(ctx, task.ID)
	if len(works) != 1 || works[0].Worker != "分析师" || works[0].Title != "周末出行建议" || works[0].Category != team.CatAnalysis {
		t.Fatalf("应记下是谁办的，供用户评分: %+v", works)
	}
}

func TestDelegate_主模型可以直接同意方案(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("t", "g", core.TrustUser)
	main := &fakeModel{replies: []string{assignReply, `{"verdict":"approve"}`}}
	workers := &scriptedWorkers{replies: []string{planReply}}
	p, _, run := delegated(t, main, workers)
	d, err := p.Decide(ctx, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err = p.Decide(ctx, task, run(task, nil, d))
	var brief briefRecord
	if err != nil || json.Unmarshal(d.Input, &brief) != nil || brief.Amended || brief.Steps[0] != "Check Saturday weather" {
		t.Fatalf("同意时用的是任务模型起草的方案: %+v %v", brief, err)
	}
	// 方案里建议的工具只留真的有的，规划器自己写日志用的不算。
	if len(brief.Tools) != 1 || brief.Tools[0] != "demo.weather" {
		t.Fatalf("不存在的工具应去掉: %v", brief.Tools)
	}
}

func TestDelegate_多轮仍不满意时改派给别的任务模型(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("t", "g", core.TrustUser)
	main := &fakeModel{replies: []string{
		assignReply, `{"verdict":"approve"}`,
		`{"verdict":"revise","feedback":"Not enough"}`, `{"verdict":"revise","feedback":"Still not enough"}`,
		`{"verdict":"reassign","feedback":"The conclusion has no evidence"}`,
		`{"verdict":"approve"}`,
		// 换过一次人之后，退回的次数用完就只能收尾了：主模型再说 reassign 也不算数。
		`{"verdict":"revise","feedback":"a"}`, `{"verdict":"revise","feedback":"b"}`,
		`{"verdict":"reassign","feedback":"c"}`,
	}}
	workers := &scriptedWorkers{replies: []string{
		planReply, `{"action":"finish","reason":"first try"}`, `{"action":"finish","reason":"second try"}`,
		`{"action":"finish","reason":"third try"}`,
		planReply, `{"action":"finish","reason":"new model result"}`,
	}}
	p, store, run := delegated(t, main, workers)

	var history []*core.Step
	step := func() kernel.Decision {
		t.Helper()
		d, err := p.Decide(ctx, task, history)
		if err != nil {
			t.Fatalf("决策失败: %v", err)
		}
		if d.Kind == kernel.DecideStep {
			history = run(task, history, d)
		}
		return d
	}
	step() // 指派
	step() // 方案
	for i := 0; i < maxReviewRounds; i++ {
		if d := step(); d.ToolName != ToolReview {
			t.Fatalf("第 %d 次应退回: %+v", i+1, d)
		}
	}
	// 退回的次数用完了，主模型决定换人：换的是没办过这件事的那个。
	d := step()
	var assign assignRecord
	if d.ToolName != ToolAssign || json.Unmarshal(d.Input, &assign) != nil ||
		assign.Worker != "写手" || assign.Round != 2 || assign.Why != "The conclusion has no evidence" {
		t.Fatalf("应改派给另一个任务模型: %+v %+v", d, assign)
	}
	if !strings.Contains(main.lastReq.Messages[0].Content, `"accept" | "reassign"`) {
		t.Fatal("退回次数用完后，主模型的选择应是认可或换人")
	}
	// 接手的模型从方案开始重来，并且知道为什么换人。
	if d = step(); d.ToolName != ToolBrief {
		t.Fatalf("改派之后应重新定方案: %+v", d)
	}
	last := len(workers.reqs) - 1
	if workers.names[last] != "写手" || !strings.Contains(text(workers.reqs[last]), "The conclusion has no evidence") {
		t.Fatal("接手的应是新的任务模型，它应看到改派的原因")
	}
	for i := 0; i < maxReviewRounds; i++ {
		if d = step(); d.ToolName != ToolReview {
			t.Fatalf("新模型的第 %d 次应退回: %+v", i+1, d)
		}
	}
	// 不能再换人了：任务结束，主模型没写答复就用任务模型的结论。
	d = step()
	if d.Kind != kernel.DecideFinish || d.Reason != "new model result" {
		t.Fatalf("到头了应结束: %+v", d)
	}
	if !strings.Contains(main.lastReq.Messages[0].Content, "verdict 必须是 accept") {
		t.Fatal("最后一次审核应告诉主模型必须收尾")
	}
	// 可评分的只有答复到了用户手里的那个模型，中途被换下去的不记。
	works, _ := store.WorkOfTask(ctx, task.ID)
	if len(works) != 1 || works[0].Worker != "写手" {
		t.Fatalf("只应记下最后办成这件事的模型: %+v", works)
	}
}

func TestDelegate_评分高的任务模型优先(t *testing.T) {
	ctx := context.Background()
	store := team.NewMemStore()
	board := team.NewBoard(store, []team.Worker{{Name: "甲"}, {Name: "乙"}})
	// 两个都办过这一类的事：用户给甲差评，给乙好评。
	for i, w := range []string{"甲", "乙"} {
		id := team.WorkID("old", i, 0)
		if err := store.PutWork(ctx, &team.Work{ID: id, TaskID: "old", StepSeq: i, Worker: w, Category: team.CatAnalysis}); err != nil {
			t.Fatal(err)
		}
		if err := store.Rate(ctx, id, team.Verdict(w == "乙", team.RatedByUser), team.RatedByUser); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPlanner(&fakeModel{replies: []string{assignReply}}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).
		WithTaskModels(board, &scriptedWorkers{})
	d, err := p.Decide(ctx, core.NewTask("t", "g", core.TrustUser), nil)
	var assign assignRecord
	if err != nil || json.Unmarshal(d.Input, &assign) != nil || assign.Worker != "乙" {
		t.Fatalf("同类的事应交给评分高的: %+v %v", assign, err)
	}
	// 换人时跳过已经办过的；都办过了就没人可换。
	if w, err := board.PickExcept(ctx, team.CatAnalysis, []string{"乙"}); err != nil || w.Name != "甲" {
		t.Fatalf("换人应跳过办过的: %+v %v", w, err)
	}
	if _, err := board.PickExcept(ctx, team.CatAnalysis, []string{"甲", "乙"}); err == nil {
		t.Fatal("都办过了应无人可换")
	}
}

func TestDelegate_提问直接到用户审核结果不可用时报错(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("t", "g", core.TrustUser)
	main := &fakeModel{replies: []string{assignReply, `{"verdict":"approve"}`, `{"verdict":"maybe"}`}}
	workers := &scriptedWorkers{replies: []string{
		planReply,
		`{"action":"ask","question":"从哪里出发？"}`,
		`{"action":"finish","reason":"done"}`,
	}}
	p, _, run := delegated(t, main, workers)
	d, err := p.Decide(ctx, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	history := run(task, nil, d)
	if d, err = p.Decide(ctx, task, history); err != nil {
		t.Fatal(err)
	}
	history = run(task, history, d)

	// 任务模型要问用户：照常进入等待，不经过审核。
	d, err = p.Decide(ctx, task, history)
	if err != nil || d.Kind != kernel.DecideWait || !strings.Contains(d.Reason, "从哪里出发") || main.calls != 2 {
		t.Fatalf("提问应直接到用户: %+v %v", d, err)
	}
	// 主模型的审核答非所问：这一步算决策失败，任务不能就此结束。
	if d, err = p.Decide(ctx, task, history); err == nil || d.Kind == kernel.DecideFinish {
		t.Fatalf("审核结果不可用时不应结束任务: %+v", d)
	}
}

func TestDelegate_任务模型不在了由主模型接着办(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("t", "g", core.TrustUser)
	main := &fakeModel{replies: []string{
		assignReply, `{"verdict":"approve"}`,
		`{"action":"tool","tool":"demo.weather","params":{},"reason":"查天气"}`,
	}}
	workers := &scriptedWorkers{replies: []string{planReply}}
	p, _, run := delegated(t, main, workers)
	d, err := p.Decide(ctx, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	history := run(task, nil, d)
	if d, err = p.Decide(ctx, task, history); err != nil {
		t.Fatal(err)
	}
	history = run(task, history, d)
	workers.gone = true
	if d, err = p.Decide(ctx, task, history); err != nil || d.ToolName != "demo.weather" {
		t.Fatalf("任务模型不在了应由主模型接着办: %+v %v", d, err)
	}
}

func TestDelegate_没配协作模型时照旧由主模型一步步办(t *testing.T) {
	m := &fakeModel{replies: []string{`{"action":"finish","reason":"好"}`}}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{})
	d, err := p.Decide(context.Background(), core.NewTask("t", "g", core.TrustUser), nil)
	if err != nil || d.Kind != kernel.DecideFinish || m.calls != 1 {
		t.Fatalf("单模型模式不应有指派和审核: %+v %v", d, err)
	}
	system := m.lastReq.Messages[0].Content
	if strings.Contains(system, "你是任务模型") || !strings.Contains(system, "用英文思考和推理") || !strings.Contains(system, "用简体中文") {
		t.Fatal("单模型模式下主模型用英文思考、中文回复，系统提示里没有任务模型一节")
	}
}

func TestDelegate_方案里提到敏感内容不会被审批拒掉(t *testing.T) {
	// 方案里写一句"客户海鲜过敏"，分级器会把它判成绝密；但这一步只写任务自己的日志，不交给任何工具。
	raw, _ := json.Marshal(briefRecord{Considerations: []string{"客户海鲜过敏，避开海鲜"}})
	d := kernel.Decision{Kind: kernel.DecideStep, Type: core.StepTool, ToolName: ToolBrief,
		SideEffect: core.SideEffectNone, Input: raw}
	a := NewApprover(&policy.Policy{}, policy.NewClassifier(), NewMemoryUsageTracker())
	outcome, reason, err := a.CheckAction(context.Background(), core.NewTask("t", "g", core.TrustUser), d)
	if err != nil || outcome != kernel.ApprovalAuto {
		t.Fatalf("定方案这一步应直接放行: %s %s %v", outcome, reason, err)
	}
	// 模型自己选不到这几个名字。
	for _, name := range []string{ToolAssign, ToolBrief, ToolReview} {
		p := NewPlanner(&fakeModel{replies: []string{`{"action":"tool","tool":"` + name + `","params":{}}`}},
			catalogWith(core.SideEffectReadOnly), PlannerOptions{})
		if _, err := p.Decide(context.Background(), core.NewTask("t", "g", core.TrustUser), nil); err == nil {
			t.Fatalf("%s 不是给模型选的工具", name)
		}
	}
}
