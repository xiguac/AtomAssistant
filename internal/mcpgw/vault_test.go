package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/vault"
)

const homeAddress = "杭州市西湖区某某路 88 号 3 幢 502"

func vaultWith(t *testing.T, allowed ...string) *vault.Vault {
	t.Helper()
	key, err := vault.GenerateKey()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	v, err := vault.Open(vault.NewMemStore(), key)
	if err != nil {
		t.Fatalf("打开保险库失败: %v", err)
	}
	if err := v.Seal(context.Background(), "家庭住址", homeAddress, "收货地址", allowed); err != nil {
		t.Fatalf("存入失败: %v", err)
	}
	return v
}

func TestCall_句柄在调用瞬间换成明文且明文不外流(t *testing.T) {
	gw, cap, audit := setup(t, weatherGrant())
	gw.secrets = vaultWith(t, "demo.weather")

	res, err := gw.Call(context.Background(), CallRequest{
		Tool: "demo.weather", Params: json.RawMessage(`{"city":"vault://家庭住址"}`), TaskID: "t1",
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	// 工具拿到的是真实内容。
	if len(cap.args) != 1 || cap.args[0].City != homeAddress {
		t.Fatalf("工具应收到明文: %+v", cap.args)
	}
	// 工具把入参原样回显了；往回传的内容里不得再有明文。
	if strings.Contains(string(res.Output), "西湖区") {
		t.Fatalf("明文不得进入步骤输出: %s", res.Output)
	}
	if !strings.Contains(string(res.Output), "[已隐藏:家庭住址]") || !json.Valid(res.Output) {
		t.Fatalf("输出应标出被隐藏的条目且仍是合法 JSON: %s", res.Output)
	}
	// 审计记的是用了哪一条，不是值。
	joined := strings.Join(audit.entries, "\n")
	if strings.Contains(joined, "西湖区") || !strings.Contains(joined, "使用保险库条目: 家庭住址") {
		t.Fatalf("审计应只记条目名:\n%s", joined)
	}
}

func TestCall_未授权的工具引用保险库条目被拒且不触达工具(t *testing.T) {
	gw, cap, audit := setup(t, weatherGrant())
	// 这一条只授权给了别的工具。
	gw.secrets = vaultWith(t, "mail.send")

	_, err := gw.Call(context.Background(), CallRequest{
		Tool: "demo.weather", Params: json.RawMessage(`{"city":"vault://家庭住址"}`), TaskID: "t1",
	})
	if !errors.Is(err, vault.ErrDenied) {
		t.Fatalf("期望 ErrDenied，实际 %v", err)
	}
	if len(cap.args) != 0 {
		t.Fatal("被拒绝的调用不得触达工具")
	}
	if !strings.Contains(strings.Join(audit.entries, "\n"), "保险库拒绝") {
		t.Fatal("拒绝必须留痕")
	}
}
