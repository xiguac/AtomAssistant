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

// fakeScanner 记录被要求扫描的路径，并按预设的结论回答。
type fakeScanner struct {
	err     error
	scanned []string
	// seen 是扫描那一刻目录里的文件。
	seen []string
}

func (f *fakeScanner) Check(_ context.Context, path string) (skillscan.Report, error) {
	f.scanned = append(f.scanned, path)
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			f.seen = append(f.seen, d.Name())
		}
		return nil
	})
	return skillscan.Report{Output: "报告"}, f.err
}

func (f *fakeScanner) CheckRegistry(_ context.Context, path string) (skillscan.Report, error) {
	f.scanned = append(f.scanned, path)
	return skillscan.Report{Output: "报告"}, f.err
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

func TestInstallSkill_扫描通过才存下并且留着(t *testing.T) {
	store, scan := memSkills{}, &fakeScanner{}
	in := &Installer{Client: sources(t, skillRepo(t)), Scanner: scan, Skills: store}
	s, err := in.InstallSkill(context.Background(), "acme/skills/meeting-notes")
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if s.ID != "acme.skills.meeting-notes" || s.Name != "meeting-notes" || s.Goal != "把会议录音整理成纪要" {
		t.Fatalf("字段不对: %+v", s)
	}
	if s.Format != skill.FormatMarkdown || s.Raw != notes || s.ScanReport != "报告" || s.Source == "" {
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

func TestInstallSkill_扫描不通过或没跑成就什么都不留(t *testing.T) {
	for _, scanErr := range []error{skillscan.ErrUnsafe, skillscan.ErrScanFailed} {
		store := memSkills{}
		in := &Installer{Client: sources(t, skillRepo(t)), Scanner: &fakeScanner{err: scanErr}, Skills: store}
		if _, err := in.InstallSkill(context.Background(), "acme/skills/meeting-notes"); !errors.Is(err, scanErr) {
			t.Fatalf("应返回扫描的结论 %v，实际 %v", scanErr, err)
		}
		if len(store) != 0 {
			t.Fatal("扫描没过的 Skill 不该进库")
		}
	}
	// 没有扫描器不等于可以跳过扫描。
	in := &Installer{Client: sources(t, skillRepo(t)), Skills: memSkills{}}
	if _, err := in.InstallSkill(context.Background(), "acme/skills/meeting-notes"); !errors.Is(err, skillscan.ErrScanFailed) {
		t.Fatalf("没有扫描器时应拒绝: %v", err)
	}
}

func TestInstallServer_扫描通过并连上才存下(t *testing.T) {
	ctx := context.Background()
	var connected []string
	build := func(scan *fakeScanner, store memServers, connectErr error) *Installer {
		return &Installer{
			Client: sources(t, nil), Scanner: scan, Servers: store,
			Connect: func(_ context.Context, s *Server) ([]string, error) {
				connected = append(connected, s.Name)
				return []string{s.Name + ".forecast"}, connectErr
			},
		}
	}

	store := memServers{}
	srv, tools, err := build(&fakeScanner{}, store, nil).InstallServer(ctx, "app.example/weather")
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if srv.Name != "mcp-app-example-weather" || srv.URL != "https://weather.example.app/mcp" || len(tools) != 1 {
		t.Fatalf("结果不对: %+v %v", srv, tools)
	}
	if store[srv.Name] == nil || !strings.Contains(srv.Payload, `"app.example/weather"`) {
		t.Fatalf("应存下登记条目的原文: %+v", store)
	}

	// 扫描不通过：不连接，不存。
	connected, store = nil, memServers{}
	if _, _, err := build(&fakeScanner{err: skillscan.ErrUnsafe}, store, nil).InstallServer(ctx, "app.example/weather"); !errors.Is(err, skillscan.ErrUnsafe) {
		t.Fatalf("应返回扫描的结论: %v", err)
	}
	if len(connected) != 0 || len(store) != 0 {
		t.Fatal("扫描没过的 server 不该被连接，也不该进库")
	}

	// 连不上：不存。
	store = memServers{}
	if _, _, err := build(&fakeScanner{}, store, errors.New("连不上")).InstallServer(ctx, "app.example/weather"); err == nil {
		t.Fatal("连不上应报错")
	}
	if len(store) != 0 {
		t.Fatal("连不上的 server 不该进库")
	}

	// 要密钥的、要在本机运行的：助手不能自己装。
	for _, name := range []string{"app.example/keyed", "app.example/local"} {
		scan := &fakeScanner{}
		if _, _, err := build(scan, memServers{}, nil).InstallServer(ctx, name); err == nil || len(scan.scanned) != 0 {
			t.Errorf("%s 应被拒绝: %v", name, err)
		}
	}
}
