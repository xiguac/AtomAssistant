package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xiguac/atomhelper/internal/skill"
	"github.com/xiguac/atomhelper/internal/skillscan"
)

// Server 是助手自己装上的一个远程 MCP server。
type Server struct {
	// Name 是它在本机的名字，也是它的工具名的前缀。
	Name string
	URL  string
	// RegistryName 是它在登记处的名字。
	RegistryName string
	Description  string
	// Payload 是安装时登记处给出的条目原文，ScanReport 是安装前的扫描报告。
	Payload     string
	ScanReport  string
	InstalledAt time.Time
}

// ServerStore 是助手安装的 MCP server 的持久化接口。
type ServerStore interface {
	PutServer(ctx context.Context, s *Server) error
	ListServers(ctx context.Context) ([]*Server, error)
	RemoveServer(ctx context.Context, name string) error
}

// ErrNotInstalled 表示没有安装这个 MCP server。
var ErrNotInstalled = errors.New("market: 没有安装这个 MCP server")

// Scanner 是安装前的扫描闸门，由 skillscan.Scanner 实现。
type Scanner interface {
	Inspect(ctx context.Context, path string) (skillscan.Report, error)
	CheckRegistry(ctx context.Context, path string) (skillscan.Report, error)
}

// ToolBrief 是一个 MCP server 上某个工具的概况，用来判断风险。
type ToolBrief struct {
	Name string
	// ReadOnly 为真表示 server 自称这个工具只读。
	ReadOnly bool
	// Params 是它接收的参数名。
	Params []string
}

// Installer 把"找到的东西"变成"装上的东西"：取回、扫描、定风险、存下。
//
// 扫描是必经的一步，没有任何参数能跳过它；扫描没通过或没跑成，什么都不会留下。
// 通过扫描的再分两档：
//   - 低风险的直接装上；
//   - 会把本机的信息传到外面去的，先搁着（见 Pending），列出风险点，等用户批准了才装。
//
// 装上的东西会一直留着，以后的任务可以直接用。
type Installer struct {
	Client  *Client
	Scanner Scanner
	Skills  skill.Store
	Servers ServerStore
	// Probe 连上一个 server 看它有哪些工具，看完就断开，什么都不登记。
	Probe func(ctx context.Context, s *Server) ([]ToolBrief, error)
	// Connect 连接一个 server 并登记它的工具，返回工具全名。连不上就不算装上。
	Connect func(ctx context.Context, s *Server) ([]string, error)

	mu      sync.Mutex
	pending map[string]*Pending
}

// Pending 是一次因为有风险而搁置的安装。
//
// 搁置的是当时取回并扫描过的那一份：用户批准的是他看到风险点的那份内容，
// 批准之后装上的也必须是它，而不是重新取一次。
type Pending struct {
	// Kind 是 "skill" 或 "mcp"，ID 是搜索结果里的标识。
	Kind, ID string
	// Risks 是要告诉用户的风险点，每条一句话。
	Risks []string

	skill  *skill.Skill
	server *Server
}

func pendingKey(kind, id string) string { return kind + "\x00" + id }

func (i *Installer) stage(p *Pending) *Pending {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pending == nil {
		i.pending = make(map[string]*Pending)
	}
	i.pending[pendingKey(p.Kind, p.ID)] = p
	return p
}

// take 取出并移除一次搁置的安装。
func (i *Installer) take(kind, id string) (*Pending, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	p := i.pending[pendingKey(kind, id)]
	if p == nil {
		return nil, fmt.Errorf("market: %s 没有等待批准的安装，先不带 accept_risk 安装一次，让系统评估风险", id)
	}
	delete(i.pending, pendingKey(kind, id))
	return p, nil
}

// PendingRisks 返回一次搁置的安装的风险点，没有搁置则返回 nil。审批时展示给用户的就是它。
func (i *Installer) PendingRisks(kind, id string) []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if p := i.pending[pendingKey(kind, id)]; p != nil {
		return append([]string(nil), p.Risks...)
	}
	return nil
}

// SearchSkills 在公开的 Skill 库里搜索。
func (i *Installer) SearchSkills(ctx context.Context, query string, limit int) ([]SkillHit, error) {
	return i.Client.SearchSkills(ctx, query, limit)
}

// SearchServers 在 MCP 登记处搜索。
func (i *Installer) SearchServers(ctx context.Context, query string, limit int) ([]ServerHit, error) {
	return i.Client.SearchServers(ctx, query, limit)
}

// SkillID 把公开 Skill 库里的标识（owner/repo/skill）换成本机的 Skill ID。
func SkillID(id string) string {
	return strings.ToLower(strings.ReplaceAll(id, "/", "."))
}

// ServerName 把登记处里的名字换成本机的 server 名。
//
// 统一加 mcp- 前缀：工具全名是"server 名.工具名"，不加前缀的话，
// 一个自称 mail 的 server 会让它的工具看起来像内建的邮件工具。
func ServerName(registryName string) string {
	var sb strings.Builder
	sb.WriteString("mcp-")
	for _, r := range strings.ToLower(registryName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	return sb.String()
}

// InstallSkill 从公开 Skill 库取回一个 Skill，扫描通过后按风险处理。
//
// 低风险的直接存下，返回 Skill。有外传风险的搁置起来，返回 Pending；
// 之后以 accept 为真再调一次（调用方必须先取得用户批准），装上的就是搁置的那一份。
//
// 扫描的是整个目录，存下的只有那篇 SKILL.md：目录里的脚本我们不会执行，也就不留。
func (i *Installer) InstallSkill(ctx context.Context, id string, accept bool) (*skill.Skill, *Pending, error) {
	if accept {
		p, err := i.take("skill", id)
		if err != nil {
			return nil, nil, err
		}
		if err := skill.Install(ctx, i.Skills, p.skill); err != nil {
			return nil, nil, err
		}
		return p.skill, nil, nil
	}
	if i.Scanner == nil {
		return nil, nil, fmt.Errorf("%w: 没有配置扫描器", skillscan.ErrScanFailed)
	}
	work, err := os.MkdirTemp("", "atom-skill-*")
	if err != nil {
		return nil, nil, fmt.Errorf("market: 创建临时目录失败: %w", err)
	}
	// 临时目录里是下载的原始文件。装上的内容已经进了库，这里不必留。
	defer os.RemoveAll(work)

	dir, err := i.Client.FetchSkill(ctx, id, work)
	if err != nil {
		return nil, nil, err
	}
	rep, err := i.Scanner.Inspect(ctx, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("%w\n%s", err, summarize(rep))
	}
	findings, err := rep.Findings()
	if err != nil {
		return nil, nil, err
	}
	raw, err := readSkillFile(dir)
	if err != nil {
		return nil, nil, err
	}
	s, err := skill.ParseMarkdown(raw, SkillID(id))
	if err != nil {
		return nil, nil, err
	}
	s.Source = "skills.sh:" + id
	s.ScanReport = rep.Output

	if risks := skillRisks(findings); len(risks) > 0 {
		return nil, i.stage(&Pending{Kind: "skill", ID: id, Risks: risks, skill: s}), nil
	}
	if err := skill.Install(ctx, i.Skills, s); err != nil {
		return nil, nil, err
	}
	return s, nil, nil
}

// maxRisks 是一次安装列给用户的风险点条数上限。
const maxRisks = 8

// skillRisks 从扫描的发现里挑出要用户拿主意的那些。
//
// 要用户批准的是两类：把数据传到外面去的，和扫描器判为高危的。
// 其余的（权限写得宽、依赖没锁版本之类）不拦——Skill 装上之后只是一篇说明，
// 不带来任何工具，这些问题落不到实处。
func skillRisks(findings []skillscan.Finding) []string {
	var risks []string
	for _, f := range findings {
		if !f.Outbound() && !f.Severe() {
			continue
		}
		if len(risks) == maxRisks {
			risks = append(risks, "（还有更多，未列出）")
			break
		}
		kind := "高危内容（" + f.Category + "）"
		if f.Outbound() {
			kind = "会把数据传到外部"
		}
		line := kind
		if f.Where != "" {
			line += "，" + f.Where
		}
		if f.Text != "" {
			line += "：" + clip(f.Text, 160)
		}
		risks = append(risks, line)
	}
	return risks
}

func readSkillFile(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("market: 读取 Skill 目录失败: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), skill.MarkdownName) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("market: 读取 %s 失败: %w", e.Name(), err)
			}
			return raw, nil
		}
	}
	return nil, fmt.Errorf("market: 取回的目录里没有 %s", skill.MarkdownName)
}

// InstallServer 从登记处取回一个 MCP server 的条目，扫描通过后按风险处理。
//
// 低风险的连上并存下，返回它和它的工具名。有外传风险的搁置起来，返回 Pending；
// 之后以 accept 为真再调一次（调用方必须先取得用户批准）。
func (i *Installer) InstallServer(ctx context.Context, registryName string, accept bool) (*Server, []string, *Pending, error) {
	if i.Connect == nil || i.Probe == nil || i.Servers == nil {
		return nil, nil, nil, fmt.Errorf("market: 未启用 MCP server 的安装")
	}
	if accept {
		p, err := i.take("mcp", registryName)
		if err != nil {
			return nil, nil, nil, err
		}
		tools, err := i.commit(ctx, p.server)
		return p.server, tools, nil, err
	}
	if i.Scanner == nil {
		return nil, nil, nil, fmt.Errorf("%w: 没有配置扫描器", skillscan.ErrScanFailed)
	}
	hit, err := i.Client.LookupServer(ctx, registryName)
	if err != nil {
		return nil, nil, nil, err
	}
	if !hit.Installable {
		return nil, nil, nil, fmt.Errorf("market: %s 装不了：%s", hit.Name, hit.Reason)
	}

	// 交给扫描器的是登记处返回的那种格式：一个 servers 列表。
	payload, err := json.Marshal(map[string][]json.RawMessage{"servers": {hit.raw}})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("market: 序列化登记条目失败: %w", err)
	}
	work, err := os.MkdirTemp("", "atom-mcp-*")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("market: 创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(work)
	file := filepath.Join(work, "registry.json")
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		return nil, nil, nil, fmt.Errorf("market: 写入临时文件失败: %w", err)
	}
	rep, err := i.Scanner.CheckRegistry(ctx, file)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w\n%s", err, summarize(rep))
	}
	findings, err := rep.Findings()
	if err != nil {
		return nil, nil, nil, err
	}

	srv := &Server{
		Name: ServerName(hit.Name), URL: hit.URL, RegistryName: hit.Name, Description: hit.Description,
		Payload: string(payload), ScanReport: rep.Output, InstalledAt: time.Now().UTC(),
	}
	// 风险要看它实际提供什么工具，登记信息里没有，得连上去问。
	briefs, err := i.Probe(ctx, srv)
	if err != nil {
		return nil, nil, nil, err
	}
	if risks := serverRisks(findings, briefs); len(risks) > 0 {
		return nil, nil, i.stage(&Pending{Kind: "mcp", ID: registryName, Risks: risks, server: srv}), nil
	}
	tools, err := i.commit(ctx, srv)
	return srv, tools, nil, err
}

// commit 连接并存下一个 server。
func (i *Installer) commit(ctx context.Context, srv *Server) ([]string, error) {
	tools, err := i.Connect(ctx, srv)
	if err != nil {
		return nil, err
	}
	if err := i.Servers.PutServer(ctx, srv); err != nil {
		return nil, err
	}
	return tools, nil
}

// serverRisks 判断一个远程 MCP server 会不会把本机的信息传到外面去。
//
// 调它的任何工具，参数都会发到它那里。只读的查询工具收到的是查询条件（城市、日期），
// 这一档按低风险对待；不自称只读的工具是用来提交内容的（发布、上传、发送），
// 交给它的东西就留在了外面——这一档要用户批准。
// 扫描器对登记信息有提示以上的发现，同样要用户批准。
func serverRisks(findings []skillscan.Finding, tools []ToolBrief) []string {
	var risks []string
	for _, f := range findings {
		if f.Severity == "" || f.Severity == "INFO" {
			continue
		}
		risks = append(risks, "登记信息有疑点（"+strings.ToLower(f.Severity)+"）："+clip(f.Text, 160))
	}
	for _, t := range tools {
		if t.ReadOnly {
			continue
		}
		if len(risks) >= maxRisks {
			risks = append(risks, "（还有更多，未列出）")
			break
		}
		line := "工具 " + t.Name + " 会把你交给它的内容提交到这个外部服务"
		if len(t.Params) > 0 {
			line += "，它接收的参数：" + clip(strings.Join(t.Params, "、"), 120)
		}
		risks = append(risks, line)
	}
	return risks
}

// summarize 给出扫描报告里值得带回给模型的部分：有结构化的发现就列发现，否则取末尾。
func summarize(rep skillscan.Report) string {
	findings, err := rep.Findings()
	if err != nil || len(findings) == 0 {
		return tail(rep.Output, 1500)
	}
	var sb strings.Builder
	for n, f := range findings {
		if n == maxRisks {
			break
		}
		fmt.Fprintf(&sb, "- [%s] %s %s %s\n", f.Severity, f.Category, f.Where, clip(f.Text, 160))
	}
	return sb.String()
}

func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// tail 取一段文字的末尾：扫描报告的结论在最后。
func tail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "…" + strings.ToValidUTF8(s[len(s)-max:], "")
}
