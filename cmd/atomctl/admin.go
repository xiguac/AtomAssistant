package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/xiguac/atomhelper/internal/admin"
)

// adminCmd 处理 atomctl admin 的子命令。
func adminCmd(args []string) error {
	if len(args) == 0 || args[0] != "passwd" {
		return fmt.Errorf("admin 需要子命令: passwd")
	}
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
