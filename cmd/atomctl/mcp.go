package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/xiguac/atomhelper/internal/store/pg"
)

// mcpCmd 处理 atomctl mcp 的子命令：查看、移除助手自己装上的 MCP server。
// 配置文件里的 server 是你亲手加的，不在这里管。
func mcpCmd(ctx context.Context, st *pg.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("mcp 需要子命令: list / rm")
	}
	store := st.MCPInstalls()
	switch args[0] {
	case "list":
		items, err := store.ListServers(ctx)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("（助手没有安装任何 MCP server）")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "名称\t登记名\t地址\t安装时间")
		for _, s := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, s.RegistryName, s.URL,
				s.InstalledAt.Local().Format("2006-01-02 15:04"))
		}
		return w.Flush()
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("rm 需要一个 MCP server 的名称")
		}
		if err := store.RemoveServer(ctx, args[1]); err != nil {
			return err
		}
		fmt.Println("已移除。正在运行的服务要重启后才会断开它。")
		return nil
	default:
		return fmt.Errorf("未知的 mcp 子命令 %q", args[0])
	}
}
