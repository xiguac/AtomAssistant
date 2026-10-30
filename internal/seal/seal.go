// Package seal 负责"落库即加密、出库即解密"：数据库里的内容列、工作区里的文件，
// 存下去之前都先过这里，读出来再还原。
//
// 它挡的是这样一种情况：别的程序拿到了数据库（账号、数据文件或者备份），看到的只是密文。
// 它挡不了拿到这台机器上助手自己那个账号的人——助手要无人值守地运行，密钥必须在机器上。
//
// 两种加密方式：
//
//   - Seal：每次加密结果都不同。绝大多数内容用它。
//   - SealEq：同样的内容加密出同样的结果，所以数据库仍然能按"等于"查找（记忆的主体、任务的标签）。
//     代价是看得出两行的值相不相同，所以只用在确实要按值查找的列上。
//
// 每个值都和它所在的位置（scope，如 "tasks.goal"）绑在一起：把一列的密文挪到另一列去，解不开。
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize 是数据密钥的长度。
const KeySize = 32

const (
	// 密文的前缀，标明加密方式与版本。以后换算法或者换密钥，靠它分辨旧数据。
	prefixRandom = "v1."
	prefixEq     = "d1."
)

var (
	// ErrNotSealed 表示读到的不是密文：数据库里的内容被改动过，或者这一列还没有加密。
	ErrNotSealed = errors.New("seal: 不是加密过的内容")
	// ErrOpen 表示解不开：密钥不对，或者密文被改动、被挪了位置。
	ErrOpen = errors.New("seal: 解密失败（密钥不对，或者数据被改动）")
)

// Box 用一把数据密钥加密和解密。可以并发使用。
type Box struct {
	enc cipher.AEAD
	det cipher.AEAD
	mac []byte
	vec []byte
	id  string
}

// NewBox 用一把 32 字节的数据密钥构造 Box。
//
// 实际干活的三把子密钥都是从它派生的，各管各的用途：一把密钥不同时用在两种算法上。
func NewBox(key []byte) (*Box, error) { return newBox(key, sealLabel) }

// sealLabel 是派生子密钥时用的标签前缀。
const sealLabel = "atomassistant seal v1 "

func newBox(key []byte, label string) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("seal: 数据密钥必须是 %d 字节", KeySize)
	}
	derive := func(info string) ([]byte, error) {
		return hkdf.Key(sha256.New, key, nil, label+info, KeySize)
	}
	aead := func(info string) (cipher.AEAD, error) {
		k, err := derive(info)
		if err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	}
	b := &Box{}
	var err error
	if b.enc, err = aead("enc"); err != nil {
		return nil, fmt.Errorf("seal: 初始化加密失败: %w", err)
	}
	if b.det, err = aead("det"); err != nil {
		return nil, fmt.Errorf("seal: 初始化加密失败: %w", err)
	}
	if b.mac, err = derive("mac"); err != nil {
		return nil, fmt.Errorf("seal: 初始化加密失败: %w", err)
	}
	if b.vec, err = derive("vector"); err != nil {
		return nil, fmt.Errorf("seal: 初始化加密失败: %w", err)
	}
	idKey, err := derive("id")
	if err != nil {
		return nil, fmt.Errorf("seal: 初始化加密失败: %w", err)
	}
	b.id = hex.EncodeToString(idKey[:8])
	return b, nil
}

// VectorKey 是给记忆向量做旋转用的子密钥，见 internal/memvec。它是秘密，不要写进日志或存起来。
func (b *Box) VectorKey() []byte { return append([]byte(nil), b.vec...) }

// KeyID 是这把密钥的指纹，不是秘密。数据库里记着它，换了密钥启动时能立刻发现，
// 而不是等读到第一条解不开的数据才报错。
func (b *Box) KeyID() string { return b.id }

// Seal 加密一段文字。空串原样返回：很多列的"没有"就是空串，不必为它生成一段密文。
func (b *Box) Seal(scope, plain string) string {
	if plain == "" {
		return ""
	}
	nonce := make([]byte, b.enc.NonceSize())
	// crypto/rand.Read 不会返回错误：系统拿不出随机数时进程直接终止。
	_, _ = rand.Read(nonce)
	return prefixRandom + base64.RawURLEncoding.EncodeToString(b.enc.Seal(nonce, nonce, []byte(plain), []byte(scope)))
}

// SealEq 加密一段文字，同样的内容在同一个位置上总是得到同样的结果。
//
// 做法是让随机数由内容本身决定：只有内容完全相同时随机数才会重复，而那时密文本来就该相同。
func (b *Box) SealEq(scope, plain string) string {
	if plain == "" {
		return ""
	}
	h := hmac.New(sha256.New, b.mac)
	h.Write([]byte(scope))
	h.Write([]byte{0})
	h.Write([]byte(plain))
	nonce := h.Sum(nil)[:b.det.NonceSize()]
	return prefixEq + base64.RawURLEncoding.EncodeToString(b.det.Seal(nonce, nonce, []byte(plain), []byte(scope)))
}

// Open 解开 Seal 或 SealEq 的结果。
func (b *Box) Open(scope, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	aead := b.enc
	rest, ok := strings.CutPrefix(token, prefixRandom)
	if !ok {
		if rest, ok = strings.CutPrefix(token, prefixEq); !ok {
			return "", fmt.Errorf("%w（%s）", ErrNotSealed, scope)
		}
		aead = b.det
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(raw) < aead.NonceSize() {
		return "", fmt.Errorf("%w（%s）", ErrOpen, scope)
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(scope))
	if err != nil {
		return "", fmt.Errorf("%w（%s）", ErrOpen, scope)
	}
	return string(plain), nil
}

// fileMagic 是加密文件的开头，用来分辨一个文件是不是加密过的。
var fileMagic = []byte("ATOMSEAL1\n")

// IsSealedFile 判断一段文件内容是不是 SealFile 的结果。
func IsSealedFile(data []byte) bool {
	return len(data) >= len(fileMagic) && string(data[:len(fileMagic)]) == string(fileMagic)
}

// SealFile 加密一个文件的内容。
func (b *Box) SealFile(plain []byte) []byte {
	nonce := make([]byte, b.enc.NonceSize())
	_, _ = rand.Read(nonce)
	out := make([]byte, 0, len(fileMagic)+len(nonce)+len(plain)+b.enc.Overhead())
	out = append(out, fileMagic...)
	out = append(out, nonce...)
	return b.enc.Seal(out, nonce, plain, fileMagic)
}

// OpenFile 解开 SealFile 的结果。
func (b *Box) OpenFile(data []byte) ([]byte, error) {
	if !IsSealedFile(data) {
		return nil, fmt.Errorf("%w（文件）", ErrNotSealed)
	}
	rest := data[len(fileMagic):]
	if len(rest) < b.enc.NonceSize() {
		return nil, fmt.Errorf("%w（文件）", ErrOpen)
	}
	plain, err := b.enc.Open(nil, rest[:b.enc.NonceSize()], rest[b.enc.NonceSize():], fileMagic)
	if err != nil {
		return nil, fmt.Errorf("%w（文件）", ErrOpen)
	}
	return plain, nil
}

// TestBox 返回一把固定密钥的 Box。**仅供测试使用**：密钥就写在源码里。
// 放在非 _test.go 文件里，是因为别的包的测试也要用它打开数据库。
func TestBox() *Box {
	b, err := NewBox([]byte("atomassistant-test-key-012345678"))
	if err != nil {
		panic(err)
	}
	return b
}
