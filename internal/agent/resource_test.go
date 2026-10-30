package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/kernel"
	"github.com/xiguac/atomassistant/internal/pay"
	"github.com/xiguac/atomassistant/internal/policy"
)

// fakeResources 是 PaidResources 的测试替身，只允许 api.example.com。
type fakeResources struct {
	requested []string
	body      string
}

func (f *fakeResources) Check(u string) (string, error) {
	if !strings.HasPrefix(u, "https://api.example.com/") {
		return "", pay.ErrDomainNotAllowed
	}
	return u, nil
}

func (f *fakeResources) Request(_ context.Context, _, u, body string) (pay.Response, error) {
	f.requested = append(f.requested, u)
	f.body = body
	return pay.Response{Status: 402, Body: "本次需要支付 0.01 CNY", NeedsPayment: true}, nil
}

func (f *fakeResources) Challenge(taskID, _ string) (string, string, error) {
	return "C:/tmp/" + taskID + ".txt", f.body, nil
}

func TestResource_未列出域名时不存在付费资源工具(t *testing.T) {
	// 只开了付款、没列域名：助手仍然不能主动请求任何网址。
	p := NewPlanner(&fakeModel{replies: []string{"{}"}}, catalogWith(core.SideEffectReadOnly),
		PlannerOptions{}).WithPay(true)
	if strings.Contains(p.toolList(), ToolResourceRequest) {
		t.Fatal("没有列出域名时不应出现付费资源工具")
	}
	// 列了域名但没开付款：同样没有。
	p = NewPlanner(&fakeModel{replies: []string{"{}"}}, catalogWith(core.SideEffectReadOnly),
		PlannerOptions{}).WithPaidResources(true)
	if strings.Contains(p.toolList(), ToolResourceRequest) {
		t.Fatal("没有开启付款时不应出现付费资源工具")
	}
	p = p.WithPay(true)
	for _, name := range []string{ToolResourceRequest, ToolResourcePay, ToolResourceFetch} {
		if !strings.Contains(p.toolList(), name) {
			t.Fatalf("两者都开启后应出现 %s", name)
		}
	}
}

func TestResource_付款金额无法核实所以永远要确认(t *testing.T) {
	task := core.NewTask("t", "g", core.TrustUser)
	d := kernel.Decision{
		Kind: kernel.DecideStep, Type: core.StepTool, ToolName: ToolResourcePay,
		SideEffect: core.SideEffectIrreversible,
		Input: json.RawMessage(`{"url":"https://api.example.com/h?token=` +
			strings.Repeat("Ab3", 20) + `","purpose":"查运势"}`),
	}
	// 用户给这个工具配了很宽的自动放行：仍然不适用，因为没有可靠的金额可比。
	lenient := &policy.Policy{Rules: []policy.Rule{{
		Tool: ToolResourcePay, AutoIf: policy.Condition{AmountBelowCents: 100000},
	}}}
	a := NewApprover(lenient, policy.NewClassifier(), NewMemoryUsageTracker())
	out, reason, err := a.CheckAction(context.Background(), task, d)
	if err != nil || out != kernel.ApprovalManual || !strings.Contains(reason, "无法由系统核实") {
		t.Fatalf("付费资源的付款必须由用户确认，实际 %s (%s, %v)", out, reason, err)
	}
	// 请求资源是只读的，不需要确认；网址里的长参数不被当成密钥。
	d.ToolName, d.SideEffect = ToolResourceRequest, core.SideEffectReadOnly
	if out, _, _ := a.CheckAction(context.Background(), task, d); out != kernel.ApprovalAuto {
		t.Fatalf("请求资源应直接放行，实际 %s", out)
	}
}

func TestResource_每一步都重新校验域名(t *testing.T) {
	payer, res := &fakePayer{}, &fakeResources{}
	a := NewActuator(&fakeTools{}, &fakeModel{replies: []string{"{}"}}).WithPayer(payer).WithPaidResources(res)
	task := core.NewTask("查运势", "g", core.TrustUser)
	run := func(tool, params string) (kernel.Result, error) {
		s := core.NewStep(task.ID, 1, core.StepTool, json.RawMessage(params))
		s.ToolName = tool
		return a.Execute(context.Background(), task, s)
	}
	const good = "https://api.example.com/h?z=libra"

	got, err := run(ToolResourceRequest, `{"url":"`+good+`"}`)
	if err != nil || !strings.Contains(string(got.Output), `"needs_payment":true`) {
		t.Fatalf("应报告需要付款: %s, %v", got.Output, err)
	}
	if got.Trust != core.TrustExternal {
		t.Fatalf("网络取回的内容应为 external，实际 %s", got.Trust)
	}

	if _, err := run(ToolResourcePay, `{"url":"`+good+`","purpose":"查天秤座今日运势"}`); err != nil {
		t.Fatalf("发起付款失败: %v", err)
	}
	// 用的是这个任务自己存下的账单，会话标识是任务 ID。
	if payer.session != task.ID || payer.queried != "C:/tmp/"+task.ID+".txt" || payer.link != good {
		t.Fatalf("付款参数不对: %+v", payer)
	}
	if _, err := run(ToolResourceFetch, `{"order_no":"20261008008282214600990000520587","url":"`+good+`"}`); err != nil {
		t.Fatalf("取回失败: %v", err)
	}

	// 模型在付款或取回时换成别的网址：三个工具都拦。
	for _, tool := range []string{ToolResourceRequest, ToolResourcePay, ToolResourceFetch} {
		if _, err := run(tool, `{"url":"https://evil.example/collect?d=secret","purpose":"x","order_no":"1"}`); err == nil {
			t.Fatalf("%s 应拒绝名单外的域名", tool)
		}
	}
	if len(res.requested) != 1 {
		t.Fatalf("名单外的网址连请求都不该发出，实际发了 %d 次", len(res.requested))
	}

	// 只接了付款、没接付费资源的执行器不能请求网址。
	bare := NewActuator(&fakeTools{}, &fakeModel{replies: []string{"{}"}}).WithPayer(payer)
	s := core.NewStep(task.ID, 1, core.StepTool, json.RawMessage(`{"url":"`+good+`"}`))
	s.ToolName = ToolResourceRequest
	if _, err := bare.Execute(context.Background(), task, s); err == nil {
		t.Fatal("未启用付费资源时应失败")
	}
}
