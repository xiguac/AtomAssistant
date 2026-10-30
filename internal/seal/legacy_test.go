package seal

import (
	"crypto/rand"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyKey(t *testing.T) {
	dir := t.TempDir()
	oldPath, newPath := filepath.Join(dir, LegacyKeyFileName), filepath.Join(dir, KeyFileName)
	withMachine(t, map[string]string{"machine-id": "aaa", "board": "X1"})

	key := make([]byte, KeySize)
	_, _ = rand.Read(key)
	if err := writeKey(oldPath, legacyWrapLabel, key); err != nil {
		t.Fatal(err)
	}
	legacy, err := newBox(key, legacySealLabel)
	if err != nil {
		t.Fatal(err)
	}
	token := legacy.Seal("tasks.goal", "旧数据")
	if _, _, err := LoadKey(oldPath, false); err == nil {
		t.Fatal("新标签不应当解得开旧密钥文件")
	}

	old, cur, err := MigrateLegacyKey(oldPath, newPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := old.Open("tasks.goal", token); err != nil || got != "旧数据" {
		t.Fatalf("旧 Box 应当解得开旧密文: %q, %v", got, err)
	}
	if _, err := cur.Open("tasks.goal", token); err == nil {
		t.Fatal("新 Box 不应当解得开旧密文")
	}
	loaded, _, err := LoadKey(newPath, false)
	if err != nil || loaded.KeyID() != cur.KeyID() || old.KeyID() == cur.KeyID() {
		t.Fatalf("新密钥文件应当对应新 Box: err=%v", err)
	}

	// 迁移到一半重跑：新密钥文件已在，核对后照常返回。
	if _, again, err := MigrateLegacyKey(oldPath, newPath); err != nil || again.KeyID() != cur.KeyID() {
		t.Fatalf("重跑应当得到同一把密钥: %v", err)
	}
	if err := RemoveLegacyKey(oldPath); err != nil {
		t.Fatal(err)
	}
}
