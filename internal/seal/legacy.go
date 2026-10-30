package seal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
)

// 改名之前用的标签和密钥文件名。
//
// 程序改名后，派生子密钥的标签也跟着改了，所以库里、工作区里的密文要用旧标签解开、再用新标签加密一遍。
// 这里只给 atomctl rekey-legacy 用，服务器迁移完成后连同那条命令一起删掉。
const (
	LegacyKeyFileName = "atomhelper.key"
	legacyWrapLabel   = "atomhelper key wrap v1"
	legacySealLabel   = "atomhelper seal v1 "
)

// MigrateLegacyKey 读出旧密钥文件里的数据密钥，用新的标签包好写到 newPath。
//
// 数据密钥本身不变，所以之前用 atomctl key backup 留的备份仍然有效。
// newPath 已经存在（上次迁移到一半）时不覆盖，只核对它里面是同一把数据密钥。
// 返回按旧标签和按新标签各自构造的 Box：前者解开旧密文，后者加密成新密文。
func MigrateLegacyKey(oldPath, newPath string) (old, cur *Box, err error) {
	key, err := readKey(oldPath, legacyWrapLabel)
	if err != nil {
		return nil, nil, err
	}
	have, err := readKey(newPath, wrapLabel)
	switch {
	case errors.Is(err, ErrKeyMissing):
		if err := writeKey(newPath, wrapLabel, key); err != nil {
			return nil, nil, err
		}
		if have, err = readKey(newPath, wrapLabel); err != nil {
			return nil, nil, fmt.Errorf("seal: 新密钥文件写好后读不回来: %w", err)
		}
	case err != nil:
		return nil, nil, err
	}
	if !bytes.Equal(have, key) {
		return nil, nil, fmt.Errorf("seal: %s 里的数据密钥与 %s 的不是同一把，不能继续迁移", newPath, oldPath)
	}
	if old, err = newBox(key, legacySealLabel); err != nil {
		return nil, nil, err
	}
	if cur, err = NewBox(key); err != nil {
		return nil, nil, err
	}
	return old, cur, nil
}

// RemoveLegacyKey 在数据全部换成新密文之后删掉旧密钥文件。
func RemoveLegacyKey(oldPath string) error {
	if err := os.Remove(oldPath); err != nil {
		return fmt.Errorf("seal: 删除旧密钥文件失败: %w", err)
	}
	return nil
}
