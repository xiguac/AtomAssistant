package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"

	"github.com/xiguac/atomhelper/internal/channel"
)

func TestConvert_只收企业成员在单聊里发来的消息(t *testing.T) {
	b := &Bot{Label: "ding", AppID: "app-1"}
	m, ok := b.convert(&chatbot.BotCallbackDataModel{
		ConversationType: "1", SenderStaffId: "staff-7", SenderId: "$:LWCP_v1:xxx", MsgId: "msg-1",
		Msgtype: "text", Text: chatbot.BotCallbackDataTextModel{Content: " 明天提醒我开会 "}, CreateAt: 1791866585144,
	})
	if !ok || m.Chat != "dm:staff-7" || m.Sender != "staff-7" || m.Text != "明天提醒我开会" || m.ID != "msg-1" || m.At.IsZero() {
		t.Fatalf("消息不对: %+v %v", m, ok)
	}
	// 图片消息带着一个下载码，取回办法在附件上。
	m, ok = b.convert(&chatbot.BotCallbackDataModel{
		ConversationType: "1", SenderStaffId: "staff-7", MsgId: "msg-2", Msgtype: "picture",
		Content: map[string]any{"downloadCode": "code-abc"},
	})
	if !ok || len(m.Images) != 1 || m.Images[0].Fetch == nil || m.Text != "" {
		t.Fatalf("图片消息不对: %+v %v", m, ok)
	}
	// 文件消息：下载码和文件名都在 content 里。
	m, ok = b.convert(&chatbot.BotCallbackDataModel{
		ConversationType: "1", SenderStaffId: "staff-7", MsgId: "msg-3", Msgtype: "file",
		Content: map[string]any{"downloadCode": "code-file", "fileName": "周报.docx"},
	})
	if !ok || len(m.Files) != 1 || m.Files[0].Name != "周报.docx" || m.Files[0].Fetch == nil {
		t.Fatalf("文件消息不对: %+v %v", m, ok)
	}
	// 不处理的：群聊、企业外部的人（没有 staffId）、空消息。
	for name, d := range map[string]*chatbot.BotCallbackDataModel{
		"群聊":   {ConversationType: "2", SenderStaffId: "staff-7", MsgId: "m", Msgtype: "text"},
		"外部的人": {ConversationType: "1", SenderId: "ext", MsgId: "m", Msgtype: "text"},
		"空":    nil,
	} {
		if _, ok := b.convert(d); ok {
			t.Errorf("%s的消息不应处理", name)
		}
	}
}

func TestBot_发消息与取图片(t *testing.T) {
	var tokens int
	var sent map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			tokens++
			if body["appKey"] != "app-1" || body["appSecret"] != "secret-1" {
				http.Error(w, `{"code":"InvalidAuthentication"}`, http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"accessToken":"tok-1","expireIn":7200}`)
		case "/v1.0/robot/oToMessages/batchSend":
			if r.Header.Get("x-acs-dingtalk-access-token") != "tok-1" {
				http.Error(w, `{"code":"InvalidAuthentication"}`, http.StatusUnauthorized)
				return
			}
			sent = body
			if ids, _ := body["userIds"].([]any); len(ids) == 1 && ids[0] == "gone" {
				_, _ = io.WriteString(w, `{"invalidStaffIdList":["gone"]}`)
				return
			}
			_, _ = io.WriteString(w, `{"processQueryKey":"q1"}`)
		case "/v1.0/robot/messageFiles/download":
			if body["downloadCode"] != "code-abc" || body["robotCode"] != "app-1" {
				http.Error(w, `{"code":"bad"}`, http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"downloadUrl":"`+srv.URL+`/file/1"}`)
		case "/file/1":
			_, _ = w.Write(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 40)...))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	bot := &Bot{Label: "ding", AppID: "app-1", APIBase: srv.URL,
		Secret: func(context.Context) (string, error) { return "secret-1", nil }}

	if err := bot.Send(ctx, channel.Outgoing{Chat: "dm:staff-7", Text: "周五三点开会"}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	// 机器人编码没单独配时用 Client ID；内容放在 msgParam 里，是一段 JSON 字符串。
	var param map[string]string
	if sent["robotCode"] != "app-1" || sent["msgKey"] != "sampleText" ||
		json.Unmarshal([]byte(sent["msgParam"].(string)), &param) != nil || param["content"] != "周五三点开会" {
		t.Fatalf("发出去的请求不对: %v", sent)
	}
	// 访问凭证换一次留着用，不是每次都换。
	if err := bot.Send(ctx, channel.Outgoing{Chat: "dm:staff-7", Text: "再发一条"}); err != nil || tokens != 1 {
		t.Fatalf("访问凭证应复用: %d 次 %v", tokens, err)
	}
	// 接口成功了但人不对：消息其实没到，要报出来。
	if err := bot.Send(ctx, channel.Outgoing{Chat: "dm:gone", Text: "hi"}); err == nil || !strings.Contains(err.Error(), "没有送达") {
		t.Fatalf("发给无效成员应报错: %v", err)
	}
	if err := bot.Send(ctx, channel.Outgoing{Chat: "c2c:x", Text: "hi"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("别的渠道的会话应被拒绝: %v", err)
	}

	// 取图片：下载码换下载地址，再取回来。
	pic, err := bot.fetchImage(ctx, "code-abc")
	if err != nil || pic.MIME != "image/png" {
		t.Fatalf("取图片失败: %s %v", pic.MIME, err)
	}
}
