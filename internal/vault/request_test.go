package vault

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type memSettings map[string]string

func (m memSettings) PutSetting(_ context.Context, key, value string) error {
	if value == "" {
		delete(m, key)
	} else {
		m[key] = value
	}
	return nil
}

func (m memSettings) SettingsWithPrefix(_ context.Context, prefix string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out, nil
}

type fixedMetas []Meta

func (f fixedMetas) List(context.Context) ([]Meta, error) { return f, nil }

func TestRequests_助手只登记名字值由用户填(t *testing.T) {
	ctx := context.Background()
	store := memSettings{}
	r := &Requests{Store: store, Vault: fixedMetas{{Label: "icloud"}}}

	if err := r.Add(ctx, Request{Label: "github-token", Note: " GitHub 的\n访问令牌 ", Tools: []string{"github.create_issue", " "}, Task: "t1"}); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	if err := r.Add(ctx, Request{Label: "mcp.flight", Note: "航班 MCP 的密钥", Tools: []string{"mcp.flight"}}); err != nil {
		t.Fatalf("给对话里安装的 MCP server 登记密钥应可以: %v", err)
	}
	list, err := r.List(ctx)
	if err != nil || len(list) != 2 || list[0].Label != "github-token" || list[0].Note != "GitHub 的 访问令牌" ||
		len(list[0].Tools) != 1 || list[0].Task != "t1" || list[0].At.IsZero() {
		t.Fatalf("登记的内容不对: %+v %v", list, err)
	}
	// 登记里没有值这一项：值只由用户在后台填。
	for _, raw := range store {
		if strings.Contains(raw, "value") {
			t.Fatalf("登记里不应有值: %s", raw)
		}
	}

	for name, bad := range map[string]Request{
		"名字不合规矩":      {Label: "a b", Note: "x", Tools: []string{"t.x"}},
		"没写用途":        {Label: "k", Tools: []string{"t.x"}},
		"没写授权对象":      {Label: "k", Note: "x"},
		"已经有值的名字":     {Label: "icloud", Note: "x", Tools: []string{"t.x"}},
		"模型服务的凭据":     {Label: "provider.deepseek", Note: "x", Tools: []string{"provider.deepseek"}},
		"消息源的凭据":      {Label: "channel.qq", Note: "x", Tools: []string{"channel.qq"}},
		"授权给连接器":      {Label: "k", Note: "x", Tools: []string{"connector.mail"}},
		"授权对象里夹着别的字符": {Label: "k", Note: "x", Tools: []string{"t.x; rm"}},
	} {
		if err := r.Add(ctx, bad); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%s应被拒绝: %v", name, err)
		}
	}

	if got, _ := r.Get(ctx, "mcp.flight"); got == nil || got.Tools[0] != "mcp.flight" {
		t.Fatalf("应能取出登记: %+v", got)
	}
	if err := r.Remove(ctx, "mcp.flight"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "mcp.flight"); got != nil {
		t.Fatal("去掉之后不应还在")
	}

	// 上限：助手跑飞了也塞不满页面；同名的是替换，不占名额。
	for i := 0; i < maxRequests+3; i++ {
		_ = r.Add(ctx, Request{Label: "k" + string(rune('a'+i)), Note: "x", Tools: []string{"t.x"}})
	}
	if list, _ := r.List(ctx); len(list) != maxRequests {
		t.Fatalf("待填写的条目应不超过 %d 个: %d", maxRequests, len(list))
	}
	if err := r.Add(ctx, Request{Label: "github-token", Note: "换个说法", Tools: []string{"t.x"}}); err != nil {
		t.Fatalf("同名的登记应可以替换: %v", err)
	}
}
