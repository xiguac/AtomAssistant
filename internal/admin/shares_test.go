package admin

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/files"
	"github.com/xiguac/atomhelper/internal/share"
)

func TestAdmin_下载链接(t *testing.T) {
	e := setup(t)
	e.login()
	ctx := context.Background()

	// 没开下载入口：只提示怎么开。
	if _, body := e.get("/files"); !strings.Contains(body, "下载入口尚未开启") || strings.Contains(body, `action="/files/share/base"`) {
		t.Fatal("没开下载入口时应只给提示")
	}
	e.server.opts.ShareOn = true

	// 还没保存地址：按当前访问的域名给出建议值。
	_, body := e.get("/files")
	if !strings.Contains(body, `value="`+e.srv.URL+`/download"`) || !strings.Contains(body, "尚未保存") {
		t.Fatalf("应给出建议的外部访问地址:\n%s", body)
	}
	for _, bad := range []string{"atom.hivehyde.com/download", "ftp://a.com", "https://a.com/?x=1"} {
		code, loc := e.post("/files/share/base", url.Values{"csrf": {e.csrf}, "base": {bad}})
		if code != http.StatusSeeOther || !strings.Contains(loc, "e=") {
			t.Errorf("%q 应被拒绝: %d %s", bad, code, loc)
		}
	}
	if got, _ := e.store.Setting(ctx, share.BaseURLSetting); got != "" {
		t.Fatalf("不合法的地址不应保存: %q", got)
	}
	code, loc := e.post("/files/share/base", url.Values{"csrf": {e.csrf}, "base": {" https://atom.hivehyde.com/download/ "}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "已保存") {
		t.Fatalf("合法的地址应保存: %d %s", code, loc)
	}
	if got, _ := e.store.Setting(ctx, share.BaseURLSetting); got != "https://atom.hivehyde.com/download" {
		t.Fatalf("保存的地址不对: %q", got)
	}
	// 不带 CSRF 令牌的不认。
	if code, loc := e.post("/files/share/base", url.Values{"base": {"https://evil.example.com"}}); code == http.StatusSeeOther && strings.Contains(loc, "已保存") {
		t.Fatal("没有 CSRF 令牌不应保存")
	}

	// 列出有效的链接，撤销。
	now := time.Now().UTC()
	l := &share.Link{ID: "s1", Path: "报告/十月.pdf", Files: []files.Pinned{{Path: "报告/十月.pdf", Size: 3, SHA256: "aa"}},
		CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := e.store.CreateShare(ctx, l, "h1"); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get("/files"); !strings.Contains(body, "报告/十月.pdf") || !strings.Contains(body, `value="https://atom.hivehyde.com/download"`) {
		t.Fatal("应列出有效的链接和已保存的地址")
	}
	if code, loc := e.post("/files/share/revoke", url.Values{"csrf": {e.csrf}, "id": {"s1"}}); code != http.StatusSeeOther || !strings.Contains(loc, "已撤销") {
		t.Fatalf("撤销失败: %d %s", code, loc)
	}
	if _, err := e.store.ShareByToken(ctx, "h1", now); err == nil {
		t.Fatal("撤销之后链接应失效")
	}
	if code, loc := e.post("/files/share/revoke", url.Values{"csrf": {e.csrf}, "id": {"s1"}}); code != http.StatusSeeOther || !strings.Contains(loc, "e=") {
		t.Fatalf("撤销一个已失效的链接应报错: %d %s", code, loc)
	}
	if _, body := e.get("/files"); strings.Contains(body, "报告/十月.pdf") {
		t.Fatal("撤销的链接不应再列出")
	}

	// 没登录的不能动。
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := anon.PostForm(e.srv.URL+"/files/share/base", url.Values{"base": {"https://evil.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, _ := e.store.Setting(ctx, share.BaseURLSetting); got != "https://atom.hivehyde.com/download" {
		t.Fatalf("没登录不应能改地址: %q", got)
	}
}
