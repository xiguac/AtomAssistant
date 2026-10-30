// Package weixin 把微信接成一个消息渠道。
//
// 走的是腾讯的微信机器人后端（iLink）：用微信扫码登录拿到一个机器人凭证，之后用 HTTP 长轮询收消息、
// 用 HTTP 接口发消息。和别的渠道一样，连接是我们主动发起的，不需要公网可达的回调地址。
//
// 这套接口没有单独的公开文档。地址、请求头、字段和流程照腾讯发布的开源客户端
// （npm 包 @tencent-weixin/openclaw-weixin 2.4.10，MIT 许可）的源码核对过；那个客户端是给 OpenClaw 用的，
// 这里是按同样的协议另写的一份。请求里的 bot_agent 如实写的是原子助手。
// 腾讯改了协议的话这里要跟着改，见 protocolVersion。
//
// 目前只处理单聊，能收文字和图片，能发文字；发文件暂不支持。
package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/channel"
)

// ErrInvalid 表示配置或参数不合法。
var ErrInvalid = errors.New("weixin: 参数不合法")

// ErrLoggedOut 表示机器人凭证已经失效，要重新扫码登录。
var ErrLoggedOut = errors.New("weixin: 登录已失效，请在管理后台重新扫码连接")

const (
	// DefaultBaseURL 是接口的默认地址。登录成功时服务端可能另给一个，以那个为准。
	DefaultBaseURL = "https://ilinkai.weixin.qq.com"
	// defaultCDNBase 是图片等媒体文件的下载地址。
	defaultCDNBase = "https://novac2c.cdn.weixin.qq.com/c2c"

	// protocolVersion 是这份实现所对照的官方客户端版本，随每个请求报给服务端。
	protocolVersion = "2.4.10"
	// appID 是官方客户端在请求头里带的应用标识，对所有使用者都是这个固定值。
	appID = "bot"
	// botAgent 说明发请求的是谁，只用于服务端的观测，不参与鉴权。
	botAgent = "AtomAssistant/1.0"

	// chatPrefix 是单聊会话标识的前缀，后面跟对方的 iLink 用户标识。
	chatPrefix = "u:"
	// longPoll 是一次长轮询等多久。服务端没有消息时会一直挂着，到时间才返回。
	longPoll = 35 * time.Second
	// staleToken 是凭证失效时服务端给的错误码。
	staleToken = -14
	// pauseOnStale 是凭证失效后停多久再试：反复拿失效的凭证去请求没有意义。
	pauseOnStale = time.Hour
	// maxImageBytes 是取回一张图片的大小上限。
	maxImageBytes = 10 << 20

	messageFromUser = 1
	messageFromBot  = 2
	stateFinished   = 2
	itemText        = 1
	itemImage       = 2
	// itemFile 是文件，取自官方客户端（@tencent-weixin/openclaw-weixin）的 MessageItemType.FILE。
	itemFile = 4
)

// KV 存取几项要跨重启留着的小状态：长轮询的游标，以及每个会话最近一次的上下文令牌。由 *pg.Store 提供。
type KV interface {
	Setting(ctx context.Context, key string) (string, error)
	PutSetting(ctx context.Context, key, value string) error
}

// Bot 是一个登录了的微信机器人。
type Bot struct {
	// Label 是这个渠道在配置里的名字。
	Label string
	// Token 在需要时取机器人凭证。
	Token func(ctx context.Context) (string, error)
	// BaseURL 是登录时服务端给的接口地址，为空用 DefaultBaseURL。
	BaseURL string
	// CDNBase 为空时用默认的媒体下载地址，只在对着模拟服务测试时才需要。
	CDNBase string
	// State 为空时游标和上下文令牌只留在内存里，重启之后要等对方再发一条消息才能回话。
	State KV
	HTTP  *http.Client
	Log   *slog.Logger
}

var _ channel.Adapter = (*Bot)(nil)

// Name 返回渠道名。
func (b *Bot) Name() string { return b.Label }

func (b *Bot) logger() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}

func (b *Bot) base() string {
	if b.BaseURL != "" {
		return strings.TrimSuffix(b.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (b *Bot) cdn() string {
	if b.CDNBase != "" {
		return strings.TrimSuffix(b.CDNBase, "/")
	}
	return defaultCDNBase
}

func (b *Bot) token(ctx context.Context) (string, error) {
	if b.Token == nil {
		return "", fmt.Errorf("%w: 微信渠道还没有登录", ErrInvalid)
	}
	t, err := b.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("weixin: 取机器人凭证失败: %w", err)
	}
	return strings.TrimSpace(t), nil
}

// baseInfo 是每个请求体里都要带的一段：协议版本，以及发请求的是谁。
func baseInfo() map[string]string {
	return map[string]string{"channel_version": protocolVersion, "bot_agent": botAgent}
}

// clientVersion 把版本号编成请求头里要的那个整数：主版本、次版本、修订号各占一个字节。
func clientVersion() string {
	var n [3]uint32
	for i, part := range strings.SplitN(protocolVersion, ".", 3) {
		v, _ := strconv.ParseUint(part, 10, 8)
		n[i] = uint32(v)
	}
	return strconv.FormatUint(uint64(n[0]<<16|n[1]<<8|n[2]), 10)
}

// commonHeaders 是 GET 和 POST 都要带的请求头。
func commonHeaders(h http.Header) {
	h.Set("iLink-App-Id", appID)
	h.Set("iLink-App-ClientVersion", clientVersion())
}

// postHeaders 是 POST 请求的请求头。token 为空表示这个接口不要凭证（登录之前的那两个）。
func postHeaders(h http.Header, token string) error {
	commonHeaders(h)
	h.Set("Content-Type", "application/json")
	h.Set("AuthorizationType", "ilink_bot_token")
	// X-WECHAT-UIN：一个随机的 32 位无符号整数，写成十进制再做 Base64。
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("weixin: 生成随机数失败: %w", err)
	}
	h.Set("X-WECHAT-UIN", base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(raw[:])), 10))))
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// post 调一个接口：把 in 作为 JSON 发过去，把回来的 JSON 解进 out。
func post(ctx context.Context, client *http.Client, endpoint, token string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if err := postHeaders(req.Header, token); err != nil {
		return err
	}
	return do(client, req, out)
}

func do(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("weixin: 读取应答失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("weixin: %s 返回 %d", req.URL.Path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	// 消息编号在线上是 64 位无符号整数，用 UseNumber 原样留着，不经过浮点数。
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("weixin: 解析 %s 的应答失败: %w", req.URL.Path, err)
	}
	return nil
}

func (b *Bot) client(timeout time.Duration) *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: timeout}
}

// cdnMedia 是媒体文件在 CDN 上的引用。
type cdnMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`
	FullURL           string `json:"full_url"`
}

type messageItem struct {
	Type     int `json:"type"`
	TextItem *struct {
		Text string `json:"text"`
	} `json:"text_item,omitempty"`
	ImageItem *struct {
		Media *cdnMedia `json:"media"`
		// AESKey 是十六进制写的 16 字节密钥，有它就优先用它。
		AESKey string `json:"aeskey"`
	} `json:"image_item,omitempty"`
	// FileItem 是文件。密钥只在 media.aes_key 里，下载、解密的办法和图片一样。
	FileItem *struct {
		Media    *cdnMedia `json:"media"`
		FileName string    `json:"file_name"`
	} `json:"file_item,omitempty"`
	// VoiceItem 与 VideoItem 是语音（silk）和视频（mp4），取的办法和文件一样（官方客户端的 media-download.js）。
	VoiceItem *struct {
		Media *cdnMedia `json:"media"`
	} `json:"voice_item,omitempty"`
	VideoItem *struct {
		Media *cdnMedia `json:"media"`
	} `json:"video_item,omitempty"`
}

// message 是服务端推来的一条消息。
type message struct {
	MessageID    json.Number   `json:"message_id"`
	FromUserID   string        `json:"from_user_id"`
	GroupID      string        `json:"group_id"`
	MessageType  int           `json:"message_type"`
	CreateTimeMS int64         `json:"create_time_ms"`
	ItemList     []messageItem `json:"item_list"`
	ContextToken string        `json:"context_token"`
}

type updates struct {
	Ret           int       `json:"ret"`
	ErrCode       int       `json:"errcode"`
	ErrMsg        string    `json:"errmsg"`
	Msgs          []message `json:"msgs"`
	GetUpdatesBuf string    `json:"get_updates_buf"`
	// LongPollingTimeoutMS 是服务端建议的下一次长轮询的时长。
	LongPollingTimeoutMS int64 `json:"longpolling_timeout_ms"`
}

func (b *Bot) stateKey(parts ...string) string {
	return "weixin." + b.Label + "." + strings.Join(parts, ".")
}

func (b *Bot) load(ctx context.Context, key string) string {
	if b.State == nil {
		return ""
	}
	v, err := b.State.Setting(ctx, key)
	if err != nil {
		b.logger().Warn("读取微信渠道的状态失败", "渠道", b.Label, "err", err)
	}
	return v
}

func (b *Bot) save(ctx context.Context, key, value string) {
	if b.State == nil || value == "" {
		return
	}
	if err := b.State.PutSetting(ctx, key, value); err != nil {
		b.logger().Warn("保存微信渠道的状态失败", "渠道", b.Label, "err", err)
	}
}

// Listen 用长轮询持续接收单聊消息，出错自动重试，直到 ctx 结束。
func (b *Bot) Listen(ctx context.Context, handle func(channel.Message)) error {
	token, err := b.token(ctx)
	if err != nil {
		return err
	}
	// 告诉服务端这个客户端上线了、下线了。不成不影响收发。
	b.notify(ctx, token, "ilink/bot/msg/notifystart")
	defer b.notify(context.WithoutCancel(ctx), token, "ilink/bot/msg/notifystop")
	b.logger().Info("微信机器人已上线", "渠道", b.Label)

	// 游标接着上次的：否则重启之后服务端会把已经处理过的消息再推一遍。
	buf := b.load(ctx, b.stateKey("buf"))
	wait, failures := longPoll, 0
	for ctx.Err() == nil {
		var resp updates
		pollCtx, cancel := context.WithTimeout(ctx, wait+10*time.Second)
		err := post(pollCtx, b.client(wait+10*time.Second), b.base()+"/ilink/bot/getupdates", token,
			map[string]any{"get_updates_buf": buf, "base_info": baseInfo()}, &resp)
		timedOut := pollCtx.Err() != nil && ctx.Err() == nil
		cancel()
		switch {
		case ctx.Err() != nil:
			return nil
		case timedOut:
			// 没有消息时等到超时是长轮询的常态，接着等就是了。
			continue
		case err != nil:
			failures++
			b.logger().Warn("微信长轮询失败，稍后重试", "渠道", b.Label, "连续失败", failures, "err", err)
			sleep(ctx, backoff(failures))
			continue
		case resp.Ret == staleToken || resp.ErrCode == staleToken:
			b.logger().Error("微信登录已失效，请在管理后台重新扫码连接", "渠道", b.Label)
			sleep(ctx, pauseOnStale)
			continue
		case resp.Ret != 0 || resp.ErrCode != 0:
			failures++
			b.logger().Warn("微信长轮询返回错误，稍后重试", "渠道", b.Label, "ret", resp.Ret, "errcode", resp.ErrCode, "errmsg", resp.ErrMsg)
			sleep(ctx, backoff(failures))
			continue
		}
		failures = 0
		if resp.LongPollingTimeoutMS > 0 {
			wait = time.Duration(resp.LongPollingTimeoutMS) * time.Millisecond
		}
		if resp.GetUpdatesBuf != "" {
			buf = resp.GetUpdatesBuf
			b.save(ctx, b.stateKey("buf"), buf)
		}
		for i := range resp.Msgs {
			if m, ok := b.convert(ctx, &resp.Msgs[i]); ok {
				handle(m)
			}
		}
	}
	return nil
}

// backoff 是连续失败 n 次之后等多久再试。
func backoff(n int) time.Duration {
	if n >= 3 {
		return 30 * time.Second
	}
	return 2 * time.Second
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (b *Bot) notify(ctx context.Context, token, endpoint string) {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := post(callCtx, b.client(10*time.Second), b.base()+"/"+endpoint, token,
		map[string]any{"base_info": baseInfo()}, nil); err != nil {
		b.logger().Debug("通知微信后端上下线失败", "渠道", b.Label, "err", err)
	}
}

// convert 把服务端推来的一条消息换成渠道层的消息。不是我们处理的那种（群聊、机器人自己发的）返回 false。
func (b *Bot) convert(ctx context.Context, m *message) (channel.Message, bool) {
	if m.MessageType != messageFromUser || m.GroupID != "" || m.FromUserID == "" {
		return channel.Message{}, false
	}
	// 回话要带上对方最近一条消息给的上下文令牌，先记下来。
	if m.ContextToken != "" {
		b.save(ctx, b.stateKey("ctx", m.FromUserID), m.ContextToken)
	}
	out := channel.Message{Chat: chatPrefix + m.FromUserID, Sender: m.FromUserID, ID: m.MessageID.String(), At: time.Now()}
	if m.CreateTimeMS > 0 {
		out.At = time.UnixMilli(m.CreateTimeMS)
	}
	var texts []string
	for _, item := range m.ItemList {
		switch {
		case item.Type == itemText && item.TextItem != nil:
			texts = append(texts, item.TextItem.Text)
		case item.Type == itemFile && item.FileItem != nil && item.FileItem.Media != nil:
			media := *item.FileItem.Media
			out.Files = append(out.Files, channel.Attachment{Name: item.FileItem.FileName,
				Fetch: func(ctx context.Context) (channel.Picture, error) { return b.fetchFile(ctx, media) }})
		case item.Type == itemVoice && item.VoiceItem != nil && item.VoiceItem.Media != nil:
			media := *item.VoiceItem.Media
			out.Voices = append(out.Voices, channel.Attachment{
				Fetch: func(ctx context.Context) (channel.Picture, error) { return b.fetchFile(ctx, media) }})
		case item.Type == itemVideo && item.VideoItem != nil && item.VideoItem.Media != nil:
			media := *item.VideoItem.Media
			out.Videos = append(out.Videos, channel.Attachment{
				Fetch: func(ctx context.Context) (channel.Picture, error) { return b.fetchFile(ctx, media) }})
		case item.Type == itemImage && item.ImageItem != nil && item.ImageItem.Media != nil:
			media, key := *item.ImageItem.Media, item.ImageItem.AESKey
			out.Images = append(out.Images, channel.Attachment{ContentType: "image/*",
				Fetch: func(ctx context.Context) (channel.Picture, error) { return b.fetchImage(ctx, media, key) }})
		}
	}
	out.Text = strings.TrimSpace(strings.Join(texts, "\n"))
	return out, true
}

// aesKey 定出解密一张图片用的 16 字节密钥，没有密钥（图片没加密）时返回 nil。
//
// 密钥有两处可能给：图片项里十六进制写的 aeskey（有就优先用），或者媒体引用里 Base64 写的 aes_key。
// 后者解开之后要么直接是 16 个字节，要么是 32 个十六进制字符。
func aesKey(hexKey, b64Key string) ([]byte, error) {
	if hexKey != "" {
		key, err := hex.DecodeString(hexKey)
		if err != nil || len(key) != 16 {
			return nil, fmt.Errorf("weixin: 图片密钥的格式不对")
		}
		return key, nil
	}
	if b64Key == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		return nil, fmt.Errorf("weixin: 图片密钥的格式不对")
	}
	if len(raw) == 16 {
		return raw, nil
	}
	if key, err := hex.DecodeString(string(raw)); err == nil && len(key) == 16 {
		return key, nil
	}
	return nil, fmt.Errorf("weixin: 图片密钥的格式不对")
}

// decryptECB 用 AES-128-ECB 解密并去掉 PKCS7 填充。微信的 CDN 上的媒体文件是这样加密的。
func decryptECB(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	size := block.BlockSize()
	if len(data) == 0 || len(data)%size != 0 {
		return nil, fmt.Errorf("weixin: 图片密文的长度不对")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += size {
		block.Decrypt(out[i:i+size], data[i:i+size])
	}
	pad := int(out[len(out)-1])
	if pad == 0 || pad > size || pad > len(out) {
		return nil, fmt.Errorf("weixin: 图片解密失败（填充不对）")
	}
	for _, c := range out[len(out)-pad:] {
		if int(c) != pad {
			return nil, fmt.Errorf("weixin: 图片解密失败（填充不对）")
		}
	}
	return out[:len(out)-pad], nil
}

// fetchImage 从 CDN 取回用户发来的一张图片并解密。
func (b *Bot) fetchImage(ctx context.Context, media cdnMedia, hexKey string) (channel.Picture, error) {
	key, err := aesKey(hexKey, media.AESKey)
	if err != nil {
		return channel.Picture{}, err
	}
	target := media.FullURL
	if target == "" {
		if media.EncryptQueryParam == "" {
			return channel.Picture{}, fmt.Errorf("weixin: 图片消息里没有下载地址")
		}
		target = b.cdn() + "/download?encrypted_query_param=" + url.QueryEscape(media.EncryptQueryParam)
	}
	// 测试里的模拟服务是本机的 http；正式环境的地址是服务端给的，只认 https。
	if !strings.HasPrefix(target, "https://") && b.CDNBase == "" {
		return channel.Picture{}, fmt.Errorf("weixin: 图片的下载地址不是 https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return channel.Picture{}, fmt.Errorf("weixin: 图片的下载地址不合法: %w", err)
	}
	resp, err := b.client(60 * time.Second).Do(req)
	if err != nil {
		return channel.Picture{}, fmt.Errorf("weixin: 取回图片失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return channel.Picture{}, fmt.Errorf("weixin: 图片服务器返回 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+17))
	if err != nil {
		return channel.Picture{}, fmt.Errorf("weixin: 读取图片失败: %w", err)
	}
	if len(raw) > maxImageBytes+16 {
		return channel.Picture{}, fmt.Errorf("weixin: 图片超过 %d MB", maxImageBytes>>20)
	}
	if key != nil {
		if raw, err = decryptECB(raw, key); err != nil {
			return channel.Picture{}, err
		}
	}
	// 类型以内容为准：模型服务对类型很挑剔。
	mime := http.DetectContentType(raw)
	if !strings.HasPrefix(mime, "image/") {
		return channel.Picture{}, fmt.Errorf("weixin: 取回来的不是图片（%s）", mime)
	}
	return channel.Picture{MIME: mime, Data: raw}, nil
}

// fetchFile 取回用户发来的一个文件：从 CDN 取回，再用消息里给的密钥解开。
func (b *Bot) fetchFile(ctx context.Context, media cdnMedia) (channel.Picture, error) {
	key, err := aesKey("", media.AESKey)
	if err != nil {
		return channel.Picture{}, err
	}
	target := media.FullURL
	if target == "" {
		if media.EncryptQueryParam == "" {
			return channel.Picture{}, fmt.Errorf("weixin: 文件消息里没有下载地址")
		}
		target = b.cdn() + "/download?encrypted_query_param=" + url.QueryEscape(media.EncryptQueryParam)
	}
	if !strings.HasPrefix(target, "https://") && b.CDNBase == "" {
		return channel.Picture{}, fmt.Errorf("weixin: 文件的下载地址不是 https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return channel.Picture{}, fmt.Errorf("weixin: 文件的下载地址不合法: %w", err)
	}
	resp, err := b.client(2 * time.Minute).Do(req)
	if err != nil {
		return channel.Picture{}, fmt.Errorf("weixin: 取回文件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return channel.Picture{}, fmt.Errorf("weixin: 文件服务器返回 %d", resp.StatusCode)
	}
	// 密文比明文多出至多 16 字节的填充。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, channel.MaxFileBytes+17))
	if err != nil {
		return channel.Picture{}, fmt.Errorf("weixin: 读取文件失败: %w", err)
	}
	if len(raw) > channel.MaxFileBytes+16 {
		return channel.Picture{}, fmt.Errorf("weixin: 文件超过 %d MB，收不了", channel.MaxFileBytes>>20)
	}
	if key != nil {
		if raw, err = decryptECB(raw, key); err != nil {
			return channel.Picture{}, err
		}
	}
	return channel.Picture{MIME: http.DetectContentType(raw), Data: raw}, nil
}

// Send 往一个单聊会话里发一条消息：文字，或者一张图片、一段视频、一个文件。
func (b *Bot) Send(ctx context.Context, m channel.Outgoing) error {
	user, ok := strings.CutPrefix(m.Chat, chatPrefix)
	if !ok || user == "" {
		return fmt.Errorf("%w: 会话标识 %q 不是微信单聊", ErrInvalid, m.Chat)
	}
	token, err := b.token(ctx)
	if err != nil {
		return err
	}
	// 一条消息只放一项，和官方客户端一样：文件、图片、视频先发，附带的话另发一条。
	if m.File != nil {
		item, err := b.mediaItem(ctx, token, user, m.File)
		if err != nil {
			return err
		}
		if err := b.sendItem(ctx, token, user, item); err != nil {
			return err
		}
		if strings.TrimSpace(m.Text) == "" {
			return nil
		}
	}
	return b.sendItem(ctx, token, user, map[string]any{"type": itemText, "text_item": map[string]string{"text": m.Text}})
}

// sendItem 发出只有一项的一条消息。
func (b *Bot) sendItem(ctx context.Context, token, user string, item map[string]any) error {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fmt.Errorf("weixin: 生成消息编号失败: %w", err)
	}
	msg := map[string]any{
		"from_user_id": "", "to_user_id": user,
		"client_id":    "atomassistant-" + hex.EncodeToString(id[:]),
		"message_type": messageFromBot, "message_state": stateFinished,
		"item_list": []map[string]any{item},
	}
	// 上下文令牌是对方最近一条消息带来的，回话时带上。没有也照发，由服务端决定收不收。
	if ctxToken := b.load(ctx, b.stateKey("ctx", user)); ctxToken != "" {
		msg["context_token"] = ctxToken
	}
	var resp struct {
		Ret     int    `json:"ret"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := post(callCtx, b.client(15*time.Second), b.base()+"/ilink/bot/sendmessage", token,
		map[string]any{"msg": msg, "base_info": baseInfo()}, &resp); err != nil {
		return fmt.Errorf("weixin: 发送消息失败: %w", err)
	}
	if resp.Ret == staleToken || resp.ErrCode == staleToken {
		return ErrLoggedOut
	}
	if resp.Ret != 0 {
		return fmt.Errorf("weixin: 发送消息失败: ret=%d %s", resp.Ret, resp.ErrMsg)
	}
	return nil
}
