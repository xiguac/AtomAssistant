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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/channel/weixin"
	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/files"
	"github.com/xiguac/atomassistant/internal/kernel"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/oplog"
	"github.com/xiguac/atomassistant/internal/policy"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/skill"
	"github.com/xiguac/atomassistant/internal/store/pg"
	"github.com/xiguac/atomassistant/internal/team"
	"github.com/xiguac/atomassistant/internal/vault"
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

// favicon 是浏览器标签页上的图标：品牌里的那个原子。
//
//go:embed static/favicon.svg
var favicon []byte

const (
	cookieName = "atom_session"
	sessionTTL = 12 * time.Hour
	// 连续输错这么多次口令之后，暂停登录一段时间。
	maxFailures = 5
	lockFor     = 5 * time.Minute
	// maxUpload 是上传文件的大小上限，与工作区的单文件上限一致。
	maxUpload = files.MaxFileBytes
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
	// Reload 在进程内按配置文件重新装一遍助手，装好了才换下旧的，不重启服务。
	// 改完配置后调用；返回时新配置已经生效，或者装不起来、旧的照常在跑。为空时只保存不载入。
	Reload func(context.Context) error
	// Status 返回当前的运行状况；非空表示助手本身没能启动（比如配置还没填完），只有后台在运行。
	Status func() string
	// ShareEnv 是服务器上 ATOM_SHARE_ADDR 设的下载入口地址。为空时下载入口在「文件」页里开关。
	ShareEnv string
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
	// searchBase 是 DeepSeek 搜索接口的地址，为空用正式的。只有测试会设它。
	searchBase string
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
		return fmt.Errorf("admin: 后台监听在 %s 上却没有配置 TLS 证书——口令和环境变量的内容会在网络上明文传输。"+
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
		"tokens":     fmtTokens,
		"masked":     func() string { return maskedSecret },
		"yuan":       func(micros int64) string { return fmt.Sprintf("¥%.2f", float64(micros)/1e6) },
		"taskTabs":   func() any { return taskTabs },
		"isSession":  isSession,
		"pct":        func(f float64) int { return int(f*100 + 0.5) },
		// 配置里的类别是字符串，记录里的是 team.Category，两种都接。
		"catName":  func(c any) string { return team.CategoryName(team.Category(fmt.Sprint(c))) },
		"tierName": func(t any) string { return team.TierName(team.Tier(fmt.Sprint(t))) },
		// 模型的思考强度与输入类型。配置里没写输入类型时按默认的文字与图片显示。
		"thinkingName": thinkingName,
		"thinkings":    func() []string { return []string{"", "off", "low", "medium", "high", "max"} },
		"inputKinds":   func() []llm.Input { return llm.Inputs },
		"inputName":    func(in any) string { return llm.InputName(llm.Input(fmt.Sprint(in))) },
		"inputsOf": func(list []string) []string {
			in, err := llm.ParseInputs(list)
			if err != nil {
				return list
			}
			out := make([]string, len(in))
			for i, x := range in {
				out[i] = string(x)
			}
			return out
		},
		// dict 把几对键值拼成一个 map，用来给子模板传几样东西。
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict 的参数要成对")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict 的键要是文字")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		"base": path.Base,
		"levelName": func(l slog.Level) string {
			switch {
			case l >= slog.LevelError:
				return "错误"
			case l >= slog.LevelWarn:
				return "警告"
			}
			return "信息"
		},
		"levelClass": func(l slog.Level) string {
			switch {
			case l >= slog.LevelError:
				return "bad"
			case l >= slog.LevelWarn:
				return "warn"
			}
			return ""
		},
		"has": func(list []string, s any) bool {
			for _, x := range list {
				if x == fmt.Sprint(s) {
					return true
				}
			}
			return false
		},
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

func thinkingName(t string) string {
	switch t {
	case "off":
		return "关闭"
	case "low":
		return "低"
	case "medium":
		return "中"
	case "high":
		return "高"
	case "max":
		return "最高"
	}
	return "服务默认"
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
	icon := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(favicon)
	}
	mux.HandleFunc("GET /static/favicon.svg", icon)
	// 浏览器不管页面怎么写都会来取这个地址一次，一并给它。
	mux.HandleFunc("GET /favicon.ico", icon)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /login/code", s.loginCode)
	mux.HandleFunc("POST /login/passkey/begin", s.passkeyLoginBegin)
	mux.HandleFunc("POST /login/passkey/finish", s.passkeyLoginFinish)

	auth := func(h func(http.ResponseWriter, *http.Request, *view)) http.HandlerFunc { return s.authed(h) }
	mux.HandleFunc("POST /logout", auth(s.logout))
	mux.HandleFunc("GET /{$}", auth(s.home))
	mux.HandleFunc("POST /tasks", auth(s.createTask))
	mux.HandleFunc("POST /tasks/batch", auth(s.batchTasks))
	mux.HandleFunc("GET /tasks", auth(s.tasksPage))
	mux.HandleFunc("GET /tasks/{id}", auth(s.taskPage))
	mux.HandleFunc("POST /tasks/{id}/approve", auth(s.approve))
	mux.HandleFunc("POST /tasks/{id}/reject", auth(s.reject))
	mux.HandleFunc("POST /tasks/{id}/cancel", auth(s.cancel))
	mux.HandleFunc("POST /tasks/{id}/budget", auth(s.raiseBudget))
	mux.HandleFunc("POST /tasks/{id}/reply", auth(s.reply))
	mux.HandleFunc("POST /models/budget", auth(s.budgetSave))
	mux.HandleFunc("GET /vault", auth(s.vaultPage))
	mux.HandleFunc("POST /vault", auth(s.vaultPut))
	mux.HandleFunc("POST /vault/delete", auth(s.vaultDelete))
	mux.HandleFunc("POST /vault/replace", auth(s.vaultReplace))
	mux.HandleFunc("POST /vault/request", auth(s.vaultRequestFill))
	mux.HandleFunc("POST /vault/request/dismiss", auth(s.vaultRequestDismiss))
	mux.HandleFunc("GET /memory", auth(s.memoryPage))
	mux.HandleFunc("POST /memory/confirm", auth(s.memoryConfirm))
	mux.HandleFunc("POST /memory/dismiss", auth(s.memoryDismiss))
	mux.HandleFunc("POST /memory/affirm", auth(s.memoryAffirm))
	mux.HandleFunc("POST /memory/forget", auth(s.memoryForget))
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
	mux.HandleFunc("POST /connectors/weather", auth(s.weatherSave))
	mux.HandleFunc("POST /connectors/search", auth(s.searchSave))
	mux.HandleFunc("POST /connectors/disable", auth(s.connectorDisable))
	mux.HandleFunc("POST /connectors/test", auth(s.connectorTest))
	mux.HandleFunc("GET /mcp", auth(s.mcpPage))
	for i, p := range mcpPresets {
		mux.HandleFunc("POST /mcp/"+p.ID, auth(s.mcpPresetSave(i)))
	}
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
	mux.HandleFunc("POST /prompts/extra", auth(s.extraSave))
	mux.HandleFunc("POST /prompts/extra/toggle", auth(s.extraToggle))
	mux.HandleFunc("POST /prompts/extra/delete", auth(s.extraDelete))
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
	mux.HandleFunc("GET /logs", auth(s.logsPage))
	mux.HandleFunc("POST /logs/delete", auth(s.deleteLogs))
	mux.HandleFunc("GET /files", auth(s.filesPage))
	mux.HandleFunc("POST /files", auth(s.filesUpload))
	mux.HandleFunc("GET /files/download", auth(s.filesDownload))
	mux.HandleFunc("POST /files/delete", auth(s.filesDelete))
	mux.HandleFunc("POST /files/share/base", auth(s.shareBaseSave))
	mux.HandleFunc("POST /files/share/listen", auth(s.shareListenSave))
	mux.HandleFunc("POST /files/share/revoke", auth(s.shareRevoke))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// 页面不引用任何外部资源，也不许被别的网页嵌进去。脚本只认自己这一个文件，不许内联：
		// 就算哪里的转义出了纰漏，混进页面的脚本也不会被执行。脚本只能向后台自己发请求。
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
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
	// Badges 是导航上各页待处理事项的数量，登录后才有。
	Badges badges
}

// badges 是需要用户处理的事项数量：等确认或等回话的任务、助手登记的待填环境变量、待核实的记忆。
type badges struct {
	Tasks, Vault, Memory int
}

// Total 是全部待处理事项的数量，手机上的「概览」标签显示它。
func (b badges) Total() int { return b.Tasks + b.Vault + b.Memory }

// countBadges 数出导航上要显示的待处理数量。查不出来的那一项不显示，不影响页面本身。
func (s *Server) countBadges(ctx context.Context) badges {
	var b badges
	if tasks, err := s.opts.Store.ListTasks(ctx, false, 60); err == nil {
		for _, t := range tasks {
			if needsUser(t) {
				b.Tasks++
			}
		}
	} else {
		s.opts.Log.Warn("统计待处理任务失败", "err", err)
	}
	if reqs, err := s.requests().List(ctx); err == nil {
		b.Vault = len(reqs)
	} else {
		s.opts.Log.Warn("统计待填写的环境变量失败", "err", err)
	}
	if pending, err := s.opts.Store.Memories().ListPending(ctx, ""); err == nil {
		b.Memory = len(pending)
	} else {
		s.opts.Log.Warn("统计待核实的记忆失败", "err", err)
	}
	return b
}

func (s *Server) render(w http.ResponseWriter, name string, v *view) {
	s.renderCtx(context.Background(), w, name, v)
}

func (s *Server) renderCtx(ctx context.Context, w http.ResponseWriter, name string, v *view) {
	t, ok := s.tmpl[name]
	if !ok {
		http.Error(w, "页面不存在", http.StatusNotFound)
		return
	}
	v.Page = name
	if v.CSRF != "" && s.opts.Store != nil {
		v.Badges = s.countBadges(ctx)
	}
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
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			got := r.FormValue("csrf")
			if subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) != 1 {
				http.Error(w, "请求已失效，请回到页面刷新后重试。", http.StatusForbidden)
				return
			}
			unmask(r)
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
		v := &view{CSRF: sess.csrf, Notice: r.URL.Query().Get("ok"), Error: r.URL.Query().Get("e")}
		if r.Method != http.MethodPost {
			h(w, r, v)
			return
		}
		// 每一次改动都记一条操作日志：做了什么、从哪来、成没成。结果由 back 填上。
		o := &outcome{}
		h(w, r.WithContext(context.WithValue(r.Context(), outcomeKey{}, o)), v)
		s.logOperation(r, o)
	}
}

// outcome 是一次后台操作的结果，由 back 填上。没经过 back 的（比如上传完直接跳转）只记做了什么。
type outcome struct {
	set bool
	ok  bool
	msg string
}

type outcomeKey struct{}

// logOperation 记下一次后台操作。只记操作和结果说明，不记提交的内容：里面可能有密码、密钥。
func (s *Server) logOperation(r *http.Request, o *outcome) {
	args := []any{"操作", opName(r.URL.Path), "地址", r.URL.Path, "来自", r.RemoteAddr}
	switch {
	case !o.set:
		s.opts.Log.Info("后台操作", args...)
	case o.ok:
		s.opts.Log.Info("后台操作", append(args, "结果", "成功", "说明", shorten(o.msg, 200))...)
	default:
		s.opts.Log.Warn("后台操作", append(args, "结果", "失败", "说明", shorten(o.msg, 200))...)
	}
}

// opNames 是后台各个改动给人看的叫法，按地址前缀认，长的在前。
var opNames = []struct{ prefix, name string }{
	{"/tasks/batch", "批量处理任务"}, {"/tasks/", "处理任务"}, {"/tasks", "创建任务"},
	{"/models/provider/delete", "删除模型服务"}, {"/models/provider", "保存模型服务"}, {"/models/main", "设置主 AI"},
	{"/models/agent/delete", "删除协作模型"}, {"/models/agent", "保存协作模型"}, {"/models/test", "测试模型连接"},
	{"/models/budget", "保存任务预算"},
	{"/vault/delete", "删除环境变量"}, {"/vault/replace", "更换环境变量的值"}, {"/vault/request/dismiss", "忽略待填写的环境变量"},
	{"/vault/request", "填写环境变量"}, {"/vault", "新增环境变量"},
	{"/memory/confirm", "确认记忆"}, {"/memory/dismiss", "否决记忆"}, {"/memory/affirm", "确认推断的记忆"}, {"/memory/forget", "删除记忆"}, {"/logs/delete", "删除日志"}, {"/files/share/listen", "开关下载入口"},
	{"/channels/weixin", "微信扫码连接"}, {"/channels/delete", "删除消息源"}, {"/channels/owner", "修改消息源主人"}, {"/channels", "保存消息源"},
	{"/connectors/disable", "停用数据源"}, {"/connectors/test", "测试数据源"}, {"/connectors/", "保存数据源"},
	{"/mcp/server/delete", "移除 MCP server"}, {"/mcp/server", "保存 MCP server"}, {"/mcp/key", "保存 MCP 密钥"},
	{"/mcp/test", "测试 MCP 连接"}, {"/mcp/sync", "读取 MCP 工具"}, {"/mcp/tools", "保存 MCP 工具授权"},
	{"/mcp/installed/delete", "移除助手安装的 MCP server"}, {"/mcp/", "接入预置的 MCP server"},
	{"/skills/delete", "移除 Skill"}, {"/skills", "安装 Skill"},
	{"/prompts/extra/delete", "删除自定义提示词"}, {"/prompts/extra/toggle", "切换自定义提示词"}, {"/prompts/extra", "保存自定义提示词"},
	{"/prompts/toggle", "切换提示词段落"}, {"/prompts", "修改提示词"},
	{"/security/", "修改登录方式"}, {"/password", "修改登录密码"}, {"/logout", "退出登录"},
	{"/files/delete", "删除工作区文件"}, {"/files", "上传文件"},
}

func opName(path string) string {
	for _, n := range opNames {
		if path == n.prefix || (strings.HasSuffix(n.prefix, "/") && strings.HasPrefix(path, n.prefix)) ||
			strings.HasPrefix(path, n.prefix+"/") {
			return n.name
		}
	}
	return "其他操作"
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
	if o, ok := r.Context().Value(outcomeKey{}).(*outcome); ok {
		o.set, o.ok, o.msg = true, err == nil, notice
		if err != nil {
			o.msg = err.Error()
		}
	}
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
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	q := sep + "ok=" + template.URLQueryEscaper(notice)
	if err != nil {
		q = sep + "e=" + template.URLQueryEscaper(err.Error())
	}
	http.Redirect(w, r, to+q, http.StatusSeeOther)
}

// ---- 任务 ----

// overviewData 是「概览」页：任务和 token 的数字、创建任务的输入框、可筛选的任务列表。
type overviewData struct {
	// Active 是正在进行的任务数，Waiting 是等用户处理的任务数。
	Active, Waiting int
	// Today、Yesterday 是今天、昨天全部模型调用的用量。
	Today, Yesterday pg.DayUsage
	// VaultRequests 与 MemoryPending 是助手登记的待填环境变量、待核实的记忆的数量。
	VaultRequests, MemoryPending int
	// Setup 是开始使用前要配好的几项，全部配好时为空。
	Setup []setupItem
	List  taskList
}

// taskList 是概览页下方的任务列表：一种筛法下的一页任务。
type taskList struct {
	Tab      string
	From, To string
	Tasks    []*core.Task
	Total    int
	Page     int
	Pages    int
	// Query 是不含页码的筛选条件，翻页和批量操作后回到同一个列表时用。已经编码过，直接拼进地址。
	Query template.URL
}

// maskedSecret 是已保存的密码、密钥在输入框里显示的样子。它和真实的值毫无关系，连长度都不透露；
// 原样交回来就表示"不改"，见 unmask。
const maskedSecret = "••••••••"

// unmask 把表单里原样交回来的掩码当作没填：各处的保存与测试连接都是"留空沿用原值"。
func unmask(r *http.Request) {
	for _, form := range []url.Values{r.PostForm, r.Form} {
		for _, vs := range form {
			for i, v := range vs {
				if v == maskedSecret {
					vs[i] = ""
				}
			}
		}
	}
	if r.MultipartForm != nil {
		for _, vs := range r.MultipartForm.Value {
			for i, v := range vs {
				if v == maskedSecret {
					vs[i] = ""
				}
			}
		}
	}
}

// fmtTokens 把 token 数写成好读的样子：一万以下照写，以上按万、亿。
func fmtTokens(n int64) string {
	switch {
	case n >= 100_000_000:
		return fmt.Sprintf("%.2f 亿", float64(n)/1e8)
	case n >= 10_000:
		return fmt.Sprintf("%.1f 万", float64(n)/1e4)
	}
	return strconv.FormatInt(n, 10)
}

// taskTabs 是任务列表顶上的标签，按显示顺序。
var taskTabs = []struct{ ID, Name string }{
	{"today", "今日"}, {"yesterday", "昨日"}, {"week", "近 7 日"},
	{"ended", "已结束"}, {"active", "进行中"}, {"all", "全部"}, {"archived", "已归档"},
}

// tasksPerPage 是任务列表每页的条数。
const tasksPerPage = 50

// setupItem 是开始使用前要配好的一项。
type setupItem struct {
	Title, Hint, Href, Action string
	Done                      bool
}

// setupItems 检查开始使用前要配好的几项。读不出配置的那一项按没配好算。
func (s *Server) setupItems(r *http.Request) []setupItem {
	_, m, err := s.readModels()
	keyed := false
	if err == nil {
		stored := map[string]bool{}
		if metas, err := s.opts.Vault.List(r.Context()); err == nil {
			for _, meta := range metas {
				stored[meta.Label] = true
			}
		}
		for _, p := range m.Providers {
			if (p.APIKey != "" && stored[strings.TrimPrefix(p.APIKey, vault.HandlePrefix)]) || (p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "") {
				keyed = true
			}
		}
	}
	mainProvider, _ := m.Main()
	owned := false
	if raw, err := os.ReadFile(s.opts.ConfigPath); err == nil {
		if chans, err := config.ReadChannels(raw); err == nil {
			for _, c := range chans {
				if len(c.Owners) > 0 {
					owned = true
				}
			}
		}
	}
	items := []setupItem{
		{Title: "添加模型服务", Hint: "填写接口地址与 API Key，助手依靠它理解与执行任务", Href: "/models", Action: "前往模型", Done: keyed},
		{Title: "设置主 AI", Hint: "指定负责规划与对话的模型", Href: "/models", Action: "前往模型", Done: mainProvider != ""},
		{Title: "接入消息源", Hint: "接入微信、QQ、飞书或钉钉，并添加你自己为主人，即可在聊天中使用", Href: "/channels", Action: "前往消息源", Done: owned},
	}
	for _, it := range items {
		if !it.Done {
			return items
		}
	}
	return nil
}

func needsUser(t *core.Task) bool {
	return t.State == core.StateSuspended ||
		(t.State == core.StateWaiting && t.Wake != nil && (t.Wake.Kind == core.WakeApproval ||
			(t.Wake.Kind == core.WakeEvent && strings.Contains(t.Wake.Expr, `"user.message"`))))
}

// home 是「概览」页：任务和 token 的数字，创建任务，以及可以筛选、批量处理的任务列表。
func (s *Server) home(w http.ResponseWriter, r *http.Request, v *view) {
	ctx := r.Context()
	d := overviewData{Setup: s.setupItems(r)}
	if active, err := s.opts.Store.ListTasks(ctx, true, 1000); err == nil {
		for _, t := range active {
			switch {
			case isSession(t):
				// 每天的会话只是个容器，不算一件在办的事。
			case needsUser(t):
				d.Waiting++
			default:
				d.Active++
			}
		}
	} else {
		v.Error = err.Error()
	}
	now := time.Now()
	var err error
	if d.Today, err = s.opts.Store.UsageOn(ctx, now); err != nil {
		v.Error = err.Error()
	}
	if d.Yesterday, err = s.opts.Store.UsageOn(ctx, now.AddDate(0, 0, -1)); err != nil {
		v.Error = err.Error()
	}
	if reqs, err := s.requests().List(ctx); err == nil {
		d.VaultRequests = len(reqs)
	} else {
		v.Error = err.Error()
	}
	if pending, err := s.opts.Store.Memories().ListPending(ctx, ""); err == nil {
		d.MemoryPending = len(pending)
	} else {
		v.Error = err.Error()
	}
	list, err := s.taskList(ctx, r.URL.Query(), now)
	if err != nil {
		v.Error = err.Error()
	}
	d.List = list
	v.Data = d
	s.renderCtx(ctx, w, "home", v)
}

// taskList 按地址里的条件查出一页任务：tab 是标签，from、to 是创建日期（含当天），p 是页码。
func (s *Server) taskList(ctx context.Context, q url.Values, now time.Time) (taskList, error) {
	l := taskList{Tab: q.Get("tab"), From: q.Get("from"), To: q.Get("to"), Page: 1}
	known := false
	for _, t := range taskTabs {
		known = known || t.ID == l.Tab
	}
	if !known {
		l.Tab = "all"
	}
	if p, err := strconv.Atoi(q.Get("p")); err == nil && p > 1 {
		l.Page = p
	}
	query := pg.TaskQuery{Status: pg.TaskStatusAll, Limit: tasksPerPage, Offset: (l.Page - 1) * tasksPerPage}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch l.Tab {
	case "today":
		query.From = day
	case "yesterday":
		query.From, query.To = day.AddDate(0, 0, -1), day
	case "week":
		query.From = day.AddDate(0, 0, -6)
	case "ended":
		query.Status = pg.TaskStatusEnded
	case "active":
		query.Status = pg.TaskStatusActive
	case "archived":
		query.Status = pg.TaskStatusArchived
	}
	// 自选的日期和标签的时间范围同时生效，取两者重叠的部分。
	var bad error
	if l.From != "" {
		if f, err := time.ParseInLocation(time.DateOnly, l.From, now.Location()); err == nil {
			if f.After(query.From) {
				query.From = f
			}
		} else {
			bad = fmt.Errorf("开始日期格式不对：%s", l.From)
			l.From = ""
		}
	}
	if l.To != "" {
		if t, err := time.ParseInLocation(time.DateOnly, l.To, now.Location()); err == nil {
			if t = t.AddDate(0, 0, 1); query.To.IsZero() || t.Before(query.To) {
				query.To = t
			}
		} else {
			bad = fmt.Errorf("结束日期格式不对：%s", l.To)
			l.To = ""
		}
	}
	vals := url.Values{}
	vals.Set("tab", l.Tab)
	if l.From != "" {
		vals.Set("from", l.From)
	}
	if l.To != "" {
		vals.Set("to", l.To)
	}
	l.Query = template.URL(vals.Encode())
	if !query.To.IsZero() && !query.To.After(query.From) {
		return l, bad
	}
	tasks, total, err := s.opts.Store.QueryTasks(ctx, query)
	if err != nil {
		return l, err
	}
	l.Tasks, l.Total = tasks, total
	l.Pages = (total + tasksPerPage - 1) / tasksPerPage
	return l, bad
}

// tasksPage 是原来的「任务」页，已经并进概览页。旧的地址跳过去。
func (s *Server) tasksPage(w http.ResponseWriter, r *http.Request, _ *view) {
	http.Redirect(w, r, "/", http.StatusSeeOther)
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

// batchTasks 对概览页任务列表里勾选的任务做同一件事：删除、归档、取消归档或取消。
//
// 删除和归档只对已结束的任务生效，取消只对还没结束的生效；不合适的那些跳过，结果里说明做了几个。
func (s *Server) batchTasks(w http.ResponseWriter, r *http.Request, _ *view) {
	ids := r.PostForm["id"]
	to := "/"
	// 回到操作前的那个列表。back 只是查询条件，带斜杠、冒号的不认，防止被拿去跳到别处。
	if q := r.PostFormValue("back"); q != "" && !strings.ContainsAny(q, `/\:`) {
		to = "/?" + q
	}
	if len(ids) == 0 {
		back(w, r, to, "", errors.New("请先勾选任务"))
		return
	}
	ctx := r.Context()
	action := r.PostFormValue("action")
	var n int
	var err error
	var msg string
	switch action {
	case "delete":
		n, err = s.opts.Store.DeleteFinishedTasks(ctx, ids)
		msg = fmt.Sprintf("已删除 %d 个已结束的任务。由这些任务提炼的记忆与评分记录不受影响。", n)
	case "archive":
		n, err = s.opts.Store.ArchiveTasks(ctx, ids, true)
		msg = fmt.Sprintf("已归档 %d 个已结束的任务，可在「已归档」中查看。", n)
	case "unarchive":
		n, err = s.opts.Store.ArchiveTasks(ctx, ids, false)
		msg = fmt.Sprintf("已取消归档 %d 个任务。", n)
	case "cancel":
		for _, id := range ids {
			_, e := kernel.Cancel(ctx, s.opts.Store, id, "用户在后台批量取消")
			switch {
			case e == nil:
				n++
			case errors.Is(e, kernel.ErrAlreadyEnded):
			default:
				err = e
			}
			if err != nil {
				break
			}
		}
		msg = fmt.Sprintf("已取消 %d 个任务。", n)
	default:
		err = fmt.Errorf("未知的操作 %q", action)
	}
	if skipped := len(ids) - n; err == nil && skipped > 0 {
		msg += fmt.Sprintf(" %d 个不适用，已跳过。", skipped)
	}
	if err == nil {
		s.opts.Log.Info("后台批量处理任务", "操作", action, "勾选", len(ids), "生效", n, "来自", r.RemoteAddr)
	}
	back(w, r, to, msg, err)
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
	// Live 为真表示任务还没结束，可以取消。
	Live bool
	// OverBudget 为真表示任务因预算用尽而挂起，页面上给出设置新上限的表单；
	// Suggest 是建议的新上限（各项为现有上限的两倍）。
	OverBudget                           bool
	SuggestTokens                        int64
	SuggestYuan, SuggestHours, UsageYuan float64
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
	if day, ok := channel.SessionDay(t.Tags); ok {
		s.sessionPage(w, r, v, t, day)
		return
	}
	steps, err := s.opts.Store.ListSteps(r.Context(), id)
	if err != nil {
		v.Error = err.Error()
	}
	d := taskData{Task: t, Asking: !t.State.Terminal(), Live: !t.State.Terminal()}
	if t.State == core.StateSuspended && strings.HasPrefix(t.Reason, "预算超限") {
		d.OverBudget = true
		d.SuggestTokens = max(t.Budget.MaxTokens*2, t.Usage.Tokens*2)
		d.SuggestYuan = float64(max(t.Budget.MaxCostCents*2, t.Usage.CostCents*2)) / 100
		d.SuggestHours = max(t.Budget.MaxWallClock*2, t.Usage.WallClock*2).Hours()
		d.UsageYuan = float64(t.Usage.CostCents) / 100
	}
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

// cancel 取消一个还没结束的任务。
func (s *Server) cancel(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PathValue("id")
	_, err := kernel.Cancel(r.Context(), s.opts.Store, id, "用户在后台取消")
	if errors.Is(err, kernel.ErrAlreadyEnded) {
		err = errors.New("任务已结束，无需取消")
	}
	if err == nil {
		s.opts.Log.Info("任务已在后台取消", "任务", id, "来自", r.RemoteAddr)
	}
	back(w, r, "/tasks/"+id, "任务已取消。", err)
}

// raiseBudget 给一个因预算用尽而挂起的任务设一个新的上限，让它接着跑。
func (s *Server) raiseBudget(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PathValue("id")
	err := func() error {
		t, err := s.opts.Store.GetTask(r.Context(), id)
		if err != nil {
			return err
		}
		// 没填的项沿用原来的上限。
		b := t.Budget
		if v := strings.TrimSpace(r.PostFormValue("max_tokens")); v != "" {
			if b.MaxTokens, err = strconv.ParseInt(v, 10, 64); err != nil || b.MaxTokens < 0 {
				return errors.New("token 上限须为非负整数")
			}
		}
		if v := strings.TrimSpace(r.PostFormValue("max_cost_yuan")); v != "" {
			yuan, err := strconv.ParseFloat(v, 64)
			if err != nil || yuan < 0 {
				return errors.New("金额上限须为非负数")
			}
			b.MaxCostCents = int64(yuan*100 + 0.5)
		}
		if v := strings.TrimSpace(r.PostFormValue("max_hours")); v != "" {
			hours, err := strconv.ParseFloat(v, 64)
			if err != nil || hours < 0 {
				return errors.New("执行时长上限须为非负数")
			}
			b.MaxWallClock = time.Duration(hours * float64(time.Hour))
		}
		err = kernel.RaiseBudget(r.Context(), s.opts.Store, id, b)
		switch {
		case errors.Is(err, kernel.ErrNotSuspended):
			return errors.New("任务不在挂起状态，无需继续")
		case errors.Is(err, kernel.ErrBudgetTooLow):
			return errors.New("新的上限须高于已用量，否则任务会立即再次挂起")
		}
		return err
	}()
	if err == nil {
		s.opts.Log.Info("任务预算已在后台调高", "任务", id, "来自", r.RemoteAddr)
	}
	back(w, r, "/tasks/"+id, "已设置新的上限，任务继续执行。", err)
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
	if d.Requests, err = s.requests().List(r.Context()); err != nil {
		v.Error = err.Error()
	}
	// 这一页只放个人信息。模型密钥、消息源凭据、MCP 密钥、数据源的密码同样加密存在这里，
	// 但它们在各自的管理页面里填写和更换，不在这一页列出。
	for _, m := range metas {
		if !m.Service() {
			d.Items = append(d.Items, m)
		}
	}
	v.Data = d
	s.render(w, "vault", v)
}

// requests 是助手登记的、待用户填写的条目。
func (s *Server) requests() *vault.Requests {
	return &vault.Requests{Store: s.opts.Store, Vault: s.opts.Vault}
}

// vaultRequestFill 填写一条助手登记的待填写条目：值由用户在这里给，授权对象按登记时写明的来。
func (s *Server) vaultRequestFill(w http.ResponseWriter, r *http.Request, _ *view) {
	label := r.PostFormValue("label")
	// 值原样存入，只去掉粘贴时常带上的结尾换行。
	value := strings.TrimRight(r.PostFormValue("value"), "\r\n")
	err := func() error {
		req, err := s.requests().Get(r.Context(), label)
		if err != nil {
			return err
		}
		if req == nil {
			return errors.New("待填写的条目不存在")
		}
		if value == "" {
			return errors.New("请填写值")
		}
		if err := s.opts.Vault.Seal(r.Context(), req.Label, value, req.Note, req.Tools); err != nil {
			return err
		}
		return s.requests().Remove(r.Context(), label)
	}()
	if err == nil {
		s.opts.Log.Info("待填写的环境变量已由用户填写", "名字", label, "来自", r.RemoteAddr)
	}
	back(w, r, "/vault", "条目「"+label+"」已保存，保存后不可查看。", err)
}

// vaultReplace 更换一个已有条目的值。用途和授权对象不变。
func (s *Server) vaultReplace(w http.ResponseWriter, r *http.Request, _ *view) {
	label := r.PostFormValue("label")
	value := strings.TrimRight(r.PostFormValue("value"), "\r\n")
	err := func() error {
		if value == "" {
			return errors.New("请填写新值")
		}
		metas, err := s.opts.Vault.List(r.Context())
		if err != nil {
			return err
		}
		for _, m := range metas {
			if m.Label == label {
				return s.opts.Vault.Seal(r.Context(), m.Label, value, m.Note, m.AllowedTools)
			}
		}
		return errors.New("条目不存在")
	}()
	if err == nil {
		s.opts.Log.Info("环境变量条目的值已更换", "名字", label, "来自", r.RemoteAddr)
	}
	back(w, r, "/vault", "条目「"+label+"」的值已更换。", err)
}

// vaultRequestDismiss 忽略一条待填写的条目。
func (s *Server) vaultRequestDismiss(w http.ResponseWriter, r *http.Request, _ *view) {
	label := r.PostFormValue("label")
	err := s.requests().Remove(r.Context(), label)
	if err == nil {
		s.opts.Log.Info("待填写的环境变量已被忽略", "名字", label, "来自", r.RemoteAddr)
	}
	back(w, r, "/vault", "已忽略待填写的条目「"+label+"」。", err)
}

type vaultData struct {
	// Requests 是助手登记的、等用户填值的条目。
	Requests []vault.Request
	// Items 是个人信息：授权给工具、由模型用句柄引用的条目。
	Items []vault.Meta
	// Tools 是可以被授权使用保险库条目的对象，供页面勾选。
	Tools []toolOption
}

// toolOption 是授权列表里的一项。
type toolOption struct {
	// Value 是写进授权名单的标识，Label 是给人看的说明。
	Value, Label string
}

// toolOptions 列出个人信息可以授权给的工具：配置里用到的工具，以及已有条目授权过的工具。
// 模型服务、消息源、MCP server、数据源这些服务只用自己的凭据，不在可选之列。
func (s *Server) toolOptions(metas []vault.Meta) []toolOption {
	var out []toolOption
	seen := map[string]bool{}
	add := func(value, label string) {
		if value != "" && !seen[value] && !vault.ServiceUser(value) {
			seen[value] = true
			out = append(out, toolOption{value, label})
		}
	}
	// 配置读不出来时只少了这部分选项，页面照常可用。
	if raw, err := os.ReadFile(s.opts.ConfigPath); err == nil {
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
		s.opts.Log.Info("环境变量条目已写入", "名字", label, "可用于", strings.Join(tools, ","))
	}
	back(w, r, "/vault", "条目「"+label+"」已保存。保存后不可查看，仅可替换或删除。", err)
}

func (s *Server) vaultDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	label := r.PostFormValue("label")
	err := s.opts.Vault.Remove(r.Context(), label)
	if err == nil {
		s.opts.Log.Info("环境变量条目已删除", "名字", label)
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

// backApplied 用于改了配置的操作：保存成功就在进程内按新配置重新载入助手，再把结果带回页面。
// 新配置装不起来时，保存的内容留着，助手仍按原来的配置运行，页面上说明原因。
func (s *Server) backApplied(w http.ResponseWriter, r *http.Request, to, notice string, err error) {
	if err == nil && s.opts.Reload != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		if rerr := s.opts.Reload(ctx); rerr != nil {
			err = fmt.Errorf("%s但新配置未能生效，助手仍按原来的配置运行：%w", notice, rerr)
		} else {
			notice += "新配置已生效。"
		}
	}
	back(w, r, to, notice, err)
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

// filesData 是「文件」页：用户发来的（聊天里发的、后台上传的）和助手生成的分开列。
type filesData struct {
	// Dir、Parent、Entries 是助手生成的文件里当前看的那一层。
	Dir     string
	Parent  string
	Entries []files.Entry
	// Received 是用户发来的文件。
	Received    []files.Entry
	ReceivedDir string
	Enabled     bool
	// Shares 是「下载链接」一节，见 shares.go。
	Shares sharesData
}

func (s *Server) filesPage(w http.ResponseWriter, r *http.Request, v *view) {
	d := filesData{Dir: strings.Trim(r.URL.Query().Get("dir"), "/"), ReceivedDir: files.ReceivedDir}
	// 「收到的文件」目录在上面单独列，不从助手生成的那一边进去。
	if d.Dir == files.ReceivedDir || strings.HasPrefix(d.Dir, files.ReceivedDir+"/") {
		d.Dir = ""
	}
	ws, err := s.workspace()
	if err != nil {
		v.Error = err.Error()
		v.Data = d
		s.render(w, "files", v)
		return
	}
	d.Enabled = true
	all, err := ws.List(d.Dir)
	if err != nil {
		v.Error = err.Error()
	}
	for _, e := range all {
		if e.Dir && e.Path == files.ReceivedDir {
			continue
		}
		d.Entries = append(d.Entries, e)
	}
	// 还没收到过文件时这个目录不存在，当作空的。
	if got, err := ws.List(files.ReceivedDir); err == nil {
		d.Received = got
	} else if !errors.Is(err, files.ErrNotFound) {
		v.Error = err.Error()
	}
	if d.Dir != "" && d.Dir != "." {
		if d.Parent = path.Dir(d.Dir); d.Parent == "." {
			d.Parent = ""
		}
	}
	if d.Shares, err = s.loadShares(r); err != nil {
		v.Error = err.Error()
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
	// 只取文件名本身：浏览器给的名字里不该有路径，有也不认。后台上传的和聊天里发来的一样，放进「收到的文件」。
	name := path.Base(strings.ReplaceAll(header.Filename, `\`, "/"))
	saved, err := ws.Import(tmp.Name(), path.Join(files.ReceivedDir, name))
	if err != nil {
		http.Redirect(w, r, to+"&e="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, to+"&ok="+template.URLQueryEscaper("文件已保存至工作区: "+saved.Path), http.StatusSeeOther)
}

// filesDelete 删掉工作区里的一个文件或空目录。
func (s *Server) filesDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	rel := r.PostFormValue("path")
	ws, err := s.workspace()
	if err == nil {
		err = ws.Delete(rel)
	}
	to := "/files?dir=" + template.URLQueryEscaper(r.PostFormValue("dir"))
	if err != nil {
		http.Redirect(w, r, to+"&e="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	s.opts.Log.Info("工作区的文件已在后台删除", "路径", rel, "来自", r.RemoteAddr)
	http.Redirect(w, r, to+"&ok="+template.URLQueryEscaper("已删除: "+rel), http.StatusSeeOther)
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

// ---- 日志 ----

// logsPerPage 是日志页一次显示多少条。
const logsPerPage = 100

type logsData struct {
	Entries    []oplog.Entry
	Categories []oplog.Category
	// Category 与 Level 是当前的筛选条件，Level 取 "" / "warn" / "error"。
	Category oplog.Category
	Level    string
	// Next 非空时还有更早的，是下一页的 before 参数。
	Next string
}

// logsPage 是「日志」页：按类别、级别看操作日志，新的在前，往前翻页。
func (s *Server) logsPage(w http.ResponseWriter, r *http.Request, v *view) {
	q := r.URL.Query()
	d := logsData{Categories: oplog.Categories, Category: oplog.Category(q.Get("cat")), Level: q.Get("level")}
	if !d.Category.Valid() {
		d.Category = ""
	}
	query := oplog.Query{Category: d.Category, Limit: logsPerPage}
	switch d.Level {
	case "warn":
		query.MinLevel = slog.LevelWarn
	case "error":
		query.MinLevel = slog.LevelError
	default:
		d.Level, query.MinLevel = "", slog.LevelInfo
	}
	if b := q.Get("before"); b != "" {
		if t, err := time.Parse(time.RFC3339Nano, b); err == nil {
			query.Before = t
		}
	}
	entries, err := s.opts.Store.ListLogs(r.Context(), query)
	if err != nil {
		v.Error = err.Error()
	}
	d.Entries = entries
	if len(entries) == logsPerPage {
		d.Next = entries[len(entries)-1].At.Format(time.RFC3339Nano)
	}
	v.Data = d
	s.renderCtx(r.Context(), w, "logs", v)
}

// deleteLogs 删掉一段日期内（含首尾两天）、当前所选类别的日志。
// 后台操作类的只是不再显示，服务器上留满 oplog.AdminRetention 才删，见 pg.DeleteLogs。
func (s *Server) deleteLogs(w http.ResponseWriter, r *http.Request, _ *view) {
	cat := oplog.Category(r.PostFormValue("cat"))
	if !cat.Valid() {
		cat = ""
	}
	to := "/logs?cat=" + url.QueryEscape(string(cat))
	now := time.Now()
	from, err1 := time.ParseInLocation(time.DateOnly, r.PostFormValue("from"), now.Location())
	until, err2 := time.ParseInLocation(time.DateOnly, r.PostFormValue("to"), now.Location())
	switch {
	case err1 != nil || err2 != nil:
		back(w, r, to, "", errors.New("请选择要删除的起止日期"))
		return
	case until.Before(from):
		back(w, r, to, "", errors.New("结束日期不能早于开始日期"))
		return
	}
	deleted, hidden, err := s.opts.Store.DeleteLogs(r.Context(), cat, from, until.AddDate(0, 0, 1))
	if err != nil {
		back(w, r, to, "", err)
		return
	}
	msg := fmt.Sprintf("已删除 %d 条日志。", deleted+hidden)
	if hidden > 0 {
		msg += fmt.Sprintf("其中 %d 条后台操作日志不再显示，服务器上仍保留至满 %d 天。", hidden, int(oplog.AdminRetention.Hours()/24))
	}
	back(w, r, to, msg, nil)
}
