package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/xiguac/atomassistant/internal/agent"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
)

func TestAdmin_记忆页先看整合摘要_条目折叠(t *testing.T) {
	e := setup(t)
	e.login()
	ctx := context.Background()

	src := memory.Source{Kind: memory.SourceUser, Trust: core.TrustUser}
	if _, err := memory.NewPipeline(e.store.Memories(), nil).Ingest(ctx, []memory.Candidate{
		{Kind: memory.KindFact, Subject: "user", Domain: "兴趣", Attribute: "跑步", Value: "喜欢长跑", Temporality: memory.TemporalDurable, Source: src},
	}); err != nil {
		t.Fatal(err)
	}

	// 还没生成过：提示可以生成，条目表在折叠区里。
	code, body := e.get("/memory")
	if code != http.StatusOK || !strings.Contains(body, "助手眼中的你") || !strings.Contains(body, "摘要还没有生成") {
		t.Fatalf("没有摘要时应提示生成: %d\n%s", code, body)
	}
	if i, j := strings.Index(body, `<details class="fold">`), strings.Index(body, "喜欢长跑"); i < 0 || j < i {
		t.Fatal("原始条目应在折叠区里")
	}

	// 假的主 AI 写出摘要。
	var gotPrompt string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotPrompt = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "m1", "choices": []any{map[string]any{"message": map[string]string{"content": "你喜欢长跑。"}}}, "usage": map[string]any{}})
	}))
	defer fake.Close()
	post := func(path string, form url.Values) (int, string) {
		form.Set("csrf", e.csrf)
		return e.post(path, form)
	}
	if _, loc := post("/memory/profile", url.Values{}); !strings.Contains(loc, "主 AI") {
		t.Fatalf("没设主 AI 时应说明: %s", loc)
	}
	post("/models/provider", url.Values{"name": {"本地"}, "base_url": {fake.URL}, "key": {"k"}})
	post("/models/main", url.Values{"provider": {"本地"}, "model": {"m1"}})
	post("/memory/profile", url.Values{})

	if !strings.Contains(gotPrompt, "喜欢长跑") {
		t.Fatal("应把已保存的记忆交给模型")
	}
	if saved, _ := e.store.Setting(ctx, agent.SettingProfile); saved != "你喜欢长跑。" {
		t.Fatalf("摘要应存下来: %q", saved)
	}
	if _, body = e.get("/memory"); !strings.Contains(body, "你喜欢长跑。") || strings.Contains(body, "摘要还没有生成") {
		t.Fatalf("页面应显示摘要:\n%s", body)
	}
}
