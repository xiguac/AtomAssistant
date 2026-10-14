// Package sandbox 让助手在一个隔离的环境里跑 Python 代码，处理工作区里的文件。
//
// 隔离靠 bubblewrap（bwrap）：
//   - 断网：没有网络命名空间之外的任何网卡，代码拿到什么都带不出去；
//   - 看不到这台机器上的东西：系统目录只读，/etc 只给运行必需的几个文件（服务的密钥文件在 /etc 里，不给），
//     保险库、配置、数据库都在它看不到的地方；独立的进程空间，环境变量清空；
//   - 工作区只读挂在 /work，代码只能在 /out 里写，跑完之后 /out 里的东西收进工作区的「沙箱输出」；
//   - 限时、限内存、限输出。
//
// 它防的是"代码写错了或者被诱导着做坏事"，不是要抵御内核漏洞级别的攻击。
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ErrUnavailable 表示这台机器上跑不了沙箱。
var ErrUnavailable = errors.New("sandbox: 沙箱不可用")

const (
	// maxCodeBytes 是一段代码的大小上限。
	maxCodeBytes = 256 << 10
	// maxStreamBytes 是标准输出、标准错误各自保留的长度。
	maxStreamBytes = 32 << 10
	// maxOutputFiles 与 maxOutputBytes 限制一次运行能产出多少文件。
	maxOutputFiles = 50
	maxOutputBytes = 200 << 20
)

// Workspace 是沙箱要用到的工作区能力，由 *files.Workspace 提供。
type Workspace interface {
	// Mountable 返回可以直接只读挂进沙箱的目录；工作区里的文件是加密存放的时候返回空，
	// 这时要用 Snapshot 把解开的副本放到别处。
	Mountable() string
	Snapshot(dst string, maxBytes int64) error
	WriteFile(rel string, data []byte) (string, error)
}

// Runner 在沙箱里跑代码。
type Runner struct {
	// Python 是沙箱里用的解释器，要在一个只读挂进去的独立环境里（如 /opt/atomhelper-sandbox/bin/python3）。
	Python string
	// Bwrap 是 bubblewrap 的路径，不写按 PATH 里的 bwrap。
	Bwrap string
	// Prlimit 用来限制内存，不写按 PATH 里的 prlimit。
	Prlimit string
	Timeout time.Duration
	// MemoryMB 是代码能用的内存上限。
	MemoryMB  int
	Workspace Workspace
	// now 测试时可以换掉。
	now func() time.Time
}

// Result 是一次运行的结果。
type Result struct {
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr,omitempty"`
	// Files 是代码写在 /out 里、已经收进工作区的文件。
	Files []string `json:"files,omitempty"`
	// Note 说明有什么没收下（太多、太大）。
	Note string `json:"note,omitempty"`
}

// Check 确认这台机器跑得了沙箱。
func (r *Runner) Check() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("%w: 只支持 Linux（这台是 %s）", ErrUnavailable, runtime.GOOS)
	}
	if _, err := exec.LookPath(r.bwrap()); err != nil {
		return fmt.Errorf("%w: 没有找到 bubblewrap（bwrap），请安装 bubblewrap", ErrUnavailable)
	}
	if _, err := exec.LookPath(r.prlimit()); err != nil {
		return fmt.Errorf("%w: 没有找到 prlimit（util-linux）", ErrUnavailable)
	}
	if !filepath.IsAbs(r.Python) {
		return fmt.Errorf("%w: sandbox.python 要写解释器的绝对路径", ErrUnavailable)
	}
	if _, err := os.Stat(r.Python); err != nil {
		return fmt.Errorf("%w: 解释器 %s 不存在，先运行 deploy/deploy.sh sandbox 安装", ErrUnavailable, r.Python)
	}
	return nil
}

func (r *Runner) bwrap() string   { return orElse(r.Bwrap, "bwrap") }
func (r *Runner) prlimit() string { return orElse(r.Prlimit, "prlimit") }

func orElse(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// envRoot 是解释器所在的独立环境的根目录（解释器路径往上两级：<root>/bin/python3）。
// 沙箱只在 Linux 上跑，这里按斜杠分隔的路径处理。
func (r *Runner) envRoot() string { return path.Dir(path.Dir(filepath.ToSlash(r.Python))) }

// args 拼出 bwrap 的参数。work 是只读挂到 /work 的目录，out 是挂到 /out 的可写目录，script 是代码文件。
func (r *Runner) args(work, out, script string) []string {
	mem := int64(r.MemoryMB) << 20
	cpu := int64(r.Timeout/time.Second) + 5
	a := []string{
		"--die-with-parent", "--new-session",
		// 网络、进程、IPC、主机名、用户都和外面隔开。
		"--unshare-all",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		// /etc 只给运行需要的几样；服务的密钥文件（/etc/atomhelper）不在其中。
		"--ro-bind-try", "/etc/ld.so.cache", "/etc/ld.so.cache",
		"--ro-bind-try", "/etc/localtime", "/etc/localtime",
		"--ro-bind-try", "/etc/fonts", "/etc/fonts",
		"--ro-bind-try", "/etc/mime.types", "/etc/mime.types",
		"--ro-bind", r.envRoot(), r.envRoot(),
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--ro-bind", work, "/work",
		"--bind", out, "/out",
		"--ro-bind", script, "/sandbox/main.py",
		"--chdir", "/out",
		"--clearenv",
		"--setenv", "HOME", "/tmp", "--setenv", "LANG", "C.UTF-8", "--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "MPLBACKEND", "Agg", "--setenv", "MPLCONFIGDIR", "/tmp",
		"--setenv", "OPENBLAS_NUM_THREADS", "1", "--setenv", "OMP_NUM_THREADS", "1",
		"--setenv", "PYTHONDONTWRITEBYTECODE", "1",
		"--",
		r.prlimit(), "--as=" + strconv.FormatInt(mem, 10), "--cpu=" + strconv.FormatInt(cpu, 10),
		"--fsize=" + strconv.Itoa(maxOutputBytes), "--nofile=256", "--core=0",
		r.Python, "-I", "/sandbox/main.py",
	}
	return a
}

// Run 跑一段 Python 代码。
func (r *Runner) Run(ctx context.Context, code string) (Result, error) {
	if len(code) > maxCodeBytes {
		return Result{}, fmt.Errorf("sandbox: 代码超过 %d KB", maxCodeBytes>>10)
	}
	if strings.TrimSpace(code) == "" {
		return Result{}, fmt.Errorf("sandbox: 没有代码")
	}
	if err := r.Check(); err != nil {
		return Result{}, err
	}
	dir, err := os.MkdirTemp("", "atom-sandbox-*")
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: 创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "out")
	script := filepath.Join(dir, "main.py")
	if err := os.Mkdir(out, 0o700); err != nil {
		return Result{}, fmt.Errorf("sandbox: 创建输出目录失败: %w", err)
	}
	if err := os.WriteFile(script, []byte(code), 0o600); err != nil {
		return Result{}, fmt.Errorf("sandbox: 写入代码失败: %w", err)
	}
	work := r.Workspace.Mountable()
	if work == "" {
		// 工作区是加密存放的：放一份解开的副本进去，跑完连同临时目录一起删掉。
		work = filepath.Join(dir, "work")
		if err := os.Mkdir(work, 0o700); err != nil {
			return Result{}, fmt.Errorf("sandbox: 创建工作区副本失败: %w", err)
		}
		if err := r.Workspace.Snapshot(work, maxOutputBytes*2); err != nil {
			return Result{}, err
		}
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, r.bwrap(), r.args(work, out, script)...)
	// bwrap 自己也不继承服务的环境变量：数据库连接串、保险库密钥都在里面。
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxStreamBytes, maxStreamBytes
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		res.TimedOut, res.ExitCode = true, -1
	case errors.As(runErr, &exit):
		res.ExitCode = exit.ExitCode()
	case runErr != nil:
		return Result{}, fmt.Errorf("sandbox: 启动沙箱失败: %w", runErr)
	}
	if err := r.collect(out, &res); err != nil {
		return Result{}, err
	}
	return res, nil
}

// collect 把代码写在 /out 里的文件收进工作区的「沙箱输出/<时间>」目录。
func (r *Runner) collect(out string, res *Result) error {
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	base := path.Join("沙箱输出", now().Local().Format("20060102-150405"))
	var total int64
	var skipped []string
	err := filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// 只收普通文件。符号链接可能指向沙箱外面（虽然那边看不到什么），一律不收。
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(out, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if len(res.Files) >= maxOutputFiles || total+info.Size() > maxOutputBytes {
			skipped = append(skipped, filepath.ToSlash(rel))
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		saved, err := r.Workspace.WriteFile(path.Join(base, filepath.ToSlash(rel)), data)
		if err != nil {
			skipped = append(skipped, filepath.ToSlash(rel)+"（"+err.Error()+"）")
			return nil
		}
		total += info.Size()
		res.Files = append(res.Files, saved)
		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox: 收取输出文件失败: %w", err)
	}
	if len(skipped) > 0 {
		res.Note = fmt.Sprintf("有 %d 个输出文件没有收下（每次最多 %d 个、共 %d MB）：%s",
			len(skipped), maxOutputFiles, maxOutputBytes>>20, strings.Join(skipped, "、"))
	}
	return nil
}

// limitedBuffer 只留前 max 个字节，后面的丢掉并注明。
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	s := strings.ToValidUTF8(b.buf.String(), "")
	if b.truncated {
		s += fmt.Sprintf("\n…（超过 %d KB，后面的省略了）", b.max>>10)
	}
	return s
}
