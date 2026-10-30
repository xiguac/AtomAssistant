package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/connector/calendar"
	"github.com/xiguac/atomassistant/internal/connector/holiday"
	"github.com/xiguac/atomassistant/internal/connector/mail"
)

// 同一类数据源接了好几个账号（两个日历账号、三个邮箱）时，对助手仍然只是一套日历工具、一套邮件工具。
//
// 只有一个账号时原样交给它，名字、编号都不加前缀，和只接一个时完全一样。
// 有好几个时：日历和文件夹写成「账号/名称」，日程和邮件的编号写成「账号|编号」，
// 靠前缀找回是哪个账号的；不指明账号的查询把所有账号都查一遍，写入不指明时落到第一个（默认）账号。

const (
	accountSep = "/"
	idSep      = "|"
)

// Account 是数据源里的一个账号：名字、用途标签，以及它的服务。
type Account[S any] struct {
	Name string
	Tags []string
	Svc  S
}

// AccountsNote 写给模型看：接了哪几个账号、各自的用途。只有一个账号时为空。
func AccountsNote[S any](kind string, accounts []Account[S]) string {
	if len(accounts) < 2 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "用户接了 %d 个%s账号：", len(accounts), kind)
	for i, a := range accounts {
		if i > 0 {
			b.WriteString("；")
		}
		fmt.Fprintf(&b, "「%s」", a.Name)
		if i == 0 {
			b.WriteString("（默认）")
		}
		if len(a.Tags) > 0 {
			fmt.Fprintf(&b, "用途：%s", strings.Join(a.Tags, "、"))
		}
	}
	b.WriteString("。按用户说的用途选账号；用户没说时查询类不指定账号会把所有账号一起查。")
	return b.String()
}

// splitAccount 把「账号<sep>其余」拆开。不带已知账号前缀时 ok 为假。
func splitAccount[S any](accounts []Account[S], s, sep string) (Account[S], string, bool) {
	name, rest, found := strings.Cut(s, sep)
	if !found {
		return Account[S]{}, s, false
	}
	for _, a := range accounts {
		if a.Name == name {
			return a, rest, true
		}
	}
	return Account[S]{}, s, false
}

// ---- 日历 ----

// Calendars 把几个日历账号合成一个。只有一个账号时直接返回它。
func Calendars(accounts []Account[CalendarService]) CalendarService {
	if len(accounts) == 1 {
		return accounts[0].Svc
	}
	return multiCalendar(accounts)
}

type multiCalendar []Account[CalendarService]

func (m multiCalendar) Calendars(ctx context.Context) ([]calendar.Calendar, error) {
	var out []calendar.Calendar
	for _, a := range m {
		cals, err := a.Svc.Calendars(ctx)
		if err != nil {
			return nil, fmt.Errorf("日历账号「%s」: %w", a.Name, err)
		}
		for _, c := range cals {
			c.Name = a.Name + accountSep + c.Name
			out = append(out, c)
		}
	}
	return out, nil
}

func (m multiCalendar) Events(ctx context.Context, name string, from, to time.Time) ([]calendar.Event, bool, error) {
	targets, cal := []Account[CalendarService](m), name
	if a, rest, ok := splitAccount(m, name, accountSep); ok {
		targets, cal = []Account[CalendarService]{a}, rest
	} else if name != "" {
		// 只写了账号名：查这个账号下的全部日历。
		for _, a := range m {
			if a.Name == name {
				targets, cal = []Account[CalendarService]{a}, ""
			}
		}
	}
	var out []calendar.Event
	more := false
	for _, a := range targets {
		evs, truncated, err := a.Svc.Events(ctx, cal, from, to)
		if err != nil {
			return nil, false, fmt.Errorf("日历账号「%s」: %w", a.Name, err)
		}
		more = more || truncated
		for _, e := range evs {
			e.ID = a.Name + idSep + e.ID
			e.Calendar = a.Name + accountSep + e.Calendar
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, more, nil
}

func (m multiCalendar) Create(ctx context.Context, d calendar.Draft, key string) (calendar.Event, error) {
	a := m[0]
	if b, rest, ok := splitAccount(m, d.Calendar, accountSep); ok {
		a, d.Calendar = b, rest
	}
	e, err := a.Svc.Create(ctx, d, key)
	if err != nil {
		return e, err
	}
	e.ID = a.Name + idSep + e.ID
	e.Calendar = a.Name + accountSep + e.Calendar
	return e, nil
}

func (m multiCalendar) Update(ctx context.Context, id string, p calendar.Patch) (calendar.Event, error) {
	a, rest, ok := splitAccount(m, id, idSep)
	if !ok {
		return calendar.Event{}, fmt.Errorf("%w: 日程编号 %q 不完整，要原样照抄 %s 结果里的 id", calendar.ErrInvalid, id, ToolCalendarEvents)
	}
	e, err := a.Svc.Update(ctx, rest, p)
	if err != nil {
		return e, err
	}
	e.ID = a.Name + idSep + e.ID
	e.Calendar = a.Name + accountSep + e.Calendar
	return e, nil
}

func (m multiCalendar) Delete(ctx context.Context, id string) error {
	a, rest, ok := splitAccount(m, id, idSep)
	if !ok {
		return fmt.Errorf("%w: 日程编号 %q 不完整，要原样照抄 %s 结果里的 id", calendar.ErrInvalid, id, ToolCalendarEvents)
	}
	return a.Svc.Delete(ctx, rest)
}

// ---- 邮件 ----

// AccountSender 是能指定从哪个账号发信的邮件服务。只有接了好几个邮箱时才有。
type AccountSender interface {
	SendAs(ctx context.Context, account string, o mail.Outgoing) (mail.Sent, error)
}

// Mailboxes 把几个邮箱合成一个。只有一个邮箱时直接返回它。
func Mailboxes(accounts []Account[MailService]) MailService {
	if len(accounts) == 1 {
		return accounts[0].Svc
	}
	return multiMail(accounts)
}

type multiMail []Account[MailService]

func (m multiMail) Folders(ctx context.Context) ([]mail.Folder, error) {
	var out []mail.Folder
	for _, a := range m {
		fs, err := a.Svc.Folders(ctx)
		if err != nil {
			return nil, fmt.Errorf("邮箱「%s」: %w", a.Name, err)
		}
		for _, f := range fs {
			f.Name = a.Name + accountSep + f.Name
			out = append(out, f)
		}
	}
	return out, nil
}

func (m multiMail) Search(ctx context.Context, q mail.Query) ([]mail.Summary, error) {
	targets := []Account[MailService](m)
	if a, rest, ok := splitAccount(m, q.Folder, accountSep); ok {
		targets, q.Folder = []Account[MailService]{a}, rest
	} else {
		for _, a := range m {
			if a.Name == q.Folder {
				targets, q.Folder = []Account[MailService]{a}, ""
			}
		}
	}
	var out []mail.Summary
	for _, a := range targets {
		list, err := a.Svc.Search(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("邮箱「%s」: %w", a.Name, err)
		}
		for _, s := range list {
			s.ID = a.Name + idSep + s.ID
			out = append(out, s)
		}
	}
	// 几个邮箱合在一起时最新的在前，条数仍按要求的上限。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m multiMail) account(id string) (Account[MailService], string, error) {
	a, rest, ok := splitAccount(m, id, idSep)
	if !ok {
		return a, "", fmt.Errorf("%w: 邮件编号 %q 不完整，要原样照抄 %s 结果里的 id", mail.ErrInvalid, id, ToolMailSearch)
	}
	return a, rest, nil
}

func (m multiMail) Read(ctx context.Context, id string, offset int) (mail.Message, error) {
	a, rest, err := m.account(id)
	if err != nil {
		return mail.Message{}, err
	}
	msg, err := a.Svc.Read(ctx, rest, offset)
	if err != nil {
		return msg, err
	}
	msg.ID = a.Name + idSep + msg.ID
	return msg, nil
}

func (m multiMail) Mark(ctx context.Context, id string, read, flagged *bool) error {
	a, rest, err := m.account(id)
	if err != nil {
		return err
	}
	return a.Svc.Mark(ctx, rest, read, flagged)
}

func (m multiMail) Move(ctx context.Context, id, folder string) (string, error) {
	a, rest, err := m.account(id)
	if err != nil {
		return "", err
	}
	// 只能在同一个邮箱里移动；目标文件夹带着账号前缀时去掉。
	if b, f, ok := splitAccount(m, folder, accountSep); ok {
		if b.Name != a.Name {
			return "", fmt.Errorf("%w: 邮件不能从邮箱「%s」移到邮箱「%s」", mail.ErrInvalid, a.Name, b.Name)
		}
		folder = f
	}
	newID, err := a.Svc.Move(ctx, rest, folder)
	if err != nil {
		return "", err
	}
	return a.Name + idSep + newID, nil
}

// Send 不指明账号：回复某封信时从收到那封信的邮箱回，否则用默认邮箱。
func (m multiMail) Send(ctx context.Context, o mail.Outgoing) (mail.Sent, error) {
	return m.SendAs(ctx, "", o)
}

func (m multiMail) SendAs(ctx context.Context, account string, o mail.Outgoing) (mail.Sent, error) {
	a := m[0]
	if o.ReplyTo != "" {
		b, rest, err := m.account(o.ReplyTo)
		if err != nil {
			return mail.Sent{}, err
		}
		if account != "" && account != b.Name {
			return mail.Sent{}, fmt.Errorf("%w: 回复邮箱「%s」里的信要从这个邮箱发，不能用「%s」", mail.ErrInvalid, b.Name, account)
		}
		a, o.ReplyTo = b, rest
	} else if account != "" {
		found := false
		for _, b := range m {
			if b.Name == account {
				a, found = b, true
			}
		}
		if !found {
			return mail.Sent{}, fmt.Errorf("%w: 没有名为「%s」的邮箱", mail.ErrInvalid, account)
		}
	}
	return a.Svc.Send(ctx, o)
}

// ---- 节假日 ----

// Holidays 把几个节假日订阅合成一个：各订阅的安排合在一起，按日期排。只有一个时直接返回它。
func Holidays(feeds []HolidayService) HolidayService {
	if len(feeds) == 1 {
		return feeds[0]
	}
	return multiHolidays(feeds)
}

type multiHolidays []HolidayService

func (m multiHolidays) Between(ctx context.Context, from, to time.Time) ([]holiday.Entry, error) {
	var out []holiday.Entry
	for _, f := range m {
		list, err := f.Between(ctx, from, to)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}
