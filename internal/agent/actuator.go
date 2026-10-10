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
	payer  Payer
	// resources 非空时启用付费资源（HTTP 402）。
	resources PaidResources
	calendar  CalendarService
	mail      MailService
	holidays  HolidayService
	files     FileService
	// team 与 workers 非空时可以把活派给协作模型。
	team    Dispatcher
	workers WorkerModel
	// capabilities 非空时助手可以自己查找、安装能力；library 用来读已安装的 Skill。
	capabilities CapabilityService
	library      SkillLibrary
	// audit 记录内建连接器的每一次调用。MCP 工具的审计由网关负责。
	audit mcpgw.AuditSink
}

// WithCalendar 接入日历连接器。
func (a *Actuator) WithCalendar(c CalendarService) *Actuator {
	a.calendar = c
	return a
}

// WithFiles 接入文件工作区。
func (a *Actuator) WithFiles(f FileService) *Actuator {
	a.files = f
	return a
}

// WithHolidays 接入节假日数据。
func (a *Actuator) WithHolidays(h HolidayService) *Actuator {
	a.holidays = h
	return a
}

// WithMail 接入邮件连接器。
func (a *Actuator) WithMail(m MailService) *Actuator {
	a.mail = m
	return a
}

// WithAudit 让内建连接器的调用留下审计记录。
func (a *Actuator) WithAudit(sink mcpgw.AuditSink) *Actuator {
	a.audit = sink
	return a
}

// WithPaidResources 接入付费资源。必须同时接入 Payer。
func (a *Actuator) WithPaidResources(r PaidResources) *Actuator {
	a.resources = r
	return a
}

// WithPayer 接入付款能力。
func (a *Actuator) WithPayer(p Payer) *Actuator {
	a.payer = p
	return a
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
	if s.ToolName == ToolDispatch {
		// 协作模型交回来的是模型生成的文本，和主 AI 自己的输出一个等级：
		// 它可能在复述材料里的话，不具备指令资格。
		return a.runDispatch(ctx, t, s)
	}
	if isFileTool(s.ToolName) {
		out, err := a.runFile(ctx, s)
		if a.audit != nil {
			detail := fmt.Sprintf("成功，副作用=%s", s.SideEffect)
			if err != nil {
				detail = "失败: " + err.Error()
			}
			_ = a.audit.ToolCall(ctx, t.ID, s.ToolName, core.TrustToolOutput, detail)
		}
		if err != nil {
			return kernel.Result{}, err
		}
		// 工作区里的文件是用户自己放进来的，读它不算"助手从外面取回了内容"；
		// 但文件里的文字不是用户此刻的指示，仍然低于 user，进上下文时在围栏里。
		return kernel.Result{Output: out, Trust: core.TrustToolOutput}, nil
	}
	if isCapabilityTool(s.ToolName) {
		out, err := a.runCapability(ctx, s)
		if a.audit != nil && s.ToolName == ToolCapabilityInstall {
			detail := "成功: " + string(out)
			if err != nil {
				detail = "失败: " + truncate(err.Error(), 300)
			}
			_ = a.audit.ToolCall(ctx, t.ID, s.ToolName, core.TrustExternal, detail)
		}
		if err != nil {
			return kernel.Result{}, err
		}
		// 搜索结果和 Skill 正文都是别人写的。安装的回执是我们自己生成的，只含标识。
		trust := core.TrustExternal
		if s.ToolName == ToolCapabilityInstall {
			trust = core.TrustToolOutput
		}
		return kernel.Result{Output: out, Trust: trust}, nil
	}
	if s.ToolName == ToolHolidays {
		out, err := a.runHolidays(ctx, s)
		if err != nil {
			return kernel.Result{}, err
		}
		// 节假日安排来自用户指定的订阅，是日期和种类这样的结构化事实，名称在取回时已经截短。
		// 按权威数据对待：查一下哪天调休，不该让后面记日程的动作变成"由外部内容引出"。
		return kernel.Result{Output: out, Trust: core.TrustConnector}, nil
	}
	if isConnectorTool(s.ToolName) {
		var out json.RawMessage
		var err error
		if isCalendarTool(s.ToolName) {
			out, err = a.runCalendar(ctx, s)
		} else {
			out, err = a.runMail(ctx, s)
		}
		trust := connectorTrust(s.ToolName)
		if a.audit != nil {
			detail := fmt.Sprintf("成功，副作用=%s", s.SideEffect)
			if err != nil {
				detail = "失败: " + err.Error()
			}
			// 审计只记做了什么，不记内容。审计写不进去不影响这一步的结果。
			_ = a.audit.ToolCall(ctx, t.ID, s.ToolName, trust, detail)
		}
		if err != nil {
			return kernel.Result{}, err
		}
		return kernel.Result{Output: out, Trust: trust}, nil
	}
	if isResourceTool(s.ToolName) {
		out, err := a.runResource(ctx, t, s)
		if err != nil {
			return kernel.Result{}, err
		}
		// 网络上取回来的内容，无论付没付钱，都是第三方写的。
		return kernel.Result{Output: out, Trust: core.TrustExternal}, nil
	}
	if isPayTool(s.ToolName) {
		out, err := a.runPay(ctx, t, s)
		if err != nil {
			return kernel.Result{}, err
		}
		// 付款程序的输出是自由文本，里面可能原样带着商户写的内容，按外部内容对待。
		return kernel.Result{Output: out, Trust: core.TrustExternal}, nil
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
