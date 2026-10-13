package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiguac/atomhelper/internal/store/pg"
)

// purgeCmd 处理 atomctl purge：把一件事在库里的痕迹彻底清掉。
//
// 要清什么全部按编号点名，不按关键词匹配：内容是加密存的，而且"和某件事有关"是要人看过才定得下来的。
// 先用 atomctl list all、atomctl memory list、atomctl memory pending 看清编号，再来清。
// 不带 --yes 时只清点、不删除。
func purgeCmd(ctx context.Context, st *pg.Store, args []string) error {
	var spec pg.PurgeSpec
	yes := false
	for _, arg := range args {
		if arg == "--yes" {
			yes = true
			continue
		}
		kind, list, ok := strings.Cut(arg, "=")
		if !ok {
			return fmt.Errorf("purge 的参数写成 tasks=ID,ID / memories=ID,ID / pending=ID,ID，实际是 %q", arg)
		}
		var ids []string
		for _, id := range strings.Split(list, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		switch kind {
		case "tasks":
			spec.Tasks = append(spec.Tasks, ids...)
		case "memories":
			spec.Memories = append(spec.Memories, ids...)
		case "pending":
			spec.Pending = append(spec.Pending, ids...)
		default:
			return fmt.Errorf("purge 不认识 %q，可以清的是 tasks / memories / pending", kind)
		}
	}
	if len(spec.Tasks)+len(spec.Memories)+len(spec.Pending) == 0 {
		return fmt.Errorf("purge 需要指明清什么: tasks=ID,ID memories=ID,ID pending=ID,ID")
	}
	res, err := st.Purge(ctx, spec, !yes)
	if err != nil {
		return err
	}
	verb := "将清掉"
	if yes {
		verb = "已清掉"
	}
	fmt.Printf("%s：任务 %d 个（步骤 %d 条、图片 %d 张、审计日志 %d 条、评分记录 %d 条、通知记录 %d 条、说话样本 %d 条），"+
		"记忆 %d 条（版本 %d 个），候选 %d 条。\n", verb,
		res.Tasks, res.Steps, res.Images, res.Audit, res.Works, res.Notices, res.Samples,
		res.Memories, res.Revisions, res.Pending)
	if !yes {
		fmt.Println("这只是清点，什么都没有删。确认无误后加上 --yes 再执行一次；清掉的找不回来。")
	}
	return nil
}
