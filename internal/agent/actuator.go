package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/mcpgw"
)

// ToolCaller 是 Actuator 依赖的工具调用能力。
//
// 抽成接口是为了让 Actuator 能在不起 MCP server 的情况下被测试；
// 生产实现是 *mcpgw.Gateway。
type ToolCaller interface {
	Call(ctx context.Context, req mcpgw.CallRequest) (mcpgw.CallResult, error)
}

// Completer 是 Actuator 依赖的模型调用能力。
type Completer interface {
	Complete(ctx context.Context, purpose llm.Purpose, req llm.Request) (llm.Response, error)
}

// llmStepInput 是 StepLLM 的输入格式，由 Planner 写入。
type llmStepInput struct {
	Purpose  llm.Purpose   `json:"purpose"`
	Messages []llm.Message `json:"messages"`
}

// Actuator 执行步骤的实际副作用。
//
// 它不做任何判断——该不该做是 Planner 和 Approver 的事，
// 它只负责把已经被批准的动作可靠地执行掉。
type Actuator struct {
	tools  ToolCaller
	model  Completer
	memory MemoryProvider
}

var _ kernel.Actuator = (*Actuator)(nil)

// NewActuator 构造执行器。
func NewActuator(tools ToolCaller, model Completer) *Actuator {
	return &Actuator{tools: tools, model: model}
}

// WithMemory 接入记忆系统，启用内置的 memory.expand 工具。
func (a *Actuator) WithMemory(m MemoryProvider) *Actuator {
	a.memory = m
	return a
}

// Execute 执行一个步骤。
func (a *Actuator) Execute(ctx context.Context, t *core.Task, s *core.Step) (kernel.Result, error) {
	switch s.Type {
	case core.StepLLM:
		return a.runLLM(ctx, s)
	case core.StepTool:
		return a.runTool(ctx, t, s)
	case core.StepWait, core.StepBranch, core.StepApprove:
		// 这些步骤只是把状态变化记进日志，没有外部副作用。
		return kernel.Result{Output: s.Input, Trust: core.TrustSystem}, nil
	default:
		return kernel.Result{}, fmt.Errorf("agent: 未知的步骤类型 %q", s.Type)
	}
}

func (a *Actuator) runLLM(ctx context.Context, s *core.Step) (kernel.Result, error) {
	var in llmStepInput
	if err := json.Unmarshal(s.Input, &in); err != nil {
		return kernel.Result{}, fmt.Errorf("agent: 解析 LLM 步骤输入失败: %w", err)
	}
	if len(in.Messages) == 0 {
		return kernel.Result{}, fmt.Errorf("agent: LLM 步骤没有消息")
	}
	purpose := in.Purpose
	if purpose == "" {
		purpose = llm.PurposePlan
	}

	resp, err := a.model.Complete(ctx, purpose, llm.Request{Messages: in.Messages})
	if err != nil {
		return kernel.Result{}, err
	}
	out, err := json.Marshal(map[string]string{"content": resp.Content, "model": resp.Model})
	if err != nil {
		return kernel.Result{}, fmt.Errorf("agent: 序列化模型返回失败: %w", err)
	}
	return kernel.Result{
		Output: out,
		// 模型生成的文本不是可信指令来源：它可能在复述 untrusted 输入。
		Trust:      core.TrustToolOutput,
		TokensIn:   resp.Usage.PromptTokens,
		TokensOut:  resp.Usage.CompletionTokens,
		CostMicros: resp.CostMicros,
	}, nil
}

func (a *Actuator) runTool(ctx context.Context, t *core.Task, s *core.Step) (kernel.Result, error) {
	// 内置工具不走 MCP：它读的是本地记忆，没有对外出口，
	// 让它绕一圈网关只会白白增加一次序列化。
	if s.ToolName == ToolPlan {
		out, err := runPlan(s.Input)
		if err != nil {
			return kernel.Result{}, err
		}
		// 清单是模型自己写的，信任等级与模型输出一致。
		return kernel.Result{Output: out, Trust: core.TrustToolOutput}, nil
	}
	if s.ToolName == ToolExpandMemory {
		out, err := a.runExpandMemory(ctx, s.Input)
		if err != nil {
			return kernel.Result{}, err
		}
		return kernel.Result{Output: out, Trust: core.TrustConnector}, nil
	}

	res, err := a.tools.Call(ctx, mcpgw.CallRequest{
		Tool:   s.ToolName,
		Params: s.Input,
		// 幂等键必须透传，否则崩溃重放会重复下单。
		IdempotencyKey: s.IdempotencyKey,
		TaskID:         t.ID,
	})
	if err != nil {
		return kernel.Result{}, err
	}
	return kernel.Result{Output: res.Output, Trust: res.Trust}, nil
}
