package mcpgw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
