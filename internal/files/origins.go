package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// 工作区记着每个文件是谁放进来的：用户发来的、上传的，还是助手生成的。
//
// 对外分享（见 share.go）只许分享助手生成的文件。光看文件在哪个目录不够：用户可以用 atomctl file add
// 把文件放到任意位置，也可以直接拷进工作区。所以按来源记，而且记的是内容的摘要——
// 同一个路径后来被换成了别的内容，摘要对不上，就不再算助手生成的。

// origin 是一个文件是谁放进工作区的。
type origin string

const (
	// byUser 是用户放进来的：聊天里发来的、后台上传的、atomctl file add 放进来的。
	byUser origin = "user"
	// byAssistant 是助手生成的：写文档、表格、演示文稿，沙箱跑出来的。
	byAssistant origin = "assistant"
	// byCopy 是用户批准后，从用户的文件复制出来、允许对外分享的副本。
	byCopy origin = "copy"
)

// originsFile 是来源记录在工作区里的位置。以 . 开头：不列给助手看，助手的工具也写不了它。
const originsFile = ".origins.json"

// originEntry 是一个文件的来源记录。
type originEntry struct {
	By     origin `json:"by"`
	SHA256 string `json:"sha256"`
}

// digest 返回一段内容的 SHA-256，十六进制。
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// loadOrigins 读出来源记录。调用方持有 w.mu。
func (w *Workspace) loadOrigins() (map[string]originEntry, error) {
	if w.origins != nil {
		return w.origins, nil
	}
	m := map[string]originEntry{}
	raw, err := os.ReadFile(filepath.Join(w.Root, originsFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("files: 读取文件来源记录失败: %w", err)
	default:
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("files: 文件来源记录已损坏（%s）: %w", originsFile, err)
		}
	}
	w.origins = m
	return m, nil
}

// record 记下一个刚写进工作区的文件是谁放进来的。plain 是它的明文内容。
//
// 先写临时文件再改名，和写普通文件一样：进程中途被杀不会留下半截的记录。
func (w *Workspace) record(norm string, who origin, plain []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	m, err := w.loadOrigins()
	if err != nil {
		return err
	}
	m[norm] = originEntry{By: who, SHA256: digest(plain)}
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("files: 保存文件来源记录失败: %w", err)
	}
	tmp := filepath.Join(w.Root, ".tmp-origins-"+fmt.Sprint(time.Now().UnixNano()))
	defer func() { _ = os.Remove(tmp) }()
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("files: 保存文件来源记录失败: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(w.Root, originsFile)); err != nil {
		return fmt.Errorf("files: 保存文件来源记录失败: %w", err)
	}
	return nil
}

// originOf 返回一个文件的来源记录。内容摘要和记录对不上的、没有记录的，返回 false。
func (w *Workspace) originOf(norm, sha string) (origin, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	m, err := w.loadOrigins()
	if err != nil {
		return "", false, err
	}
	e, ok := m[norm]
	if !ok || e.SHA256 != sha {
		return "", false, nil
	}
	return e.By, true, nil
}
