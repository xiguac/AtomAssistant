// Package dingtalk 把钉钉机器人接成一个消息渠道。
//
// 收消息用的是钉钉的 Stream 模式：我们主动连到钉钉，消息从这条连接上推过来，不需要公网可达的回调地址。
// 连接、心跳、重连用官方 Go SDK（github.com/open-dingtalk/dingtalk-stream-sdk-go）。
// 发消息和下载图片走钉钉的开放接口，地址和字段照官方 OpenAPI Go SDK
// （github.com/alibabacloud-go/dingtalk 的 robot_1_0、oauth2_1_0）核对过。
//
// 目前只处理单聊。主人名单认的是员工在企业里的 userId（消息里的 senderStaffId）：
// 主动给他发消息的接口要的就是它。所以这个渠道只对企业内部的成员可用。
// 能收文字和图片，能发文字；发文件暂不支持。
package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/xiguac/atomhelper/internal/channel"
)

// ErrInvalid 表示配置或参数不合法。
var ErrInvalid = errors.New("dingtalk: 参数不合法")

const (
	defaultAPIBase = "https://api.dingtalk.com"
	// chatPrefix 是单聊会话标识的前缀，后面跟员工的 userId。
	chatPrefix = "dm:"
	// tokenSlack 是凭证过期前多久就换新的。
	tokenSlack = 2 * time.Minute
	// maxImageBytes 是取回一张图片的大小上限。
	maxImageBytes = 10 << 20
	// conversationSingle 是单聊的会话类型。
	conversationSingle = "1"
)

// Bot 是一个钉钉机器人（企业内部应用）。
type Bot struct {
	// Label 是这个渠道在配置里的名字。
	Label string
	// AppID 是应用的 Client ID（原 AppKey）。
	AppID string
	// Secret 在需要时取 Client Secret（原 AppSecret）。
	Secret func(ctx context.Context) (string, error)
	// RobotCode 是机器人的编码，为空时用 AppID：企业内部应用的机器人两者相同。
	RobotCode string
	// APIBase 为空时用钉钉的正式地址，只在对着模拟服务测试时才需要。
	APIBase string
	HTTP    *http.Client
	Log     *slog.Logger

	mu     sync.Mutex
	token  string
	expiry time.Time
}

var _ channel.Adapter = (*Bot)(nil)

// Name 返回渠道名。
func (b *Bot) Name() string { return b.Label }

func (b *Bot) http() *http.Client {
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

func (b *Bot) robot() string {
	if b.RobotCode != "" {
		return b.RobotCode
	}
	return b.AppID
}

func (b *Bot) secret(ctx context.Context) (string, error) {
	if b.AppID == "" || b.Secret == nil {
		return "", fmt.Errorf("%w: 钉钉机器人缺少 app_id 或 secret", ErrInvalid)
	}
	s, err := b.Secret(ctx)
	if err != nil {
		return "", fmt.Errorf("dingtalk: 取 Client Secret 失败: %w", err)
	}
	return s, nil
}

// call 调一个钉钉的开放接口：把 in 作为 JSON 发过去，把回来的 JSON 解进 out。token 为空表示这个接口不要凭证。
func (b *Bot) call(ctx context.Context, path, token string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.api()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}
	resp, err := b.http().Do(req)
	if err != nil {
		return fmt.Errorf("dingtalk: 请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("dingtalk: 读取 %s 的应答失败: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		// 钉钉出错时回的是 {"code":"...","message":"..."}，里面没有我们发过去的凭据。
		return fmt.Errorf("dingtalk: %s 返回 %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("dingtalk: 解析 %s 的应答失败: %w", path, err)
	}
	return nil
}

// accessToken 返回可用的访问凭证，快过期时换新的。
func (b *Bot) accessToken(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && time.Until(b.expiry) > tokenSlack {
		return b.token, nil
	}
	secret, err := b.secret(ctx)
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int64  `json:"expireIn"`
	}
	if err := b.call(ctx, "/v1.0/oauth2/accessToken", "", map[string]string{"appKey": b.AppID, "appSecret": secret}, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("dingtalk: 换取访问凭证失败：应答里没有 accessToken")
	}
	b.token, b.expiry = out.AccessToken, time.Now().Add(time.Duration(out.ExpireIn)*time.Second)
	return b.token, nil
}

// Listen 连上钉钉并持续接收单聊消息，断线由 SDK 自动重连，直到 ctx 结束。
func (b *Bot) Listen(ctx context.Context, handle func(channel.Message)) error {
	secret, err := b.secret(ctx)
	if err != nil {
		return err
	}
	cli := client.NewStreamClient(client.WithAppCredential(client.NewAppCredentialConfig(b.AppID, secret)))
	cli.RegisterChatBotCallbackRouter(func(_ context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
		if m, ok := b.convert(data); ok {
			handle(m)
		}
		// 不管这条消息我们处理不处理，都算收到了：否则钉钉会重推。
		return []byte(""), nil
	})
	if err := cli.Start(ctx); err != nil {
		return fmt.Errorf("dingtalk: 连接失败（Client ID 与 Client Secret 对吗？应用开通了机器人并选了 Stream 模式吗？）: %w", err)
	}
	b.logger().Info("钉钉机器人已上线", "渠道", b.Label)
	// SDK 在自己的协程里收消息；这里等到要停的时候把连接关掉。
	<-ctx.Done()
	cli.Close()
	return nil
}

// convert 把钉钉推来的一条消息换成渠道层的消息。不是我们处理的那种（群聊、外部联系人）返回 false。
func (b *Bot) convert(data *chatbot.BotCallbackDataModel) (channel.Message, bool) {
	if data == nil || data.ConversationType != conversationSingle {
		return channel.Message{}, false
	}
	// 没有 staffId 的是企业外部的人：主动发消息的接口够不着他，也就没法把他当主人。
	staff := data.SenderStaffId
	if staff == "" || data.MsgId == "" {
		return channel.Message{}, false
	}
	out := channel.Message{Chat: chatPrefix + staff, Sender: staff, ID: data.MsgId, At: time.Now()}
	if data.CreateAt > 0 {
		out.At = time.UnixMilli(data.CreateAt)
	}
	switch data.Msgtype {
	case "text":
		out.Text = strings.TrimSpace(data.Text.Content)
	case "picture":
		// 图片消息的 content 里是一个下载码，要再调接口换成下载地址。
		if code := downloadCode(data.Content); code != "" {
			out.Images = append(out.Images, channel.Attachment{ContentType: "image/*",
				Fetch: func(ctx context.Context) (channel.Picture, error) { return b.fetchImage(ctx, code) }})
		}
	}
	return out, true
}

// downloadCode 从图片消息的内容里取出下载码。
//
// 图片消息的内容形如 {"downloadCode":"..."}。这个字段名取自钉钉的接口文档，官方的 Stream SDK 里
// 没有它的类型定义（内容是一个不定形的值），所以这里按不定形的值来取，取不到就当没有图片。
func downloadCode(content any) string {
	m, ok := content.(map[string]any)
	if !ok {
		return ""
	}
	code, _ := m["downloadCode"].(string)
	return code
}

// fetchImage 取回用户发来的一张图片：先用下载码换下载地址，再把图片取回来。
func (b *Bot) fetchImage(ctx context.Context, code string) (channel.Picture, error) {
	token, err := b.accessToken(ctx)
	if err != nil {
		return channel.Picture{}, err
	}
	var out struct {
		DownloadURL string `json:"downloadUrl"`
	}
	if err := b.call(ctx, "/v1.0/robot/messageFiles/download", token,
		map[string]string{"downloadCode": code, "robotCode": b.robot()}, &out); err != nil {
		return channel.Picture{}, err
	}
	// 测试里的模拟服务是本机的 http；正式环境的地址是钉钉给的，只认 https。
	if !strings.HasPrefix(out.DownloadURL, "https://") && b.APIBase == "" {
		return channel.Picture{}, fmt.Errorf("dingtalk: 图片的下载地址不是 https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, out.DownloadURL, nil)
	if err != nil {
		return channel.Picture{}, fmt.Errorf("dingtalk: 图片的下载地址不合法: %w", err)
	}
	resp, err := b.http().Do(req)
	if err != nil {
		return channel.Picture{}, fmt.Errorf("dingtalk: 取回图片失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return channel.Picture{}, fmt.Errorf("dingtalk: 图片服务器返回 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return channel.Picture{}, fmt.Errorf("dingtalk: 读取图片失败: %w", err)
	}
	if len(raw) > maxImageBytes {
		return channel.Picture{}, fmt.Errorf("dingtalk: 图片超过 %d MB", maxImageBytes>>20)
	}
	// 类型以内容为准：模型服务对类型很挑剔。
	mime := http.DetectContentType(raw)
	if !strings.HasPrefix(mime, "image/") {
		return channel.Picture{}, fmt.Errorf("dingtalk: 取回来的不是图片（%s）", mime)
	}
	return channel.Picture{MIME: mime, Data: raw}, nil
}

// Send 往一个单聊会话里发一条文字消息。
//
// 用的是"机器人批量发送单聊消息"接口：它不依赖用户刚发过消息，
// 任务隔了几个小时、几天才有进展时照样发得出去。
func (b *Bot) Send(ctx context.Context, m channel.Outgoing) error {
	staff, ok := strings.CutPrefix(m.Chat, chatPrefix)
	if !ok || staff == "" {
		return fmt.Errorf("%w: 会话标识 %q 不是钉钉单聊", ErrInvalid, m.Chat)
	}
	if m.File != nil {
		return fmt.Errorf("dingtalk: 钉钉渠道暂不支持发送文件")
	}
	token, err := b.accessToken(ctx)
	if err != nil {
		return err
	}
	param, err := json.Marshal(map[string]string{"content": m.Text})
	if err != nil {
		return err
	}
	var out struct {
		InvalidStaffIDList        []string `json:"invalidStaffIdList"`
		FlowControlledStaffIDList []string `json:"flowControlledStaffIdList"`
	}
	if err := b.call(ctx, "/v1.0/robot/oToMessages/batchSend", token, map[string]any{
		"robotCode": b.robot(), "userIds": []string{staff}, "msgKey": "sampleText", "msgParam": string(param),
	}, &out); err != nil {
		return err
	}
	// 接口本身成功了，消息却没到的两种情况：这个人不在企业里，或者被限流了。
	switch {
	case len(out.InvalidStaffIDList) > 0:
		return fmt.Errorf("dingtalk: 消息没有送达：%s 不是这个企业的有效成员", staff)
	case len(out.FlowControlledStaffIDList) > 0:
		return fmt.Errorf("dingtalk: 消息没有送达：发送频率超过了钉钉的限制")
	}
	return nil
}
