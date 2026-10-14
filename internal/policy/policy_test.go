package policy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

func TestCheckOutbound_绝密数据未经批准一律拦截(t *testing.T) {
	g := newGuard(t, ToolGrant{
		Name: "mail.send", MaxPrivacy: core.PrivacySensitive,
		SideEffect: core.SideEffectIrreversible,
	})
	params := json.RawMessage(`{"body":"客户海鲜过敏，改订川菜"}`)
	_, violations, err := g.CheckOutbound("mail.send", params, false)
	if !errors.Is(err, ErrSecretBlocked) {
		t.Fatalf("期望 ErrSecretBlocked，实际 %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("应返回被拦下的字段")
	}
	// 用户批准过这一步就放行。
	if _, _, err := g.CheckOutbound("mail.send", params, true); err != nil {
		t.Fatalf("用户批准后应放行: %v", err)
	}
	// 批准只管绝密数据，超出工具授权上限的照常拦。
	narrow := newGuard(t, ToolGrant{Name: "weather.query", MaxPrivacy: core.PrivacyNormal, SideEffect: core.SideEffectReadOnly})
	if _, _, err := narrow.CheckOutbound("weather.query", json.RawMessage(`{"phone":"13800138000"}`), true); !errors.Is(err, ErrPrivacyExceeded) {
		t.Fatalf("批准不应放宽工具授权上限: %v", err)
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
	_, _, err := g.CheckOutbound("weather.query", json.RawMessage(`{"phone":"13800138000"}`), false)
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
		json.RawMessage(`{"city":"上海","meeting_title":"与张总谈续约"}`), false)
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
	if _, _, err := g.CheckOutbound("unknown.tool", json.RawMessage(`{}`), false);!errors.Is(err, ErrToolUnknown) {
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

func TestDecide_绝密数据由用户确认并说明是哪几项(t *testing.T) {
	// 放行规则写得再宽也不自动放行。
	p := &Policy{Rules: []Rule{{Tool: "ride.book"}, {Tool: "weather.query"}}, Autonomous: true}
	d := p.Decide(Request{
		Tool: "ride.book", SideEffect: core.SideEffectIrreversible,
		Origin: core.TrustUser, MaxPrivacy: core.PrivacySecret,
		Secrets: []string{"passenger.id_no（身份证号）", "password（密码或密钥）"},
	}, Usage{}, time.Now())
	if d.Outcome != OutcomeManual || !strings.Contains(d.Reason, "passenger.id_no（身份证号）、password（密码或密钥）") {
		t.Fatalf("含 L3 数据应请用户确认并列出是哪几项，实际 %s: %s", d.Outcome, d.Reason)
	}
	// 只读操作也一样：查询条件同样会交给外部。
	d = p.Decide(Request{
		Tool: "weather.query", SideEffect: core.SideEffectReadOnly,
		Origin: core.TrustUser, MaxPrivacy: core.PrivacySecret,
	}, Usage{}, time.Now())
	if d.Outcome != OutcomeManual {
		t.Fatalf("含 L3 数据的只读操作也应确认，实际 %s: %s", d.Outcome, d.Reason)
	}
}

func TestSecretItems_只列字段和类别不含值(t *testing.T) {
	fields, _, err := NewClassifier().ClassifyJSON(json.RawMessage(
		`{"city":"杭州","passenger":{"id_no":"110101199003077715"},"note":"对青霉素过敏"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(SecretItems(fields), "、")
	for _, want := range []string{"passenger.id_no（身份证号）", "note（健康信息）"} {
		if !strings.Contains(got, want) {
			t.Fatalf("应列出 %s，实际 %s", want, got)
		}
	}
	if strings.Contains(got, "110101") || strings.Contains(got, "青霉素") || strings.Contains(got, "city") {
		t.Fatalf("不应带出值或非绝密字段: %s", got)
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

func TestPolicy_自主模式放开内部的事对外发送仍由用户确认(t *testing.T) {
	now := time.Now()
	req := Request{Tool: "calendar.delete", SideEffect: core.SideEffectIrreversible, Origin: core.TrustUser, MaxPrivacy: core.PrivacyNormal}
	p := &Policy{Autonomous: true}
	if d := p.Decide(req, Usage{}, now); d.Outcome != OutcomeAuto {
		t.Fatalf("自主模式下内部的事应直接执行: %+v", d)
	}
	// 对外发送的由用户确认；含敏感信息的在理由里写明。
	out := req
	out.Tool, out.Outbound = "mail.send", true
	if d := p.Decide(out, Usage{}, now); d.Outcome != OutcomeManual || d.Reason != ReasonOutbound {
		t.Fatalf("对外发送应由用户确认: %+v", d)
	}
	out.MaxPrivacy = core.PrivacySensitive
	if d := p.Decide(out, Usage{}, now); d.Outcome != OutcomeManual || d.Reason == ReasonOutbound {
		t.Fatalf("含敏感信息时理由应写明: %+v", d)
	}
	// 绝密数据照旧由用户确认。
	secret := req
	secret.MaxPrivacy = core.PrivacySecret
	if d := p.Decide(secret, Usage{}, now); d.Outcome != OutcomeManual {
		t.Fatalf("绝密数据不受模式影响: %+v", d)
	}
	// 花钱必须有上限罩着：没设上限仍然确认，设了就放行，超了又转手动。
	pay := req
	pay.AmountCents = 5000
	if d := p.Decide(pay, Usage{}, now); d.Outcome != OutcomeManual || d.Reason != ReasonNoCap {
		t.Fatalf("没有金额上限时花钱应确认: %+v", d)
	}
	capped := &Policy{Autonomous: true, Caps: Caps{PerTaskAmountCents: 10000}}
	if d := capped.Decide(pay, Usage{}, now); d.Outcome != OutcomeAuto {
		t.Fatalf("在上限之内应放行: %+v", d)
	}
	if d := capped.Decide(pay, Usage{TaskAmountCents: 8000}, now); d.Outcome != OutcomeManual {
		t.Fatalf("超出单任务上限应确认: %+v", d)
	}
	// 用户写的规则仍然算数：never 始终确认，带条件的按条件，写了放行的对外工具直接执行。
	ruled := &Policy{Autonomous: true, Rules: []Rule{
		{Tool: "calendar.delete", Never: true},
		{Tool: "ride.book", AutoIf: Condition{AmountBelowCents: 1000}},
		{Tool: "mail.send"},
	}}
	if d := ruled.Decide(req, Usage{}, now); d.Outcome != OutcomeManual {
		t.Fatalf("never 应始终确认: %+v", d)
	}
	ride := Request{Tool: "ride.book", SideEffect: core.SideEffectIrreversible, Origin: core.TrustUser, AmountCents: 5000, Outbound: true}
	if d := ruled.Decide(ride, Usage{}, now); d.Outcome != OutcomeManual {
		t.Fatalf("不满足用户写的条件应确认: %+v", d)
	}
	out.MaxPrivacy = core.PrivacyNormal
	if d := ruled.Decide(out, Usage{}, now); d.Outcome != OutcomeAuto {
		t.Fatalf("用户写了放行规则的对外工具应直接执行: %+v", d)
	}
}

func TestScanText_编码之后的敏感信息也认得出(t *testing.T) {
	c := NewClassifier()
	phone, id := "13800138000", "11010119900307663X"
	cases := map[string]core.PrivacyLevel{
		"q=weather+in+shanghai&page=2":                   core.PrivacyPublic,
		"order=20261013A8F3C2D1E5B7A9C4D6E8F0A1B2C3D4E5": core.PrivacyPublic, // 长编号不算
		"tel=" + phone:                           core.PrivacySensitive,
		"t=138-0013-8000":                        core.PrivacySensitive, // 夹了分隔符
		"u=" + url.QueryEscape("me@example.com"): core.PrivacySensitive,
		"d=" + url.QueryEscape(url.QueryEscape("tel:"+phone)):         core.PrivacySensitive, // 套了两层网址编码
		"d=" + base64.StdEncoding.EncodeToString([]byte("手机 "+phone)): core.PrivacySensitive,
		"d=" + base64.RawURLEncoding.EncodeToString([]byte(id)):       core.PrivacySecret,
		"id=" + id: core.PrivacySecret,
		"note=" + url.QueryEscape("客户对海鲜过敏"): core.PrivacySecret,
	}
	for text, want := range cases {
		if got := c.ScanText(text); got.Level != want {
			t.Errorf("%q 应为 %s，实际 %s（%s）", text, want, got.Level, got.What)
		}
	}
}

func TestEgressFilter_带着个人信息的请求发不出去(t *testing.T) {
	var hits int
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits++
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
	}))
	defer srv.Close()
	client := &http.Client{Transport: NewEgressFilter(nil, core.PrivacyNormal, "测试")}

	// 干净的请求照常发出，请求体原样到达。
	resp, err := client.Post(srv.URL+"/track?campaign=autumn", "application/json", strings.NewReader(`{"q":"天气"}`))
	if err != nil || hits != 1 || lastBody != `{"q":"天气"}` {
		t.Fatalf("干净的请求应照常发出: %v %d %q", err, hits, lastBody)
	}
	resp.Body.Close()

	// 钓鱼邮件让助手"点一下这个链接"：个人信息拼在参数里、藏在编码里、放在请求体里，都发不出去。
	blocked := []func() (*http.Response, error){
		func() (*http.Response, error) { return client.Get(srv.URL + "/c?tel=13800138000") },
		func() (*http.Response, error) {
			return client.Get(srv.URL + "/c?d=" + base64.URLEncoding.EncodeToString([]byte("id 11010119900307663X")))
		},
		func() (*http.Response, error) { return client.Get(srv.URL + "/u/me@example.com/profile") },
		func() (*http.Response, error) {
			return client.Post(srv.URL+"/c", "application/json", strings.NewReader(`{"contact":"13800138000"}`))
		},
	}
	for i, do := range blocked {
		resp, err := do()
		if err == nil {
			resp.Body.Close()
		}
		if !errors.Is(err, ErrLeakBlocked) || hits != 1 {
			t.Fatalf("第 %d 个请求应被拦下且不触达服务器: %v（已到达 %d 次）", i+1, err, hits)
		}
		// 报错里说的是哪一类信息，不带它的值。
		var uerr *url.Error
		if !errors.As(err, &uerr) || strings.Contains(uerr.Err.Error(), "13800138000") || !strings.Contains(uerr.Err.Error(), "测试") {
			t.Fatalf("过滤给出的理由应说明是哪一类信息、不带它的值: %v", err)
		}
	}
}
