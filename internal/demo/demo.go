// Package demo 提供 P0 阶段的假 Planner 与假 Actuator。
//
// 它们的存在只有一个目的：在接入真实 LLM 与 MCP 之前，
// 让内核的持久化执行、崩溃恢复、租约、审批这些机制能被端到端跑通和观察。
// 接入真实实现后这个包就该删掉。
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/kernel"
)

// Planner 是一个固定流程的假规划器：
//
//	第 1 步  一次"推理"
//	第 2 步  一次只读查询
//	第 3 步  等待 10 秒（用来观察 waiting → ready 的唤醒路径）
//	第 4 步  一次不可逆操作（用来观察审批与崩溃恢复路径）
//	第 5 步  完成
type Planner struct{}

var _ kernel.Planner = (*Planner)(nil)

func (Planner) Decide(_ context.Context, t *core.Task, history []*core.Step) (kernel.Decision, error) {
	// 等待唤醒后会再次进入决策，此时第 3 步已经完成，不能重复等待。
	done := len(history)

	switch done {
	case 0:
		return kernel.Decision{
			Kind:       kernel.DecideStep,
			Type:       core.StepLLM,
			SideEffect: core.SideEffectNone,
			Input:      json.RawMessage(fmt.Sprintf(`{"goal":%q}`, t.Goal)),
		}, nil
	case 1:
		return kernel.Decision{
			Kind:       kernel.DecideStep,
			Type:       core.StepTool,
			ToolName:   "demo.query",
			SideEffect: core.SideEffectReadOnly,
			Input:      json.RawMessage(`{"what":"calendar"}`),
		}, nil
	case 2:
		if t.Wake == nil {
			at := time.Now().UTC().Add(10 * time.Second)
			return kernel.Decision{
				Kind:   kernel.DecideWait,
				Wake:   &core.WakeCondition{Kind: core.WakeTime, At: &at},
				Reason: "演示：等待 10 秒",
			}, nil
		}
		// 已经等过一轮了，写一条步骤记录把它标记掉，避免无限等待。
		return kernel.Decision{
			Kind:       kernel.DecideStep,
			Type:       core.StepWait,
			SideEffect: core.SideEffectNone,
			Input:      json.RawMessage(`{"waited":true}`),
		}, nil
	case 3:
		return kernel.Decision{
			Kind:       kernel.DecideStep,
			Type:       core.StepTool,
			ToolName:   "demo.book",
			SideEffect: core.SideEffectIrreversible,
			Input:      json.RawMessage(`{"amount_cents":1200}`),
		}, nil
	default:
		return kernel.Decision{Kind: kernel.DecideFinish, Reason: "演示流程结束"}, nil
	}
}

// Actuator 是一个假执行器：不产生任何真实副作用，只回显幂等键。
type Actuator struct{}

var _ kernel.Actuator = (*Actuator)(nil)

func (Actuator) Execute(_ context.Context, _ *core.Task, s *core.Step) (kernel.Result, error) {
	out, err := json.Marshal(map[string]any{
		"step":            s.Seq,
		"tool":            s.ToolName,
		"idempotency_key": s.IdempotencyKey,
		"note":            "假执行，未产生真实副作用",
	})
	if err != nil {
		return kernel.Result{}, fmt.Errorf("demo: 序列化输出失败: %w", err)
	}
	// 工具输出默认不可信，这里如实标注。
	return kernel.Result{
		Output:    out,
		Trust:     core.TrustToolOutput,
		TokensIn:  10,
		TokensOut: 20,
		CostCents: 1,
	}, nil
}
