package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	Check(ctx context.Context, path string) (skillscan.Report, error)
	CheckRegistry(ctx context.Context, path string) (skillscan.Report, error)
}

// Installer 把"找到的东西"变成"装上的东西"：取回、扫描、存下。
//
// 扫描是必经的一步，没有任何参数能跳过它；扫描没通过或没跑成，什么都不会留下。
// 装上的东西会一直留着，以后的任务可以直接用。
type Installer struct {
	Client  *Client
	Scanner Scanner
	Skills  skill.Store
	Servers ServerStore
	// Connect 连接一个刚通过扫描的 server，返回它提供的工具名。连不上就不算装上。
	Connect func(ctx context.Context, s *Server) ([]string, error)
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

// InstallSkill 从公开 Skill 库取回一个 Skill，扫描通过后存下。
//
// 扫描的是整个目录，存下的只有那篇 SKILL.md：目录里的脚本我们不会执行，也就不留。
func (i *Installer) InstallSkill(ctx context.Context, id string) (*skill.Skill, error) {
	if i.Scanner == nil {
		return nil, fmt.Errorf("%w: 没有配置扫描器", skillscan.ErrScanFailed)
	}
	work, err := os.MkdirTemp("", "atom-skill-*")
	if err != nil {
		return nil, fmt.Errorf("market: 创建临时目录失败: %w", err)
	}
	// 临时目录里是下载的原始文件。装上的内容已经进了库，这里不必留。
	defer os.RemoveAll(work)

	dir, err := i.Client.FetchSkill(ctx, id, work)
	if err != nil {
		return nil, err
	}
	rep, err := i.Scanner.Check(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, tail(rep.Output, 1500))
	}
	raw, err := readSkillFile(dir)
	if err != nil {
		return nil, err
	}
	s, err := skill.ParseMarkdown(raw, SkillID(id))
	if err != nil {
		return nil, err
	}
	s.Source = "skills.sh:" + id
	s.ScanReport = rep.Output
	if err := skill.Install(ctx, i.Skills, s); err != nil {
		return nil, err
	}
	return s, nil
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

// InstallServer 从登记处取回一个 MCP server 的条目，扫描通过并连上之后存下。返回它提供的工具名。
func (i *Installer) InstallServer(ctx context.Context, registryName string) (*Server, []string, error) {
	if i.Scanner == nil {
		return nil, nil, fmt.Errorf("%w: 没有配置扫描器", skillscan.ErrScanFailed)
	}
	if i.Connect == nil || i.Servers == nil {
		return nil, nil, fmt.Errorf("market: 未启用 MCP server 的安装")
	}
	hit, err := i.Client.LookupServer(ctx, registryName)
	if err != nil {
		return nil, nil, err
	}
	if !hit.Installable {
		return nil, nil, fmt.Errorf("market: %s 装不了：%s", hit.Name, hit.Reason)
	}

	// 交给扫描器的是登记处返回的那种格式：一个 servers 列表。
	payload, err := json.Marshal(map[string][]json.RawMessage{"servers": {hit.raw}})
	if err != nil {
		return nil, nil, fmt.Errorf("market: 序列化登记条目失败: %w", err)
	}
	work, err := os.MkdirTemp("", "atom-mcp-*")
	if err != nil {
		return nil, nil, fmt.Errorf("market: 创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(work)
	file := filepath.Join(work, "registry.json")
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		return nil, nil, fmt.Errorf("market: 写入临时文件失败: %w", err)
	}
	rep, err := i.Scanner.CheckRegistry(ctx, file)
	if err != nil {
		return nil, nil, fmt.Errorf("%w\n%s", err, tail(rep.Output, 1500))
	}

	srv := &Server{
		Name: ServerName(hit.Name), URL: hit.URL, RegistryName: hit.Name, Description: hit.Description,
		Payload: string(payload), ScanReport: rep.Output, InstalledAt: time.Now().UTC(),
	}
	tools, err := i.Connect(ctx, srv)
	if err != nil {
		return nil, nil, err
	}
	if err := i.Servers.PutServer(ctx, srv); err != nil {
		return nil, nil, err
	}
	return srv, tools, nil
}

// tail 取一段文字的末尾：扫描报告的结论在最后。
func tail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "…" + strings.ToValidUTF8(s[len(s)-max:], "")
}
