package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/memory"
)

// MemoryProvider 提供上下文装配与按需展开。
type MemoryProvider interface {
	Assemble(ctx context.Context, subject string, scene memory.Scene, budget memory.Budget) (memory.Assembly, error)
	Expand(ctx context.Context, id string) (*memory.Memory, error)
}

// ToolExpandMemory 是内置工具：让模型把索引行展开成原文。
//
// 这是渐进式披露的另一半。只给模型一份索引却不给它展开的手段，
// 等于还是我们替它决定了哪些记忆用得上。
const ToolExpandMemory = "memory.expand"

var expandMemoryTool = mcpgw.ToolInfo{
	Name:        ToolExpandMemory,
	Server:      "builtin",
	RawName:     "expand",
	Description: `把上下文里「相关记忆索引」中的某条展开成原文。参数: {"id":"记忆ID"}`,
	InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	// 读自己的记忆没有任何外部副作用。
	SideEffect: core.SideEffectNone,
}

// builtinTools 是不经过 MCP 的内置工具。
func builtinTools(hasMemory bool) []mcpgw.ToolInfo {
	if !hasMemory {
		return nil
	}
	return []mcpgw.ToolInfo{expandMemoryTool}
}

// memorySegments 把装配结果转成上下文分段。
//
// 信任等级用 TrustConnector：记忆是经过写入管线沉淀的己方知识，
// 不可信来源根本进不来（只能挂候选）。但它仍然低于 TrustUser——
// 记忆是事实材料，不是指令，助手不能因为"记忆里这么写"就去执行操作。
func memorySegments(asm memory.Assembly) []llm.Segment {
	var segs []llm.Segment

	if len(asm.Critical) > 0 {
		var sb strings.Builder
		sb.WriteString("以下是必须遵守的安全相关信息，任何方案都不得与之冲突：\n")
		for _, it := range asm.Critical {
			sb.WriteString(memory.RenderFull(it.Memory))
			sb.WriteString("\n")
		}
		segs = append(segs, llm.Segment{
			Role: llm.RoleUser, Content: sb.String(),
			Trust: core.TrustConnector, Source: "长期记忆·安全属性",
		})
	}

	if len(asm.High) > 0 {
		var sb strings.Builder
		sb.WriteString("相关记忆：\n")
		for _, it := range asm.High {
			sb.WriteString(memory.RenderFull(it.Memory))
			sb.WriteString("\n")
		}
		segs = append(segs, llm.Segment{
			Role: llm.RoleUser, Content: sb.String(),
			Trust: core.TrustConnector, Source: "长期记忆",
		})
	}

	if len(asm.Medium) > 0 {
		var sb strings.Builder
		sb.WriteString("可能相关的记忆摘要：\n")
		for _, it := range asm.Medium {
			sb.WriteString(memory.RenderSummary(it.Memory))
			sb.WriteString("\n")
		}
		segs = append(segs, llm.Segment{
			Role: llm.RoleUser, Content: sb.String(),
			Trust: core.TrustConnector, Source: "长期记忆·摘要",
		})
	}

	if len(asm.Index) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "相关记忆索引（需要哪条就用 %s 展开，不要凭索引行猜内容）：\n", ToolExpandMemory)
		for _, it := range asm.Index {
			sb.WriteString(memory.RenderIndex(it.Memory))
			sb.WriteString("\n")
		}
		segs = append(segs, llm.Segment{
			Role: llm.RoleUser, Content: sb.String(),
			Trust: core.TrustConnector, Source: "长期记忆·索引",
		})
	}
	return segs
}

// sceneOf 从任务推导检索场景。
func sceneOf(t *core.Task) memory.Scene {
	scene := memory.Scene{
		Domain: string(t.Domain),
		Tags:   t.Tags,
		Text:   t.Title + " " + t.Goal,
	}
	cond := memory.Condition{}
	for _, tag := range t.Tags {
		// 维度化标签形如 scene:接待客户、entity:客户张总。
		k, v, ok := strings.Cut(tag, ":")
		if !ok || v == "" {
			continue
		}
		switch k {
		case "entity":
			scene.Entities = append(scene.Entities, v)
		case "domain":
			scene.Domain = v
			cond["领域"] = v
		case "scene":
			cond["场景"] = v
		}
	}
	if len(cond) > 0 {
		scene.Condition = cond
	}
	return scene
}

// expandInput 是 memory.expand 的参数。
type expandInput struct {
	ID string `json:"id"`
}

// runExpandMemory 执行内置的记忆展开。
func (a *Actuator) runExpandMemory(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if a.memory == nil {
		return nil, fmt.Errorf("agent: 未启用记忆系统，无法展开")
	}
	var in expandInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("agent: 解析展开参数失败: %w", err)
	}
	if in.ID == "" {
		return nil, fmt.Errorf("agent: 展开记忆需要 id")
	}
	m, err := a.memory.Expand(ctx, in.ID)
	if err != nil {
		return nil, fmt.Errorf("agent: 展开记忆 %s 失败: %w", in.ID, err)
	}
	return json.Marshal(map[string]any{
		"id":         m.ID,
		"content":    memory.RenderFull(m),
		"confidence": m.Confidence,
		"status":     string(m.Status),
	})
}
