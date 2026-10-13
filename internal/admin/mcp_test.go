package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xiguac/atomhelper/internal/agent"
	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/skillscan"
)

// keyedMCP 起一个要在网址里带 key 的远程 MCP server，像高德那样：一个自称只读的工具，一个没有自称的。
func keyedMCP(t *testing.T, key string) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "maps", Version: "v1"}, nil)
	type city struct {
		City string `json:"city"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "weather", Description: "查天气", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest, city) (*mcp.CallToolResult, any, error) {
			return nil, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "share", Description: "分享位置"},
		func(context.Context, *mcp.CallToolRequest, city) (*mcp.CallToolResult, any, error) {
			return nil, nil, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != key {
			http.Error(w, "INVALID_USER_KEY", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestAdmin_MCP页接入server密钥进保险库并登记工具(t *testing.T) {
	e := setup(t)
	e.login()
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post(path, form)
		return loc
	}
	const key = "map-key-7788"
	ts := keyedMCP(t, key)
	before, _ := os.ReadFile(e.cfg)

	if loc := post("/mcp/server", url.Values{"name": {"maps"}, "transport": {"http"}, "url": {"http://mcp.example.com/mcp"}}); !strings.Contains(loc, "https://") {
		t.Fatalf("明文地址应被拒绝: %s", loc)
	}
	if loc := post("/mcp/server", url.Values{"name": {"maps"}, "transport": {"http"}, "url": {ts.URL + "?key=" + key}}); !strings.Contains(loc, "保险库") {
		t.Fatalf("密钥写在地址里应被拒绝: %s", loc)
	}
	if loc := post("/mcp/server", url.Values{"name": {"maps"}, "transport": {"http"}, "url": {ts.URL}, "secret_name": {"key"}}); !strings.Contains(loc, "还需填写密钥") {
		t.Fatalf("有参数名没有密钥应被拒绝: %s", loc)
	}
	// 连接测试用的是表单里填的，不保存任何东西。
	if loc := post("/mcp/test", url.Values{"name": {"maps"}, "transport": {"http"}, "url": {ts.URL}, "secret_name": {"key"}, "key": {key}}); !strings.Contains(loc, "共 2 个工具") {
		t.Fatalf("连接测试应成功: %s", loc)
	}
	if now, _ := os.ReadFile(e.cfg); string(now) != string(before) {
		t.Fatal("连接测试不应改动配置")
	}

	loc := post("/mcp/server", url.Values{"name": {"maps"}, "transport": {"http"}, "url": {ts.URL}, "secret_name": {"key"}, "key": {key}})
	if !strings.Contains(loc, "共 2 个工具，新登记 2 个") {
		t.Fatalf("保存应成功并登记工具: %s", loc)
	}
	raw, _ := os.ReadFile(e.cfg)
	text := string(raw)
	if strings.Contains(text, key) || !strings.HasPrefix(text, string(before)) || !strings.Contains(text, "secret: vault://mcp.maps") {
		t.Fatalf("密钥不能出现在配置里，其余内容不动:\n%s", text)
	}
	file, err := config.LoadFile(e.cfg)
	if err != nil {
		t.Fatalf("保存出来的配置应能加载: %v\n%s", err, text)
	}
	// 密钥在保险库里，只授权给这个 server。
	servers, err := file.MCPServers(e.vault)
	if err != nil || len(servers) != 1 {
		t.Fatalf("解析失败: %v", err)
	}
	if got, err := servers[0].Secret(context.Background()); err != nil || got != key {
		t.Fatalf("密钥应能从保险库取出: %v", err)
	}
	// 自称只读的按只读登记，其余按不可撤销。
	sides := map[string]string{}
	for _, tool := range file.Tools {
		sides[tool.Name] = tool.SideEffect
	}
	if sides["maps.weather"] != "read_only" || sides["maps.share"] != "irreversible" {
		t.Fatalf("工具的副作用等级不对: %v", sides)
	}
	if _, err := file.BuildRegistry(); err != nil {
		t.Fatalf("登记的授权应能生效: %v", err)
	}

	_, body := e.get("/mcp")
	if strings.Contains(body, key) || !strings.Contains(body, "密钥已配置") || !strings.Contains(body, `value="maps.share"`) {
		t.Fatal("MCP 页应列出 server 与工具，不显示密钥")
	}

	// 逐个调整工具的副作用等级；别的 server 的工具改不了。
	if loc := post("/mcp/tools", url.Values{"name": {"maps"}, "tool": {"maps.share"}, "side": {"read_only"}, "privacy": {"sensitive"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存授权失败: %s", loc)
	}
	if loc := post("/mcp/tools", url.Values{"name": {"maps"}, "tool": {"other.x"}, "side": {"read_only"}, "privacy": {"normal"}}); !strings.Contains(loc, "e=") {
		t.Fatalf("别的 server 的工具不应能改: %s", loc)
	}
	if loc := post("/mcp/tools", url.Values{"name": {"maps"}, "tool": {"maps.share"}, "side": {"none"}, "privacy": {"normal"}}); !strings.Contains(loc, "e=") {
		t.Fatalf("页面上没有的等级不应接受: %s", loc)
	}
	// 修改时密钥留空：沿用原密钥；重新读取工具不覆盖已有的授权。
	if loc := post("/mcp/server", url.Values{"name": {"maps"}, "transport": {"http"}, "url": {ts.URL}, "secret_name": {"key"}}); !strings.Contains(loc, "新登记 0 个") {
		t.Fatalf("修改失败: %s", loc)
	}
	if loc := post("/mcp/tools", url.Values{"name": {"maps"}, "tool": {"maps.share"}, "side": {"read_only"}, "privacy": {"secret"}}); !strings.Contains(loc, "e=") {
		t.Fatalf("绝密不是可选的隐私上限: %s", loc)
	}
	if file, err = config.LoadFile(e.cfg); err != nil || len(file.Tools) != 2 ||
		file.Tools[0].Name != "maps.share" || file.Tools[0].SideEffect != "read_only" || file.Tools[0].MaxPrivacy != "sensitive" || file.Tools[1].MaxPrivacy != "normal" {
		t.Fatalf("已有的授权应保留: %v %+v", err, file)
	}

	// 保险库是总览：服务凭据和个人信息分开列，服务凭据标明归属。
	if err := e.vault.Seal(context.Background(), "身份证", "110101199001011234", "本人身份证号", []string{"maps.share"}); err != nil {
		t.Fatal(err)
	}
	_, body = e.get("/vault")
	personal, services, ok := strings.Cut(body, "<b>服务凭据</b>")
	if !ok || !strings.Contains(personal, "vault://身份证") || strings.Contains(personal, "vault://mcp.maps") ||
		!strings.Contains(services, "vault://mcp.maps") || !strings.Contains(services, `href="/mcp">MCP server maps</a>`) {
		t.Fatal("保险库页应把个人信息和服务凭据分开列，服务凭据链接到它的管理页面")
	}
	// 在 server 自己的卡片上换密钥：换的是保险库里的同一条。
	if loc := post("/mcp/key", url.Values{"name": {"maps"}, "key": {"wrong-key"}}); !strings.Contains(loc, "读取工具列表失败") {
		t.Fatalf("换成错的密钥后应连不上: %s", loc)
	}
	if loc := post("/mcp/key", url.Values{"name": {"maps"}, "key": {key}}); !strings.Contains(loc, "共 2 个工具") {
		t.Fatalf("换回密钥失败: %s", loc)
	}
	if loc := post("/mcp/server", url.Values{"name": {"mcp"}, "transport": {"http"}, "url": {ts.URL}}); !strings.Contains(loc, "e=") {
		t.Fatalf("会和服务名字混淆的名称应被拒绝: %s", loc)
	}

	if loc := post("/mcp/server/delete", url.Values{"name": {"maps"}}); !strings.Contains(loc, "已移除") {
		t.Fatalf("移除失败: %s", loc)
	}
	if now, _ := os.ReadFile(e.cfg); string(now) != string(before) {
		t.Fatalf("移除后 server 与它的工具授权都不应留下:\n%s", now)
	}
	// server 没了，它独占的密钥也不留；个人信息不受影响。
	metas, err := e.vault.List(context.Background())
	if err != nil || len(metas) != 1 || metas[0].Label != "身份证" {
		t.Fatalf("移除 server 后它的密钥应一并删除: %+v %v", metas, err)
	}
}

// fakeScanner 按内容里有没有"恶意"二字放行或拒绝。
type fakeScanner struct{}

func (fakeScanner) Check(_ context.Context, dir string) (skillscan.Report, error) {
	raw, err := os.ReadFile(dir + "/skill.yaml")
	if err != nil {
		return skillscan.Report{}, err
	}
	if strings.Contains(string(raw), "恶意") {
		return skillscan.Report{Output: "风险分 90"}, errors.New("扫描未通过")
	}
	return skillscan.Report{Output: "风险分 3"}, nil
}

func TestAdmin_Skill页安装要过扫描(t *testing.T) {
	e := setup(t)
	e.login()
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post(path, form)
		return loc
	}
	manifest := "id: trip.plan\nname: 出行规划\ngoal: 给出可执行的出行方案\nrequired_tools:\n  - amap.maps_weather\nsteps:\n  - 查天气\n  - 规划路线\n"

	// 没有扫描器时装不了：没扫过的不装。
	if loc := post("/skills", url.Values{"manifest": {manifest}}); !strings.Contains(loc, "e=") {
		t.Fatalf("没有扫描器应拒绝安装: %s", loc)
	}
	e.server.opts.Scanner = fakeScanner{}
	if loc := post("/skills", url.Values{"manifest": {strings.Replace(manifest, "查天气", "恶意步骤", 1)}}); !strings.Contains(loc, "风险分 90") {
		t.Fatalf("扫描不通过应拒绝并给出报告: %s", loc)
	}
	if loc := post("/skills", url.Values{"manifest": {manifest + "run: rm -rf /\n"}}); !strings.Contains(loc, "e=") {
		t.Fatalf("清单里不认识的字段应被拒绝: %s", loc)
	}
	if loc := post("/skills", url.Values{"manifest": {manifest}}); !strings.Contains(loc, "已通过扫描并安装") {
		t.Fatalf("安装失败: %s", loc)
	}
	ctx := context.Background()
	sk, err := e.store.Skills().GetSkill(ctx, "trip.plan")
	if err != nil || sk.ScanReport != "风险分 3" || len(sk.Steps) != 2 {
		t.Fatalf("装上的 Skill 不对: %+v %v", sk, err)
	}
	if _, body := e.get("/skills"); !strings.Contains(body, "出行规划") || !strings.Contains(body, "amap.maps_weather") {
		t.Fatal("Skill 页应列出已安装的 Skill 与它声明的工具")
	}
	if loc := post("/skills/delete", url.Values{"id": {"trip.plan"}}); !strings.Contains(loc, "已移除") {
		t.Fatalf("移除失败: %s", loc)
	}
	if _, err := e.store.Skills().GetSkill(ctx, "trip.plan"); err == nil {
		t.Fatal("移除后不应还在")
	}
}

func TestAdmin_系统提示词页改写与恢复默认(t *testing.T) {
	e := setup(t)
	e.login()
	post := func(form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post("/prompts", form)
		return loc
	}
	ctx := context.Background()
	source := agent.SettingPrompts{Store: e.store}

	_, body := e.get("/prompts")
	if !strings.Contains(body, "# 基础约束") || !strings.Contains(body, "任务规划器") || strings.Contains(body, "已修改") {
		t.Fatal("没有改写过时应显示内置内容")
	}
	if loc := post(url.Values{"key": {"prompt.other"}, "text": {"x"}}); !strings.Contains(loc, "e=") {
		t.Fatalf("不认识的段落应被拒绝: %s", loc)
	}
	if loc := post(url.Values{"key": {agent.PromptKeyBase}, "text": {"  "}}); !strings.Contains(loc, "e=") {
		t.Fatalf("空内容应被拒绝: %s", loc)
	}
	if loc := post(url.Values{"key": {agent.PromptKeyBase}, "text": {"# 我的约束\r\n不编造"}}); !strings.Contains(loc, "立即生效") {
		t.Fatalf("保存失败: %s", loc)
	}
	// 助手读到的就是刚保存的；没改的那一段是空的，表示用内置的。
	if base, planner, err := source.Prompts(ctx); err != nil || base != "# 我的约束\n不编造" || planner != "" {
		t.Fatalf("助手读到的系统提示不对: %q %q %v", base, planner, err)
	}
	if _, body = e.get("/prompts"); !strings.Contains(body, "# 我的约束") || strings.Count(body, "已修改") != 1 {
		t.Fatal("页面应显示改写后的内容并标明已修改")
	}
	// 存回与内置一致的内容等于恢复默认。
	if loc := post(url.Values{"key": {agent.PromptKeyPlanner}, "text": {agent.DefaultPlannerPrompt()}}); !strings.Contains(loc, "已恢复") {
		t.Fatalf("与内置一致时应视为恢复默认: %s", loc)
	}
	if loc := post(url.Values{"key": {agent.PromptKeyBase}, "reset": {"1"}, "text": {"随便什么"}}); !strings.Contains(loc, "已恢复") {
		t.Fatalf("恢复默认失败: %s", loc)
	}
	if base, planner, err := source.Prompts(ctx); err != nil || base != "" || planner != "" {
		t.Fatalf("恢复默认后不应留下改写的内容: %q %q %v", base, planner, err)
	}
}
