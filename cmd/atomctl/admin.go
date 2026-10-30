package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/xiguac/atomassistant/internal/admin"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// adminCmd 处理 atomctl admin 里要动数据库的子命令。
func adminCmd(ctx context.Context, st *pg.Store, args []string) error {
	if len(args) == 0 || args[0] != "reset-login" {
		return fmt.Errorf("admin 需要子命令: passwd / reset-login")
	}
	if err := admin.ResetLogin(ctx, st); err != nil {
		return err
	}
	fmt.Println("已清除后台的登录方式设置：动态验证码的绑定和登记过的通行密钥都已作废。")
	fmt.Println("现在可以只凭密码登录，登录后须重新设置登录方式。忘了密码的话，另用 admin passwd 重设。")
	return nil
}

// adminPasswd 为后台生成登录口令的摘要。它不碰数据库，服务第一次启动之前就能用。
func adminPasswd() error {
	// 口令走标准输入：写在命令行里会留在 shell 历史和进程列表里。
	fmt.Fprint(os.Stderr, "输入管理后台的登录口令（至少 10 个字符），回车结束: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("读取口令失败: %w", err)
	}
	hash, err := admin.HashPassword(strings.TrimRight(line, "\r\n"))
	if err != nil {
		return err
	}
	fmt.Println(hash)
	fmt.Fprintln(os.Stderr, "把上面这一行设为环境变量 ATOM_ADMIN_PASSWORD_HASH。它是口令的摘要，从它推不回口令。")
	return nil
}
