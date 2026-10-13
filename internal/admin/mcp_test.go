package admin

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/team"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

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
	// 工具列表可以折叠，并带着 server 给每个工具写的说明。
	if !strings.Contains(body, `<details class="fold">`) || !strings.Contains(body, "查天气") || !strings.Contains(body, "分享位置") {
		t.Fatal("工具列表应可折叠并显示工具说明")
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
// presetMCP 起一个要在网址里带 key 的远程 MCP server，提供给定名字的工具。
// 每个工具都自称只读：预置的服务不采信自报，按写定的名单登记。
func presetMCP(t *testing.T, key string, tools ...string) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "preset", Version: "v1"}, nil)
	type args struct {
		Q string `json:"q"`
	}
	for _, name := range tools {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: name, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
			func(context.Context, *mcp.CallToolRequest, args) (*mcp.CallToolResult, any, error) {
				return nil, nil, nil
			})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != key {
			http.Error(w, "Missing KEY parameter", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestAdmin_MCP页预置的滴滴与12306按写定的名单登记(t *testing.T) {
	e := setup(t)
	e.login()
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post(path, form)
		return loc
	}
	// 预置项的地址是写死的公网地址，测试里换成本地的假 server。12306 本来是本机进程，这里同样换成远程的。
	saved := append([]mcpPreset{}, mcpPresets...)
	t.Cleanup(func() { copy(mcpPresets, saved) })
	const key = "didi-key-5566"
	didi := presetMCP(t, key, "taxi_estimate", "maps_direction_driving", "taxi_create_order", "taxi_cancel_order", "taxi_new_thing")
	rail := presetMCP(t, "", "get-tickets", "get-train-route-stations")
	for i := range mcpPresets {
		switch mcpPresets[i].ID {
		case "didi":
			mcpPresets[i].Server.URL = didi.URL
		case "12306":
			mcpPresets[i].Server = config.MCPConfig{Name: "12306", Transport: "http", URL: rail.URL}
		}
	}
	before, _ := os.ReadFile(e.cfg)

	_, body := e.get("/mcp")
	for _, want := range []string{"高德地图", "滴滴出行", "12306", `action="/mcp/didi"`, `action="/mcp/12306"`, `id="didi-key"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("MCP 页应列出预置的服务，缺少 %q", want)
		}
	}
	if strings.Contains(body, `id="12306-key"`) {
		t.Fatal("12306 不需要密钥，不应有密钥一栏")
	}

	if loc := post("/mcp/didi", url.Values{}); !strings.Contains(loc, "e=") {
		t.Fatalf("没填 MCP Key 应被拒绝: %s", loc)
	}
	if loc := post("/mcp/didi", url.Values{"key": {key}}); !strings.Contains(loc, "共 5 个工具，新登记 5 个") {
		t.Fatalf("接入滴滴失败: %s", loc)
	}
	if loc := post("/mcp/12306", url.Values{}); !strings.Contains(loc, "共 2 个工具，新登记 2 个") {
		t.Fatalf("接入 12306 失败: %s", loc)
	}

	raw, _ := os.ReadFile(e.cfg)
	if strings.Contains(string(raw), key) || !strings.HasPrefix(string(raw), string(before)) || !strings.Contains(string(raw), "secret: vault://mcp.didi") {
		t.Fatalf("密钥不能出现在配置里，其余内容不动:\n%s", raw)
	}
	file, err := config.LoadFile(e.cfg)
	if err != nil {
		t.Fatalf("保存出来的配置应能加载: %v\n%s", err, raw)
	}
	if len(file.MCP) != 2 || file.MCP[1].Name != "12306" || file.MCP[1].Secret != "" {
		t.Fatalf("server 不对: %+v", file.MCP)
	}
	got := map[string]config.ToolConfig{}
	for _, tool := range file.Tools {
		got[tool.Name] = tool
	}
	// 预估与路线规划可直接调用；下单、取消和名单之外的工具每次都要确认——哪怕它们自称只读。
	for name, side := range map[string]string{
		"didi.taxi_estimate": "read_only", "didi.maps_direction_driving": "read_only",
		"didi.taxi_create_order": "irreversible", "didi.taxi_cancel_order": "irreversible", "didi.taxi_new_thing": "irreversible",
		"12306.get-tickets": "read_only", "12306.get-train-route-stations": "read_only",
	} {
		if got[name].SideEffect != side {
			t.Fatalf("%s 的副作用等级应为 %s: %+v", name, side, got[name])
		}
	}
	if got["didi.taxi_estimate"].MaxPrivacy != "sensitive" || got["12306.get-tickets"].MaxPrivacy != "normal" {
		t.Fatalf("参数隐私上限不对: %+v", got)
	}

	// 审批：自主模式下预估直接执行，下单必须由用户确认。
	approver := agent.NewApprover(file.BuildApprovalPolicy(), policy.NewClassifier(), agent.NewMemoryUsageTracker())
	task := core.NewTask("t", "打车去机场", core.TrustUser)
	check := func(tool string, input string) kernel.ApprovalOutcome {
		t.Helper()
		out, _, err := approver.CheckAction(context.Background(), task, kernel.Decision{
			ToolName: tool, SideEffect: core.SideEffect(got[tool].SideEffect), Input: json.RawMessage(input)})
		if err != nil {
			t.Fatalf("审批 %s 出错: %v", tool, err)
		}
		return out
	}
	if out := check("didi.taxi_estimate", `{"from_name":"望京SOHO","to_name":"首都机场"}`); out != kernel.ApprovalAuto {
		t.Fatalf("价格预估应直接执行: %s", out)
	}
	if out := check("didi.taxi_create_order", `{"product_category":"1","estimate_trace_id":"x"}`); out != kernel.ApprovalManual {
		t.Fatalf("创建订单必须由用户确认: %s", out)
	}
	if out := check("didi.taxi_cancel_order", `{"order_id":"x"}`); out != kernel.ApprovalManual {
		t.Fatalf("取消订单必须由用户确认: %s", out)
	}

	_, body = e.get("/mcp")
	if strings.Contains(body, key) || strings.Count(body, "已接入") < 2 || !strings.Contains(body, `value="didi.taxi_create_order"`) {
		t.Fatal("MCP 页应显示预置服务已接入并列出工具，不显示密钥")
	}
	// 再次保存时密钥留空：沿用原密钥，已有的授权不动。
	if loc := post("/mcp/didi", url.Values{}); !strings.Contains(loc, "新登记 0 个") {
		t.Fatalf("留空密钥应沿用原密钥: %s", loc)
	}
	for _, name := range []string{"didi", "12306"} {
		if loc := post("/mcp/server/delete", url.Values{"name": {name}}); !strings.Contains(loc, "已移除") {
			t.Fatalf("移除 %s 失败: %s", name, loc)
		}
	}
	if now, _ := os.ReadFile(e.cfg); string(now) != string(before) {
		t.Fatalf("移除后配置应回到原样:\n%s", now)
	}
}

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

func TestAdmin_系统提示词页逐段改写停用与恢复默认(t *testing.T) {
	e := setup(t)
	e.login()
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post(path, form)
		return loc
	}
	ctx := context.Background()
	source := agent.SettingPrompts{Store: e.store}
	baseKey, offKey := agent.PromptSettingKey(agent.PromptBase), agent.PromptOffKey(agent.PromptWeb)

	// 列表里每一段都有名字和用途；没有改动过时显示内置内容，全部加载。
	_, body := e.get("/prompts")
	for _, want := range []string{"基础约束", "联网搜索", "动作把关", "# Core rules", "You are the task planner", `aria-checked="true"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("系统提示词页缺少 %q", want)
		}
	}
	if strings.Contains(body, "已修改") || strings.Contains(body, "已停用") || strings.Count(body, `class="prompt"`) != len(agent.PromptSpecs()) {
		t.Fatal("没有改动过时不应有已修改或已停用的段落，且每一段各占一行")
	}

	if loc := post("/prompts", url.Values{"key": {"other"}, "text": {"x"}}); !strings.Contains(loc, "e=") {
		t.Fatalf("不认识的段落应被拒绝: %s", loc)
	}
	if loc := post("/prompts", url.Values{"key": {agent.PromptBase}, "text": {"  "}}); !strings.Contains(loc, "e=") {
		t.Fatalf("空内容应被拒绝: %s", loc)
	}
	if loc := post("/prompts", url.Values{"key": {agent.PromptBase}, "text": {"# My rules\r\nNo fabrication"}}); !strings.Contains(loc, "立即生效") {
		t.Fatalf("保存失败: %s", loc)
	}
	// 助手读到的就是刚保存的。
	if saved, err := source.Prompts(ctx); err != nil || saved[baseKey] != "# My rules\nNo fabrication" || len(saved) != 1 {
		t.Fatalf("助手读到的系统提示不对: %v %v", saved, err)
	}
	if _, body = e.get("/prompts"); !strings.Contains(body, "# My rules") || strings.Count(body, "已修改") != 1 {
		t.Fatal("页面应显示改写后的内容并标明已修改")
	}

	// 开关：停用一段，再启用。规定输出格式的段落停不掉。
	if loc := post("/prompts/toggle", url.Values{"key": {agent.PromptWeb}}); !strings.Contains(loc, "已停用") {
		t.Fatalf("停用失败: %s", loc)
	}
	if saved, err := source.Prompts(ctx); err != nil || saved[offKey] == "" {
		t.Fatalf("停用应存下来: %v %v", saved, err)
	}
	if _, body = e.get("/prompts"); strings.Count(body, "已停用") != 1 || !strings.Contains(body, `aria-checked="false"`) {
		t.Fatal("页面应标明停用的段落")
	}
	if loc := post("/prompts/toggle", url.Values{"key": {agent.PromptPlanner}}); !strings.Contains(loc, "不可停用") {
		t.Fatalf("必需的段落不应能停用: %s", loc)
	}
	if loc := post("/prompts/toggle", url.Values{"key": {agent.PromptWeb}, "on": {"1"}}); !strings.Contains(loc, "已启用") {
		t.Fatalf("启用失败: %s", loc)
	}

	// 存回与内置一致的内容等于恢复默认。
	planner := ""
	for _, spec := range agent.PromptSpecs() {
		if spec.Key == agent.PromptPlanner {
			planner = spec.Default
		}
	}
	if loc := post("/prompts", url.Values{"key": {agent.PromptPlanner}, "text": {planner}}); !strings.Contains(loc, "已恢复") {
		t.Fatalf("与内置一致时应视为恢复默认: %s", loc)
	}
	if loc := post("/prompts", url.Values{"key": {agent.PromptBase}, "reset": {"1"}, "text": {"whatever"}}); !strings.Contains(loc, "已恢复") {
		t.Fatalf("恢复默认失败: %s", loc)
	}
	if saved, err := source.Prompts(ctx); err != nil || len(saved) != 0 {
		t.Fatalf("恢复默认、重新启用之后不应留下任何改动: %v %v", saved, err)
	}
}

func TestAdmin_批量删除已结束的任务而记忆不受影响(t *testing.T) {
	e := setup(t)
	e.login()
	ctx := context.Background()
	post := func(form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post("/tasks/delete", form)
		return loc
	}
	// 四个任务：两个办完了，一个失败了，一个还在排队。
	mk := func(title string, to core.TaskState) *core.Task {
		t.Helper()
		tk := core.NewTask(title, title, core.TrustUser)
		if err := e.store.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
		for _, hop := range [][2]core.TaskState{{core.StateNew, core.StateReady}, {core.StateReady, core.StateRunning}, {core.StateRunning, to}} {
			if hop[0] == hop[1] || (to == core.StateReady && hop[0] != core.StateNew) {
				continue
			}
			if err := e.store.Transition(ctx, tk.ID, hop[0], hop[1], "测试"); err != nil {
				t.Fatalf("推进 %s 失败: %v", title, err)
			}
		}
		return tk
	}
	done1, done2 := mk("订餐厅", core.StateDone), mk("查天气", core.StateDone)
	failed, queued := mk("订机票", core.StateFailed), mk("写周报", core.StateReady)

	// 办完的任务留下了步骤、图片，以及一条提炼出的记忆和一份评分记录。
	step := core.NewStep(done1.ID, 1, core.StepTool, json.RawMessage(`{"q":"餐厅"}`))
	step.ToolName, step.SideEffect = "web.search", core.SideEffectReadOnly
	step.Complete(json.RawMessage(`{"text":"..."}`), core.TrustExternal)
	if err := e.store.AppendStep(ctx, step); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.PutTaskImage(ctx, done1.ID, "image/png", []byte("PNG")); err != nil {
		t.Fatal(err)
	}
	pipe := memory.NewPipeline(e.store.Memories(), nil)
	out, err := pipe.Ingest(ctx, []memory.Candidate{{Kind: memory.KindFact, Subject: "user", Domain: "饮食", Value: "不太吃辣",
		Temporality: memory.TemporalDurable, Source: memory.Source{Kind: memory.SourceTask, Ref: done1.ID, Trust: core.TrustToolOutput}}})
	if err != nil || len(out) != 1 {
		t.Fatalf("写入记忆失败: %v", err)
	}
	if err := e.store.Team().PutWork(ctx, &team.Work{ID: team.WorkID(done1.ID, 1, 0), TaskID: done1.ID, StepSeq: 1,
		Worker: "分析师", Category: team.CatAnalysis, Title: "订餐厅", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	_, body := e.get("/")
	if !strings.Contains(body, `name="id" value="`+done1.ID+`"`) || strings.Contains(body, `name="id" value="`+queued.ID+`"`) {
		t.Fatal("只有已结束的任务可以勾选删除")
	}
	if loc := post(url.Values{}); !strings.Contains(loc, "请先勾选") {
		t.Fatalf("什么都没选应提示: %s", loc)
	}
	// 勾选删除：没结束的任务即使编号混在里面也删不掉。
	if loc := post(url.Values{"id": {done1.ID, queued.ID}}); !strings.Contains(loc, "已删除 1 个") {
		t.Fatalf("应只删掉结束了的那一个: %s", loc)
	}
	if _, err := e.store.GetTask(ctx, done1.ID); err == nil {
		t.Fatal("勾选的已结束任务应被删除")
	}
	if _, err := e.store.GetTask(ctx, queued.ID); err != nil {
		t.Fatalf("还在排队的任务不应被删: %v", err)
	}
	// 任务自己的记录跟着没了。
	if steps, _ := e.store.ListSteps(ctx, done1.ID); len(steps) != 0 {
		t.Fatal("任务的步骤应一并删除")
	}
	if imgs, _ := e.store.TaskImages(ctx, done1.ID, 4); len(imgs) != 0 {
		t.Fatal("任务的图片应一并删除")
	}
	// 办完时落下的东西还在：记忆、评分记录。
	if m, err := e.store.Memories().Get(ctx, out[0].MemoryID); err != nil || m.Current.Value != "不太吃辣" || m.Current.Source.Ref != done1.ID {
		t.Fatalf("由这个任务提炼的记忆应保留: %+v %v", m, err)
	}
	if tally, err := e.store.Team().Tally(ctx); err != nil || len(tally) != 1 || tally[0].Runs != 1 {
		t.Fatalf("任务模型的评分记录应保留: %+v %v", tally, err)
	}

	// 删除全部已结束的：完成的和失败的都删，排队的留着。
	if loc := post(url.Values{"all": {"1"}}); !strings.Contains(loc, "已删除 2 个") {
		t.Fatalf("应删掉剩下两个已结束的任务: %s", loc)
	}
	left, err := e.store.ListTasks(ctx, false, 10)
	if err != nil || len(left) != 1 || left[0].ID != queued.ID {
		t.Fatalf("只应剩下还在排队的任务: %+v %v", left, err)
	}
	_ = done2
	_ = failed
}

func TestAdmin_渠道页接入飞书钉钉与微信扫码连接(t *testing.T) {
	e := setup(t)
	e.login()
	ctx := context.Background()
	post := func(path string, form url.Values) string {
		t.Helper()
		form.Set("csrf", e.csrf)
		_, loc := e.post(path, form)
		return loc
	}

	// 飞书、钉钉和 QQ 一样填 AppID 与 AppSecret，凭据进保险库。
	if loc := post("/channels", url.Values{"name": {"feishu"}, "type": {"feishu"}, "app_id": {"cli_abc"}, "secret": {"feishu-secret"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存飞书渠道失败: %s", loc)
	}
	if loc := post("/channels", url.Values{"name": {"ding"}, "type": {"dingtalk"}, "app_id": {"dingabc"}, "secret": {"ding-secret"}}); !strings.Contains(loc, "已保存") {
		t.Fatalf("保存钉钉渠道失败: %s", loc)
	}
	// 微信没有 AppID 可填，走不了这张表单。
	if loc := post("/channels", url.Values{"name": {"wx"}, "type": {"weixin"}, "app_id": {"x"}, "secret": {"y"}}); !strings.Contains(loc, "扫码连接") {
		t.Fatalf("微信渠道应引导去扫码: %s", loc)
	}

	// 微信：申请二维码 → 显示 → 等扫码 → 确认之后凭证进保险库、渠道进配置、扫码的人成为主人。
	step := 0
	wx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			_, _ = w.Write([]byte(`{"qrcode":"qr-1","qrcode_img_content":"https://liteapp.weixin.qq.com/q/abc"}`))
		case "/ilink/bot/get_qrcode_status":
			step++
			if step == 1 {
				_, _ = w.Write([]byte(`{"status":"scaned"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"confirmed","bot_token":"wx-bot-token-1","ilink_bot_id":"bot1@im.bot","baseurl":"https://ilink.example","ilink_user_id":"me@im.wechat"}`))
		}
	}))
	defer wx.Close()
	e.server.wxBase = wx.URL

	// 名称被别的类型的渠道占着时不能拿来接微信。
	if loc := post("/channels/weixin/start", url.Values{"name": {"feishu"}}); !strings.Contains(loc, "已被其他类型的渠道使用") {
		t.Fatalf("不应覆盖别的渠道: %s", loc)
	}
	if loc := post("/channels/weixin/start", url.Values{"name": {"wx"}}); !strings.HasPrefix(loc, "/channels/weixin?name=wx") {
		t.Fatalf("应跳到显示二维码的页面: %s", loc)
	}
	_, body := e.get("/channels/weixin?name=wx")
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "liteapp.weixin.qq.com/q/abc") || !strings.Contains(body, "data-poll") {
		t.Fatal("页面应显示二维码和它的链接，并自动查询状态")
	}
	poll := func() map[string]any {
		t.Helper()
		_, raw := e.post("/channels/weixin/poll", url.Values{"csrf": {e.csrf}, "name": {"wx"}})
		var out map[string]any
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("应答不是 JSON: %s", raw)
		}
		return out
	}
	if out := poll(); out["done"] != false || !strings.Contains(out["message"].(string), "已扫码") {
		t.Fatalf("扫码后应提示在手机上确认: %v", out)
	}
	if out := poll(); out["done"] != true || out["ok"] != true || !strings.Contains(out["message"].(string), "微信已连接") {
		t.Fatalf("确认后应连接成功: %v", out)
	}
	// 登录用完就没有了：再问就是已失效。
	if out := poll(); out["ok"] != false || out["done"] != true {
		t.Fatalf("登录结束后不应还能查询: %v", out)
	}

	raw, _ := os.ReadFile(e.cfg)
	if strings.Contains(string(raw), "wx-bot-token-1") || strings.Contains(string(raw), "feishu-secret") || strings.Contains(string(raw), "ding-secret") {
		t.Fatalf("凭据不能出现在配置里:\n%s", raw)
	}
	file, err := config.LoadFile(e.cfg)
	if err != nil || len(file.Channels) != 3 {
		t.Fatalf("保存出来的配置应能加载: %v\n%s", err, raw)
	}
	wxc := file.Channels[2]
	if wxc.Type != "weixin" || wxc.AppID != "bot1@im.bot" || wxc.APIBase != "https://ilink.example" ||
		wxc.Secret != "vault://channel.wx" || len(wxc.Owners) != 1 || wxc.Owners[0] != "me@im.wechat" {
		t.Fatalf("微信渠道的配置不对: %+v", wxc)
	}
	// 三个渠道都装得起来，各自从保险库取自己的凭据。
	bindings, err := file.BuildChannels(e.vault, e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || len(bindings) != 3 {
		t.Fatalf("渠道应能构造: %d %v", len(bindings), err)
	}
	take, err := e.vault.Lend("vault://channel.wx", "channel.wx")
	if err != nil {
		t.Fatal(err)
	}
	if token, err := take(ctx); err != nil || token != "wx-bot-token-1" {
		t.Fatalf("微信的机器人凭证应在保险库里: %v", err)
	}
	if _, body = e.get("/channels"); !strings.Contains(body, "微信扫码连接") || !strings.Contains(body, "me@im.wechat") || !strings.Contains(body, "飞书") || !strings.Contains(body, "钉钉") {
		t.Fatal("渠道页应列出三个渠道，并提供微信扫码连接")
	}
}
