package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/style"
)

// VoiceProvider 给出当前学到的用户表达风格，由 *style.Learner 提供。
type VoiceProvider interface {
	Voice(ctx context.Context) (style.Voice, error)
}

// VoiceLearner 是收尾时学习表达风格所需的能力，由 *style.Learner 提供。
type VoiceLearner interface {
	Observe(ctx context.Context, scene, ref string, texts []string) (int, error)
	Refresh(ctx context.Context) (style.Cost, error)
	// LearnDay 在一天的对话写成摘要之后，按那天用户说的话修订对助手说话的风格。
	LearnDay(ctx context.Context, ref string, texts []string) (style.Cost, error)
}

// voiceSource 是风格材料在上下文里的来源名。系统提示里的用法说明靠这个名字指向它。
const voiceSource = "用户的表达风格"

// voiceSegment 把学到的风格渲染成上下文里的一段材料。
func voiceSegment(v style.Voice) llm.Segment {
	var sb strings.Builder
	for _, sv := range v.Scenes {
		fmt.Fprintf(&sb, "【%s】\n", style.SceneName(sv.Scene))
		if sv.Description != "" {
			fmt.Fprintf(&sb, "概述：%s\n", sv.Description)
		}
		if len(sv.Samples) > 0 {
			sb.WriteString("他的原话：\n")
			for _, s := range sv.Samples {
				// 样本可能有多行，缩成一行，免得看起来像是上下文里另起的一段。
				fmt.Fprintf(&sb, "· %s\n", truncate(strings.Join(strings.Fields(s), " "), 240))
			}
		}
	}
	return llm.Segment{
		Role: llm.RoleUser, Content: sb.String(),
		// 是用户自己的话，但在这里是被模仿的材料而不是指令：低于 user，进围栏。
		Trust: core.TrustConnector, Source: voiceSource,
	}
}

// voiceTexts 收集一次任务里可以用来学习表达风格的话。
//
// 来源与 userStatements 相同——都必须是用户在可信渠道上亲口说的——
// 只是不要标题：标题常常是目标的截短，学进去只会把同一句话算两遍。
func voiceTexts(t *core.Task, history []*core.Step) []string {
	var out []string
	if t.Origin >= core.TrustUser {
		goal := strings.TrimSpace(t.Goal)
		if goal == "" {
			goal = strings.TrimSpace(t.Title)
		}
		out = append(out, goal)
	}
	for _, s := range history {
		if s.OutputTrust < core.TrustUser {
			continue
		}
		if s.Status == core.StepSkipped && s.Err != "" {
			out = append(out, s.Err)
			continue
		}
		if text := messageText(s.Output); text != "" {
			out = append(out, text)
		}
	}
	return out
}
