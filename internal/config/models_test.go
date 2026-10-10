package config

import (
	"context"
	"strings"
	"testing"
)

const commented = `# 开头的说明

# ---- 模型 ----
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
  - purpose: extract
    provider: deepseek
    model: deepseek-chat
fallback:
  provider: deepseek
  model: deepseek-chat
  max_tokens: 2000

# 回复风格我想要简洁的
reply:
  style: concise # 行尾的注释
approval:
  caps: {daily_auto_count: 20}
`

func TestWriteModels_只改模型几节其余连同注释原样保留(t *testing.T) {
	m, err := ReadModels([]byte(commented))
	if err != nil {
		t.Fatal(err)
	}
	if p, model := m.Main(); p != "deepseek" || model != "deepseek-chat" {
		t.Fatalf("主 AI 读得不对: %s %s", p, model)
	}
	m.Providers = append(m.Providers, ProviderConfig{Name: "moonshot", BaseURL: "https://api.moonshot.cn/v1", APIKey: "vault://moonshot"})
	// 兜底原来跟着主 AI，换主 AI 时一起换；单独指定了模型的用途不动。
	m.SetMain("moonshot", "kimi")
	m.Agents = []AgentConfig{{Name: "写手", Provider: "deepseek", Model: "deepseek-chat", GoodAt: []string{"writing"}}}

	out, err := WriteModels([]byte(commented), m)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		"# 开头的说明", "# ---- 模型 ----", "# 回复风格我想要简洁的", "style: concise # 行尾的注释", "daily_auto_count: 20",
		"api_key_env: DEEPSEEK_API_KEY", "timeout: 1m30s", "api_key: vault://moonshot", "max_tokens: 4000", "max_tokens: 2000", "name: 写手",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("写回去的配置里应有 %q:\n%s", want, text)
		}
	}
	// 模型几节之外的部分一个字都不动：空行、注释、排版都是用户自己的。
	tail := commented[strings.Index(commented, "# 回复风格"):]
	if !strings.Contains(text, "max_tokens: 2000\n\n"+tail) || !strings.HasPrefix(text, "# 开头的说明\n\n# ---- 模型 ----\nproviders:\n") {
		t.Fatalf("模型几节之外的部分应原样保留:\n%s", text)
	}
	// 没填的项不要写成一堆空值。
	for _, bad := range []string{`api_key: ""`, "max_tokens: 0", "timeout: 0s", "input_yuan_per_mtok"} {
		if strings.Contains(text, bad) {
			t.Fatalf("不该写出空值 %q:\n%s", bad, text)
		}
	}
	f, err := LoadFile(writeConfig(t, text))
	if err != nil {
		t.Fatalf("写回去的配置应能加载: %v\n%s", err, text)
	}
	if f.Routes[0].Provider != "moonshot" || f.Routes[0].Model != "kimi" || f.Routes[0].MaxTokens != 4000 ||
		f.Fallback.Provider != "moonshot" || f.Fallback.MaxTokens != 2000 || f.Routes[1].Provider != "deepseek" {
		t.Fatalf("主 AI 与兜底应一起换，别的用途不动: %+v %+v", f.Routes, f.Fallback)
	}
	if w := f.Workers(); len(w) != 1 || w[0].Name != "写手" || len(w[0].GoodAt) != 1 {
		t.Fatalf("协作模型读得不对: %+v", w)
	}
	if used := m.UsedBy("deepseek"); len(used) != 2 {
		t.Fatalf("deepseek 应还被提炼和写手用着: %v", used)
	}

	// 用户给兜底单独指定过模型的，换主 AI 时不动它。
	m.Fallback.Model = "cheap"
	m.SetMain("deepseek", "deepseek-reasoner")
	if m.Fallback.Provider != "moonshot" || m.Fallback.Model != "cheap" {
		t.Fatalf("单独指定的兜底不应跟着变: %+v", m.Fallback)
	}

	// 空文件：从无到有。全删光：不留空的小节。
	fresh, err := WriteModels(nil, Models{Providers: []ProviderConfig{{Name: "a", BaseURL: "https://a", APIKey: "vault://a"}}})
	if err != nil || !strings.Contains(string(fresh), "name: a") {
		t.Fatalf("空文件应能写入: %v %s", err, fresh)
	}
	gone, err := WriteModels(fresh, Models{})
	if err != nil || strings.Contains(string(gone), "providers") {
		t.Fatalf("删光之后不应留下空的小节: %v %q", err, gone)
	}
}

func TestValidate_模型几节互相对得上(t *testing.T) {
	base := "providers:\n  - {name: d, base_url: 'https://x', api_key: 'vault://d'}\n"
	for name, body := range map[string]string{
		"路由指向不存在的服务":   base + "routes:\n  - {purpose: plan, provider: nope, model: m}\n",
		"兜底指向不存在的服务":   base + "fallback: {provider: nope, model: m}\n",
		"协作模型指向不存在的服务": base + "agents:\n  - {name: 甲, provider: nope, model: m}\n",
		"协作模型重名":       base + "agents:\n  - {name: 甲, provider: d, model: m}\n  - {name: 甲, provider: d, model: n}\n",
		"协作模型的类别写错":    base + "agents:\n  - {name: 甲, provider: d, model: m, good_at: [画画]}\n",
		"协作模型没写模型":     base + "agents:\n  - {name: 甲, provider: d}\n",
		"模型服务重名":       base + "  - {name: d, base_url: 'https://y', api_key: 'vault://d'}\n",
	} {
		if _, err := LoadFile(writeConfig(t, body)); err == nil {
			t.Errorf("%s: 应在加载时就被拒绝", name)
		}
	}
	f, err := LoadFile(writeConfig(t, base+"agents:\n  - {name: 甲, provider: d, model: m, good_at: [code]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Provider(context.Background(), "没有", nil); err == nil {
		t.Fatal("取不存在的模型服务应报错")
	}
}
