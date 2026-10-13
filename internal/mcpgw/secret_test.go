package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xiguac/atomhelper/internal/policy"

	"github.com/xiguac/atomhelper/internal/core"
)

// keyedServer 起一个要在网址里带 key 的远程 MCP server，像高德那样。
func keyedServer(t *testing.T, key string) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "maps", Version: "v1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "weather", Description: "查天气"},
		func(context.Context, *mcp.CallToolRequest, cityArgs) (*mcp.CallToolResult, weatherOut, error) {
			return nil, weatherOut{Temp: 21}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != key {
			http.Error(w, "INVALID_USER_KEY", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestProbe_远程server的密钥在请求时才加进网址(t *testing.T) {
	const key = "amap-key-123456"
	ts := keyedServer(t, key)
	lend := func(context.Context) (string, error) { return key, nil }

	tools, err := Probe(context.Background(), ServerSpec{
		Name: "amap", Transport: TransportHTTP, URL: ts.URL, SecretName: "key", Secret: lend,
	})
	if err != nil || len(tools) != 1 || tools[0].Name != "amap.weather" {
		t.Fatalf("带着密钥应能连上并读到工具: %+v %v", tools, err)
	}

	// 没有密钥连不上。
	if _, err := Probe(context.Background(), ServerSpec{Name: "amap", Transport: TransportHTTP, URL: ts.URL}); err == nil {
		t.Fatal("不带密钥应连不上")
	}

	// 连不上时的报错里不能带着密钥：它会进日志。
	ts.Close()
	_, err = Probe(context.Background(), ServerSpec{
		Name: "amap", Transport: TransportHTTP, URL: ts.URL, SecretName: "key", Secret: lend,
	})
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("连接失败的报错里不应出现密钥: %v", err)
	}
}

func TestBuildTransport_密钥的取用(t *testing.T) {
	ctx := context.Background()
	lend := func(context.Context) (string, error) { return "s3cret", nil }

	// 本地 server：作为环境变量交给子进程，排在显式声明的变量之后。
	tr, err := buildTransport(ctx, ServerSpec{
		Name: "x", Transport: TransportCommand, Command: "echo", Env: []string{"A=1"},
		SecretName: "AMAP_MAPS_API_KEY", Secret: lend,
	})
	if err != nil {
		t.Fatalf("构造传输失败: %v", err)
	}
	env := tr.(*mcp.CommandTransport).Command.Env
	if len(env) < 2 || env[len(env)-2] != "A=1" || env[len(env)-1] != "AMAP_MAPS_API_KEY=s3cret" {
		t.Fatalf("密钥应作为环境变量传入: %v", env)
	}

	// 远程 server：网址本身不带密钥。
	tr, err = buildTransport(ctx, ServerSpec{
		Name: "x", Transport: TransportHTTP, URL: "https://mcp.example.com/mcp", SecretName: "key", Secret: lend,
	})
	if err != nil {
		t.Fatalf("构造传输失败: %v", err)
	}
	if st := tr.(*mcp.StreamableClientTransport); strings.Contains(st.Endpoint, "s3cret") || st.HTTPClient == nil {
		t.Fatalf("密钥不应拼进网址: %+v", st)
	}

	// 保险库不给、或者没说参数名，都连不了。
	denied := func(context.Context) (string, error) { return "", errors.New("未授权") }
	for _, spec := range []ServerSpec{
		{Name: "x", Transport: TransportHTTP, URL: "https://a.example.com", SecretName: "key", Secret: denied},
		{Name: "x", Transport: TransportHTTP, URL: "https://a.example.com", Secret: lend},
	} {
		if _, err := buildTransport(ctx, spec); err == nil {
			t.Fatalf("配置 %+v 应被拒绝", spec)
		}
	}
}

// TestPushGuard_推送通道建不起来不拖垮连接 模拟滴滴那样的 server：POST 正常，长挂着的 GET 没有回音。
func TestPushGuard_推送通道建不起来不拖垮连接(t *testing.T) {
	old := pushWait
	pushWait = 300 * time.Millisecond
	t.Cleanup(func() { pushWait = old })

	srv := mcp.NewServer(&mcp.Implementation{Name: "didi-like", Version: "v1"}, nil)
	type args struct {
		City string `json:"city"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "search", Description: "搜地点"},
		func(_ context.Context, _ *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "found " + in.City}}}, nil, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	hung := make(chan struct{}, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// 既不提供也不回绝：挂着，直到对方放弃。
			hung <- struct{}{}
			<-r.Context().Done()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	reg := policy.NewRegistry()
	if err := reg.Register(policy.ToolGrant{Name: "didi.search", MaxPrivacy: core.PrivacySensitive,
		TrustCeiling: core.TrustToolOutput, SideEffect: core.SideEffectReadOnly}); err != nil {
		t.Fatal(err)
	}
	gw, err := New(Options{Guard: policy.NewGuard(policy.NewClassifier(), reg), Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gw.Connect(ctx, ServerSpec{Name: "didi", Transport: TransportHTTP, URL: ts.URL}); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	// 等推送通道那个请求挂上、又被放弃。
	select {
	case <-hung:
	case <-time.After(10 * time.Second):
		t.Fatal("没有等到推送通道的请求")
	}
	time.Sleep(3 * pushWait)
	// 连接还在，工具照常能调，而且不止一次。
	for i := 0; i < 2; i++ {
		res, err := gw.Call(ctx, CallRequest{Tool: "didi.search", Params: json.RawMessage(`{"city":"北京"}`)})
		if err != nil || !strings.Contains(string(res.Output), "found 北京") {
			t.Fatalf("推送通道建不起来不应影响工具调用: %v %s", err, res.Output)
		}
	}
}

// TestConnect_会话的寿命不跟着建立它的那次调用走 复现的是这样一件事：启动时用一个 60 秒超时的 context 连 server，
// 60 秒一到，SDK 维持会话的长连接跟着断掉，重试几次后整个会话被关，工具全部调不动。
func TestConnect_会话的寿命不跟着建立它的那次调用走(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "maps", Version: "v1"}, nil)
	type args struct {
		City string `json:"city"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "search", Description: "搜地点"},
		func(_ context.Context, _ *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "found " + in.City}}}, nil, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	opened, closed := make(chan struct{}, 8), make(chan struct{}, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// 记下长连接什么时候建起来、什么时候断。
			opened <- struct{}{}
			go func() { <-r.Context().Done(); closed <- struct{}{} }()
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	reg := policy.NewRegistry()
	if err := reg.Register(policy.ToolGrant{Name: "maps.search", MaxPrivacy: core.PrivacySensitive,
		TrustCeiling: core.TrustToolOutput, SideEffect: core.SideEffectReadOnly}); err != nil {
		t.Fatal(err)
	}
	gw, err := New(Options{Guard: policy.NewGuard(policy.NewClassifier(), reg), Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	// 建立会话用的 context 很快就到期，像启动阶段的那个。
	connectCtx, cancel := context.WithCancel(context.Background())
	if err := gw.Connect(connectCtx, ServerSpec{Name: "maps", Transport: TransportHTTP, URL: ts.URL}); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("没有等到长连接建立")
	}
	cancel()
	// 建立它的那次调用结束了，长连接应该还在。
	select {
	case <-closed:
		t.Fatal("建立会话的 context 结束后，会话的长连接不应跟着断")
	case <-time.After(700 * time.Millisecond):
	}
	res, err := gw.Call(context.Background(), CallRequest{Tool: "maps.search", Params: json.RawMessage(`{"city":"北京"}`)})
	if err != nil || !strings.Contains(string(res.Output), "found 北京") {
		t.Fatalf("会话应仍然可用: %v %s", err, res.Output)
	}

	// 连不上的 server：等到 ctx 到期就返回，不会一直挂着。
	down := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(down.Close)
	short, stop := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer stop()
	begin := time.Now()
	if err := gw.Connect(short, ServerSpec{Name: "down", Transport: TransportHTTP, URL: down.URL}); err == nil || time.Since(begin) > 5*time.Second {
		t.Fatalf("连不上时应在 ctx 到期后返回错误: %v（用了 %s）", err, time.Since(begin))
	}
}
