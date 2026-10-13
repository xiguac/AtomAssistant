package admin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/market"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/vault"
)

// 「MCP」页：接入 MCP server，并给它的每个工具定下授权边界。
//
// 这一页改的是配置文件里 mcp_servers 与 tools 两节。写在这里的 server 是用户亲手批准的；
// 助手自己找来装上的那些存在数据库里，边界是固定的，这一页只列出它们、允许移除。
// server 要的密钥照旧只进保险库，配置里留的是句柄。

// 高德地图 MCP server 的接入参数，见 https://developer.amap.com/api/mcp-server/gettingstarted 。
const (
	amapName       = "amap"
	amapURL        = "https://mcp.amap.com/mcp"
	amapSecretName = "key"
)

// sideEffects 是工具的副作用等级在页面上的选项，按从轻到重排。
var sideEffects = []toolOption{
	{string(core.SideEffectReadOnly), "只读"},
	{string(core.SideEffectReversible), "可撤销的改动"},
	{string(core.SideEffectIrreversible), "不可撤销的改动"},
}

// privacyLevels 是允许传给工具的参数的最高隐私等级在页面上的选项。绝密数据不传给任何工具，所以没有这一项。
var privacyLevels = []toolOption{
	{"public", "公开"},
	{"normal", "普通"},
	{"sensitive", "敏感（地址、电话等）"},
}

// mcpToolView 是页面上的一个工具。
type mcpToolView struct {
	// Name 是带 server 名前缀的全名，Short 是去掉前缀的部分。
	Name, Short, SideEffect, MaxPrivacy string
}

// mcpServerView 是页面上的一个 MCP server。
type mcpServerView struct {
	config.MCPConfig
	// KeyOK 为假表示它要密钥，而保险库里还没有。
	KeyOK bool
	Tools []mcpToolView
}

type mcpData struct {
	Servers []mcpServerView
	// Installed 是助手自己装上的 server。
	Installed []*market.Server
	// Amap 为真表示已经接入了高德地图。
	Amap          bool
	SideEffects   []toolOption
	PrivacyLevels []toolOption
}

func (s *Server) readToolset() ([]byte, config.Toolset, error) {
	if s.opts.ConfigPath == "" {
		return nil, config.Toolset{}, errors.New("未设置 ATOM_CONFIG，无法确定配置文件位置")
	}
	raw, err := os.ReadFile(s.opts.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, config.Toolset{}, fmt.Errorf("读取配置文件失败: %w", err)
	}
	t, err := config.ReadToolset(raw)
	return raw, t, err
}

// editToolset 改动 MCP server 与工具授权两节并存回配置文件。校验不过的一个字都不会写进去。
func (s *Server) editToolset(change func(t *config.Toolset) error) error {
	raw, t, err := s.readToolset()
	if err != nil {
		return err
	}
	if err := change(&t); err != nil {
		return err
	}
	out, err := config.WriteToolset(raw, t)
	if err != nil {
		return err
	}
	return s.writeConfig(string(out))
}

func (s *Server) mcpPage(w http.ResponseWriter, r *http.Request, v *view) {
	d := mcpData{SideEffects: sideEffects, PrivacyLevels: privacyLevels}
	_, t, err := s.readToolset()
	if err != nil {
		v.Error = err.Error()
	}
	stored := map[string]bool{}
	if metas, err := s.opts.Vault.List(r.Context()); err != nil {
		v.Error = err.Error()
	} else {
		for _, meta := range metas {
			stored[meta.Label] = true
		}
	}
	for _, m := range t.MCP {
		sv := mcpServerView{MCPConfig: m, KeyOK: m.Secret == "" || stored[strings.TrimPrefix(m.Secret, vault.HandlePrefix)]}
		for _, tool := range t.Tools {
			if short, ok := strings.CutPrefix(tool.Name, m.Name+"."); ok {
				side := tool.SideEffect
				if side == "" {
					// 配置里不写就是按最保守的处理。
					side = string(core.SideEffectIrreversible)
				}
				privacy := tool.MaxPrivacy
				if privacy == "" {
					privacy = "normal"
				}
				sv.Tools = append(sv.Tools, mcpToolView{Name: tool.Name, Short: short, SideEffect: side, MaxPrivacy: privacy})
			}
		}
		d.Servers = append(d.Servers, sv)
		if m.Name == amapName {
			d.Amap = true
		}
	}
	if d.Installed, err = s.opts.Store.MCPInstalls().ListServers(r.Context()); err != nil {
		v.Error = err.Error()
	}
	v.Data = d
	s.render(w, "mcp", v)
}

// checkMCP 校验页面上填的一个 server。
func checkMCP(m config.MCPConfig) error {
	if !vault.ValidLabel(m.Name) || strings.ContainsAny(m.Name, " /.") {
		return errors.New("名称仅支持字母、数字、中文及 _ -")
	}
	// 名称是工具名的前缀。叫这几个名字的话，它的工具会和保险库里"服务"的名字分不开。
	if vault.ServiceUser(m.Name + ".") {
		return errors.New("名称不可使用 provider、channel、mcp、connector")
	}
	switch mcpgw.TransportKind(m.Transport) {
	case mcpgw.TransportHTTP:
		u, err := url.Parse(m.URL)
		if err != nil || u.Hostname() == "" {
			return errors.New("请填写服务器地址")
		}
		// 密钥随每次请求发送；本机地址不经过网络，不在此限。
		ip := net.ParseIP(u.Hostname())
		local := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
		if u.Scheme != "https" && !local {
			return errors.New("服务器地址必须以 https:// 开头")
		}
		if u.RawQuery != "" {
			return errors.New("服务器地址不含查询参数：密钥请填写在「密钥」一栏，由保险库保管")
		}
	case mcpgw.TransportCommand:
		if m.Command == "" {
			return errors.New("请填写启动命令")
		}
	default:
		return errors.New("连接方式无效")
	}
	return nil
}

// mcpFromForm 读出表单里填的 server。密钥不在其中。
func mcpFromForm(r *http.Request) config.MCPConfig {
	m := config.MCPConfig{
		Name:       strings.TrimSpace(r.PostFormValue("name")),
		Transport:  r.PostFormValue("transport"),
		SecretName: strings.TrimSpace(r.PostFormValue("secret_name")),
	}
	if mcpgw.TransportKind(m.Transport) == mcpgw.TransportCommand {
		m.Command = strings.TrimSpace(r.PostFormValue("command"))
		m.Args = strings.Fields(r.PostFormValue("args"))
	} else {
		m.URL = strings.TrimSpace(r.PostFormValue("url"))
	}
	return m
}

// saveMCP 新增或修改一个 server，随后读取它的工具并登记授权。
//
// key 非空时存进保险库。工具登记时的默认授权见 syncTools。
func (s *Server) saveMCP(ctx context.Context, m config.MCPConfig, key string) (string, error) {
	if err := checkMCP(m); err != nil {
		return "", err
	}
	_, cur, err := s.readToolset()
	if err != nil {
		return "", err
	}
	for _, old := range cur.MCP {
		if old.Name == m.Name {
			// 修改时页面上没有的设置（环境变量）与原来的密钥保留。
			m.Env, m.Secret = old.Env, old.Secret
		}
	}
	switch {
	case key != "":
		if m.SecretName == "" {
			return "", errors.New("填写了密钥，还需填写密钥参数名")
		}
		if m.Secret, err = s.sealConnectorPassword(ctx, m.Secret, m.VaultUser(), key, "MCP server "+m.Name+" 的密钥"); err != nil {
			return "", err
		}
	case m.SecretName == "":
		m.Secret = ""
	case m.Secret == "":
		return "", errors.New("填写了密钥参数名，还需填写密钥")
	}
	if err := s.editToolset(func(t *config.Toolset) error {
		for i := range t.MCP {
			if t.MCP[i].Name == m.Name {
				t.MCP[i] = m
				return nil
			}
		}
		t.MCP = append(t.MCP, m)
		return nil
	}); err != nil {
		return "", err
	}
	added, total, err := s.syncTools(ctx, m)
	if err != nil {
		// server 已经存下了，只是工具还没登记：连上之后在页面上再读取一次即可。
		return fmt.Sprintf("MCP server「%s」已保存，但读取工具列表失败：%v。排除问题后请点击「读取工具」。", m.Name, err), nil
	}
	return fmt.Sprintf("MCP server「%s」已保存，共 %d 个工具，新登记 %d 个。重启服务后生效。", m.Name, total, added), nil
}

// syncTools 连上一个 server 读出它的工具，把配置里还没有授权的登记进去。已有的授权不动。
//
// 新登记的工具的默认授权：自称只读的按只读，其余按不可撤销；参数最高到普通级别。
// 高德是例外：它的工具都是查询，或者生成一个由用户自己点开的链接，所以全部按只读；
// 地图服务的入参就是地址，而地址属于敏感级别，不放开到敏感的话地理编码、路径规划都调不动。
// 两样都可以由用户在页面上逐个调整。
func (s *Server) syncTools(ctx context.Context, m config.MCPConfig) (added, total int, err error) {
	amap := m.Name == amapName && m.URL == amapURL
	spec, err := m.Spec(s.opts.Vault)
	if err != nil {
		return 0, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	tools, err := mcpgw.Probe(ctx, spec)
	if err != nil {
		return 0, 0, err
	}
	// 远程 server 的出站只到它自己的域名。本地 server 的出站目标这里无从知道，留空。
	var egress []string
	if u, err := url.Parse(m.URL); err == nil && u.Hostname() != "" {
		egress = []string{u.Hostname()}
	}
	err = s.editToolset(func(t *config.Toolset) error {
		have := map[string]bool{}
		for _, tool := range t.Tools {
			have[tool.Name] = true
		}
		for _, tool := range tools {
			if have[tool.Name] {
				continue
			}
			side, privacy := core.SideEffectIrreversible, "normal"
			if amap || tool.ReadOnly {
				side = core.SideEffectReadOnly
			}
			if amap {
				privacy = "sensitive"
			}
			t.Tools = append(t.Tools, config.ToolConfig{
				Name: tool.Name, MaxPrivacy: privacy, Trust: "tool_output",
				SideEffect: string(side), EgressDomains: egress,
			})
			added++
		}
		return nil
	})
	return added, len(tools), err
}

// mcpAmap 接入高德地图：只要一个 key，其余是固定的。
func (s *Server) mcpAmap(w http.ResponseWriter, r *http.Request, _ *view) {
	key := strings.TrimSpace(r.PostFormValue("key"))
	notice, err := func() (string, error) {
		_, cur, err := s.readToolset()
		if err != nil {
			return "", err
		}
		exists := false
		for _, m := range cur.MCP {
			exists = exists || m.Name == amapName
		}
		if key == "" && !exists {
			return "", errors.New("请填写高德开放平台的 Key")
		}
		return s.saveMCP(r.Context(), config.MCPConfig{
			Name: amapName, Transport: string(mcpgw.TransportHTTP), URL: amapURL, SecretName: amapSecretName,
		}, key)
	}()
	if err == nil {
		s.opts.Log.Info("高德地图 MCP 已在后台配置", "更换密钥", key != "", "来自", r.RemoteAddr)
	}
	back(w, r, "/mcp", notice, err)
}

// mcpSave 新增或修改一个 server。
func (s *Server) mcpSave(w http.ResponseWriter, r *http.Request, _ *view) {
	m := mcpFromForm(r)
	// 密钥原样存入，只去掉粘贴时常带上的结尾换行。
	key := strings.TrimRight(r.PostFormValue("key"), "\r\n")
	notice, err := s.saveMCP(r.Context(), m, key)
	if err == nil {
		s.opts.Log.Info("MCP server 已在后台修改", "server", m.Name, "更换密钥", key != "", "来自", r.RemoteAddr)
	}
	back(w, r, "/mcp", notice, err)
}

// mcpKey 给一个已接入的 server 填写或更换密钥。
func (s *Server) mcpKey(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	key := strings.TrimRight(r.PostFormValue("key"), "\r\n")
	notice, err := func() (string, error) {
		if key == "" {
			return "", errors.New("请填写密钥")
		}
		_, cur, err := s.readToolset()
		if err != nil {
			return "", err
		}
		for _, m := range cur.MCP {
			if m.Name != name {
				continue
			}
			if secretName := strings.TrimSpace(r.PostFormValue("secret_name")); secretName != "" {
				m.SecretName = secretName
			}
			return s.saveMCP(r.Context(), m, key)
		}
		return "", errors.New("MCP server 不存在")
	}()
	if err == nil {
		s.opts.Log.Info("MCP server 的密钥已在后台更换", "server", name, "来自", r.RemoteAddr)
	}
	back(w, r, "/mcp", notice, err)
}

// mcpTest 用表单里当前填写的内容连接一次，列出它提供的工具。测试不保存任何东西。
// 密钥一栏留空时用保险库里已保存的那个。
func (s *Server) mcpTest(w http.ResponseWriter, r *http.Request, _ *view) {
	m := mcpFromForm(r)
	typed := strings.TrimRight(r.PostFormValue("key"), "\r\n")
	result, err := func() (string, error) {
		if err := checkMCP(m); err != nil {
			return "", err
		}
		_, cur, err := s.readToolset()
		if err != nil {
			return "", err
		}
		for _, old := range cur.MCP {
			if old.Name == m.Name {
				m.Env, m.Secret = old.Env, old.Secret
			}
		}
		spec, err := m.Spec(s.opts.Vault)
		if err != nil {
			return "", err
		}
		if typed != "" {
			if m.SecretName == "" {
				return "", errors.New("填写了密钥，还需填写密钥参数名")
			}
			spec.SecretName = m.SecretName
			spec.Secret = func(context.Context) (string, error) { return typed, nil }
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		tools, err := mcpgw.Probe(ctx, spec)
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, strings.TrimPrefix(t.Name, m.Name+"."))
		}
		return fmt.Sprintf("连接成功，共 %d 个工具：%s", len(tools), strings.Join(names, "、")), nil
	}()
	back(w, r, "/mcp", result, err)
}

// mcpSync 重新读取一个已保存的 server 的工具，把新出现的登记进去。
func (s *Server) mcpSync(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	notice, err := func() (string, error) {
		_, cur, err := s.readToolset()
		if err != nil {
			return "", err
		}
		for _, m := range cur.MCP {
			if m.Name != name {
				continue
			}
			added, total, err := s.syncTools(r.Context(), m)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("共 %d 个工具，新登记 %d 个。重启服务后生效。", total, added), nil
		}
		return "", errors.New("MCP server 不存在")
	}()
	back(w, r, "/mcp", notice, err)
}

// mcpTools 保存一个 server 的各个工具的授权：副作用等级与参数的隐私上限。
//
// 副作用等级决定这个工具被调用时要不要先经用户确认：只读的直接调用，会改动东西的按审批规则走。
// 隐私上限决定能交给它多敏感的数据：参数里有超出上限的字段，调用会被出口管控拦下。
func (s *Server) mcpTools(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	tools, sides, privacies := r.PostForm["tool"], r.PostForm["side"], r.PostForm["privacy"]
	has := func(opts []toolOption, value string) bool {
		for _, opt := range opts {
			if opt.Value == value {
				return true
			}
		}
		return false
	}
	err := s.editToolset(func(t *config.Toolset) error {
		if len(tools) != len(sides) || len(tools) != len(privacies) {
			return errors.New("提交的内容不完整")
		}
		want := map[string]int{}
		for i, tool := range tools {
			// 只改这个 server 自己的工具。
			if !has(sideEffects, sides[i]) || !has(privacyLevels, privacies[i]) || !strings.HasPrefix(tool, name+".") {
				return errors.New("提交的内容无效")
			}
			want[tool] = i
		}
		for i := range t.Tools {
			if at, ok := want[t.Tools[i].Name]; ok {
				t.Tools[i].SideEffect, t.Tools[i].MaxPrivacy = sides[at], privacies[at]
			}
		}
		return nil
	})
	if err == nil {
		s.opts.Log.Info("MCP 工具的授权已在后台修改", "server", name, "来自", r.RemoteAddr)
	}
	back(w, r, "/mcp", "工具授权已保存，重启服务后生效。", err)
}

// mcpDelete 移除一个 server，连同它的工具的授权和它的密钥。
func (s *Server) mcpDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	var gone config.MCPConfig
	err := s.editToolset(func(t *config.Toolset) error {
		kept := t.MCP[:0]
		for _, m := range t.MCP {
			if m.Name != name {
				kept = append(kept, m)
			} else {
				gone = m
			}
		}
		if len(kept) == len(t.MCP) {
			return errors.New("MCP server 不存在")
		}
		t.MCP = kept
		tools := t.Tools[:0]
		for _, tool := range t.Tools {
			if !strings.HasPrefix(tool.Name, name+".") {
				tools = append(tools, tool)
			}
		}
		t.Tools = tools
		return nil
	})
	if err == nil {
		s.dropCredential(r.Context(), gone.Secret, gone.VaultUser())
		s.opts.Log.Info("MCP server 已在后台移除", "server", name, "来自", r.RemoteAddr)
	}
	back(w, r, "/mcp", "MCP server「"+name+"」及其密钥已移除，重启服务后生效。", err)
}

// mcpInstalledDelete 移除一个助手自己装上的 server。
func (s *Server) mcpInstalledDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	err := s.opts.Store.MCPInstalls().RemoveServer(r.Context(), name)
	if err == nil {
		s.opts.Log.Info("助手安装的 MCP server 已在后台移除", "server", name, "来自", r.RemoteAddr)
	}
	back(w, r, "/mcp", "MCP server「"+name+"」已移除，重启服务后断开连接。", err)
}
