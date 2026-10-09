package config

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeLender 记录谁借了哪一条。
type fakeLender struct{ asked []string }

func (f *fakeLender) Lend(handle, user string) (func(context.Context) (string, error), error) {
	if !strings.HasPrefix(handle, "vault://") {
		return nil, errors.New("不是句柄")
	}
	f.asked = append(f.asked, user+"="+handle)
	return func(context.Context) (string, error) { return "pw", nil }, nil
}

func TestConnectors_按配置构造且密码只能是句柄(t *testing.T) {
	f, err := LoadFile(writeConfig(t, `
connectors:
  calendar:
    url: https://caldav.icloud.com
    username: me@icloud.com
    password: vault://icloud
    default_calendar: 个人
  mail:
    imap: imap.mail.me.com:993
    smtp: smtp.mail.me.com:587
    username: me@icloud.com
    password: vault://icloud
    name: 张三
`))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	lender := &fakeLender{}
	cal, err := f.Calendar(lender)
	if err != nil || cal.Endpoint != "https://caldav.icloud.com" || cal.Default != "个人" || cal.Secret == nil {
		t.Fatalf("日历连接器不对: %+v %v", cal, err)
	}
	box, err := f.Mail(lender)
	if err != nil || box.IMAPAddr != "imap.mail.me.com:993" || box.SMTPAddr != "smtp.mail.me.com:587" || box.Name != "张三" {
		t.Fatalf("邮件连接器不对: %+v %v", box, err)
	}
	// 两个连接器各用各的名字去借，保险库才能分别授权。
	if strings.Join(lender.asked, ",") != "connector.calendar=vault://icloud,connector.mail=vault://icloud" {
		t.Fatalf("借用身份不对: %v", lender.asked)
	}

	// 没写就是没接入。
	empty, err := LoadFile(writeConfig(t, "tools: []\n"))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cal, err := empty.Calendar(lender); cal != nil || err != nil {
		t.Fatalf("没配置日历时应返回 nil: %v %v", cal, err)
	}
	if box, err := empty.Mail(lender); box != nil || err != nil {
		t.Fatalf("没配置邮件时应返回 nil: %v %v", box, err)
	}

	// 明文密码写进配置文件：启动时就报错。
	plain, err := LoadFile(writeConfig(t, `
connectors:
  calendar:
    url: https://caldav.icloud.com
    username: me@icloud.com
    password: abcd-efgh-ijkl-mnop
`))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, err := plain.Calendar(lender); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("明文密码应被拒绝: %v", err)
	}
}
