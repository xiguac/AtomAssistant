package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/agent"
	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/device"
	"github.com/xiguac/atomassistant/internal/files"
	"github.com/xiguac/atomassistant/internal/market"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/oplog"
	chat "github.com/xiguac/atomassistant/internal/session"
	"github.com/xiguac/atomassistant/internal/share"
	"github.com/xiguac/atomassistant/internal/skill"
	"github.com/xiguac/atomassistant/internal/team"
	"github.com/xiguac/atomassistant/internal/vault"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	hash, err := HashPassword("test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{PasswordHash: hash, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	s.Handler() // 建出手机用的内部路由表
	return s
}

// 一个把每个分支都走到的样例：各个页面都有数据、有待处理的项。
func samplePages() map[string]any {
	now := time.Now()
	cand := memory.Candidate{Kind: memory.KindFact, Subject: "user", Domain: "生活", Value: "住在上海"}
	prov := config.ProviderConfig{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1"}
	return map[string]any{
		"vault": vaultData{
			Requests: []vault.Request{{Label: "token", Note: "某服务的令牌", Tools: []string{"mcp.x"}}},
			Items:    []vault.Meta{{Label: "idcard", Note: "身份证", AllowedTools: []string{"tool.a"}, CreatedAt: now}, {Label: "empty"}},
			Tools:    []toolOption{{Value: "tool.a", Label: "工具 a"}},
		},
		"memory": memoryData{
			Pending: []memory.Pending{{ID: "p1", Candidate: cand, Arb: memory.Arbitration{Challenge: "和已有的不一致"}}},
			Memories: []*memory.Memory{
				{ID: "m1", Kind: memory.KindFact, Subject: "user", Current: memory.Revision{Value: "生日 5 月 3 日", Source: memory.Source{Kind: memory.SourceUser}}},
				{ID: "m2", Kind: memory.KindFact, Subject: "user", Status: memory.StatusDisputed, Current: memory.Revision{Value: "喜欢川菜", Source: memory.Source{Kind: memory.SourceTask}}},
			},
			Proposal: &memoryProposal{Ask: "删掉前公司", Note: "找到 1 条", Changes: []memoryChange{{Op: "delete", ID: "m1", Old: "前公司", Reason: "和前公司有关"}}},
			Profile:  "一个住在上海的人。", ProfileAt: now,
		},
		"models": modelsData{
			Providers:    []providerView{{ProviderConfig: prov, KeyState: "已保存", KeyOK: true}},
			MainProvider: "deepseek", MainModel: "deepseek-chat", MainThinking: "high", MainInputs: []string{"text", "image"},
			Agents:       []config.AgentConfig{{Name: "写作", Provider: "deepseek", Model: "m", GoodAt: []string{"writing"}}},
			Categories:   []team.CategoryInfo{{ID: "writing", Name: "写作"}},
			Standings:    []standingRow{{Category: team.CategoryInfo{ID: "writing", Name: "写作"}, Rows: []team.Standing{{Worker: team.Worker{Name: "写作"}, Tally: team.Tally{Runs: 3, Good: 2}, Next: true}}}},
			BudgetTokens: 100000, BudgetYuan: 5, BudgetHours: 2,
		},
		"channels": channelsData{
			Types: config.ChannelTypes,
			Channels: []channelView{
				{ChannelConfig: config.ChannelConfig{Name: "qq", Type: "qq", AppID: "102", Owners: []string{"OWNER"}}, SecretOK: true},
				{ChannelConfig: config.ChannelConfig{Name: "wx", Type: "weixin"}},
			},
		},
		"weixin": weixinData{Name: "wx", Link: "https://example.com/login?x=1"},
		"connectors": connectorsData{
			Calendars: []calendarRow{{CalendarConfig: config.CalendarConfig{Name: "iCloud", URL: "https://caldav.icloud.com", Username: "a@b.c"}, Key: true}},
			Mailboxes: []mailRow{{MailConfig: config.MailConfig{Label: "工作", IMAP: "imap.x:993", Username: "a@b.c"}}},
			Holidays:  []config.HolidaysConfig{{Name: "节假日", URL: "https://calendars.icloud.com/holidays/cn_zh.ics"}},
			Weather:   &config.WeatherConfig{Host: "abc.qweatherapi.com"}, WeatherKey: true,
			Search: &config.SearchConfig{},
		},
		"mcp": mcpData{
			Servers: []mcpServerView{{
				MCPConfig: config.MCPConfig{Name: "amap", Transport: "http", URL: "https://x"}, KeyOK: false,
				Keys:  []mcpKeyView{{Param: "key", OK: false}},
				Tools: []mcpToolView{{Name: "amap.search", Short: "search", SideEffect: "read_only", MaxPrivacy: "normal", Description: "搜索"}},
			}},
			Installed:   []*market.Server{{Name: "tool1", Description: "装的", URL: "https://y"}},
			Presets:     []mcpPresetView{{mcpPreset: mcpPresets[0], On: true}},
			SideEffects: sideEffects, PrivacyLevels: privacyLevels,
		},
		"skills":  skillsData{Skills: []*skill.Skill{{ID: "s1", Name: "出行", Version: "1", Goal: "规划路线"}}},
		"prompts": promptsData{Specs: []promptView{{PromptSpec: agent.PromptSpec{Key: "k", Title: "标题", Usage: "用途"}, Text: "内容", Custom: true, On: true}}, Extras: []agent.ExtraPrompt{{ID: "e1", Title: "我的", Text: "内容"}}},
		"security": securityData{PasswordTOTP: true, TOTPBound: false, PendingSecret: "ABCDEF",
			Passkeys: []passkeyRow{{ID: "k1", Name: "办公室", RPID: "x.com", CreatedAt: now}}},
		"logs": logsData{Entries: []oplog.Entry{{ID: "1", At: now, Message: "做了件事", Category: oplog.Admin, Level: slog.LevelWarn, Attrs: "a=b"}},
			Categories: oplog.Categories, Next: "cursor"},
		"files": filesData{Enabled: true, Dir: "报告", Parent: "",
			Entries:  []files.Entry{{Path: "报告/a.docx", Size: 2048, Modified: now}, {Path: "报告/子目录", Dir: true}},
			Received: []files.Entry{{Path: "收到的文件/b.pdf", Size: 10}},
			Shares:   sharesData{On: true, Base: "https://x/download", Links: []*share.Link{{ID: "l1", Path: "报告/a.docx", ExpiresAt: now.Add(time.Hour)}}}},
		"session": sessionData{Day: "2026-10-31", Summary: &chat.Summary{Text: "聊了日程"},
			Children: []*core.Task{{ID: "t1", Title: "查日程", State: core.StateDone, CreatedAt: now}},
			Messages: []chat.Message{{Role: "user", Text: "你好", At: now}, {Role: "assistant", Text: "在的", At: now}}},
		"devices": devicesData{Base: "https://atom.example.com", Choices: device.SyncChoices, Channels: []string{"qq"}, ReplyChannel: "qq",
			New: &device.Device{ID: "dev_n", Name: "iPad"}, Code: "K7M2-9XQP", Link: "atomassistant://pair?x=1", Expires: now,
			Devices: []deviceRow{{Device: &device.Device{ID: "dev_1", Name: "iPhone", Admin: true, SyncMinutes: 20, PairedAt: now}, Status: "online"}}},
	}
}

func walk(p uiPage, fn func(it uiItem)) {
	for _, it := range p.Toolbar {
		fn(it)
	}
	for _, sec := range p.Sections {
		for _, it := range sec.Items {
			fn(it)
		}
	}
}

func (s *Server) routeExists(method, path string) bool {
	u, err := url.Parse(path)
	if err != nil {
		return false
	}
	_, pattern := s.appMux.Handler(&http.Request{Method: method, URL: u, Host: "x"})
	return pattern != ""
}

func TestUIPage_每个页面描述里的操作和链接都对得上后台的路由(t *testing.T) {
	s := testServer(t)
	for name, data := range samplePages() {
		p, ok := s.uiPage(name, &view{Data: data}, &url.URL{Path: "/"})
		if !ok {
			t.Errorf("页面 %s 没有手机版", name)
			continue
		}
		if p.Title == "" || len(p.Sections) == 0 {
			t.Errorf("页面 %s 是空的: %+v", name, p)
		}
		if _, err := json.Marshal(p); err != nil {
			t.Errorf("页面 %s 序列化不了: %v", name, err)
		}
		checkAction := func(a uiAction, where string) {
			if a.Path == "" || a.Label == "" {
				t.Errorf("%s / %s：操作缺路径或名字: %+v", name, where, a)
				return
			}
			method := http.MethodPost
			if a.Get {
				method = http.MethodGet
			}
			if !s.routeExists(method, a.Path) {
				t.Errorf("%s / %s：%s %s 不是后台的路由", name, where, method, a.Path)
			}
		}
		walk(p, func(it uiItem) {
			if it.Kind == "" {
				t.Errorf("%s：有一项没有 kind: %+v", name, it)
			}
			if it.Action != nil {
				checkAction(*it.Action, it.Title)
			}
			for _, a := range it.Menu {
				checkAction(a, it.Title+" 的菜单")
			}
			if f := it.Form; f != nil {
				checkAction(f.Submit, "表单 "+f.Title)
				if len(f.Fields) == 0 && !strings.Contains(f.Title, "") {
					t.Errorf("%s：表单 %s 没有字段", name, f.Title)
				}
				for _, fl := range f.Fields {
					if fl.Key == "" || fl.Type == "" {
						t.Errorf("%s：表单 %s 的字段缺 key 或类型: %+v", name, f.Title, fl)
					}
				}
			}
			if it.Page != "" && !s.routeExists(http.MethodGet, it.Page) {
				t.Errorf("%s：页面链接 %s 不是后台的路由", name, it.Page)
			}
			if it.Raw != "" && !s.routeExists(http.MethodGet, it.Raw) {
				t.Errorf("%s：下载地址 %s 不是后台的路由", name, it.Raw)
			}
		})
	}
}

func TestUIPage_敏感的操作都标了要再验证一次(t *testing.T) {
	s := testServer(t)
	sensitivePaths := []string{"/vault", "/vault/replace", "/vault/delete", "/models/provider", "/channels", "/mcp/key", "/password", "/security/methods", "/prompts"}
	seen := map[string]bool{}
	for _, data := range samplePages() {
		name := ""
		p, _ := s.uiPage(name, &view{Data: data}, &url.URL{Path: "/"})
		collect := func(a uiAction) {
			if a.Sensitive {
				seen[a.Path] = true
			}
		}
		walk(p, func(it uiItem) {
			if it.Action != nil {
				collect(*it.Action)
			}
			for _, a := range it.Menu {
				collect(a)
			}
			if it.Form != nil {
				collect(it.Form.Submit)
			}
		})
	}
	for _, path := range sensitivePaths {
		if !seen[path] {
			t.Errorf("%s 涉及密钥、密码或登录方式，应当标成敏感操作", path)
		}
	}
}

func TestUIPage_记忆建议能凑出确认用的表单(t *testing.T) {
	s := testServer(t)
	p, _ := s.uiPage("memory", &view{Data: samplePages()["memory"]}, &url.URL{Path: "/"})
	var apply *uiAction
	walk(p, func(it uiItem) {
		if it.Action != nil && it.Action.Path == "/memory/apply" {
			apply = it.Action
		}
	})
	if apply == nil {
		t.Fatal("有修改建议时应当有「确认全部修改」")
	}
	if got := apply.Form["pick"]; len(got) != 1 || got[0] != "0" || apply.Form["op_0"][0] != "delete" || apply.Form["id_0"][0] != "m1" {
		t.Errorf("确认用的表单和网页上提交的格式对不上: %+v", apply.Form)
	}
}

func TestInternalPath_只放行站内页面(t *testing.T) {
	for _, bad := range []string{"https://evil.com/x", "//evil.com/x", "vault", "/../etc/passwd", "/api/device/v1/me", "/api/app/v1/tasks"} {
		if _, ok := internalPath(bad); ok {
			t.Errorf("%q 不该放行", bad)
		}
	}
	for _, good := range []string{"/vault", "/files?dir=a%2Fb", "/logs?cat=admin&level=warn"} {
		if _, ok := internalPath(good); !ok {
			t.Errorf("%q 应当放行", good)
		}
	}
}

func TestAppGroups_合并入口的分段互相对得上且手机上不能添加设备(t *testing.T) {
	if len(appGroups) != 5 {
		t.Fatalf("管理首页应有 5 个入口，实际 %d", len(appGroups))
	}
	seen := map[string]bool{}
	for _, g := range appGroups {
		for _, tb := range g.Tabs {
			if seen[tb.Path] {
				t.Errorf("%s 出现在了不止一个入口里", tb.Path)
			}
			seen[tb.Path] = true
			got := tabsFor(tb.Path)
			if len(g.Tabs) < 2 && got != nil {
				t.Errorf("%s 单独成页，不该有分段", tb.Path)
			}
			if len(g.Tabs) >= 2 && len(got) != len(g.Tabs) {
				t.Errorf("%s 的分段应有 %d 段，实际 %d", tb.Path, len(g.Tabs), len(got))
			}
		}
	}
	for _, want := range []string{"/memory", "/files", "/_sessions", "/models", "/prompts", "/channels", "/connectors", "/mcp", "/skills", "/devices", "/vault", "/security", "/logs"} {
		if !seen[want] {
			t.Errorf("%s 没有归到任何入口，手机上就进不去了", want)
		}
	}

	p := uiDevices(devicesData{Base: "https://x", Choices: device.SyncChoices,
		New: &device.Device{ID: "n", Name: "iPad"}, Code: "K7M2-9XQP", Link: "atomassistant://pair?x=1",
		Devices: []deviceRow{{Device: &device.Device{ID: "d1", Name: "iPhone"}, Status: "online"}}})
	if len(p.Toolbar) != 0 {
		t.Errorf("手机上不该有添加设备的按钮: %+v", p.Toolbar)
	}
	walk(p, func(it uiItem) {
		if it.Kind == "qr" || it.Text == "K7M2-9XQP" {
			t.Errorf("手机上不该显示配对码或二维码: %+v", it)
		}
	})
}
