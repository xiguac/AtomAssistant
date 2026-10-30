package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// rekeyLegacy 把改名之前加密的数据换成新密文：密钥文件、数据库的内容列、工作区里加密存放的文件。
//
// 只在服务器改名迁移时由 deploy/migrate-name.sh 调用一次，迁移完成后连同 seal、pg、files 里的对应代码一起删掉。
// 要以服务账号运行：新的密钥文件和重写的文件要归它所有。中途失败可以重跑，换过的部分会跳过。
func rekeyLegacy(cfg *config.Config) error {
	oldPath := filepath.Join(filepath.Dir(cfg.KeyFile), seal.LegacyKeyFileName)
	if _, err := os.Stat(oldPath); errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("没有旧密钥文件 %s，不需要迁移。\n", oldPath)
		return nil
	}
	old, cur, err := seal.MigrateLegacyKey(oldPath, cfg.KeyFile)
	if err != nil {
		return err
	}
	fmt.Printf("密钥文件已换成 %s（数据密钥不变，之前的密钥备份仍然有效）。\n", cfg.KeyFile)

	// 库大的时候重新加密要一会儿，不用 run 里那 15 秒的时限。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	st, err := pg.Open(ctx, cfg.DSN, cur)
	if err != nil {
		return err
	}
	defer st.Close()
	changed, err := st.RekeyFrom(ctx, old)
	if err != nil {
		return err
	}
	if changed {
		fmt.Println("数据库里的内容已重新加密。")
	} else {
		fmt.Println("数据库里的内容之前已经换过，跳过。")
	}

	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		return err
	}
	if file != nil {
		ws, err := file.Workspace(cur)
		if err != nil {
			return err
		}
		if ws != nil {
			n, err := ws.RekeyFiles(old, cur)
			if err != nil {
				return err
			}
			fmt.Printf("工作区 %s 里重新加密了 %d 个文件。\n", ws.Root, n)
		}
	}

	if err := seal.RemoveLegacyKey(oldPath); err != nil {
		return err
	}
	fmt.Printf("已删除旧密钥文件 %s。\n", oldPath)
	return nil
}
