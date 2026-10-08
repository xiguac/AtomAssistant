package vault

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const cardNo = "6222021234567890123"

func opened(t *testing.T) (*Vault, *MemStore) {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	st := NewMemStore()
	v, err := Open(st, key)
	if err != nil {
		t.Fatalf("打开保险库失败: %v", err)
	}
	return v, st
}

func TestVault_落盘的是密文且没有读出明文的途径(t *testing.T) {
	ctx := context.Background()
	v, st := opened(t)
	if err := v.Seal(ctx, "银行卡", cardNo, "工资卡卡号", []string{"pay.transfer"}); err != nil {
		t.Fatalf("存入失败: %v", err)
	}

	it, _ := st.GetItem(ctx, "银行卡")
	if strings.Contains(string(it.Sealed), cardNo) {
		t.Fatal("存储层拿到的必须是密文")
	}
	metas, err := v.List(ctx)
	if err != nil || len(metas) != 1 {
		t.Fatalf("列出失败: %v", err)
	}
	if blob, _ := json.Marshal(metas); strings.Contains(string(blob), cardNo) {
		t.Fatal("公开信息里不得出现明文")
	}
	if metas[0].Label != "银行卡" || metas[0].Note != "工资卡卡号" {
		t.Fatalf("公开信息不对: %+v", metas[0])
	}
}

func TestInject_只有被授权的工具能用且只换整值句柄(t *testing.T) {
	ctx := context.Background()
	v, _ := opened(t)
	if err := v.Seal(ctx, "银行卡", cardNo, "", []string{"pay.transfer"}); err != nil {
		t.Fatalf("存入失败: %v", err)
	}

	params := json.RawMessage(`{"to":"房东","card":"vault://银行卡","memo":"请把 vault://银行卡 写进备注","list":["vault://银行卡"]}`)
	inj, err := v.Inject(ctx, "pay.transfer", params)
	if err != nil {
		t.Fatalf("授权过的工具应能使用: %v", err)
	}
	var got struct {
		Card string
		Memo string
		List []string
	}
	if err := json.Unmarshal(inj.Params, &got); err != nil {
		t.Fatalf("替换后的参数不是合法 JSON: %v", err)
	}
	if got.Card != cardNo || got.List[0] != cardNo {
		t.Fatalf("整值句柄应被替换: %+v", got)
	}
	// 夹在长文本里的句柄不替换：否则模型能把明文拼进任意字段带出去。
	if got.Memo != "请把 vault://银行卡 写进备注" {
		t.Fatalf("文本中的句柄不应被替换: %q", got.Memo)
	}
	if len(inj.Used) != 1 || inj.Used[0] != "银行卡" {
		t.Fatalf("应记录用到的条目名: %v", inj.Used)
	}

	// 没被列进名单的工具：整次调用失败。
	if _, err := v.Inject(ctx, "notes.save", params); !errors.Is(err, ErrDenied) {
		t.Fatalf("未授权的工具应被拒绝: %v", err)
	}
	if _, err := v.Inject(ctx, "pay.transfer", json.RawMessage(`{"card":"vault://不存在"}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的条目应报错: %v", err)
	}

	// 没有句柄的参数原样通过，不碰保险库。
	plain := json.RawMessage(`{"city":"上海"}`)
	if inj, err := v.Inject(ctx, "weather", plain); err != nil || string(inj.Params) != string(plain) {
		t.Fatalf("没有句柄时应原样返回: %v", err)
	}
}

func TestRedact_工具回显的明文被换回句柄(t *testing.T) {
	ctx := context.Background()
	v, _ := opened(t)
	_ = v.Seal(ctx, "银行卡", cardNo, "", []string{"pay.transfer"})
	_ = v.Seal(ctx, "口令", `a"b\c`, "", []string{"pay.transfer"})

	inj, err := v.Inject(ctx, "pay.transfer", json.RawMessage(`{"card":"vault://银行卡","pwd":"vault://口令"}`))
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}

	echoed, _ := json.Marshal(map[string]any{
		"msg": "已向卡号 " + cardNo + " 扣款", "pwd": `a"b\c`, "ok": true,
	})
	out := inj.Redact(echoed)
	if strings.Contains(string(out), cardNo) || strings.Contains(string(out), `a\"b\\c`) {
		t.Fatalf("返回内容里的明文必须被清掉: %s", out)
	}
	if !json.Valid(out) || !strings.Contains(string(out), "[已隐藏:银行卡]") {
		t.Fatalf("清理后应仍是合法 JSON 并标出被隐藏的条目: %s", out)
	}

	// 明文以数字形式回显，直接替换会破坏 JSON：宁可整体降成字符串也不留明文。
	asNumber := json.RawMessage(`{"card":` + cardNo + `}`)
	out = inj.Redact(asNumber)
	if strings.Contains(string(out), cardNo) || !json.Valid(out) {
		t.Fatalf("数字形式的明文也必须清掉且结果合法: %s", out)
	}
}

func TestVault_锁定与篡改(t *testing.T) {
	ctx := context.Background()
	v, st := opened(t)
	_ = v.Seal(ctx, "银行卡", cardNo, "", []string{"pay.transfer"})
	_ = v.Seal(ctx, "身份证", "330102199001011234", "", []string{"pay.transfer"})

	// 没有密钥：能列名字，不能存也不能用。
	locked, err := Open(st, "")
	if err != nil {
		t.Fatalf("无密钥应得到锁定的保险库: %v", err)
	}
	if metas, _ := locked.List(ctx); len(metas) != 2 {
		t.Fatal("锁定状态下仍应能列出名字")
	}
	if err := locked.Seal(ctx, "x", "y", "", nil); !errors.Is(err, ErrLocked) {
		t.Fatalf("锁定状态下不能存: %v", err)
	}
	if _, err := locked.Inject(ctx, "pay.transfer", json.RawMessage(`{"c":"vault://银行卡"}`)); !errors.Is(err, ErrLocked) {
		t.Fatalf("锁定状态下不能用: %v", err)
	}

	// 换一把密钥解不开。
	otherKey, _ := GenerateKey()
	other, _ := Open(st, otherKey)
	if _, err := other.Inject(ctx, "pay.transfer", json.RawMessage(`{"c":"vault://银行卡"}`)); err == nil {
		t.Fatal("密钥不对应当解密失败")
	}

	// 把身份证的密文挪到银行卡名下：名字参与认证，解密必须失败。
	card, _ := st.GetItem(ctx, "银行卡")
	id, _ := st.GetItem(ctx, "身份证")
	card.Sealed, card.Nonce = id.Sealed, id.Nonce
	_ = st.PutItem(ctx, card)
	if _, err := v.Inject(ctx, "pay.transfer", json.RawMessage(`{"c":"vault://银行卡"}`)); err == nil {
		t.Fatal("密文被挪用应当解密失败")
	}

	if _, err := Open(st, "不是base64"); err == nil {
		t.Fatal("非法密钥应报错，不能静默当成锁定")
	}
	for _, bad := range []string{"", "有 空格", `带"引号`, strings.Repeat("a", 65)} {
		if ValidLabel(bad) {
			t.Errorf("名字 %q 应被判为非法", bad)
		}
	}
	if err := v.Seal(ctx, "有 空格", "v", "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("非法名字应被拒绝: %v", err)
	}
}
