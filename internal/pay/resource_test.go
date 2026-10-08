package pay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestCheck_只允许用户列出的域名且必须是HTTPS(t *testing.T) {
	r := Resources{Domains: []string{"api.example.com", "paid.cn"}}
	ok := []string{
		"https://api.example.com/v1/horoscope?zodiac=libra",
		"https://sub.paid.cn/x",
		"https://PAID.CN/x",
	}
	for _, u := range ok {
		if _, err := r.Check(u); err != nil {
			t.Errorf("%s 应被允许: %v", u, err)
		}
	}
	bad := map[string]error{
		"http://api.example.com/x":               ErrInvalid, // 不是 https
		"https://evil.com/?q=api.example.com":    ErrDomainNotAllowed,
		"https://api.example.com.evil.com/x":     ErrDomainNotAllowed, // 后缀拼接
		"https://notpaid.cn/x":                   ErrDomainNotAllowed, // 不是子域名
		"https://user:pw@api.example.com/x":      ErrInvalid,
		"--resource-url=https://api.example.com": ErrInvalid,
		"ftp://api.example.com/x":                ErrInvalid,
		"":                                       ErrInvalid,
	}
	for u, want := range bad {
		if _, err := r.Check(u); !errors.Is(err, want) {
			t.Errorf("%q: 期望 %v，实际 %v", u, want, err)
		}
	}
	// 一个域名都没列：什么都不能请求。
	if _, err := (Resources{}).Check("https://api.example.com/x"); !errors.Is(err, ErrDomainNotAllowed) {
		t.Fatalf("没有允许任何域名时应全部拒绝: %v", err)
	}
}

func TestRequest_遇到402时存下账单且不把它交给调用方(t *testing.T) {
	const challenge = "eyJvcGFxdWUiOiJiaWxsIn0="
	var gotMethod, gotType, gotBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/post" {
			raw, _ := io.ReadAll(r.Body)
			gotMethod, gotType, gotBody = r.Method, r.Header.Get("Content-Type"), string(raw)
		}
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "https://evil.example/collect", http.StatusFound)
		case "/free":
			_, _ = w.Write([]byte(`{"fortune":"好"}`))
		default:
			w.Header().Set("Payment-Needed", challenge)
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte("本次需要支付 0.01 CNY"))
		}
	}))
	defer srv.Close()
	host, _ := url.Parse(srv.URL)

	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r := Resources{Domains: []string{host.Hostname()}, Dir: t.TempDir(), Client: client}
	ctx := context.Background()
	target := srv.URL + "/horoscope?zodiac=libra"

	// 还没请求过：没有账单可付。
	if _, _, err := r.Challenge("task-1", target); !errors.Is(err, ErrNoChallenge) {
		t.Fatalf("没请求过不应有账单: %v", err)
	}

	resp, err := r.Request(ctx, "task-1", target, "")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.Status != 402 || !resp.NeedsPayment || !strings.Contains(resp.Body, "0.01") {
		t.Fatalf("应报告需要付款并带上对方的说明: %+v", resp)
	}
	// 账单不出现在返回值里：它不该进模型的上下文。
	if strings.Contains(resp.Body, challenge) {
		t.Fatal("账单原文不应出现在返回内容里")
	}
	file, body, err := r.Challenge("task-1", target)
	if err != nil || body != "" {
		t.Fatalf("账单应已存好且没有请求体: %v, %q", err, body)
	}
	saved, _ := os.ReadFile(file)
	if string(saved) != challenge {
		t.Fatalf("账单必须原样保存，实际 %q", saved)
	}
	// 账单按任务隔离：别的任务拿不到这一张。
	if _, _, err := r.Challenge("task-2", target); !errors.Is(err, ErrNoChallenge) {
		t.Fatal("账单不应串到别的任务")
	}

	// 免费资源直接返回，不产生账单。
	free, err := r.Request(ctx, "task-1", srv.URL+"/free", "")
	if err != nil || free.Status != 200 || free.NeedsPayment {
		t.Fatalf("免费资源应直接返回: %+v, %v", free, err)
	}
	// 不跟随跳转：允许的是这个域名，不是它想把我们带去的地方。
	redir, err := r.Request(ctx, "task-1", srv.URL+"/redirect", "")
	if err != nil || redir.Status != http.StatusFound {
		t.Fatalf("跳转不应被跟随: %+v, %v", redir, err)
	}
	// 不在名单里的域名连请求都不发。
	if _, err := r.Request(ctx, "task-1", "https://evil.example/x", ""); !errors.Is(err, ErrDomainNotAllowed) {
		t.Fatalf("不允许的域名应被拒绝: %v", err)
	}

	// 带请求体：用 POST 以 JSON 发送，请求体和账单一起存下，供付款与取回时原样重放。
	if _, err := r.Request(ctx, "task-1", srv.URL+"/post", "{ \"zodiac\" : \"天秤座\" }"); err != nil {
		t.Fatalf("带请求体的请求失败: %v", err)
	}
	if gotMethod != http.MethodPost || gotType != "application/json" || gotBody != `{"zodiac":"天秤座"}` {
		t.Fatalf("应以 JSON 用 POST 发送: %s %s %s", gotMethod, gotType, gotBody)
	}
	if _, body, err := r.Challenge("task-1", srv.URL+"/post"); err != nil || body != `{"zodiac":"天秤座"}` {
		t.Fatalf("请求体应和账单一起存下: %q, %v", body, err)
	}
	// 同一个网址改成不带请求体再请求：旧的请求体不能留着被错误地重放。
	if _, err := r.Request(ctx, "task-1", srv.URL+"/post", ""); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if _, body, _ := r.Challenge("task-1", srv.URL+"/post"); body != "" {
		t.Fatalf("旧请求体应被清掉，实际 %q", body)
	}
	if _, err := r.Request(ctx, "task-1", srv.URL+"/post", "这不是 JSON"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("请求体必须是 JSON: %v", err)
	}
}

func TestPayResource_按官方格式组装402命令(t *testing.T) {
	ctx := context.Background()
	out, err := bot().PayResource(ctx, "task-01", "C:/tmp/abc.txt", "https://api.example.com/h?z=1", "查天秤座今日运势", "")
	if err != nil {
		t.Fatalf("发起失败: %v", err)
	}
	want := []string{"402-buyer-pay", "--session-id", "task-01", "--file", "C:/tmp/abc.txt",
		"--resource-url", "https://api.example.com/h?z=1", "--intent-summary", "原始请求：查天秤座今日运势"}
	if got := argsOf(t, out); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("命令行不对:\n期望 %v\n实际 %v", want, got)
	}

	out, err = bot().FetchPaid(ctx, "20261008008282214600990000520587", "https://api.example.com/h?z=1", "")
	if err != nil {
		t.Fatalf("取回失败: %v", err)
	}
	want = []string{"402-query-payment-status", "--out-shake-no", "20261008008282214600990000520587",
		"--resource-url", "https://api.example.com/h?z=1"}
	if got := argsOf(t, out); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("命令行不对: %v", got)
	}

	if _, err := bot().FetchPaid(ctx, "abc123", "https://a", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("订单号必须是数字: %v", err)
	}
	if _, err := bot().PayResource(ctx, "s", "f", "https://a", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("付款目的不能为空: %v", err)
	}

	// 原请求带了请求体：付款与取回都要让 alipay-bot 把同一个请求重放一遍。
	replay := []string{"--method", "POST", "--data", `{"zodiac":"天秤座"}`, "--header", "Content-Type:application/json"}
	out, err = bot().PayResource(ctx, "task-01", "C:/tmp/abc.txt", "https://api.example.com/h", "查运势", `{"zodiac":"天秤座"}`)
	if err != nil || !strings.HasSuffix(strings.Join(argsOf(t, out), "|"), strings.Join(replay, "|")) {
		t.Fatalf("付款时应带上重放参数: %v, %v", argsOf(t, out), err)
	}
	out, err = bot().FetchPaid(ctx, "20261008008282214600990000520587", "https://api.example.com/h", `{"zodiac":"天秤座"}`)
	if err != nil || !strings.HasSuffix(strings.Join(argsOf(t, out), "|"), strings.Join(replay, "|")) {
		t.Fatalf("取回时应带上重放参数: %v, %v", argsOf(t, out), err)
	}
}
