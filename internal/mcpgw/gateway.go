// Package mcpgw 是 MCP 工具网关。
//
// 它是任务对外产生副作用的**唯一出口**：所有工具调用——无论查询还是执行——
// 都必须经过这里，才能统一做隐私分级、出口管控、幂等和审计。
// 任何模块都不得绕过它直接连 MCP server。
//
// 见 docs/架构方案.md §7、docs/安全模型.md §4。
package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/vault"
)

var (
	// ErrToolNotFound 表示目录里没有这个工具。
	ErrToolNotFound = errors.New("mcpgw: 工具不存在")
	// ErrToolFailed 表示工具执行返回了错误。
	ErrToolFailed = errors.New("mcpgw: 工具执行失败")
	// ErrNotConnected 表示对应的 MCP server 尚未连接。
	ErrNotConnected = errors.New("mcpgw: MCP server 未连接")
)

// TransportKind 是连接 MCP server 的方式。
type TransportKind string

const (
	// TransportCommand 以子进程方式启动本地 MCP server，走 stdio。
	TransportCommand TransportKind = "command"
	// TransportHTTP 连接远程 MCP server。
	TransportHTTP TransportKind = "http"
)

// ServerSpec 描述一个已被用户批准的 MCP server。
type ServerSpec struct {
	// Name 是这个 server 的标识，同时作为工具名的命名空间前缀。
	Name      string
	Transport TransportKind

	// Command / Args / Env 用于 TransportCommand。
	// Env 只包含显式声明的变量——绝不能把主进程的环境整个传下去，
	// 那会把数据库密码和模型密钥一并交给第三方工具。
	Command string
	Args    []string
	Env     []string

	// URL 用于 TransportHTTP。
	URL string
}

// ToolInfo 是目录里的一个工具，供 Planner 选择使用。
type ToolInfo struct {
	// Name 是带命名空间的全局名，形如 "calendar.create_event"。
	Name        string
	Server      string
	RawName     string
	Description string
	InputSchema json.RawMessage
	SideEffect  core.SideEffect
}

// CallRequest 是一次工具调用。
type CallRequest struct {
	Tool   string
	Params json.RawMessage
	// IdempotencyKey 由内核从 taskID+seq 派生，透传给外部服务用于去重。
	IdempotencyKey string
	// TaskID 仅用于审计。
	TaskID string
}

// CallResult 是一次工具调用的结果。
type CallResult struct {
	// Output 是结构化后的返回内容。
	Output json.RawMessage
	// Trust 是这次返回内容的信任等级。
	//
	// 结构化字段取工具的信任上限；纯文本一律降级为 TrustExternal——
	// 工具返回的文本可能原样转发了第三方写的内容。
	Trust core.TrustLevel
}

// SecretInjector 在调用工具的那一刻把参数里的保险库句柄换成明文，由 *vault.Vault 实现。
//
// 网关是明文唯一出现的地方：换好的参数直接交给工具，返回内容清掉明文之后才往回传。
type SecretInjector interface {
	Inject(ctx context.Context, tool string, params json.RawMessage) (*vault.Injection, error)
}

// AuditSink 接收网关产生的审计记录。
type AuditSink interface {
	ToolCall(ctx context.Context, taskID, tool string, trust core.TrustLevel, detail string) error
}

type session struct {
	spec ServerSpec
	cs   *mcp.ClientSession
}

// Gateway 管理多个 MCP server 连接与统一的工具目录。
type Gateway struct {
	mu       sync.RWMutex
	sessions map[string]*session
	catalog  map[string]ToolInfo

	guard    *policy.Guard
	registry *policy.Registry
	audit    AuditSink
	secrets  SecretInjector

	clientName    string
	clientVersion string
}

// Options 是网关构造参数。
type Options struct {
	Guard    *policy.Guard
	Registry *policy.Registry
	Audit    AuditSink
	Version  string
	// Secrets 为 nil 时参数里的保险库句柄不会被替换，原样交给工具。
	Secrets SecretInjector
}

// New 构造网关。
func New(opts Options) (*Gateway, error) {
	if opts.Guard == nil || opts.Registry == nil {
		return nil, fmt.Errorf("mcpgw: 必须提供 Guard 与 Registry——没有出口管控的网关不允许存在")
	}
	v := opts.Version
	if v == "" {
		v = "dev"
	}
	return &Gateway{
		sessions:      make(map[string]*session),
		catalog:       make(map[string]ToolInfo),
		guard:         opts.Guard,
		registry:      opts.Registry,
		audit:         opts.Audit,
		secrets:       opts.Secrets,
		clientName:    "atomhelper",
		clientVersion: v,
	}, nil
}

// Connect 连接一个 MCP server 并把它的工具登记进目录。
//
// 工具的能力边界必须已经通过 policy.Registry 授权过；未授权的工具会被登记，
// 但调用时会被 Guard 拒绝——目录可见不等于可调用。
func (g *Gateway) Connect(ctx context.Context, spec ServerSpec) error {
	transport, err := buildTransport(spec)
	if err != nil {
		return err
	}
	return g.ConnectTransport(ctx, spec, transport)
}

// ConnectTransport 用调用方提供的传输连接一个 MCP server。
//
// Connect 内部走的就是它。单独导出是为了让测试能用进程内传输，
// 以及将来接入自定义传输时不必改动网关逻辑。
func (g *Gateway) ConnectTransport(ctx context.Context, spec ServerSpec, transport mcp.Transport) error {
	if spec.Name == "" {
		return fmt.Errorf("mcpgw: server 名称不能为空")
	}
	if transport == nil {
		return fmt.Errorf("mcpgw: %s 缺少传输", spec.Name)
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    g.clientName,
		Version: g.clientVersion,
	}, nil)

	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("mcpgw: 连接 %s 失败: %w", spec.Name, err)
	}

	tools, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		_ = cs.Close()
		return fmt.Errorf("mcpgw: 读取 %s 的工具列表失败: %w", spec.Name, err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if old, ok := g.sessions[spec.Name]; ok {
		_ = old.cs.Close()
		g.removeToolsOf(spec.Name)
	}
	g.sessions[spec.Name] = &session{spec: spec, cs: cs}

	for _, t := range tools.Tools {
		if t == nil || t.Name == "" {
			continue
		}
		full := spec.Name + "." + t.Name
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			schema = nil
		}
		info := ToolInfo{
			Name:        full,
			Server:      spec.Name,
			RawName:     t.Name,
			Description: t.Description,
			InputSchema: schema,
			// 副作用等级以用户授权时的声明为准，不采信 server 自报——
			// 让工具自己声明"我是只读的"等于没有管控。
			SideEffect: core.SideEffectIrreversible,
		}
		if grant, err := g.registry.Grant(full); err == nil {
			info.SideEffect = grant.SideEffect
		}
		g.catalog[full] = info
	}
	return nil
}

// Close 关闭全部连接。
func (g *Gateway) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var errs []error
	for name, s := range g.sessions {
		if err := s.cs.Close(); err != nil {
			errs = append(errs, fmt.Errorf("关闭 %s: %w", name, err))
		}
		delete(g.sessions, name)
	}
	g.catalog = make(map[string]ToolInfo)
	return errors.Join(errs...)
}

// Catalog 返回当前可见的工具目录。
func (g *Gateway) Catalog() []ToolInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]ToolInfo, 0, len(g.catalog))
	for _, t := range g.catalog {
		out = append(out, t)
	}
	return out
}

// Lookup 查询单个工具。
func (g *Gateway) Lookup(name string) (ToolInfo, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	t, ok := g.catalog[name]
	if !ok {
		return ToolInfo{}, fmt.Errorf("%w: %s", ErrToolNotFound, name)
	}
	return t, nil
}

// Call 执行一次工具调用。这是全系统唯一的对外副作用出口。
func (g *Gateway) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	info, err := g.Lookup(req.Tool)
	if err != nil {
		return CallResult{}, err
	}

	// 出口管控在调用之前，不在之后。参数会被裁剪成工具声明需要的最小集合。
	trimmed, violations, err := g.guard.CheckOutbound(req.Tool, req.Params)
	if err != nil {
		g.auditCall(ctx, req, core.TrustExternal,
			fmt.Sprintf("被出口管控拦截: %v (%s)", err, joinViolations(violations)))
		return CallResult{}, err
	}

	grant, err := g.registry.Grant(req.Tool)
	if err != nil {
		return CallResult{}, err
	}

	g.mu.RLock()
	s := g.sessions[info.Server]
	g.mu.RUnlock()
	if s == nil {
		return CallResult{}, fmt.Errorf("%w: %s", ErrNotConnected, info.Server)
	}

	// 句柄替换排在出口管控之后：管控看到的是句柄，分级与裁剪都不接触明文；
	// 能不能用这一条由保险库自己的授权名单决定。
	var injection *vault.Injection
	if g.secrets != nil {
		injection, err = g.secrets.Inject(ctx, req.Tool, trimmed)
		if err != nil {
			g.auditCall(ctx, req, core.TrustExternal, fmt.Sprintf("保险库拒绝: %v", err))
			return CallResult{}, err
		}
		trimmed = injection.Params
		if len(injection.Used) > 0 {
			// 审计只记用了哪几条，不记值。
			g.auditCall(ctx, req, core.TrustSystem, "使用保险库条目: "+strings.Join(injection.Used, ", "))
		}
	}

	args, err := decodeArgs(trimmed)
	if err != nil {
		return CallResult{}, err
	}

	params := &mcp.CallToolParams{Name: info.RawName, Arguments: args}
	if req.IdempotencyKey != "" {
		// 幂等键走 _meta，不混进业务参数——否则会污染工具的入参 schema 校验。
		params.Meta = mcp.Meta{"atomhelper/idempotency-key": req.IdempotencyKey}
	}

	res, err := s.cs.CallTool(ctx, params)
	if err != nil {
		g.auditCall(ctx, req, core.TrustExternal, fmt.Sprintf("调用失败: %v", err))
		return CallResult{}, fmt.Errorf("%w: %s: %w", ErrToolFailed, req.Tool, err)
	}
	if res.IsError {
		// 报错信息同样可能回显入参，先清掉明文再往外传。
		msg := redactText(injection, textOf(res))
		g.auditCall(ctx, req, core.TrustExternal, "工具返回错误: "+msg)
		return CallResult{}, fmt.Errorf("%w: %s: %s", ErrToolFailed, req.Tool, msg)
	}

	out, trust, err := normalize(res, grant.TrustCeiling)
	if err != nil {
		return CallResult{}, err
	}
	// 工具常把入参原样回显。明文不清掉就会顺着返回值进步骤日志和下一轮提示词。
	out = injection.Redact(out)
	g.auditCall(ctx, req, trust, fmt.Sprintf("成功，副作用=%s", info.SideEffect))
	return CallResult{Output: out, Trust: trust}, nil
}

// normalize 把 MCP 返回整理成结构化输出，并定出信任等级。
//
// 这里落实"信任是字段级的"：结构化内容可以按工具的信任上限对待，
// 纯文本一律降级为 TrustExternal——工具返回的文本很可能原样转发了
// 第三方撰写的内容（邮件正文、网页片段、别人填的日程标题）。
func normalize(res *mcp.CallToolResult, ceiling core.TrustLevel) (json.RawMessage, core.TrustLevel, error) {
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, core.TrustExternal, fmt.Errorf("mcpgw: 序列化结构化返回失败: %w", err)
		}
		return b, core.EffectiveTrust(ceiling, false), nil
	}
	text := textOf(res)
	b, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, core.TrustExternal, fmt.Errorf("mcpgw: 序列化文本返回失败: %w", err)
	}
	return b, core.EffectiveTrust(ceiling, true), nil
}

// redactText 清掉一段纯文本里的保险库明文。
func redactText(inj *vault.Injection, text string) string {
	if inj == nil {
		return text
	}
	quoted, err := json.Marshal(text)
	if err != nil {
		return "[工具返回内容无法处理，已隐藏]"
	}
	var out string
	if err := json.Unmarshal(inj.Redact(quoted), &out); err != nil {
		return "[工具返回内容含保险库条目，已整体隐藏]"
	}
	return out
}

func textOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func decodeArgs(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("mcpgw: 参数不是合法 JSON: %w", err)
	}
	return v, nil
}

func buildTransport(spec ServerSpec) (mcp.Transport, error) {
	switch spec.Transport {
	case TransportCommand:
		if spec.Command == "" {
			return nil, fmt.Errorf("mcpgw: %s 缺少启动命令", spec.Name)
		}
		cmd := exec.Command(spec.Command, spec.Args...)
		// 只传显式声明的环境变量。把主进程环境整个继承下去，
		// 等于把数据库密码、模型密钥交给第三方工具。
		cmd.Env = append([]string{}, spec.Env...)
		return &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil
	case TransportHTTP:
		if spec.URL == "" {
			return nil, fmt.Errorf("mcpgw: %s 缺少 URL", spec.Name)
		}
		return &mcp.StreamableClientTransport{Endpoint: spec.URL}, nil
	default:
		return nil, fmt.Errorf("mcpgw: 未知的传输方式 %q", spec.Transport)
	}
}

func (g *Gateway) removeToolsOf(server string) {
	for name, info := range g.catalog {
		if info.Server == server {
			delete(g.catalog, name)
		}
	}
}

func (g *Gateway) auditCall(ctx context.Context, req CallRequest, trust core.TrustLevel, detail string) {
	if g.audit == nil {
		return
	}
	// 审计失败不能影响主流程，但必须留下痕迹。
	_ = g.audit.ToolCall(ctx, req.TaskID, req.Tool, trust, detail)
}

func joinViolations(vs []policy.Violation) string {
	if len(vs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, v.String())
	}
	return strings.Join(parts, "; ")
}
