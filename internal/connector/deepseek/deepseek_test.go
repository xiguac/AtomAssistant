package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func key(k string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return k, nil }
}

func TestClient_搜索与抓取按文档的格式请求和解析(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		switch r.URL.Path {
		case "/v1/tools/search":
			_, _ = w.Write([]byte(`{"results":[{"rerank_score":2.4,"title":"DeepSeek V4 模型发布","url":"https://example.com/v4",
				"snippet":"片段","summary":"摘要","date":"2026-06-01T00:00:00+00:00","authority":3}]}`))
		case "/v1/tools/fetch":
			_, _ = w.Write([]byte(`{"title":"Example article","content":"# Example article\n\nArticle content."}`))
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Secret: key("sk-x")}
	ctx := context.Background()

	results, err := c.Search(ctx, " DeepSeek V4 ", 5)
	if err != nil || len(results) != 1 || results[0].URL != "https://example.com/v4" || results[0].Summary != "摘要" {
		t.Fatalf("搜索结果不对: %+v %v", results, err)
	}
	if gotAuth != "Bearer sk-x" || gotPath != "/v1/tools/search" || gotBody["query"] != "DeepSeek V4" || gotBody["count"] != float64(5) {
		t.Fatalf("搜索请求不对: %s %s %v", gotAuth, gotPath, gotBody)
	}

	page, err := c.Fetch(ctx, "https://example.com/article")
	if err != nil || page.Title != "Example article" || !strings.Contains(page.Content, "Article content") {
		t.Fatalf("抓取结果不对: %+v %v", page, err)
	}
	if gotPath != "/v1/tools/fetch" || gotBody["url"] != "https://example.com/article" {
		t.Fatalf("抓取请求不对: %s %v", gotPath, gotBody)
	}
}

func TestClient_参数不合法的不发出去(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Secret: key("sk-x")}
	ctx := context.Background()
	for _, u := range []string{"", "ftp://example.com/a", "file:///etc/passwd", "https://user:pw@example.com/", "example.com"} {
		if _, err := c.Fetch(ctx, u); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q 应被拒绝: %v", u, err)
		}
	}
	if _, err := c.Search(ctx, "  ", 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空搜索词应被拒绝: %v", err)
	}
	if _, err := c.Search(ctx, strings.Repeat("字", MaxQueryRunes+1), 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("过长的搜索词应被拒绝: %v", err)
	}
	if _, err := (&Client{BaseURL: srv.URL}).Search(ctx, "x", 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("没有密钥应被拒绝: %v", err)
	}
	if called {
		t.Fatal("参数不合法时不该发请求")
	}
}

func TestClient_出错时带上服务给的原因(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()
	_, err := (&Client{BaseURL: srv.URL, Secret: key("sk-x")}).Search(context.Background(), "x", 5)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "余额不足") || !strings.Contains(err.Error(), "Insufficient Balance") {
		t.Fatalf("报错不对: %v", err)
	}
}
