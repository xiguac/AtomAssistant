// Package pay 让助手能够付款，目前接的是支付宝的 Agent 支付。
//
// 支付宝没有给智能体开放 HTTP 接口，付款方一侧的全部能力都在它官方的命令行程序
// alipay-bot 里（由 npm 包 @alipay/agent-payment 安装）。这里做的事情只有一件：
// 把助手需要的那几个子命令包成参数受控的函数。命令行的写法照抄自官方 Skill 文档
// （alipay-payment-skill / alipay-authenticate-wallet，v1.0.15）。
//
// 钱分两层把关，缺一不可：
//
//  1. 我们这一层：发起支付是不可逆操作，走内核的审批——默认每一笔都要用户确认，
//     由邮件、网页这类外部内容驱动的付款无论金额多小都不适用自动放行。
//  2. 支付宝那一层：每笔交易都要用户本人在支付宝里授权。助手拿到的只是一个
//     待确认的订单和一个给用户点的链接，它自己完成不了付款。
//
// 钱包的开通、绑定、关闭不做成助手的工具，只能由用户用运维 CLI 操作：
// 那是"授予付款能力"本身，属于 AI 不能自我提权的范围。
package pay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

var (
	// ErrUnavailable 表示付款程序没装或跑不起来。
	ErrUnavailable = errors.New("pay: 支付宝付款程序 alipay-bot 不可用")
	// ErrInvalid 表示参数不合法。
	ErrInvalid = errors.New("pay: 参数不合法")
	// ErrFailed 表示付款程序运行了，但报告失败。
	ErrFailed = errors.New("pay: 支付宝返回失败")
)

// Alipay 调用支付宝官方的 alipay-bot。
type Alipay struct {
	// Command 是 alipay-bot 的可执行文件，空值按 "alipay-bot"。
	Command string
	// AgentName 是开通钱包时向支付宝报告的智能体名称。
	AgentName string
	// Timeout 是单次调用的时限，零值按 60 秒。
	Timeout time.Duration
}

// passthroughEnv 是 alipay-bot 能继承的环境变量。
//
// 它把钱包凭据存在用户目录下，所以需要知道用户目录在哪；除此之外只给运行所必需的。
// 数据库密码、模型密钥、保险库密钥都不给——它是一个我们看不到源码的程序。
var passthroughEnv = []string{
	"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot",
	"TEMP", "TMP", "TMPDIR", "LANG",
}

// maxArg 是单个参数的长度上限。
const maxArg = 4096

// clean 校验一个要交给 alipay-bot 的参数值。
//
// 我们不经过 shell，所以不存在命令注入；要防的是另一件事——值以 "-" 开头时
// 会被对方的参数解析器当成选项。支付链接来自工具返回的内容，是不可信的，
// 不能让它往命令行里塞进 --别的什么。
func clean(name, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("%w: %s 不能为空", ErrInvalid, name)
	}
	if len(v) > maxArg {
		return "", fmt.Errorf("%w: %s 过长", ErrInvalid, name)
	}
	if strings.HasPrefix(v, "-") {
		return "", fmt.Errorf("%w: %s 不能以 - 开头", ErrInvalid, name)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: %s 含控制字符", ErrInvalid, name)
		}
	}
	return v, nil
}

// Intent 是一笔付款的说明，会展示在支付宝的授权页上，让用户知道自己在为什么付钱。
type Intent struct {
	// Service 是买的什么。
	Service string
	// AmountCents 是金额，单位分。
	AmountCents int64
	// Payee 是付给谁。
	Payee string
}

// summary 按官方要求的格式生成付款说明。
//
// 只放这三项：说明会发给支付宝并展示出来，不该混进用户的其他个人信息。
func (i Intent) summary() (string, error) {
	if i.AmountCents <= 0 {
		return "", fmt.Errorf("%w: 金额必须为正", ErrInvalid)
	}
	service, payee := strings.TrimSpace(i.Service), strings.TrimSpace(i.Payee)
	if service == "" {
		service = "未明确"
	}
	if payee == "" {
		payee = "未明确"
	}
	s := fmt.Sprintf("服务内容：%s，支付金额：¥%d.%02d，支付对象：%s",
		service, i.AmountCents/100, i.AmountCents%100, payee)
	return clean("付款说明", s)
}

// Submit 提交一个支付宝收银台链接或订单串，发起付款。
//
// 返回的是 alipay-bot 的原始输出。通常是"待确认"：里面有一个订单号和一个给用户点的链接，
// 用户在支付宝里授权之后钱才会付出去。sessionID 用来让支付宝把同一个任务里的操作关联起来。
func (a Alipay) Submit(ctx context.Context, sessionID, paymentLink string, intent Intent) (string, error) {
	session, err := clean("会话标识", sessionID)
	if err != nil {
		return "", err
	}
	link, err := clean("支付链接", paymentLink)
	if err != nil {
		return "", err
	}
	summary, err := intent.summary()
	if err != nil {
		return "", err
	}
	return a.run(ctx, "submit-payment",
		"--session-id", session, "--payment-link", link, "--intent-summary", summary)
}

// Query 按订单号查询一笔付款的状态。
func (a Alipay) Query(ctx context.Context, orderNo string) (string, error) {
	no, err := clean("订单号", orderNo)
	if err != nil {
		return "", err
	}
	// 订单号只应是数字和字母。它会被模型从上一步的输出里抄过来，抄错了要在这里拦下。
	for _, r := range no {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') {
			return "", fmt.Errorf("%w: 订单号只能含字母和数字", ErrInvalid)
		}
	}
	return a.run(ctx, "query-payment-status", "--out-shake-no", no)
}

// WalletStatus 查询 AI 钱包的开通与授权状态。
func (a Alipay) WalletStatus(ctx context.Context) (string, error) {
	return a.run(ctx, "check-wallet")
}

// WalletApply 申请开通 AI 钱包，返回一个给用户在支付宝里完成授权的入口。
// code 非空时作为用户手里的开通口令提交。
func (a Alipay) WalletApply(ctx context.Context, code string) (string, error) {
	if code != "" {
		c, err := clean("开通口令", code)
		if err != nil {
			return "", err
		}
		return a.run(ctx, "apply-wallet", "-c", c)
	}
	if name := strings.TrimSpace(a.AgentName); name != "" {
		n, err := clean("智能体名称", name)
		if err != nil {
			return "", err
		}
		return a.run(ctx, "apply-wallet", "--agent-name", n)
	}
	return a.run(ctx, "apply-wallet")
}

// WalletBind 提交开通过程中支付宝给用户的验证码或授权码。
func (a Alipay) WalletBind(ctx context.Context, code string) (string, error) {
	c, err := clean("验证码", code)
	if err != nil {
		return "", err
	}
	return a.run(ctx, "bind-wallet", "-c", c)
}

// WalletClose 关闭授权，返回支付宝的管理入口。
func (a Alipay) WalletClose(ctx context.Context) (string, error) {
	return a.run(ctx, "close-wallet")
}

// run 执行一个子命令并返回它的输出。
func (a Alipay) run(ctx context.Context, args ...string) (string, error) {
	command := a.Command
	if command == "" {
		command = "alipay-bot"
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = make([]string, 0, len(passthroughEnv))
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if err == nil {
		return text, nil
	}
	if ctx.Err() != nil {
		return text, fmt.Errorf("%w: %s 超过 %s 仍未结束", ErrUnavailable, args[0], timeout)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return text, fmt.Errorf("%w: 无法运行 %q（安装方法见 https://aipay.alipay.com/products/agent-pay）: %v",
			ErrUnavailable, command, err)
	}
	// 程序跑了但报告失败。它的输出就是原因，原样带回去。
	return text, fmt.Errorf("%w: %s", ErrFailed, text)
}
