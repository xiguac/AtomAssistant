package skill

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 不是从外面装进来的 Skill，Source 以这两个前缀开头。
const (
	// SourceBuiltin 是随程序发布的。
	SourceBuiltin = "内置"
	// SourceLearned 是助手从办过的任务里自己总结的：只当参考做法，与用户的要求冲突时听用户的。
	SourceLearned = "助手总结"
)

// Learned 报告这个 Skill 是否由助手自己总结。只有这一类允许助手自己改写。
func (s *Skill) Learned() bool { return strings.HasPrefix(s.Source, SourceLearned) }

// Compose 把字段齐全的一份清单补上原文并校验，供不经过安装、由程序自己写出的 Skill 使用。
func Compose(s *Skill) (*Skill, error) {
	s.Format = FormatManifest
	raw, err := yaml.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("skill: 生成清单原文失败: %w", err)
	}
	s.Raw = string(raw)
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// builtinSkills 是出厂自带的流程。
//
// 它们只规定"该考虑哪些因素、按什么顺序"，不含任何具体数据——票价、天气、人流都必须在执行时现查。
// 以前作为"流程"类记忆写在记忆里，每个相关任务都整段带着；做成 Skill 之后平时只列一行，用到才读全文。
var builtinSkills = []Skill{
	{
		ID: "builtin.travel-planning", Name: "旅游出行规划",
		Goal: "用户问去哪玩、什么时候去、怎么去，或让你规划行程。目标是给出可执行的建议，而不是评价某个地方好不好玩。",
		Steps: []string{
			"先确认约束：从哪个城市出发（记忆里有常住地就用，没有才问）、哪几天有空、几个人去、大致预算、不能接受什么。",
			"查日历因素：这段时间是否法定节假日或调休、是否寒暑假。节假日意味着票价上涨、景点拥挤。",
			"查交通：出发地到候选目的地的机票与火车票，比较价格、耗时、出发到达时刻；结合用户平时的出行习惯选方式。",
			"查天气：候选日期目的地的天气，雨雪、高温、台风会直接否掉户外行程。",
			"查人流：候选景点在这些日期的拥挤程度，是否需要预约、是否限流。",
			"综合研判：把以上因素放在一起权衡，不要只看其中一项。错峰一两天能明显省钱或避开人流就明确提出来。",
			"给出结论：什么时候去、去哪些地方、怎么去、大概花多少，以及这样选的理由；再给一个备选方案和它的取舍。",
			"哪一项没查到数据，在结论里单独列出，并说明它可能怎样改变建议。",
		},
	},
	{
		ID: "builtin.dining", Name: "宴请订餐",
		Goal: "请人吃饭、订餐厅、安排聚餐。",
		Steps: []string{
			"确认谁参加、几个人、什么场合（商务接待、朋友聚会、家庭聚餐）、什么时间、人均预算。",
			"逐个核对参加者的忌口与过敏——这是安全问题，记忆里有记录的必须遵守，拿不准的要问。",
			"结合参加者的口味偏好和各自的出发位置选菜系与地段，交通要对多数人方便。",
			"查候选餐厅在那个时间是否有位、是否需要订包间、是否有最低消费。",
			"给出一个首选和一个备选，说明为什么选它；需要预订的，订之前先让用户确认。",
		},
	},
	{
		ID: "builtin.shopping", Name: "购物决策",
		Goal: "用户让你帮忙挑东西、比价、决定买不买。",
		Steps: []string{
			"确认用途、预算上限、必须有和不能有的特性；是自用还是送人（送人要考虑对方的喜好与禁忌）。",
			"列出两到四个候选，逐个查当前价格、近期价格走势、是否临近大促。",
			"查口碑与常见问题，区分真实缺陷和个别差评。",
			"综合价格、质量、到货时间、售后给出建议：买哪个、现在买还是等、在哪买。",
			"涉及付款的操作一律先让用户确认。",
		},
	},
}

// SeedBuiltin 写入出厂自带的流程，可重复调用。
//
// 还是内置版本的，内容变了就换成新的；被用户改过（同 ID 装了别的）的不动。
func SeedBuiltin(ctx context.Context, store Store, now time.Time) error {
	for _, b := range builtinSkills {
		b.Author, b.Source, b.InstalledAt = "原子助手", SourceBuiltin, now
		want, err := Compose(&b)
		if err != nil {
			return err
		}
		existing, err := store.GetSkill(ctx, b.ID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		case existing.Source != SourceBuiltin || existing.Raw == want.Raw:
			continue
		}
		if err := store.PutSkill(ctx, want); err != nil {
			return err
		}
	}
	return nil
}
