// Package memvec 给记忆检索加上向量召回：每条记忆编码成向量存进本机的 Qdrant，
// 检索时把任务的文字编码一次，在向量库里找意思相近的记忆。
//
// 向量库里只有记忆编号和转过的向量（见 rotate.go），没有记忆原文；原文仍只在加密的数据库里。
// 给用户看的始终是记忆原文。向量服务或向量库不可用时，检索退回词元重合，助手照常工作。
package memvec

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/xiguac/atomassistant/internal/memory"
)

// Source 读取记忆，由 *pg.MemoryStore 提供。
type Source interface {
	Get(ctx context.Context, id string) (*memory.Memory, error)
	List(ctx context.Context, q memory.Query) ([]*memory.Memory, error)
}

const (
	// searchLimit 是一次向量检索取回的条数，scoreFloor 是认为"有点相关"的最低余弦相似度。
	// 用 bge-small-zh-v1.5 实测：不相关的句子对余弦相似度中位数约 0.31、最高约 0.44，
	// 相关的约 0.44～0.75。低于 scoreFloor 的算 0，到 scoreFull 算满分。
	searchLimit = 100
	scoreFloor  = 0.40
	scoreFull   = 0.75
	// retryEvery 是向量服务或向量库不可用时多久再试一次；resyncEvery 是多久整体核对一遍，补上漏掉的。
	retryEvery  = time.Minute
	resyncEvery = 6 * time.Hour
	// queryTimeout 是检索时编码加查询的时限：超了就按词元重合，不拖慢任务。
	queryTimeout = 5 * time.Second
)

// Index 维护向量库里的记忆向量，并作为记忆检索的相似度实现。
type Index struct {
	embed    *Embedder
	vectors  *Qdrant
	memories Source
	key      []byte
	log      *slog.Logger

	mu     sync.Mutex
	ready  bool
	rot    *rotation
	coll   string
	hashes map[string]string // 已入库的记忆编号 → 内容摘要
	dirty  map[string]bool
	wake   chan struct{}
	// down 记着上一次是不是不可用，只在状态变化时记日志，免得刷屏。
	down bool
}

// New 构造索引。key 是数据密钥派生的子密钥（seal.Box.VectorKey），决定向量怎么转、编号怎么算。
func New(embed *Embedder, vectors *Qdrant, memories Source, key []byte, log *slog.Logger) *Index {
	return &Index{embed: embed, vectors: vectors, memories: memories, key: key, log: log,
		hashes: map[string]string{}, dirty: map[string]bool{}, wake: make(chan struct{}, 1)}
}

var _ memory.ScenePreparer = (*Index)(nil)

// Notify 告诉索引一条记忆变了（新增、修改、归档）。不阻塞，由 Run 在后台同步。
func (x *Index) Notify(id string) {
	x.mu.Lock()
	x.dirty[id] = true
	x.mu.Unlock()
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// Run 先把已有的记忆整体核对一遍，之后随记忆的变化实时同步，直到 ctx 结束。
func (x *Index) Run(ctx context.Context) {
	resync := time.NewTimer(0)
	defer resync.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-resync.C:
			if err := x.rebuild(ctx); err != nil {
				x.unavailable(ctx, err)
				resync.Reset(retryEvery)
				continue
			}
			x.available()
			resync.Reset(resyncEvery)
		case <-x.wake:
			x.mu.Lock()
			ready := x.ready
			var ids []string
			if ready {
				for id := range x.dirty {
					ids = append(ids, id)
				}
				clear(x.dirty)
			}
			x.mu.Unlock()
			if !ready {
				continue // 整体核对时会一并处理
			}
			if err := x.sync(ctx, ids); err != nil {
				x.unavailable(ctx, err)
				// 没同步上的放回去，下次再来；一直不行就等整体核对。
				x.mu.Lock()
				for _, id := range ids {
					x.dirty[id] = true
				}
				x.ready = false
				x.mu.Unlock()
				resync.Reset(retryEvery)
			}
		}
	}
}

func (x *Index) unavailable(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return // 正在退出，不是不可用
	}
	x.mu.Lock()
	first := !x.down
	x.down = true
	x.mu.Unlock()
	if first {
		x.log.Warn("向量记忆暂不可用，记忆检索按文字重合进行，稍后自动重试", "err", err)
	}
}

func (x *Index) available() {
	x.mu.Lock()
	was := x.down
	x.down = false
	n := len(x.hashes)
	x.mu.Unlock()
	if was {
		x.log.Info("向量记忆已恢复", "条数", n)
	}
}

// rebuild 连上向量服务和向量库，把向量库调整到和数据库里的记忆一致。
func (x *Index) rebuild(ctx context.Context) error {
	model, err := x.embed.Model(ctx)
	if err != nil {
		return err
	}
	probe, err := x.embed.Embed(ctx, []string{"维度"})
	if err != nil {
		return err
	}
	dim := len(probe[0])
	x.mu.Lock()
	rot := x.rot
	x.mu.Unlock()
	if rot == nil || rot.dim != dim {
		rot = newRotation(x.key, dim, model)
	}
	coll := "atom_memory_" + rot.fp
	if err := x.vectors.EnsureCollection(ctx, coll, dim); err != nil {
		return err
	}
	points, err := x.vectors.All(ctx, coll)
	if err != nil {
		return err
	}
	indexed := map[string]string{}
	pointOf := map[string]string{}
	var stale []string
	for _, p := range points {
		mid, _ := p.Payload["m"].(string)
		h, _ := p.Payload["h"].(string)
		if mid == "" || p.ID != x.pointID(mid) {
			stale = append(stale, p.ID)
			continue
		}
		indexed[mid], pointOf[mid] = h, p.ID
	}
	list, err := x.memories.List(ctx, memory.Query{})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	live := map[string]bool{}
	var todo []*memory.Memory
	for _, m := range list {
		if !m.Retrievable(now) {
			continue
		}
		live[m.ID] = true
		if indexed[m.ID] != x.hash(rot, m) {
			todo = append(todo, m)
		}
	}
	for mid, id := range pointOf {
		if !live[mid] {
			stale = append(stale, id)
			delete(indexed, mid)
		}
	}
	if err := x.vectors.Delete(ctx, coll, stale); err != nil {
		return err
	}
	if err := x.upsert(ctx, rot, coll, todo, indexed); err != nil {
		return err
	}
	x.mu.Lock()
	x.rot, x.coll, x.hashes, x.ready = rot, coll, indexed, true
	x.mu.Unlock()
	if len(todo) > 0 || len(stale) > 0 {
		x.log.Info("向量记忆已与数据库核对", "新增或更新", len(todo), "移除", len(stale), "总数", len(indexed))
	}
	return nil
}

// sync 同步几条变了的记忆。
func (x *Index) sync(ctx context.Context, ids []string) error {
	x.mu.Lock()
	rot, coll := x.rot, x.coll
	x.mu.Unlock()
	now := time.Now().UTC()
	var todo []*memory.Memory
	var gone []string
	hashes := map[string]string{}
	x.mu.Lock()
	for k, v := range x.hashes {
		hashes[k] = v
	}
	x.mu.Unlock()
	for _, id := range ids {
		m, err := x.memories.Get(ctx, id)
		switch {
		case errors.Is(err, memory.ErrNotFound):
			m = nil
		case err != nil:
			return err
		}
		if m == nil || !m.Retrievable(now) {
			if _, ok := hashes[id]; ok {
				gone = append(gone, x.pointID(id))
				delete(hashes, id)
			}
			continue
		}
		if hashes[id] != x.hash(rot, m) {
			todo = append(todo, m)
		}
	}
	if err := x.vectors.Delete(ctx, coll, gone); err != nil {
		return err
	}
	if err := x.upsert(ctx, rot, coll, todo, hashes); err != nil {
		return err
	}
	x.mu.Lock()
	x.hashes = hashes
	x.mu.Unlock()
	return nil
}

// upsert 编码几条记忆写进向量库，并在 hashes 里记下它们的内容摘要。
func (x *Index) upsert(ctx context.Context, rot *rotation, coll string, ms []*memory.Memory, hashes map[string]string) error {
	for len(ms) > 0 {
		batch := ms[:min(len(ms), maxEmbedBatch)]
		ms = ms[len(batch):]
		texts := make([]string, len(batch))
		for i, m := range batch {
			texts[i] = text(m)
		}
		vecs, err := x.embed.Embed(ctx, texts)
		if err != nil {
			return err
		}
		points := make([]Point, len(batch))
		for i, m := range batch {
			points[i] = Point{ID: x.pointID(m.ID), Vector: rot.apply(vecs[i]),
				Payload: map[string]any{"m": m.ID, "h": x.hash(rot, m)}}
		}
		if err := x.vectors.Upsert(ctx, coll, points); err != nil {
			return err
		}
		for _, m := range batch {
			hashes[m.ID] = x.hash(rot, m)
		}
	}
	return nil
}

// Prepare 把场景文字编码一次，在向量库里找相近的记忆。不可用时退回词元重合。
func (x *Index) Prepare(ctx context.Context, sceneText string) memory.Similarity {
	x.mu.Lock()
	ready, rot, coll := x.ready, x.rot, x.coll
	x.mu.Unlock()
	if !ready || strings.TrimSpace(sceneText) == "" {
		return memory.TokenOverlap{}
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	vecs, err := x.embed.Embed(ctx, []string{sceneText})
	if err != nil {
		x.log.Warn("编码任务文字失败，这次按文字重合检索记忆", "err", err)
		return memory.TokenOverlap{}
	}
	hits, err := x.vectors.Search(ctx, coll, rot.apply(vecs[0]), searchLimit, scoreFloor)
	if err != nil {
		x.log.Warn("向量检索失败，这次按文字重合检索记忆", "err", err)
		return memory.TokenOverlap{}
	}
	sims := make(map[string]float64, len(hits))
	for _, h := range hits {
		if mid, _ := h.Payload["m"].(string); mid != "" {
			sims[mid] = h.Score
		}
	}
	return scored{sims: sims}
}

// scored 是一次检索的结果：向量相似度和词元重合取高的那个，向量召回只会让结果更全，不会漏掉原来能找到的。
type scored struct {
	sims map[string]float64
}

func (s scored) Score(sceneText string, m *memory.Memory) float64 {
	tok := memory.TokenOverlap{}.Score(sceneText, m)
	cos, ok := s.sims[m.ID]
	if !ok {
		return tok
	}
	v := (cos - scoreFloor) / (scoreFull - scoreFloor)
	v = max(0, min(1, v))
	return max(tok, v)
}

// text 是一条记忆拿去编码的文字，与词元重合比对的内容相同。
func text(m *memory.Memory) string {
	parts := []string{}
	for _, p := range append([]string{m.Domain, m.Attribute, m.Current.Value}, m.Tags...) {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// hash 是一条记忆编码用的内容的摘要（带密钥，看不出内容），内容没变就不必重新编码。
func (x *Index) hash(rot *rotation, m *memory.Memory) string {
	mac := hmac.New(sha256.New, x.key)
	mac.Write([]byte("memvec content " + rot.fp + "\x00"))
	mac.Write([]byte(text(m)))
	return hex.EncodeToString(mac.Sum(nil)[:12])
}

// pointID 是一条记忆在向量库里的编号：由记忆编号算出的 UUID。
func (x *Index) pointID(memoryID string) string {
	mac := hmac.New(sha256.New, x.key)
	mac.Write([]byte("memvec point\x00" + memoryID))
	b := mac.Sum(nil)[:16]
	b[6] = b[6]&0x0f | 0x80 // UUIDv8：自定义生成方式
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:])
}

// Score 让 Index 本身也是一个 memory.Similarity。检索器会先调 Prepare 拿到按场景算好的那个；
// 没经过 Prepare 时只按文字重合。
func (x *Index) Score(sceneText string, m *memory.Memory) float64 {
	return memory.TokenOverlap{}.Score(sceneText, m)
}
