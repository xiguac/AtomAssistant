package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// memoryCmd 处理 atomctl memory 的子命令。
//
// 记忆必须看得见、改得了——这是信任的前提。助手从任务里自己提炼出来的东西
// 不会直接生效，先挂在 pending 里等用户在这里确认。
func memoryCmd(ctx context.Context, st *pg.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("memory 需要子命令: list / pending / confirm <ID> / dismiss <ID> / forget <ID> / add <类型> <主体> <领域> <内容> [属性]")
	}
	store := st.Memories()
	switch args[0] {
	case "list":
		// list all 连已经归档、过期的也列出来，多一栏状态。
		// list full 不截断内容，用来通读全部记忆。
		full := len(args) > 1 && args[1] == "full"
		all := len(args) > 1 && args[1] == "all"
		list, err := store.List(ctx, memory.Query{IncludeInactive: all})
		if err != nil {
			return err
		}
		if full {
			for _, m := range list {
				fmt.Printf("%s [%s/%s] %s\n", m.ID, m.Kind, dash(m.Domain), memoryText(m))
			}
			return nil
		}
		if len(list) == 0 {
			fmt.Println("（暂无记忆）")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		if all {
			fmt.Fprintln(w, "ID\t状态\t类型\t主体\t领域\t置信\t内容")
		} else {
			fmt.Fprintln(w, "ID\t类型\t主体\t领域\t置信\t内容")
		}
		for _, m := range list {
			fmt.Fprintf(w, "%s\t", m.ID)
			if all {
				fmt.Fprintf(w, "%s\t", m.Status)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%.2f\t%s\n",
				m.Kind, m.Subject, dash(m.Domain), m.Confidence, oneLine(memoryText(m), 80))
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
	case "forget":
		// 归档，不物理删除：之后还能回溯。和后台记忆页上的「删除」是同一个操作。
		if len(args) < 2 {
			return fmt.Errorf("forget 需要一个记忆 ID")
		}
		if err := memory.NewPipeline(store, nil).Forget(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("已归档 %s，不再被使用\n", args[1])
		return nil
	case "add":
		// 在这里写的就是用户本人写的，和他在聊天里亲口说的同等对待。
		if len(args) < 5 {
			return fmt.Errorf("add 需要: <fact|preference|entity> <主体，本人写 user> <领域> <内容> [属性]")
		}
		cand := memory.Candidate{
			Kind: memory.Kind(args[1]), Subject: args[2], Domain: args[3], Value: args[4],
			Temporality: memory.TemporalDurable,
			Source:      memory.Source{Kind: memory.SourceUser, Ref: "atomctl", Trust: core.TrustUser},
		}
		if len(args) > 5 {
			cand.Attribute = args[5]
		}
		if cand.Kind == memory.KindPlaybook {
			return fmt.Errorf("办事的方法写成 Skill（atomctl skill put），不写进记忆")
		}
		out, err := memory.NewPipeline(store, nil).Ingest(ctx, []memory.Candidate{cand})
		if err != nil {
			return err
		}
		for _, o := range out {
			fmt.Printf("%s %s\n", o.Resolution, dash(o.MemoryID+o.PendingID))
		}
		return nil
	case "import":
		return memoryImport(ctx, store, args[1:])
	default:
		return fmt.Errorf("未知的 memory 子命令 %q", args[0])
	}
}

// memoryImport 把其他助手导出的记忆文件合并导入。只报条数，不回显内容。
// 带 --dry-run 只切分、核对账目，不写库。
func memoryImport(ctx context.Context, store memory.Store, args []string) error {
	dry := false
	var texts []string
	for _, a := range args {
		if a == "--dry-run" {
			dry = true
			continue
		}
		b, err := os.ReadFile(a)
		if err != nil {
			return err
		}
		text := strings.TrimPrefix(string(b), "ï»¿")
		one := memory.ParseExports(text)
		fmt.Printf("%s: 找到 %d 条，留下 %d，占位跳过 %d\n", a, one.Found, len(one.Candidates), one.Skipped)
		texts = append(texts, text)
	}
	if len(texts) == 0 {
		return fmt.Errorf("import 需要至少一个文件: memory import [--dry-run] <文件>...")
	}
	r := memory.ParseExports(texts...)
	fmt.Printf("合并后: 留下 %d 条，文件之间或文件内重复 %d，占位跳过 %d（共找到 %d 条）\n",
		len(r.Candidates), r.Duplicates, r.Skipped, r.Found)
	if dry {
		fmt.Println("（--dry-run，未写入）")
		return nil
	}
	out, err := memory.NewPipeline(store, nil).Ingest(ctx, r.Candidates)
	if err != nil {
		return err
	}
	written, pending := 0, 0
	for _, o := range out {
		if o.PendingID != "" {
			pending++
		} else {
			written++
		}
	}
	fmt.Printf("已写入 %d 条，待确认 %d 条\n", written, pending)
	return nil
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

// readSecret 从标准输入读一行口令。写在命令行里会留在 shell 历史和进程列表里。
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// exportCmd 把一个或全部板块导出到文件。
//
// 库里的内容是用这台机器的密钥加密的，带不到别的机器上。导出时解密，再用用户当场设的口令加密。
func exportCmd(st *pg.Store, which, path string) error {
	sections := pg.Sections()
	if which != "all" {
		sec, err := pg.ParseSection(which)
		if err != nil {
			return err
		}
		sections = []pg.Section{sec}
	}
	passphrase, err := readSecret(fmt.Sprintf("给导出文件设一个口令（至少 %d 个字符），导入时要用，回车结束: ", seal.MinPassphrase))
	if err != nil {
		return err
	}
	// 等用户输入口令的时间不该算进数据库操作的时限里。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// 不覆盖已有文件：导出文件可能是用户唯一的一份备份。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建导出文件失败: %w", err)
	}
	if err := st.Export(ctx, sections, f, passphrase); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("写入导出文件失败: %w", err)
	}
	fmt.Printf("已导出 %v 到 %s\n", sections, path)
	fmt.Println("文件已用你设的口令加密。口令没有任何找回的办法，请记牢。")
	return nil
}

// importCmd 从导出文件导入。已存在的数据不会被覆盖。
//
// 用口令解开之后，内容用这台机器自己的密钥重新加密再存进去。
func importCmd(st *pg.Store, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("打开导出文件失败: %w", err)
	}
	// 加密功能上线之前导出的文件没有加密，不用问口令。
	passphrase := ""
	if seal.IsPassphraseSealed(raw) {
		if passphrase, err = readSecret("输入导出时设的口令，回车结束: "); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rep, err := st.Import(ctx, bytes.NewReader(raw), passphrase)
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

// keyCmd 处理 atomctl key 的子命令：备份和恢复数据密钥。
//
// 数据密钥和这台机器的硬件绑在一起。正常换机器走导出导入，用不到这里；
// 这两条命令是给意外情况准备的——云主机迁移、重装系统之后硬件标识变了，密钥文件就打不开了。
func keyCmd(cfg *config.Config, args []string) error {
	if len(args) < 2 || (args[0] != "backup" && args[0] != "restore") {
		return fmt.Errorf("key 需要子命令: backup <文件> / restore <文件>")
	}
	path := args[1]
	if args[0] == "backup" {
		passphrase, err := readSecret(fmt.Sprintf("给这份备份设一个口令（至少 %d 个字符），回车结束: ", seal.MinPassphrase))
		if err != nil {
			return err
		}
		backup, err := seal.BackupKey(cfg.KeyFile, passphrase)
		if err != nil {
			return err
		}
		// 不覆盖已有文件：那可能是另一把密钥的备份。
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("创建备份文件失败: %w", err)
		}
		if _, err := f.Write(backup); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return fmt.Errorf("写入备份文件失败: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("写入备份文件失败: %w", err)
		}
		fmt.Printf("数据密钥已备份到 %s。\n", path)
		fmt.Println("把它存到这台服务器之外的地方。拿到这个文件和口令的人能解开数据库里的全部内容。")
		return nil
	}
	backup, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("打开备份文件失败: %w", err)
	}
	passphrase, err := readSecret("输入备份时设的口令，回车结束: ")
	if err != nil {
		return err
	}
	if err := seal.RestoreKey(cfg.KeyFile, passphrase, backup); err != nil {
		return err
	}
	fmt.Printf("已在 %s 重建密钥文件，并与这台机器绑定。\n", cfg.KeyFile)
	fmt.Println("如果这条命令是以 root 身份运行的，记得把文件的属主改成运行 atomd 的账号，否则服务读不到它。")
	return nil
}
