package config

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/channel/dingtalk"
	"github.com/xiguac/atomassistant/internal/channel/feishu"
	"github.com/xiguac/atomassistant/internal/channel/qq"
	"github.com/xiguac/atomassistant/internal/channel/weixin"
	"github.com/xiguac/atomassistant/internal/connector/calendar"
	"github.com/xiguac/atomassistant/internal/connector/holiday"
	"github.com/xiguac/atomassistant/internal/connector/mail"
	"github.com/xiguac/atomassistant/internal/connector/weather"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/files"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/mcpgw"
	"github.com/xiguac/atomassistant/internal/media"
	"github.com/xiguac/atomassistant/internal/pay"
	"github.com/xiguac/atomassistant/internal/policy"
	"github.com/xiguac/atomassistant/internal/sandbox"
	"github.com/xiguac/atomassistant/internal/seal"
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
	// Budget 是每个任务的预算上限。不写按内置的默认值。
	Budget *BudgetConfig `yaml:"budget,omitempty"`
	// Sandbox 是代码沙箱的配置。不写这一节，助手不能跑代码。要先启用文件处理。
	Sandbox *SandboxConfig `yaml:"sandbox,omitempty"`
	// ASR 是语音识别的配置。不写这一节，聊天里发来的语音消息收不了。
	ASR *ASRConfig `yaml:"asr,omitempty"`
	// MemoryVector 是向量记忆的配置。不写这一节，记忆检索只按文字重合。
	MemoryVector *MemoryVectorConfig `yaml:"memory_vector,omitempty"`
}

// MemoryVectorConfig 是向量记忆用的两个本机服务。由 deploy/deploy.sh vector 安装并写入。
// Qdrant 的 API key 由安装脚本写进服务器的 env 文件（ATOM_QDRANT_KEY），不经用户之手。
type MemoryVectorConfig struct {
	// EmbedURL 是向量服务的地址，如 http://127.0.0.1:8766。
	EmbedURL string `yaml:"embed_url"`
	// QdrantURL 是 Qdrant 的地址，如 http://127.0.0.1:6333。
	QdrantURL string `yaml:"qdrant_url"`
}

// ASRConfig 是语音识别服务的配置。服务由 deploy/deploy.sh asr 装在本机。
type ASRConfig struct {
	// URL 是服务地址，如 http://127.0.0.1:8765。
	URL string `yaml:"url"`
}

// SpeechRecognizer 按配置构造语音识别的客户端。没有配置时返回 nil。
func (f *File) SpeechRecognizer() *media.ASR {
	if f.ASR == nil {
		return nil
	}
	return &media.ASR{URL: f.ASR.URL}
}

// SandboxConfig 是代码沙箱的配置。
type SandboxConfig struct {
	// Python 是沙箱里用的解释器的绝对路径，由 deploy/deploy.sh sandbox 装好，默认 /opt/atomassistant-sandbox/bin/python3。
	Python string `yaml:"python,omitempty"`
	// TimeoutSeconds 是一次运行的时间上限，不写按 60 秒。
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
	// MemoryMB 是一次运行能用的内存上限，不写按 1024。
	MemoryMB int `yaml:"memory_mb,omitempty"`
}

// DefaultSandboxPython 是沙箱解释器的默认位置。
const DefaultSandboxPython = "/opt/atomassistant-sandbox/bin/python3"

// SandboxRunner 按配置构造沙箱。没有配置时返回 nil。ws 是文件工作区，没有启用文件处理时报错。
func (f *File) SandboxRunner(ws *files.Workspace) (*sandbox.Runner, error) {
	c := f.Sandbox
	if c == nil {
		return nil, nil
	}
	if ws == nil {
		return nil, fmt.Errorf("config: 启用沙箱要先启用文件处理（files 一节）")
	}
	r := &sandbox.Runner{Python: c.Python, Timeout: time.Duration(c.TimeoutSeconds) * time.Second, MemoryMB: c.MemoryMB, Workspace: ws}
	if r.Python == "" {
		r.Python = DefaultSandboxPython
	}
	if r.Timeout <= 0 {
		r.Timeout = time.Minute
	}
	if r.MemoryMB <= 0 {
		r.MemoryMB = 1024
	}
	return r, nil
}

// BudgetConfig 是每个任务的预算上限。任一项不写（或写 0）就用内置的默认值；用到上限任务会暂停，不会接着花。
type BudgetConfig struct {
	// MaxTokens 是一个任务最多用多少 token（输入加输出）。
	MaxTokens int64 `yaml:"max_tokens,omitempty"`
	// MaxCostYuan 是一个任务最多花多少钱（元），按模型路由里填的单价算。
	MaxCostYuan float64 `yaml:"max_cost_yuan,omitempty"`
	// MaxHours 是一个任务最多真正执行多少小时，等待的时间不算。
	MaxHours float64 `yaml:"max_hours,omitempty"`
}

// 预算各项允许的上限：写错一位数不至于让一个跑飞的任务把钱花光。
const (
	MaxBudgetTokens = 50_000_000
	MaxBudgetYuan   = 10_000
	MaxBudgetHours  = 24 * 30
)

// Check 校验预算。
func (b *BudgetConfig) Check() error {
	if b == nil {
		return nil
	}
	if b.MaxTokens < 0 || b.MaxCostYuan < 0 || b.MaxHours < 0 {
		return fmt.Errorf("config: budget 里的各项不能为负")
	}
	if b.MaxTokens > MaxBudgetTokens || b.MaxCostYuan > MaxBudgetYuan || b.MaxHours > MaxBudgetHours {
		return fmt.Errorf("config: budget 超出允许的范围（token 至多 %d，金额至多 %d 元，时长至多 %d 小时）",
			MaxBudgetTokens, MaxBudgetYuan, MaxBudgetHours)
	}
	return nil
}

// TaskBudget 返回每个任务的预算：配置里写了的用配置的，没写的用内置的默认值。
// f 为 nil（没有配置文件）时就是默认值。
func (f *File) TaskBudget() core.Budget {
	b := core.DefaultBudget()
	if f == nil || f.Budget == nil {
		return b
	}
	if f.Budget.MaxTokens > 0 {
		b.MaxTokens = f.Budget.MaxTokens
	}
	if f.Budget.MaxCostYuan > 0 {
		b.MaxCostCents = yuanToCents(f.Budget.MaxCostYuan)
	}
	if f.Budget.MaxHours > 0 {
		b.MaxWallClock = time.Duration(f.Budget.MaxHours * float64(time.Hour))
	}
	return b
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
	// Type 是渠道的种类：qq / feishu / dingtalk / weixin。
	Type string `yaml:"type"`
	// AppID 与 Secret 是在对应平台上创建机器人时拿到的凭据。
	// Secret 必须是保险库句柄（vault://名字），授权名单里要有 channel.<渠道名>。
	// 各家的叫法不一样：QQ 是 AppID / AppSecret，飞书是 App ID / App Secret，
	// 钉钉是 Client ID / Client Secret。微信没有这两样：它是扫码登录的，
	// app_id 里记的是登录后拿到的机器人标识，secret 指向登录后拿到的机器人凭证，
	// 两项都由管理后台的扫码流程写入，不用手填。
	AppID  string `yaml:"app_id"`
	Secret string `yaml:"secret"`
	// Owners 是主人在这个渠道里的账号标识。只有他们发来的消息会被处理。
	// 第一次接入时留空：给机器人发一句话，它会把你的标识回给你。
	Owners []string `yaml:"owners,omitempty"`
	// RobotCode 只有钉钉用：机器人的编码。不写按 app_id（企业内部应用的机器人两者相同）。
	RobotCode string `yaml:"robot_code,omitempty"`
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
var ChannelTypes = []string{"qq", "feishu", "dingtalk", "weixin"}

func knownChannelType(t string) bool {
	for _, k := range ChannelTypes {
		if k == t {
			return true
		}
	}
	return false
}

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
		if !knownChannelType(c.Type) {
			return fmt.Errorf("config: 渠道 %s 的类型 %q 不支持（目前支持 %s）", c.Name, c.Type, strings.Join(ChannelTypes, " / "))
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
// state 用来存微信渠道要跨重启留着的小状态，没有微信渠道时可以为 nil。
func (f *File) BuildChannels(secrets SecretLender, state weixin.KV, log *slog.Logger) ([]channel.Binding, error) {
	var out []channel.Binding
	for _, c := range f.Channels {
		if c.Name == "" || strings.Contains(c.Name, "/") {
			return nil, fmt.Errorf("config: 渠道名 %q 不合法（不能为空，不能含 /）", c.Name)
		}
		if !knownChannelType(c.Type) {
			return nil, fmt.Errorf("config: 渠道 %s 的类型 %q 不支持（目前支持 %s）", c.Name, c.Type, strings.Join(ChannelTypes, " / "))
		}
		if c.AppID == "" {
			return nil, fmt.Errorf("config: 渠道 %s 缺少 app_id", c.Name)
		}
		secret, err := secrets.Lend(c.Secret, c.VaultUser())
		if err != nil {
			return nil, fmt.Errorf("config: 渠道 %s 的 secret: %w", c.Name, err)
		}
		var adapter channel.Adapter
		switch c.Type {
		case "qq":
			adapter = &qq.Bot{Label: c.Name, AppID: c.AppID, Secret: secret, Log: log,
				APIBase: c.qqAPIBase(), TokenURL: c.TokenURL}
		case "feishu":
			adapter = &feishu.Bot{Label: c.Name, AppID: c.AppID, Secret: secret, Log: log, APIBase: c.APIBase}
		case "dingtalk":
			adapter = &dingtalk.Bot{Label: c.Name, AppID: c.AppID, Secret: secret, Log: log,
				RobotCode: c.RobotCode, APIBase: c.APIBase}
		case "weixin":
			// 微信没有 AppSecret：secret 指向的是扫码登录后拿到的机器人凭证，接口地址也是登录时给的。
			adapter = &weixin.Bot{Label: c.Name, Token: secret, BaseURL: c.APIBase, State: state, Log: log}
		}
		out = append(out, channel.Binding{Adapter: adapter, Owners: c.Owners})
	}
	return out, nil
}

// ConnectorsConfig 是内建连接器的配置。不写的那一项就不接入。
//
// 日历、邮箱、节假日订阅都可以接好几个，写在 calendars / mailboxes / holiday_feeds 里，各有名字和用途标签。
// 早先的写法 calendar / mail / holidays 只能写一个，仍然认：读的时候排在列表最前面，算默认的那个。
// 后台保存时一律改写成列表。天气和联网搜索是一把密钥管全部，只有一个。
type ConnectorsConfig struct {
	Calendar     *CalendarConfig  `yaml:"calendar,omitempty"`
	Mail         *MailConfig      `yaml:"mail,omitempty"`
	Holidays     *HolidaysConfig  `yaml:"holidays,omitempty"`
	Calendars    []CalendarConfig `yaml:"calendars,omitempty"`
	Mailboxes    []MailConfig     `yaml:"mailboxes,omitempty"`
	HolidayFeeds []HolidaysConfig `yaml:"holiday_feeds,omitempty"`
	Weather      *WeatherConfig   `yaml:"weather,omitempty"`
	Search       *SearchConfig    `yaml:"search,omitempty"`
}

// 早先只能写一个时没有名字，按这个名字称呼它。
const (
	DefaultCalendarName = "日历"
	DefaultMailboxName  = "邮箱"
	DefaultHolidaysName = "节假日"
)

// CalendarList 返回全部日历账号，默认的在最前面。
func (c ConnectorsConfig) CalendarList() []CalendarConfig {
	var out []CalendarConfig
	if c.Calendar != nil {
		one := *c.Calendar
		if one.Name == "" {
			one.Name = DefaultCalendarName
		}
		out = append(out, one)
	}
	return append(out, c.Calendars...)
}

// MailboxList 返回全部邮箱，默认的在最前面。
func (c ConnectorsConfig) MailboxList() []MailConfig {
	var out []MailConfig
	if c.Mail != nil {
		one := *c.Mail
		if one.Label == "" {
			one.Label = DefaultMailboxName
		}
		out = append(out, one)
	}
	return append(out, c.Mailboxes...)
}

// HolidayList 返回全部节假日订阅。
func (c ConnectorsConfig) HolidayList() []HolidaysConfig {
	var out []HolidaysConfig
	if c.Holidays != nil {
		one := *c.Holidays
		if one.Name == "" {
			one.Name = DefaultHolidaysName
		}
		out = append(out, one)
	}
	return append(out, c.HolidayFeeds...)
}

// Normalize 把早先只能写一个的写法并进列表，后台保存前调用。
func (c *ConnectorsConfig) Normalize() {
	c.Calendars, c.Mailboxes, c.HolidayFeeds = c.CalendarList(), c.MailboxList(), c.HolidayList()
	c.Calendar, c.Mail, c.Holidays = nil, nil, nil
}

// ValidAccountName 校验数据源账号的名字：不能空，不能带 / 和 |（它们用来拼日历名、编号）。
func ValidAccountName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 32 || strings.ContainsAny(name, "/|\n\r\t") {
		return fmt.Errorf("名称不能为空、不超过 32 个字，不能含 / 或 |")
	}
	return nil
}

// SearchConfig 是联网搜索与读网页用的 DeepSeek 搜索接口的密钥。
// 不写这一项时，用配置过的 DeepSeek 模型服务的密钥。
type SearchConfig struct {
	// Key 是 DeepSeek API Key 在保险库里的句柄（vault://名字），不接受明文。
	// 保险库里那一条的授权名单要有 connector.search。
	Key string `yaml:"key"`
}

// WeatherConfig 是天气数据的来源：和风天气（QWeather）的开发服务。
type WeatherConfig struct {
	// Host 是开发者自己的 API Host，在和风天气控制台的「设置」里看，如 abcxyz.qweatherapi.com。
	Host string `yaml:"host"`
	// Key 是 API KEY 在保险库里的句柄（vault://名字），不接受明文。
	// 保险库里那一条的授权名单要有 connector.weather。
	Key string `yaml:"key"`
}

// Weather 按配置构造天气客户端。没有配置时返回 nil。
func (f *File) Weather(secrets SecretLender) (*weather.Client, error) {
	c := f.Connectors.Weather
	if c == nil {
		return nil, nil
	}
	if err := weather.CheckHost(c.Host); err != nil {
		return nil, fmt.Errorf("config: connectors.weather.host: %w", err)
	}
	secret, err := secrets.Lend(c.Key, VaultUserWeather)
	if err != nil {
		return nil, fmt.Errorf("config: connectors.weather.key: %w", err)
	}
	return &weather.Client{Host: c.Host, Secret: secret}, nil
}

// HolidaysConfig 是节假日数据的来源：一个公开的日历订阅（.ics）。
type HolidaysConfig struct {
	// Name、Tags 是名字和用途标签，接了多个时用来区分。
	Name string   `yaml:"name,omitempty"`
	Tags []string `yaml:"tags,omitempty"`
	// URL 是订阅地址，必须是 https。
	// Apple 的中国大陆节假日订阅：https://calendars.icloud.com/holidays/cn_zh.ics
	URL string `yaml:"url"`
}

// validateConnectors 校验连接器一节。在加载时就查，理由同 validateChannels。
func (f *File) validateConnectors() error {
	seen := map[string]bool{}
	unique := func(kind, name string) error {
		if err := ValidAccountName(name); err != nil {
			return fmt.Errorf("config: %s「%s」: %w", kind, name, err)
		}
		if seen[kind+"/"+name] {
			return fmt.Errorf("config: 有两个%s都叫「%s」，名字不能重复", kind, name)
		}
		seen[kind+"/"+name] = true
		return nil
	}
	for _, c := range f.Connectors.CalendarList() {
		if err := unique("日历", c.Name); err != nil {
			return err
		}
		if c.URL == "" || c.Username == "" {
			return fmt.Errorf("config: 日历「%s」必须配齐 url / username / password", c.Name)
		}
		if !strings.HasPrefix(c.Password, "vault://") {
			return fmt.Errorf("config: 日历「%s」的 password 只能写保险库句柄（vault://名字），不能直接写密码", c.Name)
		}
	}
	for _, c := range f.Connectors.MailboxList() {
		if err := unique("邮箱", c.Label); err != nil {
			return err
		}
		if c.IMAP == "" || c.Username == "" {
			return fmt.Errorf("config: 邮箱「%s」必须配齐 imap / username / password", c.Label)
		}
		if !strings.HasPrefix(c.Password, "vault://") {
			return fmt.Errorf("config: 邮箱「%s」的 password 只能写保险库句柄（vault://名字），不能直接写密码", c.Label)
		}
	}
	for _, c := range f.Connectors.HolidayList() {
		if err := unique("节假日订阅", c.Name); err != nil {
			return err
		}
		if err := (&holiday.Feed{URL: c.URL}).Check(); err != nil {
			return fmt.Errorf("config: 节假日订阅「%s」: %w", c.Name, err)
		}
	}
	if c := f.Connectors.Search; c != nil && !strings.HasPrefix(c.Key, "vault://") {
		return fmt.Errorf("config: connectors.search.key 只能写保险库句柄（vault://名字），不能直接写密钥")
	}
	if c := f.Connectors.Weather; c != nil {
		if err := weather.CheckHost(c.Host); err != nil {
			return fmt.Errorf("config: connectors.weather.host: %w", err)
		}
		if !strings.HasPrefix(c.Key, "vault://") {
			return fmt.Errorf("config: connectors.weather.key 只能写保险库句柄（vault://名字），不能直接写密钥")
		}
	}
	return nil
}

// 连接器在保险库授权名单里的名字。密码条目要把对应的名字列进可用工具，连接器才取得到。
const (
	VaultUserCalendar = "connector.calendar"
	VaultUserMail     = "connector.mail"
	VaultUserWeather  = "connector.weather"
	VaultUserSearch   = "connector.search"
)

// CalendarConfig 是日历连接器（CalDAV）的配置。
type CalendarConfig struct {
	// Name、Tags 是账号的名字和用途标签（如「工作」「家庭」），助手按它们区分该用哪个。
	Name string   `yaml:"name,omitempty"`
	Tags []string `yaml:"tags,omitempty"`
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
	// Label、Tags 是邮箱的名字和用途标签。Name 已经用来写发信人名称，所以名字叫 Label。
	Label string   `yaml:"label,omitempty"`
	Tags  []string `yaml:"tags,omitempty"`
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

// NamedCalendar 是按配置装好的一个日历账号。
type NamedCalendar struct {
	Name   string
	Tags   []string
	Client *calendar.Client
}

// CalendarAccounts 按配置构造全部日历账号，默认的在最前面。没有配置时返回空。
// 每个账号的密码各存一条，都授权给 connector.calendar。
func (f *File) CalendarAccounts(secrets SecretLender) ([]NamedCalendar, error) {
	var out []NamedCalendar
	for _, c := range f.Connectors.CalendarList() {
		if c.URL == "" || c.Username == "" {
			return nil, fmt.Errorf("config: 日历「%s」必须配齐 url / username / password", c.Name)
		}
		secret, err := secrets.Lend(c.Password, VaultUserCalendar)
		if err != nil {
			return nil, fmt.Errorf("config: 日历「%s」的 password: %w", c.Name, err)
		}
		out = append(out, NamedCalendar{Name: c.Name, Tags: c.Tags,
			Client: &calendar.Client{Endpoint: c.URL, Username: c.Username, Secret: secret, Default: c.DefaultCalendar}})
	}
	return out, nil
}

// Calendar 返回默认的那个日历账号。没有配置时返回 nil。
func (f *File) Calendar(secrets SecretLender) (*calendar.Client, error) {
	all, err := f.CalendarAccounts(secrets)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[0].Client, nil
}

// NamedMailbox 是按配置装好的一个邮箱。
type NamedMailbox struct {
	Name   string
	Tags   []string
	Client *mail.Client
}

// MailAccounts 按配置构造全部邮箱，默认的在最前面。没有配置时返回空。
// 每个邮箱的密码各存一条，都授权给 connector.mail。
func (f *File) MailAccounts(secrets SecretLender) ([]NamedMailbox, error) {
	var out []NamedMailbox
	for _, c := range f.Connectors.MailboxList() {
		if c.IMAP == "" || c.Username == "" {
			return nil, fmt.Errorf("config: 邮箱「%s」必须配齐 imap / username / password", c.Label)
		}
		secret, err := secrets.Lend(c.Password, VaultUserMail)
		if err != nil {
			return nil, fmt.Errorf("config: 邮箱「%s」的 password: %w", c.Label, err)
		}
		out = append(out, NamedMailbox{Name: c.Label, Tags: c.Tags, Client: &mail.Client{
			IMAPAddr: c.IMAP, IMAPSecurity: mail.Security(c.IMAPSecurity),
			SMTPAddr: c.SMTP, SMTPSecurity: mail.Security(c.SMTPSecurity),
			Username: c.Username, From: c.From, Name: c.Name, Secret: secret,
		}})
	}
	return out, nil
}

// Mail 返回默认的那个邮箱。没有配置时返回 nil。
func (f *File) Mail(secrets SecretLender) (*mail.Client, error) {
	all, err := f.MailAccounts(secrets)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[0].Client, nil
}

// HolidayFeeds 按配置构造全部节假日订阅。没有配置时返回空。
func (f *File) HolidayFeeds() ([]*holiday.Feed, error) {
	var out []*holiday.Feed
	for _, c := range f.Connectors.HolidayList() {
		feed := &holiday.Feed{URL: c.URL}
		if err := feed.Check(); err != nil {
			return nil, fmt.Errorf("config: 节假日订阅「%s」: %w", c.Name, err)
		}
		out = append(out, feed)
	}
	return out, nil
}

// Holidays 返回第一个节假日订阅。没有配置时返回 nil。
func (f *File) Holidays() (*holiday.Feed, error) {
	all, err := f.HolidayFeeds()
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[0], nil
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
	APIKey string `yaml:"api_key,omitempty"`
	// Timeout 已不再起作用，保留只为让写了它的旧配置还能读进来。
	// 它原本是整次请求的总时限；模型调用改成流式之后，改为固定的首字超时 10 分钟、不设总时限，
	// 旧配置里常见的 120s 会把强模型的长任务掐断。
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
	// Thinking 是思考强度：off / low / medium / high / max，不写按模型服务的默认。
	Thinking string `yaml:"thinking,omitempty"`
	// Inputs 是模型支持的输入：text / image / audio / video，不写按 text 与 image。
	// 只对主 AI（purpose: plan，或者兜底）有意义。
	Inputs []string `yaml:"inputs,omitempty"`
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
	// SecretIn 是远程 server 收这项密钥的位置：query（默认，网址的查询参数）/ header（请求头，
	// 这时 secret_name 是请求头的名字，如 Authorization）。
	SecretIn string `yaml:"secret_in,omitempty"`
	// Secrets 是除上面那一项之外它还要的密钥：有的 server 要两三样（如 ID 加密钥）。写法相同。
	Secrets []MCPSecret `yaml:"secrets,omitempty"`
}

// MCPSecret 是 MCP server 要的一项密钥：交给它时用的名字，以及保险库句柄。
type MCPSecret struct {
	Name   string `yaml:"name"`
	Secret string `yaml:"secret"`
	// In 取值 query（默认）/ header，含义同 MCPConfig.SecretIn。
	In string `yaml:"in,omitempty"`
}

// 密钥的位置。
const (
	SecretInQuery  = "query"
	SecretInHeader = "header"
)

// SecretLabel 定出这个 server 名为 param 的那项密钥在保险库里该叫什么。
func (m MCPConfig) SecretLabel(param string) string { return m.VaultUser() + "." + param }

// AllSecrets 返回它要的全部密钥，secret_name / secret 那一项在最前。
func (m MCPConfig) AllSecrets() []MCPSecret {
	var out []MCPSecret
	if m.Secret != "" {
		out = append(out, MCPSecret{Name: m.SecretName, Secret: m.Secret, In: m.SecretIn})
	}
	return append(out, m.Secrets...)
}

// VaultUser 返回这个 MCP server 在保险库授权名单里的名字。
func (m MCPConfig) VaultUser() string { return "mcp." + m.Name }

// ToolConfig 是一个工具的授权边界，对应用户在 App 上的批准结果。
type ToolConfig struct {
	Name string `yaml:"name"`
	// Description 是 server 给这个工具写的说明，只用来在管理后台里展示，不影响授权。
	// 它是别人写的文字，不会交给模型（模型看到的说明每次连接时现取）。
	Description string `yaml:"description,omitempty"`
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
	if err := f.Budget.Check(); err != nil {
		return err
	}
	if c := f.ASR; c != nil {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: asr 的 url 要写成 http://主机:端口，实际 %q", c.URL)
		}
	}
	if c := f.MemoryVector; c != nil {
		for name, raw := range map[string]string{"embed_url": c.EmbedURL, "qdrant_url": c.QdrantURL} {
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "http" || u.Host == "" || !isLoopback(u.Hostname()) {
				return fmt.Errorf("config: memory_vector 的 %s 要写成 http://127.0.0.1:端口（只用本机的服务），实际 %q", name, raw)
			}
		}
	}
	if c := f.Sandbox; c != nil {
		if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 3600 || c.MemoryMB < 0 || c.MemoryMB > 16384 {
			return fmt.Errorf("config: sandbox 的时间上限须在 0～3600 秒，内存上限须在 0～16384 MB")
		}
		if f.Files == nil {
			return fmt.Errorf("config: 启用沙箱要先启用文件处理（files 一节）")
		}
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
			Thinking:          llm.Thinking(rc.Thinking),
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
			Thinking:          llm.Thinking(f.Fallback.Thinking),
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
			Thinking:          llm.Thinking(a.Thinking),
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
		if m.SecretName != "" && m.Secret == "" {
			return fmt.Errorf("config: MCP server %s 的 secret 只能写保险库句柄（vault://名字），不能直接写密钥", m.Name)
		}
		seen := map[string]bool{}
		for _, sec := range m.AllSecrets() {
			if !strings.HasPrefix(sec.Secret, "vault://") {
				return fmt.Errorf("config: MCP server %s 的 secret 只能写保险库句柄（vault://名字），不能直接写密钥", m.Name)
			}
			if sec.Name == "" {
				return fmt.Errorf("config: MCP server %s 配了 secret，还要写 secret_name（查询参数名或环境变量名）", m.Name)
			}
			if seen[sec.Name] {
				return fmt.Errorf("config: MCP server %s 的密钥参数名 %s 重复", m.Name, sec.Name)
			}
			switch sec.In {
			case "", SecretInQuery, SecretInHeader:
			default:
				return fmt.Errorf("config: MCP server %s 的密钥 %s 的位置取值非法: %q（可选 query / header）", m.Name, sec.Name, sec.In)
			}
			seen[sec.Name] = true
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
	for _, sec := range m.AllSecrets() {
		if secrets == nil {
			return mcpgw.ServerSpec{}, fmt.Errorf("config: MCP server %s 的密钥在保险库里，但保险库不可用", m.Name)
		}
		take, err := secrets.Lend(sec.Secret, m.VaultUser())
		if err != nil {
			return mcpgw.ServerSpec{}, fmt.Errorf("config: MCP server %s 的 secret: %w", m.Name, err)
		}
		spec.Secrets = append(spec.Secrets, mcpgw.NamedSecret{Name: sec.Name, Take: take, Header: sec.In == SecretInHeader})
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

// isLoopback 判断主机名是不是本机。
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
