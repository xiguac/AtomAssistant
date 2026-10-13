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
	"crypto/cipher"
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
	"github.com/xiguac/atomhelper/internal/channel/weixin"
	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/files"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/skill"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/team"
	"github.com/xiguac/atomhelper/internal/vault"
)

//go:embed templates/*.html
var templateFS embed.FS

// appJS 是后台各页共用的一段脚本，见文件开头的说明。
//
//go:embed static/app.js
var appJS []byte

// passkeyJS 只在登录页和「登录与安全」页加载：通行密钥必须由浏览器里的脚本向设备发起。
//
//go:embed static/passkey.js
var passkeyJS []byte

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
	// Box 是这台机器的数据密钥，读写工作区里加密存放的文件要用。
	Box *seal.Box
	// Scanner 是安装 Skill 之前的扫描器。为空时后台装不了 Skill：没扫过的不装。
	Scanner skill.Checker
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
	// ceremonies 是进行到一半的通行密钥操作。
	ceremonies map[string]ceremony
	// wxLogins 是进行到一半的微信扫码登录，按渠道名存。
	wxLogins map[string]*weixin.Login
	// wxBase 是微信后端的地址，为空用正式的。只有测试会设它。
	wxBase string
	// hash 是当前生效的密码摘要：后台改过的优先，否则是环境变量里的。
	hash     string
	failures int
	lockedAt time.Time

	// authMu 保证登录方式的"读出—改动—存回"不会和另一个请求交错。
	authMu sync.Mutex
	// ticket 用来加密两步登录之间暂存的密码，密钥只活在这个进程里。
	ticket cipher.AEAD
	now    func() time.Time
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
	s := &Server{opts: opts, sessions: make(map[string]session), ceremonies: make(map[string]ceremony),
		tmpl: make(map[string]*template.Template), now: time.Now}
	if err := s.loadPassword(); err != nil {
		return nil, err
	}
	var err error
	if s.ticket, err = newTicketCipher(); err != nil {
		return nil, fmt.Errorf("admin: 初始化登录失败: %w", err)
	}
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
		"pct":        func(f float64) int { return int(f*100 + 0.5) },
		// 配置里的类别是字符串，记录里的是 team.Category，两种都接。
		"catName": func(c any) string { return team.CategoryName(team.Category(fmt.Sprint(c))) },
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
		return "失败"
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
		return "待确认"
	}
	return "待回复"
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
	// 脚本是固定的内容，里面没有任何数据，不登录也能取：登录页同样引用它。
	mux.HandleFunc("GET /static/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(appJS)
	})
	mux.HandleFunc("GET /static/passkey.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(passkeyJS)
	})
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /login/code", s.loginCode)
	mux.HandleFunc("POST /login/passkey/begin", s.passkeyLoginBegin)
	mux.HandleFunc("POST /login/passkey/finish", s.passkeyLoginFinish)

	auth := func(h func(http.ResponseWriter, *http.Request, *view)) http.HandlerFunc { return s.authed(h) }
	mux.HandleFunc("POST /logout", auth(s.logout))
	mux.HandleFunc("GET /{$}", auth(s.home))
	mux.HandleFunc("POST /tasks", auth(s.createTask))
	mux.HandleFunc("POST /tasks/delete", auth(s.deleteTasks))
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
	mux.HandleFunc("POST /memory/affirm", auth(s.memoryAffirm))
	mux.HandleFunc("POST /memory/forget", auth(s.memoryForget))
	mux.HandleFunc("GET /config", auth(s.configPage))
	mux.HandleFunc("POST /config", auth(s.configSave))
	mux.HandleFunc("POST /restart", auth(s.restart))
	mux.HandleFunc("GET /channels", auth(s.channelsPage))
	mux.HandleFunc("POST /channels", auth(s.channelSave))
	mux.HandleFunc("POST /channels/delete", auth(s.channelDelete))
	mux.HandleFunc("POST /channels/owner", auth(s.channelOwner))
	mux.HandleFunc("POST /channels/weixin/start", auth(s.weixinStart))
	mux.HandleFunc("GET /channels/weixin", auth(s.weixinPage))
	mux.HandleFunc("POST /channels/weixin/poll", auth(s.weixinPoll))
	mux.HandleFunc("GET /connectors", auth(s.connectorsPage))
	mux.HandleFunc("POST /connectors/calendar", auth(s.calendarSave))
	mux.HandleFunc("POST /connectors/mail", auth(s.mailSave))
	mux.HandleFunc("POST /connectors/holidays", auth(s.holidaysSave))
	mux.HandleFunc("POST /connectors/disable", auth(s.connectorDisable))
	mux.HandleFunc("POST /connectors/test", auth(s.connectorTest))
	mux.HandleFunc("GET /mcp", auth(s.mcpPage))
	mux.HandleFunc("POST /mcp/amap", auth(s.mcpAmap))
	mux.HandleFunc("POST /mcp/server", auth(s.mcpSave))
	mux.HandleFunc("POST /mcp/server/delete", auth(s.mcpDelete))
	mux.HandleFunc("POST /mcp/key", auth(s.mcpKey))
	mux.HandleFunc("POST /mcp/test", auth(s.mcpTest))
	mux.HandleFunc("POST /mcp/sync", auth(s.mcpSync))
	mux.HandleFunc("POST /mcp/tools", auth(s.mcpTools))
	mux.HandleFunc("POST /mcp/installed/delete", auth(s.mcpInstalledDelete))
	mux.HandleFunc("GET /skills", auth(s.skillsPage))
	mux.HandleFunc("POST /skills", auth(s.skillInstall))
	mux.HandleFunc("POST /skills/delete", auth(s.skillDelete))
	mux.HandleFunc("GET /prompts", auth(s.promptsPage))
	mux.HandleFunc("POST /prompts", auth(s.promptSave))
	mux.HandleFunc("POST /prompts/toggle", auth(s.promptToggle))
	mux.HandleFunc("GET /security", auth(s.securityPage))
	mux.HandleFunc("POST /security/methods", auth(s.methodsSave))
	mux.HandleFunc("POST /security/totp/start", auth(s.totpStart))
	mux.HandleFunc("POST /security/totp/confirm", auth(s.totpConfirm))
	mux.HandleFunc("POST /security/totp/cancel", auth(s.totpCancel))
	mux.HandleFunc("POST /security/totp/remove", auth(s.totpRemove))
	mux.HandleFunc("POST /security/passkey/begin", auth(s.passkeyRegisterBegin))
	mux.HandleFunc("POST /security/passkey/finish", auth(s.passkeyRegisterFinish))
	mux.HandleFunc("POST /security/passkey/remove", auth(s.passkeyRemove))
	mux.HandleFunc("GET /password", auth(s.passwordPage))
	mux.HandleFunc("POST /password", auth(s.passwordChange))
	mux.HandleFunc("GET /models", auth(s.modelsPage))
	mux.HandleFunc("POST /models/provider", auth(s.providerSave))
	mux.HandleFunc("POST /models/provider/delete", auth(s.providerDelete))
	mux.HandleFunc("POST /models/main", auth(s.mainSave))
	mux.HandleFunc("POST /models/agent", auth(s.agentSave))
	mux.HandleFunc("POST /models/agent/delete", auth(s.agentDelete))
	mux.HandleFunc("POST /models/test", auth(s.modelTest))
	mux.HandleFunc("POST /tasks/{id}/rate", auth(s.rateWork))
	mux.HandleFunc("GET /files", auth(s.filesPage))
	mux.HandleFunc("POST /files", auth(s.filesUpload))
	mux.HandleFunc("GET /files/download", auth(s.filesDownload))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// 页面不引用任何外部资源，也不许被别的网页嵌进去。脚本只认自己这一个文件，不许内联：
		// 就算哪里的转义出了纰漏，混进页面的脚本也不会被执行。脚本只能向后台自己发请求。
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
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
		// 还没有设置任何登录方式时，凭密码进来的人哪里都去不了，只能先去把登录方式设好。
		if !setupExempt(r.URL.Path) {
			a, err := s.loadAuth(r.Context())
			if err != nil {
				s.opts.Log.Error("读取登录方式失败", "err", err)
				http.Error(w, "读取登录方式失败，详见服务日志。", http.StatusInternalServerError)
				return
			}
			if !a.configured() {
				http.Redirect(w, r, "/security", http.StatusSeeOther)
				return
			}
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

// setupExempt 报告一个地址在"还没设置登录方式"时是否仍然可以访问：设置登录方式本身，以及退出。
func setupExempt(p string) bool {
	return p == "/security" || strings.HasPrefix(p, "/security/") || p == "/password" || p == "/logout"
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
	// 页面上的脚本在后台提交时要的是结果本身，不是跳转。
	if r.Header.Get("X-Requested-With") == "fetch" {
		w.Header().Set("Content-Type", "application/json")
		out := map[string]any{"ok": err == nil, "message": notice}
		if err != nil {
			out["message"] = err.Error()
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
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
		back(w, r, "/", "", errors.New("请填写任务内容"))
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

// deleteTasks 批量删除已经结束的任务。
//
// 删掉的是任务自己的记录。任务办完时提炼出的记忆、流程和评分早已另外存下，不受影响——
// 每个任务办完就把该留的东西落了盘，之后任务本身留不留都无所谓。
func (s *Server) deleteTasks(w http.ResponseWriter, r *http.Request, _ *view) {
	ids := r.PostForm["id"]
	all := r.PostFormValue("all") != ""
	n, err := 0, error(nil)
	switch {
	case all:
		n, err = s.opts.Store.DeleteFinishedTasks(r.Context(), nil)
	case len(ids) == 0:
		err = errors.New("请先勾选要删除的任务")
	default:
		n, err = s.opts.Store.DeleteFinishedTasks(r.Context(), ids)
	}
	if err == nil {
		s.opts.Log.Info("已结束的任务已在后台删除", "数量", n, "全部", all, "来自", r.RemoteAddr)
	}
	back(w, r, "/", fmt.Sprintf("已删除 %d 个已结束的任务。由这些任务提炼的记忆与评分记录不受影响。", n), err)
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
	// Works 是这个任务里派给协作模型的活，等用户评价。
	Works []workView
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
		http.Error(w, "任务不存在", http.StatusNotFound)
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
	if d.Works, err = s.worksOf(r.Context(), t, steps); err != nil {
		v.Error = err.Error()
	}
	v.Data = d
	s.render(w, "task", v)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PathValue("id")
	back(w, r, "/tasks/"+id, "已批准，任务将继续执行。", s.opts.Tasks.Approve(r.Context(), id))
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PathValue("id")
	back(w, r, "/tasks/"+id, "已驳回，助手将按修改意见重新处理。", s.opts.Tasks.Reject(r.Context(), id, strings.TrimSpace(r.PostFormValue("reason"))))
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PathValue("id")
	text := strings.TrimSpace(r.PostFormValue("text"))
	if text == "" {
		back(w, r, "/tasks/"+id, "", errors.New("请填写回复内容"))
		return
	}
	back(w, r, "/tasks/"+id, "已发送。", s.opts.Tasks.Reply(r.Context(), id, text, "admin"))
}

// ---- 保险库 ----

func (s *Server) vaultPage(w http.ResponseWriter, r *http.Request, v *view) {
	metas, err := s.opts.Vault.List(r.Context())
	if err != nil {
		v.Error = err.Error()
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Label < metas[j].Label })
	d := vaultData{Tools: s.toolOptions(metas)}
	for _, m := range metas {
		if m.Service() {
			d.Services = append(d.Services, serviceItem{Meta: m, Owners: owners(m.AllowedTools)})
		} else {
			d.Items = append(d.Items, m)
		}
	}
	v.Data = d
	s.render(w, "vault", v)
}

// serviceItem 是保险库总览里的一条服务凭据。
type serviceItem struct {
	vault.Meta
	Owners []owner
}

// owner 是一条服务凭据属于哪个配置对象，以及在哪个页面管理它。
type owner struct {
	Kind, Name, Page string
}

// owners 把授权名单里的服务名字翻成给人看的归属。
func owners(users []string) []owner {
	kinds := []struct{ prefix, kind, page string }{
		{"provider.", "模型服务", "/models"},
		{"channel.", "消息渠道", "/channels"},
		{"mcp.", "MCP server", "/mcp"},
	}
	out := make([]owner, 0, len(users))
	for _, u := range users {
		switch u {
		case config.VaultUserCalendar:
			out = append(out, owner{"连接器", "日历", "/connectors"})
			continue
		case config.VaultUserMail:
			out = append(out, owner{"连接器", "邮件", "/connectors"})
			continue
		}
		for _, k := range kinds {
			if name, ok := strings.CutPrefix(u, k.prefix); ok {
				out = append(out, owner{k.kind, name, k.page})
			}
		}
	}
	return out
}

type vaultData struct {
	// Items 是个人信息：授权给工具、由模型用句柄引用的条目。
	Items []vault.Meta
	// Services 是服务凭据：在各自的管理页面里填写、由系统连接时取用的条目。
	Services []serviceItem
	// Tools 是可以被授权使用保险库条目的对象，供页面勾选。
	Tools []toolOption
}

// toolOption 是授权列表里的一项。
type toolOption struct {
	// Value 是写进授权名单的标识，Label 是给人看的说明。
	Value, Label string
}

// toolOptions 列出当前配置里所有可以被授权的对象：模型服务、连接器、消息渠道、已授权的 MCP 工具。
//
// 做成勾选而不是手写：标识写错一个字符，授权就悄悄落空，调用时才报错。
// 已有条目上出现过、但配置里已经没有的标识也列出来，免得替换条目时把它丢掉。
func (s *Server) toolOptions(metas []vault.Meta) []toolOption {
	out := []toolOption{
		{config.VaultUserCalendar, "日历连接器"},
		{config.VaultUserMail, "邮件连接器"},
	}
	seen := map[string]bool{config.VaultUserCalendar: true, config.VaultUserMail: true}
	add := func(value, label string) {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, toolOption{value, label})
		}
	}
	// 配置读不出来时只少了这部分选项，页面照常可用。
	if raw, err := os.ReadFile(s.opts.ConfigPath); err == nil {
		if m, err := config.ReadModels(raw); err == nil {
			for _, p := range m.Providers {
				add(p.VaultUser(), "模型服务 "+p.Name)
			}
		}
		if list, err := config.ReadChannels(raw); err == nil {
			for _, c := range list {
				add(c.VaultUser(), "消息渠道 "+c.Name)
			}
		}
		if t, err := config.ReadToolset(raw); err == nil {
			for _, m := range t.MCP {
				add(m.VaultUser(), "MCP server "+m.Name)
			}
		}
		for _, name := range config.ReadToolNames(raw) {
			add(name, "工具 "+name)
		}
	}
	for _, m := range metas {
		for _, t := range m.AllowedTools {
			add(t, t)
		}
	}
	return out
}

func (s *Server) vaultPut(w http.ResponseWriter, r *http.Request, _ *view) {
	var tools []string
	// 页面上是逐项勾选的，每项一个值；一个值里用逗号分隔多个的写法也认。
	for _, t := range strings.FieldsFunc(strings.Join(r.PostForm["tools"], ","), func(c rune) bool { return c == ',' || c == '，' || c == ' ' || c == '\n' || c == '\r' }) {
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
	back(w, r, "/vault", "条目「"+label+"」已保存。保存后不可查看，仅可替换或删除。", err)
}

func (s *Server) vaultDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	label := r.PostFormValue("label")
	err := s.opts.Vault.Remove(r.Context(), label)
	if err == nil {
		s.opts.Log.Info("保险库条目已删除", "名字", label)
	}
	back(w, r, "/vault", "条目「"+label+"」已删除。", err)
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
	back(w, r, "/memory", "已确认，该记忆已生效。", err)
}

func (s *Server) memoryDismiss(w http.ResponseWriter, r *http.Request, _ *view) {
	back(w, r, "/memory", "已否决。", memory.NewPipeline(s.opts.Store.Memories(), nil).Dismiss(r.Context(), r.PostFormValue("id")))
}

func (s *Server) memoryAffirm(w http.ResponseWriter, r *http.Request, _ *view) {
	back(w, r, "/memory", "已确认，该记忆的来源已更新为用户陈述。",
		memory.NewPipeline(s.opts.Store.Memories(), nil).Affirm(r.Context(), r.PostFormValue("id")))
}

func (s *Server) memoryForget(w http.ResponseWriter, r *http.Request, _ *view) {
	back(w, r, "/memory", "已删除，该记忆不再被使用。",
		memory.NewPipeline(s.opts.Store.Memories(), nil).Forget(r.Context(), r.PostFormValue("id")))
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

// writeConfig 校验并写入配置文件：先写到旁边的临时文件里校验，通过了才替换，旧的那份留一个备份。
func (s *Server) writeConfig(text string) error {
	if s.opts.ConfigPath == "" {
		return errors.New("没有设置 ATOM_CONFIG，不知道配置文件该存在哪")
	}
	dir := filepath.Dir(s.opts.ConfigPath)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("写不了配置目录: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.WriteString(tmp, text); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := config.LoadFile(tmp.Name()); err != nil {
		return fmt.Errorf("配置校验未通过，未保存: %w", err)
	}
	// 旧的那份留一个备份，改错了能找回来。
	if old, err := os.ReadFile(s.opts.ConfigPath); err == nil {
		if err := os.WriteFile(s.opts.ConfigPath+".bak", old, 0o600); err != nil {
			return fmt.Errorf("备份旧配置失败: %w", err)
		}
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.opts.ConfigPath); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	return nil
}

// configSave 校验并保存配置。校验不过的一个字都不会写进去。
func (s *Server) configSave(w http.ResponseWriter, r *http.Request, _ *view) {
	text := strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n")
	if len(text) > maxConfigBytes {
		back(w, r, "/config", "", errors.New("配置内容超出长度限制"))
		return
	}
	if err := s.writeConfig(text); err != nil {
		back(w, r, "/config", "", err)
		return
	}
	s.opts.Log.Info("配置已在后台修改", "来自", r.RemoteAddr)
	back(w, r, "/config", "配置已保存，重启服务后生效。", nil)
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request, v *view) {
	if s.opts.Restart == nil {
		back(w, r, "/config", "", errors.New("当前进程未由进程管理器托管，请手动重启"))
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
		return nil, errors.New("配置文件不存在")
	}
	ws, err := file.Workspace(s.opts.Box)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, errors.New("配置中缺少 files 一节，文件处理功能尚未启用")
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
		http.Redirect(w, r, to+"&e="+template.URLQueryEscaper("未选择文件，或文件超出大小限制"), http.StatusSeeOther)
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
	http.Redirect(w, r, to+"&ok="+template.URLQueryEscaper("文件已保存至工作区: "+saved.Path), http.StatusSeeOther)
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
