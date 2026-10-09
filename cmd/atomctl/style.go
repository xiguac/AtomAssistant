package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/connector/mail"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/style"
	"github.com/xiguac/atomhelper/internal/vault"
)

// styleCmd 处理 atomctl style 的子命令。
func styleCmd(ctx context.Context, st *pg.Store, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("style 需要子命令: show / learn-mail / forget")
	}
	// 这里不接模型：CLI 只管攒样本和查看，概述由 atomd 在下一个任务结束时提炼。
	learner := style.NewLearner(st.Style(), nil)
	switch args[0] {
	case "show":
		return styleShow(ctx, st)
	case "forget":
		if err := learner.Forget(ctx); err != nil {
			return err
		}
		fmt.Println("已忘掉学到的说话方式，之后会从头学。")
		return nil
	case "learn-mail":
		limit := 30
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n <= 0 || n > mail.MaxResults {
				return fmt.Errorf("数量要在 1 到 %d 之间", mail.MaxResults)
			}
			limit = n
		}
		return styleLearnMail(st, cfg, learner, limit)
	default:
		return fmt.Errorf("未知的 style 子命令 %q", args[0])
	}
}

func styleShow(ctx context.Context, st *pg.Store) error {
	store := st.Style()
	empty := true
	for _, scene := range style.Scenes() {
		samples, err := store.RecentSamples(ctx, scene, 8)
		if err != nil {
			return err
		}
		profile, err := store.GetProfile(ctx, scene)
		if err != nil {
			return err
		}
		if len(samples) == 0 && profile == nil {
			continue
		}
		empty = false
		total, err := store.CountSamples(ctx, scene, time.Time{})
		if err != nil {
			return err
		}
		fmt.Printf("【%s】共 %d 条样本\n", style.SceneName(scene), total)
		if profile != nil {
			fmt.Printf("概述（%s 提炼）：%s\n", profile.UpdatedAt.Local().Format("2006-01-02 15:04"), profile.Description)
		} else {
			fmt.Println("概述：暂未提炼（样本攒够后，在下一个任务结束时提炼）")
		}
		fmt.Println("最近的样本：")
		for _, s := range samples {
			fmt.Printf("  · %s\n", strings.Join(strings.Fields(s.Text), " "))
		}
		fmt.Println()
	}
	if empty {
		fmt.Println("（还没学到什么。助手会从你给它的指示和回复里慢慢学；也可以用 learn-mail 从你发出的邮件里学）")
	}
	return nil
}

// styleLearnMail 从"已发送"里用户自己写的信学习他写邮件的口气。
//
// 只看已发送：那里的信是用户本人写的。收件箱里的信是别人写的，一封都不看。
// 每封信只留用户亲手写的部分——引用的原文、转发的内容、客户端加的签名都去掉。
func styleLearnMail(st *pg.Store, cfg *config.Config, learner *style.Learner, limit int) error {
	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		return err
	}
	if file == nil {
		return fmt.Errorf("未设置 ATOM_CONFIG，不知道邮箱在哪")
	}
	safe, err := vault.Open(st.Vault(), cfg.VaultKey)
	if err != nil {
		return err
	}
	box, err := file.Mail(safe)
	if err != nil {
		return err
	}
	if box == nil {
		return fmt.Errorf("配置里没有 connectors.mail，没有接入邮箱")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	list, err := box.Search(ctx, mail.Query{Folder: "sent", Limit: limit})
	if err != nil {
		return err
	}
	// 助手代写的信也在"已发送"里。那不是用户写的，学进去就成了助手跟自己学。
	ghost, err := st.AssistantSentMail(ctx)
	if err != nil {
		return err
	}
	added, skipped, ghosted := 0, 0, 0
	for _, m := range list {
		msg, err := box.Read(ctx, m.ID, 0)
		if err != nil {
			return fmt.Errorf("读取 %s 失败: %w", m.ID, err)
		}
		if ghost[strings.Trim(msg.MessageID, "<>")] {
			ghosted++
			continue
		}
		own := mail.OwnWords(msg.Text)
		if own == "" {
			skipped++
			continue
		}
		n, err := learner.Observe(ctx, style.SceneMail, "mail:"+m.ID, []string{own})
		if err != nil {
			return err
		}
		if n == 0 {
			skipped++
		}
		added += n
	}
	fmt.Printf("看了已发送里最近的 %d 封信，新增 %d 条样本；跳过 %d 封（没有你自己写的内容、含敏感信息或已经学过），"+
		"另有 %d 封是助手代写的，不算你写的。\n", len(list), added, skipped, ghosted)
	fmt.Println("样本只保存在你自己的数据库里。概述会在下一个任务结束时由 atomd 提炼，用 atomctl style show 查看。")
	return nil
}
