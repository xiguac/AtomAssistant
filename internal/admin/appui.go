package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/device"
	"github.com/xiguac/atomassistant/internal/files"
	"github.com/xiguac/atomassistant/internal/memory"
)

// 手机界面的描述：服务器把每个页面写成「分区 → 行 → 按钮 / 表单」，手机上只有一个通用的渲染器，
// 按它画原生的列表、表单和按钮。好处是页面长什么样、能做什么，都在服务器这一边定，
// 而且这一边能用单元测试验证；手机上不需要为每个页面单独写界面。
//
// 页面的数据来自网页后台原来的页面处理函数（见 appproxy.go），这里只做「数据 → 界面描述」的转换。

type uiPage struct {
	Title  string `json:"title"`
	Notice string `json:"notice,omitempty"`
	Error  string `json:"error,omitempty"`
	// Toolbar 是导航栏右上角的按钮，一般是「新增」。
	Toolbar []uiItem `json:"toolbar,omitempty"`
	// Tabs 是页面顶部的分段控件：同一类功能合并成一个入口，在几个页面之间切换。
	Tabs     []uiTab     `json:"tabs,omitempty"`
	Sections []uiSection `json:"sections"`
}

// uiTab 是分段控件里的一段：点了就换成 Path 这个页面。
type uiTab struct {
	Title string `json:"title"`
	Path  string `json:"path"`
}

type uiSection struct {
	Title  string   `json:"title,omitempty"`
	Footer string   `json:"footer,omitempty"`
	Items  []uiItem `json:"items"`
}

// uiItem 是一行。Kind 是 row（一行文字，可点）、text（一段文字）、mono（等宽的文字）、
// button（一个按钮）、qr（二维码，内容在 Text 里）、upload（选一个文件上传）、download（点了下载一个文件）。
type uiItem struct {
	Kind     string `json:"kind"`
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	Value    string `json:"value,omitempty"`
	Badge    string `json:"badge,omitempty"`
	// Tone 是徽标和数值的颜色：ok、warn、bad、info。
	Tone string `json:"tone,omitempty"`
	Icon string `json:"icon,omitempty"`
	Text string `json:"text,omitempty"`
	// Page 非空：点这一行进入另一个页面。Form 非空：点这一行弹出表单。Action 非空：点这一行执行动作。
	Page   string     `json:"page,omitempty"`
	Form   *uiForm    `json:"form,omitempty"`
	Action *uiAction  `json:"action,omitempty"`
	Raw    string     `json:"raw,omitempty"`
	Menu   []uiAction `json:"menu,omitempty"`
}

// uiAction 是一次写操作：POST Path，表单是 Form。
type uiAction struct {
	Label       string              `json:"label"`
	Path        string              `json:"path"`
	Form        map[string][]string `json:"form,omitempty"`
	Confirm     string              `json:"confirm,omitempty"`
	Destructive bool                `json:"destructive,omitempty"`
	// Sensitive 为真表示涉及密钥、密码、登录方式：手机上执行前要再刷一次脸。
	Sensitive bool `json:"sensitive,omitempty"`
	// Get 为真表示这不是写操作：把表单拼成查询参数，打开 Path 这个页面（用于筛选）。
	Get bool `json:"get,omitempty"`
	// Then 是成功后去哪：reload（刷新当前页，默认）、back（返回上一页）、或者一个页面路径。
	Then string `json:"then,omitempty"`
}

type uiForm struct {
	Title  string    `json:"title"`
	Intro  string    `json:"intro,omitempty"`
	Fields []uiField `json:"fields"`
	Submit uiAction  `json:"submit"`
}

// uiField 的 Type：text、secure、multiline、number、url、toggle、select、multiselect、date。
type uiField struct {
	Key         string     `json:"key"`
	Label       string     `json:"label"`
	Type        string     `json:"type"`
	Placeholder string     `json:"placeholder,omitempty"`
	Value       string     `json:"value,omitempty"`
	Values      []string   `json:"values,omitempty"`
	Options     []uiOption `json:"options,omitempty"`
	Footer      string     `json:"footer,omitempty"`
	Required    bool       `json:"required,omitempty"`
}

type uiOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ---- 小工具 ----

func form(kv ...string) map[string][]string {
	m := map[string][]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = append(m[kv[i]], kv[i+1])
	}
	return m
}

func row(title, subtitle string) uiItem { return uiItem{Kind: "row", Title: title, Subtitle: subtitle} }

func textItem(text string) uiItem { return uiItem{Kind: "text", Text: text} }

func opts(pairs ...string) []uiOption {
	var out []uiOption
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, uiOption{Value: pairs[i], Label: pairs[i+1]})
	}
	return out
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func yesNoText(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("01-02 15:04")
}

// ---- 分发 ----

// uiPage 把一个页面的数据转成手机界面的描述。u 是请求的地址（有的页面要用到它的查询参数），
// 没有手机版的页面返回 false。
func (s *Server) uiPage(name string, v *view, u *url.URL) (uiPage, bool) {
	var p uiPage
	switch d := v.Data.(type) {
	case vaultData:
		p = s.uiVault(d)
	case memoryData:
		p = uiMemory(d)
	case modelsData:
		p = uiModels(d)
	case channelsData:
		p = uiChannels(d)
	case weixinData:
		p = uiWeixin(d)
	case connectorsData:
		p = uiConnectors(d)
	case mcpData:
		p = uiMCP(d)
	case skillsData:
		p = uiSkills(d)
	case promptsData:
		p = uiPrompts(d)
	case securityData:
		p = uiSecurity(d)
	case logsData:
		p = uiLogs(d)
	case filesData:
		p = uiFiles(d)
	case devicesData:
		p = uiDevices(d)
	case sessionData:
		p = uiSession(d)
	default:
		return uiPage{}, false
	}
	p.Notice, p.Error = v.Notice, v.Error
	if u != nil {
		p.Tabs = tabsFor(u.Path)
	}
	return p, true
}

// 手机上「管理」的合并入口：同一类功能放进一个入口，进去用分段控件切换。
// 每组的第一段是点入口时打开的页面。网页后台的导航分组也沿用这一份。
var appGroups = []struct {
	Title, Sub, Icon string
	Tabs             []uiTab
}{
	{"记忆与文件", "记忆摘要、待确认修改、文件、会话记录", "brain.head.profile",
		[]uiTab{{"记忆", "/memory"}, {"文件", "/files"}, {"会话", "/_sessions"}}},
	{"模型与提示词", "模型接口、系统提示词", "cpu",
		[]uiTab{{"模型", "/models"}, {"系统提示词", "/prompts"}}},
	{"接入与能力", "消息源、数据源、MCP、Skill", "puzzlepiece.extension",
		[]uiTab{{"消息源", "/channels"}, {"数据源", "/connectors"}, {"MCP", "/mcp"}, {"Skill", "/skills"}}},
	{"手机设备", "已配对设备、上报间隔、吊销", "iphone",
		[]uiTab{{"手机", "/devices"}}},
	{"安全与日志", "环境变量、登录与安全、运行日志", "lock.shield",
		[]uiTab{{"环境变量", "/vault"}, {"登录与安全", "/security"}, {"日志", "/logs"}}},
}

// tabsFor 返回 path 所在的那一组分段；单独成页的返回 nil。
func tabsFor(path string) []uiTab {
	for _, g := range appGroups {
		if len(g.Tabs) < 2 {
			continue
		}
		for _, t := range g.Tabs {
			if t.Path == path {
				return g.Tabs
			}
		}
	}
	return nil
}

// appHub 是「管理」首页：5 个合并后的入口，带上待处理的数量。
func (s *Server) appHub(w http.ResponseWriter, r *http.Request, _ *device.Device) {
	b := s.countBadges(r.Context())
	pending := map[string]int{"记忆与文件": b.Memory, "安全与日志": b.Vault}
	sec := uiSection{}
	for _, g := range appGroups {
		it := row(g.Title, g.Sub)
		it.Icon, it.Page = g.Icon, g.Tabs[0].Path
		if n := pending[g.Title]; n > 0 {
			it.Badge, it.Tone = strconv.Itoa(n)+" 待处理", "warn"
		}
		sec.Items = append(sec.Items, it)
	}
	appReply(w, http.StatusOK, uiPage{Title: "管理", Sections: []uiSection{sec}})
}

// ---- 环境变量 ----

func (s *Server) uiVault(d vaultData) uiPage {
	p := uiPage{Title: "环境变量"}
	var toolOpts []uiOption
	for _, t := range d.Tools {
		toolOpts = append(toolOpts, uiOption{Value: t.Value, Label: t.Label})
	}
	p.Toolbar = []uiItem{{Kind: "button", Title: "新增", Icon: "plus", Form: &uiForm{
		Title: "新增条目",
		Intro: "存证件号、卡号、地址这类要给工具用的个人信息。保存后不能查看，只能更换或删除。",
		Fields: []uiField{
			{Key: "label", Label: "名称", Type: "text", Placeholder: "idcard", Required: true},
			{Key: "note", Label: "用途", Type: "text", Placeholder: "本人身份证号，订票、报名时使用"},
			{Key: "value", Label: "值", Type: "secure", Required: true},
			{Key: "tools", Label: "授权给哪些工具", Type: "multiselect", Options: toolOpts},
			{Key: "tools", Label: "其他工具（用逗号分隔）", Type: "text", Placeholder: "输入工具名，多个以逗号分隔"},
		},
		Submit: uiAction{Label: "保存", Path: "/vault", Sensitive: true},
	}}}

	if len(d.Requests) > 0 {
		sec := uiSection{Title: "待填写", Footer: "这些由助手在任务中登记，名称、用途和授权对象都是助手写的，请核对后再填值。值只由你在这里填，助手看不到。"}
		for _, r := range d.Requests {
			it := row("vault://"+r.Label, r.Note)
			it.Value = strings.Join(r.Tools, "、")
			it.Form = &uiForm{
				Title: "填写「" + r.Label + "」", Intro: r.Note,
				Fields: []uiField{{Key: "value", Label: "值", Type: "secure", Required: true}},
				Submit: uiAction{Label: "保存", Path: "/vault/request", Form: form("label", r.Label), Sensitive: true},
			}
			it.Menu = []uiAction{{Label: "忽略", Path: "/vault/request/dismiss", Form: form("label", r.Label)}}
			sec.Items = append(sec.Items, it)
		}
		p.Sections = append(p.Sections, sec)
	}

	sec := uiSection{Title: "个人信息", Footer: "助手只看得到名称和用途，通过句柄引用，拿不到内容。"}
	if len(d.Items) == 0 {
		sec.Items = append(sec.Items, textItem("还没有条目。点右上角「新增」添加。"))
	}
	for _, m := range d.Items {
		it := row("vault://"+m.Label, m.Note)
		it.Value = yesNoText(len(m.AllowedTools) > 0, strings.Join(m.AllowedTools, "、"), "未授权")
		it.Form = &uiForm{
			Title: "更换「" + m.Label + "」的值", Intro: "填写新值，原值会被替换。",
			Fields: []uiField{{Key: "value", Label: "新值", Type: "secure", Required: true}},
			Submit: uiAction{Label: "更换", Path: "/vault/replace", Form: form("label", m.Label), Sensitive: true},
		}
		it.Menu = []uiAction{{Label: "删除", Path: "/vault/delete", Form: form("label", m.Label),
			Confirm: "删除「" + m.Label + "」？删除后找不回来。", Destructive: true, Sensitive: true}}
		sec.Items = append(sec.Items, it)
	}
	p.Sections = append(p.Sections, sec)
	return p
}

// ---- 记忆 ----

func uiMemory(d memoryData) uiPage {
	p := uiPage{Title: "记忆"}

	if pr := d.Proposal; pr != nil {
		sec := uiSection{Title: "助手的修改建议", Footer: "确认后才会真的改。"}
		if pr.Note != "" {
			sec.Items = append(sec.Items, textItem(pr.Note))
		}
		f := map[string][]string{}
		for i, c := range pr.Changes {
			sec.Items = append(sec.Items, row(yesNoText(c.Op == "delete", "删除：", "改为：")+clip(joinNonEmpty(" → ", c.Old, c.Value), 120), c.Reason))
			idx := strconv.Itoa(i)
			f["pick"] = append(f["pick"], idx)
			f["op_"+idx] = []string{c.Op}
			f["id_"+idx] = []string{c.ID}
			f["value_"+idx] = []string{c.Value}
		}
		if len(pr.Changes) > 0 {
			sec.Items = append(sec.Items, uiItem{Kind: "button", Title: "确认全部修改", Icon: "checkmark.circle.fill",
				Action: &uiAction{Label: "确认全部修改", Path: "/memory/apply", Form: f, Confirm: "按上面的建议修改这 " + strconv.Itoa(len(pr.Changes)) + " 条记忆？"}})
		}
		p.Sections = append(p.Sections, sec)
	}

	head := uiSection{Title: "整理"}
	head.Items = append(head.Items,
		uiItem{Kind: "button", Title: "让助手按一句话改记忆", Icon: "wand.and.stars", Form: &uiForm{
			Title:  "改记忆",
			Intro:  "说一句话，助手对照已有的记忆给出修改建议，你确认后才会改。",
			Fields: []uiField{{Key: "ask", Label: "你想改什么", Type: "multiline", Placeholder: "例如：删除关于前公司的内容；我的生日改为 5 月 3 日", Required: true}},
			Submit: uiAction{Label: "生成建议", Path: "/memory/ask"},
		}},
		uiItem{Kind: "button", Title: "重新生成记忆摘要", Icon: "arrow.clockwise", Action: &uiAction{Label: "重新生成", Path: "/memory/profile"}},
	)
	p.Sections = append(p.Sections, head)

	if d.Profile != "" {
		p.Sections = append(p.Sections, uiSection{Title: "记忆摘要", Footer: "生成于 " + ago(d.ProfileAt), Items: []uiItem{textItem(d.Profile)}})
	}

	if len(d.Pending) > 0 {
		sec := uiSection{Title: "待核实", Footer: "助手整理出来、等你确认的记忆。点一行是确认，左滑可以否决。"}
		for _, pd := range d.Pending {
			c := pd.Candidate
			it := row(clip(c.Value, 140), joinNonEmpty(" · ", string(c.Kind), c.Subject, c.Domain, c.Attribute))
			if q := joinNonEmpty("\n", pd.Arb.Challenge, pd.Arb.Reason); q != "" {
				it.Subtitle = clip(q, 200)
			}
			confirm := uiAction{Label: "确认", Path: "/memory/confirm", Form: form("id", pd.ID)}
			it.Action = &confirm
			it.Menu = []uiAction{confirm, {Label: "否决", Path: "/memory/dismiss", Form: form("id", pd.ID), Destructive: true}}
			sec.Items = append(sec.Items, it)
		}
		p.Sections = append(p.Sections, sec)
	}

	sec := uiSection{Title: "已保存的记忆", Footer: "共 " + strconv.Itoa(len(d.Memories)) + " 条。"}
	if len(d.Memories) == 0 {
		sec.Items = append(sec.Items, textItem("还没有记忆。"))
	}
	for _, m := range d.Memories {
		it := row(clip(m.Current.Value, 140), joinNonEmpty(" · ", string(m.Kind), m.Subject, m.Domain, m.Attribute))
		switch {
		case m.Inferred():
			it.Badge, it.Tone = "未经确认", "warn"
			it.Menu = append(it.Menu, uiAction{Label: "确认是这样", Path: "/memory/affirm", Form: form("id", m.ID)})
		case m.Status == memory.StatusDisputed:
			it.Badge, it.Tone = "有分歧", "bad"
		}
		it.Menu = append(it.Menu, uiAction{Label: "删除", Path: "/memory/forget", Form: form("id", m.ID),
			Confirm: "删除这条记忆？会被归档，不再被使用。", Destructive: true})
		sec.Items = append(sec.Items, it)
	}
	p.Sections = append(p.Sections, sec)
	return p
}

// ---- 模型 ----

var thinkingOptions = opts("", "默认", "off", "关闭", "low", "低", "medium", "中", "high", "高", "max", "最高")

var inputOptions = opts("text", "文字", "image", "图片", "audio", "语音", "video", "视频")

func uiModels(d modelsData) uiPage {
	p := uiPage{Title: "模型"}
	var provOpts []uiOption
	for _, pv := range d.Providers {
		provOpts = append(provOpts, uiOption{Value: pv.Name, Label: pv.Name})
	}
	protocol := opts("", "沿用现有设置（新增时为 OpenAI 兼容）", "openai", "OpenAI 兼容", "anthropic", "Anthropic 兼容")

	// 主 AI
	main := uiSection{Title: "主 AI", Footer: "负责规划、评估和审核。"}
	mainItem := row(joinNonEmpty(" / ", d.MainProvider, d.MainModel), "思考强度："+thinkingName(d.MainThinking)+"　输入："+strings.Join(d.MainInputs, "、"))
	mainItem.Form = &uiForm{
		Title: "设置主 AI",
		Fields: []uiField{
			{Key: "provider", Label: "模型服务", Type: "select", Options: provOpts, Value: d.MainProvider},
			{Key: "model", Label: "模型", Type: "text", Value: d.MainModel, Placeholder: "deepseek-chat", Required: true},
			{Key: "thinking", Label: "思考强度", Type: "select", Options: thinkingOptions, Value: d.MainThinking},
			{Key: "inputs", Label: "支持的输入", Type: "multiselect", Options: inputOptions, Values: d.MainInputs},
		},
		Submit: uiAction{Label: "保存", Path: "/models/main"},
	}
	main.Items = append(main.Items, mainItem)
	main.Items = append(main.Items, uiItem{Kind: "button", Title: "连接测试", Icon: "bolt.horizontal",
		Action: &uiAction{Label: "连接测试", Path: "/models/test", Form: form("provider", d.MainProvider, "model", d.MainModel)}})
	p.Sections = append(p.Sections, main)

	// 模型服务
	prov := uiSection{Title: "模型服务", Footer: "密钥只存进保险库，保存后不能查看。"}
	for _, pv := range d.Providers {
		it := row(pv.Name, joinNonEmpty(" · ", pv.BaseURL, yesNoText(pv.Protocol == "anthropic", "Anthropic 兼容", "OpenAI 兼容")))
		it.Value = pv.KeyState
		it.Tone = yesNoText(pv.KeyOK, "ok", "bad")
		it.Form = providerForm(pv.Name, pv.BaseURL, pv.Protocol, protocol)
		it.Menu = []uiAction{{Label: "删除", Path: "/models/provider/delete", Form: form("name", pv.Name),
			Confirm: "删除模型服务「" + pv.Name + "」？", Destructive: true, Sensitive: true}}
		prov.Items = append(prov.Items, it)
	}
	prov.Items = append(prov.Items, uiItem{Kind: "button", Title: "添加模型服务", Icon: "plus.circle", Form: providerForm("", "", "", protocol)})
	p.Sections = append(p.Sections, prov)

	// 协作模型
	agents := uiSection{Title: "协作模型", Footer: "配了协作模型后，事情由评分高的模型办，主模型评估和审核。"}
	for _, a := range d.Agents {
		it := row(a.Name, joinNonEmpty(" · ", a.Provider+" / "+a.Model, a.Note))
		it.Form = agentForm(a.Name, a.Note, a.Provider, a.Model, a.Tier, a.Thinking, a.Inputs, a.GoodAt, provOpts, categoryOptions(d))
		it.Menu = []uiAction{
			{Label: "连接测试", Path: "/models/test", Form: form("provider", a.Provider, "model", a.Model)},
			{Label: "删除", Path: "/models/agent/delete", Form: form("name", a.Name), Confirm: "删除协作模型「" + a.Name + "」？", Destructive: true},
		}
		agents.Items = append(agents.Items, it)
	}
	agents.Items = append(agents.Items, uiItem{Kind: "button", Title: "添加协作模型", Icon: "plus.circle",
		Form: agentForm("", "", d.MainProvider, "", "", "", nil, nil, provOpts, categoryOptions(d))})
	p.Sections = append(p.Sections, agents)

	// 成绩
	for _, st := range d.Standings {
		if len(st.Rows) == 0 {
			continue
		}
		sec := uiSection{Title: "成绩 · " + st.Category.Name}
		for _, r := range st.Rows {
			it := row(r.Worker.Name, fmt.Sprintf("做过 %d 份", r.Tally.Runs))
			it.Value = fmt.Sprintf("%.0f 分", r.Tally.Score()*100)
			if r.Next {
				it.Badge, it.Tone = "下一份派给它", "info"
			}
			sec.Items = append(sec.Items, it)
		}
		p.Sections = append(p.Sections, sec)
	}

	// 预算
	budget := uiSection{Title: "每个任务的预算上限"}
	budgetItem := row("token 上限 / 金额上限 / 执行时长", fmt.Sprintf("%d token · ¥%.2f · %.1f 小时", d.BudgetTokens, d.BudgetYuan, d.BudgetHours))
	budgetItem.Form = &uiForm{
		Title: "预算上限",
		Fields: []uiField{
			{Key: "max_tokens", Label: "token 上限", Type: "number", Value: strconv.FormatInt(d.BudgetTokens, 10)},
			{Key: "max_cost_yuan", Label: "金额上限（元）", Type: "number", Value: strconv.FormatFloat(d.BudgetYuan, 'f', -1, 64)},
			{Key: "max_hours", Label: "执行时长上限（小时）", Type: "number", Value: strconv.FormatFloat(d.BudgetHours, 'f', -1, 64)},
		},
		Submit: uiAction{Label: "保存", Path: "/models/budget"},
	}
	budget.Items = append(budget.Items, budgetItem)
	p.Sections = append(p.Sections, budget)
	return p
}

func providerForm(name, base, protocol string, protocolOpts []uiOption) *uiForm {
	title := yesNoText(name == "", "添加模型服务", "修改「"+name+"」")
	return &uiForm{
		Title: title,
		Intro: "密钥留空表示不改；填了就替换。填写测试用的模型后，保存时会同时测试连接。",
		Fields: []uiField{
			{Key: "name", Label: "名称", Type: "text", Value: name, Placeholder: "deepseek", Required: true},
			{Key: "base_url", Label: "接口地址", Type: "url", Value: base, Placeholder: "https://api.deepseek.com/v1", Required: true},
			{Key: "protocol", Label: "接口协议", Type: "select", Options: protocolOpts, Value: protocol},
			{Key: "key", Label: "API Key", Type: "secure"},
			{Key: "test_model", Label: "连接测试所用的模型", Type: "text", Placeholder: "填写后保存时同时测试连接"},
		},
		Submit: uiAction{Label: "保存", Path: "/models/provider", Sensitive: true},
	}
}

func categoryOptions(d modelsData) []uiOption {
	var out []uiOption
	for _, c := range d.Categories {
		out = append(out, uiOption{Value: string(c.ID), Label: c.Name})
	}
	return out
}

func agentForm(name, note, provider, model, tier, thinking string, inputs, goodAt []string, provOpts, catOpts []uiOption) *uiForm {
	return &uiForm{
		Title: yesNoText(name == "", "添加协作模型", "修改「"+name+"」"),
		Intro: "改名等于换了一个新的：成绩记在名字下。",
		Fields: []uiField{
			{Key: "name", Label: "名称", Type: "text", Value: name, Placeholder: "写作模型", Required: true},
			{Key: "note", Label: "备注", Type: "text", Value: note, Placeholder: "长文写作，成本较低"},
			{Key: "provider", Label: "模型服务", Type: "select", Options: provOpts, Value: provider},
			{Key: "model", Label: "模型", Type: "text", Value: model, Placeholder: "moonshot-v1-32k", Required: true},
			{Key: "tier", Label: "任务难度", Type: "select", Options: opts("", "不限", "simple", "简单", "complex", "复杂"), Value: tier},
			{Key: "thinking", Label: "思考强度", Type: "select", Options: thinkingOptions, Value: thinking},
			{Key: "inputs", Label: "支持的输入", Type: "multiselect", Options: inputOptions, Values: inputs},
			{Key: "good_at", Label: "任务类别限制（不选表示不限，按评分分派）", Type: "multiselect", Options: catOpts, Values: goodAt},
		},
		Submit: uiAction{Label: "保存", Path: "/models/agent"},
	}
}

// ---- 消息源 ----

func uiChannels(d channelsData) uiPage {
	p := uiPage{Title: "消息源"}
	var typeOpts []uiOption
	for _, t := range d.Types {
		if t != "weixin" {
			typeOpts = append(typeOpts, uiOption{Value: t, Label: t})
		}
	}
	p.Toolbar = []uiItem{{Kind: "button", Title: "添加", Icon: "plus", Menu: nil, Form: &uiForm{
		Title: "添加消息源",
		Intro: "填机器人的 AppID 和密钥。微信不用填这些，用下面的「添加微信」扫码连接。",
		Fields: []uiField{
			{Key: "type", Label: "类型", Type: "select", Options: typeOpts},
			{Key: "name", Label: "名称", Type: "text", Placeholder: "如 qq", Required: true},
			{Key: "app_id", Label: "AppID", Type: "text", Placeholder: "102012345"},
			{Key: "secret", Label: "AppSecret", Type: "secure"},
			{Key: "sandbox", Label: "沙箱环境（仅 QQ 测试用）", Type: "toggle"},
		},
		Submit: uiAction{Label: "保存", Path: "/channels", Sensitive: true},
	}}}

	if len(d.Channels) == 0 {
		p.Sections = append(p.Sections, uiSection{Items: []uiItem{textItem("还没有接入消息源。")}})
	}
	for _, c := range d.Channels {
		sec := uiSection{Title: c.Name + "（" + c.Type + "）"}
		info := row("凭据", yesNoText(c.Type == "weixin", "扫码登录得到", "AppID "+c.AppID))
		if !c.SecretOK && c.Type != "weixin" {
			info.Badge, info.Tone = "缺凭据", "bad"
		}
		if c.Type != "weixin" {
			info.Form = &uiForm{
				Title: "修改「" + c.Name + "」", Intro: "密钥留空表示不改。",
				Fields: []uiField{
					{Key: "app_id", Label: "AppID", Type: "text", Value: c.AppID},
					{Key: "secret", Label: "AppSecret", Type: "secure"},
				},
				Submit: uiAction{Label: "保存", Path: "/channels", Form: form("type", c.Type, "name", c.Name), Sensitive: true},
			}
		} else {
			info.Action = &uiAction{Label: "重新扫码连接", Path: "/channels/weixin/start", Form: form("name", c.Name), Sensitive: true}
		}
		sec.Items = append(sec.Items, info)
		for _, o := range c.Owners {
			it := row("主人", o)
			it.Menu = []uiAction{{Label: "移除", Path: "/channels/owner", Form: form("name", c.Name, "owner", o, "remove", "1"),
				Confirm: "移除这位主人？他发来的消息之后不再被处理。", Destructive: true, Sensitive: true}}
			sec.Items = append(sec.Items, it)
		}
		if len(c.Owners) == 0 {
			sec.Items = append(sec.Items, textItem("还没有主人：给机器人发一句话，它会把你的账号标识回给你，填到下面。"))
		}
		sec.Items = append(sec.Items,
			uiItem{Kind: "button", Title: "添加主人", Icon: "person.badge.plus", Form: &uiForm{
				Title:  "添加主人",
				Fields: []uiField{{Key: "owner", Label: "账号标识", Type: "text", Placeholder: "机器人回复给你的那一串字符", Required: true}},
				Submit: uiAction{Label: "添加", Path: "/channels/owner", Form: form("name", c.Name), Sensitive: true},
			}},
			uiItem{Kind: "button", Title: "删除这个消息源", Icon: "trash", Action: &uiAction{Label: "删除", Path: "/channels/delete", Form: form("name", c.Name),
				Confirm: "删除消息源「" + c.Name + "」？", Destructive: true, Sensitive: true}},
		)
		p.Sections = append(p.Sections, sec)
	}
	p.Sections = append(p.Sections, uiSection{Title: "微信", Items: []uiItem{{Kind: "button", Title: "添加微信（扫码）", Icon: "qrcode.viewfinder", Form: &uiForm{
		Title: "添加微信", Intro: "起个名字，下一步显示一个二维码，用微信扫描确认。扫码的微信号会成为这个消息源的主人。",
		Fields: []uiField{{Key: "name", Label: "名称", Type: "text", Placeholder: "weixin", Required: true}},
		Submit: uiAction{Label: "获取二维码", Path: "/channels/weixin/start", Sensitive: true},
	}}}})
	return p
}

func uiWeixin(d weixinData) uiPage {
	return uiPage{Title: "微信扫码连接", Sections: []uiSection{
		{Title: "登录二维码", Footer: "用手机微信扫描并确认授权。扫码的微信号会成为「" + d.Name + "」的主人。二维码几分钟内有效。",
			Items: []uiItem{{Kind: "qr", Text: d.Link}}},
		{Items: []uiItem{
			{Kind: "button", Title: "检查连接状态", Icon: "arrow.clockwise", Action: &uiAction{Label: "检查", Path: "/channels/weixin/poll", Form: form("name", d.Name), Sensitive: true}},
			{Kind: "button", Title: "输入手机上显示的验证数字", Icon: "number", Form: &uiForm{
				Title:  "验证数字",
				Intro:  "仅当状态提示需要输入手机上显示的数字时填写。",
				Fields: []uiField{{Key: "code", Label: "验证数字", Type: "number", Placeholder: "按提示填写"}},
				Submit: uiAction{Label: "提交", Path: "/channels/weixin/poll", Form: form("name", d.Name), Sensitive: true},
			}},
		}},
	}}
}

// ---- 数据源 ----

func uiConnectors(d connectorsData) uiPage {
	p := uiPage{Title: "数据源"}

	// 日历
	cal := uiSection{Title: "日历（CalDAV）", Footer: "iCloud 要用「App 专用密码」，不是 Apple ID 的登录密码。密码只存进保险库。"}
	for _, c := range d.Calendars {
		it := row(c.Name, joinNonEmpty(" · ", c.Username, strings.Join(c.Tags, "、")))
		if it.Title == "" {
			it.Title = c.Username
		}
		it.Badge, it.Tone = yesNoText(c.Key, "", "缺密码"), "bad"
		it.Form = calendarForm(c.Name, strings.Join(c.Tags, "，"), c.URL, c.Username, c.DefaultCalendar)
		it.Menu = []uiAction{
			{Label: "读取日历（测试）", Path: "/connectors/test", Form: form("which", "calendar", "orig", c.Name)},
			{Label: "停用", Path: "/connectors/disable", Form: form("which", "calendar", "name", c.Name), Confirm: "停用日历「" + c.Name + "」？", Destructive: true},
		}
		cal.Items = append(cal.Items, it)
	}
	cal.Items = append(cal.Items, uiItem{Kind: "button", Title: "添加日历账号", Icon: "plus.circle", Form: calendarForm("", "", "https://caldav.icloud.com", "", "")})
	p.Sections = append(p.Sections, cal)

	// 邮箱
	mail := uiSection{Title: "邮箱（IMAP / SMTP）", Footer: "不填发信服务器，助手只能读和整理，不能发信。"}
	for _, m := range d.Mailboxes {
		it := row(yesNoText(m.Label != "", m.Label, m.Username), joinNonEmpty(" · ", m.Username, m.IMAP, strings.Join(m.Tags, "、")))
		it.Badge, it.Tone = yesNoText(m.Key, "", "缺密码"), "bad"
		it.Form = mailForm(m.Label, strings.Join(m.Tags, "，"), m.IMAP, m.SMTP, m.Username, m.From, m.Name)
		it.Menu = []uiAction{
			{Label: "连接测试", Path: "/connectors/test", Form: form("which", "mail", "orig", m.Label)},
			{Label: "停用", Path: "/connectors/disable", Form: form("which", "mail", "name", m.Label), Confirm: "停用邮箱「" + m.Label + "」？", Destructive: true},
		}
		mail.Items = append(mail.Items, it)
	}
	mail.Items = append(mail.Items, uiItem{Kind: "button", Title: "添加邮箱", Icon: "plus.circle",
		Form: mailForm("", "", "imap.mail.me.com:993", "smtp.mail.me.com:587", "", "", "")})
	p.Sections = append(p.Sections, mail)

	// 节假日
	hol := uiSection{Title: "节假日订阅"}
	for _, h := range d.Holidays {
		it := row(h.Name, joinNonEmpty(" · ", h.URL, strings.Join(h.Tags, "、")))
		it.Form = holidaysForm(h.Name, strings.Join(h.Tags, "，"), h.URL)
		it.Menu = []uiAction{{Label: "停用", Path: "/connectors/disable", Form: form("which", "holidays", "name", h.Name), Confirm: "停用节假日订阅「" + h.Name + "」？", Destructive: true}}
		hol.Items = append(hol.Items, it)
	}
	hol.Items = append(hol.Items, uiItem{Kind: "button", Title: "添加节假日订阅", Icon: "plus.circle",
		Form: holidaysForm("", "", "https://calendars.icloud.com/holidays/cn_zh.ics")})
	p.Sections = append(p.Sections, hol)

	// 天气、搜索
	weather := row("天气（和风天气）", yesNoText(d.Weather != nil, weatherHost(d.Weather), "未启用"))
	weather.Badge, weather.Tone = yesNoText(d.Weather == nil || d.WeatherKey, "", "缺密钥"), "bad"
	weather.Form = &uiForm{
		Title: "天气（和风天气）",
		Fields: []uiField{
			{Key: "host", Label: "API Host", Type: "text", Value: weatherHost(d.Weather), Placeholder: "abcxyz.qweatherapi.com", Required: true},
			{Key: "key", Label: "API KEY", Type: "secure", Footer: "留空表示不改"},
		},
		Submit: uiAction{Label: "保存", Path: "/connectors/weather", Sensitive: true},
	}
	weather.Menu = []uiAction{{Label: "连接测试", Path: "/connectors/test", Form: form("which", "weather")}}
	if d.Weather != nil {
		weather.Menu = append(weather.Menu, uiAction{Label: "停用", Path: "/connectors/disable", Form: form("which", "weather"), Destructive: true, Confirm: "停用天气？"})
	}
	search := row("联网搜索（DeepSeek）", yesNoText(d.Search != nil, "已启用", "未启用"))
	search.Badge, search.Tone = yesNoText(d.Search == nil || d.SearchKey, "", "缺密钥"), "bad"
	search.Form = &uiForm{
		Title:  "联网搜索（DeepSeek 搜索接口）",
		Fields: []uiField{{Key: "key", Label: "DeepSeek API Key", Type: "secure", Required: true}},
		Submit: uiAction{Label: "保存", Path: "/connectors/search", Sensitive: true},
	}
	search.Menu = []uiAction{{Label: "连接测试", Path: "/connectors/test", Form: form("which", "search")}}
	if d.Search != nil {
		search.Menu = append(search.Menu, uiAction{Label: "停用", Path: "/connectors/disable", Form: form("which", "search"), Destructive: true, Confirm: "停用联网搜索？"})
	}
	p.Sections = append(p.Sections, uiSection{Title: "天气与搜索", Items: []uiItem{weather, search}})
	return p
}

func calendarForm(name, tags, urlv, user, def string) *uiForm {
	return &uiForm{
		Title: yesNoText(name == "", "添加日历账号", "修改「"+name+"」"),
		Intro: "密码留空表示不改。iCloud 要用「App 专用密码」。",
		Fields: []uiField{
			{Key: "name", Label: "名称", Type: "text", Value: name, Placeholder: "iCloud", Required: true},
			{Key: "tags", Label: "用途标签", Type: "text", Value: tags, Placeholder: "个人，家庭"},
			{Key: "url", Label: "服务器地址", Type: "url", Value: urlv, Required: true},
			{Key: "username", Label: "账号", Type: "text", Value: user, Placeholder: "you@icloud.com", Required: true},
			{Key: "password", Label: "密码", Type: "secure"},
			{Key: "default_calendar", Label: "默认日历（可不填）", Type: "text", Value: def},
		},
		Submit: uiAction{Label: "保存", Path: "/connectors/calendar", Form: form("orig", name), Sensitive: true},
	}
}

func mailForm(label, tags, imap, smtp, user, from, dname string) *uiForm {
	return &uiForm{
		Title: yesNoText(label == "", "添加邮箱", "修改「"+label+"」"),
		Intro: "密码留空表示不改。",
		Fields: []uiField{
			{Key: "name", Label: "名称", Type: "text", Value: label, Placeholder: "工作邮箱", Required: true},
			{Key: "tags", Label: "用途标签", Type: "text", Value: tags, Placeholder: "工作，客户"},
			{Key: "imap", Label: "收信服务器（IMAP）", Type: "text", Value: imap, Required: true},
			{Key: "smtp", Label: "发信服务器（SMTP）", Type: "text", Value: smtp},
			{Key: "username", Label: "账号", Type: "text", Value: user, Required: true},
			{Key: "password", Label: "密码", Type: "secure"},
			{Key: "from", Label: "发信地址", Type: "text", Value: from},
			{Key: "display_name", Label: "发信人名称", Type: "text", Value: dname},
		},
		Submit: uiAction{Label: "保存", Path: "/connectors/mail", Form: form("orig", label), Sensitive: true},
	}
}

func holidaysForm(name, tags, urlv string) *uiForm {
	return &uiForm{
		Title: yesNoText(name == "", "添加节假日订阅", "修改「"+name+"」"),
		Fields: []uiField{
			{Key: "name", Label: "名称", Type: "text", Value: name, Placeholder: "中国大陆节假日", Required: true},
			{Key: "tags", Label: "用途标签", Type: "text", Value: tags, Placeholder: "法定假日"},
			{Key: "url", Label: "订阅地址", Type: "url", Value: urlv, Required: true},
		},
		Submit: uiAction{Label: "保存", Path: "/connectors/holidays", Form: form("orig", name)},
	}
}

// ---- MCP ----

func uiMCP(d mcpData) uiPage {
	p := uiPage{Title: "MCP"}
	secretIn := opts("query", "查询参数", "header", "请求头")
	p.Toolbar = []uiItem{{Kind: "button", Title: "添加", Icon: "plus", Form: &uiForm{
		Title: "添加 MCP server",
		Fields: []uiField{
			{Key: "name", Label: "名称", Type: "text", Placeholder: "flight", Required: true},
			{Key: "transport", Label: "连接方式", Type: "select", Options: opts("http", "远程（Streamable HTTP）", "command", "本地进程（stdio）"), Value: "http"},
			{Key: "url", Label: "服务器地址（远程）", Type: "url", Placeholder: "https://mcp.example.com/mcp"},
			{Key: "command", Label: "启动命令（本地）", Type: "text", Placeholder: "npx"},
			{Key: "args", Label: "命令参数（本地）", Type: "text", Placeholder: "-y @scope/some-mcp-server"},
			{Key: "secret_name", Label: "密钥参数名", Type: "text", Placeholder: "key"},
			{Key: "secret_in", Label: "密钥位置", Type: "select", Options: secretIn, Value: "query"},
			{Key: "key", Label: "密钥", Type: "secure"},
		},
		Submit: uiAction{Label: "保存", Path: "/mcp/server", Sensitive: true},
	}}}

	// 预置
	pre := uiSection{Title: "预置服务", Footer: "固定接入参数的常用服务，只要填一个密钥。"}
	for _, pv := range d.Presets {
		it := row(pv.Title, pv.Intro)
		it.Badge, it.Tone = yesNoText(pv.On, "已接入", ""), "ok"
		fields := []uiField{}
		if pv.KeyLabel != "" {
			fields = append(fields, uiField{Key: "key", Label: pv.KeyLabel, Type: "secure", Footer: pv.KeyHelp})
		}
		it.Form = &uiForm{Title: pv.Title, Intro: pv.Intro, Fields: fields, Submit: uiAction{Label: yesNoText(pv.On, "更新", "接入"), Path: "/mcp/" + pv.ID, Sensitive: true}}
		pre.Items = append(pre.Items, it)
	}
	p.Sections = append(p.Sections, pre)

	// 已接入的 server
	for _, sv := range d.Servers {
		sec := uiSection{Title: yesNoText(sv.Title != "", sv.Title+"（"+sv.Name+"）", sv.Name)}
		head := row("连接", joinNonEmpty(" · ", sv.Transport, joinNonEmpty(" ", sv.Command, strings.Join(sv.Args, " ")), sv.URL))
		head.Badge, head.Tone = yesNoText(sv.KeyOK, "", "缺密钥"), "bad"
		head.Menu = []uiAction{
			{Label: "读取工具", Path: "/mcp/sync", Form: form("name", sv.Name)},
			{Label: "移除", Path: "/mcp/server/delete", Form: form("name", sv.Name), Confirm: "移除 MCP server「" + sv.Name + "」？", Destructive: true, Sensitive: true},
		}
		head.Action = &head.Menu[0]
		sec.Items = append(sec.Items, head)
		for _, k := range sv.Keys {
			it := row("密钥 · "+k.Param, yesNoText(k.Header, "放在请求头里", "放在查询参数里"))
			it.Value, it.Tone = yesNoText(k.OK, "已保存", "未保存"), yesNoText(k.OK, "ok", "bad")
			it.Form = &uiForm{
				Title:  "更换密钥 " + k.Param,
				Fields: []uiField{{Key: "key", Label: "密钥", Type: "secure", Required: true}},
				Submit: uiAction{Label: "保存", Path: "/mcp/key", Form: form("name", sv.Name, "secret_name", k.Param), Sensitive: true},
			}
			sec.Items = append(sec.Items, it)
		}
		for _, t := range sv.Tools {
			it := row(t.Short, clip(t.Description, 120))
			it.Value = joinNonEmpty(" · ", sideLabel(t.SideEffect), privacyLabel(t.MaxPrivacy))
			it.Form = &uiForm{
				Title: "工具授权 · " + t.Short,
				Intro: clip(t.Description, 300),
				Fields: []uiField{
					{Key: "side", Label: "副作用等级", Type: "select", Options: toOptions(d.SideEffects), Value: t.SideEffect},
					{Key: "privacy", Label: "允许传给它的参数的最高隐私等级", Type: "select", Options: toOptions(d.PrivacyLevels), Value: t.MaxPrivacy},
				},
				Submit: uiAction{Label: "保存", Path: "/mcp/tools", Form: form("name", sv.Name, "tool", t.Name), Sensitive: true},
			}
			sec.Items = append(sec.Items, it)
		}
		p.Sections = append(p.Sections, sec)
	}

	// 助手自己装的
	if len(d.Installed) > 0 {
		sec := uiSection{Title: "助手自己装上的", Footer: "安装前都做过安全扫描。"}
		for _, m := range d.Installed {
			it := row(m.Name, joinNonEmpty(" · ", clip(m.Description, 80), m.URL))
			it.Menu = []uiAction{{Label: "移除", Path: "/mcp/installed/delete", Form: form("name", m.Name), Confirm: "移除「" + m.Name + "」？", Destructive: true, Sensitive: true}}
			sec.Items = append(sec.Items, it)
		}
		p.Sections = append(p.Sections, sec)
	}
	return p
}

func toOptions(in []toolOption) []uiOption {
	out := make([]uiOption, 0, len(in))
	for _, o := range in {
		out = append(out, uiOption{Value: o.Value, Label: o.Label})
	}
	return out
}

func sideLabel(v string) string {
	for _, o := range sideEffects {
		if o.Value == v {
			return o.Label
		}
	}
	return v
}

func privacyLabel(v string) string {
	for _, o := range privacyLevels {
		if o.Value == v {
			return "隐私上限：" + o.Label
		}
	}
	return v
}

// ---- Skill ----

func uiSkills(d skillsData) uiPage {
	p := uiPage{Title: "Skill"}
	p.Toolbar = []uiItem{{Kind: "button", Title: "安装", Icon: "plus", Form: &uiForm{
		Title:  "安装 Skill",
		Intro:  "把 Skill 清单（YAML）贴在这里。会先扫描，扫描不通过一律不装。",
		Fields: []uiField{{Key: "manifest", Label: "Skill 清单", Type: "multiline", Required: true}},
		Submit: uiAction{Label: "扫描并安装", Path: "/skills", Sensitive: true},
	}}}
	p.Sections = append(p.Sections, uiSection{Title: "让助手去找", Items: []uiItem{{Kind: "button", Title: "说需求，让助手找并安装", Icon: "magnifyingglass", Form: &uiForm{
		Title:  "让助手安装 Skill",
		Fields: []uiField{{Key: "want", Label: "需求描述", Type: "multiline", Placeholder: "例如：需要一个规划出行路线、查询沿途天气的 Skill", Required: true}},
		Submit: uiAction{Label: "交给助手", Path: "/skills/ask"},
	}}}})
	sec := uiSection{Title: "已安装", Footer: "装上的 Skill 不带来新权限：按它执行的任务只能用它声明过、且你授权过的工具。"}
	if len(d.Skills) == 0 {
		sec.Items = append(sec.Items, textItem("还没有安装 Skill。"))
	}
	for _, s := range d.Skills {
		it := row(yesNoText(s.Name != "", s.Name, s.ID), clip(s.Goal, 140))
		it.Value = "v" + s.Version
		it.Menu = []uiAction{{Label: "移除", Path: "/skills/delete", Form: form("id", s.ID), Confirm: "移除 Skill「" + s.Name + "」？", Destructive: true}}
		sec.Items = append(sec.Items, it)
	}
	p.Sections = append(p.Sections, sec)
	return p
}

// ---- 系统提示词 ----

func uiPrompts(d promptsData) uiPage {
	p := uiPage{Title: "系统提示词"}
	sec := uiSection{Title: "内置的各段", Footer: "改写后会覆盖内置内容；必需的段落不能停用。"}
	for _, sp := range d.Specs {
		it := row(sp.Title, clip(sp.Usage, 100))
		switch {
		case !sp.On:
			it.Badge, it.Tone = "已停用", "warn"
		case sp.Custom:
			it.Badge, it.Tone = "已改写", "info"
		}
		it.Form = &uiForm{
			Title: sp.Title, Intro: sp.Usage,
			Fields: []uiField{{Key: "text", Label: "内容", Type: "multiline", Value: sp.Text}},
			Submit: uiAction{Label: "保存", Path: "/prompts", Form: form("key", sp.Key), Sensitive: true},
		}
		if sp.Custom {
			it.Menu = append(it.Menu, uiAction{Label: "恢复内置内容", Path: "/prompts", Form: form("key", sp.Key, "reset", "1"), Confirm: "恢复成内置的内容？", Sensitive: true})
		}
		if !sp.Required {
			it.Menu = append(it.Menu, uiAction{Label: yesNoText(sp.On, "停用", "启用"), Path: "/prompts/toggle", Form: form("key", sp.Key), Sensitive: true})
		}
		sec.Items = append(sec.Items, it)
	}
	p.Sections = append(p.Sections, sec)

	extra := uiSection{Title: "我自己添加的", Footer: "补充你自己的约定，会和内置的一起加载。"}
	for _, e := range d.Extras {
		it := row(e.Title, clip(e.Text, 100))
		if e.Off {
			it.Badge, it.Tone = "已停用", "warn"
		}
		it.Form = &uiForm{
			Title: e.Title,
			Fields: []uiField{
				{Key: "title", Label: "标题", Type: "text", Value: e.Title, Required: true},
				{Key: "text", Label: "内容", Type: "multiline", Value: e.Text, Required: true},
			},
			Submit: uiAction{Label: "保存", Path: "/prompts/extra", Form: form("id", e.ID), Sensitive: true},
		}
		it.Menu = []uiAction{
			{Label: yesNoText(e.Off, "启用", "停用"), Path: "/prompts/extra/toggle", Form: form("id", e.ID), Sensitive: true},
			{Label: "删除", Path: "/prompts/extra/delete", Form: form("id", e.ID), Confirm: "删除「" + e.Title + "」？", Destructive: true, Sensitive: true},
		}
		extra.Items = append(extra.Items, it)
	}
	extra.Items = append(extra.Items, uiItem{Kind: "button", Title: "添加一段", Icon: "plus.circle", Form: &uiForm{
		Title: "添加一段",
		Fields: []uiField{
			{Key: "title", Label: "标题", Type: "text", Placeholder: "例如：Calendar conventions", Required: true},
			{Key: "text", Label: "内容", Type: "multiline", Placeholder: "- Put course events in the 个人 calendar.", Required: true},
		},
		Submit: uiAction{Label: "保存", Path: "/prompts/extra", Sensitive: true},
	}})
	p.Sections = append(p.Sections, extra)
	return p
}

// ---- 登录与安全 ----

func uiSecurity(d securityData) uiPage {
	p := uiPage{Title: "登录与安全"}
	methods := uiSection{Title: "登录方式", Footer: "手机上改登录方式前会再验证一次面容 ID。通行密钥的登记必须在电脑浏览器里做。"}
	m := row("网页后台的登录方式", joinNonEmpty(" · ", yesNoText(d.PasswordTOTP, "密码 + 验证码", ""), yesNoText(d.Passkey, "通行密钥", "")))
	if !d.PasswordTOTP && !d.Passkey {
		m.Subtitle, m.Badge, m.Tone = "还没有设置", "只凭密码", "warn"
	}
	m.Form = &uiForm{
		Title: "登录方式",
		Fields: []uiField{
			{Key: "password_totp", Label: "密码 + 动态验证码", Type: "toggle", Value: yesNoText(d.PasswordTOTP, "1", "")},
			{Key: "passkey", Label: "通行密钥", Type: "toggle", Value: yesNoText(d.Passkey, "1", "")},
		},
		Submit: uiAction{Label: "保存", Path: "/security/methods", Sensitive: true},
	}
	methods.Items = append(methods.Items, m)
	p.Sections = append(p.Sections, methods)

	totp := uiSection{Title: "动态验证码"}
	switch {
	case d.PendingSecret != "":
		totp.Items = append(totp.Items,
			textItem("在验证器 App 里扫这个二维码或手动输入密钥，然后填下面的 6 位验证码完成绑定。"),
			uiItem{Kind: "mono", Title: "密钥", Text: d.PendingSecret},
			uiItem{Kind: "button", Title: "输入 6 位验证码", Icon: "number", Form: &uiForm{
				Title:  "确认绑定",
				Fields: []uiField{{Key: "code", Label: "6 位验证码", Type: "number", Required: true}},
				Submit: uiAction{Label: "确认", Path: "/security/totp/confirm", Sensitive: true},
			}},
			uiItem{Kind: "button", Title: "取消绑定", Icon: "xmark", Action: &uiAction{Label: "取消", Path: "/security/totp/cancel", Destructive: true}},
		)
	case d.TOTPBound:
		totp.Items = append(totp.Items, uiItem{Kind: "button", Title: "解除验证器绑定", Icon: "minus.circle",
			Action: &uiAction{Label: "解除", Path: "/security/totp/remove", Confirm: "解除验证器绑定？", Destructive: true, Sensitive: true}})
	default:
		totp.Items = append(totp.Items, uiItem{Kind: "button", Title: "开始绑定验证器", Icon: "qrcode", Action: &uiAction{Label: "开始", Path: "/security/totp/start", Sensitive: true}})
	}
	p.Sections = append(p.Sections, totp)

	p.Sections = append(p.Sections, uiSection{Title: "登录密码", Items: []uiItem{{Kind: "button", Title: "修改登录密码", Icon: "key", Form: &uiForm{
		Title: "修改登录密码",
		Fields: []uiField{
			{Key: "current", Label: "当前密码", Type: "secure", Required: true},
			{Key: "new", Label: "新密码", Type: "secure", Required: true},
			{Key: "confirm", Label: "确认新密码", Type: "secure", Required: true},
		},
		Submit: uiAction{Label: "修改", Path: "/password", Sensitive: true},
	}}}})

	keys := uiSection{Title: "通行密钥", Footer: yesNoText(d.PasskeyBlocked != "", d.PasskeyBlocked, "域名："+d.PasskeyHost)}
	for _, k := range d.Passkeys {
		it := row(k.Name, joinNonEmpty(" · ", k.RPID, "登记 "+ago(k.CreatedAt), "最近使用 "+ago(k.LastUsedAt)))
		it.Menu = []uiAction{{Label: "删除", Path: "/security/passkey/remove", Form: form("id", k.ID), Confirm: "删除通行密钥「" + k.Name + "」？", Destructive: true, Sensitive: true}}
		keys.Items = append(keys.Items, it)
	}
	if len(d.Passkeys) == 0 {
		keys.Items = append(keys.Items, textItem("还没有通行密钥。"))
	}
	p.Sections = append(p.Sections, keys)
	return p
}

// ---- 日志 ----

func uiLogs(d logsData) uiPage {
	p := uiPage{Title: "日志"}
	filter := uiSection{Title: "筛选"}
	catOpts := []uiOption{{Value: "", Label: "全部"}}
	for _, c := range d.Categories {
		catOpts = append(catOpts, uiOption{Value: string(c), Label: string(c)})
	}
	filter.Items = append(filter.Items, uiItem{Kind: "button", Title: "类别：" + yesNoText(d.Category == "", "全部", string(d.Category)), Icon: "line.3.horizontal.decrease.circle", Form: &uiForm{
		Title: "筛选日志", Fields: []uiField{
			{Key: "cat", Label: "类别", Type: "select", Options: catOpts, Value: string(d.Category)},
			{Key: "level", Label: "级别", Type: "select", Options: opts("", "全部", "warn", "警告及以上", "error", "仅错误"), Value: d.Level},
		},
		Submit: uiAction{Label: "查看", Path: "/logs", Get: true},
	}})
	p.Sections = append(p.Sections, filter)

	sec := uiSection{Title: "最近的操作"}
	if len(d.Entries) == 0 {
		sec.Items = append(sec.Items, textItem("没有符合条件的日志。"))
	}
	for _, e := range d.Entries {
		it := row(e.Message, joinNonEmpty(" · ", ago(e.At), string(e.Category), clip(e.Attrs, 160)))
		switch {
		case e.Level >= 8:
			it.Badge, it.Tone = "错误", "bad"
		case e.Level >= 4:
			it.Badge, it.Tone = "警告", "warn"
		}
		sec.Items = append(sec.Items, it)
	}
	if d.Next != "" {
		sec.Items = append(sec.Items, uiItem{Kind: "button", Title: "更早的", Icon: "chevron.down.circle",
			Page: "/logs?" + url.Values{"cat": {string(d.Category)}, "level": {d.Level}, "before": {d.Next}}.Encode()})
	}
	p.Sections = append(p.Sections, sec)

	p.Sections = append(p.Sections, uiSection{Title: "清理", Items: []uiItem{{Kind: "button", Title: "删除一段日期的日志", Icon: "trash", Form: &uiForm{
		Title: "删除日志", Intro: "按类别和日期范围删除，删掉的找不回来。",
		Fields: []uiField{
			{Key: "cat", Label: "类别", Type: "select", Options: catOpts, Value: string(d.Category)},
			{Key: "from", Label: "从（含）", Type: "date"},
			{Key: "to", Label: "到（含）", Type: "date"},
		},
		Submit: uiAction{Label: "删除", Path: "/logs/delete", Confirm: "删除符合条件的日志？", Destructive: true},
	}}}})
	return p
}

// ---- 文件 ----

func uiFiles(d filesData) uiPage {
	p := uiPage{Title: "文件"}
	if !d.Enabled {
		p.Sections = append(p.Sections, uiSection{Items: []uiItem{textItem("没有启用文件处理：配置里缺少 files 一节。")}})
		return p
	}
	p.Toolbar = []uiItem{{Kind: "upload", Title: "上传", Icon: "arrow.up.doc"}}

	entry := func(e files.Entry, received bool) uiItem {
		name := e.Path
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if e.Dir {
			it := row(name, "文件夹")
			it.Icon = "folder"
			it.Page = "/files?" + url.Values{"dir": {e.Path}}.Encode()
			return it
		}
		it := row(name, joinNonEmpty(" · ", humanSize(e.Size), ago(e.Modified)))
		it.Icon = "doc"
		it.Kind = "download"
		it.Raw = "/files/download?" + url.Values{"path": {e.Path}}.Encode()
		it.Menu = []uiAction{{Label: "删除", Path: "/files/delete", Form: form("dir", d.Dir, "path", e.Path), Confirm: "删除「" + name + "」？删除后不可恢复。", Destructive: true}}
		return it
	}

	gen := uiSection{Title: yesNoText(d.Dir == "", "助手生成的文件", "助手生成的文件 / "+d.Dir)}
	if d.Dir != "" {
		up := row("上一级", "")
		up.Icon = "arrow.up.left"
		up.Page = "/files?" + url.Values{"dir": {d.Parent}}.Encode()
		gen.Items = append(gen.Items, up)
	}
	for _, e := range d.Entries {
		gen.Items = append(gen.Items, entry(e, false))
	}
	if len(d.Entries) == 0 {
		gen.Items = append(gen.Items, textItem("这里还没有文件。"))
	}
	p.Sections = append(p.Sections, gen)

	recv := uiSection{Title: "收到的文件", Footer: "你在聊天里发给助手的，和从这里上传的。"}
	for _, e := range d.Received {
		recv.Items = append(recv.Items, entry(e, true))
	}
	if len(d.Received) == 0 {
		recv.Items = append(recv.Items, textItem("还没有收到文件。"))
	}
	p.Sections = append(p.Sections, recv)

	// 下载链接
	sh := d.Shares
	links := uiSection{Title: "下载链接", Footer: "助手生成的文件的临时下载链接，最长 24 小时。"}
	if !sh.On {
		links.Items = append(links.Items, uiItem{Kind: "button", Title: "开启下载入口", Icon: "link.badge.plus",
			Action: &uiAction{Label: "开启", Path: "/files/share/listen", Form: form("on", "1")}})
	} else {
		base := yesNoText(sh.Base != "", sh.Base, sh.Suggest)
		b := row("外部访问地址", yesNoText(sh.Base != "", base, "尚未保存（建议 "+sh.Suggest+"）"))
		b.Form = &uiForm{Title: "外部访问地址", Intro: "下载链接以此地址开头。反向代理须把这个路径转发到下载入口。",
			Fields: []uiField{{Key: "base", Label: "地址", Type: "url", Value: base, Placeholder: "https://atom.example.com/download"}},
			Submit: uiAction{Label: "保存", Path: "/files/share/base"}}
		links.Items = append(links.Items, b)
		for _, l := range sh.Links {
			it := row(l.Path, "有效至 "+ago(l.ExpiresAt))
			it.Menu = []uiAction{{Label: "撤销", Path: "/files/share/revoke", Form: form("id", l.ID), Confirm: "撤销这个下载链接？", Destructive: true}}
			links.Items = append(links.Items, it)
		}
		if len(sh.Links) == 0 {
			links.Items = append(links.Items, textItem("现在没有有效的下载链接。"))
		}
		if !sh.Env {
			links.Items = append(links.Items, uiItem{Kind: "button", Title: "关闭下载入口", Icon: "link.badge.minus",
				Action: &uiAction{Label: "关闭", Path: "/files/share/listen", Form: form("on", "0"), Confirm: "关闭后助手不能再创建下载链接，已有的立即失效。", Destructive: true}})
		}
	}
	p.Sections = append(p.Sections, links)
	return p
}

// ---- 手机 ----

func uiDevices(d devicesData) uiPage {
	p := uiPage{Title: "手机设备"}
	// 接入新手机只在网页后台做：手机上不提供生成配对码。

	sec := uiSection{Title: "设备"}
	if len(d.Devices) == 0 {
		sec.Items = append(sec.Items, textItem("还没有设备。"))
	}
	for _, dv := range d.Devices {
		sub := joinNonEmpty(" · ", dv.Model, dv.OS)
		it := row(dv.Name, sub)
		switch dv.Status {
		case "online":
			it.Value, it.Tone = "最近在线 "+ago(dv.LastSeenAt), "ok"
			if dv.Admin {
				it.Badge, it.Tone = "可管理后台", "info"
			}
		case "waiting":
			it.Value, it.Tone = "等待配对", "warn"
		case "expired":
			it.Value = "配对码已过期"
		default:
			it.Value = "已吊销"
		}
		if dv.Status == "online" {
			choices := make([]uiOption, 0, len(d.Choices))
			for _, c := range d.Choices {
				choices = append(choices, uiOption{Value: strconv.Itoa(c), Label: fmt.Sprintf("每 %d 分钟", c)})
			}
			it.Form = &uiForm{
				Title: dv.Name, Intro: "这台手机的上报间隔，它下次同步时生效。",
				Fields: []uiField{{Key: "minutes", Label: "上报间隔", Type: "select", Options: choices, Value: strconv.Itoa(dv.SyncMinutes)}},
				Submit: uiAction{Label: "保存", Path: "/devices/interval", Form: form("id", dv.ID)},
			}
			it.Menu = append(it.Menu,
				uiAction{Label: yesNoText(dv.Admin, "关闭管理权限", "允许管理后台"), Path: "/devices/admin", Form: form("id", dv.ID, "on", yesNoText(dv.Admin, "0", "1")), Sensitive: true,
					Confirm: yesNoText(dv.Admin, "", "允许这台设备管理后台？它能看任务、批准动作、改配置。")},
				uiAction{Label: "清空数据", Path: "/devices/clear", Form: form("id", dv.ID), Confirm: "清空这台设备上报的全部数据？", Destructive: true},
				uiAction{Label: "吊销", Path: "/devices/revoke", Form: form("id", dv.ID), Confirm: "吊销后它的令牌立即失效。", Destructive: true, Sensitive: true},
			)
		}
		it.Menu = append(it.Menu, uiAction{Label: "删除", Path: "/devices/delete", Form: form("id", dv.ID), Confirm: "删除设备和它上报的全部数据？", Destructive: true, Sensitive: true})
		sec.Items = append(sec.Items, it)
	}
	p.Sections = append(p.Sections, sec)

	var chOpts []uiOption
	for _, c := range d.Channels {
		chOpts = append(chOpts, uiOption{Value: c, Label: c})
	}
	reply := row("手机提问的答复发到", d.ReplyChannel)
	reply.Form = &uiForm{
		Title: "手机提问的答复", Intro: "对 Siri 说「问原子助手」后，助手想好的答复发到这个消息源。",
		Fields: []uiField{{Key: "channel", Label: "消息源", Type: "select", Options: chOpts, Value: d.ReplyChannel}},
		Submit: uiAction{Label: "保存", Path: "/devices/reply-channel"},
	}
	p.Sections = append(p.Sections, uiSection{Title: "设置", Items: []uiItem{reply}})
	return p
}

// weatherHost 在天气配置为空时也能安全取 Host。
func weatherHost(w *config.WeatherConfig) string {
	if w == nil {
		return ""
	}
	return w.Host
}

// ---- 会话记录 ----

func uiSession(d sessionData) uiPage {
	p := uiPage{Title: "会话 " + d.Day}
	if d.Expired {
		p.Sections = append(p.Sections, uiSection{Items: []uiItem{textItem("超过 " + strconv.Itoa(summaryKeepDays) + " 天，摘要和原话不再提供查看。")}})
	}
	if d.Summary != nil {
		p.Sections = append(p.Sections, uiSection{Title: "当天摘要", Items: []uiItem{textItem(d.Summary.Text)}})
	}
	if len(d.Children) > 0 {
		sec := uiSection{Title: "当天交代的事", Footer: "详情在「任务」页里看。"}
		for _, c := range d.Children {
			it := row(c.Title, ago(c.CreatedAt))
			it.Badge = string(c.State)
			sec.Items = append(sec.Items, it)
		}
		p.Sections = append(p.Sections, sec)
	}
	if len(d.Messages) > 0 {
		sec := uiSection{Title: "原话"}
		for _, m := range d.Messages {
			who := "助手"
			if m.Role == "user" {
				who = "你"
			}
			sec.Items = append(sec.Items, row(clip(m.Text, 600), joinNonEmpty(" · ", who, m.At.Local().Format("15:04"), m.Channel)))
		}
		p.Sections = append(p.Sections, sec)
	}
	if len(p.Sections) == 0 {
		p.Sections = append(p.Sections, uiSection{Items: []uiItem{textItem("这一天没有记录。")}})
	}
	return p
}
