package weixin

import (
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/channel"
)

// memKV 是 KV 的内存实现。
type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKV) Setting(_ context.Context, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.m[key], nil
}

func (k *memKV) PutSetting(_ context.Context, key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string]string{}
	}
	k.m[key] = value
	return nil
}

// png 是一张最小的 PNG 的开头，足够让类型探测认出它是图片。
var png = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 40)...)

// encryptECB 按微信 CDN 的办法加密：AES-128-ECB，PKCS7 填充。
func encryptECB(t *testing.T, data, key []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := 16 - len(data)%16
	for i := 0; i < pad; i++ {
		data = append(data, byte(pad))
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += 16 {
		block.Encrypt(out[i:i+16], data[i:i+16])
	}
	return out
}

func TestBot_长轮询收消息并用上下文令牌回话(t *testing.T) {
	key := []byte("0123456789abcdef")
	var mu sync.Mutex
	var polls []map[string]any
	var sent map[string]any
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/ilink/bot/getupdates":
			headers = r.Header.Clone()
			polls = append(polls, body)
			if len(polls) > 1 {
				// 第二次起没有新消息。
				_, _ = io.WriteString(w, `{"ret":0,"msgs":[],"get_updates_buf":"buf-2"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ret":0,"get_updates_buf":"buf-1","msgs":[
				{"message_id":18446744073709551615,"from_user_id":"user-1@im.wechat","message_type":1,"create_time_ms":1791866585144,
				 "context_token":"ctx-abc","item_list":[{"type":1,"text_item":{"text":"看看这张图"}},
				 {"type":2,"image_item":{"aeskey":"`+hex.EncodeToString(key)+`","media":{"encrypt_query_param":"enc param/1"}}}]},
				{"message_id":2,"from_user_id":"bot@im.bot","message_type":2,"item_list":[{"type":1,"text_item":{"text":"机器人自己发的"}}]},
				{"message_id":3,"from_user_id":"user-2","group_id":"group-1","message_type":1,"item_list":[{"type":1,"text_item":{"text":"群里的"}}]}]}`)
		case "/ilink/bot/sendmessage":
			sent = body
			_, _ = io.WriteString(w, `{"ret":0,"message_id":77}`)
		case "/download":
			if r.URL.Query().Get("encrypted_query_param") != "enc param/1" {
				http.Error(w, "bad param", http.StatusBadRequest)
				return
			}
			_, _ = w.Write(encryptECB(t, append([]byte{}, png...), key))
		default:
			_, _ = io.WriteString(w, `{"ret":0}`)
		}
	}))
	defer srv.Close()

	state := &memKV{}
	bot := &Bot{Label: "wx", BaseURL: srv.URL, CDNBase: srv.URL, State: state,
		Token: func(context.Context) (string, error) { return "bot-token-1", nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan channel.Message, 4)
	done := make(chan error, 1)
	go func() { done <- bot.Listen(ctx, func(m channel.Message) { got <- m }) }()

	var m channel.Message
	select {
	case m = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("没有收到消息")
	}
	// 只有单聊里用户发来的那一条：机器人自己发的、群里的都不要。64 位的消息编号原样留着。
	if m.Chat != "u:user-1@im.wechat" || m.Sender != "user-1@im.wechat" || m.Text != "看看这张图" || m.ID != "18446744073709551615" {
		t.Fatalf("消息不对: %+v", m)
	}
	// 图片从 CDN 取回并解密。
	if len(m.Images) != 1 || m.Images[0].Fetch == nil {
		t.Fatalf("应带着一张图片: %+v", m.Images)
	}
	pic, err := m.Images[0].Fetch(ctx)
	if err != nil || pic.MIME != "image/png" || len(pic.Data) != len(png) {
		t.Fatalf("图片应取回并解密: %s %d %v", pic.MIME, len(pic.Data), err)
	}

	// 回话：带上对方那条消息给的上下文令牌。
	if err := bot.Send(ctx, channel.Outgoing{Chat: m.Chat, Text: "好的"}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	mu.Lock()
	msg := sent["msg"].(map[string]any)
	item := msg["item_list"].([]any)[0].(map[string]any)
	if msg["to_user_id"] != "user-1@im.wechat" || msg["context_token"] != "ctx-abc" || msg["message_type"] != float64(2) ||
		item["text_item"].(map[string]any)["text"] != "好的" || !strings.HasPrefix(msg["client_id"].(string), "atomhelper-") {
		t.Fatalf("发出去的消息不对: %v", msg)
	}
	if info := sent["base_info"].(map[string]any); info["bot_agent"] != botAgent || info["channel_version"] != protocolVersion {
		t.Fatalf("请求里应如实报告客户端: %v", info)
	}
	// 请求头：凭证、鉴权方式、应用标识和版本号。
	if headers.Get("Authorization") != "Bearer bot-token-1" || headers.Get("AuthorizationType") != "ilink_bot_token" ||
		headers.Get("iLink-App-Id") != "bot" || headers.Get("iLink-App-ClientVersion") != "132106" || headers.Get("X-WECHAT-UIN") == "" {
		t.Fatalf("请求头不对: %v", headers)
	}
	mu.Unlock()

	// 第二次长轮询带着上一次给的游标；游标存了下来，重启之后接着用。
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(polls)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if len(polls) < 2 || polls[0]["get_updates_buf"] != "" || polls[1]["get_updates_buf"] != "buf-1" {
		t.Fatalf("长轮询应带着上一次的游标: %v", polls)
	}
	mu.Unlock()
	if v, _ := state.Setting(ctx, "weixin.wx.buf"); v == "" {
		t.Fatal("游标应存下来")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("停止时不应报错: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen 没有随 ctx 结束")
	}

	// 发不了的：不是微信单聊的会话、文件。
	if err := bot.Send(context.Background(), channel.Outgoing{Chat: "c2c:x", Text: "hi"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("别的渠道的会话应被拒绝: %v", err)
	}
	if err := bot.Send(context.Background(), channel.Outgoing{Chat: "u:x", File: &channel.File{Name: "a.txt"}}); err == nil {
		t.Fatal("发文件暂不支持，应报错")
	}
}

func TestSend_凭证失效时说明要重新扫码(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ret":-14,"errmsg":"session timeout"}`)
	}))
	defer srv.Close()
	bot := &Bot{Label: "wx", BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "t", nil }}
	if err := bot.Send(context.Background(), channel.Outgoing{Chat: "u:user-1", Text: "hi"}); !errors.Is(err, ErrLoggedOut) {
		t.Fatalf("凭证失效应说明要重新登录: %v", err)
	}
}

func TestAESKey_两处来源三种写法(t *testing.T) {
	raw := []byte("0123456789abcdef")
	for name, c := range map[string]struct{ hexKey, b64Key string }{
		"图片项里的十六进制":       {hex.EncodeToString(raw), ""},
		"媒体引用里直接是 16 个字节": {"", base64.StdEncoding.EncodeToString(raw)},
		"媒体引用里是十六进制字符":    {"", base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(raw)))},
		"两处都有时用图片项里的那个":   {hex.EncodeToString(raw), base64.StdEncoding.EncodeToString([]byte("another-key-0000"))},
	} {
		key, err := aesKey(c.hexKey, c.b64Key)
		if err != nil || string(key) != string(raw) {
			t.Errorf("%s: %x %v", name, key, err)
		}
	}
	if key, err := aesKey("", ""); err != nil || key != nil {
		t.Fatalf("没有密钥表示图片没加密: %x %v", key, err)
	}
	if _, err := aesKey("zz", ""); err == nil {
		t.Fatal("格式不对的密钥应报错")
	}
	// 密文被改过、密钥不对：解出来的填充对不上，报错而不是给出一堆乱码。
	if _, err := decryptECB(encryptECB(t, []byte("hello"), raw), []byte("fedcba9876543210")); err == nil {
		t.Fatal("密钥不对应解密失败")
	}
}

func TestLogin_扫码登录的各个阶段(t *testing.T) {
	var step int
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			if r.URL.Query().Get("bot_type") != "3" || r.Header.Get("iLink-App-Id") != "bot" || r.Header.Get("Authorization") != "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"qrcode":"qr-123","qrcode_img_content":"https://liteapp.weixin.qq.com/q/abc"}`)
		case "/ilink/bot/get_qrcode_status":
			queries = append(queries, r.URL.RawQuery)
			step++
			replies := []string{
				`{"status":"wait"}`, `{"status":"scaned"}`, `{"status":"need_verifycode"}`,
				`{"status":"confirmed","bot_token":"bot-token-9","ilink_bot_id":"bot9@im.bot","baseurl":"https://sz.ilink.example","ilink_user_id":"me@im.wechat"}`,
			}
			_, _ = io.WriteString(w, replies[step-1])
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	login, err := StartLogin(ctx, srv.Client(), srv.URL)
	if err != nil || login.QRContent != "https://liteapp.weixin.qq.com/q/abc" {
		t.Fatalf("申请二维码失败: %+v %v", login, err)
	}
	for i, want := range []string{StatusWait, StatusScanned, StatusNeedCode} {
		res, err := login.Poll(ctx, "")
		if err != nil || res.Status != want {
			t.Fatalf("第 %d 次应为 %s: %+v %v", i+1, want, res, err)
		}
	}
	// 填了手机上显示的数字之后登录成功：拿到凭证、机器人标识、接口地址和扫码的人。
	res, err := login.Poll(ctx, "42")
	if err != nil || res.Status != StatusConfirmed || res.Token != "bot-token-9" || res.BotID != "bot9@im.bot" ||
		res.BaseURL != "https://sz.ilink.example" || res.UserID != "me@im.wechat" {
		t.Fatalf("登录结果不对: %+v %v", res, err)
	}
	if queries[0] != "qrcode=qr-123" || queries[3] != "qrcode=qr-123&verify_code=42" {
		t.Fatalf("查状态的请求不对: %v", queries)
	}
}
