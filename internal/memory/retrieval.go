package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Ring 是上下文装配的四个环。低关联的不被丢弃，而是降级成索引行。
type Ring int

const (
	// RingCritical 必达环：安全类属性。旁路强制注入，不参与打分也不参与裁剪。
	RingCritical Ring = iota
	// RingHigh 高关联环：原文下发。
	RingHigh
	// RingMedium 中关联环：摘要形式下发。
	RingMedium
	// RingIndex 低关联环：只给一行索引，模型可按需展开。
	RingIndex
)

func (r Ring) String() string {
	switch r {
	case RingCritical:
		return "critical"
	case RingHigh:
		return "high"
	case RingMedium:
		return "medium"
	case RingIndex:
		return "index"
	}
	return "unknown"
}

// Scene 描述当前任务的场景，用于匹配偏好条件与计算关联度。
type Scene struct {
	// Condition 是场景维度，如 {场景:接待客户, 时段:晚上}。
	Condition Condition
	Domain    string
	// Entities 是任务显式关联的实体，如 ["客户张总"]。
	Entities []string
	Tags     []string
	// Text 是任务目标等自由文本，用于相似度召回。
	Text string
}

// Similarity 计算一条记忆与场景文本的相似度，取值 [0,1]。
//
// 抽成接口是为了将来换成 pgvector 的向量检索而不动调用方。
// 默认实现是词元重合度——够用，且不需要为每条记忆算 embedding。
type Similarity interface {
	Score(text string, m *Memory) float64
}

// ScenePreparer 由要先就整段场景文字做一次准备的相似度实现提供，如向量检索：
// 场景文字只编码、只检索一次，再给每条记忆打分。Prepare 自己处理不可用的情况（如退回词元重合）。
type ScenePreparer interface {
	Prepare(ctx context.Context, text string) Similarity
}

// TokenOverlap 是基于词元重合的默认相似度实现。
type TokenOverlap struct{}

// Score 返回场景文本与记忆文本的重合比例。
func (TokenOverlap) Score(text string, m *Memory) float64 {
	q := tokenize(text)
	if len(q) == 0 {
		return 0
	}
	d := tokenize(strings.Join([]string{
		m.Domain, m.Attribute, m.Current.Value, strings.Join(m.Tags, " "),
	}, " "))
	if len(d) == 0 {
		return 0
	}
	hit := 0
	for t := range q {
		if d[t] {
			hit++
		}
	}
	return float64(hit) / float64(len(q))
}

// Item 是一条被选中的记忆及其关联度。
type Item struct {
	Memory *Memory
	Ring   Ring
	Score  float64
	// Reasons 说明它为什么被选中，写进审计便于排查"助手为什么想起这件事"。
	Reasons []string
}

// Assembly 是一次上下文装配的结果。
type Assembly struct {
	Critical []Item
	High     []Item
	Medium   []Item
	Index    []Item
	// Dropped 是完全没进上下文的数量。
	Dropped int
}

// Budget 控制各环的容量（按字符数）。
type Budget struct {
	Total int
	// HighRatio / MediumRatio / IndexRatio 是各环占比，必达环不受限。
	HighRatio   float64
	MediumRatio float64
	IndexRatio  float64
}

func (b *Budget) applyDefaults() {
	if b.Total <= 0 {
		b.Total = 8000
	}
	if b.HighRatio <= 0 {
		b.HighRatio = 0.40
	}
	if b.MediumRatio <= 0 {
		b.MediumRatio = 0.25
	}
	if b.IndexRatio <= 0 {
		b.IndexRatio = 0.10
	}
}

// Weights 是关联度打分的权重。
type Weights struct {
	Similarity float64
	TagOverlap float64
	EntityHit  float64
	Recency    float64
	HitRate    float64
	LowConf    float64
}

func defaultWeights() Weights {
	return Weights{
		Similarity: 0.25,
		// 实体精确命中权重最高：任务提到"张总"，关于张总的记忆就该排前面，
		// 这比任何语义相似度都可靠。
		EntityHit:  0.35,
		TagOverlap: 0.20,
		Recency:    0.10,
		HitRate:    0.10,
		LowConf:    0.30,
	}
}

// Retriever 负责上下文装配。
type Retriever struct {
	store   Store
	sim     Similarity
	weights Weights
	now     func() time.Time
}

// NewRetriever 构造检索器。sim 为 nil 时使用词元重合。
func NewRetriever(s Store, sim Similarity) *Retriever {
	if sim == nil {
		sim = TokenOverlap{}
	}
	return &Retriever{
		store:   s,
		sim:     sim,
		weights: defaultWeights(),
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Assemble 按四环模型装配上下文。
//
// 核心原则：永远不把全部记忆一股脑丢给模型。按关联度分环，
// 低关联的只给索引行，让模型自己决定要不要展开——把检索权交给模型，
// 而不是由我们一次性预填。
func (r *Retriever) Assemble(ctx context.Context, subject string, scene Scene, budget Budget) (Assembly, error) {
	budget.applyDefaults()
	now := r.now()

	all, err := r.store.List(ctx, Query{Subject: subject})
	if err != nil {
		return Assembly{}, err
	}

	var asm Assembly
	scored := make([]Item, 0, len(all))
	sim := r.sim
	if p, ok := sim.(ScenePreparer); ok {
		sim = p.Prepare(ctx, scene.Text)
	}

	for _, m := range all {
		if !m.Retrievable(now) {
			continue
		}
		// 安全类属性走旁路：强制注入，不打分、不排序、不裁剪。
		// 相似度召回失败一次就是事故——"客户海鲜过敏"不能因为
		// 这次任务的措辞里没出现"过敏"两个字就被漏掉。
		if m.SafetyCritical {
			asm.Critical = append(asm.Critical, Item{
				Memory: m, Ring: RingCritical, Score: 1,
				Reasons: []string{"安全相关属性，强制注入"},
			})
			continue
		}
		score, reasons := r.score(sim, m, scene, now)
		scored = append(scored, Item{Memory: m, Score: score, Reasons: reasons})
	}

	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })

	highCap := int(float64(budget.Total) * budget.HighRatio)
	medCap := int(float64(budget.Total) * budget.MediumRatio)
	idxCap := int(float64(budget.Total) * budget.IndexRatio)

	for i := range scored {
		it := scored[i]
		switch {
		case highCap > 0:
			body := RenderFull(it.Memory)
			if len(body) <= highCap {
				it.Ring = RingHigh
				highCap -= len(body)
				asm.High = append(asm.High, it)
				continue
			}
			fallthrough
		case medCap > 0:
			body := RenderSummary(it.Memory)
			if len(body) <= medCap {
				it.Ring = RingMedium
				medCap -= len(body)
				asm.Medium = append(asm.Medium, it)
				continue
			}
			fallthrough
		case idxCap > 0:
			line := RenderIndex(it.Memory)
			if len(line) <= idxCap {
				it.Ring = RingIndex
				idxCap -= len(line)
				asm.Index = append(asm.Index, it)
				continue
			}
			asm.Dropped++
		default:
			asm.Dropped++
		}
	}
	return asm, nil
}

// score 计算一条记忆与场景的关联度。
func (r *Retriever) score(sim Similarity, m *Memory, scene Scene, now time.Time) (float64, []string) {
	w := r.weights
	var total float64
	var reasons []string

	if s := sim.Score(scene.Text, m); s > 0 {
		total += w.Similarity * s
		reasons = append(reasons, fmt.Sprintf("文本相关度 %.2f", s))
	}

	// 实体精确命中：任务提到"张总"，关于张总的记忆直接顶上去。
	for _, e := range scene.Entities {
		if m.Subject == e || strings.Contains(m.Current.Value, e) {
			total += w.EntityHit
			reasons = append(reasons, "命中实体 "+e)
			break
		}
	}

	if overlap := tagOverlap(scene.Tags, m.Tags); overlap > 0 {
		total += w.TagOverlap * overlap
		reasons = append(reasons, fmt.Sprintf("标签重合 %.2f", overlap))
	}
	if scene.Domain != "" && m.Domain == scene.Domain {
		total += w.TagOverlap
		reasons = append(reasons, "同领域")
	}

	// 时间新近性：一年为尺度线性衰减。
	age := now.Sub(m.UpdatedAt)
	if recency := 1 - age.Hours()/(365*24); recency > 0 {
		total += w.Recency * recency
	}

	// 被证明有用的记忆更容易再被用到。
	if m.HitCount > 0 {
		total += w.HitRate * hitScore(m.HitCount)
		reasons = append(reasons, fmt.Sprintf("历史命中 %d 次", m.HitCount))
	}

	// 低置信度惩罚：拿不准的事不该挤掉确定的事。
	total -= w.LowConf * (1 - m.Confidence)

	if m.Status == StatusDisputed {
		reasons = append(reasons, "存在未裁决的分歧")
	}
	return total, reasons
}

// ResolvePreference 在给定场景下解析偏好，返回应当生效的那一条。
//
// 这是"打车"那个例子的落点：{场景:接待客户} 命中豪华型规则，
// 而不是被"默认便宜"盖过去。排序依据是 特异度 > 置信度 > 时间新近。
func (r *Retriever) ResolvePreference(ctx context.Context, subject, domain string, scene Condition) (*Memory, error) {
	now := r.now()
	all, err := r.store.List(ctx, Query{
		Subject: subject, Domain: domain, Kinds: []Kind{KindPreference},
	})
	if err != nil {
		return nil, err
	}

	matched := make([]*Memory, 0, len(all))
	for _, m := range all {
		if m.Retrievable(now) && m.Condition.Matches(scene) {
			matched = append(matched, m)
		}
	}
	if len(matched) == 0 {
		return nil, ErrNotFound
	}

	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		// 条件更具体的优先：{场景:接待客户} 盖过 {}。
		if sa, sb := a.Condition.Specificity(), b.Condition.Specificity(); sa != sb {
			return sa > sb
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		return a.UpdatedAt.After(b.UpdatedAt)
	})
	return matched[0], nil
}

// Expand 按 ID 展开一条记忆，供模型从索引行按需拉取原文。
//
// 展开会记一次命中：被展开说明这条记忆确实有用。
func (r *Retriever) Expand(ctx context.Context, id string) (*Memory, error) {
	m, err := r.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	m.RecordHit(r.now())
	if err := r.store.Put(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// RenderFull 是高关联环的原文表示。
func RenderFull(m *Memory) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s] %s", m.ID, describeSubject(m))
	if m.Kind == KindPreference && len(m.Condition) > 0 {
		fmt.Fprintf(&sb, " 条件%s", m.Condition.String())
	}
	fmt.Fprintf(&sb, ": %s", m.Current.Value)
	fmt.Fprintf(&sb, "（置信 %.2f，%s）", m.Confidence, describeSource(m.Current))
	if m.Status == StatusDisputed {
		sb.WriteString("\n注意：这条记忆存在未裁决的分歧，使用前建议向用户确认。")
	}
	if m.Inferred() {
		sb.WriteString("\n注意：这是助手自己推断的，用户没有确认过。可以拿来参考；要据此做不好撤回的事之前，先向用户核实。")
	}
	if m.ValidUntil != nil {
		fmt.Fprintf(&sb, "\n有效期至 %s。", m.ValidUntil.Local().Format("2006-01-02"))
	}
	return sb.String()
}

// RenderSummary 是中关联环的摘要表示。
func RenderSummary(m *Memory) string {
	return fmt.Sprintf("[%s] %s: %s", m.ID, describeSubject(m), truncate(m.Current.Value, 60))
}

// RenderIndex 是低关联环的索引行。
//
// 模型看到它之后可以调用展开工具拉取原文——这才是渐进式披露的形态，
// 而不是由我们替模型决定它用不上。
func RenderIndex(m *Memory) string {
	return fmt.Sprintf("[%s] %s（%s，置信%.1f）",
		m.ID, describeSubject(m), m.UpdatedAt.Local().Format("2006-01"), m.Confidence)
}

func describeSubject(m *Memory) string {
	parts := []string{m.Subject}
	if m.Domain != "" {
		parts = append(parts, m.Domain)
	}
	if m.Attribute != "" {
		parts = append(parts, m.Attribute)
	}
	return strings.Join(parts, "·")
}

func tagOverlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]bool, len(b))
	for _, t := range b {
		set[t] = true
	}
	hit := 0
	for _, t := range a {
		if set[t] {
			hit++
		}
	}
	return float64(hit) / float64(len(a))
}

// hitScore 把命中次数压到 [0,1)，避免老记忆靠次数碾压新记忆。
func hitScore(n int) float64 {
	return float64(n) / float64(n+5)
}

func tokenize(s string) map[string]bool {
	out := make(map[string]bool)
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == ',' || r == '，' ||
			r == '。' || r == '.' || r == '、' || r == '：' || r == ':'
	}) {
		if f != "" {
			out[f] = true
		}
	}
	// 中文没有空格分词，补一层双字切分，让"海鲜""过敏"这类词能被命中。
	runes := []rune(s)
	for i := 0; i+2 <= len(runes); i++ {
		seg := string(runes[i : i+2])
		if isCJK(runes[i]) && isCJK(runes[i+1]) {
			out[strings.ToLower(seg)] = true
		}
	}
	return out
}

func isCJK(r rune) bool { return r >= 0x4E00 && r <= 0x9FFF }

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
