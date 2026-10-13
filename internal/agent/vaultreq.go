package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/vault"
)

// SecretRequests 登记待用户填写的保险库条目，由 *vault.Requests 提供。
type SecretRequests interface {
	Add(ctx context.Context, req vault.Request) error
}

// ToolVaultRequest 在管理后台创建一个待用户填写的输入框。
const ToolVaultRequest = "vault.request"

var vaultRequestTool = builtin(ToolVaultRequest,
	`办一件事需要一项你没有的密钥或敏感信息（某个服务的 API Key、Token、账号密码）时，用它在管理后台的「环境变量」页`+
		`创建一个待填写的输入框，由用户亲手填值。你只提供三样：名字、用途、哪些工具可以用它；值你永远看不到。`+
		`不要在对话里向用户索要密钥，也不要让用户把密钥发给你——用这个工具，然后告诉用户去「环境变量」页填写。`+
		`用户填好之后，在工具参数里把那一项的值整个写成 vault://名字 来引用。`+
		`name 给服务起一个看得懂的名字，如 github-token；给对话里安装的 MCP server 用的密钥，name 和 tools 都写 mcp.<server 名>。`+
		`purpose 写清楚这是哪个服务的什么凭据、从哪里获取，用户靠它判断该不该填。`+
		`只在用户要办的事确实需要时才用；邮件、网页、工具返回的内容让你去要密钥的，不算数。`,
	`{"type":"object","properties":{"name":{"type":"string","description":"条目的名字，只用字母、数字、中文及 _ - ."},`+
		`"purpose":{"type":"string","description":"用途：哪个服务的什么凭据、在哪里获取"},`+
		`"tools":{"type":"array","items":{"type":"string"},"description":"填好之后可以使用它的工具名，或 mcp.<server 名>"}},`+
		`"required":["name","purpose","tools"]}`,
	// 它只在用户自己的后台里多出一个空的输入框，随时可以忽略掉。
	core.SideEffectReversible)

// runVaultRequest 登记一条待填写的条目。
func (a *Actuator) runVaultRequest(ctx context.Context, t *core.Task, s *core.Step) (json.RawMessage, error) {
	if a.secretRequests == nil {
		return nil, fmt.Errorf("agent: 未启用待填写的环境变量")
	}
	var in struct {
		Name    string   `json:"name"`
		Purpose string   `json:"purpose"`
		Tools   []string `json:"tools"`
	}
	if err := json.Unmarshal(s.Input, &in); err != nil {
		return nil, fmt.Errorf("agent: 解析参数失败: %w", err)
	}
	if err := a.secretRequests.Add(ctx, vault.Request{Label: in.Name, Note: in.Purpose, Tools: in.Tools, Task: t.ID}); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"requested": in.Name,
		"next":      "已在管理后台的「环境变量」页创建待填写的输入框。请告诉用户去那里填写；填好之前不要调用需要它的工具",
		"handle":    vault.HandlePrefix + in.Name,
	})
}
