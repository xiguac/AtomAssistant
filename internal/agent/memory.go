package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/mcpgw"
	"github.com/xiguac/atomassistant/internal/memory"
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

// ToolPlan 是内置工具：维护任务自己的计划清单。
//
// 长程任务靠它不迷路。分多天收集材料、逐个核实因素这类事，
// 步骤一多、中间一等，早先的进展就滑出了上下文窗口；
// 清单是模型自己写给自己的备忘，最新一版始终钉在上下文里。
const ToolPlan = "plan.update"

var planTool = mcpgw.ToolInfo{
	Name:    ToolPlan,
	Server:  "builtin",
	RawName: "update",
	Description: `维护这个任务的计划清单，每次传入完整的最新清单。` +
		`任务有多个步骤、要逐项收集材料、或要逐个核实多个因素时，先列清单，每推进一项就更新。` +
		`最新的清单会一直显示在上下文里；任务跨多天断续进行时，靠它记住已经拿到什么、还差什么。` +
		`参数: {"items":[{"title":"事项","status":"todo|doing|done|blocked","note":"已拿到的内容或卡住的原因"}]}`,
	InputSchema: json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object",` +
		`"properties":{"title":{"type":"string"},"status":{"type":"string","enum":["todo","doing","done","blocked"]},` +
		`"note":{"type":"string"}},"required":["title","status"]}}},"required":["items"]}`),
	// 清单只存在于任务自己的日志里，没有任何外部副作用。
	SideEffect: core.SideEffectNone,
}

// builtinTools 是不经过 MCP 的内置工具。
func builtinTools(hasMemory bool) []mcpgw.ToolInfo {
	tools := []mcpgw.ToolInfo{planTool, toolLoadTool}
	if hasMemory {
		tools = append(tools, expandMemoryTool)
	}
	return tools
}

// planInput 是 plan.update 的参数。
type planInput struct {
	Items []struct {
		Title  string `json:"title"`
		Status string `json:"status"`
		Note   string `json:"note,omitempty"`
	} `json:"items"`
}

// runPlan 校验并保存计划清单。清单本身就是这一步的输出。
func runPlan(raw json.RawMessage) (json.RawMessage, error) {
	var in planInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("agent: 解析计划清单失败: %w", err)
	}
	if len(in.Items) == 0 {
		return nil, fmt.Errorf("agent: 计划清单不能为空")
	}
	for _, it := range in.Items {
		switch it.Status {
		case "todo", "doing", "done", "blocked":
		default:
			return nil, fmt.Errorf("agent: 事项 %q 的状态 %q 不合法", it.Title, it.Status)
		}
		if strings.TrimSpace(it.Title) == "" {
			return nil, fmt.Errorf("agent: 计划清单里有没写标题的事项")
		}
	}
	return json.Marshal(in)
}

// latestPlan 返回任务最新一版计划清单，没有则返回空。
func latestPlan(history []*core.Step) string {
	for i := len(history) - 1; i >= 0; i-- {
		s := history[i]
		if s.ToolName != ToolPlan || s.Status != core.StepDone {
			continue
		}
		var in planInput
		if err := json.Unmarshal(s.Output, &in); err != nil {
			return ""
		}
		var sb strings.Builder
		for _, it := range in.Items {
			fmt.Fprintf(&sb, "- [%s] %s", it.Status, it.Title)
			if it.Note != "" {
				fmt.Fprintf(&sb, "：%s", it.Note)
			}
			sb.WriteString("\n")
		}
		return sb.String()
	}
	return ""
}

// memorySegments 把装配结果转成上下文分段。
//
// 信任等级用 TrustConnector：记忆是经过写入管线沉淀的己方知识，
// 不可信来源根本进不来（只能挂候选）。但它仍然低于 TrustUser——
// 记忆是事实材料，不是指令，助手不能因为"记忆里这么写"就去执行操作。
func memorySegments(asm memory.Assembly) []llm.Segment {
	var segs []llm.Segment

	// 流程单独成段：它不是"关于用户的事实"，而是"这类事该怎么办"。
	asm, playbooks := splitPlaybooks(asm)
	if len(playbooks) > 0 {
		var sb strings.Builder
		sb.WriteString("适用的标准流程（SOP）。任务属于其中某一类时按它的步骤做，跳过哪一步要说明原因。\n" +
			"标着「参考做法」的是助手从以往的任务里自己总结的，用户没有确认过：可以借鉴，不必照做，与用户这次的要求冲突时听用户的。\n")
		// 整段的信任等级取其中最低的：混进一条低信任的流程，整段都不能当指令。
		trust := core.TrustSystem
		for _, it := range playbooks {
			label := it.Memory.Domain
			if it.Memory.Inferred() {
				label += "·参考做法"
			}
			fmt.Fprintf(&sb, "【%s】\n%s\n", label, it.Memory.Current.Value)
			if t := it.Memory.Current.Source.Trust; t < trust {
				trust = t
			}
		}
		segs = append(segs, llm.Segment{
			Role: llm.RoleUser, Content: sb.String(),
			Trust: trust, Source: "标准流程",
		})
	}

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

// splitPlaybooks 把流程类记忆从高、中关联环里挑出来。
func splitPlaybooks(asm memory.Assembly) (memory.Assembly, []memory.Item) {
	var playbooks []memory.Item
	keep := func(items []memory.Item) []memory.Item {
		out := make([]memory.Item, 0, len(items))
		for _, it := range items {
			if it.Memory.Kind == memory.KindPlaybook {
				playbooks = append(playbooks, it)
				continue
			}
			out = append(out, it)
		}
		return out
	}
	asm.High = keep(asm.High)
	asm.Medium = keep(asm.Medium)
	return asm, playbooks
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
