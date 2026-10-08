package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/xiguac/atomhelper/internal/schedule"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

// list 列出任务，最近创建的在前。
func list(ctx context.Context, st *pg.Store, all bool) error {
	tasks, err := st.ListTasks(ctx, !all, 50)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		if all {
			fmt.Println("（还没有任务）")
		} else {
			fmt.Println("（没有进行中的任务）")
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t状态\t创建时间\t标题\t当前情况")
	for _, t := range tasks {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.State,
			t.CreatedAt.Local().Format("01-02 15:04"), oneLine(t.Title, 20), oneLine(t.Reason, 50))
	}
	return w.Flush()
}

// scheduleCmd 处理 atomctl schedule 的子命令。
func scheduleCmd(ctx context.Context, st *pg.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("schedule 需要子命令: add / list / rm")
	}
	store := st.Schedules()
	switch args[0] {
	case "add":
		if len(args) < 4 {
			return fmt.Errorf("add 需要间隔、首次时间和标题，例如: schedule add 24h 08:00 早报 \"看一眼今天的天气和日程\"")
		}
		every, err := time.ParseDuration(args[1])
		if err != nil {
			return fmt.Errorf("间隔 %q 不是合法时长（如 24h、30m）", args[1])
		}
		first, err := parseFirstRun(args[2], time.Now())
		if err != nil {
			return err
		}
		goal := ""
		if len(args) > 4 {
			goal = args[4]
		}
		// 从 CLI 创建视为用户亲手创建，它产生的任务是 user 等级。
		sc := schedule.New(args[3], goal, every, first)
		if err := store.Add(ctx, sc); err != nil {
			return err
		}
		fmt.Printf("%s  首次触发 %s，之后每 %s 一次\n", sc.ID, sc.NextRunAt.Local().Format("2006-01-02 15:04"), every)
		return nil
	case "list":
		items, err := store.List(ctx)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("（没有定时任务）")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t间隔\t下次触发\t启用\t标题\t上次的任务")
		for _, sc := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\t%s\n", sc.ID, sc.Every,
				sc.NextRunAt.Local().Format("01-02 15:04"), sc.Enabled, oneLine(sc.Title, 24), dash(sc.LastTaskID))
		}
		return w.Flush()
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("rm 需要一个定时任务 ID")
		}
		return store.Remove(ctx, args[1])
	default:
		return fmt.Errorf("未知的 schedule 子命令 %q", args[0])
	}
}

// parseFirstRun 解析首次触发时间：now、HH:MM（下一个这个时刻）或 RFC3339。
func parseFirstRun(s string, now time.Time) (time.Time, error) {
	if s == "now" {
		return now, nil
	}
	if clock, err := time.ParseInLocation("15:04", s, now.Location()); err == nil {
		at := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
		if !at.After(now) {
			// 今天这个时刻已经过了，从明天开始。
			at = at.AddDate(0, 0, 1)
		}
		return at, nil
	}
	if at, err := time.Parse(time.RFC3339, s); err == nil {
		return at, nil
	}
	return time.Time{}, fmt.Errorf("首次时间 %q 无法识别：可写 now、08:00 这样的时刻，或 2026-10-09T08:00:00+08:00", s)
}
