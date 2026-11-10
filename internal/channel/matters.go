package channel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/matter"
)

// 事项清单：新来的一句话是清单里哪件事的后续，还是一件新的事。见 internal/matter。
//
// 接入之前，判断归属时能选的只有还在办的任务和半小时内刚办完的任务：上午交代、下午补一句的事，
// 下午那句只能被当成一件新的事，新任务拿到的只有「那改成周六呢」这样一句没头没尾的话。
// 接入之后能选的是清单里的事：归给哪件，新任务就挂在哪件下面，带着它此前的进展去办。

// TaskDeleter 删除已经结束的任务，由 *pg.Store 提供。
type TaskDeleter interface {
	DeleteFinishedTasks(ctx context.Context, ids []string) (int, error)
}

// WithMatters 接入事项清单。d 用来在一件事从清单里删掉时，删掉它名下已经结束的任务。
func (h *Hub) WithMatters(s matter.Store, d TaskDeleter) *Hub {
	h.matters, h.deleter = s, d
	return h
}

const (
	// maxMatterCandidates 是交给模型判断的事的个数上限。归档的事只占一行，给得宽一些。
	maxMatterCandidates = 40
	// tidyEvery 是整理清单的间隔：该删的删，该问的问。
	tidyEvery = time.Hour
	// askGap 是两次"这件事还留着吗"之间至少隔多久，免得一下子问一串。
	askGap = 24 * time.Hour
)

// updateMatter 读出一件事、改、存回去。读和存之间加锁：收消息和推进展在两个协程里。
// 清单只是帮助判断归属的，存不下来不该让这句话办不成，所以失败只记日志。
func (h *Hub) updateMatter(ctx context.Context, id string, change func(*matter.Matter)) {
	if h.matters == nil || id == "" {
		return
	}
	h.matterMu.Lock()
	defer h.matterMu.Unlock()
	m, err := h.matters.Get(ctx, id)
	if errors.Is(err, matter.ErrNotFound) {
		return
	}
	if err == nil {
		change(m)
		err = h.matters.Put(ctx, m)
	}
	if err != nil {
		h.log.Error("更新事项清单失败", "事项", id, "err", err)
	}
}

// openMatter 在清单里记下一件新的事，返回要挂到任务上的标签。没接入清单、或者记不下来时返回 nil。
func (h *Hub) openMatter(ctx context.Context, title string, kind matter.Kind, parent, tag string) []string {
	if h.matters == nil {
		return nil
	}
	now := h.now()
	m := &matter.Matter{ID: core.NewID(), ParentID: parent, Title: title, Kind: kind, LastActiveAt: now, CreatedAt: now}
	m.Channel, m.Chat, _ = parseTag([]string{tag})
	if err := h.matters.Put(ctx, m); err != nil {
		h.log.Error("记录新的事项失败", "err", err)
		return nil
	}
	return []string{matter.Tag(m.ID)}
}

// saidAbout 记下用户刚在某个会话里说到了任务所属的那件事。
func (h *Hub) saidAbout(ctx context.Context, t *core.Task, channel, chat string) {
	h.updateMatter(ctx, matter.FromTags(t.Tags), func(m *matter.Matter) {
		m.Touch(h.now())
		m.Channel, m.Chat = channel, chat
	})
}

// progressed 记下一件事有了进展并且已经告诉了用户。办完或办不成的，把这一次的结果记进这件事的进展里。
func (h *Hub) progressed(ctx context.Context, t *core.Task) {
	h.updateMatter(ctx, matter.FromTags(t.Tags), func(m *matter.Matter) {
		now := h.now()
		m.Touch(now)
		switch t.State {
		case core.StateDone:
			m.AddNote(matter.Note{At: now, Ask: t.Goal, Result: orElse(t.Reason, "办完了。")})
		case core.StateFailed:
			m.AddNote(matter.Note{At: now, Ask: t.Goal, Result: "没办成：" + orElse(t.Reason, "原因不明")})
		}
	})
}

// matterCandidates 列出供判断的事：清单里的每一件，加上不属于任何事的任务（接入清单之前建的）。
// 用户最近说到的排在前面。读不出清单时退回只看任务。
func (h *Hub) matterCandidates(ctx context.Context, active, finished []*core.Task) []Candidate {
	list, err := h.matters.List(ctx)
	if err != nil {
		h.log.Error("读取事项清单失败，这次只按任务判断", "err", err)
		return h.candidatesOf(active, finished)
	}
	now := h.now()
	byID := make(map[string]*matter.Matter, len(list))
	for _, m := range list {
		byID[m.ID] = m
	}
	// 一件事同时最多有一个在办的任务算数：取用户最近说到的那个。
	running := make(map[string]*core.Task)
	var loose, looseDone []*core.Task
	for _, t := range active {
		id := matter.FromTags(t.Tags)
		if byID[id] == nil {
			loose = append(loose, t)
		} else if cur := running[id]; cur == nil || h.lastSaid(t).After(h.lastSaid(cur)) {
			running[id] = t
		}
	}
	for _, t := range finished {
		if byID[matter.FromTags(t.Tags)] == nil {
			looseDone = append(looseDone, t)
		}
	}
	out := h.candidatesOf(loose, looseDone)
	for _, m := range list {
		c := Candidate{ID: matter.Tag(m.ID), Title: m.Title, Matter: true, Ongoing: m.Kind == matter.KindOngoing,
			Status: "没有在办的任务", Last: m.Latest(160), Idle: now.Sub(m.LastActiveAt), Asked: m.AskedAt != nil}
		if p := byID[m.ParentID]; p != nil {
			c.Under = p.Title
		}
		if t := running[m.ID]; t != nil {
			c.Status, c.Goal = status(t), oneLine(t.Goal, 300)
		}
		switch m.Stage(now) {
		case matter.StageLow:
			c.Dormant = "low"
		case matter.StageArchived, matter.StageExpired:
			// 长期的事到了 30 天只是该问一句了，照常列着。
			if m.Kind == matter.KindOnce {
				c.Dormant, c.Last = "archived", ""
			}
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Idle < out[j].Idle })
	if len(out) > maxMatterCandidates {
		out = out[:maxMatterCandidates]
	}
	return out
}

// routeToMatter 把一句话交给清单里的一件事：它有任务在办，就送给那个任务；没有，就在它名下新开一个任务。
// handled 为假表示这件事已经不在清单里了，由调用方另作处理。
func (h *Hub) routeToMatter(ctx context.Context, id string, v Verdict, active []*core.Task, channel, chat, text, tag string) (handled, acked bool, err error) {
	m, err := h.matters.Get(ctx, id)
	if errors.Is(err, matter.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return true, false, err
	}
	// 问过用户这件事还留不留，这句话是在回答：留着，或者删掉。没问过就不认，免得一句话被理解成要删东西。
	if m.AskedAt != nil && (v.Keep || v.Close) {
		return true, false, h.settleMatter(ctx, m, v.Keep, channel, chat)
	}
	var cur *core.Task
	for _, t := range active {
		if matter.FromTags(t.Tags) == id && (cur == nil || h.lastSaid(t).After(h.lastSaid(cur))) {
			cur = t
		}
	}
	if cur != nil {
		return true, true, h.deliver(ctx, cur, channel, chat, text)
	}
	// 新任务的标题用这件事的标题：推进展、查记忆时看得出是哪件事，而不是「那改成周六呢」。
	taskID, err := h.createTask(ctx, m.Title, text, tag, matter.Tag(id))
	if err != nil {
		return true, false, err
	}
	h.noteSaid(taskID)
	h.updateMatter(ctx, id, func(m *matter.Matter) {
		m.Touch(h.now())
		m.Channel, m.Chat = channel, chat
	})
	return true, true, nil
}

// settleMatter 按用户的回答处理一件问过"还留着吗"的事：留着，或者连同它下面的事一起从清单里删掉。
func (h *Hub) settleMatter(ctx context.Context, m *matter.Matter, keep bool, channel, chat string) error {
	if keep {
		h.updateMatter(ctx, m.ID, func(m *matter.Matter) {
			m.Touch(h.now())
			m.Channel, m.Chat = channel, chat
		})
		h.reply(ctx, channel, chat, fmt.Sprintf("好，「%s」留着。", m.Title))
		return nil
	}
	list, err := h.matters.List(ctx)
	if err != nil {
		return err
	}
	gone := []*matter.Matter{m}
	for _, c := range list {
		if c.ParentID == m.ID {
			gone = append(gone, c)
		}
	}
	if err := h.dropMatters(ctx, gone); err != nil {
		return err
	}
	msg := fmt.Sprintf("好，「%s」已从清单里删掉", m.Title)
	if n := len(gone) - 1; n > 0 {
		msg += fmt.Sprintf("，连同它下面的 %d 件事", n)
	}
	h.reply(ctx, channel, chat, msg+"。相关的文件和记忆都留着。")
	return nil
}

// dropMatters 把几件事从清单里删掉，连同它们名下已经结束的任务。还在办的任务不动；文件和记忆不动。
func (h *Hub) dropMatters(ctx context.Context, gone []*matter.Matter) error {
	var ids, taskIDs []string
	for _, m := range gone {
		ids = append(ids, m.ID)
		tasks, err := h.tasks.ByTag(ctx, matter.Tag(m.ID), false, 500)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			taskIDs = append(taskIDs, t.ID)
		}
	}
	// 一定要有编号才删：DeleteFinishedTasks 收到空的编号表会删掉全部已结束的任务。
	if h.deleter != nil && len(taskIDs) > 0 {
		if _, err := h.deleter.DeleteFinishedTasks(ctx, taskIDs); err != nil {
			return err
		}
	}
	h.matterMu.Lock()
	defer h.matterMu.Unlock()
	if err := h.matters.Delete(ctx, ids); err != nil {
		return err
	}
	h.log.Info("事项从清单里删掉了", "事项数", len(ids), "任务数", len(taskIDs))
	return nil
}

// tidyMatters 整理清单：30 天没动静的一次性的事删掉；长期的事、挂在长期的事下面的事，先问用户。
//
// 问的顺序是先大后小：长期的事还没问清楚之前，不问它下面的；一次只问一件，两次之间隔开 askGap。
// 用户不回答的不删，一直留着。
func (h *Hub) tidyMatters(ctx context.Context) error {
	if h.matters == nil {
		return nil
	}
	list, err := h.matters.List(ctx)
	if err != nil {
		return err
	}
	now := h.now()
	byID := make(map[string]*matter.Matter, len(list))
	kids := make(map[string]int)
	canAsk := h.triage != nil
	for _, m := range list {
		byID[m.ID] = m
		kids[m.ParentID]++
		if m.AskedAt != nil && now.Sub(*m.AskedAt) < askGap {
			canAsk = false
		}
	}
	// 名下还有任务没结束的事不算放久了：「下个月 15 号提醒我续费」的任务要等一个多月，
	// 这期间把它从清单里删掉，到时候提醒出来就接不上了。
	active, err := h.tasks.ByTag(ctx, ConversationTag, true, 500)
	if err != nil {
		return err
	}
	busy := make(map[string]bool, len(active))
	for _, t := range active {
		busy[matter.FromTags(t.Tags)] = true
	}
	// 早的先处理，问的也是放得最久的那件。
	sort.SliceStable(list, func(i, j int) bool { return list[i].LastActiveAt.Before(list[j].LastActiveAt) })
	var gone []*matter.Matter
	var ask *matter.Matter
	for _, m := range list {
		if m.Stage(now) != matter.StageExpired || busy[m.ID] {
			continue
		}
		parent := byID[m.ParentID]
		switch {
		case m.Kind == matter.KindOnce && parent == nil:
			gone = append(gone, m)
		case m.AskedAt != nil || ask != nil:
			// 已经问过在等回答，或者这一轮已经有要问的了。
		case m.Kind == matter.KindOngoing:
			ask = m
		case parent.Stage(now) != matter.StageExpired && parent.AskedAt == nil:
			// 挂在长期的事下面，而那件事还在继续：问这一件。
			ask = m
		}
	}
	if len(gone) > 0 {
		if err := h.dropMatters(ctx, gone); err != nil {
			return err
		}
	}
	if ask == nil || !canAsk {
		return nil
	}
	if _, ok := h.bindings[ask.Channel]; !ok || ask.Chat == "" {
		return nil
	}
	var q string
	if p := byID[ask.ParentID]; p != nil {
		q = fmt.Sprintf("「%s」下面的「%s」已经 30 多天没有动静了，要从清单里删掉吗？（文件和记忆都留着）", p.Title, ask.Title)
	} else {
		q = fmt.Sprintf("「%s」这件事已经 30 多天没有动静了，还继续吗？", ask.Title)
		if n := kids[ask.ID]; n > 0 {
			q += fmt.Sprintf("不继续的话，我把它和它下面的 %d 件事从清单里删掉", n)
		} else {
			q += "不继续的话，我把它从清单里删掉"
		}
		q += "（文件和记忆都留着）。"
	}
	if err := h.send(ctx, ask.Channel, ask.Chat, q); err != nil {
		return err
	}
	h.remember(ctx, "assistant", ask.Channel, "", q)
	// 这里不能用 Touch：问一句不算这件事有了动静。
	h.matterMu.Lock()
	defer h.matterMu.Unlock()
	cur, err := h.matters.Get(ctx, ask.ID)
	if err != nil {
		return err
	}
	cur.AskedAt = &now
	return h.matters.Put(ctx, cur)
}

// titleOf 从用户的一句话里取出一个标题：头一句，最多 24 个字。
func titleOf(text string) string {
	title := strings.TrimPrefix(text, voiceNote)
	if i := strings.IndexAny(title, "\n。"); i > 0 {
		title = title[:i]
	}
	if r := []rune(title); len(r) > 24 {
		title = string(r[:24]) + "…"
	}
	return title
}
