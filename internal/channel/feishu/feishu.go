// Package feishu 把飞书机器人接成一个消息渠道。
//
// 收消息用的是飞书开放平台的"长连接"事件订阅：我们主动连到飞书，事件从这条连接上推过来。
// 选它而不是回调地址，理由和 QQ 渠道一样——助手跑在用户自己的服务器上，不一定有能被公网访问的
// HTTPS 地址。长连接的帧是飞书自己的二进制格式，所以这里用官方 Go SDK
// （github.com/larksuite/oapi-sdk-go/v3）收发，不自己实现协议。
//
// 目前只处理单聊：和 QQ 渠道一样，主人名单认的是单聊里的账号标识（open_id）。
// 能收文字、图片和图文混排的富文本，能发文字和文件。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/xiguac/atomhelper/internal/channel"
)

// ErrInvalid 表示配置或参数不合法。
var ErrInvalid = errors.New("feishu: 参数不合法")

const (
	// chatPrefix 是单聊会话标识的前缀，后面跟用户的 open_id。
	chatPrefix = "p2p:"
	// maxImageBytes 是取回一张图片的大小上限。
	maxImageBytes = 10 << 20
)

// Bot 是一个飞书机器人（企业自建应用）。
type Bot struct {
	// Label 是这个渠道在配置里的名字。
	Label string
	AppID string
	// Secret 在需要时取 App Secret。
	Secret func(ctx context.Context) (string, error)
	// APIBase 为空时用飞书的正式地址，只在对着模拟服务测试时才需要。
	APIBase string
	Log     *slog.Logger

	mu     sync.Mutex
	client *lark.Client
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

// secret 取出 App Secret。
func (b *Bot) secret(ctx context.Context) (string, error) {
	if b.AppID == "" || b.Secret == nil {
		return "", fmt.Errorf("%w: 飞书机器人缺少 app_id 或 secret", ErrInvalid)
	}
	s, err := b.Secret(ctx)
	if err != nil {
		return "", fmt.Errorf("feishu: 取 App Secret 失败: %w", err)
	}
	return s, nil
}

// api 返回调用飞书接口用的客户端。访问凭证由 SDK 自己换取和续期。
func (b *Bot) api(ctx context.Context) (*lark.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		return b.client, nil
	}
	secret, err := b.secret(ctx)
	if err != nil {
		return nil, err
	}
	opts := []lark.ClientOptionFunc{lark.WithLogLevel(larkcore.LogLevelError), lark.WithReqTimeout(30 * time.Second)}
	if b.APIBase != "" {
		opts = append(opts, lark.WithOpenBaseUrl(strings.TrimSuffix(b.APIBase, "/")))
	}
	b.client = lark.NewClient(b.AppID, secret, opts...)
	return b.client, nil
}

// Listen 连上飞书并持续接收单聊消息，断线由 SDK 自动重连，直到 ctx 结束。
func (b *Bot) Listen(ctx context.Context, handle func(channel.Message)) error {
	secret, err := b.secret(ctx)
	if err != nil {
		return err
	}
	events := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
			if m, ok := b.convert(ev); ok {
				handle(m)
			}
			// 不管这条消息我们处理不处理，都算收到了：否则飞书会一直重推。
			return nil
		})
	cli := larkws.NewClient(b.AppID, secret,
		larkws.WithEventHandler(events),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithOnReady(func() { b.logger().Info("飞书机器人已上线", "渠道", b.Label) }),
		larkws.WithOnReconnecting(func() { b.logger().Warn("飞书长连接断开，正在重连", "渠道", b.Label) }),
	)
	err = cli.Start(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// convert 把飞书推来的一条消息换成渠道层的消息。不是我们处理的那种（群聊、机器人发的）返回 false。
func (b *Bot) convert(ev *larkim.P2MessageReceiveV1) (channel.Message, bool) {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil || ev.Event.Sender == nil || ev.Event.Sender.SenderId == nil {
		return channel.Message{}, false
	}
	msg, sender := ev.Event.Message, ev.Event.Sender
	// 只处理单聊里人发来的消息：群里的发信人和单聊里的不是同一回事，没法用同一份主人名单判断。
	if str(msg.ChatType) != "p2p" || str(sender.SenderType) != "user" {
		return channel.Message{}, false
	}
	openID, id := str(sender.SenderId.OpenId), str(msg.MessageId)
	if openID == "" || id == "" {
		return channel.Message{}, false
	}
	out := channel.Message{Chat: chatPrefix + openID, Sender: openID, ID: id, At: time.Now()}
	if ms, err := strconv.ParseInt(str(msg.CreateTime), 10, 64); err == nil && ms > 0 {
		out.At = time.UnixMilli(ms)
	}
	if str(msg.MessageType) == "file" {
		var c struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if json.Unmarshal([]byte(str(msg.Content)), &c) == nil && c.FileKey != "" {
			key := c.FileKey
			out.Files = append(out.Files, channel.Attachment{Name: c.FileName,
				Fetch: func(ctx context.Context) (channel.Picture, error) { return b.fetchFile(ctx, id, key) }})
		}
		return out, true
	}
	text, imageKeys := parseContent(str(msg.MessageType), str(msg.Content))
	out.Text = text
	for _, key := range imageKeys {
		// 飞书的图片没有公开的下载地址，要带着凭证调接口取。
		out.Images = append(out.Images, channel.Attachment{ContentType: "image/*",
			Fetch: func(ctx context.Context) (channel.Picture, error) { return b.fetchImage(ctx, id, key) }})
	}
	return out, true
}

// parseContent 从消息内容里取出文字和图片。content 是一段 JSON，形态随消息类型而不同。
//
// 认三种：纯文字、单张图片、富文本（文字和图片混排）。别的类型（语音、文件、卡片）返回空，
// 由上层告诉用户这种消息暂时处理不了。
func parseContent(msgType, content string) (text string, imageKeys []string) {
	switch msgType {
	case "text":
		var c struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(content), &c) == nil {
			text = c.Text
		}
	case "image":
		var c struct {
			ImageKey string `json:"image_key"`
		}
		if json.Unmarshal([]byte(content), &c) == nil && c.ImageKey != "" {
			imageKeys = append(imageKeys, c.ImageKey)
		}
	case "post":
		var c struct {
			Title   string `json:"title"`
			Content [][]struct {
				Tag      string `json:"tag"`
				Text     string `json:"text"`
				Href     string `json:"href"`
				ImageKey string `json:"image_key"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(content), &c) != nil {
			return "", nil
		}
		var lines []string
		if c.Title != "" {
			lines = append(lines, c.Title)
		}
		for _, row := range c.Content {
			var sb strings.Builder
			for _, el := range row {
				switch el.Tag {
				case "text":
					sb.WriteString(el.Text)
				case "a":
					sb.WriteString(el.Text)
					if el.Href != "" {
						sb.WriteString("（" + el.Href + "）")
					}
				case "img":
					if el.ImageKey != "" {
						imageKeys = append(imageKeys, el.ImageKey)
					}
				}
			}
			if sb.Len() > 0 {
				lines = append(lines, sb.String())
			}
		}
		text = strings.Join(lines, "\n")
	}
	return strings.TrimSpace(text), imageKeys
}

// fetchImage 取回用户发来的一张图片。
func (b *Bot) fetchImage(ctx context.Context, messageID, imageKey string) (channel.Picture, error) {
	cli, err := b.api(ctx)
	if err != nil {
		return channel.Picture{}, err
	}
	resp, err := cli.Im.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).FileKey(imageKey).Type("image").Build())
	if err != nil {
		return channel.Picture{}, fmt.Errorf("feishu: 取回图片失败: %w", err)
	}
	if !resp.Success() {
		return channel.Picture{}, fmt.Errorf("feishu: 取回图片失败: %d %s", resp.Code, resp.Msg)
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, maxImageBytes+1))
	if err != nil {
		return channel.Picture{}, fmt.Errorf("feishu: 读取图片失败: %w", err)
	}
	if len(data) > maxImageBytes {
		return channel.Picture{}, fmt.Errorf("feishu: 图片超过 %d MB", maxImageBytes>>20)
	}
	// 类型以内容为准：模型服务对类型很挑剔。
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return channel.Picture{}, fmt.Errorf("feishu: 取回来的不是图片（%s）", mime)
	}
	return channel.Picture{MIME: mime, Data: data}, nil
}

// fetchFile 取回用户发来的一个文件。和图片走同一个接口，类型写 file。
func (b *Bot) fetchFile(ctx context.Context, messageID, fileKey string) (channel.Picture, error) {
	cli, err := b.api(ctx)
	if err != nil {
		return channel.Picture{}, err
	}
	resp, err := cli.Im.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).FileKey(fileKey).Type("file").Build())
	if err != nil {
		return channel.Picture{}, fmt.Errorf("feishu: 取回文件失败: %w", err)
	}
	if !resp.Success() {
		return channel.Picture{}, fmt.Errorf("feishu: 取回文件失败: %d %s", resp.Code, resp.Msg)
	}
	data, err := channel.ReadLimited(resp.File)
	if err != nil {
		return channel.Picture{}, err
	}
	return channel.Picture{MIME: http.DetectContentType(data), Data: data}, nil
}

// Send 往一个单聊会话里发一条消息：文字，或者一个文件。
func (b *Bot) Send(ctx context.Context, m channel.Outgoing) error {
	openID, ok := strings.CutPrefix(m.Chat, chatPrefix)
	if !ok || openID == "" {
		return fmt.Errorf("%w: 会话标识 %q 不是飞书单聊", ErrInvalid, m.Chat)
	}
	cli, err := b.api(ctx)
	if err != nil {
		return err
	}
	if m.File != nil {
		key, err := b.upload(ctx, cli, m.File)
		if err != nil {
			return err
		}
		content, err := json.Marshal(map[string]string{"file_key": key})
		if err != nil {
			return err
		}
		if err := b.post(ctx, cli, openID, "file", string(content)); err != nil {
			return err
		}
		if strings.TrimSpace(m.Text) == "" {
			return nil
		}
	}
	content, err := json.Marshal(map[string]string{"text": m.Text})
	if err != nil {
		return err
	}
	return b.post(ctx, cli, openID, "text", string(content))
}

// post 发出一条消息。
func (b *Bot) post(ctx context.Context, cli *lark.Client, openID, msgType, content string) error {
	resp, err := cli.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("open_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(openID).MsgType(msgType).Content(content).Build()).
		Build())
	if err != nil {
		return fmt.Errorf("feishu: 发送消息失败: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: 发送消息失败: %d %s", resp.Code, resp.Msg)
	}
	return nil
}

// upload 把一个文件传到飞书，返回它的 file_key。
func (b *Bot) upload(ctx context.Context, cli *lark.Client, f *channel.File) (string, error) {
	resp, err := cli.Im.File.Create(ctx, larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().FileType("stream").FileName(f.Name).File(bytes.NewReader(f.Data)).Build()).
		Build())
	if err != nil {
		return "", fmt.Errorf("feishu: 上传文件失败: %w", err)
	}
	if !resp.Success() || resp.Data == nil || str(resp.Data.FileKey) == "" {
		return "", fmt.Errorf("feishu: 上传文件失败: %d %s", resp.Code, resp.Msg)
	}
	return *resp.Data.FileKey, nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
