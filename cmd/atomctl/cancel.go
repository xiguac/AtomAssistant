package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/store"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// cancelTasks 取消任务：给出 ID 就取消那一个，写 all 就取消全部还没结束的。
//
// 取消是把任务直接迁到终态，之后不会再被调度。正在执行的那一步如果已经发出去了收不回来，
// 但不会再有下一步：执行者提交下一步的结果时会发现状态已经变了。
func cancelTasks(ctx context.Context, st *pg.Store, target, reason string) error {
	if reason == "" {
		reason = "用户取消"
	}
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
		if t.State.Terminal() {
			fmt.Printf("%s 已经结束（%s），不用取消\n", t.ID, t.State)
			continue
		}
		err := st.Transition(ctx, t.ID, t.State, core.StateCancelled, reason)
		if errors.Is(err, store.ErrStateConflict) {
			// 刚好在这一瞬间被执行者改了状态：按新的状态再来一次。
			if fresh, gerr := st.GetTask(ctx, t.ID); gerr == nil && !fresh.State.Terminal() {
				err = st.Transition(ctx, t.ID, fresh.State, core.StateCancelled, reason)
			} else if gerr == nil {
				err = nil
			}
		}
		if err != nil {
			return fmt.Errorf("取消 %s 失败: %w", t.ID, err)
		}
		cancelled++
		fmt.Printf("%s 已取消（原状态 %s）：%s\n", t.ID, t.State, oneLine(t.Title, 30))
	}
	if target == "all" && cancelled == 0 {
		fmt.Println("（没有进行中的任务）")
	}
	return nil
}
