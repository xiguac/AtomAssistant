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

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
)

func TestAdmin_用对话修改记忆(t *testing.T) {
	e := setup(t)
	e.login()
	ctx := context.Background()
	post := func(path string, form url.Values) (int, string) {
		t.Helper()
		form.Set("csrf", e.csrf)
		return e.post(path, form)
	}

	pipe := memory.NewPipeline(e.store.Memories(), nil)
	src := memory.Source{Kind: memory.SourceUser, Trust: core.TrustUser}
	out, err := pipe.Ingest(ctx, []memory.Candidate{
		{Kind: memory.KindFact, Subject: "user", Domain: "工作", Attribute: "公司", Value: "在前公司甲科技任职", Temporality: memory.TemporalDurable, Source: src},
		{Kind: memory.KindFact, Subject: "user", Domain: "生日", Attribute: "生日", Value: "生日是 5 月 1 日", Temporality: memory.TemporalPermanent, Source: src},
		{Kind: memory.KindPreference, Subject: "user", Domain: "饮食", Attribute: "口味", Value: "不吃辣", Temporality: memory.TemporalDurable, Source: src},
	})
	if err != nil || len(out) != 3 {
		t.Fatalf("写入记忆失败: %v", err)
	}
	job, birthday := out[0].MemoryID, out[1].MemoryID

	// 假的主 AI：删工作那条、改生日那条，外加一个不存在的编号（应被丢掉）。
	var gotPrompt string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotPrompt = string(raw)
		reply, _ := json.Marshal(map[string]any{"changes": []map[string]string{
			{"op": "delete", "id": job, "reason": "与前公司有关"},
			{"op": "update", "id": birthday, "value": "生日是 5 月 3 日", "reason": "用户更正"},
			{"op": "delete", "id": "no-such-id"},
		}, "note": "找到两处"})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "m1", "choices": []any{map[string]any{"message": map[string]string{"content": string(reply)}}}, "usage": map[string]any{}})
	}))
	defer fake.Close()

	if _, loc := post("/memory/ask", url.Values{"ask": {"删掉前公司的内容"}}); !strings.Contains(loc, "主 AI") {
		t.Fatalf("没设主 AI 时应说明: %s", loc)
	}
	post("/models/provider", url.Values{"name": {"本地"}, "base_url": {fake.URL}, "key": {"k"}})
	post("/models/main", url.Values{"provider": {"本地"}, "model": {"m1"}})

	code, body := post("/memory/ask", url.Values{"ask": {"删掉前公司的内容，生日改成 5 月 3 日"}})
	if code != http.StatusOK || !strings.Contains(body, "待确认的修改") || !strings.Contains(body, `value="`+job+`"`) ||
		!strings.Contains(body, "生日是 5 月 3 日") || strings.Contains(body, "no-such-id") {
		t.Fatalf("应列出模型提议的改动等待确认: %d\n%s", code, body)
	}
	if !strings.Contains(gotPrompt, "不吃辣") || !strings.Contains(gotPrompt, "删掉前公司的内容") {
		t.Fatal("应把已保存的记忆和用户的话交给模型")
	}
	// 只是提议：还没确认前记忆不变。
	if m, _ := e.store.Memories().Get(ctx, job); m.Status == memory.StatusArchived {
		t.Fatal("确认之前不应改动记忆")
	}

	// 只勾选删除那一项。
	if _, loc := post("/memory/apply", url.Values{"pick": {"0"}, "op_0": {"delete"}, "id_0": {job},
		"op_1": {"update"}, "id_1": {birthday}, "value_1": {"生日是 5 月 3 日"}}); !strings.Contains(loc, "已删除 1 条、修改 0 条") {
		t.Fatalf("应只落地勾选的: %s", loc)
	}
	if m, _ := e.store.Memories().Get(ctx, job); m.Status != memory.StatusArchived {
		t.Fatal("勾选的删除应生效")
	}
	if m, _ := e.store.Memories().Get(ctx, birthday); m.Current.Value != "生日是 5 月 1 日" {
		t.Fatal("没勾选的不应改动")
	}
	// 再确认修改：新内容算用户说的，旧的留在历史里。
	if _, loc := post("/memory/apply", url.Values{"pick": {"1"}, "op_1": {"update"}, "id_1": {birthday}, "value_1": {"生日是 5 月 3 日"}}); !strings.Contains(loc, "修改 1 条") {
		t.Fatalf("修改失败: %s", loc)
	}
	m, _ := e.store.Memories().Get(ctx, birthday)
	if m.Current.Value != "生日是 5 月 3 日" || m.Current.Source.Kind != memory.SourceUser || len(m.History) == 0 || m.History[len(m.History)-1].Value != "生日是 5 月 1 日" {
		t.Fatalf("修改应生效并保留历史: %+v", m)
	}
	if _, loc := post("/memory/apply", url.Values{}); !strings.Contains(loc, "没有勾选") {
		t.Fatalf("什么都没勾时应说明: %s", loc)
	}
}
