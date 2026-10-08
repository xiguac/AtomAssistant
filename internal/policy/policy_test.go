package policy

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
)

func TestClassifyField_字段名优先于值(t *testing.T) {
	c := NewClassifier()
	// 值看着人畜无害，但字段名是 password。
	if got := c.ClassifyField("password", "123"); got != core.PrivacySecret {
		t.Fatalf("password 字段应为 L3，实际 %s", got)
	}
	if got := c.ClassifyField("api_key", "x"); got != core.PrivacySecret {
		t.Fatalf("api_key 字段应为 L3，实际 %s", got)
	}
	if got := c.ClassifyField("验证码", "1"); got != core.PrivacySecret {
		t.Fatalf("中文字段名也要识别，实际 %s", got)
	}
}

func TestClassifyField_值形态识别(t *testing.T) {
	c := NewClassifier()
	cases := []struct {
		name  string
		field string
		value any
		want  core.PrivacyLevel
	}{
		{"身份证", "note", "310101199003075432", core.PrivacySecret},
		{"银行卡", "note", "4111111111111111", core.PrivacySecret},
		{"健康信息", "备注", "客户海鲜过敏", core.PrivacySecret},
		{"手机号", "note", "13800138000", core.PrivacySensitive},
		{"普通文本", "note", "明天上午开会", core.PrivacyNormal},
		{"城市名", "city", "上海", core.PrivacyNormal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.ClassifyField(tc.field, tc.value); got != tc.want {
				t.Fatalf("期望 %s，实际 %s", tc.want, got)
			}
		})
	}
}

func TestClassifyField_银行卡走Luhn校验(t *testing.T) {
	c := NewClassifier()
	// 位数对但 Luhn 校验不过，不应误判为银行卡。
	if got := c.ClassifyField("order_no", "4111111111111112"); got == core.PrivacySecret {
		t.Fatal("Luhn 校验不过的数字串不应被判为银行卡")
	}
}

func TestClassifyJSON_递归并取最高等级(t *testing.T) {
	c := NewClassifier()
	raw := json.RawMessage(`{
		"city": "上海",
		"user": {"phone": "13800138000", "name": "张总"},
		"items": [{"note": "客户海鲜过敏"}]
	}`)
	fields, max, err := c.ClassifyJSON(raw)
	if err != nil {
		t.Fatalf("分级失败: %v", err)
	}
	if max != core.PrivacySecret {
		t.Fatalf("整体最高等级应为 L3，实际 %s", max)
	}
	var found bool
	for _, f := range fields {
		if f.Path == "items[0].note" && f.Level == core.PrivacySecret {
			found = true
		}
	}
	if !found {
		t.Fatalf("嵌套数组里的健康信息未被识别: %+v", fields)
	}
}

func TestClassifyJSON_非法JSON按最高等级处理(t *testing.T) {
	c := NewClassifier()
	_, max, err := c.ClassifyJSON(json.RawMessage(`{不是JSON`))
	if err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	// 解析不了就不能放行，必须按最保守处理。
	if max != core.PrivacySecret {
		t.Fatalf("无法分级时应按 L3 处理，实际 %s", max)
	}
}

func newGuard(t *testing.T, grants ...ToolGrant) *Guard {
	t.Helper()
	r := NewRegistry()
	for _, g := range grants {
		if err := r.Register(g); err != nil {
			t.Fatalf("注册工具失败: %v", err)
		}
	}
	return NewGuard(NewClassifier(), r)
}

func TestCheckOutbound_绝密数据永远拦截(t *testing.T) {
	g := newGuard(t, ToolGrant{
		Name: "mail.send", MaxPrivacy: core.PrivacySensitive,
		SideEffect: core.SideEffectIrreversible,
	})
	_, violations, err := g.CheckOutbound("mail.send",
		json.RawMessage(`{"body":"客户海鲜过敏，改订川菜"}`))
	if !errors.Is(err, ErrSecretBlocked) {
		t.Fatalf("期望 ErrSecretBlocked，实际 %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("应返回被拦下的字段")
	}
}

func TestRegister_授权上限被强制压到L2(t *testing.T) {
	r := NewRegistry()
	err := r.Register(ToolGrant{
		Name: "evil", MaxPrivacy: core.PrivacySecret, SideEffect: core.SideEffectReadOnly,
	})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	g, _ := r.Grant("evil")
	// 即使调用方试图授予 L3，也必须被压回 L2：没有任何工具能拿到绝密数据。
	if g.MaxPrivacy != core.PrivacySensitive {
		t.Fatalf("L3 授权应被压到 L2，实际 %s", g.MaxPrivacy)
	}
}

func TestCheckOutbound_越权字段被拒(t *testing.T) {
	g := newGuard(t, ToolGrant{
		Name: "weather.query", MaxPrivacy: core.PrivacyNormal,
		SideEffect: core.SideEffectReadOnly,
	})
	_, _, err := g.CheckOutbound("weather.query", json.RawMessage(`{"phone":"13800138000"}`))
	if !errors.Is(err, ErrPrivacyExceeded) {
		t.Fatalf("期望 ErrPrivacyExceeded，实际 %v", err)
	}
}

func TestCheckOutbound_最小化裁剪(t *testing.T) {
	g := newGuard(t, ToolGrant{
		Name: "weather.query", MaxPrivacy: core.PrivacyNormal,
		SideEffect:    core.SideEffectReadOnly,
		AllowedFields: []string{"city"},
	})
	// 查天气只需要城市名，不该把整个日程对象传出去。
	out, _, err := g.CheckOutbound("weather.query",
		json.RawMessage(`{"city":"上海","meeting_title":"与张总谈续约"}`))
	if err != nil {
		t.Fatalf("检查失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("解析裁剪结果失败: %v", err)
	}
	if _, ok := got["meeting_title"]; ok {
		t.Fatalf("未声明的字段应被裁剪掉: %v", got)
	}
	if got["city"] != "上海" {
		t.Fatalf("声明的字段应保留: %v", got)
	}
}

func TestCheckOutbound_未注册工具被拒(t *testing.T) {
	g := newGuard(t)
	if _, _, err := g.CheckOutbound("unknown.tool", json.RawMessage(`{}`)); !errors.Is(err, ErrToolUnknown) {
		t.Fatalf("期望 ErrToolUnknown，实际 %v", err)
	}
}

func TestCheckEgressTarget_白名单与内网防护(t *testing.T) {
	g := newGuard(t, ToolGrant{
		Name: "flight.query", MaxPrivacy: core.PrivacyNormal,
		SideEffect:    core.SideEffectReadOnly,
		EgressDomains: []string{"api.example.com", "*.cdn.example.net"},
	})
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"白名单精确匹配", "https://api.example.com/v1/flights", true},
		{"通配子域", "https://img.cdn.example.net/a.png", true},
		{"通配不匹配裸域", "https://cdn.example.net/a.png", false},
		{"不在白名单", "https://evil.com/collect", false},
		{"回环地址", "http://127.0.0.1:8080/", false},
		{"内网地址", "http://192.168.1.10/", false},
		{"云元数据", "http://169.254.169.254/latest/meta-data/", false},
		{"localhost", "http://localhost/", false},
		{"非法协议", "file:///etc/passwd", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := g.CheckEgressTarget("flight.query", tc.url)
			if tc.ok && err != nil {
				t.Fatalf("应放行，实际 %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("应拦截，实际放行")
			}
		})
	}
}

func TestRegister_过宽的通配域名被拒(t *testing.T) {
	r := NewRegistry()
	for _, d := range []string{"*", "*.com", "https://a.com", "a.com:443", "a.*.com"} {
		err := r.Register(ToolGrant{
			Name: "t", MaxPrivacy: core.PrivacyNormal,
			SideEffect: core.SideEffectReadOnly, EgressDomains: []string{d},
		})
		if err == nil {
			t.Fatalf("域名 %q 应被拒绝", d)
		}
	}
}

// ---- 审批策略 ----

func basePolicy() *Policy {
	return &Policy{
		Rules: []Rule{
			{Tool: "ride.book", AutoIf: Condition{AmountBelowCents: 10000, Domain: core.DomainPersonal}},
			{Tool: "calendar.write", AutoIf: Condition{LeadTimeAbove: 24 * time.Hour}},
			{Tool: "mail.send", Never: true},
		},
		Caps: Caps{DailyAutoAmountCents: 50000, DailyAutoCount: 20, PerTaskAmountCents: 100000},
	}
}

func TestDecide_只读操作直接放行(t *testing.T) {
	d := basePolicy().Decide(Request{
		Tool: "weather.query", SideEffect: core.SideEffectReadOnly, Origin: core.TrustUser,
	}, Usage{}, time.Now())
	if d.Outcome != OutcomeAuto {
		t.Fatalf("只读应自动放行，实际 %s: %s", d.Outcome, d.Reason)
	}
}

func TestDecide_绝密数据直接拒绝而不是进审批(t *testing.T) {
	d := basePolicy().Decide(Request{
		Tool: "ride.book", SideEffect: core.SideEffectIrreversible,
		Origin: core.TrustUser, MaxPrivacy: core.PrivacySecret,
	}, Usage{}, time.Now())
	// 给了审批机会，用户点一下确认就泄露了。必须连机会都不给。
	if d.Outcome != OutcomeDeny {
		t.Fatalf("含 L3 数据应直接拒绝，实际 %s: %s", d.Outcome, d.Reason)
	}
}

func TestDecide_untrusted来源不适用任何自动放行(t *testing.T) {
	// 金额只有 1 分，完全满足阈值，但来源是截图。
	d := basePolicy().Decide(Request{
		Tool: "ride.book", SideEffect: core.SideEffectIrreversible,
		AmountCents: 1, Origin: core.TrustExternal, Domain: core.DomainPersonal,
	}, Usage{}, time.Now())
	if d.Outcome != OutcomeManual {
		t.Fatalf("untrusted 来源必须手动确认，实际 %s: %s", d.Outcome, d.Reason)
	}
}

func TestDecide_满足条件时自动放行(t *testing.T) {
	d := basePolicy().Decide(Request{
		Tool: "ride.book", SideEffect: core.SideEffectIrreversible,
		AmountCents: 3000, Origin: core.TrustUser, Domain: core.DomainPersonal,
	}, Usage{}, time.Now())
	if d.Outcome != OutcomeAuto {
		t.Fatalf("应自动放行，实际 %s: %s", d.Outcome, d.Reason)
	}
}

func TestDecide_条件不满足时转手动(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		req  Request
	}{
		{"金额超阈值", Request{Tool: "ride.book", SideEffect: core.SideEffectIrreversible,
			AmountCents: 20000, Origin: core.TrustUser, Domain: core.DomainPersonal}},
		{"域不匹配", Request{Tool: "ride.book", SideEffect: core.SideEffectIrreversible,
			AmountCents: 3000, Origin: core.TrustUser, Domain: core.DomainWork}},
		{"未配置规则的工具", Request{Tool: "unknown.tool", SideEffect: core.SideEffectReversible,
			Origin: core.TrustUser}},
		{"标记为始终手动", Request{Tool: "mail.send", SideEffect: core.SideEffectIrreversible,
			Origin: core.TrustUser}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := basePolicy().Decide(tc.req, Usage{}, now)
			if d.Outcome != OutcomeManual {
				t.Fatalf("应转手动，实际 %s: %s", d.Outcome, d.Reason)
			}
			if d.Reason == "" {
				t.Fatal("必须给出可展示给用户的理由")
			}
		})
	}
}

func TestDecide_提前量条件(t *testing.T) {
	now := time.Now()
	far := now.Add(48 * time.Hour)
	near := now.Add(2 * time.Hour)

	if d := basePolicy().Decide(Request{
		Tool: "calendar.write", SideEffect: core.SideEffectReversible,
		Origin: core.TrustUser, EventAt: &far,
	}, Usage{}, now); d.Outcome != OutcomeAuto {
		t.Fatalf("提前量充足应放行，实际 %s: %s", d.Outcome, d.Reason)
	}

	if d := basePolicy().Decide(Request{
		Tool: "calendar.write", SideEffect: core.SideEffectReversible,
		Origin: core.TrustUser, EventAt: &near,
	}, Usage{}, now); d.Outcome != OutcomeManual {
		t.Fatalf("提前量不足应转手动，实际 %s", d.Outcome)
	}

	// 缺少事件时间时不能默认放行。
	if d := basePolicy().Decide(Request{
		Tool: "calendar.write", SideEffect: core.SideEffectReversible,
		Origin: core.TrustUser,
	}, Usage{}, now); d.Outcome != OutcomeManual {
		t.Fatalf("缺少事件时间应转手动，实际 %s", d.Outcome)
	}
}

func TestDecide_累计帽子挡住拆单(t *testing.T) {
	now := time.Now()
	p := basePolicy()
	req := Request{
		Tool: "ride.book", SideEffect: core.SideEffectIrreversible,
		AmountCents: 3000, Origin: core.TrustUser, Domain: core.DomainPersonal,
	}

	// 单次金额始终满足阈值，但今天已经自动跑了 20 次。
	if d := p.Decide(req, Usage{DailyAutoCount: 20}, now); d.Outcome != OutcomeManual {
		t.Fatalf("次数帽子应生效，实际 %s: %s", d.Outcome, d.Reason)
	}
	// 今天累计金额已接近上限。
	if d := p.Decide(req, Usage{DailyAutoAmountCents: 49000}, now); d.Outcome != OutcomeManual {
		t.Fatalf("金额帽子应生效，实际 %s: %s", d.Outcome, d.Reason)
	}
	// 单任务金额帽子。
	if d := p.Decide(req, Usage{TaskAmountCents: 99000}, now); d.Outcome != OutcomeManual {
		t.Fatalf("单任务帽子应生效，实际 %s: %s", d.Outcome, d.Reason)
	}
}

func TestDecide_默认策略一律手动(t *testing.T) {
	// 空策略 = 用户什么都没配 = 全部手动确认。
	empty := &Policy{}
	d := empty.Decide(Request{
		Tool: "ride.book", SideEffect: core.SideEffectIrreversible, Origin: core.TrustUser,
	}, Usage{}, time.Now())
	if d.Outcome != OutcomeManual {
		t.Fatalf("默认应手动确认，实际 %s", d.Outcome)
	}
}
