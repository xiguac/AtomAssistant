package channel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/session"
)

// 每天一个会话任务：当天在聊天里交代的事都挂在它下面。
//
// 会话本身（说过的话、每天的摘要）在 internal/session；这里是它在任务列表里的样子。
// 一天从凌晨 4 点算起（见 session.Day）。过了这一天，RollSessions 把还没办完的事挂到新一天的会话下面，
// 再把旧的那天收尾——长程任务跟着日子走，不会留在一个已经结束的会话里。

const (
	// SessionTag 标出会话任务。
	SessionTag = "session"
	// sessionDayPrefix 加上日期（2026-10-30）标出是哪一天的会话。
	sessionDayPrefix = "session:"
)

// SessionDay 返回一个会话任务是哪一天的；不是会话任务时 ok 为假。
func SessionDay(tags []string) (day string, ok bool) {
	for _, t := range tags {
		if d, found := strings.CutPrefix(t, sessionDayPrefix); found {
			return d, true
		}
	}
	return "", false
}

// openSession 返回某一天的会话任务，没有就建一个。
func (k *Kernel) openSession(ctx context.Context, day string) (string, error) {
	title := "会话 " + day
	if d, err := time.Parse(time.DateOnly, day); err == nil {
		title = "会话 " + d.Format("01-02")
	}
	tag := sessionDayPrefix + day
	id, err := k.Store.OpenSessionTask(ctx, tag, title,
		"这是 "+day+" 这一天和用户的对话。当天交代的事各自是它下面的任务。", []string{SessionTag, tag})
	if err != nil {
		return "", fmt.Errorf("channel: 打开 %s 的会话失败: %w", day, err)
	}
	return id, nil
}

// RollSessions 收尾已经过去的日子的会话：没办完的事挂到今天的会话下面，旧的会话结束。
func (k *Kernel) RollSessions(ctx context.Context, now time.Time) error {
	today := session.Day(now)
	open, err := k.Store.TasksByTag(ctx, SessionTag, true, 50)
	if err != nil {
		return err
	}
	for _, s := range open {
		day, ok := SessionDay(s.Tags)
		if !ok || day >= today {
			continue
		}
		left, err := k.Store.ChildTasks(ctx, s.ID, true)
		if err != nil {
			return err
		}
		reason := "当天的会话已结束"
		if len(left) > 0 {
			next, err := k.openSession(ctx, today)
			if err != nil {
				return err
			}
			ids := make([]string, len(left))
			for i, t := range left {
				ids[i] = t.ID
			}
			if err := k.Store.SetParent(ctx, ids, next); err != nil {
				return err
			}
			reason = fmt.Sprintf("当天的会话已结束，%d 件没办完的事移到了 %s 的会话", len(left), today)
		}
		if err := k.Store.FinishSession(ctx, s.ID, reason); err != nil {
			return err
		}
	}
	return nil
}
