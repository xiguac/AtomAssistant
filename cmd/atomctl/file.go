package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/seal"
)

// fileCmd 处理 atomctl file 的子命令：往助手的工作区里放文件、看里面有什么。
func fileCmd(cfg *config.Config, box *seal.Box, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("file 需要子命令: add / list / dir")
	}
	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		return err
	}
	if file == nil {
		return fmt.Errorf("未设置 ATOM_CONFIG，不知道工作区在哪")
	}
	ws, err := file.Workspace(box)
	if err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("配置里没有 files 一节，没有启用文件处理")
	}
	switch args[0] {
	case "dir":
		fmt.Println(ws.Root)
		return nil
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("add 需要一个文件路径，可选一个在工作区里的名字")
		}
		rel := ""
		if len(args) > 2 {
			rel = args[2]
		}
		saved, err := ws.Import(args[1], rel)
		if err != nil {
			return err
		}
		fmt.Printf("已放进工作区: %s\n", saved.Path)
		if saved.Replaced != "" {
			fmt.Printf("原来的同名文件留底在: %s\n", saved.Replaced)
		}
		return nil
	case "list":
		dir := ""
		if len(args) > 1 {
			dir = args[1]
		}
		entries, err := ws.List(dir)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Println("（空的）")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "路径\t大小\t修改时间")
		for _, e := range entries {
			size := fmt.Sprintf("%d", e.Size)
			if e.Dir {
				size = "<目录>"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", e.Path, size, e.Modified.Local().Format("2006-01-02 15:04"))
		}
		return w.Flush()
	default:
		return fmt.Errorf("未知的 file 子命令 %q", args[0])
	}
}
