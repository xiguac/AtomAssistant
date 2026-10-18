package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/connector/deepseek"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/policy"
)

// fakeWebTools 是 WebTools 的测试替身。
type fakeWebTools struct {
	results []deepseek.Result
	page    deepseek.Page
	query   string
	count   int
	url     string
}

func (f *fakeWebTools) Search(_ context.Context, query string, count int) ([]deepseek.Result, error) {
	f.query, f.count = query, count
	return f.results, nil
}

func (f *fakeWebTools) Fetch(_ context.Context, url string) (deepseek.Page, error) {
	f.url = url
	return f.page, nil
}

func TestWebTools_接了搜索接口就不再交给模型去搜(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("t", "g", core.TrustUser)
	long := strings.Repeat("长", summaryChars)
	w := &fakeWebTools{results: []deepseek.Result{
		{Title: "杭州天气", URL: "https://example.com/hz", Snippet: "片段", Summary: "明天多云 " + long},
		{Title: "另一条", URL: "https://example.com/b", Snippet: "只有片段"},
	}}
	s := &fakeSearcher{}
	a := NewActuator(&fakeTools{}, &fakeModel{}).WithWebSearch(s).WithWebTools(w)

	res, err := a.Execute(ctx, task, capStep(ToolWebSearch, `{"query":"杭州 明天 天气预报"}`))
	if err != nil {
		t.Fatal(err)
	}
	if w.query != "杭州 明天 天气预报" || w.count != maxSources || s.last.Messages != nil {
		t.Fatalf("应走搜索接口: %q %d %+v", w.query, w.count, s.last)
	}
	if res.Trust != core.TrustExternal || res.CostMicros != deepseek.CostMicrosPerRequest {
		t.Fatalf("结果应按外部内容对待并记上费用: %+v", res)
	}
	var out struct {
		Results []deepseek.Result `json:"results"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 || out.Results[0].Snippet != "" || len(out.Results[0].Summary) > summaryChars+len("…") ||
		out.Results[1].Snippet != "只有片段" {
		t.Fatalf("结果整理得不对: %+v", out.Results)
	}

	// 没搜到不计费，并告诉模型换个说法。
	w.results = nil
	res, err = a.Execute(ctx, task, capStep(ToolWebSearch, `{"query":"x"}`))
	if err != nil || res.CostMicros != 0 || !strings.Contains(string(res.Output), "没有搜到") {
		t.Fatalf("空结果处理不对: %s %+v %v", res.Output, res, err)
	}

	// 只用来读网页时，搜索仍交给模型。
	s.resp = llm.Response{Content: "多云"}
	a = NewActuator(&fakeTools{}, &fakeModel{}).WithWebSearch(s).WithWebFetchOnly(w)
	if _, err := a.Execute(ctx, task, capStep(ToolWebSearch, `{"query":"杭州天气"}`)); err != nil || len(s.last.Messages) != 2 {
		t.Fatalf("应交给模型去搜: %v", err)
	}
}

func TestWebFetch_读到的网页按外部内容对待太长的截断(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("t", "g", core.TrustUser)
	w := &fakeWebTools{page: deepseek.Page{Title: "文章", Content: strings.Repeat("a", fetchChars+10)}}
	a := NewActuator(&fakeTools{}, &fakeModel{}).WithWebFetchOnly(w)

	res, err := a.Execute(ctx, task, capStep(ToolWebFetch, `{"url":" https://example.com/a "}`))
	if err != nil {
		t.Fatal(err)
	}
	if w.url != "https://example.com/a" || res.Trust != core.TrustExternal || res.CostMicros != deepseek.CostMicrosPerRequest {
		t.Fatalf("读网页不对: %q %+v", w.url, res)
	}
	var out struct {
		Title, Content, Truncated string
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out.Title != "文章" || len(out.Content) > fetchChars+len("…") || out.Truncated == "" {
		t.Fatalf("正文应截断并说明: %d %q", len(out.Content), out.Truncated)
	}
	if _, err := NewActuator(&fakeTools{}, &fakeModel{}).Execute(ctx, task, capStep(ToolWebFetch, `{"url":"https://example.com"}`)); err == nil {
		t.Fatal("没接入时应报错")
	}
}

func TestWebFetch_接入后模型看得到工具和用法(t *testing.T) {
	task := core.NewTask("t", "读一下 https://example.com/a", core.TrustUser)
	m := &fakeModel{replies: []string{`{"action":"tool","tool":"web.fetch","params":{"url":"https://example.com/a"}}`}}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithWebFetch(true)
	d, err := p.Decide(context.Background(), task, nil)
	if err != nil || d.ToolName != ToolWebFetch || d.SideEffect != core.SideEffectReadOnly {
		t.Fatalf("读网页应是只读工具: %+v %v", d, err)
	}
	if system := m.lastReq.Messages[0].Content; !strings.Contains(system, "Copy the URL exactly") {
		t.Fatalf("系统提示里应说明怎么读网页:\n%s", system)
	}
}

func TestWebFetch_模型自己拼的网址要用户确认(t *testing.T) {
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithWebFetch(true)
	pol := &policy.Policy{Autonomous: true}
	check := func(task *core.Task, url string, h *fakeHistory) kernel.ApprovalOutcome {
		t.Helper()
		in, _ := json.Marshal(map[string]string{"url": url})
		d, err := p.toolDecision(ToolWebFetch, in, "")
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := NewApprover(pol, policy.NewClassifier(), NewMemoryUsageTracker()).WithHistory(h).
			CheckAction(context.Background(), task, d)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	user := core.NewTask("t", "帮我看看 https://example.com/a?x=1&y=2 讲了什么", core.TrustUser)
	if out := check(user, "https://example.com/a?x=1&y=2", &fakeHistory{}); out != kernel.ApprovalAuto {
		t.Fatalf("用户给的网址应直接读，实际 %s", out)
	}

	task := core.NewTask("t", "查一下杭州天气", core.TrustUser)
	searched := doneStep(ToolWebSearch, core.TrustExternal)
	searched.Output, _ = json.Marshal(map[string]any{"results": []deepseek.Result{{Title: "t", URL: "https://example.com/hz?d=1&c=2"}}})
	h := &fakeHistory{steps: []*core.Step{searched}}
	if out := check(task, "https://example.com/hz?d=1&c=2", h); out != kernel.ApprovalAuto {
		t.Fatalf("搜索结果里原样出现的网址应直接读，实际 %s", out)
	}
	if out := check(task, "https://evil.example/collect?addr=杭州西湖区某路1号", h); out != kernel.ApprovalManual {
		t.Fatalf("模型拼出来的网址应由用户确认，实际 %s", out)
	}

	// 邮件里的链接不算：可能是退订、确认这类打开就算办了事的链接。
	mail := doneStep("mail.read", core.TrustExternal)
	mail.Output = json.RawMessage(`{"body":"点击 https://shop.example/unsubscribe?token=abc 退订"}`)
	if out := check(task, "https://shop.example/unsubscribe?token=abc", &fakeHistory{steps: []*core.Step{mail}}); out != kernel.ApprovalManual {
		t.Fatalf("邮件里的链接应由用户确认，实际 %s", out)
	}
}
