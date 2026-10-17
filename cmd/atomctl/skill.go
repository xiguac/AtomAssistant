package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/market"
	"github.com/xiguac/atomhelper/internal/skill"
	"github.com/xiguac/atomhelper/internal/skillscan"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// skillCmd 处理 atomctl skill 的子命令。
func skillCmd(ctx context.Context, st *pg.Store, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("skill 需要子命令: install / fetch <owner/repo/name> [--accept] / put <SKILL.md> / list / show / rm")
	}
	store := st.Skills()
	switch args[0] {
	case "install":
		if len(args) < 2 {
			return fmt.Errorf("install 需要一个 Skill 目录或清单文件的路径")
		}
		return installSkill(ctx, store, cfg, args[1], len(args) > 2 && args[2] == "--yes")
	case "fetch":
		if len(args) < 2 {
			return fmt.Errorf("fetch 需要公开 Skill 库里的 id，如 anthropics/skills/docx")
		}
		return fetchSkill(ctx, store, cfg, args[1], len(args) > 2 && args[2] == "--accept")
	case "put":
		if len(args) < 2 {
			return fmt.Errorf("put 需要一个 SKILL.md 文件的路径")
		}
		return putLearnedSkill(ctx, store, args[1])
	case "list":
		items, err := store.ListSkills(ctx)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("（没有安装任何 Skill）")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t名称\t版本\t步骤数\t声明的工具\t安装时间")
		for _, s := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", s.ID, s.Name, dash(s.Version), len(s.Steps),
				dash(strings.Join(s.RequiredTools, "、")), s.InstalledAt.Local().Format("2006-01-02 15:04"))
		}
		return w.Flush()
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("show 需要一个 Skill ID")
		}
		s, err := store.GetSkill(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Println(s.Raw)
		return nil
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("rm 需要一个 Skill ID")
		}
		return store.RemoveSkill(ctx, args[1])
	default:
		return fmt.Errorf("未知的 skill 子命令 %q", args[0])
	}
}

// installSkill 扫描、展示、确认、安装。
func installSkill(ctx context.Context, store skill.Store, cfg *config.Config, path string, yes bool) error {
	// 扫描可能要几分钟，不受 CLI 默认的短超时限制。
	scanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Minute)
	defer cancel()

	scanner := skillscan.Scanner{Command: cfg.SkillScanner, ExtraArgs: cfg.SkillScannerArgs}
	fmt.Fprintln(os.Stderr, "正在扫描……")
	s, err := skill.Load(scanCtx, path, scanner)
	if err != nil {
		if s != nil && s.ScanReport != "" {
			fmt.Println(s.ScanReport)
		}
		if errors.Is(err, skillscan.ErrScanFailed) {
			return fmt.Errorf("%w\n没有扫描过的 Skill 不能安装。扫描器 SkillSpector 的安装方法见 "+
				"https://github.com/NVIDIA/skillspector ；装在别处时用 ATOM_SKILL_SCANNER 指定路径", err)
		}
		return err
	}

	// 全文展示，不折叠不省略：用户批准的应该是他看到的全部内容。
	fmt.Println("──────── 清单全文 ────────")
	fmt.Println(strings.TrimRight(s.Raw, "\n"))
	fmt.Println("──────────────────────────")

	usable, missing, known := availability(cfg, s)
	switch {
	case len(s.RequiredTools) == 0:
		fmt.Println("它没有声明任何工具：按它执行的任务调不了任何外部工具。")
	case !known:
		fmt.Printf("它声明要用这些工具：%s\n", strings.Join(s.RequiredTools, "、"))
		fmt.Println("（没有设置 ATOM_CONFIG，无法判断其中哪些你已经授权）")
	default:
		if len(usable) > 0 {
			fmt.Printf("可用的工具（你已授权）：%s\n", strings.Join(usable, "、"))
		}
		if len(missing) > 0 {
			fmt.Printf("用不了的工具（你没有授权，装上也调不动）：%s\n", strings.Join(missing, "、"))
		}
	}
	fmt.Println("按它执行的任务只能调上面声明过、且你授权过的工具，装上它不会带来任何新权限。")
	fmt.Println("扫描已通过。扫描拦不住措辞正常的恶意内容，请自己过一遍上面的步骤。")

	if !yes {
		fmt.Fprint(os.Stderr, "确认安装？输入 yes: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			return fmt.Errorf("已取消")
		}
	}
	if err := skill.Install(ctx, store, s); err != nil {
		return err
	}
	fmt.Printf("已安装 %s。用 atomctl run %s <标题> [目标] 让一个任务按它执行。\n", s.ID, s.ID)
	return nil
}

// fetchSkill 从公开 Skill 库装一个 SKILL.md：和助手自己安装走同一套下载、扫描与风险评估。
// 扫描判出会把数据外传或高危的内容时只列出风险点，要装须再带 --accept。
func fetchSkill(ctx context.Context, store skill.Store, cfg *config.Config, id string, accept bool) error {
	scanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Minute)
	defer cancel()
	in := &market.Installer{
		Client:  &market.Client{},
		Scanner: skillscan.Scanner{Command: cfg.SkillScanner, ExtraArgs: cfg.SkillScannerArgs},
		Skills:  store,
	}
	fmt.Fprintln(os.Stderr, "正在下载并扫描……")
	s, pending, err := in.InstallSkill(scanCtx, id, false)
	if err != nil {
		return err
	}
	if pending != nil {
		fmt.Println("扫描发现需要你拿主意的风险点：")
		for _, r := range pending.Risks {
			fmt.Println("  - " + r)
		}
		if !accept {
			return fmt.Errorf("没有安装。确认要装的话加 --accept 再执行一次")
		}
		if s, _, err = in.InstallSkill(scanCtx, id, true); err != nil {
			return err
		}
	}
	fmt.Printf("已安装 %s（%s）\n", s.ID, s.Name)
	return nil
}

// putLearnedSkill 把一份 SKILL.md 存成助手总结类的 Skill，用于手工整理助手总结的 Skill。
// 不过扫描：扫描防的是别人写的东西，这里是用户自己写的。
func putLearnedSkill(ctx context.Context, store skill.Store, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// id 取自元数据里的 name；名称取正文的第一个标题，没有就用 id。
	const unnamed = "unnamed"
	s, err := skill.ParseMarkdown(raw, unnamed)
	if err != nil {
		return err
	}
	id := s.Name
	if id == unnamed {
		return fmt.Errorf("SKILL.md 开头的元数据里要写 name（作为 id）和 description")
	}
	s.ID, s.Name = id, id
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if title, ok := strings.CutPrefix(line, "# "); ok {
			s.Name = strings.TrimSpace(title)
			break
		}
	}
	if existing, err := store.GetSkill(ctx, id); err == nil && !existing.Learned() {
		return fmt.Errorf("已有一个不是助手总结的 %s，不覆盖", id)
	}
	s.Author, s.Source, s.InstalledAt = "原子助手", skill.SourceLearned+"（人工整理）", time.Now().UTC()
	if err := s.Validate(); err != nil {
		return err
	}
	if err := store.PutSkill(ctx, s); err != nil {
		return err
	}
	fmt.Printf("已保存 %s（%s）\n", s.ID, s.Name)
	return nil
}

// availability 按当前配置把 Skill 声明的工具分成已授权与未授权。
func availability(cfg *config.Config, s *skill.Skill) (usable, missing []string, known bool) {
	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil || file == nil {
		return nil, nil, false
	}
	registry, err := file.BuildRegistry()
	if err != nil {
		return nil, nil, false
	}
	usable, missing = s.Availability(func(tool string) bool {
		_, err := registry.Grant(tool)
		return err == nil
	})
	return usable, missing, true
}

// runSkill 创建一个按指定 Skill 执行的任务。
func runSkill(ctx context.Context, st *pg.Store, skillID, title, goal string) error {
	s, err := st.Skills().GetSkill(ctx, skillID)
	if err != nil {
		return err
	}
	if goal == "" {
		goal = s.Goal
	}
	t := core.NewTask(title, goal, core.TrustUser)
	t.Budget = core.DefaultBudget()
	// 这个标签让任务的可用工具收窄为 Skill 声明过的那些，见 agent.Planner.scoped。
	t.Tags = []string{skill.TagPrefix + s.ID}
	if err := st.CreateTask(ctx, t); err != nil {
		return err
	}
	if err := st.Transition(ctx, t.ID, core.StateNew, core.StateReady, "用户创建"); err != nil {
		return err
	}
	fmt.Println(t.ID)
	return nil
}
