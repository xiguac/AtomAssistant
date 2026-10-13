package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// cancelTasks 取消任务：给出 ID 就取消那一个，写 all 就取消全部还没结束的。
//
// 取消的含义见 kernel.Cancel。
func cancelTasks(ctx context.Context, st *pg.Store, target, reason string) error {
	var tasks []*core.Task
	if target == "all" {
		list, err := st.ListTasks(ctx, true, 1000)
		if err != nil {
			return err
		}
		tasks = list
	} else {
		t, err := st.GetTask(ctx, target)
		if err != nil {
			return err
		}
		tasks = []*core.Task{t}
	}
	cancelled := 0
	for _, t := range tasks {
		was, err := kernel.Cancel(ctx, st, t.ID, reason)
		if errors.Is(err, kernel.ErrAlreadyEnded) {
			fmt.Printf("%s 已经结束（%s），不用取消\n", t.ID, was)
			continue
		}
		if err != nil {
			return fmt.Errorf("取消 %s 失败: %w", t.ID, err)
		}
		cancelled++
		fmt.Printf("%s 已取消（原状态 %s）：%s\n", t.ID, was, oneLine(t.Title, 30))
	}
	if target == "all" && cancelled == 0 {
		fmt.Println("（没有进行中的任务）")
	}
	return nil
}
