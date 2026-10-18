package config

import (
	"context"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/llm"
)

const twoProviders = `
providers:
  - name: deepseek
    base_url: https://api.deepseek.com/v1
    api_key: vault://deepseek
  - name: qwen
    base_url: https://llm-abc.cn-beijing.maas.aliyuncs.com/compatible-mode/v1
    api_key: vault://qwen
routes:
  - purpose: plan
    provider: deepseek
    model: deepseek-flash
agents:
  - name: q
    provider: qwen
    model: qwen3.8-flash
`

func searchOf(t *testing.T, body string) (llm.Route, bool, *fakeLender) {
	t.Helper()
	f, err := LoadFile(writeConfig(t, body))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	lender := &fakeLender{}
	r, err := f.BuildRouter(context.Background(), lender)
	if err != nil {
		t.Fatalf("构造路由失败: %v", err)
	}
	route, ok := r.Routes()[llm.PurposeSearch]
	if ok != r.CanSearch() {
		t.Fatal("CanSearch 与路由表不一致")
	}
	return route, ok, lender
}

func TestSearch_不用配置自动用上自带联网的模型(t *testing.T) {
	// 主 AI 所在的服务优先。DeepSeek 的搜索在另一个接口上，所以是另起的一个连接，用同一把密钥。
	route, ok, lender := searchOf(t, twoProviders)
	if !ok || route.Provider != "deepseek/search" || route.Model != "deepseek-flash" {
		t.Fatalf("应自动用主 AI 的模型来搜: %+v %v", route, ok)
	}
	if n := strings.Count(strings.Join(lender.asked, ","), "provider.deepseek=vault://deepseek"); n != 2 {
		t.Fatalf("搜索用的连接应以这家服务自己的名义取同一把密钥，实际取了 %d 次: %v", n, lender.asked)
	}

	// 主 AI 的服务不能搜时，用别的能搜的，模型取它已经在用的那个。
	other := strings.Replace(twoProviders, "https://api.deepseek.com/v1", "https://llm.example.com/v1", 1)
	route, ok, _ = searchOf(t, other)
	if !ok || route.Provider != "qwen" || route.Model != "qwen3.8-flash" {
		t.Fatalf("应退到百炼上的模型: %+v %v", route, ok)
	}

	// 一家能搜的都没有：不接入，不报错。
	none := strings.Replace(other, "https://llm-abc.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", "https://api.example.org/v1", 1)
	if _, ok, _ := searchOf(t, none); ok {
		t.Fatal("没有认得的服务时不该接入联网搜索")
	}

	// 按 anthropic 协议接入的服务，直接在原来的连接上搜。
	anth := strings.Replace(other, "    api_key: vault://deepseek", "    protocol: anthropic\n    api_key: vault://deepseek", 1)
	route, ok, _ = searchOf(t, anth)
	if !ok || route.Provider != "deepseek" {
		t.Fatalf("anthropic 协议的服务应可直接搜: %+v %v", route, ok)
	}
}

func TestSearch_用户可以指定用哪个模型来搜(t *testing.T) {
	explicit := strings.Replace(twoProviders, "agents:",
		"  - purpose: search\n    provider: qwen\n    model: qwen-plus\nagents:", 1)
	route, ok, _ := searchOf(t, explicit)
	if !ok || route.Provider != "qwen" || route.Model != "qwen-plus" {
		t.Fatalf("应按用户指定的来: %+v %v", route, ok)
	}

	// 指定了一个不认得的服务：启动时就说清楚，而不是装作能搜。
	bad := strings.Replace(explicit, "https://llm-abc.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", "https://api.example.org/v1", 1)
	f, err := LoadFile(writeConfig(t, bad))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, err := f.BuildRouter(context.Background(), &fakeLender{}); err == nil || !strings.Contains(err.Error(), "search") {
		t.Fatalf("指定了不能搜的服务应报错: %v", err)
	}
}

func TestWebTools_配置了DeepSeek就用它的密钥接通搜索接口(t *testing.T) {
	f, err := LoadFile(writeConfig(t, twoProviders))
	if err != nil {
		t.Fatal(err)
	}
	lender := &fakeLender{}
	c, err := f.WebTools(context.Background(), lender)
	if err != nil || c == nil || c.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("应接通 DeepSeek 的搜索接口: %+v %v", c, err)
	}
	if !strings.Contains(strings.Join(lender.asked, ","), "provider.deepseek=vault://deepseek") {
		t.Fatalf("应以这家服务的名义取密钥: %v", lender.asked)
	}
	if f.SearchRouteSet() {
		t.Fatal("没写 purpose: search 时不该报告已指定")
	}

	other, err := LoadFile(writeConfig(t, strings.Replace(twoProviders, "https://api.deepseek.com/v1", "https://llm.example.com/v1", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c, err := other.WebTools(context.Background(), &fakeLender{}); c != nil || err != nil {
		t.Fatalf("没配 DeepSeek 时不接: %+v %v", c, err)
	}
}
