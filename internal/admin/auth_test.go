package admin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

func TestTOTP_标准里的例子(t *testing.T) {
	// RFC 6238 附录 B 的测试向量（SHA-1，种子是 "12345678901234567890"），取 8 位结果的后 6 位。
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for at, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		got, err := totpAt(secret, at/30)
		if err != nil || got != want {
			t.Errorf("时间 %d: 期望 %s，实际 %s (%v)", at, want, got, err)
		}
	}
}

func TestTOTP_时间偏差与重放(t *testing.T) {
	secret := newTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	step := now.Unix() / 30
	code, _ := totpAt(secret, step)
	prev, _ := totpAt(secret, step-1)
	old, _ := totpAt(secret, step-2)

	if got, ok := verifyTOTP(secret, code, now, 0); !ok || got != step {
		t.Fatal("当前的验证码应当通过")
	}
	if _, ok := verifyTOTP(secret, " "+code[:3]+" "+code[3:], now, 0); !ok {
		t.Fatal("带空格的验证码应当照样能认")
	}
	if _, ok := verifyTOTP(secret, prev, now, 0); !ok {
		t.Fatal("上一个周期的验证码应当通过：手机和服务器的钟允许差半分钟")
	}
	if _, ok := verifyTOTP(secret, old, now, 0); ok {
		t.Fatal("两个周期之前的验证码不应通过")
	}
	// 用过一次就作废：同一个周期、以及更早周期的验证码都不再认。
	if _, ok := verifyTOTP(secret, code, now, step); ok {
		t.Fatal("用过的验证码不应再次通过")
	}
	if _, ok := verifyTOTP(secret, prev, now, step); ok {
		t.Fatal("比已用过的更早的验证码不应通过")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := verifyTOTP(secret, bad, now, 0); ok {
			t.Errorf("%q 不应通过", bad)
		}
	}
	if uri := totpURI(secret, "admin"); !strings.HasPrefix(uri, "otpauth://totp/") || !strings.Contains(uri, "secret="+secret) ||
		!strings.Contains(uri, "digits=6") || !strings.Contains(uri, "period=30") || strings.Contains(uri, "+") {
		t.Fatalf("扫码地址不对: %s", uri)
	}
	if svg, err := qrSVG(totpURI(secret, "admin")); err != nil || !strings.HasPrefix(string(svg), "<svg") {
		t.Fatalf("二维码生成失败: %v", err)
	}
}

func TestAuth_没设置登录方式时哪里都去不了(t *testing.T) {
	e := setup(t)
	if code, loc := e.post("/login", url.Values{"password": {password}}); code != http.StatusSeeOther || !strings.Contains(loc, "/security") {
		t.Fatalf("首次登录应跳去设置登录方式: %d %s", code, loc)
	}
	_, body := e.get("/security")
	if !strings.Contains(body, "尚未设置登录方式") {
		t.Fatal("页面应说明必须先设置登录方式")
	}
	csrf := reCSRF.FindStringSubmatch(body)[1]
	for _, path := range []string{"/", "/vault", "/memory", "/files", "/models"} {
		if code, _ := e.get(path); code != http.StatusSeeOther {
			t.Errorf("%s 在设置登录方式之前应去不了，实际 %d", path, code)
		}
	}
	if code, _ := e.post("/vault", url.Values{"csrf": {csrf}, "label": {"x"}, "value": {"y"}, "note": {"n"}}); code != http.StatusSeeOther {
		t.Fatalf("设置登录方式之前不能写环境变量: %d", code)
	}
	if metas, _ := e.vault.List(context.Background()); len(metas) != 0 {
		t.Fatal("设置登录方式之前的请求不应写入任何东西")
	}
	// 不绑定任何东西就想启用，不行。
	if _, loc := e.post("/security/methods", url.Values{"csrf": {csrf}, "password_totp": {"1"}}); !strings.Contains(loc, "请先绑定验证器") {
		t.Fatalf("没绑定验证器时不能启用: %s", loc)
	}
	if _, loc := e.post("/security/methods", url.Values{"csrf": {csrf}, "passkey": {"1"}}); !strings.Contains(loc, "请先登记") {
		t.Fatalf("没登记通行密钥时不能启用: %s", loc)
	}
	// 绑定时验证码不对，不生效。
	e.post("/security/totp/start", url.Values{"csrf": {csrf}})
	if _, loc := e.post("/security/totp/confirm", url.Values{"csrf": {csrf}, "code": {"000000"}}); !strings.Contains(loc, "验证码不正确") {
		t.Fatalf("验证码不对时绑定不应生效: %s", loc)
	}
	if code, _ := e.get("/"); code != http.StatusSeeOther {
		t.Fatal("绑定没完成，仍然应当去不了别的页面")
	}
}

func TestAuth_先验验证码再验密码(t *testing.T) {
	e := setup(t)
	e.login()

	// 设置好之后，单凭密码进不来：第一步只是收下密码，不下发会话，也不说密码对不对。
	for _, pw := range []string{password, "completely wrong"} {
		c := newClient()
		resp, err := c.PostForm(e.srv.URL+"/login", url.Values{"password": {pw}})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !reTicket.Match(body) || len(resp.Cookies()) != 0 || strings.Contains(string(body), "密码错误") {
			t.Fatalf("第一步不应校验密码，也不应下发会话: %d", resp.StatusCode)
		}
		if strings.Contains(string(body), pw) {
			t.Fatal("页面里不应出现明文密码")
		}
		if r2, _ := c.Get(e.srv.URL + "/"); r2.StatusCode != http.StatusSeeOther {
			t.Fatal("只走完第一步不算登录")
		}
	}

	// 验证码错、密码对；验证码对、密码错：两种失败给出同一句话。
	code1, body1 := e.signIn(newClient(), password, "000000")
	good := e.code()
	code2, body2 := e.signIn(newClient(), "completely wrong", good)
	if code1 != http.StatusUnauthorized || code2 != http.StatusUnauthorized ||
		!strings.Contains(body1, errLoginFailed) || !strings.Contains(body2, errLoginFailed) {
		t.Fatalf("两种失败都应被拒绝并给出同样的提示: %d %d", code1, code2)
	}
	// 失败之后回到第一步，不留在验证码那一步。
	if reTicket.MatchString(body1) || reTicket.MatchString(body2) {
		t.Fatal("失败后应回到第一步重新输入密码")
	}
	// 那个对的验证码已经用掉了：换成对的密码再用它，也进不来。
	if code, _ := e.signIn(newClient(), password, good); code != http.StatusUnauthorized {
		t.Fatalf("通过过一次的验证码不能再用: %d", code)
	}
	// 两个都对才能进。
	c := newClient()
	if code, _ := e.signIn(c, password, ""); code != http.StatusSeeOther {
		t.Fatalf("密码和验证码都对应当登录成功: %d", code)
	}
	if resp, _ := c.Get(e.srv.URL + "/"); resp.StatusCode != http.StatusOK {
		t.Fatal("登录后应能进入后台")
	}

	// 第一步之后拖得太久，要重新输密码。
	slow := newClient()
	resp, err := slow.PostForm(e.srv.URL+"/login", url.Values{"password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	ticket := reTicket.FindStringSubmatch(string(page))[1]
	e.clock = e.clock.Add(ticketTTL + time.Minute)
	resp, err = slow.PostForm(e.srv.URL+"/login/code", url.Values{"ticket": {ticket}, "code": {e.code()}})
	if err != nil {
		t.Fatal(err)
	}
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(page), "登录已超时") {
		t.Fatalf("拖得太久应要求重新输入密码: %d", resp.StatusCode)
	}
	// 伪造的、被改动过的凭条不认。
	if resp, _ := slow.PostForm(e.srv.URL+"/login/code", url.Values{"ticket": {ticket[:len(ticket)-4] + "AAAA"}, "code": {e.code()}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("被改动过的凭条不应被接受")
	}
}

func TestAuth_连续失败会锁住而且两步共用一个计数(t *testing.T) {
	e := setup(t)
	e.login()
	for i := 0; i < maxFailures; i++ {
		if code, _ := e.signIn(newClient(), password, "000000"); code != http.StatusUnauthorized {
			t.Fatalf("错的验证码应被拒绝: %d", code)
		}
	}
	if code, body := e.signIn(newClient(), password, ""); code != http.StatusTooManyRequests || !strings.Contains(body, "连续失败次数过多") {
		t.Fatalf("连续失败后应暂时锁住，对的也进不去: %d", code)
	}
	e.clock = e.clock.Add(lockFor + time.Second)
	if code, _ := e.signIn(newClient(), password, ""); code != http.StatusSeeOther {
		t.Fatalf("过了锁定时间应当恢复: %d", code)
	}
}

func TestAuth_唯一的登录方式不能去掉以及重置(t *testing.T) {
	e := setup(t)
	e.login()
	form := func(extra url.Values) url.Values {
		if extra == nil {
			extra = url.Values{}
		}
		extra.Set("csrf", e.csrf)
		return extra
	}
	if _, loc := e.post("/security/methods", form(nil)); !strings.Contains(loc, "至少要保留一种") {
		t.Fatalf("不能把登录方式全部关掉: %s", loc)
	}
	if _, loc := e.post("/security/totp/remove", form(nil)); !strings.Contains(loc, "唯一可用的登录方式") {
		t.Fatalf("唯一的登录方式不能解除: %s", loc)
	}
	if _, loc := e.post("/security/passkey/remove", form(url.Values{"id": {"没有"}})); !strings.Contains(loc, "没有这把通行密钥") {
		t.Fatalf("删除不存在的通行密钥应报错: %s", loc)
	}
	// 种子存在数据库里，是加密的。
	raw, err := e.store.Setting(context.Background(), authSettingKey)
	if err != nil || !strings.Contains(raw, e.secret) {
		t.Fatalf("登录方式应存在设置里: %v", err)
	}

	// 手机丢了：在服务器上重置，回到只凭密码登录、登录后必须重新设置。
	if err := ResetLogin(context.Background(), e.store); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.get("/"); code != http.StatusSeeOther {
		t.Fatal("重置之后，原来的会话也得先去设置登录方式")
	}
	c := newClient()
	resp, err := c.PostForm(e.srv.URL+"/login", url.Values{"password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/security") {
		t.Fatalf("重置之后应能凭密码登录并被带去设置: %d", resp.StatusCode)
	}
}

func TestAuth_通行密钥只认域名(t *testing.T) {
	for _, c := range []struct {
		host, proto string
		ok          bool
		rp          string
	}{
		{"localhost:8780", "", true, "localhost"},
		{"127.0.0.1:8780", "", false, ""},
		{"[::1]:8780", "", false, ""},
		{"admin.example.com", "https", true, "admin.example.com"},
		{"admin.example.com", "", false, ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/security", nil)
		r.Host = c.host
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		rp, origin, err := rpFor(r, false)
		if (err == nil) != c.ok || (c.ok && (rp != c.rp || !strings.HasSuffix(origin, c.host))) {
			t.Errorf("%s (%s): rp=%q origin=%q err=%v", c.host, c.proto, rp, origin, err)
		}
	}
}

// softKey 是测试里用软件模拟的一把通行密钥：一对 P-256 密钥，加上设备会做的那几件事。
type softKey struct {
	t     *testing.T
	priv  *ecdsa.PrivateKey
	id    []byte
	count uint32
	// verified 为假时模拟"设备没有验证使用者本人"（没按指纹）。
	verified bool
}

func newSoftKey(t *testing.T) *softKey {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &softKey{t: t, priv: priv, id: id, verified: true}
}

func (k *softKey) authData(rpID string, attested bool) []byte {
	hash := sha256.Sum256([]byte(rpID))
	flags := byte(0x01) // 使用者在场
	if k.verified {
		flags |= 0x04 // 使用者已验证
	}
	if attested {
		flags |= 0x40
	}
	k.count++
	out := append(hash[:], flags)
	out = binary.BigEndian.AppendUint32(out, k.count)
	if attested {
		out = append(out, make([]byte, 16)...)
		out = binary.BigEndian.AppendUint16(out, uint16(len(k.id)))
		out = append(out, k.id...)
		pub, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1,
			-2: k.priv.X.FillBytes(make([]byte, 32)), -3: k.priv.Y.FillBytes(make([]byte, 32))})
		if err != nil {
			k.t.Fatal(err)
		}
		out = append(out, pub...)
	}
	return out
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (k *softKey) clientData(kind, challenge, origin string) []byte {
	raw, _ := json.Marshal(map[string]string{"type": kind, "challenge": challenge, "origin": origin})
	return raw
}

// create 模拟设备登记：返回浏览器会交给后台的那份结果。
func (k *softKey) create(challenge, origin, rpID string) []byte {
	att, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": k.authData(rpID, true)})
	if err != nil {
		k.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": b64(k.id), "rawId": b64(k.id), "type": "public-key",
		"response": map[string]any{"attestationObject": b64(att),
			"clientDataJSON": b64(k.clientData("webauthn.create", challenge, origin))},
	})
	return body
}

// get 模拟设备登录。
func (k *softKey) get(challenge, origin, rpID string) []byte {
	auth := k.authData(rpID, false)
	cd := k.clientData("webauthn.get", challenge, origin)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, auth...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if err != nil {
		k.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": b64(k.id), "rawId": b64(k.id), "type": "public-key",
		"response": map[string]any{"authenticatorData": b64(auth), "clientDataJSON": b64(cd), "signature": b64(sig)},
	})
	return body
}

// call 以 localhost 的名义向后台发一个请求：通行密钥不认 IP 地址，而测试服务器听在 127.0.0.1 上。
func (e *env) call(c *http.Client, path, ctype string, body []byte) (int, map[string]any) {
	e.t.Helper()
	u, _ := url.Parse(e.srv.URL)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, bytes.NewReader(body))
	req.Host = "localhost:" + u.Port()
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("X-Requested-With", "fetch")
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("%s 返回的不是 JSON: %d %s", path, resp.StatusCode, raw)
	}
	return resp.StatusCode, out
}

func challengeOf(t *testing.T, out map[string]any) (sid, challenge string) {
	t.Helper()
	opts, _ := out["options"].(map[string]any)
	pk, _ := opts["publicKey"].(map[string]any)
	sid, _ = out["sid"].(string)
	challenge, _ = pk["challenge"].(string)
	if sid == "" || challenge == "" {
		t.Fatalf("没有拿到发起结果: %v", out)
	}
	return sid, challenge
}

func TestAuth_通行密钥登记与登录(t *testing.T) {
	e := setup(t)
	e.login()
	u, _ := url.Parse(e.srv.URL)
	origin, rpID := "http://localhost:"+u.Port(), "localhost"
	const form = "application/x-www-form-urlencoded"

	// 用 IP 地址访问时说清楚为什么不行。
	if code, body := e.post("/security/passkey/begin", url.Values{"csrf": {e.csrf}, "name": {"x"}}); code != http.StatusBadRequest || !strings.Contains(body, "localhost") {
		t.Fatalf("用 IP 地址访问时应拒绝并说明: %d %s", code, body)
	}

	// 登记。
	key := newSoftKey(t)
	begin := func() (string, string) {
		code, out := e.call(e.client, "/security/passkey/begin", form, []byte(url.Values{"csrf": {e.csrf}, "name": {"办公室电脑"}}.Encode()))
		if code != http.StatusOK {
			t.Fatalf("发起登记失败: %d %v", code, out)
		}
		opts := out["options"].(map[string]any)["publicKey"].(map[string]any)
		if sel := opts["authenticatorSelection"].(map[string]any); sel["userVerification"] != "required" {
			t.Fatalf("登记时应要求设备验证使用者: %v", sel)
		}
		return challengeOf(t, out)
	}
	finish := func(sid string, body []byte) (int, map[string]any) {
		return e.call(e.client, "/security/passkey/finish?sid="+sid+"&csrf="+e.csrf, "application/json", body)
	}
	// 没带防伪令牌的登记请求不认。
	sid, challenge := begin()
	bare, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/security/passkey/finish?sid="+sid, bytes.NewReader(key.create(challenge, origin, rpID)))
	bare.Host = "localhost:" + u.Port()
	bare.Header.Set("Content-Type", "application/json")
	if resp, err := e.client.Do(bare); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("没带防伪令牌的登记请求应被拒绝: %v", err)
	}
	// 来源不对（别的网站拿到的结果）不认。
	sid, challenge = begin()
	if code, out := finish(sid, key.create(challenge, "http://evil.example", rpID)); code != http.StatusBadRequest {
		t.Fatalf("来源不对的登记结果应被拒绝: %d %v", code, out)
	}
	// 设备没有验证使用者本人，不认。
	sid, challenge = begin()
	lazy := newSoftKey(t)
	lazy.verified = false
	if code, out := finish(sid, lazy.create(challenge, origin, rpID)); code != http.StatusBadRequest {
		t.Fatalf("没有验证使用者的登记结果应被拒绝: %d %v", code, out)
	}
	// 正常登记。同一次发起只能完成一次。
	sid, challenge = begin()
	body := key.create(challenge, origin, rpID)
	if code, out := finish(sid, body); code != http.StatusOK {
		t.Fatalf("登记失败: %d %v", code, out)
	}
	if code, _ := finish(sid, body); code != http.StatusBadRequest {
		t.Fatalf("同一次发起不能完成两次: %d", code)
	}
	if _, page := e.get("/security"); !strings.Contains(page, "办公室电脑") || !strings.Contains(page, "localhost") {
		t.Fatal("页面上应列出登记的通行密钥")
	}
	// 已经有别的方式在用，新登记的不会自动启用：启用与否由用户自己勾。
	a, err := e.server.loadAuth(context.Background())
	if err != nil || a.Passkey || len(a.Passkeys) != 1 {
		t.Fatalf("登记之后的状态不对: %v %+v", err, a)
	}
	if _, loc := e.post("/security/methods", url.Values{"csrf": {e.csrf}, "password_totp": {"1"}, "passkey": {"1"}}); !strings.Contains(loc, "登录方式已保存") {
		t.Fatalf("启用通行密钥失败: %s", loc)
	}

	// 登录。
	login := func(c *http.Client, k *softKey, org string) (int, map[string]any) {
		code, out := e.call(c, "/login/passkey/begin", form, nil)
		if code != http.StatusOK {
			t.Fatalf("发起登录失败: %d %v", code, out)
		}
		sid, challenge := challengeOf(t, out)
		return e.call(c, "/login/passkey/finish?sid="+sid, "application/json", k.get(challenge, org, rpID))
	}
	if code, _ := login(newClient(), newSoftKey(t), origin); code != http.StatusUnauthorized {
		t.Fatalf("没登记过的密钥不应能登录: %d", code)
	}
	if code, _ := login(newClient(), key, "http://evil.example"); code != http.StatusUnauthorized {
		t.Fatalf("别的网站拿到的结果不应能登录: %d", code)
	}
	key.verified = false
	if code, _ := login(newClient(), key, origin); code != http.StatusUnauthorized {
		t.Fatalf("设备没有验证使用者本人时不应能登录: %d", code)
	}
	key.verified = true
	c := newClient()
	if code, out := login(c, key, origin); code != http.StatusOK || out["to"] != "/" {
		t.Fatalf("通行密钥登录失败: %d %v", code, out)
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/", nil)
	req.Host = "localhost:" + u.Port()
	if resp, err := c.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("通行密钥登录后应能进入后台: %v", err)
	}
	// 计数器倒退：这把密钥可能被复制了。
	key.count = 0
	if code, out := login(newClient(), key, origin); code != http.StatusUnauthorized || !strings.Contains(out["message"].(string), "计数异常") {
		t.Fatalf("计数器倒退时应拒绝: %d %v", code, out)
	}

	// 只留通行密钥、停用密码登录之后，密码那条路走不通。
	if _, loc := e.post("/security/methods", url.Values{"csrf": {e.csrf}, "passkey": {"1"}}); !strings.Contains(loc, "登录方式已保存") {
		t.Fatalf("保存失败: %s", loc)
	}
	if code, body := e.signIn(newClient(), password, ""); code != http.StatusUnauthorized || !strings.Contains(body, "密码登录已停用") {
		t.Fatalf("停用之后密码登录应走不通: %d", code)
	}
	// 这时通行密钥是唯一的方式，删不掉。
	if _, loc := e.post("/security/passkey/remove", url.Values{"csrf": {e.csrf}, "id": {a.Passkeys[0].ID}}); !strings.Contains(loc, "唯一可用的登录方式") {
		t.Fatalf("唯一的登录方式不能删除: %s", loc)
	}
}
