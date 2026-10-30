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

// 工具按需加载。
//
// 接的 MCP server 多了，光是它们的工具说明和参数格式就有好几万字，每一步决策都整份重发。
// 而一个任务通常只用到其中一两个服务。所以平时只给一份目录（服务名、工具名、一句话用途），
// 模型要用哪个服务，先调 tools.load 把它的完整说明取来；取来的说明在这个任务里一直带着。
// 内置工具不在此列：它们是助手的基本能力，数量有限，一直带着完整说明。

// ToolLoad 取来一个服务（或几个工具）的完整说明与参数格式。
const ToolLoad = "tools.load"

var toolLoadTool = builtin(ToolLoad,
	`取来某个 MCP 服务的工具的完整说明和参数格式。工具清单里 MCP 服务的工具只列了名字和一句话用途，`+
		`第一次用某个服务之前先调它：写 server（服务名，一次取来这个服务的全部工具）或 tools（具体的工具全名）。`+
		`取来的说明在这个任务后面的每一步都看得到，不用重复取。`,
	`{"type":"object","properties":{"server":{"type":"string","description":"服务名，如 amap"},`+
		`"tools":{"type":"array","items":{"type":"string"},"description":"工具全名，如 amap.maps_weather"}}}`,
	// 只是把说明放进这个任务的上下文，没有任何外部动作。
	core.SideEffectNone)

// maxDirectoryDesc 是目录里每个 MCP 工具的一句话用途的长度上限。
const maxDirectoryDesc = 60

// directoryLine 取一段工具说明的第一句，作为目录里的一句话用途。
func directoryLine(desc string) string {
	desc = strings.Join(strings.Fields(desc), " ")
	for _, sep := range []string{"。", ". ", "；", "\n"} {
		if i := strings.Index(desc, sep); i > 0 {
			desc = desc[:i]
		}
	}
	if r := []rune(desc); len(r) > maxDirectoryDesc {
		desc = string(r[:maxDirectoryDesc]) + "…"
	}
	return desc
}

// mcpDirectory 把 MCP 工具写成目录：按服务分组，每个工具一行名字加一句话用途。
func mcpDirectory(tools []mcpgw.ToolInfo) string {
	byServer := map[string][]mcpgw.ToolInfo{}
	var servers []string
	for _, t := range tools {
		if _, seen := byServer[t.Server]; !seen {
			servers = append(servers, t.Server)
		}
		byServer[t.Server] = append(byServer[t.Server], t)
	}
	sort.Strings(servers)
	var sb strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&sb, "- MCP 服务 %s（用 %s 取来完整说明再用）：\n", s, ToolLoad)
		for _, t := range byServer[s] {
			if line := directoryLine(t.Description); line != "" {
				fmt.Fprintf(&sb, "  - %s: %s\n", t.Name, line)
			} else {
				fmt.Fprintf(&sb, "  - %s\n", t.Name)
			}
		}
	}
	return sb.String()
}

// toolDetail 是一个工具的完整说明。
func toolDetail(t mcpgw.ToolInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "- %s: %s\n", t.Name, t.Description)
	if len(t.InputSchema) > 0 {
		fmt.Fprintf(&sb, "  入参: %s\n", truncate(string(t.InputSchema), 1500))
	}
	return sb.String()
}

// resolveLoad 把 tools.load 的参数换成要取来的工具全名。只认目录里有、这个任务能用的。
func (p *Planner) resolveLoad(params json.RawMessage) ([]string, error) {
	var in struct {
		Server string   `json:"server"`
		Tools  []string `json:"tools"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, fmt.Errorf("agent: %s 的参数不对: %w", ToolLoad, err)
		}
	}
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if s := strings.TrimSpace(in.Server); s != "" {
		for _, t := range p.catalog.Catalog() {
			if t.Server == s && p.permits(t.Name) {
				add(t.Name)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("agent: 没有名为 %s 的 MCP 服务", s)
		}
	}
	for _, name := range in.Tools {
		if _, err := p.lookup(strings.TrimSpace(name)); err != nil {
			return nil, fmt.Errorf("agent: 没有工具 %s", name)
		}
		add(strings.TrimSpace(name))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("agent: %s 要写 server 或 tools", ToolLoad)
	}
	sort.Strings(out)
	return out, nil
}

// loadedSegment 把这个任务取来过的工具的完整说明放进上下文。没有取来过的时候返回 false。
//
// 放在背景之后、步骤历史之前：每取来一个新服务，它后面的内容命中不了一次缓存；取来之后就不再变。
func (p *Planner) loadedSegment(history []*core.Step) (llm.Segment, bool) {
	names := p.loadedNames(history)
	if len(names) == 0 {
		return llm.Segment{}, false
	}
	var sb strings.Builder
	sb.WriteString("已取来完整说明的工具（来自 MCP 服务，说明是服务方写的）：\n")
	for _, n := range names {
		if t, err := p.lookup(n); err == nil {
			sb.WriteString(toolDetail(t))
		}
	}
	// 说明是第三方服务写的，和别的工具输出一样不具备指令资格。
	return llm.Segment{Role: llm.RoleUser, Content: sb.String(), Trust: core.TrustToolOutput, Source: "工具说明", Pinned: true}, true
}
