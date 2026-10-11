package admin

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/bcrypt"

	"github.com/xiguac/atomhelper/internal/store/pg"
)

// 登录方式。
//
// 光有一个密码不够：密码会被撞库、被看到、被钓走。后台提供两种登录方式，用户可以开其中一种或者两种都开：
//
//   - **密码 + 动态验证码。** 先输密码，再输手机验证器上的 6 位数字。
//     校验的顺序是反过来的：先验验证码，验证码对了才去比密码。这样没有验证器的人
//     连试密码的机会都没有——拿一批泄露的密码来撞，一个也试不出对错。
//     所以密码在第一步并不校验，两步都填完才一起判定，错了也不说是哪一个错。
//   - **通行密钥。** 用设备上的指纹、面容或者硬件密钥登录。要求设备当场验证使用者本人，
//     所以它一步就相当于"持有设备 + 本人在场"两个因素。
//
// 刚装好、还一种方式都没设的时候，只能先用密码进来，而且进来之后哪里都去不了，
// 必须先在「登录与安全」里设好一种方式。设好之后，单凭密码就再也进不来了。

const (
	// authSettingKey 是登录方式存在设置表里的名字。值是加密的：里面有动态验证码的种子。
	authSettingKey = "admin.auth"
	// ticketTTL 是输完密码之后留给输验证码的时间。
	ticketTTL = 5 * time.Minute
	// ceremonyTTL 是一次通行密钥操作从发起到完成的时限。
	ceremonyTTL = 5 * time.Minute
	// maxPasskeys 是最多登记几把通行密钥。
	maxPasskeys = 10
)

// errLoginFailed 是登录失败时给用户看的话。不管错的是验证码还是密码，都是这一句。
const errLoginFailed = "验证码或密码不正确。"

// authState 是当前的登录方式设置。
type authState struct {
	// PasswordTOTP、Passkey 表示用户开了哪几种登录方式。
	PasswordTOTP bool `json:"password_totp"`
	Passkey      bool `json:"passkey"`
	// TOTP 非空表示已经绑定了验证器。
	TOTP *totpState `json:"totp,omitempty"`
	// PendingSecret 是正在绑定、还没用验证码确认过的种子。
	PendingSecret string `json:"pending_secret,omitempty"`
	// UserID 是通行密钥体系里代表"后台管理员"的随机编号。
	UserID   []byte    `json:"user_id,omitempty"`
	Passkeys []passkey `json:"passkeys,omitempty"`
}

type totpState struct {
	Secret string `json:"secret"`
	// LastStep 是上一次通过的验证码所在的时间周期，用来拒绝重放。
	LastStep int64 `json:"last_step"`
}

// passkey 是一把登记过的通行密钥。
type passkey struct {
	// ID 是凭据编号的文字形式，页面上删除时用它指认。
	ID   string `json:"id"`
	Name string `json:"name"`
	// RPID 是登记时访问后台用的域名。通行密钥和域名绑定，换一个域名访问就用不了。
	RPID       string              `json:"rp_id"`
	CreatedAt  time.Time           `json:"created_at"`
	LastUsedAt time.Time           `json:"last_used_at"`
	Credential webauthn.Credential `json:"credential"`
}

// usable 报告两种登录方式现在各自能不能用：开了，而且该绑定的东西已经绑定。
func (a *authState) usable() (passwordTOTP, passkey bool) {
	return a.PasswordTOTP && a.TOTP != nil, a.Passkey && len(a.Passkeys) > 0
}

// configured 为假表示一种能用的登录方式都没有，后台处在"刚装好"的状态。
func (a *authState) configured() bool {
	p, k := a.usable()
	return p || k
}

// ResetLogin 清掉全部登录方式的设置，回到"刚装好"的状态：下次只凭密码登录，登录后必须重新设置。
//
// 这是验证器和通行密钥都用不了时的退路，只由运维命令调用——能在服务器上运行命令的人，
// 本来就拿得到这台机器上的一切。
func ResetLogin(ctx context.Context, st *pg.Store) error {
	return st.PutSetting(ctx, authSettingKey, "")
}

func (s *Server) loadAuth(ctx context.Context) (*authState, error) {
	raw, err := s.opts.Store.Setting(ctx, authSettingKey)
	if err != nil {
		return nil, err
	}
	a := &authState{}
	if raw == "" {
		return a, nil
	}
	if err := json.Unmarshal([]byte(raw), a); err != nil {
		return nil, fmt.Errorf("admin: 登录方式的设置无法识别: %w", err)
	}
	return a, nil
}

func (s *Server) saveAuth(ctx context.Context, a *authState) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("admin: 保存登录方式失败: %w", err)
	}
	return s.opts.Store.PutSetting(ctx, authSettingKey, string(raw))
}

// updateAuth 读出设置、改动、存回去。整个过程不会和另一个请求交错。
func (s *Server) updateAuth(ctx context.Context, change func(*authState) error) error {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	a, err := s.loadAuth(ctx)
	if err != nil {
		return err
	}
	if err := change(a); err != nil {
		return err
	}
	return s.saveAuth(ctx, a)
}

// ---- 失败次数 ----

// locked 报告登录是不是正被暂时锁住。
func (s *Server) locked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures >= maxFailures && s.now().Sub(s.lockedAt) < lockFor
}

// failed 记一次登录失败。密码、验证码、通行密钥的失败算在一起。
func (s *Server) failed(r *http.Request, what string) {
	s.mu.Lock()
	s.failures++
	s.lockedAt = s.now()
	s.mu.Unlock()
	s.opts.Log.Warn("后台登录失败", "方式", what, "来自", r.RemoteAddr)
}

// ---- 会话 ----

// startSession 建立一个登录会话并写入 cookie。
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, how string) error {
	id, err1 := token()
	csrf, err2 := token()
	if err1 != nil || err2 != nil {
		return errors.New("生成会话失败")
	}
	s.mu.Lock()
	s.failures = 0
	now := s.now()
	for k, v := range s.sessions {
		if now.After(v.expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = session{csrf: csrf, expires: now.Add(sessionTTL)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		// 前面有代理替后台提供 HTTPS 时（代理会带上这个头），cookie 同样只许走加密连接。
		// 这个头是请求方说了算的，但它只会让 cookie 管得更严，冒充它占不到便宜。
		Name: cookieName, Value: id, Path: "/", HttpOnly: true,
		Secure:   s.opts.TLS || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
	})
	s.opts.Log.Info("后台登录成功", "方式", how, "来自", r.RemoteAddr)
	return nil
}

// ---- 密码 + 动态验证码 ----

// ticket 是第一步输入的密码带到第二步去的样子。
//
// 密码在第一步不校验，也不存在服务器上：把它用一把只活在这个进程里的密钥加密，
// 放进第二步页面的隐藏字段，提交验证码时一起交回来。进程重启，没用完的就全部作废。
type ticket struct {
	Password string `json:"p"`
	IssuedAt int64  `json:"t"`
}

func newTicketCipher() (cipher.AEAD, error) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Server) sealTicket(password string) string {
	raw, _ := json.Marshal(ticket{Password: password, IssuedAt: s.now().Unix()})
	nonce := make([]byte, s.ticket.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(s.ticket.Seal(nonce, nonce, raw, nil))
}

func (s *Server) openTicket(v string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || len(raw) < s.ticket.NonceSize() {
		return "", false
	}
	plain, err := s.ticket.Open(nil, raw[:s.ticket.NonceSize()], raw[s.ticket.NonceSize():], nil)
	if err != nil {
		return "", false
	}
	var t ticket
	if json.Unmarshal(plain, &t) != nil || s.now().Sub(time.Unix(t.IssuedAt, 0)) > ticketTTL {
		return "", false
	}
	return t.Password, true
}

// loginData 是登录页要用的数据。
type loginData struct {
	// Password、Passkey 表示页面上给出哪几种登录方式。
	Password bool
	Passkey  bool
	// Setup 为真表示还没有设置任何登录方式，这次只用密码。
	Setup bool
	// Ticket 非空时显示的是第二步：输入动态验证码。
	Ticket string
}

func (s *Server) loginView(r *http.Request, errText string) (*view, error) {
	a, err := s.loadAuth(r.Context())
	if err != nil {
		return nil, err
	}
	d := loginData{Setup: !a.configured()}
	d.Password, d.Passkey = a.usable()
	if d.Setup {
		d.Password = true
	}
	return &view{Error: errText, Data: d}, nil
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, errText string) {
	v, err := s.loginView(r, errText)
	if err != nil {
		s.opts.Log.Error("读取登录方式失败", "err", err)
		http.Error(w, "读取登录方式失败，详见服务日志。", http.StatusInternalServerError)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, "login", v)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.renderLogin(w, r, http.StatusOK, r.URL.Query().Get("e"))
}

// login 处理第一步：收下密码。
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.locked() {
		s.renderLogin(w, r, http.StatusTooManyRequests, "连续失败次数过多，请稍后重试。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	password := r.PostFormValue("password")
	a, err := s.loadAuth(r.Context())
	if err != nil {
		s.opts.Log.Error("读取登录方式失败", "err", err)
		http.Error(w, "读取登录方式失败，详见服务日志。", http.StatusInternalServerError)
		return
	}
	withTOTP, _ := a.usable()
	switch {
	case !a.configured():
		// 还没有设置任何登录方式：只能先凭密码进来，进来之后必须先去设置。
		s.mu.Lock()
		hash := s.hash
		s.mu.Unlock()
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
			s.failed(r, "密码")
			s.renderLogin(w, r, http.StatusUnauthorized, "密码错误。")
			return
		}
		if err := s.startSession(w, r, "密码（尚未设置登录方式）"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/security", http.StatusSeeOther)
	case withTOTP:
		// 不在这里校验密码：对不对，要等验证码过了才看。
		v, err := s.loginView(r, "")
		if err != nil {
			http.Error(w, "读取登录方式失败，详见服务日志。", http.StatusInternalServerError)
			return
		}
		d := v.Data.(loginData)
		d.Ticket = s.sealTicket(password)
		v.Data = d
		s.render(w, "login", v)
	default:
		s.renderLogin(w, r, http.StatusUnauthorized, "密码登录已停用，请使用通行密钥登录。")
	}
}

// loginCode 处理第二步：先验动态验证码，再验密码。
func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	if s.locked() {
		s.renderLogin(w, r, http.StatusTooManyRequests, "连续失败次数过多，请稍后重试。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	password, ok := s.openTicket(r.PostFormValue("ticket"))
	if !ok {
		s.renderLogin(w, r, http.StatusUnauthorized, "登录已超时，请重新输入密码。")
		return
	}
	code := r.PostFormValue("code")

	// 先验验证码。通过的验证码立刻作废，不管后面密码对不对。
	codeOK := false
	err := s.updateAuth(r.Context(), func(a *authState) error {
		if withTOTP, _ := a.usable(); !withTOTP {
			return nil
		}
		step, ok := verifyTOTP(a.TOTP.Secret, code, s.now(), a.TOTP.LastStep)
		if ok {
			a.TOTP.LastStep, codeOK = step, true
		}
		return nil
	})
	if err != nil {
		s.opts.Log.Error("核对动态验证码失败", "err", err)
		http.Error(w, "读取登录方式失败，详见服务日志。", http.StatusInternalServerError)
		return
	}
	// 再验密码。验证码不对时也照样比一遍：两种失败花的时间一样，从快慢上分不出错在哪一步。
	s.mu.Lock()
	hash := s.hash
	s.mu.Unlock()
	passwordOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	if !codeOK || !passwordOK {
		s.failed(r, "密码与动态验证码")
		// 回到第一步重新来：留在第二步的话，等于告诉对方密码已经被收下了，可以专心试验证码。
		s.renderLogin(w, r, http.StatusUnauthorized, errLoginFailed)
		return
	}
	if err := s.startSession(w, r, "密码与动态验证码"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- 登录与安全页 ----

type passkeyRow struct {
	ID, Name, RPID string
	CreatedAt      time.Time
	LastUsedAt     time.Time
	// Here 为真表示这把密钥是在当前访问的域名下登记的，现在就能用。
	Here bool
}

type securityData struct {
	Setup        bool
	PasswordTOTP bool
	Passkey      bool
	// TOTPBound 为真表示已经绑定了验证器。
	TOTPBound bool
	// Pending* 在绑定验证器的过程中有值。
	PendingSecret string
	PendingQR     template.HTML
	Passkeys      []passkeyRow
	// PasskeyHost 是通行密钥会绑定到的域名；PasskeyBlocked 非空表示当前的访问方式用不了通行密钥。
	PasskeyHost    string
	PasskeyBlocked string
}

func (s *Server) securityPage(w http.ResponseWriter, r *http.Request, v *view) {
	a, err := s.loadAuth(r.Context())
	if err != nil {
		v.Error = err.Error()
		a = &authState{}
	}
	d := securityData{Setup: !a.configured(), PasswordTOTP: a.PasswordTOTP, Passkey: a.Passkey,
		TOTPBound: a.TOTP != nil, PendingSecret: a.PendingSecret}
	if a.PendingSecret != "" {
		if d.PendingQR, err = qrSVG(totpURI(a.PendingSecret, "admin")); err != nil {
			v.Error = err.Error()
		}
	}
	host, _, hostErr := rpFor(r, s.opts.TLS)
	if hostErr != nil {
		d.PasskeyBlocked = hostErr.Error()
	}
	d.PasskeyHost = host
	for _, k := range a.Passkeys {
		d.Passkeys = append(d.Passkeys, passkeyRow{ID: k.ID, Name: k.Name, RPID: k.RPID,
			CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, Here: hostErr == nil && k.RPID == host})
	}
	v.Data = d
	s.render(w, "security", v)
}

// methodsSave 保存用户开了哪几种登录方式。
func (s *Server) methodsSave(w http.ResponseWriter, r *http.Request, _ *view) {
	wantTOTP, wantPasskey := r.PostFormValue("password_totp") != "", r.PostFormValue("passkey") != ""
	err := s.updateAuth(r.Context(), func(a *authState) error {
		switch {
		case !wantTOTP && !wantPasskey:
			return errors.New("至少要保留一种登录方式")
		case wantTOTP && a.TOTP == nil:
			return errors.New("要启用「密码 + 动态验证码」，请先绑定验证器")
		case wantPasskey && len(a.Passkeys) == 0:
			return errors.New("要启用「通行密钥」，请先登记至少一把通行密钥")
		}
		a.PasswordTOTP, a.Passkey = wantTOTP, wantPasskey
		return nil
	})
	if err == nil {
		s.opts.Log.Warn("后台登录方式已修改", "密码与动态验证码", wantTOTP, "通行密钥", wantPasskey, "来自", r.RemoteAddr)
	}
	back(w, r, "/security", "登录方式已保存。", err)
}

// totpStart 开始绑定验证器：生成一份新的种子，等用户扫码后用验证码确认。
func (s *Server) totpStart(w http.ResponseWriter, r *http.Request, _ *view) {
	err := s.updateAuth(r.Context(), func(a *authState) error {
		a.PendingSecret = newTOTPSecret()
		return nil
	})
	back(w, r, "/security", "请用验证器扫描二维码，然后输入它显示的 6 位验证码完成绑定。", err)
}

func (s *Server) totpCancel(w http.ResponseWriter, r *http.Request, _ *view) {
	err := s.updateAuth(r.Context(), func(a *authState) error {
		a.PendingSecret = ""
		return nil
	})
	back(w, r, "/security", "已取消绑定。", err)
}

// totpConfirm 用一个验证码确认验证器绑定成功。
//
// 不确认就生效的话，用户扫码时出了岔子（扫错了、没存上），下次就再也登不进来了。
func (s *Server) totpConfirm(w http.ResponseWriter, r *http.Request, _ *view) {
	code := r.PostFormValue("code")
	err := s.updateAuth(r.Context(), func(a *authState) error {
		if a.PendingSecret == "" {
			return errors.New("没有正在进行的绑定，请重新开始")
		}
		step, ok := verifyTOTP(a.PendingSecret, code, s.now(), 0)
		if !ok {
			return errors.New("验证码不正确。请确认手机时间准确，并输入验证器上当前显示的 6 位数字")
		}
		first := !a.configured()
		a.TOTP = &totpState{Secret: a.PendingSecret, LastStep: step}
		a.PendingSecret = ""
		// 第一次设置登录方式时顺手启用；已经有别的方式在用时，启用与否留给用户自己勾。
		if first || a.PasswordTOTP {
			a.PasswordTOTP = true
		}
		return nil
	})
	if err == nil {
		s.opts.Log.Warn("后台已绑定动态验证码", "来自", r.RemoteAddr)
	}
	back(w, r, "/security", "验证器已绑定。", err)
}

func (s *Server) totpRemove(w http.ResponseWriter, r *http.Request, _ *view) {
	err := s.updateAuth(r.Context(), func(a *authState) error {
		if _, withPasskey := a.usable(); !withPasskey {
			return errors.New("这是唯一可用的登录方式，不能解除。请先登记并启用通行密钥")
		}
		a.TOTP, a.PendingSecret, a.PasswordTOTP = nil, "", false
		return nil
	})
	if err == nil {
		s.opts.Log.Warn("后台已解除动态验证码绑定", "来自", r.RemoteAddr)
	}
	back(w, r, "/security", "已解除验证器绑定，「密码 + 动态验证码」登录已停用。", err)
}

// ---- 通行密钥 ----

// adminUser 是通行密钥体系里的那一个用户：后台只有一个管理员。
type adminUser struct {
	id    []byte
	creds []webauthn.Credential
}

func (u adminUser) WebAuthnID() []byte                         { return u.id }
func (u adminUser) WebAuthnName() string                       { return "admin" }
func (u adminUser) WebAuthnDisplayName() string                { return "原子助手管理员" }
func (u adminUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// userFor 给出在某个域名下可用的管理员：只带上在这个域名下登记的密钥。
func (a *authState) userFor(rpID string) adminUser {
	u := adminUser{id: a.UserID}
	for _, k := range a.Passkeys {
		if k.RPID == rpID {
			u.creds = append(u.creds, k.Credential)
		}
	}
	return u
}

// rpFor 从这次请求定出通行密钥要绑定的域名和来源。
//
// 通行密钥只认域名，不认 IP 地址。经 SSH 隧道访问时要用 localhost 而不是 127.0.0.1。
func rpFor(r *http.Request, tls bool) (rpID, origin string, err error) {
	host := r.Host
	if h, _, splitErr := net.SplitHostPort(r.Host); splitErr == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" || net.ParseIP(host) != nil {
		return host, "", errors.New("通行密钥不支持用 IP 地址访问后台。经 SSH 隧道访问时，请把地址栏里的 127.0.0.1 换成 localhost")
	}
	scheme := "http"
	if tls || r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	if scheme == "http" && host != "localhost" {
		return host, "", errors.New("通行密钥只能在 HTTPS 或 localhost 下使用")
	}
	return host, scheme + "://" + r.Host, nil
}

func (s *Server) webauthnFor(r *http.Request) (*webauthn.WebAuthn, string, error) {
	rpID, origin, err := rpFor(r, s.opts.TLS)
	if err != nil {
		return nil, "", err
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID: rpID, RPDisplayName: "原子助手", RPOrigins: []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementPreferred,
			// 要求设备当场验证使用者（指纹、面容或 PIN）：光是拿到设备还不够。
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("通行密钥初始化失败: %w", err)
	}
	return wa, rpID, nil
}

// ceremony 是一次进行到一半的通行密钥操作。
type ceremony struct {
	data    webauthn.SessionData
	name    string
	rpID    string
	expires time.Time
}

func (s *Server) putCeremony(c ceremony) (string, error) {
	id, err := token()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.ceremonies {
		if now.After(v.expires) {
			delete(s.ceremonies, k)
		}
	}
	c.expires = now.Add(ceremonyTTL)
	s.ceremonies[id] = c
	return id, nil
}

// takeCeremony 取出并作废一次操作：每次操作只能完成一次。
func (s *Server) takeCeremony(id string) (ceremony, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.ceremonies[id]
	delete(s.ceremonies, id)
	return c, ok && !s.now().After(c.expires)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "message": msg})
}

// passkeyRegisterBegin 开始登记一把通行密钥。
func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request, _ *view) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "通行密钥"
	}
	if len([]rune(name)) > 40 {
		jsonError(w, http.StatusBadRequest, "名称不能超过 40 个字")
		return
	}
	wa, rpID, err := s.webauthnFor(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	var user adminUser
	err = s.updateAuth(r.Context(), func(a *authState) error {
		if len(a.Passkeys) >= maxPasskeys {
			return fmt.Errorf("最多登记 %d 把通行密钥", maxPasskeys)
		}
		if len(a.UserID) == 0 {
			a.UserID = make([]byte, 32)
			_, _ = rand.Read(a.UserID)
		}
		user = a.userFor(rpID)
		return nil
	})
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 已经登记过的设备不重复登记。
	creation, data, err := wa.BeginRegistration(user, webauthn.WithExclusions(webauthn.Credentials(user.creds).CredentialDescriptors()))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "发起登记失败: "+err.Error())
		return
	}
	id, err := s.putCeremony(ceremony{data: *data, name: name, rpID: rpID})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "发起登记失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sid": id, "options": creation})
}

// passkeyRegisterFinish 完成登记。
func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request, _ *view) {
	c, ok := s.takeCeremony(r.URL.Query().Get("sid"))
	if !ok {
		jsonError(w, http.StatusBadRequest, "登记已超时，请重新开始")
		return
	}
	wa, rpID, err := s.webauthnFor(r)
	if err != nil || rpID != c.rpID {
		jsonError(w, http.StatusBadRequest, "访问地址在登记过程中变了，请重新开始")
		return
	}
	a, err := s.loadAuth(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cred, err := wa.FinishRegistration(a.userFor(rpID), c.data, r)
	if err != nil {
		s.opts.Log.Warn("登记通行密钥失败", "err", err, "来自", r.RemoteAddr)
		jsonError(w, http.StatusBadRequest, "登记失败：设备返回的结果没有通过校验")
		return
	}
	err = s.updateAuth(r.Context(), func(a *authState) error {
		first := !a.configured()
		a.Passkeys = append(a.Passkeys, passkey{
			ID: base64.RawURLEncoding.EncodeToString(cred.ID), Name: c.name, RPID: rpID,
			CreatedAt: s.now().UTC(), Credential: *cred,
		})
		if first || a.Passkey {
			a.Passkey = true
		}
		return nil
	})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.opts.Log.Warn("后台已登记通行密钥", "名称", c.name, "域名", rpID, "来自", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "通行密钥已登记。"})
}

func (s *Server) passkeyRemove(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PostFormValue("id")
	err := s.updateAuth(r.Context(), func(a *authState) error {
		kept := a.Passkeys[:0:0]
		for _, k := range a.Passkeys {
			if k.ID != id {
				kept = append(kept, k)
			}
		}
		if len(kept) == len(a.Passkeys) {
			return errors.New("没有这把通行密钥")
		}
		if withTOTP, _ := a.usable(); len(kept) == 0 && !withTOTP {
			return errors.New("这是唯一可用的登录方式，不能删除。请先绑定验证器并启用「密码 + 动态验证码」")
		}
		a.Passkeys = kept
		if len(kept) == 0 {
			a.Passkey = false
		}
		return nil
	})
	if err == nil {
		s.opts.Log.Warn("后台已删除通行密钥", "来自", r.RemoteAddr)
	}
	back(w, r, "/security", "通行密钥已删除。", err)
}

// passkeyLoginBegin 开始用通行密钥登录。
func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if s.locked() {
		jsonError(w, http.StatusTooManyRequests, "连续失败次数过多，请稍后重试。")
		return
	}
	wa, rpID, err := s.webauthnFor(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := s.loadAuth(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "读取登录方式失败，详见服务日志。")
		return
	}
	user := a.userFor(rpID)
	if _, withPasskey := a.usable(); !withPasskey || len(user.creds) == 0 {
		jsonError(w, http.StatusBadRequest, "当前访问地址下没有可用的通行密钥。通行密钥只能在登记它时所用的地址下使用")
		return
	}
	assertion, data, err := wa.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "发起登录失败: "+err.Error())
		return
	}
	id, err := s.putCeremony(ceremony{data: *data, rpID: rpID})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "发起登录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sid": id, "options": assertion})
}

// passkeyLoginFinish 完成通行密钥登录。
func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if s.locked() {
		jsonError(w, http.StatusTooManyRequests, "连续失败次数过多，请稍后重试。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	c, ok := s.takeCeremony(r.URL.Query().Get("sid"))
	if !ok {
		jsonError(w, http.StatusBadRequest, "登录已超时，请重新开始")
		return
	}
	wa, rpID, err := s.webauthnFor(r)
	if err != nil || rpID != c.rpID {
		jsonError(w, http.StatusBadRequest, "访问地址在登录过程中变了，请重新开始")
		return
	}
	a, err := s.loadAuth(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "读取登录方式失败，详见服务日志。")
		return
	}
	if _, withPasskey := a.usable(); !withPasskey {
		jsonError(w, http.StatusBadRequest, "通行密钥登录已停用")
		return
	}
	cred, err := wa.FinishLogin(a.userFor(rpID), c.data, r)
	if err != nil {
		s.failed(r, "通行密钥")
		jsonError(w, http.StatusUnauthorized, "通行密钥没有通过校验。")
		return
	}
	// 计数器倒退说明这把密钥可能被复制了一份：真正的那一把每用一次计数只会往上走。
	if cred.Authenticator.CloneWarning {
		s.failed(r, "通行密钥（计数异常）")
		jsonError(w, http.StatusUnauthorized, "这把通行密钥的使用计数异常，可能已被复制。请改用其他方式登录，并删除它")
		return
	}
	id := base64.RawURLEncoding.EncodeToString(cred.ID)
	if err := s.updateAuth(r.Context(), func(a *authState) error {
		for i := range a.Passkeys {
			if a.Passkeys[i].ID == id {
				a.Passkeys[i].Credential, a.Passkeys[i].LastUsedAt = *cred, s.now().UTC()
			}
		}
		return nil
	}); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.startSession(w, r, "通行密钥"); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to": "/"})
}
