package share

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/files"
)

// memStore 是测试用的链接存储。
type memStore struct {
	mu    sync.Mutex
	links map[string]*Link
	hash  map[string]string // 令牌摘要 -> 编号
	gone  map[string]bool   // 撤销了的
}

func newMemStore() *memStore {
	return &memStore{links: map[string]*Link{}, hash: map[string]string{}, gone: map[string]bool{}}
}

func (m *memStore) CreateShare(_ context.Context, l *Link, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *l
	m.links[l.ID], m.hash[tokenHash] = &c, l.ID
	return nil
}

func (m *memStore) active(id string, now time.Time) bool {
	l, ok := m.links[id]
	return ok && !m.gone[id] && l.ExpiresAt.After(now)
}

func (m *memStore) ShareByToken(_ context.Context, tokenHash string, now time.Time) (*Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.hash[tokenHash]
	if !m.active(id, now) {
		return nil, ErrNotFound
	}
	c := *m.links[id]
	return &c, nil
}

func (m *memStore) ActiveShares(_ context.Context, now time.Time) ([]*Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Link
	for id, l := range m.links {
		if m.active(id, now) {
			out = append(out, l)
		}
	}
	return out, nil
}

func (m *memStore) RevokeShare(_ context.Context, id string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active(id, now) {
		return false, nil
	}
	m.gone[id] = true
	return true, nil
}

func (m *memStore) DeleteExpiredShares(context.Context, time.Time) error { return nil }

// memSettings 是测试用的后台设置。
type memSettings map[string]string

func (m memSettings) Setting(_ context.Context, key string) (string, error) { return m[key], nil }

type fixture struct {
	t   *testing.T
	ws  *files.Workspace
	svc *Service
	srv *httptest.Server
	now time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ws, err := files.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ws: ws, now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	settings := memSettings{}
	f.svc = &Service{Store: newMemStore(), Workspace: ws, Settings: settings, now: func() time.Time { return f.now }}
	f.srv = httptest.NewServer(f.svc.Handler(nil))
	t.Cleanup(f.srv.Close)
	settings[BaseURLSetting] = f.srv.URL
	return f
}

func (f *fixture) write(rel, body string) {
	f.t.Helper()
	if _, err := f.ws.WriteFile(rel, []byte(body)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) get(url string) (int, http.Header, string) {
	f.t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

func TestShare_文件链接(t *testing.T) {
	f := setup(t)
	f.write("报告/十月.pdf", "%PDF 十月")
	c, err := f.svc.Create(context.Background(), "task-1", "报告/十月.pdf", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.URL, f.srv.URL+"/s/") || strings.Contains(c.URL, "//s/") || c.ExpiresAt != f.now.Add(30*time.Minute) {
		t.Fatalf("链接不对: %+v", c)
	}
	code, h, body := f.get(c.URL)
	if code != http.StatusOK || body != "%PDF 十月" || h.Get("Content-Type") != "application/pdf" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "attachment;") || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("下载不对: %d %v %q", code, h, body)
	}
	// 文件链接后面不能再接路径。
	if code, _, _ := f.get(c.URL + "/十月.pdf"); code != http.StatusNotFound {
		t.Fatalf("文件链接接路径应 404: %d", code)
	}
	// 文件之后被改了：不再给。
	f.write("报告/十月.pdf", "改过的")
	if code, _, body := f.get(c.URL); code != http.StatusNotFound || strings.Contains(body, "改过的") {
		t.Fatalf("改过的文件不应经旧链接出去: %d %q", code, body)
	}
}

func TestShare_目录链接(t *testing.T) {
	f := setup(t)
	f.write("图/a.png", "PNG a")
	f.write("图/子 目录/b#1.png", "PNG b")
	f.write("别的/c.txt", "不该出去")
	if _, err := f.ws.Receive("身份证.jpg", []byte("JPEG 身份证")); err != nil {
		t.Fatal(err)
	}
	c, err := f.svc.Create(context.Background(), "", "图", 0)
	if err != nil || !c.Dir || len(c.Files) != 2 || c.ExpiresAt != f.now.Add(DefaultTTL) {
		t.Fatalf("目录链接不对: %v %+v", err, c)
	}
	code, _, body := f.get(c.URL)
	var list struct {
		Name  string `json:"name"`
		Files []struct {
			Path string `json:"path"`
			URL  string `json:"url"`
		} `json:"files"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil || list.Name != "图" || len(list.Files) != 2 {
		t.Fatalf("清单不对: %d %q", code, body)
	}
	for _, it := range list.Files {
		code, _, got := f.get(it.URL)
		if code != http.StatusOK || !strings.HasPrefix(got, "PNG") {
			t.Fatalf("取 %s 不对: %d %q", it.Path, code, got)
		}
	}
	// 清单之外的一律没有，不管怎么拼路径。
	token := strings.TrimPrefix(c.URL, f.srv.URL+"/s/")
	for _, sub := range []string{
		"../别的/c.txt", "..%2F别的%2Fc.txt", "%2e%2e/别的/c.txt", "/图/a.png", "图/a.png",
		"../收到的文件/身份证.jpg", "..%2f..%2f收到的文件%2f身份证.jpg", "a.png/", "A.png", "a.png%00",
	} {
		code, _, got := f.get(f.srv.URL + "/s/" + token + "/" + sub)
		if code == http.StatusOK || strings.Contains(got, "不该出去") || strings.Contains(got, "身份证") {
			t.Errorf("%s: 不应取到东西: %d %q", sub, code, got)
		}
	}
}

func TestShare_失效的链接一律404(t *testing.T) {
	f := setup(t)
	f.write("a.txt", "a")
	ctx := context.Background()
	c, err := f.svc.Create(ctx, "", "a.txt", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(c.URL, f.srv.URL+"/s/")
	for _, bad := range []string{
		f.srv.URL + "/s/" + strings.Repeat("A", len(token)),
		f.srv.URL + "/s/" + token[:len(token)-1],
		f.srv.URL + "/s/" + token + "x",
		f.srv.URL + "/s/",
		f.srv.URL + "/",
		f.srv.URL + "/a.txt",
	} {
		if code, _, _ := f.get(bad); code != http.StatusNotFound {
			t.Errorf("%s: 期望 404，实际 %d", bad, code)
		}
	}
	// 只接受 GET（和 HEAD）。
	resp, err := http.Post(c.URL, "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("POST 不应成功")
	}

	f.now = f.now.Add(time.Hour)
	if code, _, _ := f.get(c.URL); code != http.StatusNotFound {
		t.Fatalf("过期的链接应 404: %d", code)
	}
	f.now = f.now.Add(-time.Hour)
	if err := f.svc.Revoke(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.get(c.URL); code != http.StatusNotFound {
		t.Fatalf("撤销的链接应 404: %d", code)
	}
	if err := f.svc.Revoke(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("撤销一个已经失效的链接应报 ErrNotFound: %v", err)
	}
}

func TestShare_建链接的限制(t *testing.T) {
	f := setup(t)
	f.write("a.txt", "a")
	ctx := context.Background()
	for _, ttl := range []time.Duration{-time.Minute, time.Second, MaxTTL + time.Minute} {
		if _, err := f.svc.Create(ctx, "", "a.txt", ttl); !errors.Is(err, ErrInvalid) {
			t.Errorf("有效期 %s 应被拒绝: %v", ttl, err)
		}
	}
	if _, err := f.svc.Create(ctx, "", "a.txt", MaxTTL); err != nil {
		t.Fatalf("24 小时应可以: %v", err)
	}
	got, err := f.ws.Receive("合同.pdf", []byte("合同"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, "", got, 0); !errors.Is(err, files.ErrInvalid) {
		t.Fatalf("用户的文件不能分享: %v", err)
	}
	// 手动拷进来的、没有记录的文件。
	if err := os.WriteFile(filepath.Join(f.ws.Root, "手放的.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, "", "手放的.txt", 0); !errors.Is(err, files.ErrInvalid) {
		t.Fatalf("没有来源记录的文件不能分享: %v", err)
	}
	for i := 1; i < MaxActive; i++ {
		if _, err := f.svc.Create(ctx, "", "a.txt", 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.Create(ctx, "", "a.txt", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("有效链接到上限应拒绝: %v", err)
	}
}

func TestShare_没设外部访问地址建不了链接(t *testing.T) {
	f := setup(t)
	f.write("a.txt", "a")
	f.svc.Settings = memSettings{}
	if _, err := f.svc.Create(context.Background(), "", "a.txt", 0); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "外部访问地址") {
		t.Fatalf("没设外部访问地址应报错: %v", err)
	}
}

func TestShare_带路径前缀的外部访问地址(t *testing.T) {
	f := setup(t)
	f.write("图/a.png", "PNG a")
	// 反向代理把 /download/ 转给下载入口时会去掉前缀，这里直接拿令牌后面的部分访问。
	f.svc.Settings = memSettings{BaseURLSetting: "https://atom.example.com/download"}
	c, err := f.svc.Create(context.Background(), "", "图", 0)
	if err != nil || !strings.HasPrefix(c.URL, "https://atom.example.com/download/s/") {
		t.Fatalf("链接应以外部访问地址开头: %v %q", err, c.URL)
	}
	code, _, body := f.get(f.srv.URL + strings.TrimPrefix(c.URL, "https://atom.example.com/download"))
	if code != http.StatusOK || !strings.Contains(body, `"url":"https://atom.example.com/download/s/`) {
		t.Fatalf("清单里的地址应以外部访问地址开头: %d %q", code, body)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://atom.hivehyde.com/download":   "https://atom.hivehyde.com/download",
		" https://atom.hivehyde.com/download/": "https://atom.hivehyde.com/download",
		"http://127.0.0.1:8790":                "http://127.0.0.1:8790",
	} {
		if got, err := NormalizeBaseURL(in); err != nil || got != want {
			t.Errorf("%q: 期望 %q，实际 %q (%v)", in, want, got, err)
		}
	}
	for _, bad := range []string{"", "atom.hivehyde.com/download", "ftp://a.com", "https://a.com/?x=1", "https://a.com/#x",
		"https://user:pass@a.com", "javascript:alert(1)", "https://"} {
		if _, err := NormalizeBaseURL(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestCheckAddr(t *testing.T) {
	for addr, tls := range map[string]bool{"127.0.0.1:8790": false, "localhost:8790": false, "[::1]:8790": false, "0.0.0.0:443": true} {
		if err := CheckAddr(addr, tls); err != nil {
			t.Errorf("%s tls=%v 应被允许: %v", addr, tls, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8790", ":8790", "118.195.207.111:8790", "127.0.0.1"} {
		if err := CheckAddr(addr, false); err == nil {
			t.Errorf("%s 没有 TLS 应被拒绝", addr)
		}
	}
}
