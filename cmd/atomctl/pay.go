package main

import (
	"context"
	"fmt"
	"time"

	"github.com/xiguac/atomassistant/internal/config"
)

// payCmd 处理 atomctl pay 的子命令。
//
// 钱包的开通、绑定、关闭只在这里，不是助手的工具：
// 给助手付款的能力是一次授权，授权只能由用户亲手完成。
func payCmd(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) < 2 || args[0] != "wallet" {
		return fmt.Errorf("用法: pay wallet status | open [开通口令] | bind <验证码> | close")
	}
	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		return err
	}
	if file == nil || !file.Payment.Alipay.Enabled {
		return fmt.Errorf("还没有启用支付宝付款：在配置文件里加上 payment.alipay.enabled: true，并设置 ATOM_CONFIG")
	}
	alipay := file.Alipay()

	// 这些命令要和支付宝服务端通信，给它比 CLI 默认更宽的时限。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()

	var out string
	switch args[1] {
	case "status":
		out, err = alipay.WalletStatus(ctx)
	case "open":
		code := ""
		if len(args) > 2 {
			code = args[2]
		}
		out, err = alipay.WalletApply(ctx, code)
	case "bind":
		if len(args) < 3 {
			return fmt.Errorf("bind 需要支付宝给你的验证码")
		}
		out, err = alipay.WalletBind(ctx, args[2])
	case "close":
		out, err = alipay.WalletClose(ctx)
	default:
		return fmt.Errorf("未知的 wallet 子命令 %q", args[1])
	}
	// 付款程序的输出里有给用户操作的链接，必须一字不改地原样打出来。
	if out != "" {
		fmt.Println(out)
	}
	return err
}
