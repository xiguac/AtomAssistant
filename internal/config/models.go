package config

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/xiguac/atomhelper/internal/llm"
	"github.com/xiguac/atomhelper/internal/team"
)

// AgentConfig 是一个协作模型：主 AI 可以把一块活派给它。
//
// 配了协作模型，主 AI 才有"派活"这个工具。派给谁不由主 AI 决定，由用户的评价决定，
// 见 internal/team。
type AgentConfig struct {
	// Name 是你给它起的名字，成绩记在这个名字下。改名等于换了一个新的。
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// GoodAt 限定它只接哪些类别（writing / code / analysis / translate / summary / creative / other）。
	// 不写表示什么都接，由成绩说话。
	GoodAt []string `yaml:"good_at,omitempty"`
	// Note 是给你自己看的备注。
	Note      string `yaml:"note,omitempty"`
	MaxTokens int    `yaml:"max_tokens,omitempty"`
	// 每百万 token 的单价（元）。不填则不计费。
	InputYuanPerMTok  float64 `yaml:"input_yuan_per_mtok,omitempty"`
	OutputYuanPerMTok float64 `yaml:"output_yuan_per_mtok,omitempty"`
}

// Workers 返回协作模型的花名册。
func (f *File) Workers() []team.Worker {
	out := make([]team.Worker, 0, len(f.Agents))
	for _, a := range f.Agents {
		w := team.Worker{Name: a.Name, Model: a.Model, Note: a.Note}
		for _, g := range a.GoodAt {
			w.GoodAt = append(w.GoodAt, team.Category(g))
		}
		out = append(out, w)
	}
	return out
}

// validateModels 校验模型相关的几节互相对得上。
//
// 在这里而不是等到启动时才查：后台保存配置只过这一道校验，
// 引用了一个不存在的模型服务这种错，要在保存时就拦下来，而不是重启之后助手起不来。
func (f *File) validateModels() error {
	known := map[string]bool{}
	for _, p := range f.Providers {
		if known[p.Name] {
			return fmt.Errorf("config: 模型服务 %s 重复", p.Name)
		}
		known[p.Name] = true
	}
	for _, rc := range f.Routes {
		if !known[rc.Provider] {
			return fmt.Errorf("config: 用途 %s 用的模型服务 %q 不存在", rc.Purpose, rc.Provider)
		}
		if rc.Model == "" {
			return fmt.Errorf("config: 用途 %s 没有写模型", rc.Purpose)
		}
	}
	if f.Fallback != nil {
		if !known[f.Fallback.Provider] {
			return fmt.Errorf("config: fallback 用的模型服务 %q 不存在", f.Fallback.Provider)
		}
		if f.Fallback.Model == "" {
			return fmt.Errorf("config: fallback 没有写模型")
		}
	}
	names := map[string]bool{}
	for _, a := range f.Agents {
		if strings.TrimSpace(a.Name) == "" || a.Model == "" {
			return fmt.Errorf("config: 协作模型必须配齐 name / provider / model")
		}
		if names[a.Name] {
			return fmt.Errorf("config: 协作模型 %s 重复", a.Name)
		}
		names[a.Name] = true
		if !known[a.Provider] {
			return fmt.Errorf("config: 协作模型 %s 用的模型服务 %q 不存在", a.Name, a.Provider)
		}
		for _, g := range a.GoodAt {
			if !team.ValidCategory(team.Category(g)) {
				return fmt.Errorf("config: 协作模型 %s 的 good_at 里没有 %q 这个类别", a.Name, g)
			}
		}
		if a.InputYuanPerMTok < 0 || a.OutputYuanPerMTok < 0 {
			return fmt.Errorf("config: 协作模型 %s 的单价不能为负", a.Name)
		}
	}
	return nil
}

// Provider 按配置接通一家模型服务。密钥从环境变量或保险库读取。
func (f *File) Provider(ctx context.Context, name string, secrets SecretLender) (llm.Provider, error) {
	for _, pc := range f.Providers {
		if pc.Name != name {
			continue
		}
		var key string
		if pc.APIKey != "" {
			if secrets == nil {
				return nil, fmt.Errorf("config: provider %s 的密钥在保险库里，但保险库不可用", pc.Name)
			}
			take, err := secrets.Lend(pc.APIKey, pc.VaultUser())
			if err != nil {
				return nil, fmt.Errorf("config: provider %s 的 api_key: %w", pc.Name, err)
			}
			// 模型密钥每次请求都要用，所以启动时取一次、留在进程里，和从环境变量读是一样的。
			if key, err = take(ctx); err != nil {
				return nil, fmt.Errorf("config: 从保险库取 provider %s 的密钥失败（存了吗？授权名单里有 %s 吗？）: %w",
					pc.Name, pc.VaultUser(), err)
			}
		} else if key = os.Getenv(pc.APIKeyEnv); key == "" {
			return nil, fmt.Errorf("config: provider %s 的密钥环境变量 %s 未设置",
				pc.Name, pc.APIKeyEnv)
		}
		return llm.NewOpenAICompatible(llm.OpenAIConfig{
			Name: pc.Name, BaseURL: pc.BaseURL, APIKey: key, Timeout: pc.Timeout,
		})
	}
	return nil, fmt.Errorf("config: 没有名叫 %s 的模型服务", name)
}

// Models 是配置里和模型有关的那几节。管理后台的「模型」页读写的就是它。
type Models struct {
	Providers []ProviderConfig `yaml:"providers"`
	Routes    []RouteConfig    `yaml:"routes"`
	Fallback  *RouteConfig     `yaml:"fallback"`
	Agents    []AgentConfig    `yaml:"agents"`
}

// Main 返回主 AI 用的模型服务和模型：规划那条路由，没有的话看兜底。
func (m *Models) Main() (provider, model string) {
	for _, r := range m.Routes {
		if r.Purpose == string(llm.PurposePlan) {
			return r.Provider, r.Model
		}
	}
	if m.Fallback != nil {
		return m.Fallback.Provider, m.Fallback.Model
	}
	return "", ""
}

// SetMain 指定主 AI 用的模型服务和模型。
//
// 兜底路由原来就跟着主 AI（或者还没有）时一起改，否则换了主 AI 之后，
// 提炼记忆这类没有单独指定模型的活还在用旧的那个。用户特意给兜底指定了别的模型的，不动。
func (m *Models) SetMain(provider, model string) {
	oldProvider, oldModel := m.Main()
	if m.Fallback == nil {
		m.Fallback = &RouteConfig{Provider: provider, Model: model}
	} else if m.Fallback.Provider == oldProvider && m.Fallback.Model == oldModel {
		m.Fallback.Provider, m.Fallback.Model = provider, model
	}
	for i := range m.Routes {
		if m.Routes[i].Purpose == string(llm.PurposePlan) {
			m.Routes[i].Provider, m.Routes[i].Model = provider, model
			return
		}
	}
	m.Routes = append([]RouteConfig{{Purpose: string(llm.PurposePlan), Provider: provider, Model: model}}, m.Routes...)
}

// UsedBy 说出一家模型服务被哪些地方用着，没有则返回空。删除之前要看。
func (m *Models) UsedBy(provider string) []string {
	var out []string
	for _, r := range m.Routes {
		if r.Provider != provider {
			continue
		}
		if r.Purpose == string(llm.PurposePlan) {
			out = append(out, "主 AI")
		} else {
			out = append(out, "用途 "+r.Purpose)
		}
	}
	if m.Fallback != nil && m.Fallback.Provider == provider {
		out = append(out, "默认模型")
	}
	for _, a := range m.Agents {
		if a.Provider == provider {
			out = append(out, "协作模型 "+a.Name)
		}
	}
	return out
}

// modelKeys 是 Models 管的那几节在配置文件里的名字。
var modelKeys = []string{"providers", "routes", "fallback", "agents"}

func isModelKey(key string) bool {
	for _, k := range modelKeys {
		if k == key {
			return true
		}
	}
	return false
}

// ReadModels 从配置文件的原文里读出模型相关的几节。
func ReadModels(raw []byte) (Models, error) {
	var m Models
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("config: 解析配置失败: %w", err)
	}
	return m, nil
}

// WriteModels 把模型相关的几节写回配置文件的原文。
//
// 只替换这几节各自占的那几行，其余的部分一个字都不动——注释、空行、排版都是用户自己的。
// 被替换的小节内部的注释留不住。
func WriteModels(raw []byte, m Models) ([]byte, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("config: 解析配置失败: %w", err)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if strings.TrimSpace(text) == "" {
		lines = nil
	}
	// tops 是最外层每个键和它所在的行（从 0 数），按出现的先后。
	type span struct {
		key   string
		start int
	}
	var tops []span
	if len(doc.Content) == 1 {
		root := doc.Content[0]
		if root.Kind != yaml.MappingNode || root.Style&yaml.FlowStyle != 0 {
			return nil, fmt.Errorf("config: 配置文件的最外层要写成一行一个 键: 的形式")
		}
		for i := 0; i+1 < len(root.Content); i += 2 {
			k := root.Content[i]
			if k.Column != 1 {
				return nil, fmt.Errorf("config: 配置文件最外层的键 %s 要顶格写", k.Value)
			}
			tops = append(tops, span{k.Value, k.Line - 1})
		}
	}

	values := map[string]any{}
	if len(m.Providers) > 0 {
		values["providers"] = m.Providers
	}
	if len(m.Routes) > 0 {
		values["routes"] = m.Routes
	}
	if m.Fallback != nil {
		values["fallback"] = m.Fallback
	}
	if len(m.Agents) > 0 {
		values["agents"] = m.Agents
	}
	render := func(key string) ([]string, error) {
		v, ok := values[key]
		if !ok {
			return nil, nil
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(map[string]any{key: v}); err != nil {
			return nil, fmt.Errorf("config: 生成 %s 一节失败: %w", key, err)
		}
		if err := enc.Close(); err != nil {
			return nil, fmt.Errorf("config: 生成 %s 一节失败: %w", key, err)
		}
		return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n"), nil
	}

	// 从后往前换，前面的行号才不会跟着变。
	present := map[string]bool{}
	for i := len(tops) - 1; i >= 0; i-- {
		key := tops[i].key
		if !isModelKey(key) {
			continue
		}
		present[key] = true
		// 这一节到下一个键之前为止；但紧贴着下一个键的空行和顶格注释是下一节的，不算在内。
		end := len(lines)
		if i+1 < len(tops) {
			end = tops[i+1].start
		}
		for end > tops[i].start+1 && (strings.TrimSpace(lines[end-1]) == "" || strings.HasPrefix(lines[end-1], "#")) {
			end--
		}
		fresh, err := render(key)
		if err != nil {
			return nil, err
		}
		lines = append(lines[:tops[i].start], append(fresh, lines[end:]...)...)
	}
	// 原来没有的小节接在最后。
	for _, key := range modelKeys {
		if present[key] {
			continue
		}
		fresh, err := render(key)
		if err != nil {
			return nil, err
		}
		if len(fresh) == 0 {
			continue
		}
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, fresh...)
	}
	if len(lines) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}
