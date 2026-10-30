package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWireNames_换成接口认的写法并能换回来(t *testing.T) {
	long := "srv." + strings.Repeat("x", 80)
	tools := []Tool{{Name: "calendar.events"}, {Name: "amap.maps-weather"}, {Name: "calendar__events"}, {Name: long}, {Name: "中文.工具"}}
	names, back := wireNames(tools)
	if names[0] != "calendar__events" || names[1] != "amap__maps-weather" {
		t.Fatalf("点号应换成两个下划线: %v", names)
	}
	seen := map[string]bool{}
	for i, n := range names {
		if seen[n] || len(n) > maxWireName || n == "" {
			t.Fatalf("撞名、过长或为空: %v", names)
		}
		seen[n] = true
		for _, r := range n {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				t.Fatalf("有接口不认的字符: %q", n)
			}
		}
		if back[n] != tools[i].Name {
			t.Fatalf("换不回原名: %s → %s", n, back[n])
		}
	}
	if string(toolSchema(nil)) != `{"type":"object","properties":{}}` || !strings.Contains(string(toolSchema(json.RawMessage(`{"properties":{"a":{"type":"string"}}}`))), `"type":"object"`) {
		t.Fatal("参数格式应补齐成 object")
	}
}

func TestOpenAICompatible_原生工具调用(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 两个调用交错着分段到：按 index 拼。
		sse(w,
			`data: {"choices":[{"delta":{"reasoning_content":"查一下"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"calendar__events","arguments":""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"from\":"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"weather__now","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"2026-10-31\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()
	p, _ := NewOpenAICompatible(OpenAIConfig{Name: "ds", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	resp, err := p.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools: []Tool{{Name: "calendar.events", Description: "查日程", Parameters: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string"}}}`)}, {Name: "weather.now"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Name != "calendar.events" || string(resp.ToolCalls[0].Arguments) != `{"from":"2026-10-31"}` ||
		resp.ToolCalls[1].Name != "weather.now" || string(resp.ToolCalls[1].Arguments) != `{}` {
		t.Fatalf("工具调用解析不对: %+v", resp.ToolCalls)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 || body["tool_choice"] != nil || body["response_format"] != nil {
		t.Fatalf("应带上两个工具、不强制调用、不开 JSON 模式: %v", body)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "calendar__events" || fn["description"] != "查日程" {
		t.Fatalf("工具定义不对: %v", fn)
	}
	if params := tools[1].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any); params["type"] != "object" {
		t.Fatalf("没写参数格式的工具应补成空 object: %v", params)
	}
}

func TestAnthropicCompatible_原生工具调用(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		sse(w,
			`event: message_start`+"\n"+`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":9,"output_tokens":0}}}`,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"先查日程"}}`,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"calendar__events","input":{}}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"from\":"}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"2026-10-31\"}"}}`,
			`event: message_delta`+"\n"+`data: {"type":"message_delta","usage":{"output_tokens":7}}`,
		)
	}))
	defer srv.Close()
	p, _ := NewAnthropicCompatible(OpenAIConfig{Name: "a", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	resp, err := p.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools: []Tool{{Name: "calendar.events", Description: "查日程"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "先查日程" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "calendar.events" || string(resp.ToolCalls[0].Arguments) != `{"from":"2026-10-31"}` {
		t.Fatalf("工具调用解析不对: %+v", resp)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "calendar__events" || tools[0].(map[string]any)["input_schema"] == nil {
		t.Fatalf("工具定义不对: %v", body["tools"])
	}
}

func TestRouter_带工具的请求没调用也没正文时当场重问(t *testing.T) {
	blank := Response{Content: "  "}
	call := Response{ToolCalls: []ToolCall{{Name: "calendar.events", Arguments: json.RawMessage(`{}`)}}}
	p := &seqProvider{resps: []Response{blank, call}}
	r := NewRouter()
	mustAdd(t, r, p)
	mustRoute(t, r, Route{Purpose: PurposePlan, Provider: p.Name(), Model: "m"})
	resp, err := r.Complete(context.Background(), PurposePlan, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, Tools: []Tool{{Name: "calendar.events"}}})
	if err != nil || len(p.calls) != 2 || len(resp.ToolCalls) != 1 {
		t.Fatalf("应重问一次拿到调用: %+v %v %d", resp, err, len(p.calls))
	}
}
