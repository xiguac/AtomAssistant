// Package admin 是网页管理后台：在浏览器里看任务、批准动作、管理保险库、改配置、传文件。
//
// 它经手的是整个系统里最敏感的东西——保险库的条目是从这里填进去的，
// 消息渠道和它们的主人是在这里配的。所以它的约束比别处都严：
//
//   - **必须登录。** 口令只以 bcrypt 摘要的形式存在于服务器的环境里；连续输错会被暂时锁住。
//   - **默认只听本机。** 监听地址不是本机时必须同时配置 TLS 证书，否则拒绝启动：
//     口令和填进保险库的内容不能在网络上明文传输。没有证书时，经 SSH 隧道访问。
//   - **每个改动都要带一次性的防伪令牌**，别的网页没法替你提交表单。
//   - **保险库只进不出。** 这里和命令行一样，没有任何查看条目内容的页面。
//   - **页面上的内容一律转义。** 任务的步骤输出里有邮件、网页这类别人写的东西，
//     它们在这里只是文字，不会被当成网页代码执行。
//
// 在这里做的事，就是用户亲手做的事：这里建的任务是用户等级的，这里配的渠道是可信渠道。
package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/xiguac/atomhelper/internal/channel"
	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/files"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/vault"
)

//go:embed templates/*.html
var templateFS embed.FS

const (
	cookieName = "atom_session"
	sessionTTL = 12 * time.Hour
	// 连续输错这么多次口令之后，暂停登录一段时间。
	maxFailures = 5
	lockFor     = 5 * time.Minute
	// maxUpload 是上传文件的大小上限，与工作区的单文件上限一致。
	maxUpload = files.MaxFileBytes
	// maxConfigBytes 是配置文件的大小上限。
	maxConfigBytes = 1 << 20
)

// Options 是后台的依赖。
type Options struct {
	Store *pg.Store
	Vault *vault.Vault
	// Tasks 用来新建任务、回话、批准和拒绝，与消息渠道走的是同一套入口。
	Tasks *channel.Kernel
	// ConfigPath 是 YAML 配置文件的路径，后台可以编辑它。
	ConfigPath string
	// PasswordHash 是登录口令的 bcrypt 摘要，用 atomctl admin passwd 生成。
	PasswordHash string
	// TLS 为真表示后台自己提供 HTTPS，会话 cookie 会标成仅限加密连接。
	TLS bool
	// Restart 让进程退出，由进程管理器（systemd）拉起新的。改完配置后用。
	Restart func()
	// Status 返回当前的运行状况；非空表示助手本身没能启动（比如配置还没填完），只有后台在运行。
	Status func() string
	Log    *slog.Logger
}

// Server 是管理后台。
type Server struct {
	opts Options
	tmpl map[string]*template.Template

	mu       sync.Mutex
	sessions map[string]session
	failures int
	lockedAt time.Time
}

type session struct {
	csrf    string
	expires time.Time
}

// CheckAddr 校验监听地址：不是本机地址时必须有 TLS。
func CheckAddr(addr string, tls bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("admin: 监听地址 %q 要写成 主机:端口", addr)
	}
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !local && !tls {
		return fmt.Errorf("admin: 后台监听在 %s 上却没有配置 TLS 证书——口令和保险库的内容会在网络上明文传输。"+
			"要么监听 127.0.0.1 并经 SSH 隧道访问，要么设置 ATOM_ADMIN_TLS_CERT 与 ATOM_ADMIN_TLS_KEY", host)
	}
	return nil
}

// HashPassword 生成口令的 bcrypt 摘要。
func HashPassword(password string) (string, error) {
	if len([]rune(password)) < 10 {
		return "", fmt.Errorf("admin: 口令至少要 10 个字符")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", fmt.Errorf("admin: 生成口令摘要失败: %w", err)
	}
	return string(hash), nil
}

// New 构造后台。
func New(opts Options) (*Server, error) {
	if opts.PasswordHash == "" {
		return nil, fmt.Errorf("admin: 没有设置登录口令（ATOM_ADMIN_PASSWORD_HASH，用 atomctl admin passwd 生成）")
	}
	if _, err := bcrypt.Cost([]byte(opts.PasswordHash)); err != nil {
		return nil, fmt.Errorf("admin: ATOM_ADMIN_PASSWORD_HASH 不是有效的 bcrypt 摘要（用 atomctl admin passwd 生成）")
	}
	s := &Server{opts: opts, sessions: make(map[string]session), tmpl: make(map[string]*template.Template)}
	funcs := template.FuncMap{
		"when": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("01-02 15:04")
		},
		"short": func(s string, n int) string {
			r := []rune(strings.Join(strings.Fields(s), " "))
			if len(r) > n {
				return string(r[:n]) + "…"
			}
			return string(r)
		},
		"stateName":  stateName,
		"stateClass": stateClass,
		"waitKind":   waitKind,
		"waitText":   waitText,
		"join":       strings.Join,
		"size":       humanSize,
		"add":        func(a, b int) int { return a + b },
	}
	pages, err := templateFS.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		if p.Name() == "layout.html" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+p.Name())
		if err != nil {
			return nil, fmt.Errorf("admin: 页面模板 %s 有误: %w", p.Name(), err)
		}
		s.tmpl[strings.TrimSuffix(p.Name(), ".html")] = t
	}
	return s, nil
}

func stateName(st core.TaskState) string {
	switch st {
	case core.StateNew, core.StateReady:
		return "排队中"
	case core.StateRunning:
		return "进行中"
	case core.StateWaiting:
		return "等待中"
	case core.StateSuspended:
		return "已挂起"
	case core.StateDone:
		return "已完成"
	case core.StateFailed:
		return "没办成"
	case core.StateCancelled:
		return "已取消"
	}
	return string(st)
}

// stateClass 给出一个任务状态对应的徽标样式：ok / warn / bad / info / idle。
func stateClass(st core.TaskState) string {
	switch st {
	case core.StateDone:
		return "ok"
	case core.StateFailed, core.StateCancelled:
		return "bad"
	case core.StateRunning:
		return "info"
	case core.StateWaiting, core.StateSuspended:
		return "warn"
	}
	return "idle"
}

// waitKind 说明一个任务在等用户做什么。
func waitKind(t *core.Task) string {
	switch {
	case t.State == core.StateSuspended:
		return "已挂起"
	case t.Wake != nil && t.Wake.Kind == core.WakeApproval:
		return "等你确认"
	}
	return "等你回答"
}

// waitText 用一句话说明任务在等什么，去掉内部的套话。
func waitText(t *core.Task) string {
	switch {
	case t.State == core.StateSuspended:
		return t.Reason
	case t.Wake != nil && t.Wake.Kind == core.WakeApproval:
		tool := strings.TrimPrefix(t.Reason, "等待用户确认：")
		if i := strings.Index(tool, "（"); i > 0 {
			tool = tool[:i]
		}
		if _, why := kernel.SplitApprovalReason(t.Reason); why != "" {
			return tool + " —— " + why
		}
		return tool
	}
	return strings.TrimPrefix(t.Reason, "等待你的回复：")
}

// humanSize 把字节数写成人看的大小。
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func token() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Handler 返回后台的 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)

	auth := func(h func(http.ResponseWriter, *http.Request, *view)) http.HandlerFunc { return s.authed(h) }
	mux.HandleFunc("POST /logout", auth(s.logout))
	mux.HandleFunc("GET /{$}", auth(s.home))
	mux.HandleFunc("POST /tasks", auth(s.createTask))
	mux.HandleFunc("GET /tasks/{id}", auth(s.taskPage))
	mux.HandleFunc("POST /tasks/{id}/approve", auth(s.approve))
	mux.HandleFunc("POST /tasks/{id}/reject", auth(s.reject))
	mux.HandleFunc("POST /tasks/{id}/reply", auth(s.reply))
	mux.HandleFunc("GET /vault", auth(s.vaultPage))
	mux.HandleFunc("POST /vault", auth(s.vaultPut))
	mux.HandleFunc("POST /vault/delete", auth(s.vaultDelete))
	mux.HandleFunc("GET /memory", auth(s.memoryPage))
	mux.HandleFunc("POST /memory/confirm", auth(s.memoryConfirm))
	mux.HandleFunc("POST /memory/dismiss", auth(s.memoryDismiss))
	mux.HandleFunc("GET /config", auth(s.configPage))
	mux.HandleFunc("POST /config", auth(s.configSave))
	mux.HandleFunc("POST /restart", auth(s.restart))
	mux.HandleFunc("GET /files", auth(s.filesPage))
	mux.HandleFunc("POST /files", auth(s.filesUpload))
	mux.HandleFunc("GET /files/download", auth(s.filesDownload))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// 页面不引用任何外部资源，也不许被别的网页嵌进去。
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

// view 是渲染页面时的公共数据。
type view struct {
	CSRF   string
	Status string
	Notice string
	Error  string
	Page   string
	Data   any
}

func (s *Server) render(w http.ResponseWriter, name string, v *view) {
	t, ok := s.tmpl[name]
	if !ok {
		http.Error(w, "页面不存在", http.StatusNotFound)
		return
	}
	v.Page = name
	if s.opts.Status != nil {
		v.Status = s.opts.Status()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, v); err != nil {
		s.opts.Log.Error("渲染页面失败", "页面", name, "err", err)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := ""
	if s.opts.Status != nil {
		status = s.opts.Status()
	}
	// 不登录也能访问，所以只说"好不好"，不说为什么不好——原因里可能带着配置的细节。
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true, "assistant_running": status == ""})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login", &view{Error: r.URL.Query().Get("e")})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.failures >= maxFailures && time.Since(s.lockedAt) < lockFor {
		s.mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login", &view{Error: "口令连续输错太多次，请过几分钟再试。"})
		return
	}
	s.mu.Unlock()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	password := r.PostFormValue("password")
	if bcrypt.CompareHashAndPassword([]byte(s.opts.PasswordHash), []byte(password)) != nil {
		s.mu.Lock()
		s.failures++
		s.lockedAt = time.Now()
		s.mu.Unlock()
		s.opts.Log.Warn("后台登录失败", "来自", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", &view{Error: "口令不对。"})
		return
	}
	id, err1 := token()
	csrf, err2 := token()
	if err1 != nil || err2 != nil {
		http.Error(w, "生成会话失败", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.failures = 0
	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = session{csrf: csrf, expires: now.Add(sessionTTL)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: id, Path: "/", HttpOnly: true, Secure: s.opts.TLS,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
	})
	s.opts.Log.Info("后台登录成功", "来自", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// authed 包住需要登录的页面：没登录的去登录页；改动数据的请求还要核对防伪令牌。
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *view)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		s.mu.Lock()
		sess, ok := s.sessions[c.Value]
		if ok && time.Now().After(sess.expires) {
			delete(s.sessions, c.Value)
			ok = false
		}
		s.mu.Unlock()
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// 上传文件的表单很大，其余的都很小。
			limit := int64(1 << 20)
			if r.URL.Path == "/files" {
				limit = maxUpload + (1 << 20)
			} else if r.URL.Path == "/config" {
				limit = maxConfigBytes + (1 << 16)
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			got := r.FormValue("csrf")
			if subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) != 1 {
				http.Error(w, "请求已失效，请回到页面刷新后重试。", http.StatusForbidden)
				return
			}
			// 表单里的文字必须是合法的 UTF-8：不是的话数据库不收，与其让它在深处报一个看不懂的错，不如在门口说清楚。
			for _, values := range r.PostForm {
				for _, value := range values {
					if !utf8.ValidString(value) {
						http.Error(w, "提交的内容不是 UTF-8 编码，没有处理。", http.StatusBadRequest)
						return
					}
				}
			}
		}
		h(w, r, &view{CSRF: sess.csrf, Notice: r.URL.Query().Get("ok"), Error: r.URL.Query().Get("e")})
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ *view) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// back 做完一件事后跳回某个页面，带上一句结果。
func back(w http.ResponseWriter, r *http.Request, to, notice string, err error) {
	q := "?ok=" + template.URLQueryEscaper(notice)
	if err != nil {
		q = "?e=" + template.URLQueryEscaper(err.Error())
	}
	http.Redirect(w, r, to+q, http.StatusSeeOther)
}

// ---- 任务 ----

type homeData struct {
	Waiting []*core.Task
	Active  []*core.Task
	Recent  []*core.Task
}

func needsUser(t *core.Task) bool {
	return t.State == core.StateSuspended ||
		(t.State == core.StateWaiting && t.Wake != nil && (t.Wake.Kind == core.WakeApproval ||
			(t.Wake.Kind == core.WakeEvent && strings.Contains(t.Wake.Expr, `"user.message"`))))
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, v *view) {
	tasks, err := s.opts.Store.ListTasks(r.Context(), false, 60)
	if err != nil {
		v.Error = err.Error()
	}
	var d homeData
	for _, t := range tasks {
		switch {
		case needsUser(t):
			d.Waiting = append(d.Waiting, t)
		case !t.State.Terminal():
			d.Active = append(d.Active, t)
		default:
			d.Recent = append(d.Recent, t)
		}
	}
	v.Data = d
	s.render(w, "home", v)
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request, _ *view) {
	goal := strings.TrimSpace(r.PostFormValue("goal"))
	if goal == "" {
		back(w, r, "/", "", errors.New("没有写要办什么事"))
		return
	}
	title := goal
	if i := strings.IndexAny(title, "\n。"); i > 0 {
		title = title[:i]
	}
	if rs := []rune(title); len(rs) > 24 {
		title = string(rs[:24]) + "…"
	}
	// 后台是用户亲手使用的地方，这里建的任务是用户等级的。
	id, err := s.opts.Tasks.Create(r.Context(), title, goal, nil)
	if err != nil {
		back(w, r, "/", "", err)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

type stepView struct {
	*core.Step
	In, Out string
}

// field 是等确认的动作里的一项参数。
type field struct {
	Key   string
	Value string
	// Long 为真表示值有多行（比如邮件正文），要单独成块展示。
	Long bool
}

type taskData struct {
	Task    *core.Task
	Steps   []stepView
	Pending *channel.Pending
	Fields  []field
	// Irreversible 为真表示等确认的这一步做了就收不回来。
	Irreversible bool
	// Why 是助手想这么做的理由，Rule 是策略要求确认的特别原因（平常的那种不显示）。
	Why, Rule string
	Asking    bool
	// Question 是任务正在问用户的问题。
	Question string
}

// fields 把动作的参数拆成一项一项，方便核对。
func fields(raw json.RawMessage) []field {
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return []field{{Key: "参数", Value: string(raw), Long: true}}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]field, 0, len(keys))
	for _, k := range keys {
		v, ok := params[k].(string)
		if !ok {
			b, err := json.MarshalIndent(params[k], "", "  ")
			if err != nil {
				continue
			}
			v = string(b)
		}
		out = append(out, field{Key: k, Value: v, Long: strings.ContainsRune(v, '\n') || len([]rune(v)) > 80})
	}
	return out
}

func pretty(raw json.RawMessage, max int) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	if r := []rune(string(b)); len(r) > max {
		return string(r[:max]) + "\n…（已截断）"
	}
	return string(b)
}

func (s *Server) taskPage(w http.ResponseWriter, r *http.Request, v *view) {
	id := r.PathValue("id")
	t, err := s.opts.Store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, "没有这个任务", http.StatusNotFound)
		return
	}
	steps, err := s.opts.Store.ListSteps(r.Context(), id)
	if err != nil {
		v.Error = err.Error()
	}
	d := taskData{Task: t, Asking: !t.State.Terminal()}
	for _, st := range steps {
		d.Steps = append(d.Steps, stepView{Step: st, In: pretty(st.Input, 4000), Out: pretty(st.Output, 4000)})
	}
	if t.State == core.StateWaiting && t.Wake != nil && t.Wake.Kind == core.WakeApproval {
		if p, err := s.opts.Tasks.PendingAction(r.Context(), id); err == nil && p != nil {
			d.Pending, d.Fields = p, fields(p.Params)
			d.Rule, d.Why = kernel.SplitApprovalReason(t.Reason)
			if d.Rule == policy.ReasonNoRule {
				d.Rule = ""
			}
			for _, st := range steps {
				if st.Status == core.StepAwaiting {
					d.Irreversible = st.SideEffect == core.SideEffectIrreversible
				}
			}
		}
	}
	if needsUser(t) && d.Pending == nil && t.State == core.StateWaiting {
		d.Question = strings.TrimPrefix(t.Reason, "等待你的回复：")
	}
	v.Data = d
	s.render(w, "task", v)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PathValue("id")
	back(w, r, "/tasks/"+id, "已批准，任务继续执行。", s.opts.Tasks.Approve(r.Context(), id))
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PathValue("id")
	back(w, r, "/tasks/"+id, "已打回，助手会按你说的重新来。", s.opts.Tasks.Reject(r.Context(), id, strings.TrimSpace(r.PostFormValue("reason"))))
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PathValue("id")
	text := strings.TrimSpace(r.PostFormValue("text"))
	if text == "" {
		back(w, r, "/tasks/"+id, "", errors.New("没有写内容"))
		return
	}
	back(w, r, "/tasks/"+id, "已送达。", s.opts.Tasks.Reply(r.Context(), id, text, "admin"))
}

// ---- 保险库 ----

func (s *Server) vaultPage(w http.ResponseWriter, r *http.Request, v *view) {
	metas, err := s.opts.Vault.List(r.Context())
	if err != nil {
		v.Error = err.Error()
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Label < metas[j].Label })
	v.Data = metas
	s.render(w, "vault", v)
}

func (s *Server) vaultPut(w http.ResponseWriter, r *http.Request, _ *view) {
	var tools []string
	for _, t := range strings.FieldsFunc(r.PostFormValue("tools"), func(c rune) bool { return c == ',' || c == '，' || c == ' ' || c == '\n' || c == '\r' }) {
		if t = strings.TrimSpace(t); t != "" {
			tools = append(tools, t)
		}
	}
	label := strings.TrimSpace(r.PostFormValue("label"))
	// 值原样存入，不做任何整理——口令里的空格也是口令的一部分。只去掉粘贴时常带上的结尾换行。
	value := strings.TrimRight(r.PostFormValue("value"), "\r\n")
	err := s.opts.Vault.Seal(r.Context(), label, value, strings.TrimSpace(r.PostFormValue("note")), tools)
	if err == nil {
		s.opts.Log.Info("保险库条目已写入", "名字", label, "可用于", strings.Join(tools, ","))
	}
	back(w, r, "/vault", "已存入「"+label+"」。存进去之后就看不到内容了，只能替换或删除。", err)
}

func (s *Server) vaultDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	label := r.PostFormValue("label")
	err := s.opts.Vault.Remove(r.Context(), label)
	if err == nil {
		s.opts.Log.Info("保险库条目已删除", "名字", label)
	}
	back(w, r, "/vault", "已删除「"+label+"」。", err)
}

// ---- 记忆 ----

type memoryData struct {
	Pending  []memory.Pending
	Memories []*memory.Memory
}

func (s *Server) memoryPage(w http.ResponseWriter, r *http.Request, v *view) {
	store := s.opts.Store.Memories()
	pending, err := store.ListPending(r.Context(), "")
	if err != nil {
		v.Error = err.Error()
	}
	list, err := store.List(r.Context(), memory.Query{})
	if err != nil {
		v.Error = err.Error()
	}
	v.Data = memoryData{Pending: pending, Memories: list}
	s.render(w, "memory", v)
}

func (s *Server) memoryConfirm(w http.ResponseWriter, r *http.Request, _ *view) {
	_, err := memory.NewPipeline(s.opts.Store.Memories(), nil).Confirm(r.Context(), r.PostFormValue("id"))
	back(w, r, "/memory", "已确认，之后会按它来。", err)
}

func (s *Server) memoryDismiss(w http.ResponseWriter, r *http.Request, _ *view) {
	back(w, r, "/memory", "已否决。", memory.NewPipeline(s.opts.Store.Memories(), nil).Dismiss(r.Context(), r.PostFormValue("id")))
}

// ---- 配置 ----

func (s *Server) configPage(w http.ResponseWriter, _ *http.Request, v *view) {
	raw, err := os.ReadFile(s.opts.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		v.Error = "读取配置文件失败: " + err.Error()
	}
	v.Data = map[string]string{"Text": string(raw), "Path": s.opts.ConfigPath}
	s.render(w, "config", v)
}

// configSave 校验并保存配置。校验不过的一个字都不会写进去。
func (s *Server) configSave(w http.ResponseWriter, r *http.Request, _ *view) {
	text := strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n")
	if len(text) > maxConfigBytes {
		back(w, r, "/config", "", errors.New("配置太长了"))
		return
	}
	if s.opts.ConfigPath == "" {
		back(w, r, "/config", "", errors.New("没有设置 ATOM_CONFIG，不知道配置文件该存在哪"))
		return
	}
	dir := filepath.Dir(s.opts.ConfigPath)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		back(w, r, "/config", "", fmt.Errorf("写不了配置目录: %w", err))
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := io.WriteString(tmp, text); err != nil {
		_ = tmp.Close()
		back(w, r, "/config", "", err)
		return
	}
	if err := tmp.Close(); err != nil {
		back(w, r, "/config", "", err)
		return
	}
	if _, err := config.LoadFile(tmp.Name()); err != nil {
		back(w, r, "/config", "", fmt.Errorf("配置有误，没有保存: %w", err))
		return
	}
	// 旧的那份留一个备份，改错了能找回来。
	if old, err := os.ReadFile(s.opts.ConfigPath); err == nil {
		if err := os.WriteFile(s.opts.ConfigPath+".bak", old, 0o600); err != nil {
			back(w, r, "/config", "", fmt.Errorf("备份旧配置失败: %w", err))
			return
		}
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		back(w, r, "/config", "", err)
		return
	}
	if err := os.Rename(tmp.Name(), s.opts.ConfigPath); err != nil {
		back(w, r, "/config", "", fmt.Errorf("保存配置失败: %w", err))
		return
	}
	s.opts.Log.Info("配置已在后台修改", "来自", r.RemoteAddr)
	back(w, r, "/config", "已保存。重启之后生效。", nil)
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request, v *view) {
	if s.opts.Restart == nil {
		back(w, r, "/config", "", errors.New("这个进程不是由进程管理器启动的，请手动重启"))
		return
	}
	s.opts.Log.Info("后台请求重启", "来自", r.RemoteAddr)
	s.render(w, "restarting", v)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// 等页面发出去再退出。
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.opts.Restart()
	}()
}

// ---- 文件 ----

// workspace 按当前的配置打开工作区。每次现开：配置在后台改过之后不必重启也看得到。
func (s *Server) workspace() (*files.Workspace, error) {
	file, err := config.LoadFile(s.opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, errors.New("还没有配置文件")
	}
	ws, err := file.Workspace()
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, errors.New("配置里没有 files 一节，还没有启用文件处理")
	}
	return ws, nil
}

type filesData struct {
	Dir     string
	Parent  string
	Entries []files.Entry
	Enabled bool
}

func (s *Server) filesPage(w http.ResponseWriter, r *http.Request, v *view) {
	d := filesData{Dir: r.URL.Query().Get("dir")}
	ws, err := s.workspace()
	if err != nil {
		v.Error = err.Error()
	} else {
		d.Enabled = true
		if d.Entries, err = ws.List(d.Dir); err != nil {
			v.Error = err.Error()
		}
		if d.Dir != "" && d.Dir != "." {
			if d.Parent = path.Dir(d.Dir); d.Parent == "." {
				d.Parent = ""
			}
		}
	}
	v.Data = d
	s.render(w, "files", v)
}

func (s *Server) filesUpload(w http.ResponseWriter, r *http.Request, _ *view) {
	dir := r.FormValue("dir")
	to := "/files?dir=" + template.URLQueryEscaper(dir)
	ws, err := s.workspace()
	if err != nil {
		http.Redirect(w, r, to+"&e="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	src, header, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, to+"&e="+template.URLQueryEscaper("没有选文件，或者文件太大"), http.StatusSeeOther)
		return
	}
	defer src.Close()
	// 先落到临时文件，再由工作区按它的规矩收进去（校验路径、给同名的旧文件留底）。
	tmp, err := os.CreateTemp("", "atom-upload-*")
	if err != nil {
		http.Redirect(w, r, to+"&e="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	defer os.Remove(tmp.Name())
	_, copyErr := io.Copy(tmp, io.LimitReader(src, maxUpload+1))
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		http.Redirect(w, r, to+"&e="+template.URLQueryEscaper("接收文件失败"), http.StatusSeeOther)
		return
	}
	// 只取文件名本身：浏览器给的名字里不该有路径，有也不认。
	name := path.Base(strings.ReplaceAll(header.Filename, `\`, "/"))
	saved, err := ws.Import(tmp.Name(), path.Join(dir, name))
	if err != nil {
		http.Redirect(w, r, to+"&e="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, to+"&ok="+template.URLQueryEscaper("已放进工作区: "+saved.Path), http.StatusSeeOther)
}

func (s *Server) filesDownload(w http.ResponseWriter, r *http.Request, _ *view) {
	ws, err := s.workspace()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rel := r.URL.Query().Get("path")
	raw, err := ws.Bytes(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	name := path.Base(strings.ReplaceAll(rel, `\`, "/"))
	// 一律当成附件下载，不让浏览器直接打开：工作区里可能有别人发来的网页文件。
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+template.URLQueryEscaper(name))
	_, _ = w.Write(raw)
}

// Serve 监听并提供后台，直到 ctx 结束。certFile 与 keyFile 非空时提供 HTTPS。
func (s *Server) Serve(ctx context.Context, addr, certFile, keyFile string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("admin: 监听 %s 失败: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	s.opts.Log.Info("管理后台已开启", "地址", ln.Addr().String(), "https", certFile != "")
	if certFile != "" {
		err = srv.ServeTLS(ln, certFile, keyFile)
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
