// Package channel 是消息渠道层：让用户在 QQ、飞书、钉钉这类聊天软件里直接和助手说话。
//
// 它做两件事：
//
//  1. **进来的话变成任务。** 用户发一句话，要么是新的一件事（建任务），要么是在回答
//     某个任务的提问，要么是在对一个等确认的动作表态（同意，或者说哪里要改）。
//  2. **任务的进展推回去。** 任务问了问题、有动作等确认、办完了或办不成了，
//     都主动发到它来的那个会话里——用户不用自己去查。
//
// 各家聊天软件的差别被关在 Adapter 后面，这一层不知道也不关心消息是从哪家来的。
//
// # 谁的话算数
//
// 渠道本身必须是用户亲手写进配置文件的（见 docs/安全模型.md §3.1.1），
// 这里没有、也不会有让助手自己开通渠道的工具。
// 但"渠道可信"不等于"渠道里每个人说的都算数"：一个 QQ 机器人谁都能加、谁都能给它发消息。
// 所以每个渠道还要列出主人的账号（owners），只有主人发来的话才会变成任务，
// 并且带着用户等级的信任。其他人发来的一律不处理，只告诉对方这个助手不对外服务。
package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/policy"
)

// Message 是从渠道进来的一条消息。
type Message struct {
	// Chat 是会话的标识，回复时发回这里。同一个渠道内唯一。
	Chat string
	// Sender 是发消息的人在这个渠道里的账号标识。
	Sender string
	Text   string
	// ID 是渠道给这条消息的编号，有的渠道回复时要带上它。
	ID string
	At time.Time
}

// Outgoing 是要发到渠道里的一条消息。
type Outgoing struct {
	Chat string
	Text string
	// ReplyTo 是这条消息所回应的那条来信的编号，没有则为空。
	ReplyTo string
	// Seq 是对同一条来信的第几次回应，从 1 开始。有的渠道靠它去重。
	Seq int
}

// Adapter 是一家聊天软件的接入。
type Adapter interface {
	// Name 是这个渠道在配置里的名字。
	Name() string
	// Listen 持续接收消息并交给 handle，直到 ctx 结束。断线重连由实现自己负责。
	Listen(ctx context.Context, handle func(Message)) error
	// Send 发出一条消息。
	Send(ctx context.Context, m Outgoing) error
}

// Pending 是一个正在等用户确认的动作。
type Pending struct {
	Tool   string
	Params json.RawMessage
}

// Tasks 是渠道层对任务系统的全部依赖。
type Tasks interface {
	// Create 以用户的名义新建一个任务，返回任务 ID。
	Create(ctx context.Context, title, goal string, tags []string) (string, error)
	// Reply 把用户的话送给一个任务。
	Reply(ctx context.Context, taskID, text, source string) error
	Approve(ctx context.Context, taskID string) error
	Reject(ctx context.Context, taskID, reason string) error
	// ByTag 返回带有某个标签的任务，新的在前。activeOnly 为真时不含已结束的。
	ByTag(ctx context.Context, tag string, activeOnly bool, limit int) ([]*core.Task, error)
	// Changed 返回 since 之后有过变化、且来自某个会话的任务。
	Changed(ctx context.Context, since time.Time, limit int) ([]*core.Task, error)
	// PendingAction 返回任务当前等确认的动作，没有则返回 nil。
	PendingAction(ctx context.Context, taskID string) (*Pending, error)
}

// Feedback 记下主人对协作模型交的活的评价，由 TeamFeedback 提供。
type Feedback interface {
	// Worked 报告一个任务里有没有派给协作模型的活。
	Worked(ctx context.Context, taskID string) (bool, error)
	// RateLatest 给这些任务里最近派过活的那一个打分，返回评了几份。
	RateLatest(ctx context.Context, taskIDs []string, good bool) (int, error)
}

// ChatState 记录一个会话最近一次来信，回复时要用。
type ChatState struct {
	Channel   string
	Chat      string
	LastMsgID string
	LastMsgAt time.Time
	// Seq 是对最近那条来信已经回应了几次。
	Seq int
}

// State 是渠道层自己的持久化：会话状态，以及哪些进展已经通知过。
type State interface {
	// GetChat 返回会话状态，没有则返回 nil。
	GetChat(ctx context.Context, channel, chat string) (*ChatState, error)
	PutChat(ctx context.Context, c *ChatState) error
	// GetNotice 返回一个任务上次通知时的指纹，没有则返回空串。
	GetNotice(ctx context.Context, taskID string) (string, error)
	PutNotice(ctx context.Context, taskID, fingerprint string) error
}

// TagPrefix 是任务上标明它来自哪个会话的标签前缀，完整形式为 chat:渠道名/会话标识。
const TagPrefix = "chat:"

// Tag 生成会话标签。
func Tag(channel, chat string) string { return TagPrefix + channel + "/" + chat }

// parseTag 从任务的标签里取出它来自哪个渠道的哪个会话。
func parseTag(tags []string) (channel, chat string, ok bool) {
	for _, t := range tags {
		rest, found := strings.CutPrefix(t, TagPrefix)
		if !found {
			continue
		}
		if channel, chat, ok = strings.Cut(rest, "/"); ok && channel != "" && chat != "" {
			return channel, chat, true
		}
	}
	return "", "", false
}

// Binding 是一个接入好的渠道，连同它的主人。
type Binding struct {
	Adapter Adapter
	// Owners 是主人在这个渠道里的账号标识。空表示还没有绑定任何人。
	Owners []string
}

func (b Binding) owns(sender string) bool {
	for _, o := range b.Owners {
		if o == sender && o != "" {
			return true
		}
	}
	return false
}

const (
	// pollEvery 是查看任务有没有新进展的间隔。
	pollEvery = 2 * time.Second
	// lookBack 是启动时往回看多久的任务。再早的进展即使没通知到也不补发了。
	lookBack = 24 * time.Hour
	// maxChunkRunes 是单条消息的长度上限，超出的拆成几条发。
	maxChunkRunes = 1500
	// hintEvery 是对同一个陌生人重复提示的最短间隔。
	hintEvery = time.Hour
	// maxTextRunes 是进来的单条消息的长度上限。
	maxTextRunes = 4000
)

// Hub 把渠道和任务系统接在一起。
type Hub struct {
	bindings map[string]Binding
	tasks    Tasks
	state    State
	feedback Feedback
	log      *slog.Logger
	now      func() time.Time

	mu     sync.Mutex
	hinted map[string]time.Time
}

// NewHub 构造渠道中枢。
func NewHub(bindings []Binding, tasks Tasks, state State, log *slog.Logger) (*Hub, error) {
	h := &Hub{
		bindings: make(map[string]Binding), tasks: tasks, state: state, log: log,
		now:    func() time.Time { return time.Now().UTC() },
		hinted: make(map[string]time.Time),
	}
	for _, b := range bindings {
		name := b.Adapter.Name()
		if name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("channel: 渠道名 %q 不合法（不能为空，不能含 /）", name)
		}
		if _, dup := h.bindings[name]; dup {
			return nil, fmt.Errorf("channel: 渠道名 %q 重复", name)
		}
		h.bindings[name] = b
	}
	return h, nil
}

// WithFeedback 让主人能在聊天里用 /赞 和 /踩 评价协作模型交的活。
func (h *Hub) WithFeedback(f Feedback) *Hub {
	h.feedback = f
	return h
}

// Run 启动全部渠道的接收和进展推送，直到 ctx 结束。
func (h *Hub) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for name, b := range h.bindings {
		wg.Add(1)
		go func(name string, b Binding) {
			defer wg.Done()
			err := b.Adapter.Listen(ctx, func(m Message) { h.Handle(ctx, name, m) })
			if err != nil && ctx.Err() == nil {
				h.log.Error("消息渠道停止接收", "渠道", name, "err", err)
			}
		}(name, b)
	}
	since := h.now().Add(-lookBack)
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-ticker.C:
			next := h.now()
			if err := h.Notify(ctx, since); err != nil {
				h.log.Error("推送任务进展失败", "err", err)
				continue
			}
			// 留一点重叠：宁可多查几条靠指纹去重，也不要因为时钟的毫厘之差漏掉一条。
			since = next.Add(-5 * time.Second)
		}
	}
}

// Handle 处理一条进来的消息。
func (h *Hub) Handle(ctx context.Context, channel string, m Message) {
	b, ok := h.bindings[channel]
	if !ok {
		return
	}
	if !b.owns(m.Sender) {
		h.stranger(ctx, b, channel, m)
		return
	}
	if err := h.state.PutChat(ctx, &ChatState{
		Channel: channel, Chat: m.Chat, LastMsgID: m.ID, LastMsgAt: h.now(),
	}); err != nil {
		h.log.Error("保存会话状态失败", "渠道", channel, "err", err)
	}
	if err := h.route(ctx, channel, m); err != nil {
		h.log.Error("处理消息失败", "渠道", channel, "err", err)
		h.say(ctx, channel, m.Chat, "这条消息我没处理成："+err.Error())
	}
}

// stranger 回应一个不是主人的发信人。
//
// 不处理他说的任何内容，只告诉他这里不对外服务，并把他的账号标识报给他——
// 主人第一次接入时就是靠这个知道该往配置里填什么。提示有间隔限制，
// 免得被人拿来刷屏。
func (h *Hub) stranger(ctx context.Context, b Binding, channel string, m Message) {
	key := channel + "/" + m.Sender
	h.mu.Lock()
	last, seen := h.hinted[key]
	if seen && h.now().Sub(last) < hintEvery {
		h.mu.Unlock()
		return
	}
	h.hinted[key] = h.now()
	h.mu.Unlock()

	h.log.Warn("收到非主人的消息，未处理", "渠道", channel, "发信人", m.Sender)
	text := "这是一个私人助手，只听它主人的。\n如果你就是主人：把下面这串标识加进配置里这个渠道的 owners，然后重启。\n" + m.Sender
	if err := b.Adapter.Send(ctx, Outgoing{Chat: m.Chat, Text: text, ReplyTo: m.ID, Seq: 1}); err != nil {
		h.log.Error("回复非主人失败", "渠道", channel, "err", err)
	}
}

// 表示同意的说法。必须是整句，不是包含：
// "可以，但是改到四点"不是同意，是要改。
var approveWords = map[string]bool{
	"同意": true, "批准": true, "可以": true, "好": true, "好的": true, "行": true, "嗯": true, "确认": true,
	"发吧": true, "做吧": true, "去吧": true, "没问题": true, "ok": true, "yes": true, "y": true, "是": true, "对": true,
}

// isApproval 报告一句话是不是单纯的同意。
func isApproval(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.TrimRight(t, "。.!！~～ ")
	return approveWords[t]
}

// newPrefix 是强制开一件新事的前缀。
const newPrefix = "/new"

// 评价协作成果的两个口令。必须是整句：夸一句"不错"多半是在说整件事，
// 而这里要的是明确针对"交出来的东西"的表态，所以用不会和日常说话撞上的写法。
const (
	praiseWord = "/赞"
	blameWord  = "/踩"
)

// rateHint 是办完一件有协作模型参与的事之后，附在结论后面的一句提示。
const rateHint = "\n\n（这件事有一部分是协作模型做的。回 " + praiseWord + " 或 " + blameWord + " 告诉我做得怎么样，以后同类的活按你的评价来派。）"

// rate 记下主人对这个会话里最近一件事的协作成果的评价。
func (h *Hub) rate(ctx context.Context, channel, chat, tag string, good bool) error {
	if h.feedback == nil {
		h.say(ctx, channel, chat, "现在是主 AI 一个人在干活，没有可评价的协作模型。")
		return nil
	}
	recent, err := h.tasks.ByTag(ctx, tag, false, 20)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(recent))
	for _, t := range recent {
		ids = append(ids, t.ID)
	}
	n, err := h.feedback.RateLatest(ctx, ids, good)
	if err != nil {
		return err
	}
	if n == 0 {
		h.say(ctx, channel, chat, "最近的事里没有协作模型做的部分，没什么可评的。")
		return nil
	}
	h.say(ctx, channel, chat, "记下了。")
	return nil
}

// route 决定一条消息该去哪。
func (h *Hub) route(ctx context.Context, channel string, m Message) error {
	text := strings.TrimSpace(m.Text)
	if text == "" {
		h.say(ctx, channel, m.Chat, "目前我只看得懂文字。")
		return nil
	}
	if utf8.RuneCountInString(text) > maxTextRunes {
		text = string([]rune(text)[:maxTextRunes])
	}
	tag := Tag(channel, m.Chat)

	if rest, forced := strings.CutPrefix(text, newPrefix); forced {
		if text = strings.TrimSpace(rest); text == "" {
			h.say(ctx, channel, m.Chat, newPrefix+" 后面写上要办的事。")
			return nil
		}
		return h.create(ctx, text, tag)
	}

	if text == praiseWord || text == blameWord {
		return h.rate(ctx, channel, m.Chat, tag, text == praiseWord)
	}

	active, err := h.tasks.ByTag(ctx, tag, true, 20)
	if err != nil {
		return err
	}
	// 有动作在等确认：这句话就是对它的表态。单纯的同意就放行；
	// 其他任何话都当作"不行，要这样改"，原话交回给任务，让它照着重新来。
	for _, t := range active {
		if t.State != core.StateWaiting || t.Wake == nil || t.Wake.Kind != core.WakeApproval {
			continue
		}
		if isApproval(text) {
			return h.tasks.Approve(ctx, t.ID)
		}
		return h.tasks.Reject(ctx, t.ID, text)
	}
	// 有任务在等用户回话：这句话是给它的。
	for _, t := range active {
		if asking(t) {
			return h.tasks.Reply(ctx, t.ID, text, channel)
		}
	}
	return h.create(ctx, text, tag)
}

// asking 报告一个任务是不是正在等用户回话。
func asking(t *core.Task) bool {
	return t.State == core.StateWaiting && t.Wake != nil && t.Wake.Kind == core.WakeEvent &&
		strings.Contains(t.Wake.Expr, `"user.message"`) && strings.Contains(t.Wake.Expr, t.ID)
}

func (h *Hub) create(ctx context.Context, text, tag string) error {
	title := text
	if i := strings.IndexAny(title, "\n。"); i > 0 {
		title = title[:i]
	}
	if r := []rune(title); len(r) > 24 {
		title = string(r[:24]) + "…"
	}
	_, err := h.tasks.Create(ctx, title, text, []string{tag})
	return err
}

// askPrefix 是内核在任务提问时写进原因里的前缀。
const askPrefix = "等待你的回复："

// Notify 把 since 之后有变化的任务的进展推到各自的会话里。
//
// 只推用户需要知道的四种：问了问题、有动作等确认、办完了、办不成了。
// 每种进展有一个指纹，推过的不再推——进程重启、同一个任务被扫到多次都不会重复打扰。
func (h *Hub) Notify(ctx context.Context, since time.Time) error {
	tasks, err := h.tasks.Changed(ctx, since, 200)
	if err != nil {
		return err
	}
	// 按时间先后推，同一个会话里的消息顺序才对。
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].UpdatedAt.Before(tasks[j].UpdatedAt) })
	var errs []error
	for _, t := range tasks {
		channel, chat, ok := parseTag(t.Tags)
		if !ok {
			continue
		}
		if _, known := h.bindings[channel]; !known {
			continue
		}
		text, fingerprint, err := h.describe(ctx, t)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if fingerprint == "" {
			continue
		}
		prev, err := h.state.GetNotice(ctx, t.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if prev == fingerprint {
			continue
		}
		if err := h.send(ctx, channel, chat, text); err != nil {
			errs = append(errs, fmt.Errorf("channel: 向 %s 推送任务 %s 的进展失败: %w", channel, t.ID, err))
			continue
		}
		if err := h.state.PutNotice(ctx, t.ID, fingerprint); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// describe 把一个任务当前的状态说成一句给用户看的话。指纹为空表示这个状态不用通知。
func (h *Hub) describe(ctx context.Context, t *core.Task) (text, fingerprint string, err error) {
	switch {
	case t.State == core.StateDone:
		text := orElse(t.Reason, "办完了。")
		if h.feedback != nil {
			worked, err := h.feedback.Worked(ctx, t.ID)
			if err != nil {
				return "", "", err
			}
			if worked {
				text += rateHint
			}
		}
		return text, "done", nil
	case t.State == core.StateFailed:
		return "这件事没办成：" + orElse(t.Reason, "原因不明"), "failed", nil
	case t.State == core.StateWaiting && t.Wake != nil && t.Wake.Kind == core.WakeApproval:
		p, err := h.tasks.PendingAction(ctx, t.ID)
		if err != nil {
			return "", "", err
		}
		if p == nil {
			return "", "", nil
		}
		return renderPending(t, p), "approval|" + t.Wake.Ref, nil
	case asking(t):
		q := strings.TrimPrefix(t.Reason, askPrefix)
		// 同一个任务可以先后问好几个问题，用步骤序号把它们区分开。
		return q, fmt.Sprintf("ask|%d", t.NextSeq), nil
	}
	return "", "", nil
}

func orElse(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// renderPending 把一个等确认的动作写给用户看。参数原样列出：
// 批准一封信之前得看得到正文，批准一笔钱之前得看得到金额。
func renderPending(t *core.Task, p *Pending) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "要做这一步，等你点头：%s\n", p.Tool)
	var params map[string]any
	if err := json.Unmarshal(p.Params, &params); err != nil {
		sb.WriteString(string(p.Params) + "\n")
	} else {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, ok := params[k].(string)
			if !ok {
				b, err := json.Marshal(params[k])
				if err != nil {
					continue
				}
				v = string(b)
			}
			if strings.Contains(v, "\n") {
				fmt.Fprintf(&sb, "%s:\n%s\n", k, v)
			} else {
				fmt.Fprintf(&sb, "%s: %s\n", k, v)
			}
		}
	}
	// 助手为什么想这么做；策略那边若有特别的理由（比如"这个任务读过外部内容"）也说一声，
	// 平平常常的"没配放行规则"就不念了。
	rule, why := kernel.SplitApprovalReason(t.Reason)
	if why != "" {
		sb.WriteString(why + "\n")
	}
	if rule != "" && rule != policy.ReasonNoRule {
		sb.WriteString("（" + rule + "）\n")
	}
	sb.WriteString("回「同意」就执行；要改就直接说怎么改。")
	return sb.String()
}

// say 发一条消息，失败只记日志。用于回应用户刚发来的话。
func (h *Hub) say(ctx context.Context, channel, chat, text string) {
	if err := h.send(ctx, channel, chat, text); err != nil {
		h.log.Error("发送消息失败", "渠道", channel, "err", err)
	}
}

// send 把一段话发到会话里，太长就拆成几条。
func (h *Hub) send(ctx context.Context, channel, chat, text string) error {
	b, ok := h.bindings[channel]
	if !ok {
		return fmt.Errorf("channel: 没有名为 %q 的渠道", channel)
	}
	st, err := h.state.GetChat(ctx, channel, chat)
	if err != nil {
		return err
	}
	if st == nil {
		st = &ChatState{Channel: channel, Chat: chat}
	}
	for _, part := range chunk(text, maxChunkRunes) {
		st.Seq++
		if err := b.Adapter.Send(ctx, Outgoing{Chat: chat, Text: part, ReplyTo: st.LastMsgID, Seq: st.Seq}); err != nil {
			return err
		}
		// 每发一条就记一次：序号用过了就不能再用，哪怕后面那条没发出去。
		if err := h.state.PutChat(ctx, st); err != nil {
			return err
		}
	}
	return nil
}

// chunk 把一段话按长度拆开，尽量在换行处断。
func chunk(text string, max int) []string {
	r := []rune(strings.TrimSpace(text))
	if len(r) == 0 {
		return nil
	}
	var out []string
	for len(r) > max {
		cut := max
		for i := max; i > max/2; i-- {
			if r[i-1] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, strings.TrimSpace(string(r[:cut])))
		r = r[cut:]
	}
	if rest := strings.TrimSpace(string(r)); rest != "" {
		out = append(out, rest)
	}
	return out
}
