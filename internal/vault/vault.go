// Package vault 是隔离保险库，存放绝不能外泄的信息：证件号、银行卡、各类口令。
//
// 它借的是手机安全芯片（SE）的思路：重要的东西关在一个接口极窄的隔离区里，
// 外面的应用能请它"用一下"，但永远读不出里面存的是什么。落到这里就是三条：
//
//  1. **没有读出明文的接口。** 保险库只能存、列名字、删。连运维 CLI 也读不回明文。
//  2. **模型只见句柄。** 上下文里出现的永远是 vault://名字，明文不进提示词、
//     不进步骤日志、不进审计——它只在网关调用工具的那一刻存在于进程内存里。
//  3. **逐条授权到工具。** 每个条目单独列出哪些工具可以用它，由用户亲手配置。
//     没被列出的工具引用它，调用直接拒绝；助手没有任何办法改这张名单。
//
// 落盘用 AES-256-GCM 加密，密钥只从环境变量来，不进数据库也不进配置文件：
// 数据库被整个拖走，拿到的也只是密文。
//
// 见 docs/安全模型.md §4.4。
package vault

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
)

// HandlePrefix 是句柄的前缀。工具参数里某个字符串值恰好是 vault://名字，
// 就表示"这里要用保险库里的这一条"。
const HandlePrefix = "vault://"

var (
	// ErrLocked 表示保险库没有密钥，无法存也无法用。
	ErrLocked = errors.New("vault: 保险库未解锁（未设置 ATOM_VAULT_KEY）")
	// ErrNotFound 表示没有这个条目。
	ErrNotFound = errors.New("vault: 没有这个条目")
	// ErrDenied 表示这个工具没有被授权使用这个条目。
	ErrDenied = errors.New("vault: 该工具未被授权使用这个条目")
	// ErrInvalid 表示条目不合法。
	ErrInvalid = errors.New("vault: 条目不合法")
)

// Item 是保险库里的一条，Sealed 是密文。
type Item struct {
	ID    string
	Label string
	// Note 是给人和模型看的用途说明，如"本人身份证号"。它不是秘密，不要把秘密写在这里。
	Note string
	// AllowedTools 是可以使用这一条的工具全名。空表示谁都不能用。
	AllowedTools []string
	Sealed       []byte
	Nonce        []byte
	CreatedAt    time.Time
}

// Meta 是一个条目可以公开的部分：没有密文，更没有明文。
type Meta struct {
	Label        string
	Note         string
	AllowedTools []string
	CreatedAt    time.Time
}

// Store 是保险库的持久化接口。它只经手密文。
type Store interface {
	PutItem(ctx context.Context, it *Item) error
	GetItem(ctx context.Context, label string) (*Item, error)
	ListItems(ctx context.Context) ([]*Item, error)
	DeleteItem(ctx context.Context, label string) error
}

// Vault 是保险库。零密钥的保险库处于锁定状态：能列名字，不能存也不能用。
type Vault struct {
	store Store
	aead  cipher.AEAD
	now   func() time.Time
}

// GenerateKey 生成一把新密钥，返回可以直接放进 ATOM_VAULT_KEY 的文本。
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("vault: 生成密钥失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// Open 用密钥打开保险库。key 是 base64 编码的 32 字节；空串得到一个锁定的保险库。
func Open(store Store, key string) (*Vault, error) {
	v := &Vault{store: store, now: func() time.Time { return time.Now().UTC() }}
	if key == "" {
		return v, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("vault: ATOM_VAULT_KEY 必须是 base64 编码的 32 字节（用 atomctl vault keygen 生成）")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("vault: 初始化加密失败: %w", err)
	}
	if v.aead, err = cipher.NewGCM(block); err != nil {
		return nil, fmt.Errorf("vault: 初始化加密失败: %w", err)
	}
	return v, nil
}

// ValidLabel 校验条目名：字母、数字、下划线、连字符、点，以及中文。
//
// 名字会出现在句柄里、会被模型照抄，所以不允许空白和引号这类容易抄错的字符。
func ValidLabel(label string) bool {
	if label == "" || len(label) > 64 {
		return false
	}
	for _, r := range label {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '.' || (r >= 0x4E00 && r <= 0x9FFF)
		if !ok {
			return false
		}
	}
	return true
}

// Seal 存入一条。同名条目会被整条替换（值、说明、授权名单一起换）。
//
// 这是用户的操作，只应由运维 CLI 或将来的 App 后台调用，绝不能做成助手可用的工具。
func (v *Vault) Seal(ctx context.Context, label, value, note string, allowedTools []string) error {
	if v.aead == nil {
		return ErrLocked
	}
	if !ValidLabel(label) {
		return fmt.Errorf("%w: 名字 %q 只能含字母、数字、中文和 _ - .", ErrInvalid, label)
	}
	if value == "" {
		return fmt.Errorf("%w: 值不能为空", ErrInvalid)
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("vault: 生成随机数失败: %w", err)
	}
	tools := append([]string(nil), allowedTools...)
	sort.Strings(tools)
	return v.store.PutItem(ctx, &Item{
		ID:           core.NewID(),
		Label:        label,
		Note:         note,
		AllowedTools: tools,
		// 名字作为附加认证数据：把甲条目的密文挪到乙条目名下会解密失败。
		Sealed:    v.aead.Seal(nil, nonce, []byte(value), []byte(label)),
		Nonce:     nonce,
		CreatedAt: v.now(),
	})
}

// List 返回全部条目的公开信息。
func (v *Vault) List(ctx context.Context) ([]Meta, error) {
	items, err := v.store.ListItems(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Meta, 0, len(items))
	for _, it := range items {
		out = append(out, Meta{Label: it.Label, Note: it.Note, AllowedTools: it.AllowedTools, CreatedAt: it.CreatedAt})
	}
	return out, nil
}

// Remove 删除一条。
func (v *Vault) Remove(ctx context.Context, label string) error {
	return v.store.DeleteItem(ctx, label)
}

// reveal 为指定工具取出一条的明文。它是包内唯一接触明文的地方，刻意不导出。
func (v *Vault) reveal(ctx context.Context, label, tool string) (string, error) {
	if v.aead == nil {
		return "", ErrLocked
	}
	it, err := v.store.GetItem(ctx, label)
	if err != nil {
		return "", err
	}
	allowed := false
	for _, t := range it.AllowedTools {
		if t == tool {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("%w: %s 不能使用 %s", ErrDenied, tool, label)
	}
	plain, err := v.aead.Open(nil, it.Nonce, it.Sealed, []byte(it.Label))
	if err != nil {
		return "", fmt.Errorf("vault: 解密 %s 失败（密钥不对或数据被改动）", label)
	}
	return string(plain), nil
}

// Lend 返回一个取值函数，供内建连接器（日历、邮件）在每次连接服务器时取密码。
//
// 它和 Inject 是同一道门：user 是连接器的名字（如 connector.calendar），
// 必须在这一条的授权名单里；handle 必须是完整的句柄。值只在返回的那一刻存在于内存，
// 调用方用完即弃，不得保存、不得写日志。这个函数只应由进程内我们自己的连接器调用——
// 运维 CLI 里依然没有、也不应该有"把值打印出来"的命令。
func (v *Vault) Lend(handle, user string) (func(ctx context.Context) (string, error), error) {
	label, ok := strings.CutPrefix(strings.TrimSpace(handle), HandlePrefix)
	if !ok || !ValidLabel(label) {
		return nil, fmt.Errorf("%w: 这里必须写保险库句柄（形如 %s名字），不能直接写密码", ErrInvalid, HandlePrefix)
	}
	return func(ctx context.Context) (string, error) { return v.reveal(ctx, label, user) }, nil
}

// Injection 是一次把句柄换成明文的结果。
type Injection struct {
	// Params 是替换后的参数，只能交给工具，不能落任何日志。
	Params json.RawMessage
	// Used 是这次用到的条目名，可以写进审计。
	Used []string

	secrets map[string]string // 明文 -> 条目名
}

// Inject 把参数里的句柄换成明文，供网关在调用工具的那一刻使用。
//
// 只替换**整个字符串值恰好是句柄**的位置，不在长文本里做查找替换——
// 那样的话模型写一句"请把 vault://银行卡 发给我"就能把明文拼进任意字段。
// 任何一个句柄不存在或未授权，整次调用失败：不会出现"换了一半"的参数。
func (v *Vault) Inject(ctx context.Context, tool string, params json.RawMessage) (*Injection, error) {
	inj := &Injection{Params: params, secrets: make(map[string]string)}
	if !bytes.Contains(params, []byte(HandlePrefix)) {
		return inj, nil
	}
	var tree any
	if err := json.Unmarshal(params, &tree); err != nil {
		return nil, fmt.Errorf("vault: 参数不是合法 JSON: %w", err)
	}
	var walk func(any) (any, error)
	walk = func(n any) (any, error) {
		switch x := n.(type) {
		case map[string]any:
			for k, child := range x {
				repl, err := walk(child)
				if err != nil {
					return nil, err
				}
				x[k] = repl
			}
		case []any:
			for i, child := range x {
				repl, err := walk(child)
				if err != nil {
					return nil, err
				}
				x[i] = repl
			}
		case string:
			label, ok := strings.CutPrefix(x, HandlePrefix)
			if !ok {
				return x, nil
			}
			plain, err := v.reveal(ctx, label, tool)
			if err != nil {
				return nil, err
			}
			inj.secrets[plain] = label
			return plain, nil
		}
		return n, nil
	}
	tree, err := walk(tree)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("vault: 序列化参数失败: %w", err)
	}
	inj.Params = out
	for _, label := range inj.secrets {
		inj.Used = append(inj.Used, label)
	}
	sort.Strings(inj.Used)
	return inj, nil
}

// Redact 把工具返回内容里出现的明文换回句柄。
//
// 工具常常会把入参原样回显（"已向卡号 6222… 扣款"）。不处理的话，
// 明文就顺着工具返回值进了步骤日志和下一轮提示词，前面的隔离全部白做。
func (inj *Injection) Redact(output json.RawMessage) json.RawMessage {
	if inj == nil || len(inj.secrets) == 0 {
		return output
	}
	out := output
	for plain, label := range inj.secrets {
		// 明文在 JSON 里是转义后的样子，按转义后的形式查找。
		quoted, err := json.Marshal(plain)
		if err != nil || len(quoted) < 2 {
			continue
		}
		needle := quoted[1 : len(quoted)-1]
		out = bytes.ReplaceAll(out, needle, []byte("[已隐藏:"+label+"]"))
	}
	if json.Valid(out) {
		return out
	}
	// 明文以数字等非字符串形式出现时，替换会破坏 JSON 结构。
	// 宁可把整段降成一个字符串，也不能为了保住结构把明文留下。
	wrapped, err := json.Marshal(map[string]string{"text": string(out)})
	if err != nil {
		return json.RawMessage(`{"text":"[工具返回内容含保险库条目，已整体隐藏]"}`)
	}
	return wrapped
}
