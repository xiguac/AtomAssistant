package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// 用用户自己设的密钥（口令）加密：导出文件和数据密钥的备份都用它。
// 这类文件要离开这台机器，所以不能用和硬件绑在一起的那把密钥。

// MinPassphrase 是口令的最短长度。导出文件里是全部数据，口令太短等于没加密。
const MinPassphrase = 10

var passMagic = []byte("ATOMPASS1\n")

// ErrPassphrase 表示口令不对，或者文件被改动过。两者从结果上分不出来。
var ErrPassphrase = errors.New("seal: 口令不对，或者文件已损坏")

const (
	// passIterations 是 PBKDF2-HMAC-SHA256 的迭代次数。它写进文件头，以后调高了，旧文件照样能解开。
	passIterations = 600_000
	passSalt       = 16
)

func passAEAD(passphrase string, salt []byte, iterations int) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, KeySize)
	if err != nil {
		return nil, fmt.Errorf("seal: 派生密钥失败: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("seal: 初始化加密失败: %w", err)
	}
	return cipher.NewGCM(block)
}

// SealWithPassphrase 用口令加密一段数据。
func SealWithPassphrase(passphrase string, plain []byte) ([]byte, error) {
	if len([]rune(passphrase)) < MinPassphrase {
		return nil, fmt.Errorf("seal: 口令至少要 %d 个字符", MinPassphrase)
	}
	salt := make([]byte, passSalt)
	_, _ = rand.Read(salt)
	aead, err := passAEAD(passphrase, salt, passIterations)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 0, len(passMagic)+4+passSalt+aead.NonceSize())
	head = append(head, passMagic...)
	head = binary.BigEndian.AppendUint32(head, passIterations)
	head = append(head, salt...)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	out := append(head, nonce...)
	// 文件头参与认证：改了迭代次数或者盐，解密直接失败。
	return aead.Seal(out, nonce, plain, head), nil
}

// IsPassphraseSealed 判断一段数据是不是 SealWithPassphrase 的结果。
func IsPassphraseSealed(data []byte) bool {
	return len(data) >= len(passMagic) && string(data[:len(passMagic)]) == string(passMagic)
}

// OpenWithPassphrase 用口令解开 SealWithPassphrase 的结果。
func OpenWithPassphrase(passphrase string, data []byte) ([]byte, error) {
	if !IsPassphraseSealed(data) {
		return nil, fmt.Errorf("seal: 这不是用口令加密的文件")
	}
	headLen := len(passMagic) + 4 + passSalt
	if len(data) < headLen+12 {
		return nil, ErrPassphrase
	}
	head := data[:headLen]
	iterations := binary.BigEndian.Uint32(head[len(passMagic):])
	// 迭代次数来自文件，先卡住范围：不然一个伪造的文件头能让这里算上几个小时。
	if iterations < 100_000 || iterations > 10_000_000 {
		return nil, ErrPassphrase
	}
	aead, err := passAEAD(passphrase, head[len(passMagic)+4:], int(iterations))
	if err != nil {
		return nil, err
	}
	rest := data[headLen:]
	plain, err := aead.Open(nil, rest[:aead.NonceSize()], rest[aead.NonceSize():], head)
	if err != nil {
		return nil, ErrPassphrase
	}
	return plain, nil
}
