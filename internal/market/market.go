// Package market 让助手在缺少能力时自己去公开的地方找：Skill 库里的 Skill，登记处里的 MCP server。
//
// 这里只管"找"和"取"。取回来的东西一律是别人写的，能不能装由扫描闸门决定（见 Installer），
// 装上之后能做什么由能力边界决定，都不由这里说了算。
//
// 两个来源：
//   - Skill：skills.sh 的搜索接口给出"仓库/Skill 名"，内容从 GitHub 上对应的仓库取。
//   - MCP server：modelcontextprotocol 官方登记处。
//
// 见 docs/MCP市场与Skill.md §5.2。
package market

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultSkillsAPI   = "https://skills.sh/api/search"
	defaultRegistryAPI = "https://registry.modelcontextprotocol.io/v0/servers"
	defaultArchiveBase = "https://codeload.github.com"

	// maxArchiveBytes 是一个仓库压缩包的大小上限。
	maxArchiveBytes = 64 << 20
	// maxSkillBytes 与 maxSkillFiles 限制解出来的单个 Skill 目录。
	maxSkillBytes = 16 << 20
	maxSkillFiles = 400
	// maxResponseBytes 是搜索接口返回内容的大小上限。
	maxResponseBytes = 2 << 20
)

// ErrNotFound 表示公开来源里没有这个东西。
var ErrNotFound = errors.New("market: 没有找到")

// Client 访问公开的 Skill 库与 MCP 登记处。零值可用。
type Client struct {
	// HTTP 为 nil 时用带超时的默认客户端。
	HTTP *http.Client
	// 以下三项留空用公开的默认地址，测试时指向本地。
	SkillsAPI   string
	RegistryAPI string
	ArchiveBase string
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 90 * time.Second}
}

func pick(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// getJSON 取回一段 JSON。
func (c *Client) getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("market: 构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("market: 请求 %s 失败: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("market: %s 返回 %s", req.URL.Host, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("market: 读取 %s 的返回失败: %w", req.URL.Host, err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("market: %s 的返回超过 %d 字节", req.URL.Host, maxResponseBytes)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("market: %s 的返回不是合法 JSON: %w", req.URL.Host, err)
	}
	return nil
}

// SkillHit 是 Skill 库里的一条搜索结果。
type SkillHit struct {
	// ID 形如 "owner/repo/skill"，安装时原样传回。
	ID       string `json:"id"`
	Name     string `json:"name"`
	Installs int64  `json:"installs"`
}

var reSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// splitSkillID 把 "owner/repo/skill" 拆开。三段都只接受 GitHub 允许的字符——
// 它们会被拼进下载地址，不合规的一律不认。
func splitSkillID(id string) (owner, repo, name string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("market: Skill 标识 %q 应为 owner/repo/skill", id)
	}
	for _, p := range parts {
		if !reSegment.MatchString(p) || strings.Contains(p, "..") {
			return "", "", "", fmt.Errorf("market: Skill 标识 %q 含不允许的字符", id)
		}
	}
	return parts[0], parts[1], parts[2], nil
}

// SearchSkills 在公开的 Skill 库里搜索。只返回内容放在 GitHub 仓库里的那些——别处的我们取不回来。
func (c *Client) SearchSkills(ctx context.Context, query string, limit int) ([]SkillHit, error) {
	u := pick(c.SkillsAPI, defaultSkillsAPI) + "?" + url.Values{
		"q": {query}, "limit": {fmt.Sprint(limit)},
	}.Encode()
	var body struct {
		Skills []SkillHit `json:"skills"`
	}
	if err := c.getJSON(ctx, u, &body); err != nil {
		return nil, err
	}
	out := make([]SkillHit, 0, len(body.Skills))
	for _, h := range body.Skills {
		if _, _, _, err := splitSkillID(h.ID); err != nil {
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// FetchSkill 把一个 Skill 的整个目录取到 dest 下，返回这个目录的路径。
//
// 取的是仓库默认分支的压缩包，从中只解出含 SKILL.md 且目录名与 Skill 名相同的那个目录。
// 整个目录都要落盘：扫描的对象是整个 Skill，不只是那篇说明。
func (c *Client) FetchSkill(ctx context.Context, id, dest string) (string, error) {
	owner, repo, name, err := splitSkillID(id)
	if err != nil {
		return "", err
	}
	u := fmt.Sprintf("%s/%s/%s/tar.gz/HEAD", pick(c.ArchiveBase, defaultArchiveBase), owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("market: 构造请求失败: %w", err)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", fmt.Errorf("market: 下载 %s/%s 失败: %w", owner, repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: 仓库 %s/%s", ErrNotFound, owner, repo)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("market: 下载 %s/%s 返回 %s", owner, repo, resp.Status)
	}

	// 先整包落到临时文件：要找的目录在包里的位置事先不知道，得过两遍。
	tmp, err := os.CreateTemp(dest, "archive-*.tgz")
	if err != nil {
		return "", fmt.Errorf("market: 创建临时文件失败: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return "", fmt.Errorf("market: 下载 %s/%s 失败: %w", owner, repo, err)
	}
	if n > maxArchiveBytes {
		return "", fmt.Errorf("market: 仓库 %s/%s 的压缩包超过 %d MB", owner, repo, maxArchiveBytes>>20)
	}

	root, err := locateSkill(tmp, name)
	if err != nil {
		return "", err
	}
	out := filepath.Join(dest, name)
	if err := extractDir(tmp, root, out); err != nil {
		return "", err
	}
	return out, nil
}

// eachEntry 从头遍历压缩包里的每一项。
func eachEntry(f *os.File, fn func(h *tar.Header, r io.Reader) error) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("market: 读取压缩包失败: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("market: 压缩包格式不对: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("market: 压缩包格式不对: %w", err)
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

// locateSkill 在压缩包里找出名为 name 的 Skill 目录（包内路径，以 / 结尾）。
// 同名目录不止一个时取路径最短的。
func locateSkill(f *os.File, name string) (string, error) {
	best := ""
	err := eachEntry(f, func(h *tar.Header, _ io.Reader) error {
		p := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || !strings.EqualFold(path.Base(p), "SKILL.md") {
			return nil
		}
		dir := path.Dir(p)
		if path.Base(dir) != name {
			return nil
		}
		if best == "" || len(dir) < len(best) {
			best = dir
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if best == "" {
		return "", fmt.Errorf("%w: 仓库里没有名为 %s 的 Skill", ErrNotFound, name)
	}
	return best + "/", nil
}

// extractDir 把压缩包里 root 之下的普通文件解到 out。
//
// 只解普通文件。符号链接、设备之类一概跳过；路径里带 .. 的也跳过——
// 压缩包是别人做的，不能让它把文件写到 out 外面去。
func extractDir(f *os.File, root, out string) error {
	var files int
	var total int64
	return eachEntry(f, func(h *tar.Header, r io.Reader) error {
		p := path.Clean(h.Name)
		rel, ok := strings.CutPrefix(p, root)
		if !ok || rel == "" || h.Typeflag != tar.TypeReg {
			return nil
		}
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return nil
		}
		files++
		total += h.Size
		if files > maxSkillFiles || total > maxSkillBytes {
			return fmt.Errorf("market: 这个 Skill 太大（超过 %d 个文件或 %d MB），不安装", maxSkillFiles, maxSkillBytes>>20)
		}
		target := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("market: 创建目录失败: %w", err)
		}
		w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("market: 写入文件失败: %w", err)
		}
		_, err = io.Copy(w, io.LimitReader(r, h.Size))
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("market: 写入文件失败: %w", err)
		}
		return nil
	})
}

// ServerHit 是登记处里的一个 MCP server。
type ServerHit struct {
	// Name 是它在登记处的名字，安装时原样传回。
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
	// URL 是可以直接连接的远程地址。
	URL string `json:"url,omitempty"`
	// Installable 为假时 Reason 说明为什么装不了。
	Installable bool   `json:"installable"`
	Reason      string `json:"reason,omitempty"`

	// raw 是登记处给出的条目原文，交给扫描器的就是它。
	raw json.RawMessage
}

// registryEntry 是登记处返回的一条记录里我们用到的字段。
type registryEntry struct {
	Server struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Remotes     []struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Headers []struct {
				Name       string `json:"name"`
				IsRequired bool   `json:"isRequired"`
			} `json:"headers"`
		} `json:"remotes"`
	} `json:"server"`
	Meta map[string]struct {
		Status   string `json:"status"`
		IsLatest bool   `json:"isLatest"`
	} `json:"_meta"`
}

// hit 把一条登记记录整理成搜索结果，并判断我们能不能装它。
//
// 目前只装"可以直接连、不需要密钥"的远程 server：
//   - 要在本机运行的（npm、PyPI 包）不装。那是在保险库旁边运行别人的代码，
//     没有隔离环境之前不能交给助手自己决定。
//   - 需要密钥的不装。密钥得用户亲手放进保险库并授权，助手替不了。
func (e registryEntry) hit(raw json.RawMessage) ServerHit {
	h := ServerHit{Name: e.Server.Name, Title: e.Server.Title, Description: e.Server.Description, raw: raw}
	if len(e.Server.Remotes) == 0 {
		h.Reason = "它要在本机运行，助手不能自己安装，需要你在配置里添加"
		return h
	}
	h.Reason = "它需要密钥，助手不能自己安装，需要你在配置里添加并把密钥放进保险库"
	for _, r := range e.Server.Remotes {
		if r.Type != "streamable-http" {
			h.Reason = "它的连接方式不受支持"
			continue
		}
		needsKey := false
		for _, hd := range r.Headers {
			if hd.IsRequired {
				needsKey = true
			}
		}
		// 地址里带 {占位符} 的同样要用户填东西才连得上。
		if needsKey || strings.ContainsAny(r.URL, "{}") {
			continue
		}
		u, err := url.Parse(r.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			h.Reason = "它的地址不是 https"
			continue
		}
		h.URL, h.Installable, h.Reason = r.URL, true, ""
		return h
	}
	return h
}

func (e registryEntry) current() bool {
	for _, m := range e.Meta {
		if !m.IsLatest || (m.Status != "" && m.Status != "active") {
			return false
		}
	}
	return true
}

// SearchServers 在 MCP 登记处搜索，只返回每个 server 当前有效的那一版。
func (c *Client) SearchServers(ctx context.Context, query string, limit int) ([]ServerHit, error) {
	u := pick(c.RegistryAPI, defaultRegistryAPI) + "?" + url.Values{
		"search": {query}, "limit": {fmt.Sprint(limit)},
	}.Encode()
	var body struct {
		Servers []json.RawMessage `json:"servers"`
	}
	if err := c.getJSON(ctx, u, &body); err != nil {
		return nil, err
	}
	out := make([]ServerHit, 0, len(body.Servers))
	for _, raw := range body.Servers {
		var e registryEntry
		if err := json.Unmarshal(raw, &e); err != nil || e.Server.Name == "" || !e.current() {
			continue
		}
		out = append(out, e.hit(raw))
	}
	return out, nil
}

// LookupServer 按登记处里的名字找到一个 server。
func (c *Client) LookupServer(ctx context.Context, name string) (ServerHit, error) {
	hits, err := c.SearchServers(ctx, name, 30)
	if err != nil {
		return ServerHit{}, err
	}
	for _, h := range hits {
		if h.Name == name {
			return h, nil
		}
	}
	return ServerHit{}, fmt.Errorf("%w: 登记处里没有名为 %s 的 MCP server", ErrNotFound, name)
}
