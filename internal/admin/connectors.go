package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/connector/calendar"
	"github.com/xiguac/atomhelper/internal/connector/deepseek"
	"github.com/xiguac/atomhelper/internal/connector/mail"
	"github.com/xiguac/atomhelper/internal/connector/weather"
	"github.com/xiguac/atomhelper/internal/vault"
)

// 「连接器」页：日历（CalDAV）、邮件（IMAP/SMTP）的账号与密码，节假日订阅，天气（和风天气），
// 以及联网搜索（DeepSeek 搜索接口）的密钥。
//
// 这一页改的是配置文件里 connectors 一节。密码照旧只进保险库，配置里留的是句柄；
// 各存一条、各自授权，停用其中一个不影响另一个。

type connectorsData struct {
	Calendar *config.CalendarConfig
	Mail     *config.MailConfig
	Holidays *config.HolidaysConfig
	Weather  *config.WeatherConfig
	Search   *config.SearchConfig
	// CalendarKey、MailKey、WeatherKey 与 SearchKey 表示保险库里有没有对应的密码或密钥。
	CalendarKey, MailKey, WeatherKey, SearchKey bool
}

func (s *Server) readConnectors() ([]byte, config.ConnectorsConfig, error) {
	if s.opts.ConfigPath == "" {
		return nil, config.ConnectorsConfig{}, errors.New("未设置 ATOM_CONFIG，无法确定配置文件位置")
	}
	raw, err := os.ReadFile(s.opts.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, config.ConnectorsConfig{}, fmt.Errorf("读取配置文件失败: %w", err)
	}
	c, err := config.ReadConnectors(raw)
	return raw, c, err
}

// editConnectors 改动连接器一节并存回配置文件。校验不过的一个字都不会写进去。
func (s *Server) editConnectors(change func(c *config.ConnectorsConfig) error) error {
	raw, c, err := s.readConnectors()
	if err != nil {
		return err
	}
	if err := change(&c); err != nil {
		return err
	}
	out, err := config.WriteConnectors(raw, c)
	if err != nil {
		return err
	}
	return s.writeConfig(string(out))
}

func (s *Server) connectorsPage(w http.ResponseWriter, r *http.Request, v *view) {
	_, c, err := s.readConnectors()
	if err != nil {
		v.Error = err.Error()
	}
	d := connectorsData{Calendar: c.Calendar, Mail: c.Mail, Holidays: c.Holidays, Weather: c.Weather, Search: c.Search}
	if metas, err := s.opts.Vault.List(r.Context()); err != nil {
		v.Error = err.Error()
	} else {
		for _, meta := range metas {
			if c.Calendar != nil && vault.HandlePrefix+meta.Label == c.Calendar.Password {
				d.CalendarKey = true
			}
			if c.Mail != nil && vault.HandlePrefix+meta.Label == c.Mail.Password {
				d.MailKey = true
			}
			if c.Weather != nil && vault.HandlePrefix+meta.Label == c.Weather.Key {
				d.WeatherKey = true
			}
			if c.Search != nil && vault.HandlePrefix+meta.Label == c.Search.Key {
				d.SearchKey = true
			}
		}
	}
	v.Data = d
	s.render(w, "connectors", v)
}

// sealConnectorPassword 把连接器的密码存进保险库，返回句柄。
// 原来就指着保险库里某一条的，换的是那一条，它原有的授权保留；否则存成与连接器同名的一条。
func (s *Server) sealConnectorPassword(ctx context.Context, handle, user, password, note string) (string, error) {
	label := user
	if handle != "" {
		label = strings.TrimPrefix(handle, vault.HandlePrefix)
	}
	tools := []string{user}
	metas, err := s.opts.Vault.List(ctx)
	if err != nil {
		return "", err
	}
	for _, meta := range metas {
		if meta.Label != label {
			continue
		}
		if meta.Note != "" {
			note = meta.Note
		}
		for _, t := range meta.AllowedTools {
			if t != user {
				tools = append(tools, t)
			}
		}
	}
	if err := s.opts.Vault.Seal(ctx, label, password, note, tools); err != nil {
		return "", err
	}
	return vault.HandlePrefix + label, nil
}

func (s *Server) calendarSave(w http.ResponseWriter, r *http.Request, _ *view) {
	// 密码原样存入，只去掉粘贴时常带上的结尾换行。
	password := strings.TrimRight(r.PostFormValue("password"), "\r\n")
	err := func() error {
		_, cur, err := s.readConnectors()
		if err != nil {
			return err
		}
		c := config.CalendarConfig{}
		if cur.Calendar != nil {
			c = *cur.Calendar
		}
		c.URL = strings.TrimSpace(r.PostFormValue("url"))
		c.Username = strings.TrimSpace(r.PostFormValue("username"))
		c.DefaultCalendar = strings.TrimSpace(r.PostFormValue("default_calendar"))
		if c.URL == "" || c.Username == "" {
			return errors.New("请填写服务器地址与账号")
		}
		if !strings.HasPrefix(c.URL, "https://") {
			return errors.New("服务器地址必须以 https:// 开头：密码将随每次请求发送")
		}
		if password != "" {
			if c.Password, err = s.sealConnectorPassword(r.Context(), c.Password, config.VaultUserCalendar, password, "日历账号的密码"); err != nil {
				return err
			}
		} else if c.Password == "" {
			return errors.New("首次配置日历时必须填写密码")
		}
		return s.editConnectors(func(all *config.ConnectorsConfig) error {
			all.Calendar = &c
			return nil
		})
	}()
	if err == nil {
		s.opts.Log.Info("日历连接器已在后台修改", "更换密码", password != "", "来自", r.RemoteAddr)
	}
	back(w, r, "/connectors", "日历配置已保存，重启服务后生效。", err)
}

func (s *Server) mailSave(w http.ResponseWriter, r *http.Request, _ *view) {
	password := strings.TrimRight(r.PostFormValue("password"), "\r\n")
	err := func() error {
		_, cur, err := s.readConnectors()
		if err != nil {
			return err
		}
		c := config.MailConfig{}
		if cur.Mail != nil {
			c = *cur.Mail
		}
		c.IMAP = strings.TrimSpace(r.PostFormValue("imap"))
		c.SMTP = strings.TrimSpace(r.PostFormValue("smtp"))
		c.Username = strings.TrimSpace(r.PostFormValue("username"))
		c.From = strings.TrimSpace(r.PostFormValue("from"))
		c.Name = strings.TrimSpace(r.PostFormValue("display_name"))
		if c.IMAP == "" || c.Username == "" {
			return errors.New("请填写收信服务器与账号")
		}
		if password != "" {
			if c.Password, err = s.sealConnectorPassword(r.Context(), c.Password, config.VaultUserMail, password, "邮箱账号的密码"); err != nil {
				return err
			}
		} else if c.Password == "" {
			return errors.New("首次配置邮件时必须填写密码")
		}
		return s.editConnectors(func(all *config.ConnectorsConfig) error {
			all.Mail = &c
			return nil
		})
	}()
	if err == nil {
		s.opts.Log.Info("邮件连接器已在后台修改", "更换密码", password != "", "来自", r.RemoteAddr)
	}
	back(w, r, "/connectors", "邮件配置已保存，重启服务后生效。", err)
}

func (s *Server) holidaysSave(w http.ResponseWriter, r *http.Request, _ *view) {
	url := strings.TrimSpace(r.PostFormValue("url"))
	err := s.editConnectors(func(all *config.ConnectorsConfig) error {
		if url == "" {
			return errors.New("请填写订阅地址")
		}
		all.Holidays = &config.HolidaysConfig{URL: url}
		return nil
	})
	back(w, r, "/connectors", "节假日订阅已保存，重启服务后生效。", err)
}

// weatherSave 保存和风天气的 API Host 与 API KEY。
func (s *Server) weatherSave(w http.ResponseWriter, r *http.Request, _ *view) {
	key := strings.TrimSpace(r.PostFormValue("key"))
	err := func() error {
		_, cur, err := s.readConnectors()
		if err != nil {
			return err
		}
		c := config.WeatherConfig{}
		if cur.Weather != nil {
			c = *cur.Weather
		}
		c.Host = weather.NormalizeHost(r.PostFormValue("host"))
		if err := weather.CheckHost(c.Host); err != nil {
			return errors.New("请填写 API Host，格式为主机名，如 abcxyz.qweatherapi.com")
		}
		if key != "" {
			if c.Key, err = s.sealConnectorPassword(r.Context(), c.Key, config.VaultUserWeather, key, "和风天气的 API KEY"); err != nil {
				return err
			}
		} else if c.Key == "" {
			return errors.New("首次配置天气时必须填写 API KEY")
		}
		return s.editConnectors(func(all *config.ConnectorsConfig) error {
			all.Weather = &c
			return nil
		})
	}()
	if err == nil {
		s.opts.Log.Info("天气连接器已在后台修改", "更换密钥", key != "", "来自", r.RemoteAddr)
	}
	back(w, r, "/connectors", "天气配置已保存，重启服务后生效。", err)
}

// searchSave 保存联网搜索用的 DeepSeek API Key。
func (s *Server) searchSave(w http.ResponseWriter, r *http.Request, _ *view) {
	key := strings.TrimSpace(r.PostFormValue("key"))
	err := func() error {
		_, cur, err := s.readConnectors()
		if err != nil {
			return err
		}
		c := config.SearchConfig{}
		if cur.Search != nil {
			c = *cur.Search
		}
		if key == "" {
			if c.Key == "" {
				return errors.New("请填写 API Key")
			}
			return errors.New("API Key 未修改")
		}
		if c.Key, err = s.sealConnectorPassword(r.Context(), c.Key, config.VaultUserSearch, key, "DeepSeek 搜索接口的 API Key"); err != nil {
			return err
		}
		return s.editConnectors(func(all *config.ConnectorsConfig) error {
			all.Search = &c
			return nil
		})
	}()
	if err == nil {
		s.opts.Log.Info("联网搜索密钥已在后台修改", "来自", r.RemoteAddr)
	}
	back(w, r, "/connectors", "联网搜索密钥已保存，重启服务后生效。", err)
}

// dropCredential 删掉一个配置对象独占的凭据：对象没了，它的密钥不该留在保险库里没人管。
//
// 只删"只授权给了这一个对象"的条目。用户另外把它授权给了别的对象或工具的，说明它还有别的用处，不动。
func (s *Server) dropCredential(ctx context.Context, handle, user string) {
	label, ok := strings.CutPrefix(handle, vault.HandlePrefix)
	if !ok {
		return
	}
	metas, err := s.opts.Vault.List(ctx)
	if err != nil {
		s.opts.Log.Error("清理凭据时读取环境变量失败", "条目", label, "err", err)
		return
	}
	for _, meta := range metas {
		if meta.Label != label || len(meta.AllowedTools) != 1 || meta.AllowedTools[0] != user {
			continue
		}
		if err := s.opts.Vault.Remove(ctx, label); err != nil {
			s.opts.Log.Error("清理凭据失败", "条目", label, "err", err)
			return
		}
		s.opts.Log.Info("凭据已随所属对象一并删除", "条目", label, "属于", user)
	}
}

// connectorDisable 停用一个连接器，它的密码一并从保险库里删除。
func (s *Server) connectorDisable(w http.ResponseWriter, r *http.Request, _ *view) {
	which := r.PostFormValue("which")
	names := map[string]string{"calendar": "日历", "mail": "邮件", "holidays": "节假日订阅", "weather": "天气", "search": "联网搜索密钥"}
	var handle, user string
	err := s.editConnectors(func(all *config.ConnectorsConfig) error {
		switch which {
		case "calendar":
			if all.Calendar != nil {
				handle, user = all.Calendar.Password, config.VaultUserCalendar
			}
			all.Calendar = nil
		case "mail":
			if all.Mail != nil {
				handle, user = all.Mail.Password, config.VaultUserMail
			}
			all.Mail = nil
		case "holidays":
			all.Holidays = nil
		case "weather":
			if all.Weather != nil {
				handle, user = all.Weather.Key, config.VaultUserWeather
			}
			all.Weather = nil
		case "search":
			if all.Search != nil {
				handle, user = all.Search.Key, config.VaultUserSearch
			}
			all.Search = nil
		default:
			return errors.New("连接器类型无效")
		}
		return nil
	})
	if err == nil {
		s.dropCredential(r.Context(), handle, user)
		s.opts.Log.Info("连接器已在后台停用", "连接器", which, "来自", r.RemoteAddr)
	}
	back(w, r, "/connectors", names[which]+"已停用，重启服务后生效。", err)
}

// connectorTest 用表单里当前填写的内容登录一次，确认服务器地址、账号、密码是否正确。
//
// 测的是页面上正填着的，不是已经保存的：填完先测、测通了再保存才是顺手的次序。
// 密码一栏留空时用保险库里已保存的那个。测试不保存任何东西。
func (s *Server) connectorTest(w http.ResponseWriter, r *http.Request, _ *view) {
	which := r.PostFormValue("which")
	typed := strings.TrimRight(r.PostFormValue("password"), "\r\n")
	if which == "weather" || which == "search" {
		typed = strings.TrimSpace(r.PostFormValue("key"))
	}
	result, err := func() (string, error) {
		_, saved, err := s.readConnectors()
		if err != nil {
			return "", err
		}
		// secret 定出这次测试用的密码：填了就用填的，没填就向保险库借已保存的。
		secret := func(handle, user string) (func(context.Context) (string, error), error) {
			if typed != "" {
				return func(context.Context) (string, error) { return typed, nil }, nil
			}
			if handle == "" {
				if which == "weather" || which == "search" {
					return nil, errors.New("请填写 API KEY")
				}
				return nil, errors.New("请填写密码")
			}
			return s.opts.Vault.Lend(handle, user)
		}
		// 真实的邮件和日历服务器在公网上，握手加登录比本地慢得多。
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		switch which {
		case "calendar":
			url := strings.TrimSpace(r.PostFormValue("url"))
			user := strings.TrimSpace(r.PostFormValue("username"))
			if url == "" || user == "" {
				return "", errors.New("请填写服务器地址与账号")
			}
			if !strings.HasPrefix(url, "https://") {
				return "", errors.New("服务器地址必须以 https:// 开头：密码将随每次请求发送")
			}
			handle := ""
			if saved.Calendar != nil {
				handle = saved.Calendar.Password
			}
			take, err := secret(handle, config.VaultUserCalendar)
			if err != nil {
				return "", err
			}
			list, err := (&calendar.Client{Endpoint: url, Username: user, Secret: take}).Calendars(ctx)
			if err != nil {
				return "", err
			}
			names := make([]string, 0, len(list))
			for _, c := range list {
				names = append(names, c.Name)
			}
			return fmt.Sprintf("日历连接成功，共 %d 个日历：%s", len(list), strings.Join(names, "、")), nil
		case "mail":
			imap := strings.TrimSpace(r.PostFormValue("imap"))
			user := strings.TrimSpace(r.PostFormValue("username"))
			if imap == "" || user == "" {
				return "", errors.New("请填写收信服务器与账号")
			}
			handle := ""
			box := &mail.Client{IMAPAddr: imap, Username: user}
			if saved.Mail != nil {
				handle = saved.Mail.Password
				// 加密方式页面上没有，沿用已保存的。
				box.IMAPSecurity = mail.Security(saved.Mail.IMAPSecurity)
			}
			if box.Secret, err = secret(handle, config.VaultUserMail); err != nil {
				return "", err
			}
			folders, err := box.Folders(ctx)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("邮件连接成功，共 %d 个文件夹。", len(folders)), nil
		case "weather":
			host := weather.NormalizeHost(r.PostFormValue("host"))
			if err := weather.CheckHost(host); err != nil {
				return "", errors.New("请填写 API Host，格式为主机名，如 abcxyz.qweatherapi.com")
			}
			handle := ""
			if saved.Weather != nil {
				handle = saved.Weather.Key
			}
			take, err := secret(handle, config.VaultUserWeather)
			if err != nil {
				return "", err
			}
			rep, err := (&weather.Client{Host: host, Secret: take}).Forecast(ctx, "北京", "", 1)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("天气连接成功：%s当前%s，%s。", rep.Place.Name, rep.Now.Text, rep.Now.Temp), nil
		case "search":
			handle := ""
			if saved.Search != nil {
				handle = saved.Search.Key
			}
			take, err := secret(handle, config.VaultUserSearch)
			if err != nil {
				return "", err
			}
			results, err := (&deepseek.Client{BaseURL: s.searchBase, Secret: take}).Search(ctx, "DeepSeek", 1)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("联网搜索连接成功，返回 %d 条结果。", len(results)), nil
		}
		return "", errors.New("连接器类型无效")
	}()
	back(w, r, "/connectors", result, err)
}
