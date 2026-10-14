package files

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiguac/atomhelper/internal/seal"
)

// 沙箱要用到的几样：见 internal/sandbox。

// Mountable 返回可以直接只读挂进沙箱的目录。
//
// 工作区里有加密存放的文件时返回空：挂进去的只会是密文，要改用 Snapshot 放一份解开的副本。
// 为了不必每次都翻一遍所有文件，只要配置了加密、或者手里有数据密钥而根目录下找到过密文，就按加密的办。
func (w *Workspace) Mountable() string {
	if w.encrypt {
		return ""
	}
	if w.box != nil && w.hasSealed() {
		return ""
	}
	return w.Root
}

// hasSealed 报告工作区里有没有加密存放的文件。
func (w *Workspace) hasSealed() bool {
	found := false
	_ = filepath.WalkDir(w.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		head := make([]byte, 64)
		n, _ := f.Read(head)
		_ = f.Close()
		if seal.IsSealedFile(head[:n]) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// Snapshot 把工作区里的文件解开后复制到 dst，供沙箱只读使用。留底目录、隐藏文件不复制。
// 总量超过 maxBytes 时报错：沙箱不该为了跑一段代码把整个磁盘复制一遍。
func (w *Workspace) Snapshot(dst string, maxBytes int64) error {
	var total int64
	return filepath.WalkDir(w.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if p != w.Root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$")) {
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
			return err
		}
		data, err := w.content(p)
		if err != nil {
			return err
		}
		if total += int64(len(data)); total > maxBytes {
			return fmt.Errorf("%w: 工作区超过 %d MB，放不进沙箱", ErrUnsupported, maxBytes>>20)
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("files: 创建目录失败: %w", err)
		}
		if err := os.WriteFile(target, data, 0o400); err != nil {
			return fmt.Errorf("files: 写入副本失败: %w", err)
		}
		return nil
	})
}

// WriteFile 把一份内容写进工作区，返回它的路径。已有同名文件时旧的留底。
func (w *Workspace) WriteFile(rel string, data []byte) (string, error) {
	saved, err := w.save(rel, func() ([]byte, error) { return data, nil })
	if err != nil {
		return "", err
	}
	return saved.Path, nil
}
