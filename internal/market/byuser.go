package market

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// SecretKeeper 把密钥存进保险库，由 *vault.Vault 提供。
type SecretKeeper interface {
	Seal(ctx context.Context, label, value, note string, allowedTools []string) error
}

// ErrBadAddress 表示用户给的 MCP 地址用不了。
var ErrBadAddress = errors.New("market: 这个 MCP 地址用不了")

// vaultHandlePrefix 与 vault.HandlePrefix 一致。这里不引用 vault 包，免得为一个常量多一层依赖。
const vaultHandlePrefix = "vault://"

// InstallUserServer 装上用户亲口给出地址的一个远程 MCP server。
//
// 这和助手自己找来装的不是一回事。助手自己找的，地址来自公开登记处，谁写的都不知道，
// 所以要扫描、要评估风险、装上也只给固定的保守边界。这里的地址是用户给的——
// 和他在管理后台的「MCP」页里亲手填一样，是他的决定：不扫描，不搁置，按用户接入的对待。
// 调用方必须先确认这个地址确实是用户说的，而不是助手从别处读来的（见 agent.Approver）。
//
// 地址里带的查询参数当作密钥：存进保险库，库里只留句柄，连接时才取出来加到请求上。
func (i *Installer) InstallUserServer(ctx context.Context, name, rawURL string) (*Server, []string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return nil, nil, fmt.Errorf("%w: 无法解析", ErrBadAddress)
	}
	if u.Scheme != "https" {
		return nil, nil, fmt.Errorf("%w: 必须是 https 地址", ErrBadAddress)
	}
	if u.User != nil {
		return nil, nil, fmt.Errorf("%w: 地址里不能带账号密码", ErrBadAddress)
	}
	name = userServerName(name, u.Hostname())
	query := u.Query()
	if len(query) > 1 {
		return nil, nil, fmt.Errorf("%w: 地址里有多个参数，分不清哪个是密钥，请在管理后台的「MCP」页添加", ErrBadAddress)
	}
	srv := &Server{Name: name, ByUser: true, Description: "用户在对话里给出的地址", Payload: "{}", InstalledAt: time.Now().UTC()}
	for key, values := range query {
		if len(values) != 1 || values[0] == "" {
			return nil, nil, fmt.Errorf("%w: 参数 %s 没有值", ErrBadAddress, key)
		}
		if i.Secrets == nil {
			return nil, nil, fmt.Errorf("%w: 地址里带着密钥，但保险库不可用", ErrBadAddress)
		}
		label := "mcp." + name
		if err := i.Secrets.Seal(ctx, label, values[0], "MCP server "+name+" 的密钥", []string{label}); err != nil {
			return nil, nil, fmt.Errorf("market: 把密钥存进保险库失败: %w", err)
		}
		srv.SecretName, srv.Secret = key, vaultHandlePrefix+label
	}
	// 存下来的地址不带参数：密钥只在保险库里。
	u.RawQuery, u.Fragment = "", ""
	srv.URL = u.String()
	tools, err := i.commit(ctx, srv)
	if err != nil {
		return nil, nil, err
	}
	return srv, tools, nil
}

// userServerName 定出本机的 server 名：用助手起的，起得不合规矩就从域名里取。
//
// 不加 mcp- 前缀：那是给来路不明的 server 用的。但名字仍然只能是小写字母、数字、下划线和连字符，
// 并且不能撞上内建工具的名字空间。
func userServerName(name, host string) string {
	clean := func(s string) string {
		var sb strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(s)) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
				sb.WriteRune(r)
			}
		}
		return strings.Trim(sb.String(), "-_")
	}
	n := clean(name)
	if n == "" {
		// mcp.amap.com → amap；api.example.co → example。
		labels := strings.Split(strings.ToLower(host), ".")
		for _, l := range labels {
			if l != "mcp" && l != "api" && l != "www" && clean(l) != "" {
				n = clean(l)
				break
			}
		}
	}
	if n == "" || reservedNames[n] {
		n = "mcp-" + n
	}
	return n
}

// reservedNames 是内建工具和保险库用着的名字空间。
var reservedNames = map[string]bool{
	"calendar": true, "mail": true, "file": true, "sheet": true, "doc": true, "slides": true,
	"pay": true, "capability": true, "skill": true, "memory": true, "plan": true, "web": true,
	"team": true, "task": true, "mcp": true, "provider": true, "channel": true, "connector": true,
	"weather": true,
}
