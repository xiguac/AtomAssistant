package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSecurity_不在明文连接上发密码(t *testing.T) {
	cases := []struct {
		addr string
		in   Security
		want Security
		err  error
	}{
		{"imap.mail.me.com:993", "", SecurityTLS, nil},
		{"imap.example.com:143", "", SecurityStartTLS, nil},
		{"smtp.mail.me.com:587", SecurityStartTLS, SecurityStartTLS, nil},
		{"127.0.0.1:3143", SecurityNone, SecurityNone, nil},
		{"localhost:3143", SecurityNone, SecurityNone, nil},
		// 真实的服务器不允许明文：密码会在网络上裸奔。
		{"imap.mail.me.com:143", SecurityNone, "", ErrInsecure},
		{"10.0.0.8:143", SecurityNone, "", ErrInsecure},
		{"imap.mail.me.com", "", "", ErrInvalid},
		{"imap.mail.me.com:993", "ssl", "", ErrInvalid},
	}
	for _, c := range cases {
		got, err := security(c.addr, c.in, "993")
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%s %q: 期望 %q %v，实际 %q %v", c.addr, c.in, c.want, c.err, got, err)
		}
	}
}

func TestParseID(t *testing.T) {
	r, err := parseID("42.1700000000@Sent Messages")
	if err != nil || r.uid != 42 || r.validity != 1700000000 || r.folder != "Sent Messages" {
		t.Fatalf("解析不对: %+v %v", r, err)
	}
	if formatID(r.uid, r.validity, r.folder) != "42.1700000000@Sent Messages" {
		t.Fatal("往返不一致")
	}
	for _, bad := range []string{"", "42@INBOX", "0.1@INBOX", "x.1@INBOX", "42.1@", "42.1", "-1.1@INBOX"} {
		if _, err := parseID(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q 应被拒绝: %v", bad, err)
		}
	}
}

func TestExtract_国内邮件的编码与只有HTML的信(t *testing.T) {
	// GBK 编码、base64 传输的纯文本信。正文是"明天下午三点开会"。
	gbk := "From: =?GBK?B?1cXI/Q==?= <zhang@example.com>\r\n" +
		"Subject: test\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=GBK\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"w/fM7M/CzucgyP2148CwyrE=\r\n"
	text, _ := extract(strings.NewReader(gbk))
	if !strings.Contains(text, "明天下午") {
		t.Fatalf("GBK 正文应被解码: %q", text)
	}

	rich := "Subject: x\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<html><head><style>p{color:red}</style></head><body><p>您好，</p><div>账单&nbsp;&amp;&nbsp;发票已出</div>" +
		"<script>steal()</script></body></html>\r\n" +
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0=\r\n--b--\r\n"
	text, files := extract(strings.NewReader(rich))
	if text != "您好，\n账单 & 发票已出" {
		t.Fatalf("HTML 应去掉标签、样式和脚本: %q", text)
	}
	if len(files) != 1 || files[0] != "invoice.pdf" {
		t.Fatalf("附件只报名字: %v", files)
	}

	// 被截断的信：能取到多少算多少，不报错。
	cut := rich[:strings.Index(rich, "账单")+6]
	if text, _ := extract(strings.NewReader(cut)); !strings.Contains(text, "您好") {
		t.Fatalf("截断的信应尽量取出正文: %q", text)
	}
}

func TestOwnWords_只留用户亲手写的部分(t *testing.T) {
	cases := map[string]string{
		"好的，周五见。\n\n在 2026年10月8日，张三 <z@x.com> 写道：\n> 周五有空吗？":                 "好的，周五见。",
		"收到，我看一下再回你\n\nOn Oct 8, 2026, at 10:00, Bob <b@x.com> wrote:\n> hi": "收到，我看一下再回你",
		"行\n\n发自我的 iPhone": "行",
		"麻烦帮忙看下这个\n\n-----Original Message-----\nFrom: a@b.com\n正文": "麻烦帮忙看下这个",
		"> 只有引用\n> 没有自己写的":                                          "",
		"第一段\n> 中间引用\n第二段":                                          "第一段\n第二段",
	}
	for in, want := range cases {
		if got := OwnWords(in); got != want {
			t.Errorf("输入 %q\n期望 %q\n实际 %q", in, want, got)
		}
	}
}

// TestMail_对真实服务器 需要一个 IMAP/SMTP 服务器，地址放在 ATOM_TEST_IMAP 与 ATOM_TEST_SMTP，例如
// docker run -p 55143:3143 -p 55025:3025 -e GREENMAIL_OPTS="-Dgreenmail.setup.test.all
// -Dgreenmail.hostname=0.0.0.0 -Dgreenmail.auth.disabled" greenmail/standalone
// 之后分别设为 127.0.0.1:55143 与 127.0.0.1:55025。
func TestMail_对真实服务器(t *testing.T) {
	imapAddr, smtpAddr := os.Getenv("ATOM_TEST_IMAP"), os.Getenv("ATOM_TEST_SMTP")
	if imapAddr == "" || smtpAddr == "" {
		t.Skip("未设置 ATOM_TEST_IMAP / ATOM_TEST_SMTP，跳过")
	}
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	secret := func(context.Context) (string, error) { return "pw", nil }
	box := func(name string) *Client {
		addr := fmt.Sprintf("%s%d@example.com", name, stamp)
		return &Client{
			IMAPAddr: imapAddr, IMAPSecurity: SecurityNone,
			SMTPAddr: smtpAddr, SMTPSecurity: SecurityNone,
			Username: addr, Name: name, Secret: secret,
		}
	}
	me, boss := box("me"), box("boss")

	// 测试服务器的新邮箱只有收件箱，把常见的几个文件夹建出来。
	for _, c := range []*Client{me, boss} {
		conn, err := c.dial(ctx)
		if err != nil {
			t.Fatalf("登录失败: %v", err)
		}
		for _, f := range []string{"Sent Messages", "Deleted Messages", "Archive"} {
			if err := conn.Create(f); err != nil {
				t.Fatalf("建文件夹失败: %v", err)
			}
		}
		_ = conn.Logout()
	}

	long := strings.Repeat("这是一封很长的信。", 300)
	sent, err := boss.Send(ctx, Outgoing{To: []string{me.Username}, Subject: "周五评审改期", Body: "评审改到周五下午三点，会议室 B。\n\n" + long})
	if err != nil {
		t.Fatalf("发信失败: %v", err)
	}
	if !sent.Saved || sent.Note != "" {
		t.Fatalf("发出的信应在已发送里留底: %+v", sent)
	}
	if _, err := boss.Send(ctx, Outgoing{To: []string{me.Username}, Subject: "报销单", Body: "记得交报销单"}); err != nil {
		t.Fatalf("发信失败: %v", err)
	}

	folders, err := me.Folders(ctx)
	if err != nil {
		t.Fatalf("列文件夹失败: %v", err)
	}
	roles := map[string]string{}
	for _, f := range folders {
		roles[f.Role] = f.Name
	}
	if roles["inbox"] != "INBOX" || roles["sent"] != "Sent Messages" || roles["trash"] != "Deleted Messages" || roles["archive"] != "Archive" {
		t.Fatalf("应认出各文件夹的用途: %+v", folders)
	}

	var inbox []Summary
	for i := 0; i < 20 && len(inbox) < 2; i++ {
		time.Sleep(200 * time.Millisecond)
		if inbox, err = me.Search(ctx, Query{}); err != nil {
			t.Fatalf("搜索失败: %v", err)
		}
	}
	if len(inbox) != 2 {
		t.Fatalf("收件箱应有两封信: %+v", inbox)
	}
	found, err := me.Search(ctx, Query{Subject: "评审", UnreadOnly: true})
	if err != nil || len(found) != 1 || found[0].Subject != "周五评审改期" || !found[0].Unread ||
		!strings.Contains(found[0].From, boss.Username) {
		t.Fatalf("按标题搜索不对: %v %+v", err, found)
	}
	review := found[0]

	// 读信：分段返回，而且不把信标成已读。
	msg, err := me.Read(ctx, review.ID, 0)
	if err != nil {
		t.Fatalf("读信失败: %v", err)
	}
	if !strings.HasPrefix(msg.Text, "评审改到周五下午三点，会议室 B。") || msg.NextOffset == 0 || len(msg.Text) > ChunkBytes {
		t.Fatalf("长信应分段: %d 字节, next=%d", len(msg.Text), msg.NextOffset)
	}
	rest, err := me.Read(ctx, review.ID, msg.NextOffset)
	if err != nil || !strings.Contains(rest.Text, "这是一封很长的信。") || strings.ContainsRune(rest.Text, '�') {
		t.Fatalf("接着读应从字符边界继续: %v %q", err, rest.Text[:30])
	}
	if still, _ := me.Search(ctx, Query{UnreadOnly: true}); len(still) != 2 {
		t.Fatalf("读信不应把它标成已读，未读应仍是两封: %d", len(still))
	}

	yes := true
	if err := me.Mark(ctx, review.ID, &yes, &yes); err != nil {
		t.Fatalf("标记失败: %v", err)
	}
	if after, _ := me.Search(ctx, Query{Subject: "评审"}); len(after) != 1 || after[0].Unread || !after[0].Flagged {
		t.Fatalf("应已读且有星标: %+v", after)
	}

	// 回复：不写收件人和标题，按原信补。
	reply, err := me.Send(ctx, Outgoing{ReplyTo: review.ID, Body: "收到，周五见。"})
	if err != nil {
		t.Fatalf("回复失败: %v", err)
	}
	if reply.Subject != "Re: 周五评审改期" || len(reply.To) != 1 || reply.To[0] != boss.Username || !reply.Saved {
		t.Fatalf("回复应沿用原信的标题和发件人: %+v", reply)
	}
	mine, err := me.Search(ctx, Query{Folder: "sent"})
	if err != nil || len(mine) != 1 || mine[0].Unread {
		t.Fatalf("已发送里应有这封回复且是已读: %v %+v", err, mine)
	}
	if got, _ := me.Read(ctx, mine[0].ID, 0); got.Text != "收到，周五见。" {
		t.Fatalf("留底的正文不对: %q", got.Text)
	}

	// 删除就是移进废纸篓。
	dest, err := me.Move(ctx, review.ID, "trash")
	if err != nil || dest != "Deleted Messages" {
		t.Fatalf("移动失败: %v %q", err, dest)
	}
	if left, _ := me.Search(ctx, Query{}); len(left) != 1 || left[0].Subject != "报销单" {
		t.Fatalf("收件箱应只剩一封: %+v", left)
	}
	if bin, _ := me.Search(ctx, Query{Folder: "Deleted Messages"}); len(bin) != 1 || bin[0].Subject != "周五评审改期" {
		t.Fatalf("废纸篓里应能找回: %+v", bin)
	}

	// 不替模型新建文件夹；编号对不上的 id 不执行。
	if _, err := me.Move(ctx, inbox[0].ID, "一个不存在的文件夹"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的文件夹应被拒绝: %v", err)
	}
	if err := me.Mark(ctx, "1.999@INBOX", &yes, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失效的 id 应被拒绝: %v", err)
	}
	for name, o := range map[string]Outgoing{
		"没有收件人": {Subject: "x", Body: "y"},
		"地址不合法": {To: []string{"不是邮箱"}, Subject: "x", Body: "y"},
		"正文为空":  {To: []string{boss.Username}, Subject: "x"},
		"标题换行":  {To: []string{boss.Username}, Subject: "x\r\nBcc: evil@example.com", Body: "y"},
	} {
		if _, err := me.Send(ctx, o); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: 期望 ErrInvalid，实际 %v", name, err)
		}
	}
}
