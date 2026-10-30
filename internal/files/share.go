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

	"github.com/xiguac/atomassistant/internal/seal"
)

// 对外分享：助手可以给工作区里的一个文件或一个目录建临时下载链接（见 internal/share）。
// 这里管的是"哪些文件能分享"，规则只有一条——**只有助手生成的文件能对外分享**：
//
//   - 用户放进来的文件（聊天里发来的、后台上传的、atomctl file add 放进来的、直接拷进来的）不行。
//     用户批准之后，可以由 CopyReceived 复制出一份副本，副本可以分享。
//   - 助手生成的文件，内容和某个用户文件一字不差的也不行：那是原样转手（比如在沙箱里复制了一份），
//     等于绕开了上面那条。
//   - 建链接时记下每个文件的内容摘要，下载时内容必须还是那样：链接分享的是用户看到的那一版，
//     之后被换掉的内容不会经这个链接出去。

const (
	// CopiesDir 是用户批准对外分享的副本放的目录。
	CopiesDir = "分享副本"
	// MaxShareFiles 与 MaxShareBytes 限制一个目录链接里能有多少文件。
	MaxShareFiles = 100
	MaxShareBytes = 200 << 20
)

// Pinned 是分享出去的一个文件：路径和建链接时的内容摘要。
type Pinned struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// cachedSum 是一个文件的内容摘要，连同算它时文件的大小和修改时间：两者都没变才沿用。
type cachedSum struct {
	size    int64
	modTime time.Time
	plain   int64
	sha     string
}

// Shareable 检查一个文件或目录能不能对外分享，能的话给出要分享的文件。
//
// norm 是规范化之后的路径；dir 为真表示分享的是目录，pinned 是其中全部的文件（不含以 . 开头的）。
// 有任何一个文件不能分享就整个拒绝，并说明是哪些：悄悄少分享几个，比报错更让人摸不着头脑。
func (w *Workspace) Shareable(rel string) (norm string, dir bool, pinned []Pinned, err error) {
	abs, norm, err := w.resolve(rel, false)
	if err != nil {
		return "", false, nil, err
	}
	if norm == "" {
		return "", false, nil, fmt.Errorf("%w: 不能分享整个工作区，请指定一个文件或一个目录", ErrInvalid)
	}
	if hidden(norm) {
		return "", false, nil, fmt.Errorf("%w: 不能分享以 . 开头的文件或目录", ErrInvalid)
	}
	if norm == ReceivedDir || strings.HasPrefix(norm, ReceivedDir+"/") {
		return "", false, nil, fmt.Errorf("%w: 「%s」里是用户发来的文件，不能对外分享；用户同意后可以先复制一份副本再分享副本",
			ErrInvalid, ReceivedDir)
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil, fmt.Errorf("%w: %s", ErrNotFound, rel)
	}
	if err != nil {
		return "", false, nil, fmt.Errorf("files: 读取文件信息失败: %w", err)
	}
	var paths []string
	switch {
	case info.Mode().IsRegular():
		paths = []string{norm}
	case info.IsDir():
		dir = true
		if paths, err = w.walkShared(abs, norm); err != nil {
			return "", false, nil, err
		}
		if len(paths) == 0 {
			return "", false, nil, fmt.Errorf("%w: 目录 %s 里没有文件", ErrInvalid, norm)
		}
	default:
		return "", false, nil, fmt.Errorf("%w: %s 不是普通文件", ErrInvalid, norm)
	}

	var total int64
	var refused []string
	for _, p := range paths {
		data, err := w.plainOf(filepath.Join(w.Root, filepath.FromSlash(p)))
		if err != nil {
			return "", false, nil, err
		}
		if total += int64(len(data)); total > MaxShareBytes {
			return "", false, nil, fmt.Errorf("%w: 要分享的文件加起来超过 %d MB", ErrUnsupported, MaxShareBytes>>20)
		}
		sha := digest(data)
		by, known, err := w.originOf(p, sha)
		if err != nil {
			return "", false, nil, err
		}
		if !known || by == byUser {
			refused = append(refused, p+"（不是助手生成的）")
			continue
		}
		pinned = append(pinned, Pinned{Path: p, Size: int64(len(data)), SHA256: sha})
	}
	// 原样转手的：内容和某个用户文件一样。用户批准过的副本除外。
	if len(refused) == 0 {
		same, err := w.matchUserFiles(pinned)
		if err != nil {
			return "", false, nil, err
		}
		for _, p := range pinned {
			if src, hit := same[p.SHA256]; hit {
				if by, _, _ := w.originOf(p.Path, p.SHA256); by != byCopy {
					refused = append(refused, fmt.Sprintf("%s（内容和用户的文件 %s 一样）", p.Path, src))
				}
			}
		}
	}
	if len(refused) > 0 {
		return "", false, nil, fmt.Errorf("%w: 这些文件不能对外分享：%s。只有助手生成的文件能分享；用户的文件要经用户同意复制成副本后再分享",
			ErrInvalid, strings.Join(refused, "、"))
	}
	return norm, dir, pinned, nil
}

// walkShared 列出要分享的目录里的全部文件，返回相对工作区的路径。
// 不顺着符号链接走；遇到符号链接等非普通文件直接拒绝。以 . 开头的跳过。
func (w *Workspace) walkShared(abs, norm string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("files: 读取目录失败: %w", err)
		}
		if p != abs && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return fmt.Errorf("files: 读取目录失败: %w", err)
		}
		full := path.Join(norm, filepath.ToSlash(rel))
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s 不是普通文件（可能是符号链接），这个目录不能分享", ErrInvalid, full)
		}
		if len(out) >= MaxShareFiles {
			return fmt.Errorf("%w: 目录里的文件超过 %d 个，请分享一个更小的目录", ErrUnsupported, MaxShareFiles)
		}
		out = append(out, full)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// plainOf 读出一个文件的明文。不是普通文件（如符号链接）的、超过大小上限的，拒绝。
func (w *Workspace) plainOf(abs string) ([]byte, error) {
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("files: 读取文件信息失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: 不是普通文件", ErrInvalid)
	}
	if info.Size() > MaxFileBytes+(1<<20) {
		return nil, fmt.Errorf("%w: 文件超过 %d MB 的上限", ErrUnsupported, MaxFileBytes>>20)
	}
	return w.content(abs)
}

// matchUserFiles 在工作区里找内容和 pinned 中某个文件一样的用户文件，返回 摘要 -> 那个用户文件的路径。
//
// "用户文件"是除了经核实由助手生成（或批准复制）之外的全部文件：没有来源记录的也算——
// 用户直接拷进工作区的文件不经过任何记录。留底目录和以 . 开头的不看。
// 明文存放的文件先比大小，大小对不上的不必读；加密存放的要解开才知道，算过的摘要按大小和修改时间缓存。
func (w *Workspace) matchUserFiles(pinned []Pinned) (map[string]string, error) {
	want := map[string]bool{}
	sizes := map[int64]bool{}
	for _, p := range pinned {
		want[p.SHA256] = true
		sizes[p.Size] = true
	}
	found := map[string]string{}
	err := filepath.WalkDir(w.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("files: 读取工作区失败: %w", err)
		}
		if p != w.Root && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(w.Root, p)
		if err != nil {
			return fmt.Errorf("files: 读取工作区失败: %w", err)
		}
		norm := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("files: 读取文件信息失败: %w", err)
		}
		sha, ok, err := w.sumIfSized(p, norm, info, sizes)
		if err != nil || !ok || !want[sha] {
			return err
		}
		if by, known, err := w.originOf(norm, sha); err != nil {
			return err
		} else if known && by != byUser {
			return nil
		}
		if _, dup := found[sha]; !dup {
			found[sha] = norm
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// sumIfSized 算一个文件明文的摘要，明文大小不在 sizes 里时返回 false。
func (w *Workspace) sumIfSized(abs, norm string, info fs.FileInfo, sizes map[int64]bool) (string, bool, error) {
	w.mu.Lock()
	c, hit := w.sums[norm]
	w.mu.Unlock()
	if hit && c.size == info.Size() && c.modTime.Equal(info.ModTime()) {
		return c.sha, sizes[c.plain], nil
	}
	// 明文存放的，磁盘上的大小就是明文大小：对不上就不必读。
	head := make([]byte, 64)
	f, err := os.Open(abs)
	if err != nil {
		return "", false, fmt.Errorf("files: 读取文件失败: %w", err)
	}
	n, _ := f.Read(head)
	_ = f.Close()
	if !seal.IsSealedFile(head[:n]) && !sizes[info.Size()] {
		return "", false, nil
	}
	data, err := w.plainOf(abs)
	if err != nil {
		return "", false, err
	}
	sha := digest(data)
	w.mu.Lock()
	if w.sums == nil {
		w.sums = map[string]cachedSum{}
	}
	w.sums[norm] = cachedSum{size: info.Size(), modTime: info.ModTime(), plain: int64(len(data)), sha: sha}
	w.mu.Unlock()
	return sha, sizes[int64(len(data))], nil
}

// SharedBytes 读出分享出去的一个文件，供下载。
//
// 文件要还在原处、还是普通文件、内容还和建链接时一样；任何一条不满足都拒绝。
func (w *Workspace) SharedBytes(p Pinned) ([]byte, error) {
	abs, norm, err := w.resolve(p.Path, false)
	if err != nil {
		return nil, err
	}
	if norm == "" || norm != p.Path {
		return nil, fmt.Errorf("%w: 路径不对", ErrInvalid)
	}
	data, err := w.plainOf(abs)
	if err != nil {
		return nil, err
	}
	if digest(data) != p.SHA256 {
		return nil, fmt.Errorf("%w: %s 的内容在建链接之后变了", ErrInvalid, norm)
	}
	return data, nil
}

// CopyReceived 把用户的一个文件复制到「分享副本」目录，副本可以对外分享。返回副本的路径。
//
// **只能在用户批准之后调用**：审批由调用方（助手的审批器）负责，这里不做判断。
// 来源必须是用户的文件——助手生成的文件本来就能分享，不需要副本。同名的不覆盖，加序号。
func (w *Workspace) CopyReceived(rel string) (string, error) {
	abs, norm, err := w.existing(rel)
	if err != nil {
		return "", err
	}
	if hidden(norm) {
		return "", fmt.Errorf("%w: 不能复制以 . 开头的文件", ErrInvalid)
	}
	data, err := w.plainOf(abs)
	if err != nil {
		return "", err
	}
	if by, known, err := w.originOf(norm, digest(data)); err != nil {
		return "", err
	} else if known && by != byUser {
		return "", fmt.Errorf("%w: %s 不是用户的文件，不需要复制，可以直接分享", ErrInvalid, norm)
	}
	name := path.Base(norm)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	dst := path.Join(CopiesDir, name)
	for i := 2; ; i++ {
		dstAbs, _, err := w.resolve(dst, true)
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(dstAbs); errors.Is(err, fs.ErrNotExist) {
			break
		}
		if i > 999 {
			return "", fmt.Errorf("%w: 同名文件太多", ErrInvalid)
		}
		dst = path.Join(CopiesDir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
	saved, err := w.save(dst, byCopy, func() ([]byte, error) { return data, nil })
	if err != nil {
		return "", err
	}
	return saved.Path, nil
}
