package admin

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/vault"
)

// 「渠道」页：接入聊天软件里的机器人，并指定谁是主人。
//
// 这一页改的是配置文件里 channels 一节。写在这里的渠道，其主人发来的消息带着用户等级的信任，
// 所以它只能由用户本人在后台（或直接编辑配置文件）填写——助手自己没有任何办法往这里加东西，
// 见 docs/安全模型.md §3.1.1。凭据照旧只进保险库，配置里留的是句柄。

// channelView 是页面上的一个渠道。
type channelView struct {
	config.ChannelConfig
	// SecretOK 为假表示保险库里还没有这个渠道的凭据。
	SecretOK bool
}

type channelsData struct {
	Channels []channelView
	Types    []string
}

func (s *Server) readChannels() ([]byte, []config.ChannelConfig, error) {
	if s.opts.ConfigPath == "" {
		return nil, nil, errors.New("未设置 ATOM_CONFIG，无法确定配置文件位置")
	}
	raw, err := os.ReadFile(s.opts.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	list, err := config.ReadChannels(raw)
	return raw, list, err
}

// editChannels 改动渠道一节并存回配置文件。校验不过的一个字都不会写进去。
func (s *Server) editChannels(change func(list []config.ChannelConfig) ([]config.ChannelConfig, error)) error {
	raw, list, err := s.readChannels()
	if err != nil {
		return err
	}
	if list, err = change(list); err != nil {
		return err
	}
	out, err := config.WriteChannels(raw, list)
	if err != nil {
		return err
	}
	return s.writeConfig(string(out))
}

func (s *Server) channelsPage(w http.ResponseWriter, r *http.Request, v *view) {
	d := channelsData{Types: config.ChannelTypes}
	_, list, err := s.readChannels()
	if err != nil {
		v.Error = err.Error()
	}
	stored := map[string]bool{}
	if metas, err := s.opts.Vault.List(r.Context()); err != nil {
		v.Error = err.Error()
	} else {
		for _, meta := range metas {
			stored[meta.Label] = true
		}
	}
	for _, c := range list {
		d.Channels = append(d.Channels, channelView{
			ChannelConfig: c, SecretOK: stored[strings.TrimPrefix(c.Secret, vault.HandlePrefix)],
		})
	}
	v.Data = d
	s.render(w, "channels", v)
}

// channelSave 新增或修改一个渠道。填了凭据就存进保险库，配置里只留句柄。
func (s *Server) channelSave(w http.ResponseWriter, r *http.Request, _ *view) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	kind := r.PostFormValue("type")
	appID := strings.TrimSpace(r.PostFormValue("app_id"))
	secret := strings.TrimSpace(r.PostFormValue("secret"))
	sandbox := r.PostFormValue("sandbox") != ""
	err := func() error {
		if !vault.ValidLabel(name) || strings.ContainsAny(name, " /") {
			return errors.New("名称仅支持字母、数字、中文及 _ - .")
		}
		if appID == "" {
			return errors.New("请填写 AppID")
		}
		_, list, err := s.readChannels()
		if err != nil {
			return err
		}
		at := -1
		for i, c := range list {
			if c.Name == name {
				at = i
			}
		}
		c := config.ChannelConfig{Name: name}
		if at >= 0 {
			// 修改时主人名单和其余设置保留，只换页面上有的那几项。
			c = list[at]
		}
		c.Type, c.AppID, c.Sandbox = kind, appID, sandbox
		if secret != "" {
			label := "channel." + name
			if c.Secret != "" {
				label = strings.TrimPrefix(c.Secret, vault.HandlePrefix)
			}
			if err := s.opts.Vault.Seal(r.Context(), label, secret, "消息渠道 "+name+" 的凭据", []string{c.VaultUser()}); err != nil {
				return err
			}
			c.Secret = vault.HandlePrefix + label
		} else if c.Secret == "" {
			return errors.New("新增渠道时必须填写 AppSecret")
		}
		return s.editChannels(func(list []config.ChannelConfig) ([]config.ChannelConfig, error) {
			if at >= 0 {
				list[at] = c
				return list, nil
			}
			return append(list, c), nil
		})
	}()
	if err == nil {
		s.opts.Log.Info("消息渠道已在后台修改", "渠道", name, "更换凭据", secret != "", "来自", r.RemoteAddr)
	}
	back(w, r, "/channels", "渠道「"+name+"」已保存，重启服务后生效。", err)
}

func (s *Server) channelDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	var gone config.ChannelConfig
	err := s.editChannels(func(list []config.ChannelConfig) ([]config.ChannelConfig, error) {
		for i := range list {
			if list[i].Name == name {
				gone = list[i]
				return append(list[:i], list[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("渠道 %s 不存在", name)
	})
	if err == nil {
		s.dropCredential(r.Context(), gone.Secret, gone.VaultUser())
		s.opts.Log.Info("消息渠道已在后台删除", "渠道", name, "来自", r.RemoteAddr)
	}
	back(w, r, "/channels", "渠道「"+name+"」及其凭据已删除，重启服务后生效。", err)
}

// channelOwner 给一个渠道添加或移除主人。
//
// 这是整个系统里分量最重的一个表单：加进来的账号，说的话就是用户的话。
// 所以它只存在于需要登录、带防伪令牌的后台里，并且每次改动都留日志。
func (s *Server) channelOwner(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	owner := strings.TrimSpace(r.PostFormValue("owner"))
	remove := r.PostFormValue("remove") != ""
	err := s.editChannels(func(list []config.ChannelConfig) ([]config.ChannelConfig, error) {
		if owner == "" || strings.ContainsAny(owner, " \t\r\n") {
			return nil, errors.New("请填写账号标识，标识中不含空白字符")
		}
		for i := range list {
			if list[i].Name != name {
				continue
			}
			kept := list[i].Owners[:0:0]
			for _, o := range list[i].Owners {
				if o != owner {
					kept = append(kept, o)
				}
			}
			if !remove {
				kept = append(kept, owner)
			}
			list[i].Owners = kept
			return list, nil
		}
		return nil, fmt.Errorf("渠道 %s 不存在", name)
	})
	notice := "已将该账号设为渠道「" + name + "」的主人，重启服务后生效。"
	if remove {
		notice = "已移除渠道「" + name + "」的该主人，重启服务后生效。"
	}
	if err == nil {
		s.opts.Log.Warn("消息渠道的主人已在后台变更", "渠道", name, "账号", owner, "移除", remove, "来自", r.RemoteAddr)
	}
	back(w, r, "/channels", notice, err)
}
