package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRouter_联网搜索不走兜底(t *testing.T) {
	r := NewRouter()
	plain := &stubProvider{name: "plain"}
	mustAdd(t, r, plain)
	if err := r.SetFallback(Route{Provider: "plain", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	// 兜底的模型不一定能联网：没有专门配置就明确报错，而不是拿它顶上。
	if r.CanSearch() {
		t.Fatal("没配置联网搜索时不该报告可以搜")
	}
	if _, err := r.Search(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "q"}}}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("没有能联网的模型时应报错: %v", err)
	}

	searcher := &stubProvider{name: "searcher"}
	mustAdd(t, r, searcher)
	mustRoute(t, r, Route{Purpose: PurposeSearch, Provider: "searcher", Model: "s1"})
	if !r.CanSearch() {
		t.Fatal("配置之后应报告可以搜")
	}
	if _, err := r.Search(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "q"}}}); err != nil {
		t.Fatal(err)
	}
	if !searcher.calls[len(searcher.calls)-1].WebSearch || searcher.calls[len(searcher.calls)-1].Model != "s1" {
		t.Fatalf("搜索请求应带上联网标记并用搜索的模型: %+v", searcher.calls)
	}
	// 平常的调用不带这个标记。
	if _, err := r.Complete(context.Background(), PurposePlan, Request{Messages: []Message{{Role: RoleUser, Content: "q"}}}); err != nil {
		t.Fatal(err)
	}
	if plain.calls[len(plain.calls)-1].WebSearch {
		t.Fatal("平常的调用不该带联网标记")
	}
}

func TestOpenAICompatible_联网搜索用百炼的开关(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"杭州今天晴"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	p, err := NewOpenAICompatible(OpenAIConfig{Name: "qwen", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []Message{{Role: RoleUser, Content: "杭州天气"}}
	if _, err := p.Complete(context.Background(), Request{Model: "m", Messages: msgs, WebSearch: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(context.Background(), Request{Model: "m", Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	opts, _ := bodies[0]["search_options"].(map[string]any)
	if bodies[0]["enable_search"] != true || opts["forced_search"] != true {
		t.Fatalf("请求联网时应打开搜索并要求一定去搜: %v", bodies[0])
	}
	// 没请求联网时这两个字段根本不出现：别家的接口不认识它们。
	if _, ok := bodies[1]["enable_search"]; ok {
		t.Fatalf("平常的请求不该带搜索开关: %v", bodies[1])
	}
	if _, ok := bodies[1]["search_options"]; ok {
		t.Fatalf("平常的请求不该带搜索选项: %v", bodies[1])
	}
}

func TestAnthropicCompatible_联网搜索用服务端工具并带回来源(t *testing.T) {
	var body map[string]any
	reply := `{"model":"m","content":[
		{"type":"thinking","thinking":"先搜一下"},
		{"type":"server_tool_use","id":"c1","name":"web_search","input":{"query":"杭州天气"}},
		{"type":"web_search_tool_result","tool_use_id":"c1","content":[
			{"type":"web_search_result","title":"杭州天气预报","url":"https://www.weather.com.cn/a","encrypted_content":"xx"},
			{"type":"web_search_result","title":"重复的","url":"https://www.weather.com.cn/a"},
			{"type":"web_search_result","title":"墨迹天气","url":"https://tianqi.moji.com/b"}]},
		{"type":"web_search_tool_result","tool_use_id":"c2","content":{"type":"web_search_tool_result_error","error_code":"unavailable"}},
		{"type":"text","text":"杭州今天晴，"},{"type":"text","text":"最高 27 度。"}],
		"usage":{"input_tokens":9,"output_tokens":7}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	p, err := NewAnthropicCompatible(OpenAIConfig{Name: "ds", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []Message{{Role: RoleUser, Content: "杭州天气"}}

	resp, err := p.Complete(context.Background(), Request{Model: "m", Messages: msgs, WebSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "web_search" ||
		!strings.HasPrefix(tools[0].(map[string]any)["type"].(string), "web_search_") {
		t.Fatalf("请求里应带服务端的搜索工具: %v", body["tools"])
	}
	if resp.Content != "杭州今天晴，最高 27 度。" || resp.NoSearch {
		t.Fatalf("回答不对: %+v", resp)
	}
	// 来源去重；搜索出错的那一块不算来源，也不让整次调用失败。
	if len(resp.Sources) != 2 || resp.Sources[0].URL != "https://www.weather.com.cn/a" || resp.Sources[1].Title != "墨迹天气" {
		t.Fatalf("来源不对: %+v", resp.Sources)
	}

	// 请求了联网、模型却一次都没搜：要标出来。
	reply = `{"model":"m","content":[{"type":"text","text":"杭州大概是晴天"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	resp, err = p.Complete(context.Background(), Request{Model: "m", Messages: msgs, WebSearch: true})
	if err != nil || !resp.NoSearch || len(resp.Sources) != 0 {
		t.Fatalf("没搜的回答应被标出: %+v, %v", resp, err)
	}
	// 平常的请求不带工具，也不标。
	resp, err = p.Complete(context.Background(), Request{Model: "m", Messages: msgs})
	if err != nil || resp.NoSearch || body["tools"] != nil {
		t.Fatalf("平常的请求不该带搜索工具: %v, %+v", body["tools"], resp)
	}
}
