package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
	return path
}

const validConfig = `
providers:
  - name: deepseek
    base_url: https://api.deepseek.com/v1
    api_key_env: DEEPSEEK_API_KEY
    timeout: 90s
routes:
  - purpose: plan
    provider: deepseek
    model: deepseek-chat
    max_tokens: 4000
tools:
  - name: demo.weather
    max_privacy: normal
    trust: connector
    side_effect: read_only
    egress_domains: [api.example.com]
    allowed_fields: [city]
approval:
  rules:
    - tool: demo.book
      amount_below_yuan: 100
      domain: personal
    - tool: mail.send
      never: true
  caps:
    daily_auto_amount_yuan: 500
    daily_auto_count: 20
    per_task_amount_yuan: 1000
`

func TestLoadFile_正常配置(t *testing.T) {
	f, err := LoadFile(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(f.Providers) != 1 || f.Providers[0].Timeout.String() != "1m30s" {
		t.Fatalf("provider 解析不对: %+v", f.Providers)
	}

	reg, err := f.BuildRegistry()
	if err != nil {
		t.Fatalf("构造授权表失败: %v", err)
	}
	g, err := reg.Grant("demo.weather")
	if err != nil {
		t.Fatalf("查询授权失败: %v", err)
	}
	if g.TrustCeiling != core.TrustConnector || g.SideEffect != core.SideEffectReadOnly {
		t.Fatalf("授权解析不对: %+v", g)
	}

	p := f.BuildApprovalPolicy()
	if p.Caps.DailyAutoAmountCents != 50000 || p.Caps.PerTaskAmountCents != 100000 {
		t.Fatalf("元转分不对: %+v", p.Caps)
	}
	if len(p.Rules) != 2 || p.Rules[0].AutoIf.AmountBelowCents != 10000 {
		t.Fatalf("规则解析不对: %+v", p.Rules)
	}
	if !p.Rules[1].Never {
		t.Fatal("never 规则未解析")
	}
}

func TestLoadFile_路径为空表示未配置(t *testing.T) {
	f, err := LoadFile("")
	if err != nil || f != nil {
		t.Fatalf("空路径应返回 nil，实际 %+v, %v", f, err)
	}
}

func TestLoadFile_拼错的字段名必须报错(t *testing.T) {
	// 一个拼错的 side_effect 会让工具按最保守等级跑，用户却以为自己配好了。
	// 静默忽略未知字段是最糟的处理方式。
	body := `
tools:
  - name: demo.weather
    side_efect: read_only
`
	if _, err := LoadFile(writeConfig(t, body)); err == nil {
		t.Fatal("未知字段应报错")
	}
}

func TestLoadFile_拒绝把绝密数据授权给工具(t *testing.T) {
	body := `
tools:
  - name: evil
    max_privacy: secret
    side_effect: read_only
`
	_, err := LoadFile(writeConfig(t, body))
	if err == nil {
		t.Fatal("max_privacy: secret 应被拒绝")
	}
	if !strings.Contains(err.Error(), "绝密数据") {
		t.Fatalf("错误信息应说明原因，实际: %v", err)
	}
}

func TestLoadFile_拒绝把工具输出提升为可信(t *testing.T) {
	for _, trust := range []string{"user", "system"} {
		body := "tools:\n  - name: evil\n    trust: " + trust + "\n    side_effect: read_only\n"
		if _, err := LoadFile(writeConfig(t, body)); err == nil {
			t.Fatalf("trust: %s 应被拒绝——工具输出不能具备指令资格", trust)
		}
	}
}

func TestLoadFile_未写副作用时按最保守处理(t *testing.T) {
	body := `
tools:
  - name: demo.unknown
    max_privacy: normal
`
	f, err := LoadFile(writeConfig(t, body))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	reg, err := f.BuildRegistry()
	if err != nil {
		t.Fatalf("构造授权表失败: %v", err)
	}
	g, _ := reg.Grant("demo.unknown")
	if g.SideEffect != core.SideEffectIrreversible {
		t.Fatalf("缺省应为 irreversible，实际 %s", g.SideEffect)
	}
}

func TestLoadFile_非法取值被拒(t *testing.T) {
	cases := map[string]string{
		"非法隐私等级":      "tools:\n  - name: t\n    max_privacy: topsecret\n",
		"非法副作用":       "tools:\n  - name: t\n    side_effect: maybe\n",
		"非法信任等级":      "tools:\n  - name: t\n    trust: godmode\n",
		"非法域":         "approval:\n  rules:\n    - tool: t\n      domain: office\n",
		"provider 缺项": "providers:\n  - name: x\n    base_url: https://a\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadFile(writeConfig(t, body)); err == nil {
				t.Fatal("应被拒绝")
			}
		})
	}
}

func TestBuildRouter_密钥缺失时报错且不写进配置(t *testing.T) {
	f, err := LoadFile(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := f.BuildRouter(context.Background(), nil); err == nil {
		t.Fatal("密钥环境变量未设置时应报错")
	}

	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	r, err := f.BuildRouter(context.Background(), nil)
	if err != nil {
		t.Fatalf("构造路由失败: %v", err)
	}
	// 配置里写的一条，加上自动接上的联网搜索（DeepSeek 自带）。
	if len(r.Routes()) != 2 || !r.CanSearch() {
		t.Fatalf("路由数量不对: %d", len(r.Routes()))
	}
}

func TestMCPServers_传输方式校验(t *testing.T) {
	ok := "mcp_servers:\n  - name: cal\n    transport: command\n    command: mcp-cal\n"
	f, err := LoadFile(writeConfig(t, ok))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if servers, err := f.MCPServers(nil); err != nil || len(servers) != 1 {
		t.Fatalf("解析失败: %v, %+v", err, servers)
	}

	bad := "mcp_servers:\n  - name: cal\n    transport: carrier-pigeon\n"
	f2, err := LoadFile(writeConfig(t, bad))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, err := f2.MCPServers(nil); err == nil {
		t.Fatal("非法传输方式应被拒绝")
	}
}

func TestMCPServers_密钥只能是保险库句柄(t *testing.T) {
	for _, bad := range []string{
		"mcp_servers:\n  - name: amap\n    transport: http\n    url: https://mcp.amap.com/mcp\n    secret_name: key\n    secret: abc123\n",
		"mcp_servers:\n  - name: amap\n    transport: http\n    url: https://mcp.amap.com/mcp\n    secret: vault://amap\n",
	} {
		if _, err := LoadFile(writeConfig(t, bad)); err == nil {
			t.Fatalf("应被拒绝:\n%s", bad)
		}
	}

	f, err := LoadFile(writeConfig(t, "mcp_servers:\n  - name: amap\n    transport: http\n"+
		"    url: https://mcp.amap.com/mcp\n    secret_name: key\n    secret: vault://mcp.amap\n"))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, err := f.MCPServers(nil); err == nil {
		t.Fatal("密钥在保险库里而保险库不可用时应报错")
	}
	lender := &fakeLender{}
	servers, err := f.MCPServers(lender)
	if err != nil || len(servers) != 1 {
		t.Fatalf("解析失败: %v", err)
	}
	// 以 mcp.<server 名> 的身份向保险库借，网址里没有密钥。
	if len(lender.asked) != 1 || lender.asked[0] != "mcp.amap=vault://mcp.amap" {
		t.Fatalf("借用身份不对: %v", lender.asked)
	}
	s := servers[0]
	if len(s.Secrets) != 1 || s.Secrets[0].Name != "key" || s.Secrets[0].Take == nil || s.URL != "https://mcp.amap.com/mcp" {
		t.Fatalf("连接参数不对: %+v", s)
	}
}

func TestWriteToolset_只改MCP与工具两节(t *testing.T) {
	raw := []byte("# 我的配置\nreply:\n  style: concise\n\nmcp_servers: []\n")
	out, err := WriteToolset(raw, Toolset{
		MCP:   []MCPConfig{{Name: "amap", Transport: "http", URL: "https://mcp.amap.com/mcp", SecretName: "key", Secret: "vault://mcp.amap"}},
		Tools: []ToolConfig{{Name: "amap.maps_weather", MaxPrivacy: "normal", Trust: "tool_output", SideEffect: "read_only", EgressDomains: []string{"mcp.amap.com"}}},
	})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	text := string(out)
	if !strings.HasPrefix(text, "# 我的配置\nreply:\n  style: concise\n") || strings.Contains(text, "command:") {
		t.Fatalf("其余内容应原样保留，空字段不写出:\n%s", text)
	}
	f, err := LoadFile(writeConfig(t, text))
	if err != nil {
		t.Fatalf("写出来的配置应能加载: %v\n%s", err, text)
	}
	got, err := ReadToolset(out)
	if err != nil || len(got.MCP) != 1 || len(got.Tools) != 1 || got.Tools[0].SideEffect != "read_only" || len(f.Tools) != 1 {
		t.Fatalf("读回来不对: %+v %v", got, err)
	}
	// 全部删掉之后两节都不留。
	out, err = WriteToolset(out, Toolset{})
	if err != nil || strings.Contains(string(out), "mcp_servers") || strings.Contains(string(out), "tools") {
		t.Fatalf("清空后不应留下空的小节: %v\n%s", err, out)
	}
}

func TestTaskBudget_配置里写了的用配置的没写的用默认值(t *testing.T) {
	def := core.DefaultBudget()
	if got := (*File)(nil).TaskBudget(); got != def {
		t.Fatalf("没有配置文件时应是默认值: %+v", got)
	}
	f := &File{Budget: &BudgetConfig{MaxTokens: 2_000_000, MaxHours: 1.5}}
	got := f.TaskBudget()
	if got.MaxTokens != 2_000_000 || got.MaxWallClock != 90*time.Minute || got.MaxCostCents != def.MaxCostCents {
		t.Fatalf("预算不对: %+v", got)
	}
	for _, bad := range []*BudgetConfig{{MaxTokens: -1}, {MaxTokens: MaxBudgetTokens + 1}, {MaxCostYuan: MaxBudgetYuan + 1}, {MaxHours: -2}} {
		if err := bad.Check(); err == nil {
			t.Fatalf("%+v 应被拒绝", bad)
		}
	}
	// 只改 budget 一节，其余原样；全部清空就删掉这一节。
	raw := []byte("# 注释\ntools: []\n")
	out, err := WriteBudget(raw, &BudgetConfig{MaxTokens: 900000})
	if err != nil || !strings.HasPrefix(string(out), string(raw)) || !strings.Contains(string(out), "max_tokens: 900000") {
		t.Fatalf("写入预算不对: %v %s", err, out)
	}
	if b, err := ReadBudget(out); err != nil || b == nil || b.MaxTokens != 900000 {
		t.Fatalf("读回预算不对: %v %+v", err, b)
	}
	if back, err := WriteBudget(out, &BudgetConfig{}); err != nil || string(back) != string(raw) {
		t.Fatalf("清空后应回到原样: %v %q", err, back)
	}
}
