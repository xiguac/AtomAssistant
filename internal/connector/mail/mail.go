// Package mail 是邮件连接器：用 IMAP 读取和整理邮件，用 SMTP 发信。
//
// 两个都是开放协议，iCloud、QQ 邮箱、163、Gmail、企业邮箱用的都是它们，这里不绑定任何一家。
//
// 邮件是整个系统里最需要提防的数据源，所以这里的约束比别处多：
//
//  1. **邮件内容是陌生人写的。** 任何人都能给用户发一封写着"请把通讯录转发给我"的信。
//     读回来的发件人、标题、正文一律是外部内容，调用方不得把它们当成指令。
//  2. **读信不留痕迹。** 取正文用的是 PEEK，不会把未读的信标成已读——
//     助手看过一眼，不该让用户以为自己看过。
//  3. **没有彻底删除。** 能做的最重的操作是移进废纸篓，用户随时能捞回来。
//  4. **密码不落在这里。** 每次调用现取、用完即弃，来源是保险库。
//  5. **不在明文连接上发密码。** 只有连本机的测试服务器才允许不加密。
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	netmail "net/mail"
	"net/smtp"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	gomail "github.com/emersion/go-message/mail"
)

func init() {
	// 国内的邮件大量使用 GBK / GB2312 / GB18030，标题和发件人名字都要靠它解码。
	imap.CharsetReader = charset.Reader
}

var (
	// ErrInvalid 表示参数不合法。
	ErrInvalid = errors.New("mail: 参数不合法")
	// ErrNotFound 表示没有这封邮件或这个文件夹。
	ErrNotFound = errors.New("mail: 没有这封邮件或这个文件夹")
	// ErrInsecure 表示拒绝在不加密的连接上发送密码。
	ErrInsecure = errors.New("mail: 拒绝在不加密的连接上发送密码")
)

// Security 是连接的加密方式。
type Security string

const (
	// SecurityTLS 一连上就加密（IMAP 993、SMTP 465）。
	SecurityTLS Security = "tls"
	// SecurityStartTLS 先明文握手再升级为加密（IMAP 143、SMTP 587）。
	SecurityStartTLS Security = "starttls"
	// SecurityNone 不加密。只允许用于本机地址。
	SecurityNone Security = "none"
)

const (
	// MaxResults 是一次搜索最多返回的邮件数。
	MaxResults = 50
	// ChunkBytes 是一次读取返回的正文长度。正文要进模型的上下文，长信分段读。
	ChunkBytes = 1500
	// maxFetchBytes 是从服务器取一封信的字节上限，挡住带大附件的信。
	maxFetchBytes = 2 << 20
	timeout       = 30 * time.Second
)

// Client 连接一个邮箱。
type Client struct {
	// IMAPAddr 形如 imap.mail.me.com:993。
	IMAPAddr     string
	IMAPSecurity Security
	// SMTPAddr 形如 smtp.mail.me.com:587。为空则不能发信。
	SMTPAddr     string
	SMTPSecurity Security
	Username     string
	// From 是发信地址，空值按 Username。
	From string
	// Name 是发信时显示的名字。
	Name string
	// Secret 在每次调用时取密码。
	Secret func(ctx context.Context) (string, error)
}

// security 定出一个地址实际使用的加密方式：没写就按端口推断。
func security(addr string, s Security, implicit string) (Security, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%w: 地址 %q 要写成 主机:端口", ErrInvalid, addr)
	}
	switch s {
	case "":
		if port == implicit {
			return SecurityTLS, nil
		}
		return SecurityStartTLS, nil
	case SecurityTLS, SecurityStartTLS:
		return s, nil
	case SecurityNone:
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("%w: %s 不是本机地址", ErrInsecure, host)
		}
		return s, nil
	}
	return "", fmt.Errorf("%w: 未知的加密方式 %q（可选 tls / starttls / none）", ErrInvalid, s)
}

// dial 登录 IMAP。调用方负责 Logout。
func (c *Client) dial(ctx context.Context) (*client.Client, error) {
	if c.IMAPAddr == "" || c.Username == "" || c.Secret == nil {
		return nil, fmt.Errorf("%w: 邮件连接器缺少地址、账号或密码来源", ErrInvalid)
	}
	sec, err := security(c.IMAPAddr, c.IMAPSecurity, "993")
	if err != nil {
		return nil, err
	}
	password, err := c.Secret(ctx)
	if err != nil {
		return nil, fmt.Errorf("mail: 取邮箱密码失败: %w", err)
	}
	host, _, _ := net.SplitHostPort(c.IMAPAddr)
	dialer := &net.Dialer{Timeout: timeout}
	var conn *client.Client
	if sec == SecurityTLS {
		conn, err = client.DialWithDialerTLS(dialer, c.IMAPAddr, &tls.Config{ServerName: host})
	} else {
		conn, err = client.DialWithDialer(dialer, c.IMAPAddr)
	}
	if err != nil {
		return nil, fmt.Errorf("mail: 连接收信服务器失败: %w", err)
	}
	conn.Timeout = timeout
	if sec == SecurityStartTLS {
		if err := conn.StartTLS(&tls.Config{ServerName: host}); err != nil {
			_ = conn.Logout()
			return nil, fmt.Errorf("mail: 收信连接升级加密失败: %w", err)
		}
	}
	if err := conn.Login(c.Username, password); err != nil {
		_ = conn.Logout()
		return nil, fmt.Errorf("mail: 登录邮箱失败（账号或密码不对？）: %w", err)
	}
	return conn, nil
}

// Folder 是一个邮件文件夹。
type Folder struct {
	Name string `json:"name"`
	// Role 是它的用途：inbox / sent / drafts / trash / junk / archive，普通文件夹为空。
	Role string `json:"role,omitempty"`
}

var roleAttrs = map[string]string{
	imap.SentAttr: "sent", imap.DraftsAttr: "drafts", imap.TrashAttr: "trash",
	imap.JunkAttr: "junk", imap.ArchiveAttr: "archive",
}

// roleNames 是服务器没有标明用途时，按常见名字认出文件夹的用途。
var roleNames = map[string]string{
	"sent": "sent", "sent messages": "sent", "sent items": "sent", "已发送": "sent",
	"drafts": "drafts", "草稿箱": "drafts",
	"trash": "trash", "deleted messages": "trash", "deleted items": "trash", "已删除": "trash",
	"junk": "junk", "spam": "junk", "垃圾邮件": "junk",
	"archive": "archive", "归档": "archive",
}

func (c *Client) folders(conn *client.Client) ([]Folder, error) {
	ch := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() { done <- conn.List("", "*", ch) }()
	var out []Folder
	for info := range ch {
		skip := false
		role := ""
		for _, a := range info.Attributes {
			if strings.EqualFold(a, imap.NoSelectAttr) {
				skip = true
			}
			for attr, r := range roleAttrs {
				if strings.EqualFold(a, attr) {
					role = r
				}
			}
		}
		if skip {
			continue
		}
		if strings.EqualFold(info.Name, "INBOX") {
			role = "inbox"
		} else if role == "" {
			role = roleNames[strings.ToLower(info.Name)]
		}
		out = append(out, Folder{Name: info.Name, Role: role})
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("mail: 读取文件夹列表失败: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Folders 列出邮箱里的文件夹。
func (c *Client) Folders(ctx context.Context) ([]Folder, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Logout() }()
	return c.folders(conn)
}

// resolve 把调用方给的文件夹名换成服务器上真实存在的名字。
//
// 可以写真实的名字，也可以写用途（inbox / sent / trash / archive / junk / drafts）。
// 名字必须已经存在：不替模型凭一个拼错的名字去新建文件夹。
func (c *Client) resolve(conn *client.Client, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "inbox") {
		return "INBOX", nil
	}
	list, err := c.folders(conn)
	if err != nil {
		return "", err
	}
	for _, f := range list {
		if f.Name == name {
			return f.Name, nil
		}
	}
	for _, f := range list {
		if f.Role != "" && strings.EqualFold(f.Role, name) {
			return f.Name, nil
		}
	}
	return "", fmt.Errorf("%w: 没有文件夹 %q", ErrNotFound, name)
}

// Summary 是一封邮件的概要。
type Summary struct {
	// ID 指明是哪个文件夹里的哪一封，读取和整理时用它。
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      string    `json:"to,omitempty"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date"`
	Unread  bool      `json:"unread,omitempty"`
	Flagged bool      `json:"flagged,omitempty"`
}

// ref 是解析后的邮件 ID。
type ref struct {
	uid, validity uint32
	folder        string
}

func formatID(uid, validity uint32, folder string) string {
	return fmt.Sprintf("%d.%d@%s", uid, validity, folder)
}

func parseID(id string) (ref, error) {
	nums, folder, ok := strings.Cut(strings.TrimSpace(id), "@")
	a, b, ok2 := strings.Cut(nums, ".")
	if !ok || !ok2 || folder == "" {
		return ref{}, fmt.Errorf("%w: 邮件 id 不合法", ErrInvalid)
	}
	uid, err1 := strconv.ParseUint(a, 10, 32)
	validity, err2 := strconv.ParseUint(b, 10, 32)
	if err1 != nil || err2 != nil || uid == 0 {
		return ref{}, fmt.Errorf("%w: 邮件 id 不合法", ErrInvalid)
	}
	return ref{uid: uint32(uid), validity: uint32(validity), folder: folder}, nil
}

// open 选中一封邮件所在的文件夹，并核对 ID 仍然指向原来那封信。
//
// 服务器重建文件夹后编号会重排，同一个编号可能变成另一封信。
// 不核对的话，"把这封移进废纸篓"就可能移走一封毫不相干的。
func (c *Client) open(conn *client.Client, id string, readOnly bool) (ref, error) {
	r, err := parseID(id)
	if err != nil {
		return r, err
	}
	status, err := conn.Select(r.folder, readOnly)
	if err != nil {
		return r, fmt.Errorf("%w: 打不开文件夹 %q: %v", ErrNotFound, r.folder, err)
	}
	if status.UidValidity != r.validity {
		return r, fmt.Errorf("%w: 文件夹 %q 已被服务器重排，这个 id 失效了，请重新搜索", ErrNotFound, r.folder)
	}
	return r, nil
}

func one(uid uint32) *imap.SeqSet {
	set := new(imap.SeqSet)
	set.AddNum(uid)
	return set
}

func addresses(list []*imap.Address) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		if a == nil {
			continue
		}
		if name := strings.TrimSpace(a.PersonalName); name != "" {
			parts = append(parts, name+" <"+a.Address()+">")
		} else {
			parts = append(parts, a.Address())
		}
	}
	return strings.Join(parts, ", ")
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

func summarize(m *imap.Message, validity uint32, folder string) Summary {
	s := Summary{
		ID: formatID(m.Uid, validity, folder), Date: m.InternalDate,
		Unread: !hasFlag(m.Flags, imap.SeenFlag), Flagged: hasFlag(m.Flags, imap.FlaggedFlag),
	}
	if e := m.Envelope; e != nil {
		s.From, s.To, s.Subject = addresses(e.From), addresses(e.To), strings.TrimSpace(e.Subject)
		if !e.Date.IsZero() {
			s.Date = e.Date
		}
	}
	return s
}

// Query 是搜索条件，各项同时满足。
type Query struct {
	Folder     string
	From       string
	Subject    string
	Text       string
	Since      time.Time
	Before     time.Time
	UnreadOnly bool
	Limit      int
}

// Search 搜索邮件，最新的在前。
func (c *Client) Search(ctx context.Context, q Query) ([]Summary, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Logout() }()

	folder, err := c.resolve(conn, q.Folder)
	if err != nil {
		return nil, err
	}
	status, err := conn.Select(folder, true)
	if err != nil {
		return nil, fmt.Errorf("%w: 打不开文件夹 %q: %v", ErrNotFound, folder, err)
	}
	crit := imap.NewSearchCriteria()
	if v := strings.TrimSpace(q.From); v != "" {
		crit.Header.Add("From", v)
	}
	if v := strings.TrimSpace(q.Subject); v != "" {
		crit.Header.Add("Subject", v)
	}
	if v := strings.TrimSpace(q.Text); v != "" {
		crit.Text = []string{v}
	}
	crit.Since, crit.Before = q.Since, q.Before
	if q.UnreadOnly {
		crit.WithoutFlags = []string{imap.SeenFlag}
	}
	uids, err := conn.UidSearch(crit)
	if err != nil {
		return nil, fmt.Errorf("mail: 搜索失败: %w", err)
	}
	if len(uids) == 0 {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxResults {
		limit = MaxResults
	}
	// 编号越大越新。只取最新的那一批的概要，而不是把整个文件夹拉回来。
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if len(uids) > limit {
		uids = uids[:limit]
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	ch := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- conn.UidFetch(set, []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, imap.FetchUid}, ch)
	}()
	var out []Summary
	for m := range ch {
		out = append(out, summarize(m, status.UidValidity, folder))
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("mail: 读取邮件概要失败: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

// Message 是一封邮件的内容。
type Message struct {
	Summary
	Cc string `json:"cc,omitempty"`
	// Text 是正文中从 Offset 开始的一段。
	Text string `json:"text"`
	// NextOffset 非零表示正文还没完，下一次从这里接着读。
	NextOffset  int      `json:"next_offset,omitempty"`
	Attachments []string `json:"attachments,omitempty"`

	messageID string
	replyTo   string
}

// Read 读取一封邮件的正文，从 offset 字节处开始取一段。读取不会把邮件标成已读。
func (c *Client) Read(ctx context.Context, id string, offset int) (Message, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = conn.Logout() }()
	return c.read(conn, id, offset)
}

func (c *Client) read(conn *client.Client, id string, offset int) (Message, error) {
	r, err := c.open(conn, id, true)
	if err != nil {
		return Message{}, err
	}
	fetch := func(items ...imap.FetchItem) (*imap.Message, error) {
		ch := make(chan *imap.Message, 1)
		done := make(chan error, 1)
		go func() { done <- conn.UidFetch(one(r.uid), items, ch) }()
		var got *imap.Message
		for m := range ch {
			got = m
		}
		if err := <-done; err != nil {
			return nil, fmt.Errorf("mail: 读取邮件失败: %w", err)
		}
		if got == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return got, nil
	}
	head, err := fetch(imap.FetchEnvelope, imap.FetchFlags, imap.FetchInternalDate, imap.FetchUid, imap.FetchRFC822Size)
	if err != nil {
		return Message{}, err
	}
	out := Message{Summary: summarize(head, r.validity, r.folder)}
	if e := head.Envelope; e != nil {
		out.Cc, out.messageID = addresses(e.Cc), e.MessageId
		if len(e.ReplyTo) > 0 && e.ReplyTo[0] != nil {
			out.replyTo = e.ReplyTo[0].Address()
		} else if len(e.From) > 0 && e.From[0] != nil {
			out.replyTo = e.From[0].Address()
		}
	}
	// PEEK：看一眼不等于读过，不动"未读"标记。
	section := &imap.BodySectionName{Peek: true}
	if head.Size > maxFetchBytes {
		// 带大附件的信只取开头一段，不整封下载。正文总在附件前面。
		section.Partial = []int{0, maxFetchBytes}
	}
	got, err := fetch(imap.FetchUid, section.FetchItem())
	if err != nil {
		return Message{}, err
	}
	var raw io.Reader
	for _, lit := range got.Body {
		raw = lit
	}
	if raw == nil {
		return out, nil
	}
	text, attachments := extract(raw)
	out.Attachments = attachments
	if offset < 0 || offset > len(text) {
		offset = len(text)
	}
	// 退到字符边界：调用方给的偏移量可能落在一个汉字中间。
	for offset > 0 && offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset--
	}
	end := offset + ChunkBytes
	if end >= len(text) {
		end = len(text)
	} else {
		for end > offset && !utf8.RuneStart(text[end]) {
			end--
		}
		out.NextOffset = end
	}
	out.Text = text[offset:end]
	return out, nil
}

// extract 从一封信的原文里取出给人看的正文和附件名。
//
// 优先用纯文本；只有 HTML 时去掉标签。附件只报名字，不读内容。
// 信被截断或格式有问题时，能取到多少算多少。
func extract(raw io.Reader) (text string, attachments []string) {
	// 字符集认不出时库会同时给出错误和一个仍然可用的读取器，这种情况继续读。
	mr, err := gomail.CreateReader(raw)
	if err != nil && !message.IsUnknownCharset(err) {
		return "", nil
	}
	var plain, rich strings.Builder
	for {
		part, err := mr.NextPart()
		if err != nil && !message.IsUnknownCharset(err) {
			// 正常读完，或者信在这里被截断了。
			break
		}
		switch h := part.Header.(type) {
		case *gomail.InlineHeader:
			kind, _, _ := h.ContentType()
			body, _ := io.ReadAll(io.LimitReader(part.Body, maxFetchBytes))
			switch kind {
			case "text/plain", "":
				plain.Write(body)
				plain.WriteString("\n")
			case "text/html":
				rich.Write(body)
			}
		case *gomail.AttachmentHeader:
			if name, _ := h.Filename(); name != "" {
				attachments = append(attachments, name)
			} else {
				attachments = append(attachments, "（未命名附件）")
			}
		}
	}
	text = strings.TrimSpace(plain.String())
	if text == "" {
		text = stripHTML(rich.String())
	}
	return tidy(text), attachments
}

var (
	reHidden  = regexp.MustCompile(`(?is)<(style|script|head)\b.*?</(style|script|head)>`)
	reBreak   = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/li|/h[1-6])\b[^>]*>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlank   = regexp.MustCompile(`\n{3,}`)
	reSpacing = regexp.MustCompile(`[ \t\x{00a0}]{2,}`)
)

// stripHTML 把 HTML 正文变成纯文本。
func stripHTML(s string) string {
	s = reHidden.ReplaceAllString(s, "")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}

// tidy 去掉多余的空行、空白和控制字符。
func tidy(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// 不换行空格在网页邮件里到处都是，按普通空格处理。
	s = strings.ReplaceAll(s, " ", " ")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(reSpacing.ReplaceAllString(l, " "))
	}
	return strings.TrimSpace(reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// Mark 修改一封邮件的已读、星标状态。nil 表示这一项不动。
func (c *Client) Mark(ctx context.Context, id string, read, flagged *bool) error {
	if read == nil && flagged == nil {
		return fmt.Errorf("%w: 没有说要改什么", ErrInvalid)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Logout() }()
	r, err := c.open(conn, id, false)
	if err != nil {
		return err
	}
	set := func(flag string, on bool) error {
		op := imap.FlagsOp(imap.RemoveFlags)
		if on {
			op = imap.AddFlags
		}
		if err := conn.UidStore(one(r.uid), imap.FormatFlagsOp(op, true), []interface{}{flag}, nil); err != nil {
			return fmt.Errorf("mail: 修改邮件标记失败: %w", err)
		}
		return nil
	}
	if read != nil {
		if err := set(imap.SeenFlag, *read); err != nil {
			return err
		}
	}
	if flagged != nil {
		if err := set(imap.FlaggedFlag, *flagged); err != nil {
			return err
		}
	}
	return nil
}

// Move 把一封邮件移到另一个文件夹，返回目标文件夹的真实名字。
// 删除就是移进废纸篓（folder 写 trash）。
func (c *Client) Move(ctx context.Context, id, folder string) (string, error) {
	if strings.TrimSpace(folder) == "" {
		return "", fmt.Errorf("%w: 没有说移到哪个文件夹", ErrInvalid)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Logout() }()
	dest, err := c.resolve(conn, folder)
	if err != nil {
		return "", err
	}
	r, err := c.open(conn, id, false)
	if err != nil {
		return "", err
	}
	if dest == r.folder {
		return dest, nil
	}
	if err := conn.UidMove(one(r.uid), dest); err != nil {
		return "", fmt.Errorf("mail: 移动邮件失败: %w", err)
	}
	return dest, nil
}

// Outgoing 是一封要发出的邮件。
type Outgoing struct {
	To      []string
	Cc      []string
	Subject string
	Body    string
	// ReplyTo 是要回复的那封邮件的 ID。填了之后收件人和标题可以不写，按原信补上。
	ReplyTo string
}

// Sent 是发信的结果。
type Sent struct {
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	// Saved 表示是否已在"已发送"里留底。
	Saved bool   `json:"saved_to_sent"`
	Note  string `json:"note,omitempty"`
}

func parseList(kind string, list []string) ([]*gomail.Address, error) {
	var out []*gomail.Address
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		a, err := netmail.ParseAddress(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s %q 不是合法的邮箱地址", ErrInvalid, kind, raw)
		}
		out = append(out, &gomail.Address{Name: a.Name, Address: a.Address})
	}
	return out, nil
}

// Send 发出一封邮件，并尽量在"已发送"里留底。
//
// 留底失败不算发信失败：信已经发出去了，这时候报错只会让上层再发一遍。
func (c *Client) Send(ctx context.Context, o Outgoing) (Sent, error) {
	if c.SMTPAddr == "" {
		return Sent{}, fmt.Errorf("%w: 没有配置发信服务器", ErrInvalid)
	}
	sec, err := security(c.SMTPAddr, c.SMTPSecurity, "465")
	if err != nil {
		return Sent{}, err
	}
	if strings.TrimSpace(o.Body) == "" {
		return Sent{}, fmt.Errorf("%w: 邮件正文是空的", ErrInvalid)
	}
	if strings.ContainsAny(o.Subject, "\r\n") {
		return Sent{}, fmt.Errorf("%w: 标题不能换行", ErrInvalid)
	}
	to, err := parseList("收件人", o.To)
	if err != nil {
		return Sent{}, err
	}
	cc, err := parseList("抄送", o.Cc)
	if err != nil {
		return Sent{}, err
	}
	fromAddr := c.From
	if fromAddr == "" {
		fromAddr = c.Username
	}
	if _, err := netmail.ParseAddress(fromAddr); err != nil {
		return Sent{}, fmt.Errorf("%w: 发信地址 %q 不是合法的邮箱地址，请在配置里写明 from", ErrInvalid, fromAddr)
	}
	subject := strings.TrimSpace(o.Subject)

	var header gomail.Header
	if o.ReplyTo != "" {
		conn, err := c.dial(ctx)
		if err != nil {
			return Sent{}, err
		}
		orig, err := c.read(conn, o.ReplyTo, 0)
		_ = conn.Logout()
		if err != nil {
			return Sent{}, fmt.Errorf("mail: 读取要回复的邮件失败: %w", err)
		}
		if len(to) == 0 && orig.replyTo != "" {
			to = []*gomail.Address{{Address: orig.replyTo}}
		}
		if subject == "" {
			subject = orig.Subject
			if !strings.HasPrefix(strings.ToLower(subject), "re:") && !strings.HasPrefix(subject, "回复") {
				subject = "Re: " + subject
			}
		}
		if id := strings.Trim(orig.messageID, "<>"); id != "" {
			header.SetMsgIDList("In-Reply-To", []string{id})
			header.SetMsgIDList("References", []string{id})
		}
	}
	if len(to) == 0 {
		return Sent{}, fmt.Errorf("%w: 没有收件人", ErrInvalid)
	}
	if subject == "" {
		return Sent{}, fmt.Errorf("%w: 邮件要有标题", ErrInvalid)
	}

	now := time.Now()
	header.SetDate(now)
	header.SetAddressList("From", []*gomail.Address{{Name: c.Name, Address: fromAddr}})
	header.SetAddressList("To", to)
	if len(cc) > 0 {
		header.SetAddressList("Cc", cc)
	}
	header.SetSubject(subject)
	header.Set("Content-Type", "text/plain; charset=utf-8")
	if err := header.GenerateMessageID(); err != nil {
		return Sent{}, fmt.Errorf("mail: 生成邮件编号失败: %w", err)
	}
	var buf bytes.Buffer
	w, err := gomail.CreateSingleInlineWriter(&buf, header)
	if err != nil {
		return Sent{}, fmt.Errorf("mail: 组装邮件失败: %w", err)
	}
	if _, err := io.WriteString(w, strings.ReplaceAll(strings.ReplaceAll(o.Body, "\r\n", "\n"), "\n", "\r\n")); err != nil {
		return Sent{}, fmt.Errorf("mail: 组装邮件失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return Sent{}, fmt.Errorf("mail: 组装邮件失败: %w", err)
	}

	password, err := c.Secret(ctx)
	if err != nil {
		return Sent{}, fmt.Errorf("mail: 取邮箱密码失败: %w", err)
	}
	var rcpt []string
	for _, a := range append(append([]*gomail.Address{}, to...), cc...) {
		rcpt = append(rcpt, a.Address)
	}
	if err := c.deliver(ctx, sec, password, fromAddr, rcpt, buf.Bytes()); err != nil {
		return Sent{}, err
	}

	out := Sent{To: rcpt, Subject: subject}
	if err := c.keepCopy(ctx, buf.Bytes(), now); err != nil {
		out.Note = "邮件已发出，但没能在「已发送」里留底：" + err.Error()
	} else {
		out.Saved = true
	}
	return out, nil
}

// deliver 通过 SMTP 把邮件交出去。
func (c *Client) deliver(ctx context.Context, sec Security, password, from string, rcpt []string, msg []byte) error {
	host, _, _ := net.SplitHostPort(c.SMTPAddr)
	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error
	if sec == SecurityTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: host}}).DialContext(ctx, "tcp", c.SMTPAddr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", c.SMTPAddr)
	}
	if err != nil {
		return fmt.Errorf("mail: 连接发信服务器失败: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * timeout))
	sc, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mail: 发信服务器握手失败: %w", err)
	}
	defer func() { _ = sc.Close() }()
	if sec == SecurityStartTLS {
		if err := sc.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("mail: 发信连接升级加密失败: %w", err)
		}
	}
	if ok, _ := sc.Extension("AUTH"); ok {
		// PlainAuth 自己也会拒绝在非本机的明文连接上交出密码，这里是第二道。
		if err := sc.Auth(smtp.PlainAuth("", c.Username, password, host)); err != nil {
			return fmt.Errorf("mail: 发信服务器登录失败: %w", err)
		}
	}
	if err := sc.Mail(from); err != nil {
		return fmt.Errorf("mail: 发信服务器不接受发信地址: %w", err)
	}
	for _, r := range rcpt {
		if err := sc.Rcpt(r); err != nil {
			return fmt.Errorf("mail: 发信服务器不接受收件人 %s: %w", r, err)
		}
	}
	w, err := sc.Data()
	if err != nil {
		return fmt.Errorf("mail: 发送邮件失败: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("mail: 发送邮件失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: 发送邮件失败: %w", err)
	}
	// 到这里信已经被服务器收下。QUIT 出错不影响结果。
	_ = sc.Quit()
	return nil
}

// keepCopy 把发出的信存一份到"已发送"。
//
// 助手以用户的名义发出去的每一封信，用户都应该能在自己的邮箱里看到。
func (c *Client) keepCopy(ctx context.Context, msg []byte, at time.Time) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Logout() }()
	folder, err := c.resolve(conn, "sent")
	if err != nil {
		return err
	}
	if err := conn.Append(folder, []string{imap.SeenFlag}, at, bytes.NewReader(msg)); err != nil {
		return fmt.Errorf("mail: 写入 %s 失败: %w", folder, err)
	}
	return nil
}

var (
	reQuoteHead = regexp.MustCompile(`(?m)^\s*(On .{0,200}wrote:|在.{0,200}写道[:：]|-{2,}\s*(Original Message|原始邮件|Forwarded message|转发的邮件)\s*-{2,}|发件人[:：].*|From:\s.*|_{10,})\s*$`)
	reSignature = regexp.MustCompile(`(?m)^(-- |发自我的.*|Sent from my .*)$`)
)

// OwnWords 从一封用户自己发出的信里取出他亲手写的那部分：
// 去掉引用的原文、转发的内容和客户端自动加的签名。
func OwnWords(text string) string {
	if loc := reQuoteHead.FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}
	if loc := reSignature.FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		kept = append(kept, line)
	}
	return tidy(strings.Join(kept, "\n"))
}
