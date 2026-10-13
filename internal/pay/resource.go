package pay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/policy"
)

// 付费资源走的是 HTTP 402 协议：请求一个要收费的接口，对方回 402 并在
// Payment-Needed 响应头里给出账单；付款之后带着凭证再取一次，才拿到内容。
// 账单的内容对我们是不透明的，原样交给 alipay-bot，不解码、不改写。
//
// 这条路上有一个比付款更需要小心的东西：它让助手能主动向一个网址发请求。
// 不加限制的话，这就是一条把数据送出去的通道——把想外传的内容拼进网址就行。
// 所以只允许请求用户在配置里**逐个列出**的域名，只走 HTTPS，不跟随跳转；
// 带请求体时只接受 JSON，并且请求体照常要过隐私分级。

var (
	// ErrDomainNotAllowed 表示网址不在用户列出的付费资源域名内。
	ErrDomainNotAllowed = errors.New("pay: 该网址不在允许的付费资源域名内")
	// ErrNoChallenge 表示没有这次请求对应的待付账单。
	ErrNoChallenge = errors.New("pay: 没有待付的账单，需要先请求一次资源")
)

// maxResourceBody 是资源内容的大小上限。内容要进模型的上下文。
const maxResourceBody = 256 << 10

// Resources 负责请求付费资源并保管 402 账单。
type Resources struct {
	// Domains 是允许请求的域名。写 example.com 同时允许它的子域名。空表示一个都不允许。
	Domains []string
	// Dir 是存放账单文件的目录，空值按系统临时目录下的 atomhelper-402。
	Dir string
	// Client 为空时使用带超时、不跟随跳转的默认客户端。
	Client *http.Client
}

// Response 是一次资源请求的结果。
type Response struct {
	Status int
	Body   string
	// NeedsPayment 为真表示对方要求付款，账单已经存好，可以发起支付。
	NeedsPayment bool
}

// Check 校验网址：HTTPS、在允许的域名内、不带账号密码。返回规范化后的网址。
func (r Resources) Check(raw string) (string, error) {
	v, err := clean("资源网址", raw)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: 资源网址无法解析", ErrInvalid)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("%w: 资源网址必须是 https", ErrInvalid)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: 资源网址不能带账号密码", ErrInvalid)
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range r.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
			return u.String(), nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrDomainNotAllowed, host)
}

// challengeFile 返回某个任务、某个网址对应的账单文件路径。
//
// 路径由任务和网址唯一决定，所以进程重启之后发起支付时还找得到它；
// 文件名只含十六进制字符，不会被网址里的内容带偏。
func (r Resources) challengeFile(taskID, resourceURL string) string {
	dir := r.Dir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "atomhelper-402")
	}
	sum := sha256.Sum256([]byte(taskID + "\x00" + resourceURL))
	return filepath.Join(dir, hex.EncodeToString(sum[:16])+".txt")
}

// Challenge 返回已经存好的账单文件路径，以及当时请求用的请求体（没有则为空）。
//
// 请求体是和账单一起存下的。付款和取回资源时必须重放**同一个**请求，
// 所以它由这里给出，而不是让模型再说一遍——模型再说一遍就可能说得不一样。
func (r Resources) Challenge(taskID, resourceURL string) (file, body string, err error) {
	path := r.challengeFile(taskID, resourceURL)
	if _, err := os.Stat(path); err != nil {
		return "", "", ErrNoChallenge
	}
	saved, err := os.ReadFile(bodyFile(path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("pay: 读取已存的请求体失败: %w", err)
	}
	return path, string(saved), nil
}

func bodyFile(challenge string) string { return strings.TrimSuffix(challenge, ".txt") + ".body" }

// compactJSON 校验请求体是 JSON 并压成一行。空串原样返回，表示不带请求体。
func compactJSON(body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(body)); err != nil {
		return "", fmt.Errorf("%w: 请求体必须是 JSON", ErrInvalid)
	}
	return buf.String(), nil
}

// Request 请求一个资源：body 为空时用 GET，否则以 JSON 用 POST 发送。
// 对方要求付款时把账单和请求体存下来，并在结果里标明。
func (r Resources) Request(ctx context.Context, taskID, resourceURL, body string) (Response, error) {
	target, err := r.Check(resourceURL)
	if err != nil {
		return Response{}, err
	}
	payload, err := compactJSON(body)
	if err != nil {
		return Response{}, err
	}
	client := r.Client
	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Second,
			// 网址是模型写的：发出去之前过一遍出站过滤，里面带着个人敏感信息的不发。
			Transport: policy.NewEgressFilter(nil, core.PrivacyNormal, "付费资源"),
			// 不跟随跳转：允许的是这个域名，不是它愿意把我们带去的任何地方。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	method, reader := http.MethodGet, io.Reader(nil)
	if payload != "" {
		method, reader = http.MethodPost, strings.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return Response{}, fmt.Errorf("pay: 构造请求失败: %w", err)
	}
	if payload != "" {
		req.Header.Set("Content-Type", jsonContentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("pay: 请求资源失败: %w", err)
	}
	defer resp.Body.Close()

	content, err := io.ReadAll(io.LimitReader(resp.Body, maxResourceBody))
	if err != nil {
		return Response{}, fmt.Errorf("pay: 读取资源失败: %w", err)
	}
	out := Response{Status: resp.StatusCode, Body: string(content)}
	if resp.StatusCode != http.StatusPaymentRequired {
		return out, nil
	}

	needed := resp.Header.Get("Payment-Needed")
	if needed == "" {
		return out, fmt.Errorf("pay: 对方要求付款，但没有给出账单（缺少 Payment-Needed）")
	}
	path := r.challengeFile(taskID, target)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return out, fmt.Errorf("pay: 创建账单目录失败: %w", err)
	}
	// 账单原样落盘，只有当前用户可读。不解码、不改写。
	if err := os.WriteFile(path, []byte(needed), 0o600); err != nil {
		return out, fmt.Errorf("pay: 保存账单失败: %w", err)
	}
	// 请求体一并存下；没有请求体时清掉上一次留下的，免得被错误地重放。
	if payload == "" {
		if err := os.Remove(bodyFile(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return out, fmt.Errorf("pay: 清理旧请求体失败: %w", err)
		}
	} else if err := os.WriteFile(bodyFile(path), []byte(payload), 0o600); err != nil {
		return out, fmt.Errorf("pay: 保存请求体失败: %w", err)
	}
	out.NeedsPayment = true
	return out, nil
}

// PayResource 为一个付费资源发起付款，使用之前 Request 存下的账单。
//
// purpose 是用户当初想做什么，会展示在支付宝的授权页上。
// 和收银台付款一样，返回的通常是"待确认"：用户在支付宝里授权之后钱才付出去。
func (a Alipay) PayResource(ctx context.Context, sessionID, challengeFile, resourceURL, purpose, body string) (string, error) {
	session, err := clean("会话标识", sessionID)
	if err != nil {
		return "", err
	}
	file, err := clean("账单文件", challengeFile)
	if err != nil {
		return "", err
	}
	target, err := clean("资源网址", resourceURL)
	if err != nil {
		return "", err
	}
	why, err := clean("付款目的", purpose)
	if err != nil {
		return "", err
	}
	replay, err := replayArgs(body)
	if err != nil {
		return "", err
	}
	return a.run(ctx, append([]string{"402-buyer-pay",
		"--session-id", session, "--file", file,
		"--resource-url", target, "--intent-summary", "原始请求：" + why}, replay...)...)
}

// jsonContentType 是带请求体时使用的内容类型。
const jsonContentType = "application/json"

// replayArgs 生成让 alipay-bot 重放原请求所需的参数。
//
// 付款之后它要带着凭证把原请求再发一遍，方法、请求体、内容类型都必须和第一次一致。
func replayArgs(body string) ([]string, error) {
	payload, err := compactJSON(body)
	if err != nil || payload == "" {
		return nil, err
	}
	if _, err := clean("请求体", payload); err != nil {
		return nil, err
	}
	return []string{"--method", "POST", "--data", payload, "--header", "Content-Type:" + jsonContentType}, nil
}

// FetchPaid 在用户付款之后取回资源。alipay-bot 会查询付款状态、带着凭证请求资源，
// 并在拿到内容后向对方发送履约回执。
func (a Alipay) FetchPaid(ctx context.Context, orderNo, resourceURL, body string) (string, error) {
	no, err := clean("订单号", orderNo)
	if err != nil {
		return "", err
	}
	for _, r := range no {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%w: 订单号只能是数字", ErrInvalid)
		}
	}
	target, err := clean("资源网址", resourceURL)
	if err != nil {
		return "", err
	}
	replay, err := replayArgs(body)
	if err != nil {
		return "", err
	}
	return a.run(ctx, append([]string{"402-query-payment-status",
		"--out-shake-no", no, "--resource-url", target}, replay...)...)
}
