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
//
// 工作区里的文件可以加密存放（由用户在配置里打开）。打开之后写进工作区的文件落盘就是密文，
// 读它、把它发给用户的时候在内存里解开——明文不经过磁盘。
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
	"sync"
	"time"

	"github.com/xiguac/atomassistant/internal/seal"
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
	// box 用来解开加密存放的文件；encrypt 为真时新写入的文件也加密。
	box     *seal.Box
	encrypt bool

	// mu 保护下面两样：几个任务可能同时写文件、同时建分享链接。
	mu sync.Mutex
	// origins 是文件来源记录（见 origins.go），第一次用到时从磁盘读出来。
	origins map[string]originEntry
	// sums 缓存用户文件的内容摘要，免得每次建分享链接都把加密的文件全解一遍，见 share.go。
	sums map[string]cachedSum
}

// WithSeal 接入文件加密。
//
// 给了 box 就能读加密存放的文件；encrypt 为真时，之后写进工作区的文件落盘即加密。
// 两者分开，是因为加密可以中途打开或关掉：关掉之后，之前加密的文件仍然要读得出来；
// 打开之前放进来的明文文件也照常能读，下次被改写时才变成密文。
func (w *Workspace) WithSeal(box *seal.Box, encrypt bool) *Workspace {
	w.box, w.encrypt = box, encrypt
	return w
}

// content 读出一个文件的明文内容。加密存放的在内存里解开。
func (w *Workspace) content(abs string) ([]byte, error) {
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("files: 读取文件失败: %w", err)
	}
	if !seal.IsSealedFile(raw) {
		return raw, nil
	}
	if w.box == nil {
		return nil, fmt.Errorf("files: 这个文件是加密存放的，当前没有可用的数据密钥")
	}
	plain, err := w.box.OpenFile(raw)
	if err != nil {
		return nil, fmt.Errorf("files: 解密文件失败（数据密钥不对，或者文件已损坏）: %w", err)
	}
	return plain, nil
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
	// 以 . 开头的是留底目录和工作区自己的记录（谁放进来的哪个文件，见 origins.go），不让写。
	if forWrite && (norm == "" || hidden(norm)) {
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

// ReceivedDir 是用户发来的文件放的目录：聊天里发来的，和在管理后台上传的。
// 工作区里其余的都是助手生成的。
const ReceivedDir = "收到的文件"

// Receive 收下用户在聊天里发来的一个文件，放进「收到的文件」目录，返回它在工作区里的路径。
// 同名的不覆盖：后来的那个在名字后面加上序号。
func (w *Workspace) Receive(name string, data []byte) (string, error) {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		name = "文件" + strings.TrimLeft(name, ".")
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	rel := path.Join(ReceivedDir, name)
	for i := 2; ; i++ {
		abs, _, err := w.resolve(rel, true)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
			break
		}
		if i > 999 {
			return "", fmt.Errorf("%w: 同名文件太多", ErrInvalid)
		}
		rel = path.Join(ReceivedDir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
	saved, err := w.save(rel, byUser, func() ([]byte, error) { return data, nil })
	if err != nil {
		return "", err
	}
	return saved.Path, nil
}

// Delete 删掉工作区里的一个文件，或者一个空目录。
//
// 这是给用户在管理后台用的，助手没有删除文件的工具。删掉的不留底：用户亲手删的就是不要了。
// 目录只能删空的——一下子删掉一整棵目录树的操作不放在一个按钮上。留底目录里的旧版本不受影响。
func (w *Workspace) Delete(rel string) error {
	abs, norm, err := w.resolve(rel, true)
	if err != nil {
		return err
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, rel)
	}
	if err != nil {
		return fmt.Errorf("files: 读取文件信息失败: %w", err)
	}
	if info.IsDir() {
		items, err := os.ReadDir(abs)
		if err != nil {
			return fmt.Errorf("files: 读取目录失败: %w", err)
		}
		if len(items) > 0 {
			return fmt.Errorf("%w: 目录 %s 不是空的，请先删除其中的文件", ErrInvalid, norm)
		}
	}
	if err := os.Remove(abs); err != nil {
		return fmt.Errorf("files: 删除 %s 失败: %w", norm, err)
	}
	return nil
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

// Bytes 读出一个文件的原始内容，供下载、作为邮件附件或者发到聊天窗口。
// 加密存放的文件在这里解开：交到用户手上的是明文。
func (w *Workspace) Bytes(rel string) ([]byte, error) {
	abs, _, err := w.existing(rel)
	if err != nil {
		return nil, err
	}
	return w.content(abs)
}

// Saved 是一次写文件的结果。
type Saved struct {
	Path string `json:"path"`
	// Replaced 非空表示这次写入覆盖了一个已有的文件，旧的那份留在这个路径下。
	Replaced string `json:"previous_version,omitempty"`
}

// hidden 报告一个规范化的相对路径里有没有以 . 开头的一段。
func hidden(norm string) bool {
	for _, seg := range strings.Split(norm, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// save 把一个文件写进工作区。who 是谁放进来的，produce 给出文件的完整内容。
//
// 内容先在内存里备齐，开了加密就先加密，然后才落盘——明文不会出现在磁盘上。
// 先写临时文件再改名：写到一半出错或者进程被杀，原来的文件完好无损，
// 不会留下一个打不开的半截文件。目标已经存在时，旧的那份先挪去留底。
func (w *Workspace) save(rel string, who origin, produce func() ([]byte, error)) (Saved, error) {
	abs, norm, err := w.resolve(rel, true)
	if err != nil {
		return Saved{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return Saved{}, fmt.Errorf("files: 创建目录失败: %w", err)
	}
	data, err := produce()
	if err != nil {
		return Saved{}, err
	}
	plain := data
	if w.encrypt {
		if w.box == nil {
			return Saved{}, fmt.Errorf("files: 配置要求文件加密存放，但当前没有可用的数据密钥")
		}
		data = w.box.SealFile(data)
	}
	tmp := filepath.Join(filepath.Dir(abs), ".tmp-"+fmt.Sprint(time.Now().UnixNano()))
	defer func() { _ = os.Remove(tmp) }()
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return Saved{}, fmt.Errorf("files: 写入文件失败: %w", err)
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
	if err := w.record(norm, who, plain); err != nil {
		return Saved{}, err
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
	return w.save(rel, byUser, func() ([]byte, error) {
		raw, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("files: 读取 %s 失败: %w", src, err)
		}
		return raw, nil
	})
}
