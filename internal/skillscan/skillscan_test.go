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
