package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/agent"
	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/team"
	"github.com/xiguac/atomhelper/internal/vault"
)

// 「模型」页：主 AI 用哪个接口、哪把密钥、哪个模型，有哪些协作模型，它们各自的成绩。
//
// 这一页改的是配置文件里 providers / routes / fallback / agents 这几节，
// 和在「配置」页里手写是一回事，只是不用记格式；密钥照旧只进保险库，配置里留的是句柄。

// providerView 是页面上的一家模型服务。
type providerView struct {
	config.ProviderConfig
	// KeyState 说明密钥放在哪、在不在。KeyOK 为假时这家服务用不了。
	KeyState string
	KeyOK    bool
	UsedBy   []string
}

// standingRow 是成绩榜上的一个类别。
type standingRow struct {
	Category team.CategoryInfo
	Rows     []team.Standing
}

type modelsData struct {
	Providers []providerView
	// MainProvider 与 MainModel 是主 AI 现在用的。
	MainProvider, MainModel string
	// Others 是规划之外单独指定了模型的用途。
	Others     []config.RouteConfig
	Agents     []config.AgentConfig
	Categories []team.CategoryInfo
	Standings  []standingRow
}

// readModels 读出配置文件的原文和其中模型相关的几节。文件还不存在时当作空的。
func (s *Server) readModels() ([]byte, config.Models, error) {
	if s.opts.ConfigPath == "" {
		return nil, config.Models{}, errors.New("未设置 ATOM_CONFIG，无法确定配置文件位置")
	}
	raw, err := os.ReadFile(s.opts.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, config.Models{}, fmt.Errorf("读取配置文件失败: %w", err)
	}
	m, err := config.ReadModels(raw)
	return raw, m, err
}

// editModels 改动模型相关的几节并存回配置文件。校验不过的一个字都不会写进去。
func (s *Server) editModels(change func(m *config.Models) error) error {
	raw, m, err := s.readModels()
	if err != nil {
		return err
	}
	if err := change(&m); err != nil {
		return err
	}
	out, err := config.WriteModels(raw, m)
	if err != nil {
		return err
	}
	return s.writeConfig(string(out))
}

func (s *Server) modelsPage(w http.ResponseWriter, r *http.Request, v *view) {
	d := modelsData{Categories: team.Categories}
	_, m, err := s.readModels()
	if err != nil {
		v.Error = err.Error()
	}
	stored := map[string]bool{}
	if metas, err := s.opts.Vault.List(r.Context()); err != nil {
		v.Error = err.Error()
	} else {
		for _, meta := range metas {
			stored[meta.Label] = true
		}
	}
	for _, p := range m.Providers {
		pv := providerView{ProviderConfig: p, UsedBy: m.UsedBy(p.Name)}
		switch {
		case p.APIKey != "":
			pv.KeyOK = stored[strings.TrimPrefix(p.APIKey, vault.HandlePrefix)]
			pv.KeyState = "保险库 " + p.APIKey
			if !pv.KeyOK {
				pv.KeyState += "（未保存）"
			}
		case p.APIKeyEnv != "":
			pv.KeyOK = os.Getenv(p.APIKeyEnv) != ""
			pv.KeyState = "环境变量 " + p.APIKeyEnv
			if !pv.KeyOK {
				pv.KeyState += "（未设置）"
			}
		}
		d.Providers = append(d.Providers, pv)
	}
	d.MainProvider, d.MainModel = m.Main()
	for _, rc := range m.Routes {
		if rc.Purpose != string(llm.PurposePlan) {
			d.Others = append(d.Others, rc)
		}
	}
	d.Agents = m.Agents
	if len(m.Agents) > 0 {
		board := team.NewBoard(s.opts.Store.Team(), (&config.File{Agents: m.Agents}).Workers())
		for _, c := range team.Categories {
			rows, err := board.Standings(r.Context(), c.ID)
			if err != nil {
				v.Error = err.Error()
				break
			}
			d.Standings = append(d.Standings, standingRow{Category: c, Rows: rows})
		}
	}
	v.Data = d
	s.render(w, "models", v)
}

// providerSave 新增或修改一家模型服务。填了密钥就存进保险库，配置里只留句柄。
func (s *Server) providerSave(w http.ResponseWriter, r *http.Request, _ *view) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	base := strings.TrimRight(strings.TrimSpace(r.PostFormValue("base_url")), "/")
	// 密钥原样存入，只去掉粘贴时常带上的首尾空白——密钥里不会有空白。
	key := strings.TrimSpace(r.PostFormValue("key"))
	protocol := r.PostFormValue("protocol")
	err := func() error {
		if !vault.ValidLabel(name) || strings.ContainsAny(name, " /") {
			return errors.New("名称仅支持字母、数字、中文及 _ - .")
		}
		u, err := url.Parse(base)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("接口地址格式应为 https://…，不含 /chat/completions 或 /messages")
		}
		// 密钥会随每次请求发往这个地址：不是本机就必须加密传输。
		if u.Scheme == "http" && !loopback(u.Hostname()) {
			return errors.New("非本机的接口地址必须使用 https：密钥将随每次请求发送")
		}
		_, m, err := s.readModels()
		if err != nil {
			return err
		}
		at := -1
		for i, p := range m.Providers {
			if p.Name == name {
				at = i
			}
		}
		pc := config.ProviderConfig{Name: name}
		if at >= 0 {
			pc = m.Providers[at]
		}
		pc.BaseURL = base
		// 没选协议表示沿用原来的（新增时即默认的 OpenAI 兼容）。默认协议不写进配置，保持文件简洁。
		switch protocol {
		case "":
		case config.ProtocolOpenAI:
			pc.Protocol = ""
		case config.ProtocolAnthropic:
			pc.Protocol = protocol
		default:
			return errors.New("接口协议取值无效")
		}
		if key != "" {
			// 原来就指着保险库里某一条的，换的就是那一条；否则存成和服务同名的一条。
			label := name
			if pc.APIKey != "" {
				label = strings.TrimPrefix(pc.APIKey, vault.HandlePrefix)
			}
			if err := s.sealProviderKey(r.Context(), label, key, pc); err != nil {
				return err
			}
			pc.APIKey, pc.APIKeyEnv = vault.HandlePrefix+label, ""
		} else if pc.APIKey == "" && pc.APIKeyEnv == "" {
			return errors.New("新增模型服务时必须填写 API Key")
		}
		return s.editModels(func(m *config.Models) error {
			if at >= 0 {
				m.Providers[at] = pc
			} else {
				m.Providers = append(m.Providers, pc)
			}
			return nil
		})
	}()
	if err == nil {
		s.opts.Log.Info("模型服务已在后台修改", "服务", name, "换了密钥", key != "", "来自", r.RemoteAddr)
	}
	notice := "模型服务「" + name + "」已保存，重启服务后生效。"
	// 填了测试用的模型就顺手测一次：地址或密钥填错了，当场就知道，不必等到重启之后。
	if model := strings.TrimSpace(r.PostFormValue("test_model")); err == nil && model != "" {
		result, testErr := s.probeModel(r.Context(), name, model)
		if testErr != nil {
			err = fmt.Errorf("模型服务「%s」已保存，但连接测试未通过: %w", name, testErr)
		} else {
			notice += result
		}
	}
	back(w, r, "/models", notice, err)
}

// sealProviderKey 把一家模型服务的密钥存进保险库。
// 那一条原来允许哪些工具用，照旧允许；再加上这家服务自己。
func (s *Server) sealProviderKey(ctx context.Context, label, key string, pc config.ProviderConfig) error {
	note, tools := "模型服务 "+pc.Name+" 的 API Key", []string{pc.VaultUser()}
	metas, err := s.opts.Vault.List(ctx)
	if err != nil {
		return err
	}
	for _, meta := range metas {
		if meta.Label != label {
			continue
		}
		if meta.Note != "" {
			note = meta.Note
		}
		for _, t := range meta.AllowedTools {
			if t != pc.VaultUser() {
				tools = append(tools, t)
			}
		}
	}
	return s.opts.Vault.Seal(ctx, label, key, note, tools)
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (s *Server) providerDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	err := s.editModels(func(m *config.Models) error {
		if used := m.UsedBy(name); len(used) > 0 {
			return fmt.Errorf("模型服务「%s」正在使用中（%s），请先将其改用其他服务", name, strings.Join(used, "、"))
		}
		for i, p := range m.Providers {
			if p.Name == name {
				m.Providers = append(m.Providers[:i], m.Providers[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("模型服务 %s 不存在", name)
	})
	// 保险库里的密钥不跟着删：它可能还被别处用着，要删去「保险库」页删。
	back(w, r, "/models", "模型服务「"+name+"」已删除。其密钥仍保留在保险库中，可在「保险库」页删除。", err)
}

func (s *Server) mainSave(w http.ResponseWriter, r *http.Request, _ *view) {
	provider := r.PostFormValue("provider")
	model := strings.TrimSpace(r.PostFormValue("model"))
	err := s.editModels(func(m *config.Models) error {
		if provider == "" || model == "" {
			return errors.New("请选择模型服务并填写模型标识")
		}
		m.SetMain(provider, model)
		return nil
	})
	if err == nil {
		s.opts.Log.Info("主 AI 已在后台修改", "服务", provider, "模型", model, "来自", r.RemoteAddr)
	}
	back(w, r, "/models", "主 AI 已设置为 "+provider+" / "+model+"，重启服务后生效。", err)
}

func (s *Server) agentSave(w http.ResponseWriter, r *http.Request, _ *view) {
	a := config.AgentConfig{
		Name:     strings.TrimSpace(r.PostFormValue("name")),
		Provider: r.PostFormValue("provider"),
		Model:    strings.TrimSpace(r.PostFormValue("model")),
		Note:     strings.TrimSpace(r.PostFormValue("note")),
		GoodAt:   r.PostForm["good_at"],
	}
	err := s.editModels(func(m *config.Models) error {
		if a.Name == "" || a.Provider == "" || a.Model == "" {
			return errors.New("请填写协作模型的名称、模型服务与模型标识")
		}
		for i := range m.Agents {
			if m.Agents[i].Name == a.Name {
				// 页面上没有的那几项（长度上限、单价）保留原来的。
				a.MaxTokens = m.Agents[i].MaxTokens
				a.InputYuanPerMTok, a.OutputYuanPerMTok = m.Agents[i].InputYuanPerMTok, m.Agents[i].OutputYuanPerMTok
				m.Agents[i] = a
				return nil
			}
		}
		m.Agents = append(m.Agents, a)
		return nil
	})
	back(w, r, "/models", "协作模型「"+a.Name+"」已保存，重启服务后生效。", err)
}

func (s *Server) agentDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	err := s.editModels(func(m *config.Models) error {
		for i := range m.Agents {
			if m.Agents[i].Name == name {
				m.Agents = append(m.Agents[:i], m.Agents[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("协作模型 %s 不存在", name)
	})
	back(w, r, "/models", "协作模型「"+name+"」已删除。历史评分仍保留，以相同名称重新添加后可继续沿用。", err)
}

// probeModel 用已保存的配置向一个模型发一句话，确认接口地址、密钥、模型标识是否正确，返回给人看的结果。
func (s *Server) probeModel(ctx context.Context, provider, model string) (string, error) {
	if provider == "" || model == "" {
		return "", errors.New("请选择模型服务并填写模型标识")
	}
	file, err := config.LoadFile(s.opts.ConfigPath)
	if err != nil {
		return "", err
	}
	if file == nil {
		return "", errors.New("配置文件不存在")
	}
	p, err := file.Provider(ctx, provider, s.opts.Vault)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := p.Complete(ctx, llm.Request{
		// 留够长度：会先思考再作答的模型，思考也占这个额度，给少了回答是空的。
		Model: model, MaxTokens: 1024,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "这是一次连通测试，请只回复两个字：收到"}},
	})
	if err != nil {
		return "", err
	}
	reply := resp.Content
	if strings.TrimSpace(reply) == "" {
		reply = "（未返回文本）"
	}
	return fmt.Sprintf("%s / %s 连接成功。模型回复：%s", provider, model, shorten(reply, 80)), nil
}

// modelTest 是页面上的「连接测试」按钮。
func (s *Server) modelTest(w http.ResponseWriter, r *http.Request, _ *view) {
	result, err := s.probeModel(r.Context(), r.PostFormValue("provider"), strings.TrimSpace(r.PostFormValue("model")))
	back(w, r, "/models", result, err)
}

func shorten(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// ---- 协作成果的评价 ----

// workView 是任务页上的一份协作成果。
type workView struct {
	team.Work
	Text string
}

// worksOf 列出一个任务里派出去的活，连同交回来的成果。
func (s *Server) worksOf(ctx context.Context, taskID string, steps []*core.Step) ([]workView, error) {
	works, err := s.opts.Store.Team().WorkOfTask(ctx, taskID)
	if err != nil || len(works) == 0 {
		return nil, err
	}
	texts := map[string]string{}
	for _, st := range steps {
		if st.ToolName != agent.ToolDispatch || st.Status != core.StepDone {
			continue
		}
		var out agent.DispatchOutput
		if err := json.Unmarshal(st.Output, &out); err != nil {
			continue
		}
		for i, res := range out.Results {
			texts[team.WorkID(taskID, st.Seq, i)] = res.Text
		}
	}
	views := make([]workView, 0, len(works))
	for _, w := range works {
		views = append(views, workView{Work: w, Text: texts[w.ID]})
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].StepSeq < views[j].StepSeq })
	return views, nil
}

// rateWork 记下用户对一份协作成果的评价。在后台点的，就是用户亲口说的，算一整票。
func (s *Server) rateWork(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PathValue("id")
	work := r.PostFormValue("work")
	verdict := r.PostFormValue("verdict")
	err := func() error {
		if verdict != "good" && verdict != "bad" {
			return errors.New("评价取值无效")
		}
		// 编号是 任务ID:步骤:第几份，别的任务的活不能从这个任务的页面上评。
		if !strings.HasPrefix(work, id+":") {
			return errors.New("该任务项不属于此任务")
		}
		return s.opts.Store.Team().Rate(r.Context(), work, team.Verdict(verdict == "good", team.RatedByUser), team.RatedByUser)
	}()
	back(w, r, "/tasks/"+id, "评价已保存，将用于同类任务的后续分派。", err)
}
