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
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/media"
	"github.com/xiguac/atomhelper/internal/policy"
)

// Message 是从渠道进来的一条消息。
type Message struct {
	// Chat 是会话的标识，回复时发回这里。同一个渠道内唯一。
	Chat string
	// Sender 是发消息的人在这个渠道里的账号标识。
	Sender string
	Text   string
	// Images 是随消息发来的图片。
	Images []Attachment
	// Files 是随消息发来的文件（图片、语音、视频以外的附件）。
	Files []Attachment
	// Voices 与 Videos 是随消息发来的语音和视频。
	Voices []Attachment
	Videos []Attachment
	// ID 是渠道给这条消息的编号，有的渠道回复时要带上它。
	ID string
	At time.Time
}

// Attachment 是消息里带的一个附件：渠道给的下载地址和类型。
type Attachment struct {
	URL string
	// ContentType 形如 image/jpeg。
	ContentType string
	// Name 是文件名，只有文件有。
	Name string
	// Fetch 非空时用它取回附件，不看 URL。有的平台的图片没有公开的下载地址，
	// 要带着凭证调接口取（飞书、钉钉），或者取回来还要解密（微信）。
	Fetch func(ctx context.Context) (Picture, error)
}

// Picture 是已经取回来的一张图片。
type Picture struct {
	MIME string
	Data []byte
}

// Outgoing 是要发到渠道里的一条消息。
type Outgoing struct {
	Chat string
	Text string
	// ReplyTo 是这条消息所回应的那条来信的编号，没有则为空。
	ReplyTo string
	// Seq 是对同一条来信的第几次回应，从 1 开始。有的渠道靠它去重。
	Seq int
	// File 非空时这条消息发的是一个文件，Text 可以为空。
	File *File
}

// File 是要发到会话里的一个文件。
type File struct {
	Name string
	Data []byte
	// Kind 是按哪一类发：图片、语音、视频按各平台原生的消息发（聊天里直接显示、能直接播放），
	// 其余的按文件发。为空按文件发。
	Kind media.Kind
}

// ErrUnsupportedKind 表示这个消息源不能按这一类发（比如微信发不了语音）。中枢收到它会改成按文件发。
var ErrUnsupportedKind = errors.New("channel: 这个消息源不能按这一类发")

// MaxFileBytes 是发到会话里的单个文件的大小上限。
const MaxFileBytes = 20 << 20

// ErrNoChat 表示这个任务不是从聊天会话里来的，没有地方可发。
var ErrNoChat = errors.New("channel: 这个任务不是从聊天里发起的")

// SendFile 把一个文件发到任务所来自的那个会话里。
//
// 发到哪里由任务的标签决定，调用方指定不了：文件只会回到发起这个任务的主人手上。
func (h *Hub) SendFile(ctx context.Context, tags []string, name string, data []byte) error {
	channel, chat, ok := parseTag(tags)
	if !ok {
		return ErrNoChat
	}
	b, ok := h.bindings[channel]
	if !ok {
		return fmt.Errorf("channel: 没有名为 %q 的渠道", channel)
	}
	if len(data) == 0 || len(data) > MaxFileBytes {
		return fmt.Errorf("channel: 文件是空的，或超过 %d MB，发不了", MaxFileBytes>>20)
	}
	st, err := h.state.GetChat(ctx, channel, chat)
	if err != nil {
		return err
	}
	if st == nil {
		st = &ChatState{Channel: channel, Chat: chat}
	}
	st.Seq++
	kind := media.KindOf(name, data)
	out := Outgoing{Chat: chat, ReplyTo: st.LastMsgID, Seq: st.Seq, File: &File{Name: name, Data: data, Kind: kind}}
	err = b.Adapter.Send(ctx, out)
	// 这一类发不了（平台不支持，或者转不了它要的格式）：退回按文件发，总比发不出去强。
	if err != nil && kind != media.KindFile {
		if !errors.Is(err, ErrUnsupportedKind) {
			h.log.Warn("按原生消息发送失败，改为按文件发送", "渠道", channel, "文件", name, "类别", kind, "err", err)
		}
		st.Seq++
		out.Seq, out.File.Kind = st.Seq, media.KindFile
		err = b.Adapter.Send(ctx, out)
	}
	if err != nil {
		return err
	}
	h.log.Info("发出文件", "渠道", channel, "文件", name, "类别", out.File.Kind)
	// 序号用过了就不能再用。
	return h.state.PutChat(ctx, st)
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
	// Attach 把用户发来的图片交给一个任务。图片原样存下，之后每一步决策都随上下文一起给模型看；
	// 图里的内容按外部内容对待。
	Attach(ctx context.Context, taskID string, pictures []Picture, source string) error
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

// ConversationTag 标明一个任务属于用户与助手的会话，与 agent.ConversationTag 一致。
// 这里不引用 agent 包：渠道层在它下面。
const ConversationTag = "conversation:owner"

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
	// fetch 按地址取回图片，测试里可以换掉。
	fetch func(ctx context.Context, a Attachment) (Picture, error)
	log   *slog.Logger
	now   func() time.Time

	// triage 非空时，一句话归给哪件事由主模型判断，见 triage.go。
	triage Triager
	// files 非空时用户可以在聊天里发文件，见 files.go。
	files FileSink
	// accept 是配置的模型收得了哪几类媒体：都收不了的那一类，收到时直接告诉用户，不交给任务。
	accept accepted
	// asr 把用户发来的语音消息转成文字，见 voice.go。为空时收不了语音。
	asr Transcriber
	// saidAt 是用户最近说到各件事的时间，判断新的话归给谁时用，见 triage.go。
	saidAt map[string]time.Time

	mu     sync.Mutex
	hinted map[string]time.Time
	// asked 是一句拿不准归给谁、正在问用户的话。所有消息源共用一个会话，所以只有一个。
	asked *pendingAsk
}

// NewHub 构造渠道中枢。
func NewHub(bindings []Binding, tasks Tasks, state State, log *slog.Logger) (*Hub, error) {
	h := &Hub{
		bindings: make(map[string]Binding), tasks: tasks, state: state, log: log, fetch: fetchMedia,
		accept: accepted{image: true},
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

// accepted 记录配置的模型里有没有收得了图片、视频的。语音不交给模型，转成文字，见 voice.go。
type accepted struct {
	image, video bool
}

// WithMedia 说明配置的模型里有没有收得了图片、视频的。不设置时只收图片。
func (h *Hub) WithMedia(image, video bool) *Hub {
	h.accept = accepted{image: image, video: video}
	return h
}

// unreadable 是收到看不懂的内容时的回复。
const unreadable = "我还看不懂这个类型的数据哦"

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
		h.log.Info("收到非主人的消息，不处理", "渠道", channel)
		h.stranger(ctx, b, channel, m)
		return
	}
	// 只记收到了什么，不记内容：日志会被翻看，消息原文留在任务里。
	h.log.Info("收到消息", "渠道", channel, "字数", utf8.RuneCountInString(m.Text),
		"图片", len(m.Images), "语音", len(m.Voices), "视频", len(m.Videos), "文件", len(m.Files))
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

// rateHint 是办完一件交给任务模型办的事之后，附在答复后面的评分邀请。
const rateHint = "\n\n（请给这次的回答打个分：回 " + praiseWord + " 或 " + blameWord + "。评分会记在办这件事的模型名下，以后同类的事优先交给评分高的。）"

// rate 记下主人对这个会话里最近一件事的协作成果的评价。
func (h *Hub) rate(ctx context.Context, channel, chat, tag string, good bool) error {
	if h.feedback == nil {
		h.say(ctx, channel, chat, "现在是主 AI 一个人在干活，没有可评价的协作模型。")
		return nil
	}
	// 评的是整个会话里最近的一件事，不分它是在哪个消息源里发起的。
	recent, err := h.tasks.ByTag(ctx, ConversationTag, false, 20)
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
	if utf8.RuneCountInString(text) > maxTextRunes {
		text = string([]rune(text)[:maxTextRunes])
	}
	tag := Tag(channel, m.Chat)
	// 文件先取回来放进工作区，换成一句"我发来了文件 xxx"，再和用户的话一起照常处理。
	if len(m.Files) > 0 {
		note, err := h.receiveFiles(ctx, channel, m)
		if err != nil {
			return err
		}
		if text == "" {
			text = note
		} else {
			text = text + "\n" + note
		}
		m.Files = nil
		m.Text = text
	}
	// plain 是不带说明的原话，只用来认"同意"这类整句的表态；text 是交给任务和模型的。
	plain := text
	// 语音消息转成文字，和打的字一样处理，只在前面说明是语音转的。
	if len(m.Voices) > 0 {
		spoken := h.transcribe(ctx, channel, m)
		m.Voices = nil
		if spoken != "" {
			plain = strings.TrimSpace(plain + "\n" + spoken)
			text = strings.TrimSpace(text + "\n" + voiceNote + spoken)
		}
		if text == "" && len(m.Images)+len(m.Videos) == 0 {
			// 什么都没转出来，transcribe 已经告诉用户了。
			return nil
		}
	}
	// 没有一个模型收得了的那一类：告诉用户，不收。有文字的话文字照常处理。
	var refused []string
	for _, k := range []struct {
		list *[]Attachment
		ok   bool
		name string
	}{{&m.Images, h.accept.image, "图片"}, {&m.Videos, h.accept.video, "视频"}} {
		if len(*k.list) > 0 && !k.ok {
			refused = append(refused, k.name)
			*k.list = nil
		}
	}
	if len(refused) > 0 {
		h.say(ctx, channel, m.Chat, unreadable+"（"+strings.Join(refused, "、")+"）。")
		if text == "" && len(m.Images)+len(m.Videos) == 0 {
			return nil
		}
	}
	if len(m.Images)+len(m.Videos) > 0 {
		return h.routeMedia(ctx, channel, m, text, tag)
	}
	if text == "" {
		// 消息源收不了的语音、视频等，到这里都只剩一条空消息。
		h.say(ctx, channel, m.Chat, unreadable+"。")
		return nil
	}

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

	// 所有消息源共用一个会话：在 QQ 里问出来的问题，到微信里回答也算数。
	active, err := h.tasks.ByTag(ctx, ConversationTag, true, 50)
	if err != nil {
		return err
	}
	// 上一句话拿不准归给谁、正在问用户：这一句先当作对那个问题的回答。
	if handled, err := h.answerAsk(ctx, channel, m.Chat, text, active); err != nil || handled {
		return err
	}
	if len(active) == 0 {
		return h.create(ctx, text, tag)
	}
	// 有动作在等确认，而这句话就是一个"同意"：放行。这不需要判断。
	if isApproval(plain) {
		for _, t := range active {
			if t.State == core.StateWaiting && t.Wake != nil && t.Wake.Kind == core.WakeApproval {
				h.follow(ctx, t, channel, m.Chat)
				return h.tasks.Approve(ctx, t.ID)
			}
		}
	}
	if h.triage == nil {
		return h.routeByRule(ctx, channel, m.Chat, text, tag, active)
	}
	// 手头有事在办：这句话是说其中哪一件，还是另起一件新的，由主模型判断。
	// 刚办完的事也列上：接着它说的话不该被错归到另一件不相干的、还在办的事上。
	all, err := h.tasks.ByTag(ctx, ConversationTag, false, 20)
	if err != nil {
		return err
	}
	cands := h.candidatesOf(active, h.justFinished(all))
	verdict, err := h.triage.Triage(ctx, text, cands)
	if err != nil {
		// 判断不了不能把话丢了：退回老办法，并留下痕迹。
		h.log.Error("判断消息归属失败，按规则处理", "err", err)
		return h.routeByRule(ctx, channel, m.Chat, text, tag, active)
	}
	pick := func(id string) error {
		if t := find(active, id); t != nil {
			return h.deliver(ctx, t, channel, m.Chat, text)
		}
		// 刚办完的事，或者判断的这会儿结束了的：另起一个任务接着办，之前的对话随上下文带过去。
		return h.create(ctx, text, tag)
	}
	switch {
	case verdict.New:
		return h.create(ctx, text, tag)
	case verdict.TaskID != "":
		return pick(verdict.TaskID)
	}
	// 模型也拿不准：刚说过的那件事最可能是在接着说；时间窗里没有，才问用户。
	if c, ok := recent(cands); ok {
		return pick(c.ID)
	}
	h.askWhich(ctx, channel, m.Chat, tag, text, cands)
	return nil
}

// find 从任务列表里按编号找出一个，没有返回 nil。
func find(tasks []*core.Task, id string) *core.Task {
	for _, t := range tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// routeByRule 是没有模型可用时的老办法：有动作在等确认，这句话就是对它的表态；
// 有任务在等用户回话，这句话是给它的；否则是一件新的事。
func (h *Hub) routeByRule(ctx context.Context, channel, chat, text, tag string, active []*core.Task) error {
	for _, t := range active {
		if t.State == core.StateWaiting && t.Wake != nil && t.Wake.Kind == core.WakeApproval {
			return h.deliver(ctx, t, channel, chat, text)
		}
	}
	for _, t := range active {
		if asking(t) {
			return h.deliver(ctx, t, channel, chat, text)
		}
	}
	// 用户刚说过的那件事还在办：多半是在接着说。
	if c, ok := recent(h.candidatesOf(active, nil)); ok {
		return h.deliver(ctx, find(active, c.ID), channel, chat, text)
	}
	return h.create(ctx, text, tag)
}

// asking 报告一个任务是不是正在等用户回话。
func asking(t *core.Task) bool {
	return t.State == core.StateWaiting && t.Wake != nil && t.Wake.Kind == core.WakeEvent &&
		strings.Contains(t.Wake.Expr, `"user.message"`) && strings.Contains(t.Wake.Expr, t.ID)
}

func (h *Hub) create(ctx context.Context, text, tag string) error {
	id, err := h.createTask(ctx, text, text, tag)
	if err == nil {
		h.noteSaid(id)
	}
	return err
}

// createTask 新建一个任务，标题取 title 的头一句。
func (h *Hub) createTask(ctx context.Context, title, goal, tag string) (string, error) {
	// 语音转的话，标题取转出来的文字，不取前面那句说明。
	title = strings.TrimPrefix(title, voiceNote)
	if i := strings.IndexAny(title, "\n。"); i > 0 {
		title = title[:i]
	}
	if r := []rune(title); len(r) > 24 {
		title = string(r[:24]) + "…"
	}
	// 第二个标签标明它属于用户与助手的会话：所有消息源共用一个，新任务靠它看到最近的对话。
	id, err := h.tasks.Create(ctx, title, goal, []string{tag, ConversationTag})
	if err == nil {
		h.log.Info("消息新建了任务", "任务", id)
	}
	return id, err
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
		// 用户换了个消息源接着说这件事的话，进展跟着他走。
		if c, ch, moved := h.followed(ctx, t.ID); moved {
			channel, chat = c, ch
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

// followKey 是"这个任务的进展改推到哪里"在通知记录里的名字。
// 借用通知记录的存储：它就是一张按名字存一个字符串的表，任务 ID 不会带竖线，不会撞上。
func followKey(taskID string) string { return "follow|" + taskID }

// follow 记下用户是在哪个会话里接着说这件事的：与任务发起的地方不同时，之后的进展改推到这里。
// 记不下来只影响进展推到哪，不该让这句话送不到任务手里，所以只记日志。
func (h *Hub) follow(ctx context.Context, t *core.Task, channel, chat string) {
	if origin, originChat, ok := parseTag(t.Tags); ok && origin == channel && originChat == chat {
		// 回到了发起的地方：去掉之前记下的改道。
		if _, _, moved := h.followed(ctx, t.ID); !moved {
			return
		}
	}
	if err := h.state.PutNotice(ctx, followKey(t.ID), channel+"/"+chat); err != nil {
		h.log.Error("记录任务进展的去向失败", "任务", t.ID, "err", err)
	}
}

// followed 返回一个任务的进展改推到了哪里；没有改过时 moved 为假。
func (h *Hub) followed(ctx context.Context, taskID string) (channel, chat string, moved bool) {
	v, err := h.state.GetNotice(ctx, followKey(taskID))
	if err != nil || v == "" {
		return "", "", false
	}
	channel, chat, ok := strings.Cut(v, "/")
	return channel, chat, ok && channel != "" && chat != ""
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
	h.log.Info("发出消息", "渠道", channel, "字数", utf8.RuneCountInString(text))
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

// 媒体的几个上限：一条消息最多收几张图片、几段语音、几个视频，单个多大，取回一个最多等多久。
// 视频以 Base64 交给模型服务，阿里云百炼对单个 Base64 的限制是 10 MB，所以原件不超过 7 MB。
const (
	maxPictures     = 4
	maxVoices       = 3
	maxVideos       = 1
	maxPictureBytes = 10 << 20
	maxMediaBytes   = 7 << 20
	pictureTimeout  = 30 * time.Second
	mediaTimeout    = 2 * time.Minute
)

// mediaOnlyGoal 是用户只发了图片或视频，没有说话时任务的目标。what 是"图片""视频"这样的叫法。
func mediaOnlyGoal(what string) string {
	return "用户发来了" + what + "，没有附文字说明。" + what + "随上下文一起给出：看明白它是什么，" +
		"能判断出用户想让你做什么就去办，判断不出就问用户。"
}

// routeMedia 处理一条带图片或视频的消息：取回来，原样交给任务，再把用户说的话送过去。
// 语音消息不走这里，在 route 里已经转成了文字。
//
// 不在这里转成文字：转写会丢掉版式、图表、画面里的细节，所以任务里的模型拿到的是原件。
// 原件和用户的话分开送：话是他亲口说的，图片、视频里的内容可能是别人的，两者的信任等级不一样。
func (h *Hub) routeMedia(ctx context.Context, channel string, m Message, text, tag string) error {
	var pictures []Picture
	var kinds, problems []string
	take := func(list []Attachment, max int, name, unit string, timeout time.Duration, prepare func(Picture) (Picture, error)) {
		if len(list) == 0 {
			return
		}
		if len(list) > max {
			h.say(ctx, channel, m.Chat, fmt.Sprintf("一次最多收 %d %s%s，这次只收下了前 %d %s。", max, unit, name, max, unit))
			list = list[:max]
		}
		got := 0
		for _, a := range list {
			fetchCtx, cancel := context.WithTimeout(ctx, timeout)
			var p Picture
			var err error
			if a.Fetch != nil {
				p, err = a.Fetch(fetchCtx)
			} else {
				p, err = h.fetch(fetchCtx, a)
			}
			if err == nil {
				p, err = prepare(p)
			}
			cancel()
			if err != nil {
				h.log.Error("收"+name+"失败", "渠道", channel, "err", err)
				problems = append(problems, fmt.Sprintf("有一%s%s没收成：%v", unit, name, err))
				continue
			}
			pictures = append(pictures, p)
			got++
		}
		if got > 0 {
			kinds = append(kinds, name)
		}
	}
	take(m.Images, maxPictures, "图片", "张", pictureTimeout, func(p Picture) (Picture, error) {
		// 各消息源自己取的图片已经按内容核对过类型；按地址取回的，类型以内容为准：
		// 平台报的类型不一定对，而模型服务对类型很挑剔。
		if !strings.HasPrefix(p.MIME, "image/") {
			if mime := http.DetectContentType(p.Data); strings.HasPrefix(mime, "image/") {
				p.MIME = mime
			} else {
				return Picture{}, fmt.Errorf("取回来的不是图片（%s）", mime)
			}
		}
		if len(p.Data) > maxPictureBytes {
			return Picture{}, fmt.Errorf("图片超过 %d MB", maxPictureBytes>>20)
		}
		return p, nil
	})
	take(m.Videos, maxVideos, "视频", "个", mediaTimeout, func(p Picture) (Picture, error) {
		mime := media.Sniff(p.Data)
		if !strings.HasPrefix(mime, "video/") {
			return Picture{}, fmt.Errorf("取回来的不是视频（%s）", mime)
		}
		if len(p.Data) > maxMediaBytes {
			return Picture{}, fmt.Errorf("视频超过 %d MB，请剪短或压缩后再发", maxMediaBytes>>20)
		}
		return Picture{MIME: mime, Data: p.Data}, nil
	})
	if len(problems) > 0 {
		h.say(ctx, channel, m.Chat, strings.Join(problems, "\n"))
	}
	if len(pictures) == 0 {
		// 一个都没收成：用户的话多半是在说这些附件，离开它们办不了，等用户重发。
		return nil
	}

	active, err := h.tasks.ByTag(ctx, ConversationTag, true, 50)
	if err != nil {
		return err
	}
	// 有任务在等用户回话：这些是给它的。先送媒体，再送用户的话。
	for _, t := range active {
		if !asking(t) {
			continue
		}
		h.noteSaid(t.ID)
		h.follow(ctx, t, channel, m.Chat)
		if err := h.tasks.Attach(ctx, t.ID, pictures, channel); err != nil {
			return err
		}
		if text == "" {
			return nil
		}
		return h.tasks.Reply(ctx, t.ID, text, channel)
	}
	what := strings.Join(kinds, "、")
	goal, title := text, text
	if goal == "" {
		goal, title = mediaOnlyGoal(what), "处理"+what
	}
	id, err := h.createTask(ctx, title, goal, tag)
	if err != nil {
		return err
	}
	h.noteSaid(id)
	return h.tasks.Attach(ctx, id, pictures, channel)
}

// fetchMedia 按渠道给的地址取回一个附件（图片、语音、视频）。类型和大小由调用方再核对。
//
// 地址是渠道平台给的，不是用户或模型写的；即便如此也只认 https，并且限制大小。
func fetchMedia(ctx context.Context, a Attachment) (Picture, error) {
	data, err := fetchFile(ctx, a.URL)
	if err != nil {
		return Picture{}, err
	}
	return Picture{MIME: media.Sniff(data), Data: data}, nil
}
