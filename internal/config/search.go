package config

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/xiguac/atomhelper/internal/connector/deepseek"
	"github.com/xiguac/atomhelper/internal/llm"
)

// 联网搜索用的是模型服务自带的搜索能力。配置了 DeepSeek 时另有它的搜索、抓取接口（见 WebTools），
// 没有指定 purpose: search 的路由时，搜索改走那个接口。
//
// 模型服务自带的搜索，
// 各家的开法不一样，目前认得三种：
//   - 阿里云百炼：OpenAI 兼容接口上加 enable_search。
//   - DeepSeek：只在它的 Anthropic 兼容接口（/anthropic）上提供，是服务端执行的 web_search 工具。
//   - 其他按 Anthropic 协议接入的服务：同样用服务端的 web_search 工具。
//
// 用户不必为此单独配置：已经配好的模型服务里有认得的，就自动用它。
// 想指定用哪个模型来搜，写一条 purpose: search 的路由。

// searchKind 是一家模型服务开启联网搜索的方式，空值表示不认得。
type searchKind string

const (
	searchBailian   searchKind = "bailian"
	searchDeepSeek  searchKind = "deepseek"
	searchAnthropic searchKind = "anthropic"
)

// deepseekAnthropicBase 是 DeepSeek 的 Anthropic 兼容接口，它的联网搜索只在这里提供。
const deepseekAnthropicBase = "https://api.deepseek.com/anthropic"

func (p ProviderConfig) searchKind() searchKind {
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "api.deepseek.com":
		return searchDeepSeek
	case p.Protocol == ProtocolAnthropic:
		return searchAnthropic
	case host == "aliyuncs.com" || strings.HasSuffix(host, ".aliyuncs.com"):
		return searchBailian
	}
	return ""
}

// modelOn 找出配置里已经在用的、属于这家服务的一个模型：先看主 AI，再看其他用途，最后看协作模型。
func (f *File) modelOn(provider string) string {
	for _, r := range f.Routes {
		if r.Provider == provider && r.Purpose == string(llm.PurposePlan) {
			return r.Model
		}
	}
	for _, r := range append(append([]RouteConfig{}, f.Routes...), fallbackOf(f)...) {
		if r.Provider == provider {
			return r.Model
		}
	}
	for _, a := range f.Agents {
		if a.Provider == provider {
			return a.Model
		}
	}
	return ""
}

// searchRoute 定出用哪家服务的哪个模型来联网搜索。没有可用的返回 ok=false。
//
// 用户写了 purpose: search 的路由就用它；否则在配置过的服务里自动找，主 AI 所在的那家优先。
func (f *File) searchRoute() (pc ProviderConfig, route RouteConfig, ok bool, err error) {
	byName := make(map[string]ProviderConfig, len(f.Providers))
	for _, p := range f.Providers {
		byName[p.Name] = p
	}
	for _, r := range f.Routes {
		if r.Purpose != string(llm.PurposeSearch) {
			continue
		}
		p := byName[r.Provider]
		if p.searchKind() == "" {
			return pc, route, false, fmt.Errorf(
				"config: 用途 search 指定的模型服务 %s 不在已知自带联网搜索的服务之列（目前认得阿里云百炼、DeepSeek，以及按 anthropic 协议接入的服务）", r.Provider)
		}
		return p, r, true, nil
	}
	main, _ := (&Models{Routes: f.Routes, Fallback: f.Fallback}).Main()
	ordered := make([]ProviderConfig, 0, len(f.Providers))
	if p, found := byName[main]; found {
		ordered = append(ordered, p)
	}
	for _, p := range f.Providers {
		if p.Name != main {
			ordered = append(ordered, p)
		}
	}
	for _, p := range ordered {
		model := f.modelOn(p.Name)
		if p.searchKind() == "" || model == "" {
			continue
		}
		return p, RouteConfig{Purpose: string(llm.PurposeSearch), Provider: p.Name, Model: model}, true, nil
	}
	return pc, route, false, nil
}

// WebTools 用配置过的 DeepSeek 服务的密钥接通它的联网搜索与网页抓取接口（/v1/tools/search、/v1/tools/fetch）。
// 没有配置 DeepSeek 时返回 nil。有多个时主 AI 所在的那个优先。
func (f *File) WebTools(ctx context.Context, secrets SecretLender) (*deepseek.Client, error) {
	main, _ := (&Models{Routes: f.Routes, Fallback: f.Fallback}).Main()
	var pick *ProviderConfig
	for i := range f.Providers {
		p := &f.Providers[i]
		if p.searchKind() != searchDeepSeek {
			continue
		}
		if pick == nil || p.Name == main {
			pick = p
		}
	}
	if pick == nil {
		return nil, nil
	}
	key, err := f.apiKey(ctx, *pick, secrets)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(pick.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("config: provider %s 的 base_url 不合法: %w", pick.Name, err)
	}
	return &deepseek.Client{BaseURL: u.Scheme + "://" + u.Host, Key: key}, nil
}

// SearchRouteSet 报告用户是否写了 purpose: search 的路由，指定用某个模型来联网搜索。
func (f *File) SearchRouteSet() bool {
	for _, r := range f.Routes {
		if r.Purpose == string(llm.PurposeSearch) {
			return true
		}
	}
	return false
}

// addSearch 给路由接上联网搜索。配置过的服务里没有能搜的就什么都不做。
func (f *File) addSearch(ctx context.Context, r *llm.Router, secrets SecretLender) error {
	pc, rc, ok, err := f.searchRoute()
	if err != nil || !ok {
		return err
	}
	provider := pc.Name
	// DeepSeek 平时走 OpenAI 兼容接口，搜索要换到它的另一个接口上：同一把密钥，另起一个连接。
	if pc.searchKind() == searchDeepSeek && pc.Protocol != ProtocolAnthropic {
		key, err := f.apiKey(ctx, pc, secrets)
		if err != nil {
			return err
		}
		provider = pc.Name + "/search"
		p, err := llm.NewAnthropicCompatible(llm.OpenAIConfig{
			Name: provider, BaseURL: deepseekAnthropicBase, APIKey: key,
		})
		if err != nil {
			return err
		}
		if err := r.AddProvider(p); err != nil {
			return err
		}
	}
	return r.SetRoute(llm.Route{
		Purpose:           llm.PurposeSearch,
		Provider:          provider,
		Model:             rc.Model,
		MaxTokens:         rc.MaxTokens,
		InputYuanPerMTok:  rc.InputYuanPerMTok,
		OutputYuanPerMTok: rc.OutputYuanPerMTok,
	})
}
