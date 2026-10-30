package memvec

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
)

func TestRotation_正交且可重复(t *testing.T) {
	a := newRotation([]byte("k1"), 64, "m")
	b := newRotation([]byte("k1"), 64, "m")
	c := newRotation([]byte("k2"), 64, "m")
	if a.fp != b.fp || a.fp == c.fp || a.fp == newRotation([]byte("k1"), 64, "other").fp {
		t.Fatal("同一把密钥、同一个模型应得到同一个矩阵，换了任何一样都应不同")
	}
	for i := range a.rows {
		for k := range a.rows {
			want := 0.0
			if i == k {
				want = 1
			}
			if d := dot(a.rows[i], a.rows[k]); math.Abs(d-want) > 1e-9 {
				t.Fatalf("不是正交矩阵: 第 %d、%d 行内积 %g", i, k, d)
			}
		}
	}
	// 转过之后夹角不变。
	u, v := make([]float32, 64), make([]float32, 64)
	for i := range u {
		u[i], v[i] = float32(math.Sin(float64(i))), float32(math.Cos(float64(i*3)))
	}
	cos := func(x, y []float32) float64 {
		var s, nx, ny float64
		for i := range x {
			s += float64(x[i]) * float64(y[i])
			nx += float64(x[i]) * float64(x[i])
			ny += float64(y[i]) * float64(y[i])
		}
		return s / math.Sqrt(nx*ny)
	}
	if d := math.Abs(cos(u, v) - cos(a.apply(u), a.apply(v))); d > 1e-5 {
		t.Fatalf("旋转改变了夹角: %g", d)
	}
}

type memSource struct {
	mu sync.Mutex
	ms map[string]*memory.Memory
}

func (s *memSource) Get(_ context.Context, id string) (*memory.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.ms[id]; ok {
		return m, nil
	}
	return nil, memory.ErrNotFound
}

func (s *memSource) List(_ context.Context, _ memory.Query) ([]*memory.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*memory.Memory
	for _, m := range s.ms {
		if m.Status != memory.StatusArchived {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *memSource) put(m *memory.Memory) {
	s.mu.Lock()
	s.ms[m.ID] = m
	s.mu.Unlock()
}

func newMem(domain, value string) *memory.Memory {
	now := time.Now().UTC().Add(-time.Hour)
	return &memory.Memory{ID: core.NewID(), Kind: memory.KindFact, Subject: "user", Domain: domain, Status: memory.StatusActive,
		Temporality: memory.TemporalDurable, ValidFrom: now, CreatedAt: now, UpdatedAt: now,
		Current: memory.Revision{ID: core.NewID(), Value: value}}
}

// 要真的向量服务和 Qdrant：ATOM_TEST_EMBED=http://127.0.0.1:端口，ATOM_TEST_QDRANT=http://127.0.0.1:端口，
// ATOM_TEST_QDRANT_KEY 是它的 API key。没设就跳过。
func TestIndex_实时同步并按意思召回(t *testing.T) {
	embedURL, qdrantURL := os.Getenv("ATOM_TEST_EMBED"), os.Getenv("ATOM_TEST_QDRANT")
	if embedURL == "" || qdrantURL == "" {
		t.Skip("未设置 ATOM_TEST_EMBED 与 ATOM_TEST_QDRANT")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &memSource{ms: map[string]*memory.Memory{}}
	allergy, birthday, office := newMem("饮食", "客户张总对海鲜过敏"), newMem("家人", "妈妈的生日是 3 月 12 日"), newMem("工作", "公司地址在深圳南山科技园")
	for _, m := range []*memory.Memory{allergy, birthday, office} {
		src.put(m)
	}
	// 每次用一把新密钥：落到一个新集合里，不受上一次测试残留的影响。
	key := []byte(core.NewID())
	q := &Qdrant{URL: qdrantURL, APIKey: os.Getenv("ATOM_TEST_QDRANT_KEY")}
	x := New(&Embedder{URL: embedURL}, q, src, key, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go x.Run(ctx)

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("等不到: %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	count := func() int {
		x.mu.Lock()
		defer x.mu.Unlock()
		return len(x.hashes)
	}
	waitFor("整体核对完成", func() bool { return count() == 3 })

	// 字面上没有一个词重合，意思相近的也要找得到。
	sim := x.Prepare(ctx, "给我妈准备生日礼物")
	if _, ok := sim.(scored); !ok {
		t.Fatal("就绪后应走向量检索")
	}
	if b, o := sim.Score("给我妈准备生日礼物", birthday), sim.Score("给我妈准备生日礼物", office); b < 0.5 || o != 0 {
		t.Fatalf("生日那条应召回、地址那条不应: %.2f %.2f", b, o)
	}

	// 向量库里不能有原文。
	points, err := q.All(ctx, x.coll)
	if err != nil || len(points) != 3 {
		t.Fatalf("向量库里应有 3 条: %d %v", len(points), err)
	}
	for _, p := range points {
		for k, v := range p.Payload {
			if k != "m" && k != "h" {
				t.Fatalf("向量库里不应有别的字段: %s=%v", k, v)
			}
		}
	}

	// 改动实时同步：新增一条、归档一条。
	seat := newMem("出行", "坐飞机喜欢靠窗的座位")
	src.put(seat)
	x.Notify(seat.ID)
	archived := *office
	archived.Status = memory.StatusArchived
	src.put(&archived)
	x.Notify(office.ID)
	waitFor("新增的入库、归档的移除", func() bool {
		x.mu.Lock()
		defer x.mu.Unlock()
		_, hasSeat := x.hashes[seat.ID]
		_, hasOffice := x.hashes[office.ID]
		return hasSeat && !hasOffice
	})
	if points, _ := q.All(ctx, x.coll); len(points) != 3 {
		t.Fatalf("同步后向量库里应有 3 条: %d", len(points))
	}

	// 重启后整体核对：内容没变的不重新编码，删掉的补删。
	delete(src.ms, seat.ID)
	y := New(&Embedder{URL: embedURL}, q, src, key, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := y.rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if points, _ := q.All(ctx, y.coll); len(points) != 2 || y.coll != x.coll {
		t.Fatalf("重启核对后应剩 2 条且沿用同一个集合: %d", len(points))
	}
}

func TestIndex_不可用时退回文字重合(t *testing.T) {
	src := &memSource{ms: map[string]*memory.Memory{}}
	x := New(&Embedder{URL: "http://127.0.0.1:1"}, &Qdrant{URL: "http://127.0.0.1:1"}, src, []byte("k"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := x.rebuild(context.Background()); err == nil {
		t.Fatal("连不上时应报错")
	}
	if _, ok := x.Prepare(context.Background(), "任意文字").(memory.TokenOverlap); !ok {
		t.Fatal("没就绪时应退回文字重合")
	}
	m := newMem("饮食", "不吃辣")
	if got := (scored{sims: map[string]float64{m.ID: 0.30}}).Score("不吃辣", m); got != 1 {
		t.Fatalf("向量分低时应取文字重合的分: %v", got)
	}
}
