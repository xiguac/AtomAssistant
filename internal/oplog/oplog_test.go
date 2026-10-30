package oplog

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu      sync.Mutex
	entries []Entry
	fail    error
	pruned  time.Time
}

func (m *memStore) AppendLogs(_ context.Context, es []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.entries = append(m.entries, es...)
	return nil
}

func (m *memStore) PruneLogs(_ context.Context, before, _ time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruned = before
	return 0, nil
}

func (m *memStore) all() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Entry(nil), m.entries...)
}

func TestHandler_按类别记下并照常写到标准输出(t *testing.T) {
	var out bytes.Buffer
	h := NewHandler(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log := slog.New(h)

	// Start 之前的也攒着，Start 之后一并写进库。
	log.Info("启动")
	log.With(Key, Admin).Info("后台操作", "操作", "保存模型服务", "说明", "已保存 重启后生效")
	log.With(Key, Model).WithGroup("调用").Warn("模型调用", "模型", "m")
	log.Debug("调试信息不进库")
	log.With(Key, "乱写的").Error("出错了")

	st := &memStore{}
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx, st)
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for len(st.all()) < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := st.all()
	if len(got) != 4 {
		t.Fatalf("应记下 4 条（调试信息不记）: %+v", got)
	}
	if got[0].Category != System || got[0].Message != "启动" {
		t.Fatalf("没带类别的算系统: %+v", got[0])
	}
	if got[1].Category != Admin || got[1].Attrs != `操作=保存模型服务 说明="已保存 重启后生效"` {
		t.Fatalf("后台操作的类别或内容不对: %+v", got[1])
	}
	if got[2].Category != Model || got[2].Level != slog.LevelWarn || got[2].Attrs != "调用.模型=m" {
		t.Fatalf("分组里的项要带上组名: %+v", got[2])
	}
	if got[3].Category != System {
		t.Fatalf("认不得的类别算系统: %+v", got[3])
	}
	// 标准输出照旧，调试信息也在。
	if !strings.Contains(out.String(), "调试信息不进库") || !strings.Contains(out.String(), "类别=admin") {
		t.Fatalf("标准输出不应受影响:\n%s", out.String())
	}
}

func TestHandler_写不进库时不拖住产生日志的地方(t *testing.T) {
	var out bytes.Buffer
	h := NewHandler(slog.NewTextHandler(&out, nil))
	log := slog.New(h)
	for i := 0; i < queueSize+10; i++ {
		log.Info("刷屏")
	}
	// 攒满了就丢，丢了多少下次写库时补一条说明。
	st := &memStore{}
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx, st)
	deadline := time.Now().Add(3 * time.Second)
	for len(st.all()) < queueSize+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	es := st.all()
	noted := 0
	for _, e := range es {
		if strings.Contains(e.Message, "丢掉了 10 条") {
			noted++
		}
	}
	if len(es) != queueSize+1 || noted != 1 {
		t.Fatalf("应写进 %d 条并说明一次丢了 10 条，实际 %d 条、说明 %d 次", queueSize, len(es), noted)
	}

	// 写库失败只打到标准输出，不再进队列。
	bad := &memStore{fail: errors.New("库连不上")}
	h2 := NewHandler(slog.NewTextHandler(&out, nil))
	slog.New(h2).Info("一条")
	ctx2, cancel2 := context.WithCancel(context.Background())
	h2.Start(ctx2, bad)
	cancel2()
	deadline = time.Now().Add(3 * time.Second)
	for !strings.Contains(out.String(), "写入操作日志失败") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "写入操作日志失败") {
		t.Fatal("写库失败应打到标准输出")
	}
}
