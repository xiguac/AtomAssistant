package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/connector/calendar"
	"github.com/xiguac/atomassistant/internal/connector/mail"
)

// acctCal 是一个日历账号：有哪些日历、每个日历有哪些日程，记下改了哪一条。
type acctCal struct {
	cals    []string
	events  []calendar.Event
	created []calendar.Draft
	updated []string
	deleted []string
}

func (f *acctCal) Calendars(context.Context) ([]calendar.Calendar, error) {
	var out []calendar.Calendar
	for _, c := range f.cals {
		out = append(out, calendar.Calendar{Name: c})
	}
	return out, nil
}

func (f *acctCal) Events(_ context.Context, name string, _, _ time.Time) ([]calendar.Event, bool, error) {
	var out []calendar.Event
	for _, e := range f.events {
		if name == "" || e.Calendar == name {
			out = append(out, e)
		}
	}
	return out, false, nil
}

func (f *acctCal) Create(_ context.Context, d calendar.Draft, _ string) (calendar.Event, error) {
	f.created = append(f.created, d)
	return calendar.Event{ID: "/new.ics", Calendar: d.Calendar, Title: d.Title}, nil
}

func (f *acctCal) Update(_ context.Context, id string, _ calendar.Patch) (calendar.Event, error) {
	f.updated = append(f.updated, id)
	return calendar.Event{ID: id, Calendar: "工作"}, nil
}

func (f *acctCal) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestCalendars_一个账号原样不加前缀(t *testing.T) {
	one := &acctCal{cals: []string{"个人"}}
	if svc := Calendars([]Account[CalendarService]{{Name: "icloud", Svc: one}}); svc != CalendarService(one) {
		t.Fatal("只有一个账号时应原样返回它")
	}
	if note := AccountsNote("日历", []Account[CalendarService]{{Name: "icloud", Svc: one}}); note != "" {
		t.Fatalf("只有一个账号时不需要说明: %q", note)
	}
}

func TestCalendars_多个账号按前缀分派(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	icloud := &acctCal{cals: []string{"个人", "工作"}, events: []calendar.Event{{ID: "/a.ics", Calendar: "工作", Start: at.Add(time.Hour)}}}
	company := &acctCal{cals: []string{"日程"}, events: []calendar.Event{{ID: "/a.ics", Calendar: "日程", Start: at}}}
	accts := []Account[CalendarService]{{Name: "icloud", Tags: []string{"个人"}, Svc: icloud}, {Name: "公司", Tags: []string{"工作"}, Svc: company}}
	svc := Calendars(accts)

	cals, _ := svc.Calendars(ctx)
	if len(cals) != 3 || cals[0].Name != "icloud/个人" || cals[2].Name != "公司/日程" {
		t.Fatalf("日历名应带账号前缀: %+v", cals)
	}
	// 不指定日历：两个账号都查，编号带账号前缀，按时间排；同样的服务器路径不会混。
	evs, _, err := svc.Events(ctx, "", at, at.Add(24*time.Hour))
	if err != nil || len(evs) != 2 || evs[0].ID != "公司|/a.ics" || evs[1].ID != "icloud|/a.ics" || evs[1].Calendar != "icloud/工作" {
		t.Fatalf("应合并两个账号的日程: %+v %v", evs, err)
	}
	// 只写账号名：查这个账号的全部日历。
	if evs, _, _ := svc.Events(ctx, "公司", at, at.Add(24*time.Hour)); len(evs) != 1 || evs[0].ID != "公司|/a.ics" {
		t.Fatalf("只写账号名应只查这个账号: %+v", evs)
	}
	// 新建：写明「账号/日历」就写到那里，不写落到默认（第一个）账号。
	if e, err := svc.Create(ctx, calendar.Draft{Calendar: "公司/日程", Title: "周会"}, "k"); err != nil || e.ID != "公司|/new.ics" || company.created[0].Calendar != "日程" {
		t.Fatalf("新建应写到指定账号: %+v %v %+v", e, err, company.created)
	}
	if _, err := svc.Create(ctx, calendar.Draft{Title: "体检"}, "k"); err != nil || len(icloud.created) != 1 {
		t.Fatalf("不指定时应写到默认账号: %v", err)
	}
	// 改和删按编号前缀找账号；编号缺了前缀要明说。
	if _, err := svc.Update(ctx, "icloud|/a.ics", calendar.Patch{}); err != nil || icloud.updated[0] != "/a.ics" {
		t.Fatalf("修改应交给对应账号: %v %+v", err, icloud.updated)
	}
	if err := svc.Delete(ctx, "公司|/a.ics"); err != nil || company.deleted[0] != "/a.ics" {
		t.Fatalf("删除应交给对应账号: %v", err)
	}
	if err := svc.Delete(ctx, "/a.ics"); err == nil || !strings.Contains(err.Error(), "原样照抄") {
		t.Fatalf("缺了账号前缀的编号应被拒绝: %v", err)
	}
	if note := AccountsNote("日历", accts); !strings.Contains(note, "「icloud」（默认）用途：个人") || !strings.Contains(note, "「公司」用途：工作") {
		t.Fatalf("给模型的说明应列出账号和用途: %q", note)
	}
}

type acctMail struct {
	list []mail.Summary
	sent []mail.Outgoing
}

func (f *acctMail) Folders(context.Context) ([]mail.Folder, error) {
	return []mail.Folder{{Name: "INBOX"}}, nil
}
func (f *acctMail) Search(context.Context, mail.Query) ([]mail.Summary, error) { return f.list, nil }
func (f *acctMail) Read(_ context.Context, id string, _ int) (mail.Message, error) {
	return mail.Message{Summary: mail.Summary{ID: id}}, nil
}
func (f *acctMail) Mark(context.Context, string, *bool, *bool) error { return nil }
func (f *acctMail) Move(_ context.Context, id, _ string) (string, error) {
	return id + "-moved", nil
}
func (f *acctMail) Send(_ context.Context, o mail.Outgoing) (mail.Sent, error) {
	f.sent = append(f.sent, o)
	return mail.Sent{}, nil
}

func TestMailboxes_多个邮箱(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	work := &acctMail{list: []mail.Summary{{ID: "1@INBOX", Date: day}}}
	home := &acctMail{list: []mail.Summary{{ID: "1@INBOX", Date: day.Add(time.Hour)}}}
	svc := Mailboxes([]Account[MailService]{{Name: "工作", Svc: work}, {Name: "个人", Svc: home}})

	if fs, _ := svc.Folders(ctx); len(fs) != 2 || fs[1].Name != "个人/INBOX" {
		t.Fatalf("文件夹应带邮箱前缀: %+v", fs)
	}
	list, err := svc.Search(ctx, mail.Query{})
	if err != nil || len(list) != 2 || list[0].ID != "个人|1@INBOX" {
		t.Fatalf("不指定邮箱时两个都搜，最新的在前: %+v %v", list, err)
	}
	if list, _ := svc.Search(ctx, mail.Query{Folder: "工作/INBOX"}); len(list) != 1 || list[0].ID != "工作|1@INBOX" {
		t.Fatalf("指定邮箱时只搜它: %+v", list)
	}
	if msg, err := svc.Read(ctx, "个人|1@INBOX", 0); err != nil || msg.ID != "个人|1@INBOX" {
		t.Fatalf("读信应交给对应邮箱，编号保持带前缀: %+v %v", msg, err)
	}
	if _, err := svc.Move(ctx, "工作|1@INBOX", "个人/Archive"); err == nil {
		t.Fatal("不能把信从一个邮箱移到另一个邮箱")
	}
	if id, err := svc.Move(ctx, "工作|1@INBOX", "工作/Archive"); err != nil || id != "工作|1@INBOX-moved" {
		t.Fatalf("同一个邮箱里移动应可以: %q %v", id, err)
	}
	// 发信：回复时从收信的邮箱回；指定了就用指定的；都没有用默认的。
	sender := svc.(AccountSender)
	if _, err := sender.SendAs(ctx, "", mail.Outgoing{Body: "好的", ReplyTo: "个人|1@INBOX"}); err != nil || len(home.sent) != 1 || home.sent[0].ReplyTo != "1@INBOX" {
		t.Fatalf("回复应从收信的邮箱发: %v %+v", err, home.sent)
	}
	if _, err := sender.SendAs(ctx, "工作", mail.Outgoing{Body: "x", ReplyTo: "个人|1@INBOX"}); err == nil {
		t.Fatal("回复个人邮箱的信不能用工作邮箱发")
	}
	if _, err := sender.SendAs(ctx, "个人", mail.Outgoing{Body: "x"}); err != nil || len(home.sent) != 2 {
		t.Fatalf("应从指定的邮箱发: %v", err)
	}
	if _, err := svc.Send(ctx, mail.Outgoing{Body: "x"}); err != nil || len(work.sent) != 1 {
		t.Fatalf("不指定时从默认邮箱发: %v", err)
	}
	if _, err := sender.SendAs(ctx, "没有", mail.Outgoing{Body: "x"}); err == nil {
		t.Fatal("指定了不存在的邮箱应报错")
	}
}
