package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/session"
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
	weather   WeatherService
	sandbox   SandboxService
	files     FileService
	// team 与 workers 非空时可以把活派给协作模型。
	team    Dispatcher
	workers WorkerModel
	// chat 非空时可以把工作区里的文件发到用户的聊天窗口里。
	chat ChatFiles
	// shares 非空时可以给工作区里的文件建临时下载链接。
	shares ShareService
	// searcher 非空时可以联网搜索。webTools 非空时可以读网页，searchByTools 为真时搜索也改走它。
	searcher      Searcher
	webTools      WebTools
	searchByTools bool
	// capabilities 非空时助手可以自己查找、安装能力；library 用来读已安装的 Skill。
	capabilities CapabilityService
	// secretRequests 非空时助手可以在后台登记待用户填写的环境变量。
	secretRequests SecretRequests
	library        SkillLibrary
	// audit 记录内建连接器的每一次调用。MCP 工具的审计由网关负责。
	audit mcpgw.AuditSink
	// sessions 非空时可以查以前的会话，见 session.go。
	sessions session.Store
}

// WithCalendar 接入日历连接器。
func (a *Actuator) WithCalendar(c CalendarService) *Actuator {
	a.calendar = c
	return a
}

// WithChatFiles 接入消息渠道的发文件能力。
func (a *Actuator) WithChatFiles(c ChatFiles) *Actuator {
	a.chat = c
	return a
}

// WithFiles 接入文件工作区。
func (a *Actuator) WithFiles(f FileService) *Actuator {
	a.files = f
	return a
}

// WithShares 接入对外分享。
func (a *Actuator) WithShares(s ShareService) *Actuator {
	a.shares = s
	return a
}

// WithHolidays 接入节假日数据。
func (a *Actuator) WithHolidays(h HolidayService) *Actuator {
	a.holidays = h
	return a
}

// WithSandbox 接入沙箱。
func (a *Actuator) WithSandbox(s SandboxService) *Actuator {
	a.sandbox = s
	return a
}

// WithWeather 接入天气数据。
func (a *Actuator) WithWeather(w WeatherService) *Actuator {
	a.weather = w
	return a
}

// WithSecretRequests 让助手可以在管理后台登记待用户填写的环境变量。
func (a *Actuator) WithSecretRequests(r SecretRequests) *Actuator {
	a.secretRequests = r
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
	if s.ToolName == ToolLoad {
		// 说明由规划器按日志里的这一步放进上下文，这里只留一条回执。
		var in struct {
			Tools []string `json:"tools"`
		}
		if err := json.Unmarshal(s.Input, &in); err != nil {
			return kernel.Result{}, fmt.Errorf("agent: 解析 %s 的参数失败: %w", ToolLoad, err)
		}
		out, err := json.Marshal(map[string]any{"loaded": in.Tools, "note": "完整说明已放进上下文的「工具说明」一段"})
		if err != nil {
			return kernel.Result{}, err
		}
		return kernel.Result{Output: out, Trust: core.TrustToolOutput}, nil
	}
	if isInternalTool(s.ToolName) {
		// 指派、方案、审核意见是规划器自己写的记录，原样留在日志里。它们是模型写的，信任等级与模型输出一致。
		return kernel.Result{Output: s.Input, Trust: core.TrustToolOutput}, nil
	}
	if s.ToolName == ToolDispatch {
		// 协作模型交回来的是模型生成的文本，和主 AI 自己的输出一个等级：
		// 它可能在复述材料里的话，不具备指令资格。
		return a.runDispatch(ctx, t, s)
	}
	if s.ToolName == ToolFileSend {
		out, err := a.runFileSend(ctx, t, s)
		if a.audit != nil {
			detail := "成功: " + string(out)
			if err != nil {
				detail = "失败: " + truncate(err.Error(), 300)
			}
			_ = a.audit.ToolCall(ctx, t.ID, s.ToolName, core.TrustSystem, detail)
		}
		if err != nil {
			return kernel.Result{}, err
		}
		// 回执是我们自己生成的：文件名和大小。
		return kernel.Result{Output: out, Trust: core.TrustSystem}, nil
	}
	if isShareTool(s.ToolName) {
		out, err := a.runShare(ctx, t, s)
		if a.audit != nil {
			// 只记做了什么，不记结果：建链接的结果里有下载地址，谁拿到它谁就能下载。
			detail := "成功: " + truncate(string(s.Input), 300)
			if err != nil {
				detail = "失败: " + truncate(err.Error(), 300)
			}
			_ = a.audit.ToolCall(ctx, t.ID, s.ToolName, core.TrustSystem, detail)
		}
		if err != nil {
			return kernel.Result{}, err
		}
		// 结果是我们自己生成的：地址、编号、路径和过期时间。
		return kernel.Result{Output: out, Trust: core.TrustSystem}, nil
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
	if s.ToolName == ToolWebSearch || s.ToolName == ToolWebFetch {
		run := a.runWebSearch
		if s.ToolName == ToolWebFetch {
			run = a.runWebFetch
		}
		res, err := run(ctx, s)
		if a.audit != nil {
			detail := "成功"
			if err != nil {
				detail = "失败: " + truncate(err.Error(), 300)
			}
			// 审计只记查过，不记问了什么、查到什么。
			_ = a.audit.ToolCall(ctx, t.ID, s.ToolName, core.TrustExternal, detail)
		}
		return res, err
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
		// 搜索结果和 Skill 正文都是别人写的。装上了的回执是我们自己生成的，只含标识；
		// 被搁置的回执里有风险点，其中引着对方写的原文，仍按外部内容对待。
		trust := core.TrustExternal
		if s.ToolName == ToolCapabilityInstall && !strings.Contains(string(out), `"needs_approval":true`) {
			trust = core.TrustToolOutput
		}
		if s.ToolName == ToolSkillRead {
			trust = a.skillTrust(ctx, s)
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
	if s.ToolName == ToolVaultRequest {
		out, err := a.runVaultRequest(ctx, t, s)
		if a.audit != nil {
			detail := "成功: " + string(out)
			if err != nil {
				detail = "失败: " + truncate(err.Error(), 300)
			}
			_ = a.audit.ToolCall(ctx, t.ID, s.ToolName, core.TrustSystem, detail)
		}
		if err != nil {
			return kernel.Result{}, err
		}
		// 回执是我们自己生成的：名字和下一步怎么办。
		return kernel.Result{Output: out, Trust: core.TrustSystem}, nil
	}
	if s.ToolName == ToolSandboxRun {
		out, err := a.runSandbox(ctx, s)
		if a.audit != nil {
			detail := "成功"
			if err != nil {
				detail = "失败: " + truncate(err.Error(), 300)
			}
			// 审计只记跑过，不记代码和输出：它们可能带着文件里的内容。
			_ = a.audit.ToolCall(ctx, t.ID, s.ToolName, core.TrustToolOutput, detail)
		}
		if err != nil {
			return kernel.Result{}, err
		}
		// 代码是模型写的，处理的是工作区里的文件；输出和读文件一样按工具输出对待。
		return kernel.Result{Output: out, Trust: core.TrustToolOutput}, nil
	}
	if s.ToolName == ToolRecall {
		out, err := a.runRecall(ctx, s)
		if err != nil {
			return kernel.Result{}, err
		}
		// 以前的对话里有助手转述过的外部内容，和"此前的对话"一样按工具输出对待。
		return kernel.Result{Output: out, Trust: core.TrustToolOutput}, nil
	}
	if s.ToolName == ToolWeather {
		out, err := a.runWeather(ctx, s)
		if err != nil {
			return kernel.Result{}, err
		}
		// 天气来自用户自己接入的气象服务。数值是结构化的，但预警的标题和正文是气象台写的文字，
		// 所以不按权威数据对待，和普通工具的输出一个等级。
		return kernel.Result{Output: out, Trust: core.TrustToolOutput}, nil
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
		UserApproved:   kernel.UserApproved(ctx),
	})
	if err != nil {
		return kernel.Result{}, err
	}
	return kernel.Result{Output: res.Output, Trust: res.Trust}, nil
}
