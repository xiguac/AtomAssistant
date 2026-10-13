package config

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xiguac/atomhelper/internal/channel"
	"github.com/xiguac/atomhelper/internal/channel/qq"
	"github.com/xiguac/atomhelper/internal/connector/calendar"
	"github.com/xiguac/atomhelper/internal/connector/holiday"
	"github.com/xiguac/atomhelper/internal/connector/mail"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/files"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/pay"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/seal"
)

// File 是 atomd 的 YAML 配置。
//
// 用 YAML 而不是环境变量，是因为这部分配置是结构化的、需要用户手工编辑的；
// 连接串、密码这类单值仍然走环境变量。
type File struct {
	Providers []ProviderConfig `yaml:"providers"`
	Routes    []RouteConfig    `yaml:"routes"`
	Fallback  *RouteConfig     `yaml:"fallback"`
	// Agents 是协作模型：主 AI 可以把一块活派给它们。不写就是单模型模式。
	Agents   []AgentConfig  `yaml:"agents"`
	MCP      []MCPConfig    `yaml:"mcp_servers"`
	Tools    []ToolConfig   `yaml:"tools"`
	Approval ApprovalConfig `yaml:"approval"`
	Reply    ReplyConfig    `yaml:"reply"`
	Payment  PaymentConfig  `yaml:"payment"`
	// Connectors 是直连开放协议的内建连接器：日历（CalDAV）、邮件（IMAP/SMTP）。
	Connectors ConnectorsConfig `yaml:"connectors"`
	// Channels 是用户可以和助手对话的消息渠道（QQ 机器人等）。
	//
	// 这一节只能由用户亲手编辑：写在这里的渠道，其主人发来的消息带着用户等级的信任。
	// 助手没有任何办法往这里添加渠道，见 docs/安全模型.md §3.1.1。
	Channels []ChannelConfig `yaml:"channels"`
	// Files 是文件处理的配置。不写这一节，助手没有任何读写文件的工具。
	Files *FilesConfig `yaml:"files"`
}

// FilesConfig 是文件处理的配置。
type FilesConfig struct {
	// Dir 是工作区目录：助手只能读写这个目录里的文件。把要处理的文件放进来，生成的文件也在这里。
	Dir string `yaml:"dir"`
	// Encrypt 为真时，写进工作区的文件落盘即加密，读它、把它发给用户时再解开。
	// 默认不开：文件可能很大，是否值得为它多做一遍加解密由用户决定。
	Encrypt bool `yaml:"encrypt"`
}

// Workspace 按配置打开文件工作区。没有配置时返回 nil。
//
// box 是这台机器的数据密钥。不管开没开加密都要给：加密中途关掉之后，之前加密的文件还得读得出来。
func (f *File) Workspace(box *seal.Box) (*files.Workspace, error) {
	if f.Files == nil {
		return nil, nil
	}
	if strings.TrimSpace(f.Files.Dir) == "" {
		return nil, fmt.Errorf("config: files.dir 不能为空")
	}
	ws, err := files.Open(f.Files.Dir)
	if err != nil {
		return nil, err
	}
	return ws.WithSeal(box, f.Files.Encrypt), nil
}

// ChannelConfig 是一个消息渠道。
type ChannelConfig struct {
	// Name 是渠道的名字，自己起，不能重复，不能含 /。
	Name string `yaml:"name"`
	// Type 是渠道的种类，目前支持 qq。
	Type string `yaml:"type"`
	// AppID 与 Secret 是在对应平台上创建机器人时拿到的凭据。
	// Secret 必须是保险库句柄（vault://名字），授权名单里要有 channel.<渠道名>。
	AppID  string `yaml:"app_id"`
	Secret string `yaml:"secret"`
	// Owners 是主人在这个渠道里的账号标识。只有他们发来的消息会被处理。
	// 第一次接入时留空：给机器人发一句话，它会把你的标识回给你。
	Owners []string `yaml:"owners,omitempty"`
	// Sandbox 为真时连 QQ 的沙箱环境（机器人还没上线、只在沙箱里调试时用）。
	Sandbox bool `yaml:"sandbox,omitempty"`
	// APIBase 与 TokenURL 覆盖平台接口的地址，只在对着自建的模拟服务调试时才需要。
	APIBase  string `yaml:"api_base,omitempty"`
	TokenURL string `yaml:"token_url,omitempty"`
}

// qqAPIBase 定出 QQ 接口的地址：显式指定的优先，其次是沙箱，都没有则用正式环境。
func (c ChannelConfig) qqAPIBase() string {
	if c.APIBase != "" {
		return c.APIBase
	}
	if c.Sandbox {
		return qq.SandboxAPIBase
	}
	return ""
}

// VaultUser 返回这个渠道在保险库授权名单里的名字。
func (c ChannelConfig) VaultUser() string { return "channel." + c.Name }

// ChannelTypes 是目前支持的渠道种类。
var ChannelTypes = []string{"qq"}

// validateChannels 校验消息渠道一节。
//
// 在加载时就查：后台保存配置只过这一道校验，写错的渠道要在保存时拦下，而不是重启之后助手起不来。
func (f *File) validateChannels() error {
	seen := map[string]bool{}
	for _, c := range f.Channels {
		if c.Name == "" || strings.ContainsAny(c.Name, "/ ") {
			return fmt.Errorf("config: 渠道名 %q 不合法（不能为空，不能含 / 和空格）", c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("config: 渠道 %s 重复", c.Name)
		}
		seen[c.Name] = true
		if c.Type != "qq" {
			return fmt.Errorf("config: 渠道 %s 的类型 %q 不支持（目前支持 qq）", c.Name, c.Type)
		}
		if c.AppID == "" {
			return fmt.Errorf("config: 渠道 %s 缺少 app_id", c.Name)
		}
		if !strings.HasPrefix(c.Secret, "vault://") {
			return fmt.Errorf("config: 渠道 %s 的 secret 只能写保险库句柄（vault://名字），不能直接写密钥", c.Name)
		}
	}
	return nil
}

// BuildChannels 按配置构造消息渠道。
func (f *File) BuildChannels(secrets SecretLender, log *slog.Logger) ([]channel.Binding, error) {
	var out []channel.Binding
	for _, c := range f.Channels {
		if c.Name == "" || strings.Contains(c.Name, "/") {
			return nil, fmt.Errorf("config: 渠道名 %q 不合法（不能为空，不能含 /）", c.Name)
		}
		switch c.Type {
		case "qq":
			if c.AppID == "" {
				return nil, fmt.Errorf("config: 渠道 %s 缺少 app_id", c.Name)
			}
			secret, err := secrets.Lend(c.Secret, c.VaultUser())
			if err != nil {
				return nil, fmt.Errorf("config: 渠道 %s 的 secret: %w", c.Name, err)
			}
			out = append(out, channel.Binding{
				Adapter: &qq.Bot{
					Label: c.Name, AppID: c.AppID, Secret: secret, Log: log,
					APIBase: c.qqAPIBase(), TokenURL: c.TokenURL,
				},
				Owners: c.Owners,
			})
		default:
			return nil, fmt.Errorf("config: 渠道 %s 的类型 %q 不支持（目前支持 qq）", c.Name, c.Type)
		}
	}
	return out, nil
}

// ConnectorsConfig 是内建连接器的配置。不写的那一项就不接入。
type ConnectorsConfig struct {
	Calendar *CalendarConfig `yaml:"calendar,omitempty"`
	Mail     *MailConfig     `yaml:"mail,omitempty"`
	Holidays *HolidaysConfig `yaml:"holidays,omitempty"`
}

// HolidaysConfig 是节假日数据的来源：一个公开的日历订阅（.ics）。
type HolidaysConfig struct {
	// URL 是订阅地址，必须是 https。
	// Apple 的中国大陆节假日订阅：https://calendars.icloud.com/holidays/cn_zh.ics
	URL string `yaml:"url"`
}

// Holidays 按配置构造节假日订阅。没有配置时返回 nil。
func (f *File) Holidays() (*holiday.Feed, error) {
	c := f.Connectors.Holidays
	if c == nil {
		return nil, nil
	}
	feed := &holiday.Feed{URL: c.URL}
	if err := feed.Check(); err != nil {
		return nil, fmt.Errorf("config: connectors.holidays.url: %w", err)
	}
	return feed, nil
}

// validateConnectors 校验连接器一节。在加载时就查，理由同 validateChannels。
func (f *File) validateConnectors() error {
	if c := f.Connectors.Calendar; c != nil {
		if c.URL == "" || c.Username == "" {
			return fmt.Errorf("config: connectors.calendar 必须配齐 url / username / password")
		}
		if !strings.HasPrefix(c.Password, "vault://") {
			return fmt.Errorf("config: connectors.calendar.password 只能写保险库句柄（vault://名字），不能直接写密码")
		}
	}
	if c := f.Connectors.Mail; c != nil {
		if c.IMAP == "" || c.Username == "" {
			return fmt.Errorf("config: connectors.mail 必须配齐 imap / username / password")
		}
		if !strings.HasPrefix(c.Password, "vault://") {
			return fmt.Errorf("config: connectors.mail.password 只能写保险库句柄（vault://名字），不能直接写密码")
		}
	}
	if c := f.Connectors.Holidays; c != nil {
		if err := (&holiday.Feed{URL: c.URL}).Check(); err != nil {
			return fmt.Errorf("config: connectors.holidays.url: %w", err)
		}
	}
	return nil
}

// 连接器在保险库授权名单里的名字。密码条目要把对应的名字列进可用工具，连接器才取得到。
const (
	VaultUserCalendar = "connector.calendar"
	VaultUserMail     = "connector.mail"
)

// CalendarConfig 是日历连接器（CalDAV）的配置。
type CalendarConfig struct {
	// URL 是 CalDAV 服务器地址，iCloud 是 https://caldav.icloud.com。
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	// Password 必须是保险库句柄（vault://名字），不接受明文。
	// iCloud 要用"App 专用密码"，不是 Apple ID 的登录密码。
	Password string `yaml:"password"`
	// DefaultCalendar 是新建日程默认写入的日历名。账户里只有一个日历时可以不写。
	DefaultCalendar string `yaml:"default_calendar,omitempty"`
}

// MailConfig 是邮件连接器（IMAP/SMTP）的配置。
type MailConfig struct {
	// IMAP 是收信服务器，写成 主机:端口，iCloud 是 imap.mail.me.com:993。
	IMAP string `yaml:"imap"`
	// SMTP 是发信服务器，iCloud 是 smtp.mail.me.com:587。不写则助手只能读和整理，不能发信。
	SMTP     string `yaml:"smtp,omitempty"`
	Username string `yaml:"username"`
	// Password 必须是保险库句柄（vault://名字），不接受明文。
	Password string `yaml:"password"`
	// From 是发信地址，不写按 username。Name 是发信时显示的名字。
	From string `yaml:"from,omitempty"`
	Name string `yaml:"name,omitempty"`
	// 加密方式 tls / starttls / none，不写按端口推断。none 只允许用于本机地址。
	IMAPSecurity string `yaml:"imap_security,omitempty"`
	SMTPSecurity string `yaml:"smtp_security,omitempty"`
}

// SecretLender 按句柄借出保险库里的值，由 *vault.Vault 提供。
type SecretLender interface {
	Lend(handle, user string) (func(ctx context.Context) (string, error), error)
}

// Calendar 按配置构造日历连接器。没有配置时返回 nil。
func (f *File) Calendar(secrets SecretLender) (*calendar.Client, error) {
	c := f.Connectors.Calendar
	if c == nil {
		return nil, nil
	}
	if c.URL == "" || c.Username == "" {
		return nil, fmt.Errorf("config: connectors.calendar 必须配齐 url / username / password")
	}
	secret, err := secrets.Lend(c.Password, VaultUserCalendar)
	if err != nil {
		return nil, fmt.Errorf("config: connectors.calendar.password: %w", err)
	}
	return &calendar.Client{Endpoint: c.URL, Username: c.Username, Secret: secret, Default: c.DefaultCalendar}, nil
}

// Mail 按配置构造邮件连接器。没有配置时返回 nil。
func (f *File) Mail(secrets SecretLender) (*mail.Client, error) {
	c := f.Connectors.Mail
	if c == nil {
		return nil, nil
	}
	if c.IMAP == "" || c.Username == "" {
		return nil, fmt.Errorf("config: connectors.mail 必须配齐 imap / username / password")
	}
	secret, err := secrets.Lend(c.Password, VaultUserMail)
	if err != nil {
		return nil, fmt.Errorf("config: connectors.mail.password: %w", err)
	}
	return &mail.Client{
		IMAPAddr: c.IMAP, IMAPSecurity: mail.Security(c.IMAPSecurity),
		SMTPAddr: c.SMTP, SMTPSecurity: mail.Security(c.SMTPSecurity),
		Username: c.Username, From: c.From, Name: c.Name, Secret: secret,
	}, nil
}

// ProviderConfig 是一家模型服务。
type ProviderConfig struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	// Protocol 是接口协议：openai（默认，OpenAI Chat Completions 兼容）/ anthropic（Anthropic Messages 兼容）。
	// 两种都支持的服务优先用 openai：它有"只输出 JSON"的开关，规划和提炼都靠这个保证输出能被解析。
	Protocol string `yaml:"protocol,omitempty"`
	// APIKeyEnv 是存放密钥的环境变量名。
	//
	// 刻意不允许在配置文件里直接写密钥：配置文件常被复制、备份、
	// 贴进聊天窗口求助，密钥不该跟着一起走。
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// APIKey 是密钥在保险库里的句柄（vault://名字），与 api_key_env 二选一。
	// 用它的话密钥可以在管理后台里填，不必登上服务器改环境变量。
	// 保险库里那一条的授权名单要有 provider.<服务名>。
	APIKey  string        `yaml:"api_key,omitempty"`
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

// VaultUser 返回这家模型服务在保险库授权名单里的名字。
func (p ProviderConfig) VaultUser() string { return "provider." + p.Name }

// RouteConfig 把一个用途绑定到具体模型。
type RouteConfig struct {
	Purpose   string `yaml:"purpose,omitempty"`
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens,omitempty"`
	// 每百万 token 的单价（元）。不填则这条路由不计费。
	InputYuanPerMTok  float64 `yaml:"input_yuan_per_mtok,omitempty"`
	OutputYuanPerMTok float64 `yaml:"output_yuan_per_mtok,omitempty"`
}

// ReplyConfig 控制助手回复的风格。
type ReplyConfig struct {
	// Style 取值 concise / balanced / detailed，不填按 concise。
	Style string `yaml:"style"`
	// Custom 是用户自己写的额外要求，如"不要用表情""结论放第一句"。
	Custom string `yaml:"custom"`
	// Voice 控制是否学习用户本人的说话方式：learn（默认）/ off。
	// 学到之后，助手替用户写东西、对用户说话时会向他的语气靠拢。
	Voice string `yaml:"voice"`
}

// LearnVoice 报告是否学习用户的表达风格。
func (r ReplyConfig) LearnVoice() bool { return r.Voice != "off" }

// MCPConfig 是一个已批准的 MCP server。
type MCPConfig struct {
	Name      string   `yaml:"name"`
	Transport string   `yaml:"transport"`
	Command   string   `yaml:"command,omitempty"`
	Args      []string `yaml:"args,omitempty"`
	// Env 形如 KEY=VALUE。只有这里列出的变量会传给子进程。密钥不要写在这里，用 secret。
	Env []string `yaml:"env,omitempty"`
	URL string   `yaml:"url,omitempty"`
	// Secret 是这个 server 要的密钥在保险库里的句柄（vault://名字），不接受明文。
	// 保险库里那一条的授权名单要有 mcp.<server 名>。
	// SecretName 是把它交给 server 时用的名字：远程 server 是网址里的查询参数名
	// （高德是 key），本地 server 是环境变量名（高德是 AMAP_MAPS_API_KEY）。
	SecretName string `yaml:"secret_name,omitempty"`
	Secret     string `yaml:"secret,omitempty"`
}

// VaultUser 返回这个 MCP server 在保险库授权名单里的名字。
func (m MCPConfig) VaultUser() string { return "mcp." + m.Name }

// ToolConfig 是一个工具的授权边界，对应用户在 App 上的批准结果。
type ToolConfig struct {
	Name string `yaml:"name"`
	// MaxPrivacy: public / normal / sensitive。secret 不被接受。
	MaxPrivacy string `yaml:"max_privacy,omitempty"`
	// Trust: external / tool_output / connector。
	Trust string `yaml:"trust,omitempty"`
	// SideEffect: none / read_only / reversible / irreversible。
	SideEffect    string   `yaml:"side_effect,omitempty"`
	EgressDomains []string `yaml:"egress_domains,omitempty"`
	AllowedFields []string `yaml:"allowed_fields,omitempty"`
}

// ApprovalConfig 是审批策略。
type ApprovalConfig struct {
	// Mode 取值 auto / confirm，不写按 auto。
	//   auto     自主模式：没有为它写规则的工具直接执行，任务读过外部内容之后也一样。
	//            长程任务不用人一直盯着。硬性限制照旧，见 policy.Policy.Autonomous。
	//   confirm  逐项确认：没有写规则的改动一律先问；任务读过外部内容之后的改动一律先问。
	Mode  string         `yaml:"mode,omitempty"`
	Rules []ApprovalRule `yaml:"rules"`
	Caps  CapsConfig     `yaml:"caps"`
}

// 审批模式。
const (
	ApprovalAuto    = "auto"
	ApprovalConfirm = "confirm"
)

// ApprovalRule 是针对某个工具的放行规则。
type ApprovalRule struct {
	Tool            string        `yaml:"tool"`
	Never           bool          `yaml:"never"`
	AmountBelowYuan float64       `yaml:"amount_below_yuan"`
	Domain          string        `yaml:"domain"`
	LeadTimeAbove   time.Duration `yaml:"lead_time_above"`
}

// CapsConfig 是累计上限。
type CapsConfig struct {
	DailyAutoAmountYuan float64 `yaml:"daily_auto_amount_yuan"`
	DailyAutoCount      int     `yaml:"daily_auto_count"`
	PerTaskAmountYuan   float64 `yaml:"per_task_amount_yuan"`
}

// PaymentConfig 是付款能力的配置。不写这一节，助手就没有任何付款工具。
type PaymentConfig struct {
	Alipay AlipayConfig `yaml:"alipay"`
}

// AlipayConfig 是支付宝 Agent 支付的配置。
type AlipayConfig struct {
	Enabled bool `yaml:"enabled"`
	// Command 是官方 alipay-bot 的可执行文件，不填按 alipay-bot（需在 PATH 里）。
	Command string `yaml:"command"`
	// AgentName 是开通钱包时向支付宝报告的智能体名称，不填按"原子助手"。
	AgentName string `yaml:"agent_name"`
	// ResourceDomains 是允许助手请求的付费资源域名（HTTP 402）。
	// 不填则助手不能主动请求任何网址。写 example.com 同时允许它的子域名。
	ResourceDomains []string `yaml:"resource_domains"`
}

// PaidResources 按配置构造付费资源客户端。
func (f *File) PaidResources() pay.Resources {
	return pay.Resources{Domains: f.Payment.Alipay.ResourceDomains}
}

// Alipay 按配置构造支付宝付款客户端。
func (f *File) Alipay() pay.Alipay {
	name := f.Payment.Alipay.AgentName
	if name == "" {
		name = "原子助手"
	}
	return pay.Alipay{Command: f.Payment.Alipay.Command, AgentName: name}
}

// LoadFile 读取并校验 YAML 配置。路径为空时返回 nil，表示未配置。
func LoadFile(path string) (*File, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// 严格模式：配置里写错的字段名必须报错，不能被静默忽略——
	// 一个拼错的 side_effect 会让工具按最保守等级跑，用户却以为自己配好了。
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("config: 解析 %s 失败: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *File) validate() error {
	for _, p := range f.Providers {
		if p.Name == "" || p.BaseURL == "" || (p.APIKeyEnv == "") == (p.APIKey == "") {
			return fmt.Errorf("config: provider 必须配齐 name / base_url，密钥用 api_key_env 或 api_key（保险库句柄）二选一")
		}
		switch p.Protocol {
		case "", ProtocolOpenAI, ProtocolAnthropic:
		default:
			return fmt.Errorf("config: provider %s 的 protocol 取值非法: %q（可选 openai / anthropic）", p.Name, p.Protocol)
		}
		if p.APIKey != "" && !strings.HasPrefix(p.APIKey, "vault://") {
			return fmt.Errorf("config: provider %s 的 api_key 只能写保险库句柄（vault://名字），不能直接写密钥", p.Name)
		}
	}
	if err := f.validateModels(); err != nil {
		return err
	}
	if err := f.validateChannels(); err != nil {
		return err
	}
	if err := f.validateConnectors(); err != nil {
		return err
	}
	if err := f.validateMCP(); err != nil {
		return err
	}
	for _, t := range f.Tools {
		if _, err := parsePrivacy(t.MaxPrivacy); err != nil {
			return fmt.Errorf("config: 工具 %s: %w", t.Name, err)
		}
		if _, err := parseTrust(t.Trust); err != nil {
			return fmt.Errorf("config: 工具 %s: %w", t.Name, err)
		}
		if _, err := parseSideEffect(t.SideEffect); err != nil {
			return fmt.Errorf("config: 工具 %s: %w", t.Name, err)
		}
	}
	switch f.Approval.Mode {
	case "", ApprovalAuto, ApprovalConfirm:
	default:
		return fmt.Errorf("config: approval.mode 取值非法: %q（可选 auto / confirm）", f.Approval.Mode)
	}
	switch f.Reply.Style {
	case "", "concise", "balanced", "detailed":
	default:
		return fmt.Errorf("config: reply.style 取值非法: %q（可选 concise / balanced / detailed）", f.Reply.Style)
	}
	switch f.Reply.Voice {
	case "", "learn", "off":
	default:
		return fmt.Errorf("config: reply.voice 取值非法: %q（可选 learn / off）", f.Reply.Voice)
	}
	for _, rc := range append(append([]RouteConfig{}, f.Routes...), fallbackOf(f)...) {
		if rc.InputYuanPerMTok < 0 || rc.OutputYuanPerMTok < 0 {
			return fmt.Errorf("config: 模型 %s 的单价不能为负", rc.Model)
		}
	}
	for _, r := range f.Approval.Rules {
		if r.Domain != "" && !core.Domain(r.Domain).Valid() {
			return fmt.Errorf("config: 审批规则 %s 的域取值非法: %q", r.Tool, r.Domain)
		}
	}
	return nil
}

func fallbackOf(f *File) []RouteConfig {
	if f.Fallback == nil {
		return nil
	}
	return []RouteConfig{*f.Fallback}
}

// BuildRouter 按配置构造 LLM 路由。密钥从环境变量或保险库读取。
// 没有任何 provider 用保险库时 secrets 可以为 nil。
func (f *File) BuildRouter(ctx context.Context, secrets SecretLender) (*llm.Router, error) {
	r := llm.NewRouter()
	for _, pc := range f.Providers {
		p, err := f.Provider(ctx, pc.Name, secrets)
		if err != nil {
			return nil, err
		}
		if err := r.AddProvider(p); err != nil {
			return nil, err
		}
	}
	for _, rc := range f.Routes {
		if err := r.SetRoute(llm.Route{
			Purpose:           llm.Purpose(rc.Purpose),
			Provider:          rc.Provider,
			Model:             rc.Model,
			MaxTokens:         rc.MaxTokens,
			InputYuanPerMTok:  rc.InputYuanPerMTok,
			OutputYuanPerMTok: rc.OutputYuanPerMTok,
		}); err != nil {
			return nil, err
		}
	}
	if f.Fallback != nil {
		if err := r.SetFallback(llm.Route{
			Provider:          f.Fallback.Provider,
			Model:             f.Fallback.Model,
			MaxTokens:         f.Fallback.MaxTokens,
			InputYuanPerMTok:  f.Fallback.InputYuanPerMTok,
			OutputYuanPerMTok: f.Fallback.OutputYuanPerMTok,
		}); err != nil {
			return nil, err
		}
	}
	for _, a := range f.Agents {
		if err := r.AddWorker(a.Name, llm.Route{
			Provider:          a.Provider,
			Model:             a.Model,
			MaxTokens:         a.MaxTokens,
			InputYuanPerMTok:  a.InputYuanPerMTok,
			OutputYuanPerMTok: a.OutputYuanPerMTok,
		}); err != nil {
			return nil, err
		}
	}
	if err := f.addSearch(ctx, r, secrets); err != nil {
		return nil, err
	}
	return r, nil
}

// BuildRegistry 按配置构造工具授权表。
func (f *File) BuildRegistry() (*policy.Registry, error) {
	reg := policy.NewRegistry()
	for _, t := range f.Tools {
		privacy, err := parsePrivacy(t.MaxPrivacy)
		if err != nil {
			return nil, err
		}
		trust, err := parseTrust(t.Trust)
		if err != nil {
			return nil, err
		}
		side, err := parseSideEffect(t.SideEffect)
		if err != nil {
			return nil, err
		}
		if err := reg.Register(policy.ToolGrant{
			Name:          t.Name,
			MaxPrivacy:    privacy,
			TrustCeiling:  trust,
			SideEffect:    side,
			EgressDomains: t.EgressDomains,
			AllowedFields: t.AllowedFields,
		}); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// BuildApprovalPolicy 按配置构造审批策略。
func (f *File) BuildApprovalPolicy() *policy.Policy {
	p := &policy.Policy{
		Autonomous: f.Approval.Mode != ApprovalConfirm,
		Caps: policy.Caps{
			DailyAutoAmountCents: yuanToCents(f.Approval.Caps.DailyAutoAmountYuan),
			DailyAutoCount:       f.Approval.Caps.DailyAutoCount,
			PerTaskAmountCents:   yuanToCents(f.Approval.Caps.PerTaskAmountYuan),
		},
	}
	for _, r := range f.Approval.Rules {
		p.Rules = append(p.Rules, policy.Rule{
			Tool:  r.Tool,
			Never: r.Never,
			AutoIf: policy.Condition{
				AmountBelowCents: yuanToCents(r.AmountBelowYuan),
				Domain:           core.Domain(r.Domain),
				LeadTimeAbove:    r.LeadTimeAbove,
			},
		})
	}
	return p
}

// validateMCP 校验 MCP server 的密钥写法。在加载时就查，理由同 validateChannels。
func (f *File) validateMCP() error {
	for _, m := range f.MCP {
		if m.Secret == "" && m.SecretName == "" {
			continue
		}
		if !strings.HasPrefix(m.Secret, "vault://") {
			return fmt.Errorf("config: MCP server %s 的 secret 只能写保险库句柄（vault://名字），不能直接写密钥", m.Name)
		}
		if m.SecretName == "" {
			return fmt.Errorf("config: MCP server %s 配了 secret，还要写 secret_name（查询参数名或环境变量名）", m.Name)
		}
	}
	return nil
}

// MCPServers 返回已批准的 MCP server 列表。
// 没有任何 server 用到保险库时 secrets 可以为 nil。
func (f *File) MCPServers(secrets SecretLender) ([]mcpgw.ServerSpec, error) {
	out := make([]mcpgw.ServerSpec, 0, len(f.MCP))
	for _, m := range f.MCP {
		spec, err := m.Spec(secrets)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	return out, nil
}

// Spec 把一个 MCP server 的配置变成连接参数。密钥到连接的那一刻才向保险库取。
func (m MCPConfig) Spec(secrets SecretLender) (mcpgw.ServerSpec, error) {
	kind := mcpgw.TransportKind(m.Transport)
	if kind != mcpgw.TransportCommand && kind != mcpgw.TransportHTTP {
		return mcpgw.ServerSpec{}, fmt.Errorf("config: MCP server %s 的传输方式非法: %q", m.Name, m.Transport)
	}
	spec := mcpgw.ServerSpec{
		Name: m.Name, Transport: kind,
		Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL,
	}
	if m.Secret != "" {
		if secrets == nil {
			return mcpgw.ServerSpec{}, fmt.Errorf("config: MCP server %s 的密钥在保险库里，但保险库不可用", m.Name)
		}
		take, err := secrets.Lend(m.Secret, m.VaultUser())
		if err != nil {
			return mcpgw.ServerSpec{}, fmt.Errorf("config: MCP server %s 的 secret: %w", m.Name, err)
		}
		spec.SecretName, spec.Secret = m.SecretName, take
	}
	return spec, nil
}

func parsePrivacy(s string) (core.PrivacyLevel, error) {
	switch s {
	case "public":
		return core.PrivacyPublic, nil
	case "", "normal":
		return core.PrivacyNormal, nil
	case "sensitive":
		return core.PrivacySensitive, nil
	case "secret":
		// 允许配成 secret 等于允许把绝密数据交给工具，这条没有开关。
		return 0, fmt.Errorf("max_privacy 不允许配置为 secret：绝密数据不传给任何工具")
	default:
		return 0, fmt.Errorf("max_privacy 取值非法: %q", s)
	}
}

func parseTrust(s string) (core.TrustLevel, error) {
	switch s {
	case "", "external":
		return core.TrustExternal, nil
	case "tool_output":
		return core.TrustToolOutput, nil
	case "connector":
		return core.TrustConnector, nil
	case "user", "system":
		return 0, fmt.Errorf("trust 不允许配置为 %q：工具输出不能具备指令资格", s)
	default:
		return 0, fmt.Errorf("trust 取值非法: %q", s)
	}
}

func parseSideEffect(s string) (core.SideEffect, error) {
	switch core.SideEffect(s) {
	case core.SideEffectNone, core.SideEffectReadOnly,
		core.SideEffectReversible, core.SideEffectIrreversible:
		return core.SideEffect(s), nil
	case "":
		// 不写就按最保守的处理，而不是按最宽松的。
		return core.SideEffectIrreversible, nil
	default:
		return "", fmt.Errorf("side_effect 取值非法: %q", s)
	}
}

func yuanToCents(y float64) int64 {
	if y <= 0 {
		return 0
	}
	return int64(y*100 + 0.5)
}
