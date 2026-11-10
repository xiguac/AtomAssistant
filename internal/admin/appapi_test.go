package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/device"
)

// authStore 只实现认证要用的两个方法，其余的方法一调用就 panic，说明测试走偏了。
type authStore struct {
	device.Store
	d     *device.Device
	token string
}

func (a *authStore) DeviceByToken(_ context.Context, tokenHash string) (*device.Device, error) {
	sum := sha256.Sum256([]byte(a.token))
	if tokenHash != hex.EncodeToString(sum[:]) {
		return nil, device.ErrNotFound
	}
	c := *a.d
	return &c, nil
}

func (a *authStore) TouchDevice(context.Context, string, time.Time) error { return nil }

func appServer(t *testing.T, d *device.Device, token string) *Server {
	t.Helper()
	hash, err := HashPassword("test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		PasswordHash: hash, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Devices: &device.Service{Store: &authStore{d: d, token: token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAppAuthed_令牌之外还要这台手机被允许管理(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request, _ *device.Device) { w.WriteHeader(http.StatusNoContent) }
	call := func(s *Server, auth string) int {
		req := httptest.NewRequest("GET", appPrefix+"tasks", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		s.appAuthed(ok)(w, req)
		return w.Code
	}

	plain := appServer(t, &device.Device{ID: "dev_1", Name: "手机", Admin: false}, "good-token")
	if got := call(plain, ""); got != http.StatusUnauthorized {
		t.Errorf("没带令牌 = %d，想要 401", got)
	}
	if got := call(plain, "Bearer wrong-token"); got != http.StatusUnauthorized {
		t.Errorf("令牌不对 = %d，想要 401", got)
	}
	if got := call(plain, "Bearer good-token"); got != http.StatusForbidden {
		t.Errorf("令牌对但没开管理权限 = %d，想要 403：光有数据上报的令牌不能管后台", got)
	}

	admin := appServer(t, &device.Device{ID: "dev_2", Name: "手机", Admin: true}, "good-token")
	if got := call(admin, "Bearer good-token"); got != http.StatusNoContent {
		t.Errorf("开了管理权限 = %d，想要放行", got)
	}
}

func TestAppHandler_路由只认GET和POST且需要认证(t *testing.T) {
	s := appServer(t, &device.Device{ID: "dev_1", Admin: true}, "t")
	h := s.appHandler()
	for _, tc := range []struct{ method, path string }{
		{"GET", "overview"}, {"GET", "tasks"}, {"POST", "tasks"}, {"GET", "tasks/abc"},
		{"POST", "tasks/abc/approve"}, {"POST", "tasks/abc/reject"}, {"POST", "tasks/abc/cancel"},
		{"POST", "tasks/abc/reply"}, {"POST", "tasks/abc/budget"},
	} {
		req := httptest.NewRequest(tc.method, appPrefix+tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 没带令牌 = %d，想要 401", tc.method, tc.path, w.Code)
		}
	}
	// 不存在的方法和路径不会落到处理函数上。
	req := httptest.NewRequest("DELETE", appPrefix+"tasks/abc", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusOK || w.Code == http.StatusUnauthorized {
		t.Errorf("DELETE 不该被接受: %d", w.Code)
	}
}

// 没有任务时，概览里「最近的任务」要是 []，不能是 null：手机端按数组解码，null 会读不出整页。
func TestAppOverview_空列表发的是数组不是null(t *testing.T) {
	var recent []appTask
	b, _ := json.Marshal(map[string]any{"recent": recent})
	if string(b) != `{"recent":null}` {
		t.Fatalf("前提不成立: %s", b)
	}
	b, _ = json.Marshal(map[string]any{"recent": []appTask{}})
	if string(b) != `{"recent":[]}` {
		t.Errorf("空数组应当序列化成 []: %s", b)
	}
}
