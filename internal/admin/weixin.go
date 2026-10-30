package admin

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/channel/weixin"
	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/vault"
)

// 微信渠道的扫码连接。
//
// 微信和别的渠道不一样，没有 AppID 和 AppSecret 可填：用微信扫一个二维码，服务端就发下一个机器人凭证。
// 所以它在这一页有自己的流程——申请二维码、显示出来、等用户扫码确认，成了之后把凭证存进保险库、
// 把渠道写进配置。扫码的那个微信号就是这个渠道的主人。
//
// 进行到一半的登录只留在这个进程的内存里，过几分钟不扫就作废。

// weixinLoginTTL 是一个登录二维码留多久。
const weixinLoginTTL = 5 * time.Minute

type weixinData struct {
	Name string
	// QR 是画好的二维码；Link 是它的内容，二维码显示不出来时可以直接在手机上打开。
	QR   template.HTML
	Link string
}

// weixinStart 申请一个登录二维码，然后跳到显示它的页面。
func (s *Server) weixinStart(w http.ResponseWriter, r *http.Request, _ *view) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	err := func() error {
		if !vault.ValidLabel(name) || strings.ContainsAny(name, " /") {
			return errors.New("名称仅支持字母、数字、中文及 _ - .")
		}
		// 名称已经被别的类型的渠道用了的话，不能拿来接微信：那会把它的凭据冲掉。
		_, list, err := s.readChannels()
		if err != nil {
			return err
		}
		for _, c := range list {
			if c.Name == name && c.Type != "weixin" {
				return errors.New("名称「" + name + "」已被其他类型的消息源使用")
			}
		}
		login, err := weixin.StartLogin(r.Context(), nil, s.wxBase)
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.wxLogins == nil {
			s.wxLogins = make(map[string]*weixin.Login)
		}
		s.wxLogins[name] = login
		s.mu.Unlock()
		return nil
	}()
	if err != nil {
		back(w, r, "/channels", "", err)
		return
	}
	http.Redirect(w, r, "/channels/weixin?name="+template.URLQueryEscaper(name), http.StatusSeeOther)
}

// weixinLogin 取出一个进行中的登录。过期的顺手清掉。
func (s *Server) weixinLogin(name string) *weixin.Login {
	s.mu.Lock()
	defer s.mu.Unlock()
	login := s.wxLogins[name]
	if login != nil && s.now().Sub(login.StartedAt) > weixinLoginTTL {
		delete(s.wxLogins, name)
		return nil
	}
	return login
}

func (s *Server) weixinDrop(name string) {
	s.mu.Lock()
	delete(s.wxLogins, name)
	s.mu.Unlock()
}

// weixinPage 显示登录二维码。页面上的脚本会不停地问 weixinPoll，直到登录有了结果。
func (s *Server) weixinPage(w http.ResponseWriter, r *http.Request, v *view) {
	name := r.URL.Query().Get("name")
	login := s.weixinLogin(name)
	if login == nil {
		http.Redirect(w, r, "/channels?e="+template.URLQueryEscaper("二维码已失效，请重新发起扫码连接"), http.StatusSeeOther)
		return
	}
	qr, err := qrSVG(login.QRContent)
	if err != nil {
		v.Error = "生成二维码失败: " + err.Error()
	}
	v.Data = weixinData{Name: name, QR: qr, Link: login.QRContent}
	s.render(w, "weixin", v)
}

// weixinPoll 查一次登录状态。请求会挂住一会儿：服务端要等状态有变化才回话。
//
// 登录成了就在这里收尾：凭证进保险库，渠道进配置，扫码的账号设成主人。
func (s *Server) weixinPoll(w http.ResponseWriter, r *http.Request, _ *view) {
	name := r.PostFormValue("name")
	code := strings.TrimSpace(r.PostFormValue("code"))
	// done 为真表示不用再问了：成了，或者这个二维码已经用不了。
	reply := func(ok, done bool, message string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "done": done, "message": message})
	}
	login := s.weixinLogin(name)
	if login == nil {
		reply(false, true, "二维码已失效，请返回「消息源」页重新发起扫码连接。")
		return
	}
	res, err := login.Poll(r.Context(), code)
	if err != nil {
		s.weixinDrop(name)
		reply(false, true, "连接失败："+err.Error())
		return
	}
	switch res.Status {
	case weixin.StatusWait:
		reply(true, false, "等待扫码：请使用手机微信扫描二维码。")
	case weixin.StatusScanned:
		reply(true, false, "已扫码，正在验证，请在手机上确认。")
	case weixin.StatusNeedCode:
		reply(true, false, "请在下方输入手机微信上显示的数字。")
	case weixin.StatusExpired:
		s.weixinDrop(name)
		reply(false, true, "二维码已过期，请返回「消息源」页重新发起扫码连接。")
	case weixin.StatusBlocked:
		s.weixinDrop(name)
		reply(false, true, "数字多次输入错误，本次连接已终止，请稍后重试。")
	case weixin.StatusAlready:
		s.weixinDrop(name)
		reply(false, true, "该微信号已连接过，无需重复连接。")
	case weixin.StatusConfirmed:
		s.weixinDrop(name)
		if err := s.weixinSave(r, name, res); err != nil {
			reply(false, true, "扫码已确认，但保存失败："+err.Error())
			return
		}
		s.opts.Log.Info("微信消息源已在后台扫码连接", "消息源", name, "来自", r.RemoteAddr)
		reply(true, true, "微信已连接，扫码的微信号已设为该消息源的主人。重启服务后生效。")
	}
}

// weixinSave 把登录拿到的东西存下来：凭证进保险库，渠道进配置。
func (s *Server) weixinSave(r *http.Request, name string, res weixin.LoginResult) error {
	c := config.ChannelConfig{Name: name, Type: "weixin", AppID: res.BotID, APIBase: res.BaseURL}
	label := c.VaultUser()
	if err := s.opts.Vault.Seal(r.Context(), label, res.Token, "微信消息源 "+name+" 的机器人凭证", []string{c.VaultUser()}); err != nil {
		return err
	}
	c.Secret = vault.HandlePrefix + label
	// 扫码的就是用户本人：这一页要登录才进得来，二维码也只显示在这一页上。
	if res.UserID != "" {
		c.Owners = []string{res.UserID}
	}
	return s.editChannels(func(list []config.ChannelConfig) ([]config.ChannelConfig, error) {
		for i := range list {
			if list[i].Name == name {
				list[i] = c
				return list, nil
			}
		}
		return append(list, c), nil
	})
}
