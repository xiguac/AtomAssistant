package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/xiguac/atomhelper/internal/core"
)

var (
	// ErrSecretBlocked 表示参数里含有 L3 绝密数据。这条没有任何开关。
	ErrSecretBlocked = errors.New("policy: 绝密数据永不传出实例")
	// ErrPrivacyExceeded 表示参数隐私等级超出该工具的授权范围。
	ErrPrivacyExceeded = errors.New("policy: 参数隐私等级超出工具授权")
	// ErrEgressBlocked 表示出站目标不在白名单内。
	ErrEgressBlocked = errors.New("policy: 出站目标不在白名单内")
	// ErrToolUnknown 表示工具未注册或未被授权。
	ErrToolUnknown = errors.New("policy: 工具未注册")
)

// ToolGrant 是一个工具被授予的能力边界，在用户批准安装/授权时确定。
type ToolGrant struct {
	Name string
	// MaxPrivacy 是允许传给这个工具的最高隐私等级。
	// 绝不允许设为 PrivacySecret——L3 数据不传给任何工具。
	MaxPrivacy core.PrivacyLevel
	// TrustCeiling 是这个工具输出内容的信任上限。
	TrustCeiling core.TrustLevel
	// SideEffect 是这个工具声明的副作用等级。
	SideEffect core.SideEffect
	// EgressDomains 是允许的出站域名。空表示不允许任何网络出站。
	EgressDomains []string
	// AllowedFields 是允许传入的参数字段白名单（点号路径）。
	// 空表示不做字段白名单限制，只做隐私分级检查。
	AllowedFields []string
}

// Registry 保存已授权工具的能力边界。
type Registry struct {
	// 助手运行期间可以装上新的 MCP server，登记与查询会同时发生。
	mu     sync.RWMutex
	grants map[string]ToolGrant
}

// NewRegistry 构造工具注册表。
func NewRegistry() *Registry {
	return &Registry{grants: make(map[string]ToolGrant)}
}

// Register 登记一个已被用户授权的工具。
//
// MaxPrivacy 被强制压到 L2：任何工具都拿不到 L3 数据，这是硬约束。
func (r *Registry) Register(g ToolGrant) error {
	if g.Name == "" {
		return fmt.Errorf("policy: 工具名不能为空")
	}
	if g.SideEffect == "" {
		return fmt.Errorf("policy: 工具 %s 未声明副作用等级", g.Name)
	}
	if g.MaxPrivacy >= core.PrivacySecret {
		g.MaxPrivacy = core.PrivacySensitive
	}
	for _, d := range g.EgressDomains {
		if err := validateEgressDomain(d); err != nil {
			return fmt.Errorf("policy: 工具 %s 的出站域名不合法: %w", g.Name, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grants[g.Name] = g
	return nil
}

// Grant 返回工具的能力边界。
func (r *Registry) Grant(name string) (ToolGrant, error) {
	r.mu.RLock()
	g, ok := r.grants[name]
	r.mu.RUnlock()
	if !ok {
		return ToolGrant{}, fmt.Errorf("%w: %s", ErrToolUnknown, name)
	}
	return g, nil
}

// Guard 是所有对外调用的必经出口。任何模块都不得绕过它直接发包。
type Guard struct {
	classifier *Classifier
	registry   *Registry
}

// NewGuard 构造出口守卫。
func NewGuard(c *Classifier, r *Registry) *Guard {
	return &Guard{classifier: c, registry: r}
}

// Violation 描述一次被拦下的字段。
type Violation struct {
	Path  string
	Level core.PrivacyLevel
	Why   string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s(%s): %s", v.Path, v.Level, v.Why)
}

// CheckOutbound 在调用工具之前检查参数。
//
// 返回裁剪后的参数：只保留工具声明需要的字段。查天气只需要城市名，
// 不需要传完整日程对象；叫车只需要起终点，不需要传客户姓名。
func (g *Guard) CheckOutbound(toolName string, params json.RawMessage) (json.RawMessage, []Violation, error) {
	grant, err := g.registry.Grant(toolName)
	if err != nil {
		return nil, nil, err
	}

	fields, _, err := g.classifier.ClassifyJSON(params)
	if err != nil {
		return nil, nil, err
	}

	var violations []Violation
	for _, f := range fields {
		switch {
		case f.Level.NeverLeavesInstance():
			violations = append(violations, Violation{f.Path, f.Level, "绝密数据，永不传出实例"})
		case f.Level > grant.MaxPrivacy:
			violations = append(violations, Violation{f.Path, f.Level,
				fmt.Sprintf("超出工具 %s 的授权上限 %s", toolName, grant.MaxPrivacy)})
		}
	}
	if len(violations) > 0 {
		// L3 与越权分别对应不同的处置：前者永远拒绝，后者可以请求用户单次授权。
		for _, v := range violations {
			if v.Level.NeverLeavesInstance() {
				return nil, violations, fmt.Errorf("%w: %s", ErrSecretBlocked, v.Path)
			}
		}
		return nil, violations, fmt.Errorf("%w: %s", ErrPrivacyExceeded, violations[0].Path)
	}

	trimmed, err := trimFields(params, grant.AllowedFields)
	if err != nil {
		return nil, nil, err
	}
	return trimmed, nil, nil
}

// CheckEgressTarget 校验一次出站请求的目标地址。
//
// 除了白名单，还要挡住内网与云元数据地址——个人服务器上云时，
// 169.254.169.254 是经典的逃逸点。
func (g *Guard) CheckEgressTarget(toolName, rawURL string) error {
	grant, err := g.registry.Grant(toolName)
	if err != nil {
		return err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("policy: 出站地址不合法: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("%w: 不支持的协议 %q", ErrEgressBlocked, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: 缺少主机名", ErrEgressBlocked)
	}
	if isPrivateHost(host) {
		return fmt.Errorf("%w: 禁止访问内网或元数据地址 %s", ErrEgressBlocked, host)
	}
	for _, allowed := range grant.EgressDomains {
		if hostMatches(host, allowed) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s 未声明可访问 %s", ErrEgressBlocked, toolName, host)
}

// trimFields 只保留白名单字段。白名单为空时原样返回。
func trimFields(params json.RawMessage, allowed []string) (json.RawMessage, error) {
	if len(allowed) == 0 || len(params) == 0 {
		return params, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(params, &obj); err != nil {
		// 顶层不是对象时无法按字段裁剪，原样返回——
		// 隐私检查已经在上一步做过了。
		return params, nil
	}
	keep := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		keep[a] = true
	}
	out := make(map[string]any, len(keep))
	for k, v := range obj {
		if keep[k] {
			out[k] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("policy: 裁剪参数失败: %w", err)
	}
	return b, nil
}

func validateEgressDomain(d string) error {
	if d == "" {
		return fmt.Errorf("域名为空")
	}
	if strings.Contains(d, "/") || strings.Contains(d, ":") {
		return fmt.Errorf("应为纯域名，不含协议或端口: %q", d)
	}
	// 通配符只允许出现在最左侧一级，避免 "*" 这种等于不限制的写法。
	if strings.HasPrefix(d, "*.") {
		if strings.Count(d, ".") < 2 {
			return fmt.Errorf("通配域名过宽: %q", d)
		}
		return nil
	}
	if strings.Contains(d, "*") {
		return fmt.Errorf("通配符只能出现在最左侧一级: %q", d)
	}
	return nil
}

func hostMatches(host, pattern string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	pattern = strings.ToLower(pattern)
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // 保留前导的点，"*.a.com" → ".a.com"
		return strings.HasSuffix(host, suffix)
	}
	return host == pattern
}

// isPrivateHost 判断主机是否指向内网、回环或云元数据地址。
func isPrivateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return true
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// 云元数据服务，个人服务器上云时的经典逃逸点。
	if ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("100.100.100.200")) {
		return true
	}
	return false
}
