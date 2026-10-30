package channel

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/session"
)

// 一句话该归给谁。
//
// 用户面前只有一个对话框，手头却可能同时有好几件事在办。新来的一句话是在补充其中某一件，
// 还是另起一件新的，光靠规则分不出来（"再便宜点的"是在说哪件事？）——交给主模型判断；
// 用户自己说明了的按他说的。
//
// 时间是最有用的线索：人在聊天里常常一句拆成几句发，隔几分钟再补一句。所以每件事都带上
// 用户上次说到它是多久以前；continueWindow 之内的，没有明显另起话头就按接着说算。
// 模型也拿不准时，先归给这段时间里刚说过的那件事，再不行才问用户。

// Candidate 是一件还没结束的事，供判断一句话是不是在说它。
type Candidate struct {
	ID    string
	Title string
	Goal  string
	// Status 是它现在的情况，写给模型和用户看的一句话。
	Status string
	// Idle 是用户上次说到这件事到现在过了多久。
	Idle time.Duration
	// Finished 为真表示这件事刚办完。归给它的话另起一个任务接着办，之前的对话随上下文带过去。
	Finished bool
}

// Verdict 是判断的结果：TaskID 非空表示这句话是说那件事的；New 为真表示是一件新的事；
// Reply 非空表示这句话不用办什么事（道谢、寒暄、"知道了"），直接这样回一句；
// 都没有表示拿不准。
type Verdict struct {
	TaskID string
	New    bool
	Reply  string
}

// TriageInput 是判断一句话归给谁时能看到的全部材料。
type TriageInput struct {
	// Text 是用户刚说的这句话。
	Text       string
	Candidates []Candidate
	// Today 是今天的会话里在这句话之前说过的，早的在前。没接入会话时为空。
	Today []session.Message
	// Earlier 是最近一天会话的摘要，没有则为空。
	Earlier *session.Summary
}

// Triager 判断一句话该归给哪件事，由主模型实现（agent.Triager）。
type Triager interface {
	Triage(ctx context.Context, in TriageInput) (Verdict, error)
}

// WithTriage 接入主模型的判断。不接入时按老办法：有任务在等用户就归给它，否则算新的事。
func (h *Hub) WithTriage(t Triager) *Hub {
	h.triage = t
	return h
}

const (
	// maxCandidates 是交给模型判断的事的个数上限，取最近的。
	// 给得宽一些：拖了好几天的长程任务排在后面，被截掉的话，用户再提起它就会被当成一件新的事。
	maxCandidates = 20
	// askTTL 是"这句话是说哪件事"这个问题等用户回答的时间。过了就不等了，那句话算一件新的事。
	askTTL = 10 * time.Minute
	// continueWindow 是"接着说"的时间窗：用户上次说到一件事在这之内，新的话默认是接着它说的；
	// 刚办完的事在这之内也还算在说。
	continueWindow = 30 * time.Minute
)

// noteSaid 记下用户刚说到一件事。
func (h *Hub) noteSaid(taskID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.saidAt == nil {
		h.saidAt = make(map[string]time.Time)
	}
	now := h.now()
	h.saidAt[taskID] = now
	// 只留时间窗之内的，免得越攒越多。
	for id, at := range h.saidAt {
		if now.Sub(at) > continueWindow {
			delete(h.saidAt, id)
		}
	}
}

// lastSaid 是用户上次说到一件事的时间：记下过的，或者它被建起来的时候。进程重启后只剩后者。
func (h *Hub) lastSaid(t *core.Task) time.Time {
	h.mu.Lock()
	at, ok := h.saidAt[t.ID]
	h.mu.Unlock()
	if ok && at.After(t.CreatedAt) {
		return at
	}
	return t.CreatedAt
}

// pendingAsk 是一句拿不准归给谁、正在问用户的话。
type pendingAsk struct {
	text string
	// tag 是这句话来自哪个会话，另起新事时用它。
	tag        string
	candidates []Candidate
	at         time.Time
}

// status 把一个任务现在的情况说成一句话。
func status(t *core.Task) string {
	switch {
	case t.State == core.StateWaiting && t.Wake != nil && t.Wake.Kind == core.WakeApproval:
		return "有一步操作正在等用户确认"
	case asking(t):
		return "正在等用户回答：" + oneLine(strings.TrimPrefix(t.Reason, askPrefix), 120)
	case t.State == core.StateSuspended:
		return "已暂停：" + oneLine(t.Reason, 60)
	}
	return "正在办"
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// candidatesOf 列出供判断的事：还在办的，加上时间窗之内刚办完的；用户最近说到的排在前面。
func (h *Hub) candidatesOf(active, finished []*core.Task) []Candidate {
	now := h.now()
	out := make([]Candidate, 0, len(active)+len(finished))
	for _, t := range active {
		out = append(out, Candidate{ID: t.ID, Title: t.Title, Goal: oneLine(t.Goal, 300), Status: status(t), Idle: now.Sub(h.lastSaid(t))})
	}
	for _, t := range finished {
		out = append(out, Candidate{ID: t.ID, Title: t.Title, Goal: oneLine(t.Goal, 300),
			Status: "刚办完：" + oneLine(t.Reason, 80), Idle: now.Sub(h.lastSaid(t)), Finished: true})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Idle < out[j].Idle })
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

// recent 找出时间窗之内用户刚说到的那件事。候选已按最近排好；没有返回 false。
func recent(cands []Candidate) (Candidate, bool) {
	if len(cands) == 0 || cands[0].Idle > continueWindow {
		return Candidate{}, false
	}
	return cands[0], true
}

// justFinished 从会话里最近的任务中挑出时间窗之内办完的。
func (h *Hub) justFinished(all []*core.Task) []*core.Task {
	var out []*core.Task
	for _, t := range all {
		if t.State.Terminal() && h.now().Sub(t.UpdatedAt) <= continueWindow {
			out = append(out, t)
		}
	}
	return out
}

// deliver 把一句话交给一件已有的事：它在等确认，这句话就是"不行，要这样改"；否则是回答或补充。
func (h *Hub) deliver(ctx context.Context, t *core.Task, channel, chat, text string) error {
	h.noteSaid(t.ID)
	h.log.Info("消息交给已有的任务", "任务", t.ID)
	h.follow(ctx, t, channel, chat)
	if t.State == core.StateWaiting && t.Wake != nil && t.Wake.Kind == core.WakeApproval {
		return h.tasks.Reject(ctx, t.ID, text)
	}
	return h.tasks.Reply(ctx, t.ID, text, channel)
}

// answerAsk 处理用户对"这句话是说哪件事"的回答。handled 为假表示这句话不是在回答那个问题。
func (h *Hub) answerAsk(ctx context.Context, channel, chat, text string, active []*core.Task) (handled bool, err error) {
	h.mu.Lock()
	ask := h.asked
	h.asked = nil
	h.mu.Unlock()
	if ask == nil {
		return false, nil
	}
	if h.now().Sub(ask.at) > askTTL {
		// 问了没人答，那句话按一件新的事办，不能丢。
		return false, h.create(ctx, ask.text, ask.tag)
	}
	choice := strings.TrimRight(strings.TrimSpace(text), "。.!！ ")
	switch choice {
	case "0", "新", "新的", "新任务", "新的事", "都不是":
		return true, h.create(ctx, ask.text, ask.tag)
	}
	if n, convErr := strconv.Atoi(choice); convErr == nil && n >= 1 && n <= len(ask.candidates) {
		want := ask.candidates[n-1].ID
		for _, t := range active {
			if t.ID == want {
				return true, h.deliver(ctx, t, channel, chat, ask.text)
			}
		}
		// 问的这会儿那件事已经结束了：那句话只能算新的。
		return true, h.create(ctx, ask.text, ask.tag)
	}
	// 用户没有回答，说了别的：之前那句话按一件新的事办，这一句照常处理。
	return false, h.create(ctx, ask.text, ask.tag)
}

// askWhich 问用户这句话是说哪件事。
func (h *Hub) askWhich(ctx context.Context, channel, chat, tag, text string, cands []Candidate) {
	h.mu.Lock()
	h.asked = &pendingAsk{text: text, tag: tag, candidates: cands, at: h.now()}
	h.mu.Unlock()
	var sb strings.Builder
	fmt.Fprintf(&sb, "「%s」这句话是接着哪件事说的？\n", oneLine(text, 40))
	for i, c := range cands {
		fmt.Fprintf(&sb, "%d. %s（%s）\n", i+1, oneLine(c.Title, 30), c.Status)
	}
	sb.WriteString("0. 都不是，是一件新的事\n回个序号就行。")
	h.reply(ctx, channel, chat, sb.String())
}
