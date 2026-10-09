// Package qq 把 QQ 机器人接成一个消息渠道。
//
// 用的是 QQ 开放平台的机器人接口（https://bot.q.qq.com/wiki/）：
// 用 AppID 和 AppSecret 换取访问凭证，通过 WebSocket 网关接收消息，通过 HTTP 接口发消息。
// 选 WebSocket 而不是回调地址，是因为助手跑在用户自己的服务器上，
// 不一定有能被公网访问的 HTTPS 地址；WebSocket 是我们主动连出去的，不需要。
//
// 接口的地址、字段和流程照官方 Go SDK（github.com/tencent-connect/botgo）核对过。
//
// 目前只处理单聊（C2C）：群里的发信人标识和单聊里的不是同一个，
// 没法用同一份主人名单判断是不是主人。
package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/xiguac/atomhelper/internal/channel"
)

const (
	defaultTokenURL = "https://bots.qq.com/app/getAppAccessToken"
	defaultAPIBase  = "https://api.sgroup.qq.com"
	// SandboxAPIBase 是沙箱环境的接口地址。
	SandboxAPIBase = "https://sandbox.api.sgroup.qq.com"

	// intentGroupAndC2C 订阅群聊与单聊事件（1 << 25）。
	intentGroupAndC2C = 1 << 25

	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatAck   = 11

	eventReady = "READY"
	eventC2C   = "C2C_MESSAGE_CREATE"

	// chatPrefix 是单聊会话标识的前缀，后面跟用户的 openid。
	chatPrefix = "c2c:"
	// tokenSlack 是凭证过期前多久就换新的。
	tokenSlack = 2 * time.Minute
	// recentIDs 是用来去重的最近消息数：断线恢复时网关可能把同一条消息再推一遍。
	recentIDs = 256
)

// ErrInvalid 表示配置或参数不合法。
var ErrInvalid = errors.New("qq: 参数不合法")

// Bot 是一个 QQ 机器人。
type Bot struct {
	// Label 是这个渠道在配置里的名字。
	Label string
	AppID string
	// Secret 在需要时取 AppSecret。
	Secret func(ctx context.Context) (string, error)
	// TokenURL 与 APIBase 为空时用正式环境的地址。
	TokenURL string
	APIBase  string
	HTTP     *http.Client
	Log      *slog.Logger

	mu     sync.Mutex
	token  string
	expiry time.Time
}

var _ channel.Adapter = (*Bot)(nil)

// Name 返回渠道名。
func (b *Bot) Name() string { return b.Label }

func (b *Bot) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (b *Bot) logger() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}

func (b *Bot) api() string {
	if b.APIBase != "" {
		return strings.TrimSuffix(b.APIBase, "/")
	}
	return defaultAPIBase
}

// flexInt 接受数字或写成字符串的数字：凭证接口的 expires_in 两种写法都出现过。
type flexInt int64

func (f *flexInt) UnmarshalJSON(raw []byte) error {
	s := strings.Trim(string(raw), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// accessToken 返回可用的访问凭证，快过期时换新的。
func (b *Bot) accessToken(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && time.Until(b.expiry) > tokenSlack {
		return b.token, nil
	}
	if b.AppID == "" || b.Secret == nil {
		return "", fmt.Errorf("%w: QQ 机器人缺少 app_id 或 secret", ErrInvalid)
	}
	secret, err := b.Secret(ctx)
	if err != nil {
		return "", fmt.Errorf("qq: 取 AppSecret 失败: %w", err)
	}
	body, err := json.Marshal(map[string]string{"appId": b.AppID, "clientSecret": secret})
	if err != nil {
		return "", err
	}
	endpoint := b.TokenURL
	if endpoint == "" {
		endpoint = defaultTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("qq: 获取访问凭证失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var out struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   flexInt `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		// 返回内容里不会有密钥，但可能很长，只留开头一段。
		return "", fmt.Errorf("qq: 获取访问凭证失败（AppID 或 AppSecret 不对？）: %s %s", resp.Status, snippet(raw))
	}
	b.token = out.AccessToken
	b.expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return b.token, nil
}

func snippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// call 调一次 HTTP 接口。
func (b *Bot) call(ctx context.Context, method, path string, body any, out any) error {
	token, err := b.accessToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.api()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client().Do(req)
	if err != nil {
		return fmt.Errorf("qq: 请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("qq: %s 返回 %s: %s", path, resp.Status, snippet(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("qq: %s 的返回内容无法解析: %w", path, err)
		}
	}
	return nil
}

// Send 发出一条单聊消息。
//
// QQ 把机器人的消息分成两种：回应用户刚发来的某条消息（带上那条消息的编号），和主动发起。
// 前者有时限和次数限制，后者有额度限制。任务办完可能是在用户发话很久之后，
// 那时再用旧消息的编号会被拒绝，所以带编号发不出去时，改成主动消息再试一次。
func (b *Bot) Send(ctx context.Context, m channel.Outgoing) error {
	openid, ok := strings.CutPrefix(m.Chat, chatPrefix)
	if !ok || !validID(openid) {
		return fmt.Errorf("%w: 会话标识 %q 不是 QQ 单聊", ErrInvalid, m.Chat)
	}
	if strings.TrimSpace(m.Text) == "" {
		return fmt.Errorf("%w: 消息是空的", ErrInvalid)
	}
	path := "/v2/users/" + url.PathEscape(openid) + "/messages"
	body := map[string]any{"content": m.Text, "msg_type": 0}
	if m.ReplyTo == "" {
		return b.call(ctx, http.MethodPost, path, body, nil)
	}
	body["msg_id"] = m.ReplyTo
	if m.Seq > 0 {
		body["msg_seq"] = m.Seq
	}
	first := b.call(ctx, http.MethodPost, path, body, nil)
	if first == nil {
		return nil
	}
	delete(body, "msg_id")
	delete(body, "msg_seq")
	if second := b.call(ctx, http.MethodPost, path, body, nil); second != nil {
		return fmt.Errorf("%w；改为主动消息后仍失败: %v", first, second)
	}
	return nil
}

// validID 校验一个要拼进接口路径的标识。
func validID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// payload 是网关上收发的一帧。
type payload struct {
	Op   int             `json:"op"`
	Data json.RawMessage `json:"d,omitempty"`
	Seq  uint32          `json:"s,omitempty"`
	Type string          `json:"t,omitempty"`
	ID   string          `json:"id,omitempty"`
}

// session 是一次网关连接要延续到下一次的东西：有了它，断线之后可以接着收，不丢消息。
type session struct {
	id  string
	seq uint32
}

// Listen 连接网关并持续接收单聊消息，断线自动重连，直到 ctx 结束。
func (b *Bot) Listen(ctx context.Context, handle func(channel.Message)) error {
	var sess session
	seen := newRecent(recentIDs)
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := b.connect(ctx, &sess, seen, handle)
		if ctx.Err() != nil {
			return nil
		}
		// 连上之后稳定跑过一阵再断，说明不是配置问题，从头开始退避。
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		b.logger().Warn("QQ 网关连接断开，稍后重连", "渠道", b.Label, "等待", backoff, "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
	return nil
}

// connect 建立一次网关连接并一直读到它断开。
func (b *Bot) connect(ctx context.Context, sess *session, seen *recent, handle func(channel.Message)) error {
	var gw struct {
		URL string `json:"url"`
	}
	if err := b.call(ctx, http.MethodGet, "/gateway/bot", nil, &gw); err != nil {
		return err
	}
	if gw.URL == "" {
		return fmt.Errorf("qq: 网关接口没有给出地址")
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, gw.URL, nil)
	if err != nil {
		return fmt.Errorf("qq: 连接网关失败: %w", err)
	}
	defer conn.Close()
	// ctx 结束时把连接关掉，读循环才会退出。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	var writeMu sync.Mutex
	write := func(op int, d any) error {
		raw, err := json.Marshal(d)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return conn.WriteJSON(payload{Op: op, Data: raw})
	}

	// 第一帧是 Hello，告诉我们多久发一次心跳。
	var hello payload
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("qq: 读取网关问候失败: %w", err)
	}
	var helloData struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if hello.Op != opHello || json.Unmarshal(hello.Data, &helloData) != nil || helloData.HeartbeatInterval <= 0 {
		return fmt.Errorf("qq: 网关的第一帧不是问候")
	}
	interval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond

	token, err := b.accessToken(ctx)
	if err != nil {
		return err
	}
	if sess.id != "" {
		err = write(opResume, map[string]any{"token": "QQBot " + token, "session_id": sess.id, "seq": sess.seq})
	} else {
		err = write(opIdentify, map[string]any{
			"token": "QQBot " + token, "intents": intentGroupAndC2C, "shard": []int{0, 1},
			"properties": map[string]string{"$os": "linux", "$browser": "atomhelper", "$device": "atomhelper"},
		})
	}
	if err != nil {
		return fmt.Errorf("qq: 向网关鉴权失败: %w", err)
	}

	var seqMu sync.Mutex
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				seqMu.Lock()
				seq := sess.seq
				seqMu.Unlock()
				if err := write(opHeartbeat, seq); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	for {
		// 两个心跳周期没有任何动静，就当连接已经死了。
		_ = conn.SetReadDeadline(time.Now().Add(2*interval + 10*time.Second))
		var p payload
		if err := conn.ReadJSON(&p); err != nil {
			return fmt.Errorf("qq: 网关连接中断: %w", err)
		}
		switch p.Op {
		case opDispatch:
			seqMu.Lock()
			if p.Seq > sess.seq {
				sess.seq = p.Seq
			}
			seqMu.Unlock()
			b.dispatch(p, sess, seen, handle)
		case opReconnect:
			return fmt.Errorf("qq: 网关要求重连")
		case opInvalidSession:
			// 会话接不上了，下次从头鉴权。
			seqMu.Lock()
			*sess = session{}
			seqMu.Unlock()
			return fmt.Errorf("qq: 网关不认这个会话（凭证或订阅的事件不对？）")
		case opHeartbeatAck, opHeartbeat:
		}
	}
}

// dispatch 处理网关推来的一个事件。
func (b *Bot) dispatch(p payload, sess *session, seen *recent, handle func(channel.Message)) {
	switch p.Type {
	case eventReady:
		var ready struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(p.Data, &ready) == nil {
			sess.id = ready.SessionID
		}
		b.logger().Info("QQ 机器人已上线", "渠道", b.Label)
	case eventC2C:
		var m struct {
			ID        string `json:"id"`
			Content   string `json:"content"`
			Timestamp string `json:"timestamp"`
			Author    struct {
				ID         string `json:"id"`
				UserOpenID string `json:"user_openid"`
			} `json:"author"`
		}
		if err := json.Unmarshal(p.Data, &m); err != nil {
			b.logger().Warn("QQ 消息解析失败", "渠道", b.Label, "err", err)
			return
		}
		openid := m.Author.UserOpenID
		if openid == "" {
			openid = m.Author.ID
		}
		if !validID(openid) || m.ID == "" || !seen.add(m.ID) {
			return
		}
		at, err := time.Parse(time.RFC3339, m.Timestamp)
		if err != nil {
			at = time.Now()
		}
		handle(channel.Message{Chat: chatPrefix + openid, Sender: openid, Text: m.Content, ID: m.ID, At: at})
	}
}

// recent 记住最近处理过的消息编号。
type recent struct {
	mu    sync.Mutex
	set   map[string]bool
	order []string
	max   int
}

func newRecent(max int) *recent { return &recent{set: make(map[string]bool), max: max} }

// add 记下一个编号。已经见过则返回 false。
func (r *recent) add(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.set[id] {
		return false
	}
	r.set[id] = true
	r.order = append(r.order, id)
	if len(r.order) > r.max {
		delete(r.set, r.order[0])
		r.order = r.order[1:]
	}
	return true
}
