package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/mcpgw"
)

// 规划器用原生工具调用作答：可用的工具作为接口的 tools 参数发出，
// 结束、提问、等待、失败也做成几个函数，模型调用其中之一，或者调用一个、几个工具。
// 模型的回答先换成 modelDecision，再走 validate：工具必须在目录里、副作用等级取自目录、
// 附带的查询不能有副作用，这些规则和以前一样。见 llm/tools.go 里为什么要这样做。

// 控制函数的名字。工具全名都带点号（如 calendar.events），不会和它们撞上。
const (
	callFinish = "finish"
	callAsk    = "ask_user"
	callWait   = "wait"
	callFail   = "fail"
)

var controlTools = []llm.Tool{
	{Name: callFinish, Description: "任务办完了：在 reason 里写出完整的结论或给用户的答复。",
		Parameters: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string","description":"完整的结论或给用户的答复"}},"required":["reason"]}`)},
	{Name: callAsk, Description: "缺少会改变结论、又查不到的信息时，向用户提一个具体的问题。",
		Parameters: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"给用户的一个问题，带上你建议的答案"}},"required":["question"]}`)},
	{Name: callWait, Description: "等一段时间再继续，或等某个事件发生再继续。",
		Parameters: json.RawMessage(`{"type":"object","properties":{"wait_seconds":{"type":"integer","description":"等多少秒；设了 wait_event 时是等事件的超时"},` +
			`"wait_event":{"type":"string","description":"可选：要等的事件的订阅表达式"},"reason":{"type":"string","description":"一句话说明为什么等"}},"required":["wait_seconds"]}`)},
	{Name: callFail, Description: "办不下去了：在 reason 里说明原因。",
		Parameters: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string","description":"为什么办不下去"}},"required":["reason"]}`)},
}

func isControl(name string) bool {
	return name == callFinish || name == callAsk || name == callWait || name == callFail
}

// nativeTools 是这一步发给模型的工具：内置工具、这个任务取来过说明的 MCP 工具，以及几个控制函数。
// 没取来说明的 MCP 工具不发：模型要先调 tools.load。
func (p *Planner) nativeTools(history []*core.Step) []llm.Tool {
	builtins := p.builtins()
	sort.Slice(builtins, func(i, j int) bool { return builtins[i].Name < builtins[j].Name })
	out := make([]llm.Tool, 0, len(builtins)+len(controlTools)+4)
	for _, t := range builtins {
		out = append(out, llm.Tool{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	for _, name := range p.loadedNames(history) {
		t, err := p.lookup(name)
		if err != nil || !p.permits(t.Name) {
			continue
		}
		// 服务方写的说明不放进工具定义：它和工具输出一样不具备指令资格，留在上下文的围栏里（见 loadedSegment）。
		out = append(out, llm.Tool{Name: t.Name, Parameters: t.InputSchema,
			Description: fmt.Sprintf("MCP 服务 %s 的工具，说明见上下文里的「工具说明」。", t.Server)})
	}
	return append(out, controlTools...)
}

// toolDirectory 是写进系统提示的 MCP 工具目录。内置工具不在这里：它们作为函数发给模型了。
func (p *Planner) toolDirectory() string {
	var tools []mcpgw.ToolInfo
	for _, t := range p.catalog.Catalog() {
		if p.permits(t.Name) {
			tools = append(tools, t)
		}
	}
	if len(tools) == 0 {
		return ""
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return "\n\nMCP tools (call " + ToolLoad + " first to get their parameters; after that they can be called directly):\n" + mcpDirectory(tools)
}

// maxCallReason 是工具调用旁边那段说明的长度上限，它作为这一步的理由显示给用户。
const maxCallReason = 200

// decodeNative 把模型的回答换成 modelDecision。
//
// 调了工具：第一个是这一步，其余的作为附带查询（validate 会拒绝其中有副作用的）。同时调了控制函数的，以工具为准。
// 只调了控制函数：按它办。什么都没调：正文若是以前那种 JSON 决策就照它办；
// 否则 acceptText 为真时（已经提醒过一次还是只写了文字）把正文当作给用户的答复，为假时算答非所问、要求重答。
// 看着像 JSON 却不是决策的（比如一个数组）一律不收。
func decodeNative(resp llm.Response, acceptText bool) (modelDecision, error) {
	var tools []llm.ToolCall
	var ctrl *llm.ToolCall
	for i, c := range resp.ToolCalls {
		if isControl(c.Name) {
			if ctrl == nil {
				ctrl = &resp.ToolCalls[i]
			}
			continue
		}
		if !json.Valid(c.Arguments) {
			return modelDecision{}, fmt.Errorf("%w: 调用 %s 的参数不是合法 JSON: %s", errBadOutput, c.Name, truncate(string(c.Arguments), 200))
		}
		tools = append(tools, c)
	}
	text := strings.TrimSpace(resp.Content)
	switch {
	case len(tools) > 0:
		md := modelDecision{Action: "tool", Tool: tools[0].Name, Params: tools[0].Arguments, Reason: truncate(text, maxCallReason)}
		for _, c := range tools[1:] {
			md.More = append(md.More, modelCall{Tool: c.Name, Params: c.Arguments})
		}
		return md, nil
	case ctrl != nil:
		var in struct {
			Reason      string `json:"reason"`
			Question    string `json:"question"`
			WaitSeconds int64  `json:"wait_seconds"`
			WaitEvent   string `json:"wait_event"`
		}
		if err := json.Unmarshal(ctrl.Arguments, &in); err != nil {
			return modelDecision{}, fmt.Errorf("%w: 调用 %s 的参数不对: %v", errBadOutput, ctrl.Name, err)
		}
		md := modelDecision{Reason: in.Reason, Question: in.Question, WaitSeconds: in.WaitSeconds, WaitEvent: in.WaitEvent}
		switch ctrl.Name {
		case callFinish:
			md.Action = "finish"
			// 结论写在了正文里、reason 空着：用正文。
			if strings.TrimSpace(md.Reason) == "" {
				md.Reason = text
			}
		case callAsk:
			md.Action = "ask"
		case callWait:
			md.Action = "wait"
		case callFail:
			md.Action = "fail"
		}
		return md, nil
	case text == "":
		return modelDecision{}, fmt.Errorf("%w: 既没有调用工具也没有正文", errBadOutput)
	}
	var md modelDecision
	body := stripFence(text)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&md); err == nil && md.Action != "" {
		return md, nil
	}
	if acceptText && !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
		return modelDecision{Action: "finish", Reason: text}, nil
	}
	return modelDecision{}, fmt.Errorf("%w: 没有调用任何函数，只写了一段文字（%s）", errBadOutput, truncate(text, 120))
}

// loadedNames 是这个任务里用 tools.load 取来过说明的工具，按取来的先后。
func (p *Planner) loadedNames(history []*core.Step) []string {
	var names []string
	seen := map[string]bool{}
	for _, s := range history {
		if s.ToolName != ToolLoad || s.Status != core.StepDone {
			continue
		}
		var in struct {
			Tools []string `json:"tools"`
		}
		if json.Unmarshal(s.Input, &in) != nil {
			continue
		}
		for _, n := range in.Tools {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names
}
