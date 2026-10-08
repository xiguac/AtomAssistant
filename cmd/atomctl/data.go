package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// memoryCmd 处理 atomctl memory 的子命令。
//
// 记忆必须看得见、改得了——这是信任的前提。助手从任务里自己提炼出来的东西
// 不会直接生效，先挂在 pending 里等用户在这里确认。
func memoryCmd(ctx context.Context, st *pg.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("memory 需要子命令: list / pending / confirm <ID> / dismiss <ID>")
	}
	store := st.Memories()
	switch args[0] {
	case "list":
		list, err := store.List(ctx, memory.Query{})
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("（暂无记忆）")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t类型\t主体\t领域\t置信\t内容")
		for _, m := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.2f\t%s\n",
				m.ID, m.Kind, m.Subject, dash(m.Domain), m.Confidence, oneLine(memoryText(m), 80))
		}
		return w.Flush()
	case "pending":
		list, err := store.ListPending(ctx, "")
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("（没有待确认的记忆）")
			return nil
		}
		for _, p := range list {
			c := p.Candidate
			fmt.Printf("%s  [%s] %s·%s\n  %s\n  原因: %s\n\n",
				p.ID, c.Kind, c.Subject, dash(c.Domain), c.Value, p.Arb.Reason)
		}
		return nil
	case "confirm":
		if len(args) < 2 {
			return fmt.Errorf("confirm 需要一个待确认记忆的 ID")
		}
		id, err := memory.NewPipeline(store, nil).Confirm(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("已记住，记忆 ID %s\n", id)
		return nil
	case "dismiss":
		if len(args) < 2 {
			return fmt.Errorf("dismiss 需要一个待确认记忆的 ID")
		}
		return memory.NewPipeline(store, nil).Dismiss(ctx, args[1])
	default:
		return fmt.Errorf("未知的 memory 子命令 %q", args[0])
	}
}

func memoryText(m *memory.Memory) string {
	text := m.Current.Value
	if len(m.Condition) > 0 {
		text = "条件" + m.Condition.String() + " → " + text
	}
	if m.Attribute != "" {
		text = m.Attribute + ": " + text
	}
	return text
}

// oneLine 把多行文本压成一行并按字符截断，用于表格展示。
func oneLine(s string, max int) string {
	r := []rune(s)
	for i, c := range r {
		if c == '\n' || c == '\r' || c == '\t' {
			r[i] = ' '
		}
	}
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return string(r)
}

// exportCmd 把一个或全部板块导出到文件。
func exportCmd(ctx context.Context, st *pg.Store, which, path string) error {
	sections := pg.Sections()
	if which != "all" {
		sec, err := pg.ParseSection(which)
		if err != nil {
			return err
		}
		sections = []pg.Section{sec}
	}
	// 不覆盖已有文件：导出文件可能是用户唯一的一份备份。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建导出文件失败: %w", err)
	}
	if err := st.Export(ctx, sections, f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("写入导出文件失败: %w", err)
	}
	fmt.Printf("已导出 %v 到 %s\n", sections, path)
	return nil
}

// importCmd 从导出文件导入。已存在的数据不会被覆盖。
func importCmd(ctx context.Context, st *pg.Store, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开导出文件失败: %w", err)
	}
	defer f.Close()

	rep, err := st.Import(ctx, f)
	if err != nil {
		return err
	}
	tables := make([]string, 0, len(rep.Inserted))
	for t := range rep.Inserted {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		fmt.Printf("%s: 新增 %d 行\n", t, rep.Inserted[t])
	}
	return nil
}
