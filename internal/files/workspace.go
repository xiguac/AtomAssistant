// Package files 让助手能读写常见的办公文件：Word、Excel、PowerPoint、CSV 和纯文本。
//
// 全部在本机完成，用的是开放的文件格式和纯 Go 的实现，不调用任何在线文档服务——
// 助手部署在哪里、连不连得上外网，都不影响它处理文件。
//
// 助手只能碰一个目录（工作区）里的东西。用户把要处理的文件放进来，助手生成的文件也落在这里。
// 三条约束：
//
//  1. **出不了工作区。** 路径一律相对工作区解析，带 .. 的、绝对的、经符号链接绕出去的，全部拒绝。
//  2. **不会悄悄覆盖。** 写一个已经存在的文件时，旧的那份先挪进 .versions 留底，随时能找回。
//  3. **文件内容不是指令。** 文件是别人写的也好、用户自己写的也好，读出来的文字都只是材料。
package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	// ErrInvalid 表示参数不合法。
	ErrInvalid = errors.New("files: 参数不合法")
	// ErrNotFound 表示没有这个文件。
	ErrNotFound = errors.New("files: 没有这个文件")
	// ErrUnsupported 表示这种文件目前处理不了。
	ErrUnsupported = errors.New("files: 暂不支持")
)

const (
	// versionsDir 是存放被覆盖的旧文件的目录。
	versionsDir = ".versions"
	// MaxFileBytes 是能读写的单个文件的大小上限。
	MaxFileBytes = 50 << 20
	// maxListEntries 是一次列目录最多返回的条目数。
	maxListEntries = 500
)

// Workspace 是助手可以读写的那个目录。
type Workspace struct {
	Root string
}

// Open 打开（必要时创建）一个工作区。
func Open(root string) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: 没有指定工作区目录", ErrInvalid)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("files: 工作区路径无法解析: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("files: 创建工作区失败: %w", err)
	}
	// 把工作区自己的符号链接解开，之后判断"是否还在工作区内"才有可比的基准。
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("files: 工作区路径无法解析: %w", err)
	}
	return &Workspace{Root: real}, nil
}

// clean 校验并规范化一个相对路径，返回用 / 分隔的形式。
func clean(rel string) (string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, `\`, "/"))
	if rel == "" || rel == "." {
		return "", nil
	}
	if strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) || strings.Contains(rel, ":") {
		return "", fmt.Errorf("%w: 路径要相对工作区来写，不能是绝对路径", ErrInvalid)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: 路径不能跳出工作区", ErrInvalid)
		}
	}
	for _, r := range rel {
		if r < 0x20 {
			return "", fmt.Errorf("%w: 路径里有控制字符", ErrInvalid)
		}
	}
	return path.Clean(rel), nil
}

// resolve 把相对路径解析成磁盘上的绝对路径，并确认它没有绕出工作区。
// forWrite 为真时不允许落在留底目录里。
func (w *Workspace) resolve(rel string, forWrite bool) (abs, norm string, err error) {
	norm, err = clean(rel)
	if err != nil {
		return "", "", err
	}
	if forWrite && (norm == "" || norm == versionsDir || strings.HasPrefix(norm, versionsDir+"/")) {
		return "", "", fmt.Errorf("%w: 不能写到这个位置", ErrInvalid)
	}
	abs = filepath.Join(w.Root, filepath.FromSlash(norm))
	// 沿着已经存在的那一段把符号链接解开：工作区里若有一个指向外面的链接，
	// 光看路径的字面是看不出来的。
	probe := abs
	for {
		real, evalErr := filepath.EvalSymlinks(probe)
		if evalErr == nil {
			if real != w.Root && !strings.HasPrefix(real, w.Root+string(filepath.Separator)) {
				return "", "", fmt.Errorf("%w: 路径经符号链接指到了工作区之外", ErrInvalid)
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", "", fmt.Errorf("%w: 路径无法解析", ErrInvalid)
		}
		probe = parent
	}
	return abs, norm, nil
}

// Entry 是工作区里的一个文件或目录。
type Entry struct {
	Path     string    `json:"path"`
	Dir      bool      `json:"dir,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Modified time.Time `json:"modified"`
}

// List 列出一个目录下的内容，不递归。dir 为空表示工作区根目录。
func (w *Workspace) List(dir string) ([]Entry, error) {
	abs, norm, err := w.resolve(dir, false)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, dir)
	}
	if err != nil {
		return nil, fmt.Errorf("files: 读取目录失败: %w", err)
	}
	out := []Entry{}
	for _, it := range items {
		// 留底目录和隐藏文件不给助手看：前者是给用户找回旧版本用的，后者多半是系统产生的。
		if strings.HasPrefix(it.Name(), ".") || strings.HasPrefix(it.Name(), "~$") {
			continue
		}
		info, err := it.Info()
		if err != nil {
			continue
		}
		e := Entry{Path: path.Join(norm, it.Name()), Dir: it.IsDir(), Modified: info.ModTime()}
		if !e.Dir {
			e.Size = info.Size()
		}
		out = append(out, e)
		if len(out) >= maxListEntries {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// existing 解析一个必须已经存在的文件，并检查大小。
func (w *Workspace) existing(rel string) (abs, norm string, err error) {
	abs, norm, err = w.resolve(rel, false)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) || norm == "" {
		return "", "", fmt.Errorf("%w: %s", ErrNotFound, rel)
	}
	if err != nil {
		return "", "", fmt.Errorf("files: 读取文件信息失败: %w", err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("%w: %s 是一个目录", ErrInvalid, rel)
	}
	if info.Size() > MaxFileBytes {
		return "", "", fmt.Errorf("%w: %s 有 %d MB，超过了 %d MB 的上限", ErrUnsupported, rel, info.Size()>>20, MaxFileBytes>>20)
	}
	return abs, norm, nil
}

// Bytes 读出一个文件的原始内容，供作为邮件附件发出。
func (w *Workspace) Bytes(rel string) ([]byte, error) {
	abs, _, err := w.existing(rel)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("files: 读取文件失败: %w", err)
	}
	return raw, nil
}

// Saved 是一次写文件的结果。
type Saved struct {
	Path string `json:"path"`
	// Replaced 非空表示这次写入覆盖了一个已有的文件，旧的那份留在这个路径下。
	Replaced string `json:"previous_version,omitempty"`
}

// save 把一个文件写进工作区。write 负责把内容写到给它的临时路径上。
//
// 先写临时文件再改名：写到一半出错或者进程被杀，原来的文件完好无损，
// 不会留下一个打不开的半截文件。目标已经存在时，旧的那份先挪去留底。
func (w *Workspace) save(rel string, write func(tmp string) error) (Saved, error) {
	abs, norm, err := w.resolve(rel, true)
	if err != nil {
		return Saved{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return Saved{}, fmt.Errorf("files: 创建目录失败: %w", err)
	}
	// 临时文件保留原来的扩展名：有的库按扩展名决定写成什么格式。
	tmp := filepath.Join(filepath.Dir(abs), ".tmp-"+fmt.Sprint(time.Now().UnixNano())+filepath.Ext(abs))
	defer func() { _ = os.Remove(tmp) }()
	if err := write(tmp); err != nil {
		return Saved{}, err
	}
	out := Saved{Path: norm}
	if info, err := os.Stat(abs); err == nil {
		if info.IsDir() {
			return Saved{}, fmt.Errorf("%w: %s 是一个目录", ErrInvalid, rel)
		}
		keep := path.Join(versionsDir, time.Now().Format("20060102-150405.000")+"-"+strings.ReplaceAll(norm, "/", "__"))
		keepAbs := filepath.Join(w.Root, filepath.FromSlash(keep))
		if err := os.MkdirAll(filepath.Dir(keepAbs), 0o700); err != nil {
			return Saved{}, fmt.Errorf("files: 创建留底目录失败: %w", err)
		}
		if err := os.Rename(abs, keepAbs); err != nil {
			return Saved{}, fmt.Errorf("files: 给旧文件留底失败（文件是不是正被别的程序打开着？）: %w", err)
		}
		out.Replaced = keep
	}
	if err := os.Rename(tmp, abs); err != nil {
		return Saved{}, fmt.Errorf("files: 写入文件失败: %w", err)
	}
	return out, nil
}

// ext 返回小写的扩展名，不带点。
func ext(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
}

// Import 把工作区之外的一个文件复制进来。这是用户的操作，只由运维 CLI 调用，不是助手的工具。
func (w *Workspace) Import(src, rel string) (Saved, error) {
	info, err := os.Stat(src)
	if err != nil {
		return Saved{}, fmt.Errorf("%w: %s", ErrNotFound, src)
	}
	if info.IsDir() {
		return Saved{}, fmt.Errorf("%w: %s 是一个目录，一次放一个文件", ErrInvalid, src)
	}
	if info.Size() > MaxFileBytes {
		return Saved{}, fmt.Errorf("%w: 文件有 %d MB，超过了 %d MB 的上限", ErrUnsupported, info.Size()>>20, MaxFileBytes>>20)
	}
	if strings.TrimSpace(rel) == "" {
		rel = filepath.Base(src)
	}
	return w.save(rel, func(tmp string) error {
		raw, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("files: 读取 %s 失败: %w", src, err)
		}
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			return fmt.Errorf("files: 写入文件失败: %w", err)
		}
		return nil
	})
}
