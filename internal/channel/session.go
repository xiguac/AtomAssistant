package channel

import (
	"context"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/session"
)

// 会话记录：用户说的每一句、助手回的每一句都记进当天的会话，见 internal/session。
//
// 判断一句话归给谁时，模型看得到今天前面聊过什么、昨天的摘要，而不只是一张任务清单。

// transcriptLimit 是判断归属时带上的今天会话的条数上限，取最后这么多条。
const transcriptLimit = 200

// WithSession 接入会话记录。不接入时不记，判断归属时也只看任务清单。
func (h *Hub) WithSession(s session.Store) *Hub {
	h.sessions = s
	return h
}

// remember 把一条消息记进今天的会话，返回它的编号。记不下来只影响以后的判断和摘要，
// 不该让这句话办不成，所以只记日志。
func (h *Hub) remember(ctx context.Context, role session.Role, channel, taskID, text string) string {
	if h.sessions == nil || text == "" {
		return ""
	}
	m := session.Message{ID: core.NewID(), At: h.now(), Role: role, Channel: channel, TaskID: taskID, Text: text}
	m.Day = session.Day(m.At)
	if err := h.sessions.Append(ctx, m); err != nil {
		h.log.Error("记录会话失败", "渠道", channel, "err", err)
		return ""
	}
	return m.ID
}

// triageInput 备好判断归属的材料：今天在这句话之前说过的，以及最近一天的摘要。
// 读不出来就只给任务清单，照样能判断。
func (h *Hub) triageInput(ctx context.Context, text, current string, cands []Candidate) TriageInput {
	in := TriageInput{Text: text, Candidates: cands}
	if h.sessions == nil {
		return in
	}
	today := session.Day(h.now())
	msgs, err := h.sessions.Messages(ctx, today, transcriptLimit+1)
	if err != nil {
		h.log.Error("读取今天的会话失败", "err", err)
	}
	for _, m := range msgs {
		if m.ID != current {
			in.Today = append(in.Today, m)
		}
	}
	if len(in.Today) > transcriptLimit {
		in.Today = in.Today[len(in.Today)-transcriptLimit:]
	}
	// 昨天没说话的话，往前找最近的一天；再早的不在这里给，任务里需要时自己去查。
	from := session.Day(h.now().AddDate(0, 0, -7))
	sums, err := h.sessions.Summaries(ctx, from, today)
	if err != nil {
		h.log.Error("读取会话摘要失败", "err", err)
	}
	for i := len(sums) - 1; i >= 0; i-- {
		if sums[i].Day < today {
			in.Earlier = &sums[i]
			break
		}
	}
	return in
}

// reply 直接回用户一句话，不建任务，并记进会话。
func (h *Hub) reply(ctx context.Context, channel, chat, text string) {
	h.say(ctx, channel, chat, text)
	h.remember(ctx, session.RoleAssistant, channel, "", text)
}
