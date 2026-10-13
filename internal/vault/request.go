package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 待填写的条目：助手需要一项它没有的密钥或敏感信息时，不向用户索要，而是在这里登记一个"待填写"——
// 它只给出名字、用途和哪些对象可以用，值由用户在管理后台亲手填。助手自始至终看不到值。
//
// 这样新接一个要密钥的服务不用改代码：要不要输入框、输入框叫什么，由助手登记的这一条决定。

// ErrBadRequest 表示一条待填写的登记不合规矩。
var ErrBadRequest = errors.New("vault: 这条待填写的登记不合规矩")

const (
	// requestPrefix 是待填写的条目在设置表里的名字前缀。
	requestPrefix = "vault.request."
	// maxRequests 是同时挂着的待填写条目的上限：助手跑飞了也不至于把页面塞满。
	maxRequests = 20
	// maxRequestNote 是用途说明的长度上限。
	maxRequestNote = 200
	// maxRequestTools 是一条登记里授权对象的个数上限。
	maxRequestTools = 20
)

// Request 是一条待填写的登记。
type Request struct {
	// Label 是条目的名字，填好之后以 vault://Label 引用。
	Label string `json:"label"`
	// Note 是用途说明，是助手写的。
	Note string `json:"note"`
	// Tools 是填好之后可以使用它的对象：工具名，或 mcp.<server 名>。
	Tools []string `json:"tools"`
	// Task 是登记它的任务。
	Task string    `json:"task,omitempty"`
	At   time.Time `json:"at"`
}

// RequestStore 存放待填写的登记，由 *pg.Store 的设置表提供。
type RequestStore interface {
	PutSetting(ctx context.Context, key, value string) error
	SettingsWithPrefix(ctx context.Context, prefix string) (map[string]string, error)
}

// Requests 管理待填写的登记。
type Requests struct {
	Store RequestStore
	// Vault 用来查重：已经有值的名字不能再登记，否则助手可以借此诱使用户把一条现有的凭据换掉。
	Vault interface {
		List(ctx context.Context) ([]Meta, error)
	}
}

// ownedByPages 是只能在各自的管理页面里填写的凭据的名字前缀，助手不能登记。
var ownedByPages = []string{"provider.", "channel.", "connector."}

func ownedByPage(name string) bool {
	for _, p := range ownedByPages {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Add 登记一条待填写的条目。同名的登记会被新的替换。
func (r *Requests) Add(ctx context.Context, req Request) error {
	req.Label = strings.TrimSpace(req.Label)
	if !ValidLabel(req.Label) {
		return fmt.Errorf("%w: 名字只能用字母、数字、中文及 _ - .", ErrBadRequest)
	}
	if ownedByPage(req.Label) {
		return fmt.Errorf("%w: %s 是模型服务、消息源或连接器的凭据，只能由用户在对应的管理页面填写", ErrBadRequest, req.Label)
	}
	req.Note = strings.Join(strings.Fields(req.Note), " ")
	if req.Note == "" {
		return fmt.Errorf("%w: 要说明用途，用户靠它判断该不该填", ErrBadRequest)
	}
	if n := []rune(req.Note); len(n) > maxRequestNote {
		req.Note = string(n[:maxRequestNote])
	}
	tools := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !ValidLabel(t) || ownedByPage(t) {
			return fmt.Errorf("%w: 授权对象 %q 不合规矩，应是工具名或 mcp.<server 名>", ErrBadRequest, t)
		}
		tools = append(tools, t)
	}
	if len(tools) == 0 || len(tools) > maxRequestTools {
		return fmt.Errorf("%w: 要写明哪些工具可以用它（1 到 %d 个）", ErrBadRequest, maxRequestTools)
	}
	req.Tools = tools
	metas, err := r.Vault.List(ctx)
	if err != nil {
		return err
	}
	for _, m := range metas {
		if m.Label == req.Label {
			return fmt.Errorf("%w: 名为 %s 的条目已经有值了，直接用 %s%s 引用", ErrBadRequest, req.Label, HandlePrefix, req.Label)
		}
	}
	pending, err := r.List(ctx)
	if err != nil {
		return err
	}
	replacing := false
	for _, p := range pending {
		replacing = replacing || p.Label == req.Label
	}
	if !replacing && len(pending) >= maxRequests {
		return fmt.Errorf("%w: 待填写的条目已经有 %d 个，等用户处理之后再登记", ErrBadRequest, maxRequests)
	}
	if req.At.IsZero() {
		req.At = time.Now().UTC()
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("vault: 序列化登记失败: %w", err)
	}
	return r.Store.PutSetting(ctx, requestPrefix+req.Label, string(raw))
}

// List 列出待填写的条目，先登记的在前。
func (r *Requests) List(ctx context.Context) ([]Request, error) {
	all, err := r.Store.SettingsWithPrefix(ctx, requestPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(all))
	for _, raw := range all {
		var req Request
		// 读不出来的跳过：一条坏掉的登记不该让整个页面打不开。
		if json.Unmarshal([]byte(raw), &req) == nil && req.Label != "" {
			out = append(out, req)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}

// Get 取出一条待填写的登记，没有时返回 nil。
func (r *Requests) Get(ctx context.Context, label string) (*Request, error) {
	list, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Label == label {
			return &list[i], nil
		}
	}
	return nil, nil
}

// Remove 去掉一条登记：用户填好了，或者不打算填。
func (r *Requests) Remove(ctx context.Context, label string) error {
	return r.Store.PutSetting(ctx, requestPrefix+label, "")
}
