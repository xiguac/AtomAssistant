package seal

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func testBox(t *testing.T) *Box {
	t.Helper()
	b, err := NewBox(bytes.Repeat([]byte{7}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSeal_往返与随机性(t *testing.T) {
	b := testBox(t)
	a1, a2 := b.Seal("tasks.goal", "订周五的票"), b.Seal("tasks.goal", "订周五的票")
	if a1 == a2 {
		t.Fatal("同样的内容两次加密结果相同，Seal 应当每次不同")
	}
	if strings.Contains(a1, "订") {
		t.Fatal("密文里出现了明文")
	}
	got, err := b.Open("tasks.goal", a1)
	if err != nil || got != "订周五的票" {
		t.Fatalf("解密结果 %q, err=%v", got, err)
	}
	if b.Seal("x", "") != "" {
		t.Fatal("空串应当原样返回")
	}
}

func TestSealEq_同值同密文(t *testing.T) {
	b := testBox(t)
	a1, a2 := b.SealEq("memories.subject", "张总"), b.SealEq("memories.subject", "张总")
	if a1 != a2 {
		t.Fatal("SealEq 对同样的内容应当给出同样的结果")
	}
	if b.SealEq("memories.subject", "李总") == a1 {
		t.Fatal("不同的内容得到了同样的密文")
	}
	if b.SealEq("memory_pending.subject", "张总") == a1 {
		t.Fatal("不同位置上的同一个值不应得到同样的密文")
	}
	got, err := b.Open("memories.subject", a1)
	if err != nil || got != "张总" {
		t.Fatalf("解密结果 %q, err=%v", got, err)
	}
}

func TestOpen_挪位置与改动都解不开(t *testing.T) {
	b := testBox(t)
	token := b.Seal("tasks.goal", "内容")
	if _, err := b.Open("tasks.title", token); !errors.Is(err, ErrOpen) {
		t.Fatalf("把密文挪到另一列应当解不开，实际 err=%v", err)
	}
	if _, err := b.Open("tasks.goal", token[:len(token)-2]+"AA"); !errors.Is(err, ErrOpen) {
		t.Fatalf("改动过的密文应当解不开，实际 err=%v", err)
	}
	if _, err := b.Open("tasks.goal", "明文"); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("明文应当被认出来，实际 err=%v", err)
	}
	other, _ := NewBox(bytes.Repeat([]byte{9}, KeySize))
	if _, err := other.Open("tasks.goal", token); !errors.Is(err, ErrOpen) {
		t.Fatalf("换一把密钥应当解不开，实际 err=%v", err)
	}
	if other.KeyID() == b.KeyID() {
		t.Fatal("不同的密钥指纹相同")
	}
}

func TestSealFile_往返(t *testing.T) {
	b := testBox(t)
	plain := []byte("PK\x03\x04 一个文件")
	sealed := b.SealFile(plain)
	if !IsSealedFile(sealed) || IsSealedFile(plain) {
		t.Fatal("加密文件的识别不对")
	}
	got, err := b.OpenFile(sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("解密结果不对, err=%v", err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := b.OpenFile(sealed); !errors.Is(err, ErrOpen) {
		t.Fatalf("改动过的文件应当解不开，实际 err=%v", err)
	}
}

func TestPassphrase_往返(t *testing.T) {
	sealed, err := SealWithPassphrase("correct horse 电池", []byte("全部数据"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenWithPassphrase("correct horse 电池", sealed)
	if err != nil || string(got) != "全部数据" {
		t.Fatalf("解密结果 %q, err=%v", got, err)
	}
	if _, err := OpenWithPassphrase("wrong passphrase", sealed); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("口令不对应当报 ErrPassphrase，实际 %v", err)
	}
	if _, err := SealWithPassphrase("short", []byte("x")); err == nil {
		t.Fatal("太短的口令应当被拒绝")
	}
}

func withMachine(t *testing.T, parts map[string]string) {
	t.Helper()
	old := machineParts
	machineParts = func() (map[string]string, error) { return parts, nil }
	t.Cleanup(func() { machineParts = old })
}

func TestKeyFile_绑定机器(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFileName)
	withMachine(t, map[string]string{"machine-id": "aaa", "board": "X1"})

	if _, _, err := LoadKey(path, false); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("没有密钥文件且不许生成时应当报 ErrKeyMissing，实际 %v", err)
	}
	b1, created, err := LoadKey(path, true)
	if err != nil || !created {
		t.Fatalf("首次应当生成密钥, created=%v err=%v", created, err)
	}
	b2, created, err := LoadKey(path, true)
	if err != nil || created || b2.KeyID() != b1.KeyID() {
		t.Fatalf("再次打开应当得到同一把密钥, created=%v err=%v", created, err)
	}

	// 机器上多出一项新的特征不影响已有的密钥。
	withMachine(t, map[string]string{"machine-id": "aaa", "board": "X1", "cpu": "new"})
	if _, _, err := LoadKey(path, false); err != nil {
		t.Fatalf("多出一项特征不应让密钥失效: %v", err)
	}
	// 换了机器。
	withMachine(t, map[string]string{"machine-id": "bbb", "board": "X1"})
	if _, _, err := LoadKey(path, true); !errors.Is(err, ErrMachineChanged) {
		t.Fatalf("换了机器应当报 ErrMachineChanged，而且不能悄悄生成新密钥，实际 %v", err)
	}
}

func TestKeyFile_备份与恢复(t *testing.T) {
	dir := t.TempDir()
	withMachine(t, map[string]string{"machine-id": "aaa"})
	old, _, err := LoadKey(filepath.Join(dir, "old.key"), true)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := BackupKey(filepath.Join(dir, "old.key"), "a long passphrase")
	if err != nil {
		t.Fatal(err)
	}

	withMachine(t, map[string]string{"machine-id": "bbb"})
	fresh := filepath.Join(dir, "new.key")
	if err := RestoreKey(fresh, "wrong passphrase!", backup); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("口令不对应当恢复失败，实际 %v", err)
	}
	if err := RestoreKey(fresh, "a long passphrase", backup); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadKey(fresh, false)
	if err != nil || got.KeyID() != old.KeyID() {
		t.Fatalf("恢复出来的不是同一把密钥, err=%v", err)
	}
	if err := RestoreKey(fresh, "a long passphrase", backup); err == nil {
		t.Fatal("已有密钥文件时恢复应当拒绝覆盖")
	}
}
