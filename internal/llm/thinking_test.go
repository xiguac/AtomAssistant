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

// sentBody 让一家 provider 发一次请求，返回它发出去的请求体。
func sentBody(t *testing.T, anthropic bool, d Dialect, thinking Thinking) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		if anthropic {
			_ = json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}}})
	}))
	defer srv.Close()
	cfg := OpenAIConfig{Name: "p", BaseURL: srv.URL + "/v1", APIKey: "k", Timeout: 5 * time.Second, Dialect: d}
	var p Provider
	var err error
	if anthropic {
		p, err = NewAnthropicCompatible(cfg)
	} else {
		p, err = NewOpenAICompatible(cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(context.Background(), Request{Model: "m", Thinking: thinking,
		Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestThinking_按各家的写法传思考强度(t *testing.T) {
	// 不指定：什么都不带，按服务的默认。
	for _, d := range []Dialect{DialectGeneric, DialectDeepSeek, DialectBailian} {
		b := sentBody(t, false, d, ThinkingDefault)
		for _, k := range []string{"reasoning_effort", "thinking", "enable_thinking", "thinking_budget"} {
			if _, ok := b[k]; ok {
				t.Fatalf("%q 没指定思考强度时不该带 %s: %v", d, k, b)
			}
		}
	}

	b := sentBody(t, false, DialectDeepSeek, ThinkingOff)
	if b["thinking"].(map[string]any)["type"] != "disabled" || b["reasoning_effort"] != nil {
		t.Fatalf("DeepSeek 关闭思考不对: %v", b)
	}
	b = sentBody(t, false, DialectDeepSeek, ThinkingMax)
	if b["thinking"].(map[string]any)["type"] != "enabled" || b["reasoning_effort"] != "max" {
		t.Fatalf("DeepSeek 最高档不对: %v", b)
	}

	b = sentBody(t, false, DialectBailian, ThinkingOff)
	if b["enable_thinking"] != false || b["thinking_budget"] != nil {
		t.Fatalf("百炼关闭思考不对: %v", b)
	}
	b = sentBody(t, false, DialectBailian, ThinkingMedium)
	if b["enable_thinking"] != true || b["thinking_budget"] != float64(8192) {
		t.Fatalf("百炼中档不对: %v", b)
	}

	// 通用的服务：关闭没有统一写法，不传；最高按 high。
	if b = sentBody(t, false, DialectGeneric, ThinkingOff); b["reasoning_effort"] != nil {
		t.Fatalf("通用服务关闭时不该传: %v", b)
	}
	if b = sentBody(t, false, DialectGeneric, ThinkingMax); b["reasoning_effort"] != "high" {
		t.Fatalf("通用服务最高档应传 high: %v", b)
	}

	// DeepSeek 的 Anthropic 兼容接口。
	if b = sentBody(t, true, DialectDeepSeek, ThinkingOff); b["reasoning"].(map[string]any)["effort"] != "none" {
		t.Fatalf("DeepSeek Anthropic 接口关闭思考不对: %v", b)
	}
	if b = sentBody(t, true, DialectGeneric, ThinkingHigh); b["reasoning"] != nil {
		t.Fatalf("没适配的 Anthropic 服务不该带: %v", b)
	}
}

func TestThinking_路由上配的强度用在调用上(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}}})
	}))
	defer srv.Close()
	p, err := NewOpenAICompatible(OpenAIConfig{Name: "p", BaseURL: srv.URL, APIKey: "k", Dialect: DialectDeepSeek})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter()
	if err := r.AddProvider(p); err != nil {
		t.Fatal(err)
	}
	if err := r.AddWorker("w", Route{Provider: "p", Model: "m", Thinking: ThinkingLow}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CompleteWith(context.Background(), "w", Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if body["reasoning_effort"] != "low" {
		t.Fatalf("应带上协作模型配的思考强度: %v", body)
	}
}

func TestInputs_文字必须有不写按文字和图片(t *testing.T) {
	if in, err := ParseInputs(nil); err != nil || !Accepts(in, InputImage) || !Accepts(in, InputText) {
		t.Fatalf("不写应按文字和图片: %v %v", in, err)
	}
	if _, err := ParseInputs([]string{"image"}); err == nil {
		t.Fatal("没有文字应被拒绝")
	}
	if _, err := ParseInputs([]string{"text", "smell"}); err == nil {
		t.Fatal("不认得的类型应被拒绝")
	}
	if in, err := ParseInputs([]string{"text"}); err != nil || Accepts(in, InputImage) {
		t.Fatalf("只写文字就不支持图片: %v %v", in, err)
	}
	if _, err := ParseThinking("extreme"); err == nil {
		t.Fatal("不认得的思考强度应被拒绝")
	}
}

func TestMedia_语音视频按各家的写法交给模型(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: "听听这段", Audio: []Image{{MIME: "audio/wav", Data: []byte("RIFF")}},
		Video: []Image{{MIME: "video/mp4", Data: []byte("mp4")}}}}
	parts := func(d Dialect) []any {
		t.Helper()
		raw, _ := json.Marshal(openAIMessages(d, msgs))
		var out []map[string]any
		_ = json.Unmarshal(raw, &out)
		return out[0]["content"].([]any)
	}
	// 阿里云百炼：语音数据写成 data:;base64,…；视频按 video_url。
	p := parts(DialectBailian)
	audio := p[1].(map[string]any)["input_audio"].(map[string]any)
	video := p[2].(map[string]any)["video_url"].(map[string]any)
	if p[1].(map[string]any)["type"] != "input_audio" || audio["data"] != "data:;base64,UklGRg==" || audio["format"] != "wav" ||
		p[2].(map[string]any)["type"] != "video_url" || video["url"] != "data:video/mp4;base64,bXA0" {
		t.Fatalf("百炼的写法不对: %v", p)
	}
	// OpenAI：语音数据直接是 Base64。
	if a := parts(DialectGeneric)[1].(map[string]any)["input_audio"].(map[string]any); a["data"] != "UklGRg==" {
		t.Fatalf("OpenAI 的写法不对: %v", a)
	}
	// Anthropic 的接口不收语音：说清楚，不发出去。
	if _, err := anthropicMessage(RoleUser, msgs[0]); err == nil || !strings.Contains(err.Error(), "语音") {
		t.Fatalf("Anthropic 不收语音应报错: %v", err)
	}
}
