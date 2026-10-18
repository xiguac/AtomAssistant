package llm

import "fmt"

// Thinking 是模型的思考强度。空值表示不指定，按模型服务自己的默认。
type Thinking string

const (
	ThinkingDefault Thinking = ""
	ThinkingOff     Thinking = "off"
	ThinkingLow     Thinking = "low"
	ThinkingMedium  Thinking = "medium"
	ThinkingHigh    Thinking = "high"
	ThinkingMax     Thinking = "max"
)

// ParseThinking 校验配置里写的思考强度。
func ParseThinking(s string) (Thinking, error) {
	switch t := Thinking(s); t {
	case ThinkingDefault, ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingMax:
		return t, nil
	}
	return "", fmt.Errorf("llm: 思考强度只能是 off / low / medium / high / max，实际 %q", s)
}

// Dialect 是一家模型服务在 OpenAI / Anthropic 协议之外各自加的参数写法。目前只有思考强度用得到。
type Dialect string

const (
	// DialectGeneric 是没有单独适配的服务：思考强度按 OpenAI 的 reasoning_effort 传。
	DialectGeneric Dialect = ""
	// DialectDeepSeek 见 https://api-docs.deepseek.com/zh-cn/guides/thinking_mode 。
	DialectDeepSeek Dialect = "deepseek"
	// DialectBailian 是阿里云百炼，见 https://help.aliyun.com/zh/model-studio/deep-thinking 。
	DialectBailian Dialect = "bailian"
)

// bailianBudget 是百炼按思考强度给的思考长度上限（token）。百炼只认长度，不认档位；接口允许 1～32768。
var bailianBudget = map[Thinking]int{
	ThinkingLow:    2048,
	ThinkingMedium: 8192,
	ThinkingHigh:   16384,
	ThinkingMax:    32768,
}

// openAIThinking 把思考强度写进 OpenAI 兼容接口的请求体。
func openAIThinking(d Dialect, t Thinking, body *chatRequest) {
	if t == ThinkingDefault {
		return
	}
	switch d {
	case DialectDeepSeek:
		// DeepSeek 认 low / high / max，别的档位它自己映射（medium 按 high）。
		if t == ThinkingOff {
			body.Thinking = &thinkingSwitch{Type: "disabled"}
			return
		}
		body.Thinking = &thinkingSwitch{Type: "enabled"}
		body.ReasoningEffort = string(t)
	case DialectBailian:
		on := t != ThinkingOff
		body.EnableThinking = &on
		if on {
			body.ThinkingBudget = bailianBudget[t]
		}
	default:
		// OpenAI 的 reasoning_effort 没有统一的"关闭"写法，有的服务收到不认得的值会直接报错，所以关闭时不传。
		switch t {
		case ThinkingLow, ThinkingMedium, ThinkingHigh:
			body.ReasoningEffort = string(t)
		case ThinkingMax:
			body.ReasoningEffort = string(ThinkingHigh)
		}
	}
}

// anthropicThinking 把思考强度写进 Anthropic 兼容接口的请求体。目前只适配了 DeepSeek 的那一个。
func anthropicThinking(d Dialect, t Thinking, body *anthropicRequest) {
	if t == ThinkingDefault || d != DialectDeepSeek {
		return
	}
	effort := string(t)
	switch t {
	case ThinkingOff:
		effort = "none"
	case ThinkingMedium:
		effort = string(ThinkingHigh)
	}
	body.Reasoning = &reasoningEffort{Effort: effort}
}

type thinkingSwitch struct {
	Type string `json:"type"`
}

type reasoningEffort struct {
	Effort string `json:"effort"`
}
