package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xiguac/atomhelper/internal/core"
)

// OverrideMerged 表示这个版本是几条相近的推断合并出来的。
const OverrideMerged = "merged"

// ErrCannotMerge 表示这几条记忆不能合并。
var ErrCannotMerge = errors.New("memory: 这几条记忆不能合并")

// Mergeable 返回可以参与合并的记忆：助手自己推断出来、还没被用户确认过的，按写入的先后。
//
// 用户亲口说的、确认过的不在其中——那是用户的原话，不由助手改写。
// 安全相关的也不在其中：过敏、禁忌这类一个字都不能被"归纳"掉。
func (p *Pipeline) Mergeable(ctx context.Context) ([]*Memory, error) {
	all, err := p.store.List(ctx, Query{})
	if err != nil {
		return nil, err
	}
	out := make([]*Memory, 0, len(all))
	for _, m := range all {
		if mergeable(m) {
			out = append(out, m)
		}
	}
	return out, nil
}

func mergeable(m *Memory) bool {
	return m.Status == StatusActive && m.Inferred() && !m.SafetyCritical
}

// MergeInferred 把几条相近的推断并成一条，value 是合并后的表述。返回留下来的那一条的 ID。
//
// 推断出来的记忆要用户逐条确认，同一件事被几个任务各记了一遍的话，用户就得确认好几次。
// 合并只减少条数，不改变性质：留下来的那一条仍然是"推断"，仍然等用户确认；
// 被并掉的几条归档而不删除，合并之前各自是怎么说的都查得到。
//
// 合并什么是模型提的，这里把住几条底线：只能并推断出来的；类型、主体、适用条件必须相同；
// 安全相关的不并。
func (p *Pipeline) MergeInferred(ctx context.Context, ids []string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(ids) < 2 || value == "" {
		return "", fmt.Errorf("%w: 至少要两条，并且要有合并后的表述", ErrCannotMerge)
	}
	seen := make(map[string]bool, len(ids))
	members := make([]*Memory, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			return "", fmt.Errorf("%w: 同一条出现了两次", ErrCannotMerge)
		}
		seen[id] = true
		m, err := p.store.Get(ctx, id)
		if err != nil {
			return "", err
		}
		if !mergeable(m) {
			return "", fmt.Errorf("%w: %s 不是待确认的推断", ErrCannotMerge, id)
		}
		if first := members; len(first) > 0 {
			f := first[0]
			if m.Kind != f.Kind || m.Subject != f.Subject || m.Condition.String() != f.Condition.String() {
				return "", fmt.Errorf("%w: 类型、主体或适用条件不一样", ErrCannotMerge)
			}
		}
		members = append(members, m)
	}

	// 留下分量最重的那一条；证据并到一起，信任等级取最低的——里面只要有一条来自外部内容，合并后的也算。
	keep := members[0]
	trust := keep.Current.Source.Trust
	var evidence []string
	have := map[string]bool{}
	for _, m := range members {
		if m.Confidence > keep.Confidence {
			keep = m
		}
		if m.Current.Source.Trust < trust {
			trust = m.Current.Source.Trust
		}
		for _, e := range m.Current.Evidence {
			if !have[e] {
				have[e] = true
				evidence = append(evidence, e)
			}
		}
	}
	now := p.now()
	keep.Supersede(Revision{
		ID:         core.NewID(),
		Value:      value,
		Source:     Source{Kind: SourceTask, Ref: keep.Current.Source.Ref, Trust: trust},
		Evidence:   evidence,
		Confidence: keep.Confidence,
	}, OverrideMerged, now)
	if err := p.store.Put(ctx, keep); err != nil {
		return "", err
	}
	for _, m := range members {
		if m.ID == keep.ID {
			continue
		}
		m.Status, m.UpdatedAt = StatusArchived, now
		if err := p.store.Put(ctx, m); err != nil {
			return "", err
		}
	}
	return keep.ID, nil
}
