package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 数据密钥的保管。
//
// 数据密钥是首次启动时随机生成的，不直接落盘：落盘的是它被"包"起来之后的样子。
// 包它的那把密钥由两样东西派生——同样在首次启动时生成的一段随机种子，和这台机器的硬件特征。
// 所以密钥文件连同数据库一起被拷到别的机器上，是解不开的。
//
// 硬件特征本身不是秘密（同一台机器上的任何程序都读得到），真正要保密的是密钥文件：
// 它的权限是 0600，只有助手自己的账号和 root 读得到。
//
// 换机器、或者机器的硬件标识变了，这把密钥就失效了。两条退路：
// 迁移走导出导入（导出文件用用户自己设的口令加密，与硬件无关）；
// 意外情况靠事先用 BackupKey 留下的备份，用 RestoreKey 在新环境里恢复。

// KeyFileName 是密钥文件的默认名字，与配置文件放在同一个目录。
const KeyFileName = "atomassistant.key"

// wrapLabel 是派生"包数据密钥的那把密钥"时用的标签。
const wrapLabel = "atomassistant key wrap v1"

// ErrKeyMissing 表示密钥文件不存在。
var ErrKeyMissing = errors.New("seal: 密钥文件不存在")

// ErrMachineChanged 表示密钥文件不是在这台机器上生成的，或者机器的硬件标识变了。
var ErrMachineChanged = errors.New("seal: 密钥文件与这台机器对不上（换了机器，或者硬件标识变了）")

// machineParts 返回这台机器的硬件特征：名字 -> 值。测试里会换掉它。
var machineParts = readMachineParts

type keyFile struct {
	Version int `json:"version"`
	// Seed 是首次启动时生成的随机种子。
	Seed string `json:"seed"`
	// Wrapped 是被包起来的数据密钥。
	Wrapped string `json:"wrapped"`
	// Bound 是派生时用到的硬件特征的名字（不含值）。以后只认这几项，
	// 机器上多出一项新的特征不会让密钥失效。
	Bound []string `json:"bound"`
}

// wrapKey 派生出包数据密钥用的那把密钥。
func wrapKey(label string, seed []byte, bound []string) (cipher.AEAD, error) {
	parts, err := machineParts()
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	for _, name := range bound {
		v, ok := parts[name]
		if !ok || v == "" {
			return nil, fmt.Errorf("%w：读不到 %s", ErrMachineChanged, name)
		}
		fmt.Fprintf(&sb, "%s=%s\n", name, v)
	}
	salt := sha256.Sum256([]byte(sb.String()))
	k, err := hkdf.Key(sha256.New, seed, salt[:], label, KeySize)
	if err != nil {
		return nil, fmt.Errorf("seal: 派生密钥失败: %w", err)
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, fmt.Errorf("seal: 初始化加密失败: %w", err)
	}
	return cipher.NewGCM(block)
}

// readKey 读出密钥文件里的数据密钥。
func readKey(path, label string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrKeyMissing, path)
	}
	if err != nil {
		return nil, fmt.Errorf("seal: 读取密钥文件失败: %w", err)
	}
	var kf keyFile
	if err := json.Unmarshal(raw, &kf); err != nil || kf.Version != 1 {
		return nil, fmt.Errorf("seal: 密钥文件 %s 的内容无法识别", path)
	}
	seed, err1 := base64.StdEncoding.DecodeString(kf.Seed)
	wrapped, err2 := base64.StdEncoding.DecodeString(kf.Wrapped)
	if err1 != nil || err2 != nil || len(seed) != KeySize || len(kf.Bound) == 0 {
		return nil, fmt.Errorf("seal: 密钥文件 %s 的内容无法识别", path)
	}
	aead, err := wrapKey(label, seed, kf.Bound)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < aead.NonceSize() {
		return nil, fmt.Errorf("seal: 密钥文件 %s 的内容无法识别", path)
	}
	key, err := aead.Open(nil, wrapped[:aead.NonceSize()], wrapped[aead.NonceSize():], nil)
	if err != nil {
		return nil, ErrMachineChanged
	}
	return key, nil
}

// writeKey 把一把数据密钥包起来写进密钥文件。文件已经存在时拒绝覆盖。
func writeKey(path, label string, key []byte) error {
	parts, err := machineParts()
	if err != nil {
		return err
	}
	bound := make([]string, 0, len(parts))
	for name, v := range parts {
		if v != "" {
			bound = append(bound, name)
		}
	}
	if len(bound) == 0 {
		return fmt.Errorf("seal: 读不到这台机器的任何硬件标识，无法生成密钥")
	}
	sort.Strings(bound)
	seed := make([]byte, KeySize)
	_, _ = rand.Read(seed)
	aead, err := wrapKey(label, seed, bound)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	body, err := json.MarshalIndent(keyFile{
		Version: 1,
		Seed:    base64.StdEncoding.EncodeToString(seed),
		Wrapped: base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, key, nil)),
		Bound:   bound,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("seal: 生成密钥文件失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("seal: 创建密钥文件所在目录失败: %w", err)
	}
	// O_EXCL：密钥文件一旦被覆盖，原来的数据就再也解不开了，所以绝不覆盖。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("seal: 写入密钥文件失败: %w", err)
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("seal: 写入密钥文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("seal: 写入密钥文件失败: %w", err)
	}
	return nil
}

// LoadKey 打开密钥文件。create 为真且文件不存在时，生成一把新的数据密钥。
//
// 只有常驻服务（atomd）应该传 create=true：运维命令常常是以 root 身份跑的，
// 由它生成的密钥文件服务账号读不到。created 为真表示这次新生成了密钥。
func LoadKey(path string, create bool) (box *Box, created bool, err error) {
	key, err := readKey(path, wrapLabel)
	if errors.Is(err, ErrKeyMissing) && create {
		key = make([]byte, KeySize)
		_, _ = rand.Read(key)
		if err := writeKey(path, wrapLabel, key); err != nil {
			return nil, false, err
		}
		created = true
	} else if err != nil {
		return nil, false, err
	}
	box, err = NewBox(key)
	return box, created, err
}

// BackupKey 把数据密钥用口令加密后交出来，供用户另存。
// 机器的硬件标识意外变化时，靠它和 RestoreKey 找回数据。
func BackupKey(path, passphrase string) ([]byte, error) {
	key, err := readKey(path, wrapLabel)
	if err != nil {
		return nil, err
	}
	return SealWithPassphrase(passphrase, key)
}

// RestoreKey 用 BackupKey 的备份在这台机器上重建密钥文件。已有密钥文件时拒绝覆盖。
func RestoreKey(path, passphrase string, backup []byte) error {
	key, err := OpenWithPassphrase(passphrase, backup)
	if err != nil {
		return err
	}
	if len(key) != KeySize {
		return fmt.Errorf("seal: 这份备份里不是数据密钥")
	}
	return writeKey(path, wrapLabel, key)
}
