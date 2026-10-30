package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/mcpgw"
	"github.com/xiguac/atomassistant/internal/policy"
	"github.com/xiguac/atomassistant/internal/share"
)

// 对外分享：给工作区里助手生成的文件建临时下载链接，见 internal/share。

// ShareService 建、列、撤销下载链接，由 *share.Service 提供。
type ShareService interface {
	Create(ctx context.Context, taskID, path string, ttl time.Duration) (share.Created, error)
	List(ctx context.Context) ([]*share.Link, error)
	Revoke(ctx context.Context, id string) error
	CopyReceived(path string) (string, error)
}

const (
	ToolShareCreate = "share.create"
	ToolShareList   = "share.list"
	ToolShareRevoke = "share.revoke"
	// ToolShareCopy 把用户的文件复制成可以分享的副本。每次都要用户批准，没有规则能放行它，见 Approver.CheckAction。
	ToolShareCopy = "share.copy_received"
)

var shareToolset = []mcpgw.ToolInfo{
	builtin(ToolShareCreate,
		`给工作区里的一个文件或一个目录建临时下载链接，让别的服务、别的人能按地址下载。`+
			`要把你生成的文件交给某个接口（比如它要一个文件地址）时用它。`+
			`只能分享你自己生成的文件（写的文档、表格、演示文稿，沙箱跑出来的）；`+
			`用户发来、上传的文件不能直接分享，要先用 `+ToolShareCopy+` 经用户批准复制一份，再分享那份副本。`+
			`有效期用 minutes 指定，默认 60，最长 1440（24 小时），按需要给，不要一律给满。`+
			`目录链接打开是一份 JSON 文件清单，里面有每个文件各自的下载地址。`+
			`链接分享的是此刻的内容：文件之后再改，要重新建链接。地址只在这次结果里出现，之后查不到。`+
			`用完了用 `+ToolShareRevoke+` 撤销。`,
		`{"type":"object","properties":{"path":{"type":"string","description":"`+pathHint+`，或一个目录"},`+
			`"minutes":{"type":"integer","minimum":1,"maximum":1440,"description":"有效期（分钟），默认 60"}},"required":["path"]}`,
		// 链接随时可以撤销。它本身不把东西发给谁：地址要经过对外发送的动作才出得去，那一步由用户确认。
		core.SideEffectReversible),
	builtin(ToolShareList, `列出现在有效的下载链接：分享的是什么、什么时候过期。地址本身不在其中。`,
		`{"type":"object","properties":{}}`, core.SideEffectReadOnly),
	builtin(ToolShareRevoke, `提前撤销一个下载链接，之后谁也下载不到。id 来自 `+ToolShareCreate+` 或 `+ToolShareList+` 的结果。`,
		`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`,
		// 撤销只会让对外敞开的东西变少，不必再确认。
		core.SideEffectReversible),
	builtin(ToolShareCopy,
		`把用户发来、上传的一个文件复制到「分享副本」目录，副本可以用 `+ToolShareCreate+` 分享。`+
			`每次都要用户批准：只在用户明确要把这个文件给别人、给别的服务时用。返回副本的路径。`,
		`{"type":"object","properties":{"path":{"type":"string","description":"`+pathHint+`"}},"required":["path"]}`,
		core.SideEffectReversible),
}

// isShareTool 报告一个工具名是否属于分享工具。
func isShareTool(name string) bool {
	for _, t := range shareToolset {
		if t.Name == name {
			return true
		}
	}
	return false
}

// ShareDefaults 给建链接和撤销链接加上默认放行：用户没有为它们单独写规则时，直接执行。
//
// 建链接不把任何东西交给第三方：地址要经过发信、HTTP 请求、MCP 这些对外发送的动作才出得去，
// 那些动作照旧由用户确认。能分享的又只有助手自己生成的文件。复制用户的文件不在其中，见 CheckAction。
func ShareDefaults(p *policy.Policy) {
	for _, tool := range []string{ToolShareCreate, ToolShareRevoke} {
		listed := false
		for _, r := range p.Rules {
			if r.Tool == tool {
				listed = true
			}
		}
		if !listed {
			p.Rules = append(p.Rules, policy.Rule{Tool: tool})
		}
	}
}

// shareCopyReason 是请用户批准复制一个文件时给他看的理由。
func shareCopyReason(input json.RawMessage) string {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &in)
	return fmt.Sprintf("助手要把你的文件「%s」复制一份到「分享副本」，之后这份副本可以通过下载链接给到外部。需要你批准",
		strings.TrimSpace(in.Path))
}

// runShare 执行分享工具。
func (a *Actuator) runShare(ctx context.Context, t *core.Task, s *core.Step) (json.RawMessage, error) {
	if a.shares == nil {
		return nil, fmt.Errorf("agent: 没有开启对外分享")
	}
	var in struct {
		Path    string `json:"path"`
		Minutes int    `json:"minutes"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(s.Input, &in); err != nil {
		return nil, fmt.Errorf("agent: 解析参数失败: %w", err)
	}
	switch s.ToolName {
	case ToolShareCreate:
		out, err := a.shares.Create(ctx, t.ID, in.Path, time.Duration(in.Minutes)*time.Minute)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	case ToolShareList:
		links, err := a.shares.List(ctx)
		if err != nil {
			return nil, err
		}
		type item struct {
			ID        string    `json:"id"`
			Path      string    `json:"path"`
			Dir       bool      `json:"dir,omitempty"`
			Files     int       `json:"files"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		items := []item{}
		for _, l := range links {
			items = append(items, item{ID: l.ID, Path: l.Path, Dir: l.Dir, Files: len(l.Files), ExpiresAt: l.ExpiresAt})
		}
		return json.Marshal(map[string]any{"links": items})
	case ToolShareRevoke:
		if err := a.shares.Revoke(ctx, in.ID); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"revoked": in.ID})
	case ToolShareCopy:
		copied, err := a.shares.CopyReceived(in.Path)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"copy": copied})
	}
	return nil, fmt.Errorf("agent: 未知的分享工具 %q", s.ToolName)
}
