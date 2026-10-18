package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sse 按 SSE 的格式写出几条事件，每条之后都刷出去。
func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		_, _ = io.WriteString(w, e+"\n\n")
		w.(http.Flusher).Flush()
	}
}

func TestOpenAICompatible_流式返回拼成完整答案(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		sse(w,
			`: keep-alive`,
			`data: {"model":"qwen3-max","choices":[{"delta":{"reasoning_content":"先想想"}}]}`,
			`data: {"model":"qwen3-max","choices":[{"delta":{"content":"你"}}]}`,
			`data: {"model":"qwen3-max","choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`,
			`data: {"model":"qwen3-max","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":6}}}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	p, err := NewOpenAICompatible(OpenAIConfig{Name: "qwen", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "你好" || resp.Model != "qwen3-max" ||
		resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 8 || resp.Usage.CachedTokens != 6 {
		t.Fatalf("返回解析不对: %+v", resp)
	}
	opts, _ := body["stream_options"].(map[string]any)
	if body["stream"] != true || opts["include_usage"] != true {
		t.Fatalf("请求应要求流式并带回用量: %v", body)
	}
}

func TestOpenAICompatible_记下先想了多久再开口(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 本机回环太快，不等一下的话首字节的耗时可能小到读成 0。
		time.Sleep(20 * time.Millisecond)
		sse(w, `data: {"choices":[{"delta":{"reasoning_content":"先想一想这件事"}}]}`)
		time.Sleep(80 * time.Millisecond)
		sse(w,
			`data: {"choices":[{"delta":{"reasoning_content":"再想想"}}]}`,
			`data: {"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	p, err := NewOpenAICompatible(OpenAIConfig{Name: "qwen", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	r := NewRouter().WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	if err := r.AddProvider(p); err != nil {
		t.Fatal(err)
	}
	if err := r.SetFallback(Route{Provider: "qwen", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	resp, err := r.Complete(context.Background(), PurposePlan, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	tm := resp.Timing
	if tm.ReasoningChars != 10 || tm.FirstByte <= 0 || tm.FirstContent-tm.FirstByte < 60*time.Millisecond {
		t.Fatalf("耗时统计不对: %+v", tm)
	}
	for _, want := range []string{"模型调用", "模型=m", "思考字数=10", "首段正文="} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("日志里应有 %q: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "hi") {
		t.Fatalf("日志里不应有消息内容: %s", logs.String())
	}
}

func TestOpenAICompatible_流里报错要带出原因(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `data: {"error":{"message":"quota exceeded"}}`)
	}))
	defer srv.Close()
	p, _ := NewOpenAICompatible(OpenAIConfig{Name: "o", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	_, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("应报上游错误并带出原因: %v", err)
	}
}

func TestAnthropicCompatible_流式返回拼成完整答案(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		sse(w,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m-1\",\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}}",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先搜"}}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"c1","name":"web_search","input":{}}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"x\"}"}}`,
			`data: {"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"c1","content":[{"type":"web_search_result","title":"甲","url":"https://a.example/1"}]}}`,
			`data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`,
			`event: ping`+"\n"+`data: {"type":"ping"}`,
			`data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"杭州"}}`,
			`data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"晴"}}`,
			`data: {"type":"content_block_stop","index":3}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			`data: {"type":"message_stop"}`,
		)
	}))
	defer srv.Close()

	p, err := NewAnthropicCompatible(OpenAIConfig{Name: "a", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Complete(context.Background(), Request{Model: "m-1", WebSearch: true, Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "杭州晴" || resp.Model != "m-1" || resp.Usage.PromptTokens != 9 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("返回解析不对: %+v", resp)
	}
	if resp.NoSearch || len(resp.Sources) != 1 || resp.Sources[0].URL != "https://a.example/1" {
		t.Fatalf("搜索来源不对: %+v", resp)
	}
	if body["stream"] != true {
		t.Fatalf("请求应要求流式: %v", body)
	}
}

func TestAnthropicCompatible_流里报错要带出原因(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	defer srv.Close()
	p, _ := NewAnthropicCompatible(OpenAIConfig{Name: "a", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	_, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("应报上游错误并带出原因: %v", err)
	}
}

func TestProviders_首字超时只看有没有新数据(t *testing.T) {
	const idle = 300 * time.Millisecond
	// 慢慢地流：每段间隔小于时限，总时长远超时限。只要还在来就不该超时。
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 6; i++ {
			time.Sleep(idle / 2)
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"字"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer slow.Close()
	p, _ := NewOpenAICompatible(OpenAIConfig{Name: "slow", BaseURL: slow.URL, APIKey: "k", Timeout: idle})
	resp, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil || resp.Content != "字字字字字字" {
		t.Fatalf("一直有数据来就不该超时: %+v, %v", resp, err)
	}

	// 迟迟不回：响应头都没有。
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer silent.Close()
	defer close(release)
	p, _ = NewOpenAICompatible(OpenAIConfig{Name: "silent", BaseURL: silent.URL, APIKey: "k", Timeout: idle})
	_, err = p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "没有返回任何内容") {
		t.Fatalf("超时应说清楚是等不到内容: %v", err)
	}

	// 流到一半停住。
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, `data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":1}}}`)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stall.Close()
	a, _ := NewAnthropicCompatible(OpenAIConfig{Name: "stall", BaseURL: stall.URL, APIKey: "k", Timeout: idle})
	_, err = a.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "没有返回任何内容") {
		t.Fatalf("流到一半停住也应超时: %v", err)
	}

	// 调用方自己取消时不说成超时。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = a.Complete(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil || strings.Contains(err.Error(), "没有返回任何内容") || !errors.Is(err, context.Canceled) {
		t.Fatalf("调用方取消不该报成超时: %v", err)
	}
}
