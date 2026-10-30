package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/xiguac/atomassistant/internal/channel"
)

func ptr(s string) *string { return &s }

func event(chatType, senderType, msgType, content string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: ptr("ou_abc")}, SenderType: ptr(senderType)},
		Message: &larkim.EventMessage{MessageId: ptr("om_1"), ChatType: ptr(chatType), MessageType: ptr(msgType),
			Content: ptr(content), CreateTime: ptr("1791866585144")},
	}}
}

func TestConvert_只收单聊里人发来的消息(t *testing.T) {
	b := &Bot{Label: "feishu"}
	m, ok := b.convert(event("p2p", "user", "text", `{"text":" 明天提醒我开会 "}`))
	if !ok || m.Chat != "p2p:ou_abc" || m.Sender != "ou_abc" || m.Text != "明天提醒我开会" || m.ID != "om_1" || m.At.UnixMilli() != 1791866585144 {
		t.Fatalf("消息不对: %+v %v", m, ok)
	}
	// 图片消息：取回办法在附件上。
	m, ok = b.convert(event("p2p", "user", "image", `{"image_key":"img_v2_1"}`))
	if !ok || len(m.Images) != 1 || m.Images[0].Fetch == nil || m.Text != "" {
		t.Fatalf("图片消息不对: %+v %v", m, ok)
	}
	// 文件消息：带着文件名，取回办法在附件上。
	m, ok = b.convert(event("p2p", "user", "file", `{"file_key":"file_v2_1","file_name":"报表.xlsx"}`))
	if !ok || len(m.Files) != 1 || m.Files[0].Name != "报表.xlsx" || m.Files[0].Fetch == nil || m.Text != "" {
		t.Fatalf("文件消息不对: %+v %v", m, ok)
	}
	// 不处理的：群聊、机器人发的、残缺的事件。
	if _, ok := b.convert(event("group", "user", "text", `{"text":"hi"}`)); ok {
		t.Fatal("群聊消息不应处理")
	}
	if _, ok := b.convert(event("p2p", "app", "text", `{"text":"hi"}`)); ok {
		t.Fatal("机器人发的消息不应处理")
	}
	if _, ok := b.convert(&larkim.P2MessageReceiveV1{}); ok {
		t.Fatal("残缺的事件不应处理")
	}
}

func TestParseContent_文字图片与富文本(t *testing.T) {
	text, imgs := parseContent("post", `{"title":"行程","content":[
		[{"tag":"text","text":"周五去"},{"tag":"a","text":"故宫","href":"https://www.dpm.org.cn"}],
		[{"tag":"img","image_key":"img_1"}],
		[{"tag":"text","text":"下午回来"},{"tag":"img","image_key":"img_2"}]]}`)
	if text != "行程\n周五去故宫（https://www.dpm.org.cn）\n下午回来" || len(imgs) != 2 || imgs[1] != "img_2" {
		t.Fatalf("富文本解析不对: %q %v", text, imgs)
	}
	// 认不出的类型、坏掉的内容：什么都不给，由上层告诉用户处理不了。
	for _, c := range [][2]string{{"audio", `{"file_key":"f"}`}, {"text", `not json`}, {"post", `[]`}} {
		if text, imgs := parseContent(c[0], c[1]); text != "" || len(imgs) != 0 {
			t.Errorf("%s 不应解析出内容: %q %v", c[0], text, imgs)
		}
	}
}

func TestSend_发文字消息(t *testing.T) {
	var sent map[string]any
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "tenant_access_token"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok","tenant_access_token":"t-1","expire":7200}`)
		case r.URL.Path == "/open-apis/im/v1/messages":
			if r.Header.Get("Authorization") != "Bearer t-1" {
				_, _ = io.WriteString(w, `{"code":99991663,"msg":"token invalid"}`)
				return
			}
			query = r.URL.RawQuery
			_ = json.NewDecoder(r.Body).Decode(&sent)
			if sent["receive_id"] == "ou_gone" {
				_, _ = io.WriteString(w, `{"code":230001,"msg":"invalid receive_id"}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"success","data":{"message_id":"om_2"}}`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	bot := &Bot{Label: "feishu", AppID: "cli_1", APIBase: srv.URL,
		Secret: func(context.Context) (string, error) { return "secret-1", nil }}

	if err := bot.Send(ctx, channel.Outgoing{Chat: "p2p:ou_abc", Text: "周五三点开会"}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	// 按 open_id 发；内容是一段 JSON 字符串。
	var content map[string]string
	if query != "receive_id_type=open_id" || sent["receive_id"] != "ou_abc" || sent["msg_type"] != "text" ||
		json.Unmarshal([]byte(sent["content"].(string)), &content) != nil || content["text"] != "周五三点开会" {
		t.Fatalf("发出去的请求不对: %s %v", query, sent)
	}
	// 飞书说不行就是没发出去。
	if err := bot.Send(ctx, channel.Outgoing{Chat: "p2p:ou_gone", Text: "hi"}); err == nil || !strings.Contains(err.Error(), "230001") {
		t.Fatalf("飞书返回错误码时应报错: %v", err)
	}
	if err := bot.Send(ctx, channel.Outgoing{Chat: "c2c:x", Text: "hi"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("别的渠道的会话应被拒绝: %v", err)
	}
	// 没配凭据的发不了。
	if err := (&Bot{Label: "x"}).Send(ctx, channel.Outgoing{Chat: "p2p:ou_abc", Text: "hi"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("没有凭据应报错: %v", err)
	}
}
