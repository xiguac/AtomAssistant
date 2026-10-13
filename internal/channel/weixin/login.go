package weixin

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// botType 是申请二维码时报的机器人类型，官方客户端用的就是这个值。
const botType = "3"

// Login 是一次进行中的扫码登录。
type Login struct {
	// QRContent 是要画成二维码给用户扫的内容（一个链接）。
	QRContent string
	// StartedAt 是申请这个二维码的时间，过几分钟它就失效了。
	StartedAt time.Time

	qrcode string
	// base 是查状态用的地址，服务端可能在扫码之后让我们换到别的机房去查。
	base string
	http *http.Client
}

// 扫码登录的各个阶段。
const (
	StatusWait      = "wait"
	StatusScanned   = "scanned"
	StatusNeedCode  = "need_code"
	StatusExpired   = "expired"
	StatusBlocked   = "blocked"
	StatusAlready   = "already"
	StatusConfirmed = "confirmed"
)

// LoginResult 是查一次登录状态的结果。
type LoginResult struct {
	Status string
	// 以下几项只在 Status 为 StatusConfirmed 时有：机器人凭证、机器人的标识、
	// 之后调接口用的地址，以及扫码的那个人的用户标识。
	Token, BotID, BaseURL, UserID string
}

// StartLogin 申请一个登录用的二维码。base 为空用默认地址。
func StartLogin(ctx context.Context, client *http.Client, base string) (*Login, error) {
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimSuffix(base, "/")
	var out struct {
		QRCode  string `json:"qrcode"`
		Content string `json:"qrcode_img_content"`
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// local_token_list 是本机已经登录过的机器人凭证，用来让服务端判断是不是重复连接；我们不报。
	if err := post(callCtx, client, base+"/ilink/bot/get_bot_qrcode?bot_type="+botType, "",
		map[string]any{"local_token_list": []string{}}, &out); err != nil {
		return nil, fmt.Errorf("weixin: 申请登录二维码失败: %w", err)
	}
	if out.QRCode == "" || out.Content == "" {
		return nil, fmt.Errorf("weixin: 申请登录二维码失败：应答里没有二维码")
	}
	return &Login{QRContent: out.Content, StartedAt: time.Now(), qrcode: out.QRCode, base: base, http: client}, nil
}

// Poll 查一次登录状态。服务端会把请求挂住，直到状态有变化或者三十多秒过去。
// verifyCode 是手机上显示的那串数字，只有上一次的结果是 StatusNeedCode 时才要给。
func (l *Login) Poll(ctx context.Context, verifyCode string) (LoginResult, error) {
	endpoint := l.base + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(l.qrcode)
	if verifyCode != "" {
		endpoint += "&verify_code=" + url.QueryEscape(verifyCode)
	}
	callCtx, cancel := context.WithTimeout(ctx, longPoll)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return LoginResult{}, err
	}
	commonHeaders(req.Header)
	var out struct {
		Status       string `json:"status"`
		BotToken     string `json:"bot_token"`
		BotID        string `json:"ilink_bot_id"`
		BaseURL      string `json:"baseurl"`
		UserID       string `json:"ilink_user_id"`
		RedirectHost string `json:"redirect_host"`
	}
	if err := do(l.http, req, &out); err != nil {
		// 没人扫码时等到超时是常态；网关超时之类的临时故障也当作还在等，下一次再查。
		if ctx.Err() != nil {
			return LoginResult{}, ctx.Err()
		}
		return LoginResult{Status: StatusWait}, nil
	}
	switch out.Status {
	case "wait":
		return LoginResult{Status: StatusWait}, nil
	case "scaned":
		return LoginResult{Status: StatusScanned}, nil
	case "scaned_but_redirect":
		// 扫过了，但这个账号在另一个机房：之后到那边去查。
		if out.RedirectHost != "" {
			l.base = "https://" + out.RedirectHost
		}
		return LoginResult{Status: StatusScanned}, nil
	case "need_verifycode":
		return LoginResult{Status: StatusNeedCode}, nil
	case "verify_code_blocked":
		return LoginResult{Status: StatusBlocked}, nil
	case "expired":
		return LoginResult{Status: StatusExpired}, nil
	case "binded_redirect":
		return LoginResult{Status: StatusAlready}, nil
	case "confirmed":
		if out.BotToken == "" || out.BotID == "" {
			return LoginResult{}, fmt.Errorf("weixin: 登录已确认，但服务端没有给出凭证")
		}
		return LoginResult{Status: StatusConfirmed, Token: out.BotToken, BotID: out.BotID, BaseURL: out.BaseURL, UserID: out.UserID}, nil
	}
	return LoginResult{}, fmt.Errorf("weixin: 不认识的登录状态 %q", out.Status)
}
