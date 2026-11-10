// Package matter 是用户交代过的事项：一张清单，助手手头有哪些事、各自办到了哪。
//
// 事项和任务是两层。任务是一次执行：建起来、办完、结束。用户说的"一件事"却常常不止一次：
// 上午交代、下午补一句，过两天又问结果怎么样——每次都是一个新任务，说的是同一件事。
// 所以在任务之上记一层事项：新来的一句话先判断是清单里的哪一件，还是一件新的；
// 是已有的，新任务就带着这件事此前的进展去办，不用从聊天记录里重新猜。
//
// 事项分两种：
//   - 一次性的（KindOnce）：有明确的完成点，如"收这次作业""订一张票"。办完后慢慢淡出：
//     7 天没动静降为低优先，14 天归档，30 天从清单里删掉。
//   - 长期的（KindOngoing）：没有完成点，如"这门课""每周的例会"。不随时间淡出；
//     30 天没动静时问用户一句还继续吗，用户说不继续才删，连同它下面的事一起。
//
// 一次性的事可以挂在长期的事下面（"收第三次作业"挂在"某门课"下）。挂着的事到了 30 天不直接删，
// 先看上面那件长期的事：它还继续，再问这件要不要删。
//
// 删掉的只是清单里的记录和它名下已结束的任务。办事时存下的记忆、工作区里的文件都不动。
package matter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiguac/atomassistant/internal/session"
)

// Kind 是事项的种类。
type Kind string

const (
	KindOnce    Kind = "once"
	KindOngoing Kind = "ongoing"
)

// ParseKind 校验种类，认不得的按一次性算：一次性的会自己淡出，判错了代价小。
func ParseKind(s string) Kind {
	if Kind(strings.ToLower(strings.TrimSpace(s))) == KindOngoing {
		return KindOngoing
	}
	return KindOnce
}

// TagPrefix 是任务上标明它属于哪件事的标签前缀，完整形式为 matter:事项编号。
const TagPrefix = "matter:"

// Tag 生成任务上的事项标签。
func Tag(id string) string { return TagPrefix + id }

// FromTags 从任务的标签里取出它属于哪件事，没有返回空。
func FromTags(tags []string) string {
	for _, t := range tags {
		if id, ok := strings.CutPrefix(t, TagPrefix); ok && id != "" {
			return id
		}
	}
	return ""
}

// 各个阶段的时间线，都按"最后一次说到或办过这件事"起算。
const (
	LowAfter     = 7 * 24 * time.Hour
	ArchiveAfter = 14 * 24 * time.Hour
	ExpireAfter  = 30 * 24 * time.Hour
)

// Stage 是一件事按时间算出来的阶段。它不存库：最后活动的时间一变，阶段跟着变，提起来就又活了。
type Stage int

const (
	// StageActive 在办或最近说到过。
	StageActive Stage = iota
	// StageLow 一次性的事 7 天没动静：还列在清单里，排在后面。
	StageLow
	// StageArchived 一次性的事 14 天没动静：清单里只留一个名字，用户提起才接上。
	StageArchived
	// StageExpired 30 天没动静：一次性的该删了；长期的该问用户还继续吗。
	StageExpired
)

// Note 是一件事的一次进展：用户说了什么，办成了什么样。
type Note struct {
	At     time.Time `json:"at"`
	Ask    string    `json:"ask"`
	Result string    `json:"result"`
}

// 进展的条数和长度上限。超出条数时丢最早的：一件事拖得再久，要接上的也是最近这些。
const (
	maxNotes    = 30
	askRunes    = 300
	resultRunes = 500
)

// Matter 是一件事。
type Matter struct {
	ID string
	// ParentID 是它挂在哪件长期的事下面，没有为空。
	ParentID string
	Title    string
	Kind     Kind
	Notes    []Note
	// Channel 与 Chat 是用户最近在哪个会话里说到它的，要问用户话时发到这里。
	Channel, Chat string
	// LastActiveAt 是最后一次说到或办过这件事的时间。
	LastActiveAt time.Time
	// AskedAt 非空表示已经问过用户这件事还要不要留着，还没等到回答。
	AskedAt   *time.Time
	CreatedAt time.Time
}

// Stage 返回这件事现在的阶段。
func (m *Matter) Stage(now time.Time) Stage {
	idle := now.Sub(m.LastActiveAt)
	switch {
	case idle >= ExpireAfter:
		return StageExpired
	case m.Kind == KindOngoing:
		return StageActive
	case idle >= ArchiveAfter:
		return StageArchived
	case idle >= LowAfter:
		return StageLow
	}
	return StageActive
}

// Touch 记下这件事刚被说到或办过。等用户回答的那个问题也就不用等了：他已经在说这件事。
func (m *Matter) Touch(now time.Time) {
	m.LastActiveAt = now
	m.AskedAt = nil
}

// AddNote 记一次进展。
func (m *Matter) AddNote(n Note) {
	n.Ask, n.Result = clip(n.Ask, askRunes), clip(n.Result, resultRunes)
	m.Notes = append(m.Notes, n)
	if len(m.Notes) > maxNotes {
		m.Notes = m.Notes[len(m.Notes)-maxNotes:]
	}
}

// Latest 把最近一次进展说成一行，没有进展返回空。
func (m *Matter) Latest(max int) string {
	if len(m.Notes) == 0 {
		return ""
	}
	n := m.Notes[len(m.Notes)-1]
	return clip(oneLine(n.Ask)+" → "+oneLine(n.Result), max)
}

// KindName 是种类给人看的叫法。
func KindName(k Kind) string {
	if k == KindOngoing {
		return "长期的事"
	}
	return "一次性的事"
}

// Render 把一件事此前的进展写给接着办它的任务看。parent 是它挂在哪件事下面，可以为 nil。
func Render(m, parent *Matter) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "这件事：%s（%s）", m.Title, KindName(m.Kind))
	if parent != nil {
		fmt.Fprintf(&sb, "，属于「%s」", parent.Title)
	}
	sb.WriteString("\n")
	if len(m.Notes) == 0 {
		sb.WriteString("此前还没有办出结果的记录。\n")
		return sb.String()
	}
	// 从最近的往回取，总长不超过 renderRunes：要接上的是最近这些，太早的省掉。
	var lines []string
	total := 0
	for i := len(m.Notes) - 1; i >= 0; i-- {
		n := m.Notes[i]
		line := fmt.Sprintf("[%s] 用户：%s\n结果：%s\n", n.At.Local().Format("01-02 15:04"), n.Ask, n.Result)
		if total += utf8.RuneCountInString(line); total > renderRunes && len(lines) > 0 {
			lines = append(lines, "（更早的省略了）\n")
			break
		}
		lines = append(lines, line)
	}
	sb.WriteString("此前的进展，早的在前：\n")
	for i := len(lines) - 1; i >= 0; i-- {
		sb.WriteString(lines[i])
	}
	return sb.String()
}

// renderRunes 是写给任务看的进展的总长上限。
const renderRunes = 4000

// Store 是事项的持久化。
type Store interface {
	// Put 保存一件事，已有的整条覆盖。
	Put(ctx context.Context, m *Matter) error
	// Get 取一件事，没有返回 ErrNotFound。
	Get(ctx context.Context, id string) (*Matter, error)
	// List 返回清单里全部的事。
	List(ctx context.Context) ([]*Matter, error)
	Delete(ctx context.Context, ids []string) error
}

// ErrNotFound 表示清单里没有这件事。
var ErrNotFound = errors.New("matter: 没有这件事")

// Reader 把事项读给任务和每日摘要用。
type Reader struct {
	Store Store
}

// TaskContext 返回任务所属的那件事此前的进展。任务不属于任何事、或者那件事已经不在了，ok 为假。
func (r Reader) TaskContext(ctx context.Context, tags []string) (text string, ok bool, err error) {
	id := FromTags(tags)
	if id == "" {
		return "", false, nil
	}
	m, err := r.Store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var parent *Matter
	if m.ParentID != "" {
		if parent, err = r.Store.Get(ctx, m.ParentID); errors.Is(err, ErrNotFound) {
			parent = nil
		} else if err != nil {
			return "", false, err
		}
	}
	return Render(m, parent), true, nil
}

// DayDigest 把某一天各件事的进展写成那天的摘要。那天没有任何进展时返回空。
func (r Reader) DayDigest(ctx context.Context, day string) (string, error) {
	all, err := r.Store.List(ctx)
	if err != nil {
		return "", err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt.Before(all[j].CreatedAt) })
	titles := make(map[string]string, len(all))
	for _, m := range all {
		titles[m.ID] = m.Title
	}
	var sb strings.Builder
	for _, m := range all {
		var lines []string
		for _, n := range m.Notes {
			if session.Day(n.At) == day {
				lines = append(lines, fmt.Sprintf("- [%s] 用户：%s\n  结果：%s", n.At.Local().Format("15:04"), oneLine(n.Ask), oneLine(n.Result)))
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "【%s】%s", m.Title, KindName(m.Kind))
		if p := titles[m.ParentID]; p != "" {
			fmt.Fprintf(&sb, "，属于「%s」", p)
		}
		sb.WriteString("\n" + strings.Join(lines, "\n") + "\n")
	}
	if sb.Len() == 0 {
		return "", nil
	}
	return "这一天办的事：\n" + sb.String(), nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
