package pay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain 让测试二进制自己充当假的 alipay-bot：
// 第一个参数是已知的子命令时，把收到的参数原样打印出来。
func TestMain(m *testing.M) {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "submit-payment", "query-payment-status", "check-wallet",
			"apply-wallet", "bind-wallet", "close-wallet",
			"402-buyer-pay", "402-query-payment-status":
			fakeBot(os.Args[1:])
		}
	}
	os.Exit(m.Run())
}

func fakeBot(args []string) {
	// 我们看不到源码的程序不该拿到主进程的密钥。
	for _, k := range []string{"ATOM_DSN", "ATOM_VAULT_KEY", "DEEPSEEK_API_KEY"} {
		if os.Getenv(k) != "" {
			fmt.Println("泄露: 付款程序拿到了", k)
			os.Exit(3)
		}
	}
	fmt.Println(strings.Join(args, "\x1f"))
	for _, a := range args {
		if a == "FAIL" {
			fmt.Println("买家账户不匹配")
			os.Exit(1)
		}
		if a == "HANG" {
			time.Sleep(time.Minute)
		}
	}
	os.Exit(0)
}

func bot() Alipay {
	return Alipay{Command: os.Args[0], AgentName: "AtomHelper", Timeout: 20 * time.Second}
}

func argsOf(t *testing.T, out string) []string {
	t.Helper()
	return strings.Split(strings.SplitN(out, "\n", 2)[0], "\x1f")
}

func TestSubmit_按官方格式组装命令(t *testing.T) {
	t.Setenv("ATOM_DSN", "postgres://secret")
	t.Setenv("ATOM_VAULT_KEY", "secret")
	out, err := bot().Submit(context.Background(), "task-01", "https://cashier.alipay.com/pay?id=abc",
		Intent{Service: "加油包 100 次", AmountCents: 990, Payee: "某某科技"})
	if err != nil {
		t.Fatalf("提交失败: %v\n%s", err, out)
	}
	got := argsOf(t, out)
	want := []string{
		"submit-payment",
		"--session-id", "task-01",
		"--payment-link", "https://cashier.alipay.com/pay?id=abc",
		"--intent-summary", "服务内容：加油包 100 次，支付金额：¥9.90，支付对象：某某科技",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("命令行不对:\n期望 %v\n实际 %v", want, got)
	}

	// 没说清楚的项写"未明确"，不编。
	out, _ = bot().Submit(context.Background(), "task-01", "ORDER123", Intent{AmountCents: 5})
	if s := argsOf(t, out)[6]; s != "服务内容：未明确，支付金额：¥0.05，支付对象：未明确" {
		t.Fatalf("付款说明不对: %s", s)
	}
}

func TestSubmit_拒绝会被当成选项或不合法的参数(t *testing.T) {
	ctx := context.Background()
	ok := Intent{Service: "x", AmountCents: 100, Payee: "y"}
	cases := map[string]func() error{
		// 支付链接来自工具返回的内容，不能让它往命令行里塞选项。
		"链接以 - 开头": func() error { _, err := bot().Submit(ctx, "s", "--launch-url=https://evil", ok); return err },
		"链接含换行":    func() error { _, err := bot().Submit(ctx, "s", "https://a\n--x", ok); return err },
		"链接为空":     func() error { _, err := bot().Submit(ctx, "s", "  ", ok); return err },
		"没有会话标识":   func() error { _, err := bot().Submit(ctx, "", "https://a", ok); return err },
		"金额为零":     func() error { _, err := bot().Submit(ctx, "s", "https://a", Intent{}); return err },
		"说明含换行": func() error {
			_, err := bot().Submit(ctx, "s", "https://a", Intent{Service: "a\nb", AmountCents: 1})
			return err
		},
		"订单号含符号":    func() error { _, err := bot().Query(ctx, "123;rm -rf"); return err },
		"订单号以 - 开头": func() error { _, err := bot().Query(ctx, "-p"); return err },
		"验证码为空":     func() error { _, err := bot().WalletBind(ctx, ""); return err },
	}
	for name, fn := range cases {
		if err := fn(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: 期望 ErrInvalid，实际 %v", name, err)
		}
	}
}

func TestAlipay_查询与钱包命令(t *testing.T) {
	ctx := context.Background()
	check := func(name string, out string, err error, want ...string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s 失败: %v", name, err)
		}
		if got := argsOf(t, out); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s 的命令行不对: %v", name, got)
		}
	}
	out, err := bot().Query(ctx, "20261008200040011100828200001234")
	check("查询", out, err, "query-payment-status", "--out-shake-no", "20261008200040011100828200001234")
	out, err = bot().WalletStatus(ctx)
	check("钱包状态", out, err, "check-wallet")
	out, err = bot().WalletApply(ctx, "")
	check("申请开通", out, err, "apply-wallet", "--agent-name", "AtomHelper")
	// 开通口令与验证码是两个阶段的两个命令，不能互换。
	out, err = bot().WalletApply(ctx, "ENROLL-123")
	check("提交开通口令", out, err, "apply-wallet", "-c", "ENROLL-123")
	out, err = bot().WalletBind(ctx, "654321")
	check("提交验证码", out, err, "bind-wallet", "-c", "654321")
	out, err = bot().WalletClose(ctx)
	check("关闭授权", out, err, "close-wallet")
}

func TestAlipay_失败与不可用分开报告(t *testing.T) {
	ctx := context.Background()

	// 程序跑了但报告失败：把它说的原因带回来。
	out, err := bot().Submit(ctx, "s", "FAIL", Intent{Service: "x", AmountCents: 1, Payee: "y"})
	if !errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "买家账户不匹配") {
		t.Fatalf("业务失败应带回原因: %v (%s)", err, out)
	}

	// 程序没装。
	if _, err := (Alipay{Command: "一个不存在的 alipay-bot"}).WalletStatus(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("没装应返回 ErrUnavailable: %v", err)
	}

	slow := bot()
	slow.Timeout = 300 * time.Millisecond
	if _, err := slow.Submit(ctx, "s", "HANG", Intent{Service: "x", AmountCents: 1, Payee: "y"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("超时应返回 ErrUnavailable: %v", err)
	}
}

func TestStripTerminalArt_滤掉终端二维码保留正文(t *testing.T) {
	raw := "**订单号**：2026\r\n**支付方式**：\nhttps://u.alipay.cn/_abc\n\n" +
		"\x1b[47m  \x1b[0m\x1b[40m  \x1b[0m\n\x1b[47m  \x1b[0m\nMEDIA: file:///C:/x.png\n"
	got := stripTerminalArt(raw)
	want := "**订单号**：2026\n**支付方式**：\nhttps://u.alipay.cn/_abc\n\nMEDIA: file:///C:/x.png"
	if got != want {
		t.Fatalf("期望\n%q\n实际\n%q", want, got)
	}
}
