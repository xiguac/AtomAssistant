// Package session 是用户与助手的会话：一天一个，当天说过的话都在里面。
//
// 会话和任务是两回事。用户面前只有一个对话框，一天里会在里面说好几件事；
// 任务是其中要动手去办的那几件，有的当场办完，有的要拖好几天。
// 所以话记在会话里，事记在任务里：判断一句话该交给哪件事、短任务想知道前面聊过什么，看当天的会话；
// 一天结束后会话写成一段摘要，长程任务和拿不准的事可以回头查以前的摘要。
//
// 所有消息源共用一个会话：在 QQ 里说过的事，到微信里接着说也接得上——说话的是同一个人。
package session

import (
	"context"
	"time"
)

// dayStartHour 是一天从几点算起。半夜还在聊的话不该被切成两天，所以不从零点算。
const dayStartHour = 4

// Day 返回时刻 t 属于哪一天的会话，形如 2026-10-21。按服务器的本地时区算。
func Day(t time.Time) string {
	return t.Local().Add(-dayStartHour * time.Hour).Format(time.DateOnly)
}

// Role 是一条消息是谁说的。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message 是会话里的一条消息。
type Message struct {
	ID  string
	Day string
	At  time.Time
	// Role 是谁说的：用户，或者助手（包括任务推给用户的提问、等确认的动作和结果）。
	Role Role
	// Channel 是从哪个消息源说的或发到哪个消息源的。
	Channel string
	// TaskID 是这条消息关于哪个任务，助手推送任务进展时才有；用户说的话归给谁不在这里记。
	TaskID string
	Text   string
}

// Summary 是一天会话的摘要。
type Summary struct {
	Day  string
	Text string
	At   time.Time
}

// Store 是会话的持久化。
type Store interface {
	// Append 记下一条消息。ID 为空时由存储生成。
	Append(ctx context.Context, m Message) error
	// Messages 返回某一天的消息，早的在前；超过 limit 条时只返回最后 limit 条。
	Messages(ctx context.Context, day string, limit int) ([]Message, error)
	// PutSummary 保存一天的摘要，已有的覆盖。
	PutSummary(ctx context.Context, s Summary) error
	// Summaries 返回 from 到 to（含两端）之间各天的摘要，早的在前。
	Summaries(ctx context.Context, from, to string) ([]Summary, error)
	// Unsummarized 返回 before 之前有消息、却还没有摘要的日子，早的在前。
	Unsummarized(ctx context.Context, before string) ([]string, error)
}
