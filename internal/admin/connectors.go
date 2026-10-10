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
	"github.com/xiguac/atomhelper/internal/vault"
)

// 「连接器」页：日历（CalDAV）、邮件（IMAP/SMTP）的账号与密码，以及节假日订阅。
//
// 这一页改的是配置文件里 connectors 一节。密码照旧只进保险库，配置里留的是句柄；
// 日历和邮件各存一条、各自授权，停用其中一个不影响另一个。

type connectorsData struct {
	Calendar *config.CalendarConfig
	Mail     *config.MailConfig
	Holidays *config.HolidaysConfig
	// CalendarKey 与 MailKey 表示保险库里有没有对应的密码。
	CalendarKey, MailKey bool
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
	d := connectorsData{Calendar: c.Calendar, Mail: c.Mail, Holidays: c.Holidays}
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

// connectorDisable 停用一个连接器。保险库里的密码不跟着删。
func (s *Server) connectorDisable(w http.ResponseWriter, r *http.Request, _ *view) {
	which := r.PostFormValue("which")
	names := map[string]string{"calendar": "日历", "mail": "邮件", "holidays": "节假日订阅"}
	err := s.editConnectors(func(all *config.ConnectorsConfig) error {
		switch which {
		case "calendar":
			all.Calendar = nil
		case "mail":
			all.Mail = nil
		case "holidays":
			all.Holidays = nil
		default:
			return errors.New("连接器类型无效")
		}
		return nil
	})
	if err == nil {
		s.opts.Log.Info("连接器已在后台停用", "连接器", which, "来自", r.RemoteAddr)
	}
	back(w, r, "/connectors", names[which]+"已停用，重启服务后生效。保险库中的密码仍保留，可在「保险库」页删除。", err)
}

// connectorTest 用已保存的配置登录一次，确认服务器地址、账号、密码是否正确。
func (s *Server) connectorTest(w http.ResponseWriter, r *http.Request, _ *view) {
	which := r.PostFormValue("which")
	result, err := func() (string, error) {
		file, err := config.LoadFile(s.opts.ConfigPath)
		if err != nil {
			return "", err
		}
		if file == nil {
			return "", errors.New("配置文件不存在")
		}
		// 真实的邮件和日历服务器在公网上，握手加登录比本地慢得多。
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		switch which {
		case "calendar":
			cal, err := file.Calendar(s.opts.Vault)
			if err != nil {
				return "", err
			}
			if cal == nil {
				return "", errors.New("尚未配置日历")
			}
			list, err := cal.Calendars(ctx)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("日历连接成功，共 %d 个日历。", len(list)), nil
		case "mail":
			box, err := file.Mail(s.opts.Vault)
			if err != nil {
				return "", err
			}
			if box == nil {
				return "", errors.New("尚未配置邮件")
			}
			folders, err := box.Folders(ctx)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("邮件连接成功，共 %d 个文件夹。", len(folders)), nil
		}
		return "", errors.New("连接器类型无效")
	}()
	back(w, r, "/connectors", result, err)
}
