// Package share 给工作区里的文件建临时下载链接，让助手能把自己生成的文件交给别的服务或别的人。
//
// 几条不让步的规则：
//
//  1. **只分享助手生成的文件。** 用户放进来的文件不能分享，用户批准后复制出的副本可以，见 files.Shareable。
//  2. **最长 24 小时。** 到期自动失效；用户或助手可以随时提前撤销。
//  3. **链接里只有一个随机令牌。** 256 位，库里只存它的摘要。下载时 URL 里的路径只用来在建链接时
//     记下的文件清单里查找，从来不拿去拼磁盘路径——路径穿越在这里无从谈起。
//  4. **分享的是建链接时的那一版。** 文件之后被改了、换了，链接就下载不到它。
//  5. **不认识的令牌、过期的、撤销的，一律 404**，不告诉对方是哪一种。
package share

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/files"
)

const (
	// MaxTTL 是链接最长的有效期。
	MaxTTL = 24 * time.Hour
	// DefaultTTL 是不指定有效期时的默认值。
	DefaultTTL = time.Hour
	// MaxActive 是同时有效的链接数上限：对外敞开的东西越少越好。
	MaxActive = 50
	// tokenBytes 是令牌的随机字节数。
	tokenBytes = 32
)

var (
	// ErrInvalid 表示参数不合法。
	ErrInvalid = errors.New("share: 参数不合法")
	// ErrNotFound 表示没有这个链接，或者它已经失效。
	ErrNotFound = errors.New("share: 链接不存在或已失效")
)

// Link 是一个下载链接。令牌本身不在这里：它只在建链接的那一刻出现一次。
type Link struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id,omitempty"`
	// Path 是分享的文件或目录在工作区里的路径；Dir 为真表示是目录。
	Path      string         `json:"path"`
	Dir       bool           `json:"dir,omitempty"`
	Files     []files.Pinned `json:"files"`
	CreatedAt time.Time      `json:"created_at"`
	ExpiresAt time.Time      `json:"expires_at"`
}

// Store 存放链接，由 *pg.Store 提供。
type Store interface {
	// CreateShare 存下一个链接。tokenHash 是令牌的摘要。
	CreateShare(ctx context.Context, l *Link, tokenHash string) error
	// ShareByToken 按令牌摘要找一个此刻有效（没过期、没撤销）的链接，没有返回 ErrNotFound。
	ShareByToken(ctx context.Context, tokenHash string, now time.Time) (*Link, error)
	// ActiveShares 列出此刻有效的链接，新的在前。
	ActiveShares(ctx context.Context, now time.Time) ([]*Link, error)
	// RevokeShare 撤销一个链接，返回它此前是否有效。
	RevokeShare(ctx context.Context, id string, now time.Time) (bool, error)
	// DeleteExpiredShares 删掉 before 之前就已经失效的链接。
	DeleteExpiredShares(ctx context.Context, before time.Time) error
}

// Workspace 是分享要用到的工作区能力，由 *files.Workspace 提供。
type Workspace interface {
	Shareable(rel string) (norm string, dir bool, pinned []files.Pinned, err error)
	SharedBytes(p files.Pinned) ([]byte, error)
	CopyReceived(rel string) (string, error)
}

// BaseURLSetting 是外部访问地址在后台设置里的名字。地址由用户在管理后台「文件」页填写。
const BaseURLSetting = "share.base_url"

// Settings 读后台设置，由 *pg.Store 提供。
type Settings interface {
	Setting(ctx context.Context, key string) (string, error)
}

// Service 建、列、撤销链接，并提供下载入口（见 http.go）。
type Service struct {
	Store     Store
	Workspace Workspace
	// Settings 里有外部访问地址（BaseURLSetting），如 https://atom.example.com/download。
	// 每次建链接时现读：用户在后台改了地址，不必重启。
	Settings Settings
	// now 测试时可以换掉。
	now func() time.Time
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// Created 是新建链接的结果。URL 只在这里出现一次。
type Created struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Path      string    `json:"path"`
	Dir       bool      `json:"dir,omitempty"`
	Files     []string  `json:"files"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Create 给工作区里的一个文件或目录建链接。ttl 为 0 用默认值，超过 24 小时报错。
func (s *Service) Create(ctx context.Context, taskID, rel string, ttl time.Duration) (Created, error) {
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < time.Minute || ttl > MaxTTL {
		return Created{}, fmt.Errorf("%w: 有效期要在 1 分钟到 24 小时之间", ErrInvalid)
	}
	base, err := s.Settings.Setting(ctx, BaseURLSetting)
	if err != nil {
		return Created{}, err
	}
	if base == "" {
		return Created{}, fmt.Errorf("%w: 还没有设置下载链接的外部访问地址，请用户在管理后台「文件」页填写", ErrInvalid)
	}
	now := s.clock()
	if err := s.Store.DeleteExpiredShares(ctx, now); err != nil {
		return Created{}, err
	}
	active, err := s.Store.ActiveShares(ctx, now)
	if err != nil {
		return Created{}, err
	}
	if len(active) >= MaxActive {
		return Created{}, fmt.Errorf("%w: 已经有 %d 个有效的链接，先撤销用不着的", ErrInvalid, len(active))
	}
	norm, dir, pinned, err := s.Workspace.Shareable(rel)
	if err != nil {
		return Created{}, err
	}
	token, hash, err := newToken()
	if err != nil {
		return Created{}, err
	}
	l := &Link{ID: core.NewID(), TaskID: taskID, Path: norm, Dir: dir, Files: pinned, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := s.Store.CreateShare(ctx, l, hash); err != nil {
		return Created{}, err
	}
	out := Created{ID: l.ID, URL: link(base, token, ""), Path: l.Path, Dir: dir, ExpiresAt: l.ExpiresAt}
	for _, p := range pinned {
		out.Files = append(out.Files, p.Path)
	}
	return out, nil
}

// CopyReceived 把用户的一个文件复制成可以分享的副本。只能在用户批准之后调用。
func (s *Service) CopyReceived(rel string) (string, error) {
	return s.Workspace.CopyReceived(rel)
}

// List 列出此刻有效的链接。
func (s *Service) List(ctx context.Context) ([]*Link, error) {
	return s.Store.ActiveShares(ctx, s.clock())
}

// Revoke 撤销一个链接。
func (s *Service) Revoke(ctx context.Context, id string) error {
	ok, err := s.Store.RevokeShare(ctx, strings.TrimSpace(id), s.clock())
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// link 拼出下载地址。base 是外部访问地址；sub 是目录链接里某个文件相对于目录的路径，逐段转义。
func link(base, token, sub string) string {
	u := strings.TrimRight(base, "/") + "/s/" + token
	if sub == "" {
		return u
	}
	segs := strings.Split(sub, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return u + "/" + strings.Join(segs, "/")
}

// newToken 生成一个令牌和它的摘要。
func newToken() (token, hash string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("share: 生成令牌失败: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, hashToken(token), nil
}

// hashToken 是令牌在库里的样子。令牌本身是 256 位随机数，不需要加盐或慢哈希。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// validToken 报告一个字符串是不是令牌的样子。不是的不必去查库。
func validToken(s string) bool {
	if len(s) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// NormalizeBaseURL 校验并规范化外部访问地址：必须是 http(s) 的绝对地址，不带查询串和片段，去掉末尾的 /。
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(raw, "?") || strings.HasSuffix(raw, "#") {
		return "", fmt.Errorf("%w: 外部访问地址要写成 https://域名[:端口][/路径] 的样子", ErrInvalid)
	}
	return strings.TrimRight(raw, "/"), nil
}

// CheckAddr 校验下载入口的监听地址：不是本机地址时必须有 TLS——文件会在网络上明文传输。
func CheckAddr(addr string, tls bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("share: 监听地址 %q 不合法: %w", addr, err)
	}
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !local && !tls {
		return fmt.Errorf("share: 下载入口监听在 %q 上却没有配置 TLS 证书——分享的文件会在网络上明文传输。"+
			"要么监听 127.0.0.1 并由前面的反向代理提供 HTTPS，要么设置 ATOM_SHARE_TLS_CERT 与 ATOM_SHARE_TLS_KEY", addr)
	}
	return nil
}
