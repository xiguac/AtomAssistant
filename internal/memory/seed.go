package memory

import (
	"context"
	"errors"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
)

// builtinPlaybook 是随程序发布的一条标准流程（SOP）。
type builtinPlaybook struct {
	Domain string
	Tags   []string
	Value  string
}

// builtinPlaybooks 是出厂自带的流程。
//
// 它们只规定"该考虑哪些因素、按什么顺序"，不含任何具体数据——
// 票价、天气、人流都必须在执行时现查。用户确认过的同名流程会盖过这里的版本。
var builtinPlaybooks = []builtinPlaybook{
	{
		Domain: "旅游出行规划",
		Tags:   []string{"sop", "domain:旅游", "domain:出行"},
		Value: `适用：用户问去哪玩、什么时候去、怎么去，或让你规划行程。目标是给出可执行的建议，而不是评价某个地方好不好玩。
1. 先确认约束：从哪个城市出发（记忆里有常住地就用，没有才问）、哪几天有空、几个人去、大致预算、不能接受什么。
2. 查日历因素：这段时间是否法定节假日或调休、是否寒暑假。节假日意味着票价上涨、景点拥挤。
3. 查交通：出发地到候选目的地的机票与火车票，比较价格、耗时、出发到达时刻；结合用户平时的出行习惯选方式。
4. 查天气：候选日期目的地的天气，雨雪、高温、台风会直接否掉户外行程。
5. 查人流：候选景点在这些日期的拥挤程度，是否需要预约、是否限流。
6. 综合研判：把以上因素放在一起权衡，不要只看其中一项。错峰一两天能明显省钱或避开人流就明确提出来。
7. 给出结论：什么时候去、去哪些地方、怎么去、大概花多少，以及这样选的理由；再给一个备选方案和它的取舍。
8. 哪一项没查到数据，在结论里单独列出，并说明它可能怎样改变建议。`,
	},
	{
		Domain: "宴请订餐",
		Tags:   []string{"sop", "domain:餐饮", "domain:接待"},
		Value: `适用：请人吃饭、订餐厅、安排聚餐。
1. 确认谁参加、几个人、什么场合（商务接待、朋友聚会、家庭聚餐）、什么时间、人均预算。
2. 逐个核对参加者的忌口与过敏——这是安全问题，记忆里有记录的必须遵守，拿不准的要问。
3. 结合参加者的口味偏好和各自的出发位置选菜系与地段，交通要对多数人方便。
4. 查候选餐厅在那个时间是否有位、是否需要订包间、是否有最低消费。
5. 给出一个首选和一个备选，说明为什么选它；需要预订的，订之前先让用户确认。`,
	},
	{
		Domain: "购物决策",
		Tags:   []string{"sop", "domain:购物"},
		Value: `适用：用户让你帮忙挑东西、比价、决定买不买。
1. 确认用途、预算上限、必须有和不能有的特性；是自用还是送人（送人要考虑对方的喜好与禁忌）。
2. 列出两到四个候选，逐个查当前价格、近期价格走势、是否临近大促。
3. 查口碑与常见问题，区分真实缺陷和个别差评。
4. 综合价格、质量、到货时间、售后给出建议：买哪个、现在买还是等、在哪买。
5. 涉及付款的操作一律先让用户确认。`,
	},
}

// Seed 写入出厂自带的流程，可重复调用。
//
// 已存在且仍是内置版本的，内容变了就升一个版本；
// 被用户改过或确认过的不动——用户的版本优先于出厂的。
func Seed(ctx context.Context, s Store, now time.Time) error {
	src := Source{Kind: SourceBuiltin, Ref: "seed", Trust: core.TrustSystem}
	for _, pb := range builtinPlaybooks {
		cand := Candidate{Kind: KindPlaybook, Subject: "user", Domain: pb.Domain}
		existing, err := s.FindByKey(ctx, cand.Key())
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if existing != nil {
			if existing.Current.Source.Kind != SourceBuiltin || existing.Current.Value == pb.Value {
				continue
			}
			existing.Supersede(Revision{
				ID: core.NewID(), Value: pb.Value, Source: src,
				Confidence: initialConfidence(src), CreatedAt: now,
			}, OverrideNewEvidence, now)
			if err := s.Put(ctx, existing); err != nil {
				return err
			}
			continue
		}
		m := New(KindPlaybook, "user", pb.Domain, pb.Value, src, now)
		m.Temporality = TemporalPermanent
		m.Tags = pb.Tags
		if err := s.Put(ctx, m); err != nil {
			return err
		}
	}
	return nil
}
