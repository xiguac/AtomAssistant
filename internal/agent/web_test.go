package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/kernel"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/policy"
	"github.com/xiguac/atomassistant/internal/skill"
)

// fakeSearcher 是 Searcher 的测试替身。
type fakeSearcher struct {
	resp llm.Response
	err  error
	last llm.Request
}

func (f *fakeSearcher) Search(_ context.Context, req llm.Request) (llm.Response, error) {
	f.last = req
	return f.resp, f.err
}

func TestWebSearch_接入后模型看得到工具和用法(t *testing.T) {
	task := core.NewTask("t", "杭州明天天气怎么样", core.TrustUser)
	decide := func(p *Planner) (kernel.Decision, string, error) {
		m := &fakeModel{replies: []string{
			`{"action":"tool","tool":"web.search","params":{"query":"杭州 明天 天气预报"}}`,
			`{"action":"tool","tool":"web.search","params":{"query":"杭州 明天 天气预报"}}`,
		}}
		p.model = m
		d, err := p.Decide(context.Background(), task, nil)
		return d, m.lastReq.Messages[0].Content, err
	}

	d, system, err := decide(NewPlanner(nil, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithWebSearch(true))
	if err != nil || d.ToolName != ToolWebSearch || d.SideEffect != core.SideEffectReadOnly {
		t.Fatalf("联网搜索应是可直接执行的只读工具: %+v, %v", d, err)
	}
	if !strings.Contains(system, "Do not answer from memory") || !strings.Contains(system, ToolWebSearch) {
		t.Fatalf("系统提示里应说明什么时候去查:\n%s", system)
	}

	// 没接入：看不到，也调不动。
	_, system, err = decide(NewPlanner(nil, catalogWith(core.SideEffectReadOnly), PlannerOptions{}))
	if err == nil || strings.Contains(system, ToolWebSearch) {
		t.Fatalf("没接入时不该出现联网搜索: %v", err)
	}

	// 别人写的流程没声明就不能联网。
	bound := &skill.Skill{ID: "s", Name: "某流程", Goal: "g", RequiredTools: []string{"demo.weather"}, Steps: []string{"做事"}}
	task.Tags = []string{skill.TagPrefix + bound.ID}
	_, system, err = decide(NewPlanner(nil, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).
		WithWebSearch(true).WithSkills(fakeSkills{bound.ID: bound}))
	if err == nil || strings.Contains(system, "Do not answer from memory") {
		t.Fatalf("绑定了 Skill 的任务没声明就不该能联网: %v", err)
	}
}

func TestWebSearch_查到的内容按外部内容对待并带上来源(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("t", "g", core.TrustUser)
	var sources []llm.Source
	for i := 0; i < maxSources+3; i++ {
		sources = append(sources, llm.Source{Title: "站点", URL: "https://example.com/" + string(rune('a'+i))})
	}
	s := &fakeSearcher{resp: llm.Response{
		Content: " 杭州明天多云，17～26℃。 ", Sources: sources,
		Usage: llm.Usage{PromptTokens: 300, CompletionTokens: 40}, CostMicros: 12,
	}}
	a := NewActuator(&fakeTools{}, &fakeModel{}).WithWebSearch(s)

	res, err := a.Execute(ctx, task, capStep(ToolWebSearch, `{"query":"杭州 明天 天气预报"}`))
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	// 网上的内容是别人写的：之后由它引出的改动要用户确认。
	if res.Trust != core.TrustExternal {
		t.Fatalf("搜索结果应按外部内容对待，实际 %s", res.Trust)
	}
	// 搜索花的钱要记到任务头上。
	if res.TokensIn != 300 || res.TokensOut != 40 || res.CostMicros != 12 {
		t.Fatalf("用量没有带回: %+v", res)
	}
	var out struct {
		Answer  string       `json:"answer"`
		Sources []llm.Source `json:"sources"`
		Warning string       `json:"warning"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out.Answer != "杭州明天多云，17～26℃。" || len(out.Sources) != maxSources || out.Warning != "" {
		t.Fatalf("结果不对: %+v", out)
	}
	// 交给搜索模型的是问题本身，外加当前时间；任务的其他上下文不带过去。
	if len(s.last.Messages) != 2 || s.last.Messages[1].Content != "杭州 明天 天气预报" ||
		!strings.Contains(s.last.Messages[0].Content, "Current time") {
		t.Fatalf("交给搜索模型的内容不对: %+v", s.last.Messages)
	}

	// 模型没有真的去搜：必须标出来，不能让凭记忆写的话顶着搜索结果的名义。
	s.resp = llm.Response{Content: "大概是晴天", NoSearch: true}
	res, err = a.Execute(ctx, task, capStep(ToolWebSearch, `{"query":"杭州天气"}`))
	if err != nil || !strings.Contains(string(res.Output), "没有真的去搜") {
		t.Fatalf("没搜的结果应带警告: %s, %v", res.Output, err)
	}

	s.err = errors.New("上游不可达")
	if _, err := a.Execute(ctx, task, capStep(ToolWebSearch, `{"query":"杭州天气"}`)); err == nil {
		t.Fatal("搜索出错应让这一步失败")
	}
	if _, err := a.Execute(ctx, task, capStep(ToolWebSearch, `{"query":"  "}`)); err == nil {
		t.Fatal("空问题应被拒绝")
	}
	if _, err := NewActuator(&fakeTools{}, &fakeModel{}).Execute(ctx, task, capStep(ToolWebSearch, `{"query":"x"}`)); err == nil {
		t.Fatal("没接入时应报错")
	}
}

func TestWebSearch_含绝密信息的问题发不出去上过网之后的改动要确认(t *testing.T) {
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).
		WithWebSearch(true).WithCalendar(true)
	task := core.NewTask("t", "g", core.TrustUser)
	check := func(tool, params string, h *fakeHistory) kernel.ApprovalOutcome {
		t.Helper()
		pol := &policy.Policy{}
		CalendarDefaults(pol)
		d, err := p.toolDecision(tool, json.RawMessage(params), "")
		if err != nil {
			t.Fatalf("决策失败: %v", err)
		}
		out, _, err := NewApprover(pol, policy.NewClassifier(), NewMemoryUsageTracker()).WithHistory(h).
			CheckAction(context.Background(), task, d)
		if err != nil {
			t.Fatalf("判定失败: %v", err)
		}
		return out
	}
	if out := check(ToolWebSearch, `{"query":"杭州 明天 天气预报"}`, &fakeHistory{}); out != kernel.ApprovalAuto {
		t.Fatalf("普通的搜索应直接执行，实际 %s", out)
	}
	// 身份证号这类绝密数据要交给任何工具都得用户确认，搜索也不例外。
	if out := check(ToolWebSearch, `{"query":"身份证 110101199003077715 能查到什么"}`, &fakeHistory{}); out != kernel.ApprovalManual {
		t.Fatalf("含绝密信息的搜索应由用户确认，实际 %s", out)
	}
	// 上过网之后记日程：网上的内容引出的改动交给用户定夺。
	create := `{"title":"看展","start_at":"2099-01-01T10:00:00+08:00"}`
	if out := check(ToolCalendarCreate, create, &fakeHistory{}); out != kernel.ApprovalAuto {
		t.Fatalf("没上过网时记日程应直接执行，实际 %s", out)
	}
	searched := &fakeHistory{steps: []*core.Step{doneStep(ToolWebSearch, core.TrustExternal)}}
	if out := check(ToolCalendarCreate, create, searched); out != kernel.ApprovalManual {
		t.Fatalf("上过网之后记日程应由用户确认，实际 %s", out)
	}
}

func TestWebSearch_较早查到的结论留在上下文里不必再查(t *testing.T) {
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{RecentSteps: 2}).WithWebSearch(true)
	task := core.NewTask("t", "规划北京行程", core.TrustUser)
	long := strings.Repeat("票", findingChars+50)
	var history []*core.Step
	add := func(tool, input, output string, status core.StepStatus) {
		s := core.NewStep(task.ID, len(history)+1, core.StepTool, json.RawMessage(input))
		s.ToolName, s.Status, s.Output, s.OutputTrust = tool, status, json.RawMessage(output), core.TrustExternal
		history = append(history, s)
	}
	add(ToolWebSearch, `{"query":"故宫 门票 价格"}`, `{"answer":"结论：旺季 60 元。`+long+`"}`, core.StepDone)
	add(ToolWebSearch, `{"query":"失败的那次"}`, `{}`, core.StepFailed)
	add(ToolCalendarEvents, `{}`, `{"events":[]}`, core.StepDone)
	add(ToolWebSearch, `{"query":"慕田峪 交通"}`, `{"answer":"结论：916 路转 h23。"}`, core.StepDone)
	add(ToolWebSearch, `{"query":"最近一步"}`, `{"answer":"最近的结果"}`, core.StepDone)
	add(ToolWebSearch, `{"query":"最近两步"}`, `{"answer":"最近的结果"}`, core.StepDone)
	add(ToolWebSearch, `{"query":"最近三步"}`, `{"answer":"最近的结果"}`, core.StepDone)

	var found *llm.Segment
	for _, seg := range p.segments(task, history) {
		if seg.Source == "较早的联网搜索结果" {
			found = &seg
		}
	}
	if found == nil {
		t.Fatal("较早的搜索结论应留在上下文里")
	}
	// 网上的内容，照旧是外部内容；钉住，不随预算被裁掉。
	if found.Trust != core.TrustExternal || !found.Pinned {
		t.Fatalf("这一段应是钉住的外部内容: %+v", found)
	}
	for _, want := range []string{"故宫 门票 价格", "旺季 60 元", "慕田峪 交通", "916 路转 h23", "不要再查"} {
		if !strings.Contains(found.Content, want) {
			t.Errorf("应包含 %q:\n%s", want, found.Content)
		}
	}
	// 只留结论的开头；失败的、不是搜索的、还在窗口里的最近几步都不重复列。
	if strings.Contains(found.Content, long) || strings.Contains(found.Content, "失败的那次") ||
		strings.Contains(found.Content, "最近一步") || strings.Count(found.Content, "步骤 ") != 2 {
		t.Fatalf("这一段的内容不对:\n%s", found.Content)
	}

	// 没有较早的搜索就没有这一段。
	for _, seg := range p.segments(task, history[4:]) {
		if seg.Source == "较早的联网搜索结果" {
			t.Fatal("没有较早的搜索时不该有这一段")
		}
	}
}
