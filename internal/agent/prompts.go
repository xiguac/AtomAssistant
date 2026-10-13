package agent

import (
	"context"
	"fmt"
	"strings"
)

// 系统提示里用户可以在管理后台改写的两段，各自在设置里的名字。没有存过的那一段用内置的。
const (
	PromptKeyBase    = "prompt.base"
	PromptKeyPlanner = "prompt.planner"
)

// DefaultBaseRules 返回内置的基础约束，DefaultPlannerPrompt 返回内置的规划说明。
// 管理后台用它们展示默认内容、恢复默认。
func DefaultBaseRules() string     { return baseRules }
func DefaultPlannerPrompt() string { return plannerSystemPrompt }

// PromptSource 给出用户改写过的系统提示。某一段返回空表示用内置的。
type PromptSource interface {
	Prompts(ctx context.Context) (base, planner string, err error)
}

// SettingReader 读出一项设置，由 *pg.Store 提供。
type SettingReader interface {
	Setting(ctx context.Context, key string) (string, error)
}

// SettingPrompts 从设置里读用户改写过的系统提示。
type SettingPrompts struct {
	Store SettingReader
}

func (s SettingPrompts) Prompts(ctx context.Context) (base, planner string, err error) {
	if base, err = s.Store.Setting(ctx, PromptKeyBase); err != nil {
		return "", "", err
	}
	if planner, err = s.Store.Setting(ctx, PromptKeyPlanner); err != nil {
		return "", "", err
	}
	return base, planner, nil
}

// WithPrompts 接入用户改写过的系统提示。每次决策现读：在后台改完不必重启就生效。
func (p *Planner) WithPrompts(s PromptSource) *Planner {
	p.prompts = s
	return p
}

// corePrompt 返回系统提示最前面的两段：基础约束与规划说明。
func (p *Planner) corePrompt(ctx context.Context) (string, error) {
	base, planner := baseRules, plannerSystemPrompt
	if p.prompts != nil {
		b, pl, err := p.prompts.Prompts(ctx)
		if err != nil {
			return "", fmt.Errorf("agent: 读取系统提示失败: %w", err)
		}
		if strings.TrimSpace(b) != "" {
			base = b
		}
		if strings.TrimSpace(pl) != "" {
			planner = pl
		}
	}
	return base + "\n\n" + planner, nil
}
