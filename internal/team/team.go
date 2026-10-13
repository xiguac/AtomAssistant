// Package team 是多模型协作里"谁来做"的那一半：协作模型的花名册、每份活的记录、
// 按用户反馈算出来的成绩，以及下一份活该派给谁。
//
// 不同的模型擅长的事不一样，而且这件事没法事先知道——只能用出来。所以这里不让任何人
// （包括主 AI）凭印象指定模型：主 AI 只说这份活属于哪一类，派给谁由成绩决定。
//
// 成绩只来自用户的反馈，分量和记忆是同一个道理：
//
//   - 用户在后台或聊天里明确表态的，算一整票；
//   - 助手从用户在任务里说的话推断出来的（"太啰嗦了，重写"），算半票，
//     而且必须能在用户原话里逐字找到依据；之后用户明确表态了，以用户的为准。
//
// 没有人评价过的活不计分——"没人抱怨"不等于"做得好"。
package team

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Category 是一份活的类别。成绩按类别分开算。
type Category string

const (
	CatWriting   Category = "writing"
	CatCode      Category = "code"
	CatAnalysis  Category = "analysis"
	CatTranslate Category = "translate"
	CatSummary   Category = "summary"
	CatCreative  Category = "creative"
	CatOther     Category = "other"
)

// CategoryInfo 是一个类别给人看的名字和说明。
type CategoryInfo struct {
	ID   Category
	Name string
	Hint string
}

// Categories 是全部类别。类别是固定的：让主 AI 自己起类别名，成绩就会散在一堆近义词里攒不起来。
var Categories = []CategoryInfo{
	{CatWriting, "写作", "写文章、邮件、文案、讲稿，润色改写"},
	{CatCode, "编程", "写代码、查错、解释代码、写脚本和公式"},
	{CatAnalysis, "分析推理", "比较方案、推演利弊、从数据里找结论、算账"},
	{CatTranslate, "翻译", "不同语言之间的翻译和本地化"},
	{CatSummary, "提炼总结", "把长材料归纳成要点、会议纪要、摘要"},
	{CatCreative, "创意", "起名字、出点子、策划、设计方案"},
	{CatOther, "其他", "不属于上面任何一类的"},
}

// CategoryName 返回类别的中文名。
func CategoryName(c Category) string {
	for _, info := range Categories {
		if info.ID == c {
			return info.Name
		}
	}
	return string(c)
}

// ValidCategory 报告一个类别是否存在。
func ValidCategory(c Category) bool {
	for _, info := range Categories {
		if info.ID == c {
			return true
		}
	}
	return false
}

// Worker 是一个协作模型。
type Worker struct {
	// Name 是用户给它起的名字，成绩记在这个名字下。
	Name string
	// Model 是它用的模型，只用来展示。
	Model string
	// GoodAt 是用户指定它只接哪些类别。为空表示什么都接。
	GoodAt []Category
	Note   string
}

// takes 报告这个协作模型接不接某一类活。
func (w Worker) takes(c Category) bool {
	if len(w.GoodAt) == 0 {
		return true
	}
	for _, g := range w.GoodAt {
		if g == c {
			return true
		}
	}
	return false
}

// 评价的来源。
const (
	RatedByUser     = "user"
	RatedByInferred = "inferred"
)

// 一次评价的分量。
const (
	// WeightUser 是用户明确表态的分量。
	WeightUser = 1.0
	// WeightInferred 是从用户的话里推断出来的分量。
	WeightInferred = 0.5
)

// Work 是派出去的一份活。
type Work struct {
	ID      string
	TaskID  string
	StepSeq int
	// Index 是它在那一次派活里排第几份。
	Index    int
	Worker   string
	Category Category
	Title    string
	// Rating 为正表示做得好，为负表示不行，绝对值是分量；0 表示还没有人评价。
	Rating    float64
	RatedBy   string
	CreatedAt time.Time
}

// WorkID 给出一份活的编号。同一步重跑时编号不变，所以不会重复计数。
func WorkID(taskID string, stepSeq, index int) string {
	return fmt.Sprintf("%s:%d:%d", taskID, stepSeq, index)
}

// Tally 是一个协作模型在一个类别上的累计。
type Tally struct {
	Worker   string
	Category Category
	// Runs 是做过多少份，Good 与 Bad 是好评和差评的分量之和。
	Runs      int
	Good, Bad float64
}

// Score 是按反馈算出的成绩，0 到 1。
//
// 好评和差评各垫一票再算比例：一次好评不该直接把成绩推到满分，一次差评也不该归零。
// 没有任何评价时是 0.5。
func (t Tally) Score() float64 {
	return (t.Good + 1) / (t.Good + t.Bad + 2)
}

// Store 保存派出去的活和它们的评价。
type Store interface {
	// PutWork 记下一份活。同一个编号再记一次不改变已有的评价。
	PutWork(ctx context.Context, w *Work) error
	// WorkOfTask 按派出的先后列出一个任务里的全部活。
	WorkOfTask(ctx context.Context, taskID string) ([]Work, error)
	// LatestWork 返回若干个任务里最近派出的那个任务的全部活。
	LatestWork(ctx context.Context, taskIDs []string) ([]Work, error)
	// Rate 给一份活打分。by 为 RatedByInferred 时不覆盖用户已经给出的评价。
	Rate(ctx context.Context, id string, rating float64, by string) error
	// Tally 返回全部累计。
	Tally(ctx context.Context) ([]Tally, error)
}

// Board 是成绩榜：知道有哪些协作模型，并决定下一份活派给谁。
type Board struct {
	store   Store
	workers []Worker
}

// NewBoard 构造成绩榜。workers 的顺序就是配置里的顺序，成绩相同时靠前的优先。
func NewBoard(store Store, workers []Worker) *Board {
	return &Board{store: store, workers: workers}
}

// Workers 返回花名册。
func (b *Board) Workers() []Worker { return b.workers }

// Standing 是成绩榜上的一行。
type Standing struct {
	Worker Worker
	Tally  Tally
	// Next 为真表示这一类的下一份活会派给它。
	Next bool
}

// Standings 返回某个类别里各协作模型的成绩，下一份活会派给谁排在最前面。
func (b *Board) Standings(ctx context.Context, c Category) ([]Standing, error) {
	all, err := b.store.Tally(ctx)
	if err != nil {
		return nil, err
	}
	return b.rank(c, all), nil
}

// rank 排出某个类别里的先后。
func (b *Board) rank(c Category, all []Tally) []Standing {
	var out []Standing
	for _, w := range b.workers {
		if !w.takes(c) {
			continue
		}
		out = append(out, Standing{Worker: w, Tally: find(all, w.Name, c)})
	}
	// 用户给每个协作模型都划了范围、而这一类谁都不接时，不能让活没人做：全员候选。
	if len(out) == 0 {
		for _, w := range b.workers {
			out = append(out, Standing{Worker: w, Tally: find(all, w.Name, c)})
		}
	}
	// 先试后比：这一类里还没做过的先各做一份，否则先拿到好评的会一直占着位置，
	// 别的模型永远没有机会证明自己。都做过之后按成绩，成绩一样的让做得少的来。
	sort.SliceStable(out, func(i, j int) bool {
		a, z := out[i].Tally, out[j].Tally
		if (a.Runs == 0) != (z.Runs == 0) {
			return a.Runs == 0
		}
		if a.Score() != z.Score() {
			return a.Score() > z.Score()
		}
		return a.Runs < z.Runs
	})
	if len(out) > 0 {
		out[0].Next = true
	}
	return out
}

func find(all []Tally, worker string, c Category) Tally {
	for _, t := range all {
		if t.Worker == worker && t.Category == c {
			return t
		}
	}
	return Tally{Worker: worker, Category: c}
}

// Pick 决定某个类别的下一份活派给谁。
func (b *Board) Pick(ctx context.Context, c Category) (Worker, error) {
	if len(b.workers) == 0 {
		return Worker{}, fmt.Errorf("team: 没有配置协作模型")
	}
	if !ValidCategory(c) {
		return Worker{}, fmt.Errorf("team: 没有 %q 这个类别", c)
	}
	rows, err := b.Standings(ctx, c)
	if err != nil {
		return Worker{}, err
	}
	return rows[0].Worker, nil
}

// PickExcept 决定某个类别的下一份活派给谁，但不派给 exclude 里的那些。
//
// 一件事交给某个模型办了几轮仍不合格、要换人时用：exclude 是已经试过的。
// 除了它们再没有别人时返回错误。
func (b *Board) PickExcept(ctx context.Context, c Category, exclude []string) (Worker, error) {
	if len(b.workers) == 0 {
		return Worker{}, fmt.Errorf("team: 没有配置协作模型")
	}
	if !ValidCategory(c) {
		return Worker{}, fmt.Errorf("team: 没有 %q 这个类别", c)
	}
	rows, err := b.Standings(ctx, c)
	if err != nil {
		return Worker{}, err
	}
	tried := make(map[string]bool, len(exclude))
	for _, name := range exclude {
		tried[name] = true
	}
	for _, row := range rows {
		if !tried[row.Worker.Name] {
			return row.Worker, nil
		}
	}
	// 这一类里的都试过了：别的类别的模型也可以顶上，总比没人接强。
	for _, w := range b.workers {
		if !tried[w.Name] {
			return w, nil
		}
	}
	return Worker{}, fmt.Errorf("team: 没有别的协作模型可换")
}

// Record 记下一份派出去的活。
func (b *Board) Record(ctx context.Context, w *Work) error { return b.store.PutWork(ctx, w) }

// Verdict 把"好 / 不好"和来源换算成带分量的评分。
func Verdict(good bool, by string) float64 {
	weight := WeightUser
	if by == RatedByInferred {
		weight = WeightInferred
	}
	if !good {
		return -weight
	}
	return weight
}
