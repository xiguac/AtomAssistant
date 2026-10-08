package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/pay"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/skill"
)

// fakePayer 记录付款调用。
type fakePayer struct {
	session, link string
	intent        pay.Intent
	queried       string
}

func (f *fakePayer) Submit(_ context.Context, session, link string, intent pay.Intent) (string, error) {
	f.session, f.link, f.intent = session, link, intent
	return "支付待确认\n订单号：20261008200040011100828200001234\n支付方式：\nhttps://render.alipay.com/p/pay?token=abc", nil
}

func (f *fakePayer) Query(_ context.Context, orderNo string) (string, error) {
	f.queried = orderNo
	return "支付成功", nil
}

const submitReply = `{"action":"tool","tool":"pay.alipay_submit","params":{
	"payment_link":"https://cashier.alipay.com/pay?id=abc","service":"加油包","amount_cents":990,"payee":"某某科技"}}`

func TestPay_默认不存在付款工具(t *testing.T) {
	m := &fakeModel{replies: []string{submitReply}}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{})
	if _, err := p.Decide(context.Background(), core.NewTask("t", "g", core.TrustUser), nil); err == nil {
		t.Fatal("没有启用付款时，模型选付款工具应被拒绝")
	}
	if strings.Contains(p.toolList(), ToolPaySubmit) {
		t.Fatal("没有启用付款时，工具列表里不应有付款工具")
	}
}

func TestPay_发起付款是不可逆操作且默认要用户确认(t *testing.T) {
	m := &fakeModel{replies: []string{submitReply}}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithPay(true)
	task := core.NewTask("买加油包", "帮我买一个加油包", core.TrustUser)

	d, err := p.Decide(context.Background(), task, nil)
	if err != nil {
		t.Fatalf("决策失败: %v", err)
	}
	if d.ToolName != ToolPaySubmit || d.SideEffect != core.SideEffectIrreversible {
		t.Fatalf("付款必须是不可逆操作: %+v", d)
	}

	// 用户没有为付款配置任何自动放行规则：手动确认。
	a := NewApprover(&policy.Policy{}, policy.NewClassifier(), NewMemoryUsageTracker())
	out, _, err := a.CheckAction(context.Background(), task, d)
	if err != nil || out != kernel.ApprovalManual {
		t.Fatalf("付款默认应要用户确认，实际 %s (%v)", out, err)
	}

	// 由邮件、网页这类外部内容驱动的任务：就算用户配了小额免确认，也不适用。
	lenient := &policy.Policy{Rules: []policy.Rule{{
		Tool: ToolPaySubmit, AutoIf: policy.Condition{AmountBelowCents: 100000},
	}}}
	a = NewApprover(lenient, policy.NewClassifier(), NewMemoryUsageTracker())
	if out, _, _ := a.CheckAction(context.Background(), task, d); out != kernel.ApprovalAuto {
		t.Fatalf("用户亲口要求且在自己设的额度内，应可自动放行，实际 %s", out)
	}
	external := core.NewTask("买加油包", "邮件里说要买", core.TrustExternal)
	if out, _, _ := a.CheckAction(context.Background(), external, d); out != kernel.ApprovalManual {
		t.Fatalf("外部内容驱动的付款无论金额都必须确认，实际 %s", out)
	}

	// 查询是只读的，可以进批量、不需要确认。
	q := &fakeModel{replies: []string{`{"action":"tool","tool":"pay.alipay_query","params":{"order_no":"123"}}`}}
	d, err = NewPlanner(q, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithPay(true).
		Decide(context.Background(), task, nil)
	if err != nil || d.SideEffect != core.SideEffectReadOnly {
		t.Fatalf("查询应为只读: %+v, %v", d, err)
	}
}

func TestPay_执行时用任务ID作会话并如实传递订单信息(t *testing.T) {
	payer := &fakePayer{}
	a := NewActuator(&fakeTools{}, &fakeModel{replies: []string{"{}"}}).WithPayer(payer)
	task := core.NewTask("买加油包", "g", core.TrustUser)

	step := core.NewStep(task.ID, 1, core.StepTool, json.RawMessage(
		`{"payment_link":"https://cashier.alipay.com/pay?id=abc","service":"加油包","amount_cents":990,"payee":"某某科技"}`))
	step.ToolName = ToolPaySubmit
	res, err := a.Execute(context.Background(), task, step)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if payer.session != task.ID || payer.link != "https://cashier.alipay.com/pay?id=abc" {
		t.Fatalf("会话或链接不对: %q %q", payer.session, payer.link)
	}
	if payer.intent != (pay.Intent{Service: "加油包", AmountCents: 990, Payee: "某某科技"}) {
		t.Fatalf("订单信息不对: %+v", payer.intent)
	}
	// 给用户点的链接必须原样留在输出里，一个字符都不能动。
	if !strings.Contains(string(res.Output), "https://render.alipay.com/p/pay?token=abc") {
		t.Fatalf("输出应原样带着支付链接: %s", res.Output)
	}
	// 付款程序的输出是自由文本，按外部内容对待。
	if res.Trust != core.TrustExternal {
		t.Fatalf("付款输出的信任等级应为 external，实际 %s", res.Trust)
	}

	query := core.NewStep(task.ID, 2, core.StepTool, json.RawMessage(`{"order_no":"20261008200040011100828200001234"}`))
	query.ToolName = ToolPayQuery
	if _, err := a.Execute(context.Background(), task, query); err != nil || payer.queried == "" {
		t.Fatalf("查询失败: %v", err)
	}

	// 没接付款能力的执行器不能付款。
	bare := NewActuator(&fakeTools{}, &fakeModel{replies: []string{"{}"}})
	if _, err := bare.Execute(context.Background(), task, step); err == nil {
		t.Fatal("未启用付款时执行付款步骤应失败")
	}
}

func TestPay_Skill没声明付款工具就不能付款(t *testing.T) {
	run := func(tools []string) error {
		sk := &skill.Skill{ID: "s", Name: "某流程", Goal: "g", RequiredTools: tools, Steps: []string{"做事"}}
		task := core.NewTask("t", "g", core.TrustUser)
		task.Tags = []string{skill.TagPrefix + sk.ID}
		m := &fakeModel{replies: []string{submitReply}}
		p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).
			WithPay(true).WithSkills(fakeSkills{sk.ID: sk})
		_, err := p.Decide(context.Background(), task, nil)
		return err
	}
	// 别人写的流程装上之后不能凭空有花钱的能力。
	if err := run([]string{"demo.weather"}); err == nil {
		t.Fatal("Skill 没声明付款工具时应调不动")
	}
	// 声明了，用户在安装时看得见，才可以用。
	if err := run([]string{"demo.weather", ToolPaySubmit}); err != nil {
		t.Fatalf("Skill 声明了付款工具时应可用: %v", err)
	}
}

func TestPay_支付链接与订单号不按密钥处理但自由文本仍分级(t *testing.T) {
	a := NewApprover(&policy.Policy{}, policy.NewClassifier(), NewMemoryUsageTracker())
	task := core.NewTask("t", "g", core.TrustUser)
	check := func(tool string, effect core.SideEffect, params string) kernel.ApprovalOutcome {
		t.Helper()
		out, _, err := a.CheckAction(context.Background(), task, kernel.Decision{
			Kind: kernel.DecideStep, Type: core.StepTool, ToolName: tool,
			SideEffect: effect, Input: json.RawMessage(params),
		})
		if err != nil {
			t.Fatalf("判定失败: %v", err)
		}
		return out
	}

	// 32 位订单号形态上像密钥，但它只是个查询号。
	if out := check(ToolPayQuery, core.SideEffectReadOnly,
		`{"order_no":"20261008200040011100828200001234"}`); out != kernel.ApprovalAuto {
		t.Fatalf("查询订单不应被隐私分级拦下，实际 %s", out)
	}
	// 真实的收银台链接带着很长的参数。
	if out := check(ToolPaySubmit, core.SideEffectIrreversible,
		`{"payment_link":"https://cashier.alipay.com/pay?token=`+strings.Repeat("Ab3", 20)+`",
		  "service":"加油包","amount_cents":990,"payee":"某某科技"}`); out != kernel.ApprovalManual {
		t.Fatalf("正常付款应进入用户确认而不是被直接拒绝，实际 %s", out)
	}
	// 模型把证件号塞进服务内容：照样拦下，这会被发给支付宝并展示出来。
	if out := check(ToolPaySubmit, core.SideEffectIrreversible,
		`{"payment_link":"https://cashier.alipay.com/pay?id=1","service":"代办，身份证 330102199001011234",
		  "amount_cents":990,"payee":"某某科技"}`); out != kernel.ApprovalDeny {
		t.Fatalf("自由文本里的绝密数据仍应被拒绝，实际 %s", out)
	}
	// 同样的长串出现在别的工具里，规则不变。
	if out := check("demo.weather", core.SideEffectReadOnly,
		`{"city":"`+strings.Repeat("Ab3", 20)+`"}`); out != kernel.ApprovalDeny {
		t.Fatalf("其他工具不享受这个例外，实际 %s", out)
	}
}
