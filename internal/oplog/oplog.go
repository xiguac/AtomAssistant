// Package oplog 是操作日志：把运行时的日志按类别存进数据库，在管理后台里分类查看。
//
// 程序原本的日志照旧写到标准输出（由 systemd 收进 journal）；这里在它旁边多接一路，
// 同样的内容异步写进数据库。写库失败、来不及写时丢掉，不拖住产生日志的地方——
// 日志是给人回头看的，不能因为它让助手停下来。
//
// 类别靠日志上带的 "类别" 这一项区分：各个部件拿到的 logger 在 main 里用 With 带上了它；
// 没带的算"系统"。任务的状态变化、批准驳回、工具调用来自审计表，由存储层写入审计时一并记下。
package oplog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Key 是日志上标明类别的那一项的名字。
const Key = "类别"

// Category 是日志的类别。
type Category string

const (
	Admin   Category = "admin"
	Task    Category = "task"
	Tool    Category = "tool"
	Model   Category = "model"
	Message Category = "message"
	System  Category = "system"
)

// Categories 列出全部类别，按后台里显示的先后。
var Categories = []Category{Admin, Task, Tool, Model, Message, System}

// Name 是类别给人看的名字。
func (c Category) Name() string {
	switch c {
	case Admin:
		return "后台操作"
	case Task:
		return "任务"
	case Tool:
		return "工具调用"
	case Model:
		return "模型调用"
	case Message:
		return "消息"
	}
	return "系统"
}

// Valid 报告 c 是不是认得的类别。
func (c Category) Valid() bool {
	for _, x := range Categories {
		if x == c {
			return true
		}
	}
	return false
}

// Entry 是一条日志。
type Entry struct {
	ID       string
	At       time.Time
	Level    slog.Level
	Category Category
	Message  string
	// Attrs 是日志上的其余各项，写成 键=值 的一行。
	Attrs string
}

// Query 是查日志的条件。
type Query struct {
	// Category 为空表示全部类别。
	Category Category
	// MinLevel 是最低级别，只看警告和错误时设成 slog.LevelWarn。
	MinLevel slog.Level
	// Before 非零时只取它之前的，用来往前翻页。
	Before time.Time
	Limit  int
}

// Store 存取日志，由 *pg.Store 提供。
type Store interface {
	AppendLogs(ctx context.Context, entries []Entry) error
	PruneLogs(ctx context.Context, before, adminBefore time.Time) (int64, error)
}

// Retention 是日志留多久。
const Retention = 90 * 24 * time.Hour

// AdminRetention 是后台操作类日志留多久。它涉及安全，在后台删除了也只是不再显示，留满这么久才真正删掉。
const AdminRetention = 300 * 24 * time.Hour

const (
	// queueSize 是还没写进库的日志最多攒多少条，满了新的丢掉。
	queueSize = 4096
	// batchSize 是一次写库最多写多少条。
	batchSize = 200
	// flushEvery 是攒着的日志多久写一次库。
	flushEvery = 2 * time.Second
	// pruneEvery 是多久清一次过期的日志。
	pruneEvery = 6 * time.Hour
)

// sink 是同一棵 Handler 树共用的写库队列。
type sink struct {
	mu      sync.Mutex
	pending []Entry
	dropped int
	wake    chan struct{}
}

// Handler 把日志交给下一个 Handler（写到标准输出），同时把 Info 及以上的放进写库队列。
type Handler struct {
	next  slog.Handler
	sink  *sink
	attrs []slog.Attr
	group string
}

// NewHandler 包住原来的 Handler。在 Start 之前产生的日志先攒着，Start 之后一并写库。
func NewHandler(next slog.Handler) *Handler {
	return &Handler{next: next, sink: &sink{wake: make(chan struct{}, 1)}}
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs), sink: h.sink, group: h.group,
		attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &Handler{next: h.next.WithGroup(name), sink: h.sink, attrs: h.attrs, group: g}
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	err := h.next.Handle(ctx, r)
	if r.Level >= slog.LevelInfo {
		h.sink.put(h.entry(r))
	}
	return err
}

// entry 把一条日志换成要存的样子：取出类别，其余各项写成一行。
func (h *Handler) entry(r slog.Record) Entry {
	e := Entry{At: r.Time.UTC(), Level: r.Level, Message: r.Message, Category: System}
	var parts []string
	add := func(a slog.Attr) {
		if a.Key == Key {
			if c := Category(a.Value.String()); c.Valid() {
				e.Category = c
			}
			return
		}
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		parts = append(parts, key+"="+quote(a.Value.Resolve().String()))
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		add(a)
		return true
	})
	e.Attrs = strings.Join(parts, " ")
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	return e
}

// quote 给带空白的值加上引号，和标准库文本日志的写法一样好认。
func quote(s string) string {
	if strings.ContainsAny(s, " \t\n\"=") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func (s *sink) put(e Entry) {
	s.mu.Lock()
	if len(s.pending) >= queueSize {
		s.dropped++
		s.mu.Unlock()
		return
	}
	s.pending = append(s.pending, e)
	full := len(s.pending) >= batchSize
	s.mu.Unlock()
	if full {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *sink) take() ([]Entry, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.pending)
	if n > batchSize {
		n = batchSize
	}
	out := append([]Entry(nil), s.pending[:n]...)
	s.pending = s.pending[n:]
	dropped := s.dropped
	s.dropped = 0
	return out, dropped
}

// Start 开始把攒着的日志写进库，并定期清掉过期的，直到 ctx 结束（结束前把剩下的写完）。
// 返回的通道在剩下的写完后关闭：关库之前要等它，不然最后一批写进的是已关掉的连接池。
// 写库失败只打到标准输出，不再进队列：否则一次失败会滚出一串失败。
func (h *Handler) Start(ctx context.Context, st Store) <-chan struct{} {
	done := make(chan struct{})
	next := slog.New(h.next)
	flush := func(c context.Context) {
		for {
			batch, dropped := h.sink.take()
			if dropped > 0 {
				batch = append(batch, Entry{At: time.Now().UTC(), Level: slog.LevelWarn, Category: System,
					Message: fmt.Sprintf("操作日志来不及写入，丢掉了 %d 条", dropped)})
			}
			if len(batch) == 0 {
				return
			}
			if err := st.AppendLogs(c, batch); err != nil {
				next.Error("写入操作日志失败", "条数", len(batch), "err", err)
				return
			}
		}
	}
	prune := func() {
		if n, err := st.PruneLogs(ctx, time.Now().Add(-Retention), time.Now().Add(-AdminRetention)); err != nil {
			next.Error("清理过期的操作日志失败", "err", err)
		} else if n > 0 {
			next.Info("已清理过期的操作日志", "条数", n)
		}
	}
	go func() {
		defer close(done)
		tick := time.NewTicker(flushEvery)
		defer tick.Stop()
		lastPrune := time.Time{}
		for {
			select {
			case <-ctx.Done():
				c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				flush(c)
				cancel()
				return
			case <-tick.C:
			case <-h.sink.wake:
			}
			flush(ctx)
			if time.Since(lastPrune) > pruneEvery {
				prune()
				lastPrune = time.Now()
			}
		}
	}()
	return done
}
