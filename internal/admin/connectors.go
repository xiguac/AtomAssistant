package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/connector/calendar"
	"github.com/xiguac/atomassistant/internal/connector/deepseek"
	"github.com/xiguac/atomassistant/internal/connector/mail"
	"github.com/xiguac/atomassistant/internal/connector/weather"
	"github.com/xiguac/atomassistant/internal/vault"
)

// 「数据源」页：日历（CalDAV）、邮箱（IMAP/SMTP）、节假日订阅可以各加好几个，各有名字和用途标签；
// 天气（和风天气）与联网搜索（DeepSeek 搜索接口）各一把密钥。
//
// 这一页改的是配置文件里 connectors 一节，保存时一律写成列表。密码照旧只进保险库，配置里留的是句柄；
// 每个账号各存一条，删掉其中一个不影响别的。

type connectorsData struct {
	Calendars []calendarRow
	Mailboxes []mailRow
	Holidays  []config.HolidaysConfig
	Weather   *config.WeatherConfig
	Search    *config.SearchConfig
	// WeatherKey 与 SearchKey 表示保险库里有没有对应的密钥。
	WeatherKey, SearchKey bool
}

// calendarRow、mailRow 是页面上的一个账号，Key 表示保险库里有它的密码。
type calendarRow struct {
	config.CalendarConfig
	Key bool
}

type mailRow struct {
	config.MailConfig
	Key bool
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
	d := connectorsData{Holidays: c.HolidayList(), Weather: c.Weather, Search: c.Search}
	stored := map[string]bool{}
	if metas, err := s.opts.Vault.List(r.Context()); err != nil {
		v.Error = err.Error()
	} else {
		for _, meta := range metas {
			stored[vault.HandlePrefix+meta.Label] = true
		}
	}
	for _, cal := range c.CalendarList() {
		d.Calendars = append(d.Calendars, calendarRow{cal, stored[cal.Password]})
	}
	for _, m := range c.MailboxList() {
		d.Mailboxes = append(d.Mailboxes, mailRow{m, stored[m.Password]})
	}
	d.WeatherKey = c.Weather != nil && stored[c.Weather.Key]
	d.SearchKey = c.Search != nil && stored[c.Search.Key]
	v.Data = d
	s.render(w, "connectors", v)
}

// sealConnectorPassword 把连接器的密码存进保险库，返回句柄。
// 原来就指着保险库里某一条的，换的是那一条，它原有的授权保留；否则新存一条，名字是 label。
func (s *Server) sealConnectorPassword(ctx context.Context, handle, label, user, password, note string) (string, error) {
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

// accountLabel 是一个账号的密码在保险库里的名字：同类账号各存一条，靠名字区分。
// 账号名里有保险库名字不收的字符时，用名字的摘要代替。
func accountLabel(user, name string) string {
	if label := user + "." + name; vault.ValidLabel(label) {
		return label
	}
	sum := sha256.Sum256([]byte(name))
	return user + "." + hex.EncodeToString(sum[:6])
}

// splitTags 把页面上填的用途标签拆开：中英文逗号、顿号、分号、空白都算分隔。
func splitTags(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",，、;；", r)
	}) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// upsertAccount 在列表里按原名字（orig）找到那一项换掉；orig 为空是新增。名字不能和别的重复。
func upsertAccount[T any](list []T, nameOf func(T) string, orig string, item T) ([]T, error) {
	name := nameOf(item)
	if err := config.ValidAccountName(name); err != nil {
		return nil, err
	}
	at := -1
	for i, x := range list {
		switch {
		case orig != "" && nameOf(x) == orig:
			at = i
		case nameOf(x) == name:
			return nil, fmt.Errorf("已有一个叫「%s」的，名称不能重复", name)
		}
	}
	if orig != "" && at < 0 {
		return nil, fmt.Errorf("没有找到「%s」，可能已被删除，请刷新页面", orig)
	}
	if at < 0 {
		return append(list, item), nil
	}
	list[at] = item
	return list, nil
}

// withLists 把 cur 里的日历、邮箱、节假日列表换进 all，天气和搜索保持 all 里的。
func withLists(all, cur config.ConnectorsConfig) config.ConnectorsConfig {
	all.Normalize()
	all.Calendars, all.Mailboxes, all.HolidayFeeds = cur.Calendars, cur.Mailboxes, cur.HolidayFeeds
	return all
}

// trimSecret 去掉粘贴密码时常带上的结尾换行，其余原样保留。
func trimSecret(s string) string { return strings.TrimRight(s, "\r\n") }

func (s *Server) calendarSave(w http.ResponseWriter, r *http.Request, _ *view) {
	password := trimSecret(r.PostFormValue("password"))
	orig := strings.TrimSpace(r.PostFormValue("orig"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	err := func() error {
		_, cur, err := s.readConnectors()
		if err != nil {
			return err
		}
		cur.Normalize()
		c := config.CalendarConfig{}
		for _, x := range cur.Calendars {
			if orig != "" && x.Name == orig {
				c = x
			}
		}
		c.Name, c.Tags = name, splitTags(r.PostFormValue("tags"))
		c.URL = strings.TrimSpace(r.PostFormValue("url"))
		c.Username = strings.TrimSpace(r.PostFormValue("username"))
		c.DefaultCalendar = strings.TrimSpace(r.PostFormValue("default_calendar"))
		if c.URL == "" || c.Username == "" {
			return errors.New("请填写服务器地址与账号")
		}
		if !strings.HasPrefix(c.URL, "https://") {
			return errors.New("服务器地址必须以 https:// 开头：密码将随每次请求发送")
		}
		// 名字先查过再存密码：重名之类被拒绝时，不能已经把别人的密码换掉了。
		if _, err := upsertAccount(append([]config.CalendarConfig(nil), cur.Calendars...), func(x config.CalendarConfig) string { return x.Name }, orig, c); err != nil {
			return err
		}
		if password != "" {
			if c.Password, err = s.sealConnectorPassword(r.Context(), c.Password, accountLabel(config.VaultUserCalendar, name),
				config.VaultUserCalendar, password, "日历「"+name+"」的密码"); err != nil {
				return err
			}
		} else if c.Password == "" {
			return errors.New("新增日历时必须填写密码")
		}
		cur.Calendars, _ = upsertAccount(cur.Calendars, func(x config.CalendarConfig) string { return x.Name }, orig, c)
		return s.editConnectors(func(all *config.ConnectorsConfig) error {
			*all = withLists(*all, cur)
			return nil
		})
	}()
	if err == nil {
		s.opts.Log.Info("日历数据源已在后台修改", "名称", name, "更换密码", password != "", "来自", r.RemoteAddr)
	}
	s.backApplied(w, r, "/connectors", "日历「"+name+"」已保存。", err)
}

func (s *Server) mailSave(w http.ResponseWriter, r *http.Request, _ *view) {
	password := trimSecret(r.PostFormValue("password"))
	orig := strings.TrimSpace(r.PostFormValue("orig"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	err := func() error {
		_, cur, err := s.readConnectors()
		if err != nil {
			return err
		}
		cur.Normalize()
		c := config.MailConfig{}
		for _, x := range cur.Mailboxes {
			if orig != "" && x.Label == orig {
				c = x
			}
		}
		c.Label, c.Tags = name, splitTags(r.PostFormValue("tags"))
		c.IMAP = strings.TrimSpace(r.PostFormValue("imap"))
		c.SMTP = strings.TrimSpace(r.PostFormValue("smtp"))
		c.Username = strings.TrimSpace(r.PostFormValue("username"))
		c.From = strings.TrimSpace(r.PostFormValue("from"))
		c.Name = strings.TrimSpace(r.PostFormValue("display_name"))
		if c.IMAP == "" || c.Username == "" {
			return errors.New("请填写收信服务器与账号")
		}
		// 名字先查过再存密码：重名之类被拒绝时，不能已经把别人的密码换掉了。
		if _, err := upsertAccount(append([]config.MailConfig(nil), cur.Mailboxes...), func(x config.MailConfig) string { return x.Label }, orig, c); err != nil {
			return err
		}
		if password != "" {
			if c.Password, err = s.sealConnectorPassword(r.Context(), c.Password, accountLabel(config.VaultUserMail, name),
				config.VaultUserMail, password, "邮箱「"+name+"」的密码"); err != nil {
				return err
			}
		} else if c.Password == "" {
			return errors.New("新增邮箱时必须填写密码")
		}
		cur.Mailboxes, _ = upsertAccount(cur.Mailboxes, func(x config.MailConfig) string { return x.Label }, orig, c)
		return s.editConnectors(func(all *config.ConnectorsConfig) error {
			*all = withLists(*all, cur)
			return nil
		})
	}()
	if err == nil {
		s.opts.Log.Info("邮件数据源已在后台修改", "名称", name, "更换密码", password != "", "来自", r.RemoteAddr)
	}
	s.backApplied(w, r, "/connectors", "邮箱「"+name+"」已保存。", err)
}

func (s *Server) holidaysSave(w http.ResponseWriter, r *http.Request, _ *view) {
	orig := strings.TrimSpace(r.PostFormValue("orig"))
	item := config.HolidaysConfig{Name: strings.TrimSpace(r.PostFormValue("name")), Tags: splitTags(r.PostFormValue("tags")),
		URL: strings.TrimSpace(r.PostFormValue("url"))}
	err := func() error {
		if item.URL == "" {
			return errors.New("请填写订阅地址")
		}
		_, cur, err := s.readConnectors()
		if err != nil {
			return err
		}
		cur.Normalize()
		if cur.HolidayFeeds, err = upsertAccount(cur.HolidayFeeds, func(x config.HolidaysConfig) string { return x.Name }, orig, item); err != nil {
			return err
		}
		return s.editConnectors(func(all *config.ConnectorsConfig) error {
			*all = withLists(*all, cur)
			return nil
		})
	}()
	s.backApplied(w, r, "/connectors", "节假日订阅「"+item.Name+"」已保存。", err)
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
			if c.Key, err = s.sealConnectorPassword(r.Context(), c.Key, config.VaultUserWeather, config.VaultUserWeather, key, "和风天气的 API KEY"); err != nil {
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
		s.opts.Log.Info("天气数据源已在后台修改", "更换密钥", key != "", "来自", r.RemoteAddr)
	}
	s.backApplied(w, r, "/connectors", "天气配置已保存。", err)
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
		if c.Key, err = s.sealConnectorPassword(r.Context(), c.Key, config.VaultUserSearch, config.VaultUserSearch, key, "DeepSeek 搜索接口的 API Key"); err != nil {
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
	s.backApplied(w, r, "/connectors", "联网搜索密钥已保存。", err)
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

// connectorDisable 删除一个数据源（日历、邮箱、节假日订阅按名字），它的密码一并从保险库里删除。
func (s *Server) connectorDisable(w http.ResponseWriter, r *http.Request, _ *view) {
	which, name := r.PostFormValue("which"), strings.TrimSpace(r.PostFormValue("name"))
	names := map[string]string{"calendar": "日历", "mail": "邮箱", "holidays": "节假日订阅", "weather": "天气", "search": "联网搜索密钥"}
	var handle, user string
	err := s.editConnectors(func(all *config.ConnectorsConfig) error {
		all.Normalize()
		found := false
		switch which {
		case "calendar":
			for i, c := range all.Calendars {
				if c.Name == name && !found {
					handle, user, found = c.Password, config.VaultUserCalendar, true
					all.Calendars = append(all.Calendars[:i:i], all.Calendars[i+1:]...)
				}
			}
		case "mail":
			for i, c := range all.Mailboxes {
				if c.Label == name && !found {
					handle, user, found = c.Password, config.VaultUserMail, true
					all.Mailboxes = append(all.Mailboxes[:i:i], all.Mailboxes[i+1:]...)
				}
			}
		case "holidays":
			for i, c := range all.HolidayFeeds {
				if c.Name == name && !found {
					found = true
					all.HolidayFeeds = append(all.HolidayFeeds[:i:i], all.HolidayFeeds[i+1:]...)
				}
			}
		case "weather":
			if all.Weather != nil {
				handle, user, found = all.Weather.Key, config.VaultUserWeather, true
			}
			all.Weather = nil
		case "search":
			if all.Search != nil {
				handle, user, found = all.Search.Key, config.VaultUserSearch, true
			}
			all.Search = nil
		default:
			return errors.New("数据源类型无效")
		}
		if !found {
			return fmt.Errorf("没有找到这个%s，可能已被删除，请刷新页面", names[which])
		}
		return nil
	})
	if err == nil {
		s.dropCredential(r.Context(), handle, user)
		s.opts.Log.Info("数据源已在后台删除", "数据源", which, "名称", name, "来自", r.RemoteAddr)
	}
	what := names[which]
	if name != "" {
		what += "「" + name + "」"
	}
	s.backApplied(w, r, "/connectors", what+"已删除。", err)
}

// connectorTest 用表单里当前填写的内容登录一次，确认服务器地址、账号、密码是否正确。
//
// 测的是页面上正填着的，不是已经保存的：填完先测、测通了再保存才是顺手的次序。
// 密码一栏留空时用保险库里已保存的那个。测试不保存任何东西。
func (s *Server) connectorTest(w http.ResponseWriter, r *http.Request, _ *view) {
	which := r.PostFormValue("which")
	typed := strings.TrimRight(r.PostFormValue("password"), "\r\n")
	orig := strings.TrimSpace(r.PostFormValue("orig"))
	// options 是读到的日历列表，页面用它填「默认日历」的下拉框。
	var options []string
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
			for _, c := range saved.CalendarList() {
				if c.Name == orig {
					handle = c.Password
				}
			}
			take, err := secret(handle, config.VaultUserCalendar)
			if err != nil {
				return "", err
			}
			list, err := (&calendar.Client{Endpoint: url, Username: user, Secret: take}).Calendars(ctx)
			if err != nil {
				return "", err
			}
			options = make([]string, 0, len(list))
			for _, c := range list {
				options = append(options, c.Name)
			}
			return fmt.Sprintf("日历连接成功，共 %d 个日历：%s", len(list), strings.Join(options, "、")), nil
		case "mail":
			imap := strings.TrimSpace(r.PostFormValue("imap"))
			user := strings.TrimSpace(r.PostFormValue("username"))
			if imap == "" || user == "" {
				return "", errors.New("请填写收信服务器与账号")
			}
			handle := ""
			box := &mail.Client{IMAPAddr: imap, Username: user}
			for _, c := range saved.MailboxList() {
				if c.Label == orig {
					handle = c.Password
					// 加密方式页面上没有，沿用已保存的。
					box.IMAPSecurity = mail.Security(c.IMAPSecurity)
				}
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
		return "", errors.New("数据源类型无效")
	}()
	if options != nil && r.Header.Get("X-Requested-With") == "fetch" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": err == nil, "message": result, "options": options})
		return
	}
	back(w, r, "/connectors", result, err)
}
