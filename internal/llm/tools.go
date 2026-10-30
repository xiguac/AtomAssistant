package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// 原生工具调用：把可用的工具作为接口的 tools 参数发给模型，模型以结构化的调用作答，
// 而不是在正文里按我们约定的格式写一段 JSON。
//
// 这一代模型都专门训练过原生的工具调用。把工具清单写进提示词、再用 JSON 模式要它输出自定义格式，
// 两者是冲突的：模型想用原生格式调工具，JSON 模式又不许它输出那些标记，结果常常只吐出一串空白。
// 实测 DeepSeek：同一份规划请求用 JSON 模式重放 20 次，空白 2～10 次；改用原生工具调用 40 次全部可用。

// Tool 是一个可供模型调用的工具。
type Tool struct {
	// Name 是工具的全名，如 calendar.events。发给接口时会换成它认的写法，返回时再换回来。
	Name        string
	Description string
	// Parameters 是参数的 JSON Schema。空值按不需要参数。
	Parameters json.RawMessage
}

// ToolCall 是模型发起的一次工具调用。
type ToolCall struct {
	// Name 是工具全名（已换回 Tool.Name 的写法）。
	Name string
	// Arguments 是模型给的参数，原样交回。不保证是合法 JSON，调用方要自己校验。
	Arguments json.RawMessage
}

// maxWireName 是接口允许的函数名长度上限（OpenAI 是 64）。
const maxWireName = 64

// wireNames 把工具全名换成接口认的函数名（只能是字母、数字、下划线和连字符），并给出反查表。
// 点号换成两个下划线，其余不认的字符换成下划线；太长或撞名的，截短后加一段摘要。
func wireNames(tools []Tool) ([]string, map[string]string) {
	out := make([]string, len(tools))
	back := make(map[string]string, len(tools))
	for i, t := range tools {
		w := wireName(t.Name)
		if _, taken := back[w]; taken || len(w) > maxWireName {
			sum := sha256.Sum256([]byte(t.Name))
			suffix := "_" + hex.EncodeToString(sum[:4])
			if len(w) > maxWireName-len(suffix) {
				w = w[:maxWireName-len(suffix)]
			}
			w += suffix
		}
		out[i] = w
		back[w] = t.Name
	}
	return out, back
}

func wireName(name string) string {
	var sb strings.Builder
	for _, r := range strings.ReplaceAll(name, ".", "__") {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// toolSchema 返回发给接口的参数格式：必须是一个 object 的 JSON Schema。
// MCP 服务给的格式可能是空的或者没写 type，这里补齐。
func toolSchema(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || m == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if _, ok := m["type"]; !ok {
		m["type"] = "object"
	}
	if m["type"] == "object" {
		if _, ok := m["properties"]; !ok {
			m["properties"] = map[string]any{}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return out
}

// toolArgs 把模型给的参数整理成 JSON：空的算 {}。
func toolArgs(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(s)
}
