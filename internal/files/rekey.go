package files

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/xiguac/atomassistant/internal/seal"
)

// RekeyFiles 把工作区里用 old 加密存放的文件改用 cur 重新加密，连同 .versions 里留底的旧文件。
//
// 只给改名迁移用（见 seal.MigrateLegacyKey），服务器迁移完成后删掉。
// 已经是 cur 加密的、明文存放的都跳过，所以中途失败可以重跑。返回重新加密了几个文件。
// 每个文件先写临时文件再改名，写到一半出错原来的文件完好无损。
func (w *Workspace) RekeyFiles(old, cur *seal.Box) (int, error) {
	n := 0
	err := filepath.WalkDir(w.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("files: 读取 %s 失败: %w", p, err)
		}
		if !seal.IsSealedFile(raw) {
			return nil
		}
		if _, err := cur.OpenFile(raw); err == nil {
			return nil
		}
		plain, err := old.OpenFile(raw)
		if err != nil {
			return fmt.Errorf("files: 用旧密钥解不开 %s: %w", p, err)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("files: 读取 %s 失败: %w", p, err)
		}
		tmp := filepath.Join(filepath.Dir(p), ".tmp-"+fmt.Sprint(time.Now().UnixNano()))
		if err := os.WriteFile(tmp, cur.SealFile(plain), info.Mode().Perm()); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("files: 写入 %s 失败: %w", p, err)
		}
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("files: 替换 %s 失败: %w", p, err)
		}
		n++
		return nil
	})
	return n, err
}
