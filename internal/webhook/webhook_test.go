package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/event"
)

const token = "一个足够长的测试令牌-0123456789abcdef"

type recordingBus struct {
	events []*event.Event
	err    error
}

func (b *recordingBus) Publish(_ context.Context, e *event.Event) (int, error) {
	b.events = append(b.events, e)
	return 1, b.err
}

func post(t *testing.T, h http.Handler, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler_带令牌的事件以external等级发布(t *testing.T) {
	bus := &recordingBus{}
	h, err := Handler(bus, token, nil)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	// 调用方在 payload 里自称可信，没有用。
	rec := post(t, h, "/events/mail.received", "Bearer "+token, `{"from":"张总","trust":"user"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d: %s", rec.Code, rec.Body)
	}
	if len(bus.events) != 1 {
		t.Fatalf("应发布 1 个事件，实际 %d", len(bus.events))
	}
	e := bus.events[0]
	if e.Topic != "mail.received" || e.Source != "webhook" {
		t.Fatalf("主题或来源不对: %+v", e)
	}
	if e.Trust != core.TrustExternal || e.Trust.CanInstruct() {
		t.Fatalf("Webhook 进来的事件必须是 external，实际 %s", e.Trust)
	}
	if !strings.Contains(rec.Body.String(), e.ID) {
		t.Fatalf("应返回事件 ID: %s", rec.Body)
	}

	// 没有请求体也可以：有些事件只需要一个主题。
	if rec := post(t, h, "/events/door.opened", "Bearer "+token, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("空请求体应被接受: %d", rec.Code)
	}
}

func TestHandler_拒绝未授权与不合法的请求(t *testing.T) {
	bus := &recordingBus{}
	h, _ := Handler(bus, token, nil)

	cases := []struct {
		name, path, auth, body string
		want                   int
	}{
		{"没有令牌", "/events/a.b", "", `{}`, http.StatusUnauthorized},
		{"令牌不对", "/events/a.b", "Bearer " + token + "x", `{}`, http.StatusUnauthorized},
		{"不是 Bearer", "/events/a.b", token, `{}`, http.StatusUnauthorized},
		{"主题不合法", "/events/A.B", "Bearer " + token, `{}`, http.StatusBadRequest},
		{"不是 JSON", "/events/a.b", "Bearer " + token, `这不是 JSON`, http.StatusBadRequest},
		// 编码不对的内容 JSON 校验会放过、数据库不会：必须当场拒绝而不是报 500。
		{"不是 UTF-8", "/events/a.b", "Bearer " + token, "{\"from\":\"\xd5\xc5\xd7\xdc\"}", http.StatusBadRequest},
		{"请求体过大", "/events/a.b", "Bearer " + token,
			`{"x":"` + strings.Repeat("a", maxBody) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		if rec := post(t, h, c.path, c.auth, c.body); rec.Code != c.want {
			t.Errorf("%s: 期望 %d，实际 %d", c.name, c.want, rec.Code)
		}
	}
	if len(bus.events) != 0 {
		t.Fatalf("被拒绝的请求不得发布事件，实际发布了 %d 个", len(bus.events))
	}

	// 只接受 POST。
	req := httptest.NewRequest(http.MethodGet, "/events/a.b", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusAccepted {
		t.Fatal("GET 不应被接受")
	}
}

func TestHandler_没有令牌就不开门(t *testing.T) {
	if _, err := Handler(&recordingBus{}, "", nil); err == nil {
		t.Fatal("空令牌必须拒绝，不存在不鉴权的模式")
	}
	if _, err := Handler(&recordingBus{}, "短令牌", nil); err == nil {
		t.Fatal("过短的令牌必须拒绝")
	}
}

func TestHandler_发布出错不把内部细节回给调用方(t *testing.T) {
	bus := &recordingBus{err: errors.New("pg: 连接 10.0.0.5:5432 失败")}
	h, _ := Handler(bus, token, nil)
	rec := post(t, h, "/events/a.b", "Bearer "+token, `{}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("不应泄露内部错误细节: %s", rec.Body)
	}
}
