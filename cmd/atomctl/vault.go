package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/vault"
)

// vaultCmd 处理 atomctl vault 的子命令。
//
// 这里刻意没有"读出明文"的命令。保险库只进不出：存进去之后，
// 能做的只有让被授权的工具用它，或者删掉它。
func vaultCmd(ctx context.Context, st *pg.Store, key string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("vault 需要子命令: keygen / put / list / rm")
	}
	if args[0] == "keygen" {
		k, err := vault.GenerateKey()
		if err != nil {
			return err
		}
		fmt.Println(k)
		fmt.Fprintln(os.Stderr, "把上面这一行设为环境变量 ATOM_VAULT_KEY（atomd 与 atomctl 都要）。")
		fmt.Fprintln(os.Stderr, "密钥丢了，保险库里的内容就再也解不开；迁移服务器时要把它一起带走。")
		return nil
	}

	v, err := vault.Open(st.Vault(), key)
	if err != nil {
		return err
	}
	switch args[0] {
	case "put":
		if len(args) < 3 {
			return fmt.Errorf("put 需要名字和用途说明，可选一个逗号分隔的工具名单")
		}
		var tools []string
		if len(args) > 3 {
			for _, t := range strings.Split(args[3], ",") {
				if t = strings.TrimSpace(t); t != "" {
					tools = append(tools, t)
				}
			}
		}
		// 值走标准输入：写在命令行里会留在 shell 历史和进程列表里。
		fmt.Fprint(os.Stderr, "输入要存入的值，回车结束: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("读取值失败: %w", err)
		}
		value := strings.TrimRight(line, "\r\n")
		if err := v.Seal(ctx, args[1], value, args[2], tools); err != nil {
			return err
		}
		if len(tools) == 0 {
			fmt.Println("已存入。没有授权任何工具，目前谁都用不了它。")
		} else {
			fmt.Printf("已存入，可用工具: %s\n", strings.Join(tools, "、"))
		}
		return nil
	case "list":
		metas, err := v.List(ctx)
		if err != nil {
			return err
		}
		if len(metas) == 0 {
			fmt.Println("（保险库是空的）")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "名字\t用途\t可用工具\t存入时间")
		for _, m := range metas {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Label, dash(m.Note),
				dash(strings.Join(m.AllowedTools, "、")), m.CreatedAt.Local().Format("2006-01-02 15:04"))
		}
		return w.Flush()
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("rm 需要一个条目名")
		}
		return v.Remove(ctx, args[1])
	default:
		return fmt.Errorf("未知的 vault 子命令 %q", args[0])
	}
}
