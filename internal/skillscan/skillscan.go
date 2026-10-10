// Package skillscan 是 Skill 安装前的恶意内容扫描闸门。
//
// 助手可以自己提名并安装 Skill，所以每个 Skill 在安装前都必须先被扫描。
// 扫描由外部工具 SkillSpector 完成，这里只负责调用它并把结果收敛成
// "能装 / 不能装"——并且保证扫描没跑成时结论一定是不能装。
//
// 见 docs/MCP市场与Skill.md §5.1。
package skillscan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrUnsafe 表示扫描器判定这个 Skill 不安全。
var ErrUnsafe = errors.New("skillscan: 扫描判定不安全，拒绝安装")

// ErrScanFailed 表示扫描没有跑成：扫描器没装、超时、或自己出了错。
//
// 它和 ErrUnsafe 一样意味着不能安装。没扫过等于不安全——
// 否则让扫描器崩掉就成了绕过它的办法。
var ErrScanFailed = errors.New("skillscan: 扫描未能完成，拒绝安装")

// Scanner 调用外部扫描器。
type Scanner struct {
	// Command 是扫描器的可执行文件，空值按 "skillspector"。
	Command string
	// ExtraArgs 追加在 "scan <路径>" 之后，由用户在后台配置（例如关闭它的联网分析）。
	ExtraArgs []string
	// Timeout 是单次扫描的时限，零值按 5 分钟。
	Timeout time.Duration
}

// Report 是一次扫描的结果。
type Report struct {
	// Output 是扫描器的输出，原样保留用于展示给用户。
	Output string
	// ExitCode 是扫描器的退出码。
	ExitCode int
}

// passthroughEnv 是扫描器进程能继承的环境变量。
//
// 只给运行一个程序所必需的那几项。主进程的环境里有数据库密码和模型密钥，
// 扫描器要读的是一个来历不明的 Skill，不该让它拿着这些东西去读。
var passthroughEnv = []string{
	"PATH", "HOME", "USERPROFILE", "SystemRoot", "TEMP", "TMP", "TMPDIR", "LANG",
}

// Check 扫描 path 处的 Skill。
//
// 只有返回的 error 为 nil 才允许继续安装。判定不安全返回 ErrUnsafe，
// 扫描没跑成返回 ErrScanFailed；两种情况下 Report 都尽量带上扫描器的输出。
func (s Scanner) Check(ctx context.Context, path string) (Report, error) {
	return s.run(ctx, path)
}

// CheckRegistry 扫描 path 处的一份 MCP 登记处条目（登记处接口返回的 JSON 原文）。
//
// 查的是登记信息本身：来源仓库、远程地址、版本这些有没有可疑之处。
// 它看不到远程 server 的代码，所以通过了也只说明"登记信息没问题"。结论的含义与 Check 相同。
func (s Scanner) CheckRegistry(ctx context.Context, path string) (Report, error) {
	// 这个模式下扫描器只支持 JSON 输出。
	return s.run(ctx, path, "--mcp-registry", "--format", "json")
}

func (s Scanner) run(ctx context.Context, path string, mode ...string) (Report, error) {
	if strings.TrimSpace(path) == "" {
		return Report{}, fmt.Errorf("%w: 没有给出要扫描的路径", ErrScanFailed)
	}
	command := s.Command
	if command == "" {
		command = "skillspector"
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append([]string{"scan", path}, mode...)
	args = append(args, s.ExtraArgs...)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = make([]string, 0, len(passthroughEnv))
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	rep := Report{Output: out.String()}
	if err == nil {
		return rep, nil
	}
	if ctx.Err() != nil {
		return rep, fmt.Errorf("%w: 超过 %s 仍未结束", ErrScanFailed, timeout)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		// 进程根本没起来：扫描器没装，或路径配错了。
		return rep, fmt.Errorf("%w: 无法运行扫描器 %q: %v", ErrScanFailed, command, err)
	}
	rep.ExitCode = exit.ExitCode()
	// SkillSpector 约定：1 表示风险过高或触发策略门，其余非零都是它自己出错。
	if rep.ExitCode == 1 {
		return rep, ErrUnsafe
	}
	return rep, fmt.Errorf("%w: 扫描器以退出码 %d 结束", ErrScanFailed, rep.ExitCode)
}
