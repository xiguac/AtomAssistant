package calendar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// putRaw 直接往测试服务器放一个日程对象。
func putRaw(t *testing.T, endpoint, user, path, vevent string) {
	t.Helper()
	body := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//\r\n" + strings.ReplaceAll(vevent, "\n", "\r\n") + "END:VCALENDAR\r\n"
	req, err := http.NewRequest(http.MethodPut, endpoint+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, "pw")
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("写入测试日程失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("写入测试日程失败: %s", resp.Status)
	}
}

// TestOccurrence_只改或取消重复日程里的某一次 需要 CalDAV 服务器，见 TestCalDAV_对真实服务器。
func TestOccurrence_只改或取消重复日程里的某一次(t *testing.T) {
	endpoint := os.Getenv("ATOM_TEST_CALDAV")
	if endpoint == "" {
		t.Skip("未设置 ATOM_TEST_CALDAV，跳过")
	}
	ctx := context.Background()
	user := fmt.Sprintf("o%d", time.Now().UnixNano())
	mkcalendar(t, endpoint, user, "course", "课程", "VEVENT")
	id := "/" + user + "/course/math.ics"
	// 每周一、三 10:00 的课，从 10 月 5 日起。
	putRaw(t, endpoint, user, id, `BEGIN:VEVENT
UID:math
DTSTAMP:20260901T000000Z
DTSTART;TZID=Asia/Shanghai:20261005T100000
DTEND;TZID=Asia/Shanghai:20261005T114500
RRULE:FREQ=WEEKLY;BYDAY=MO,WE
SUMMARY:高等数学
LOCATION:三教 201
END:VEVENT
`)
	c := &Client{Endpoint: endpoint, Username: user, Loc: shanghai,
		Secret: func(context.Context) (string, error) { return "pw", nil }}
	list := func() string {
		t.Helper()
		events, _, err := c.Events(ctx, "", at("2026-10-05 00:00"), at("2026-10-19 00:00"))
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		var lines []string
		for _, e := range events {
			lines = append(lines, e.Start.Format("01-02 15:04")+"-"+e.End.Format("15:04")+" "+e.Title+"@"+e.Location)
		}
		return strings.Join(lines, "|")
	}
	if got := list(); got != "10-05 10:00-11:45 高等数学@三教 201|10-07 10:00-11:45 高等数学@三教 201|10-12 10:00-11:45 高等数学@三教 201|10-14 10:00-11:45 高等数学@三教 201" {
		t.Fatalf("初始展开不对: %s", got)
	}

	// 调课：10 月 7 日那次挪到 10 月 10 日（周六）下午，换个教室。其他各次不动。
	occ, moved, room := at("2026-10-07 10:00"), at("2026-10-10 14:00"), "二教 105"
	changed, err := c.Update(ctx, id, Patch{Occurrence: &occ, Start: &moved, Location: &room})
	if err != nil {
		t.Fatalf("改某一次失败: %v", err)
	}
	if !changed.Start.Equal(moved) || !changed.End.Equal(at("2026-10-10 15:45")) || changed.Location != room || !changed.Recurring {
		t.Fatalf("返回的应是改后的这一次，时长不变: %+v", changed)
	}
	want := "10-05 10:00-11:45 高等数学@三教 201|10-10 14:00-15:45 高等数学@二教 105|10-12 10:00-11:45 高等数学@三教 201|10-14 10:00-11:45 高等数学@三教 201"
	if got := list(); got != want {
		t.Fatalf("只应动 10 月 7 日那一次:\n期望 %s\n实际 %s", want, got)
	}

	// 再改一次已经改过的那次：调用方看到的是它现在的时间（10 日 14:00），按这个时间也要找得到。
	title := "高等数学（补课）"
	if _, err := c.Update(ctx, id, Patch{Occurrence: &moved, Title: &title}); err != nil {
		t.Fatalf("按现在的时间改已改期的那一次失败: %v", err)
	}
	if got := list(); !strings.Contains(got, "10-10 14:00-15:45 高等数学（补课）@二教 105") || strings.Count(got, "|") != 3 {
		t.Fatalf("应改在同一次上而不是多出一条: %s", got)
	}

	// 调休：10 月 12 日那次不上了。
	skip := at("2026-10-12 10:00")
	gone, err := c.Update(ctx, id, Patch{Occurrence: &skip, Cancel: true})
	if err != nil {
		t.Fatalf("取消某一次失败: %v", err)
	}
	if !gone.Start.Equal(skip) || gone.Title != "高等数学" {
		t.Fatalf("应返回被取消的那一次: %+v", gone)
	}
	want = "10-05 10:00-11:45 高等数学@三教 201|10-10 14:00-15:45 高等数学（补课）@二教 105|10-14 10:00-11:45 高等数学@三教 201"
	if got := list(); got != want {
		t.Fatalf("10 月 12 日那次应消失，其他不动:\n期望 %s\n实际 %s", want, got)
	}
	// 把改期过的那一次也取消：单独存的那份要一并去掉。
	if _, err := c.Update(ctx, id, Patch{Occurrence: &moved, Cancel: true}); err != nil {
		t.Fatalf("取消已改期的那一次失败: %v", err)
	}
	if got := list(); got != "10-05 10:00-11:45 高等数学@三教 201|10-14 10:00-11:45 高等数学@三教 201" {
		t.Fatalf("改期过的那一次取消后不应以任何形式留下: %s", got)
	}

	// 整个系列：标题可以改，时间不许一次全改。
	all := "高数"
	if _, err := c.Update(ctx, id, Patch{Title: &all}); err != nil {
		t.Fatalf("改系列标题失败: %v", err)
	}
	if got := list(); got != "10-05 10:00-11:45 高数@三教 201|10-14 10:00-11:45 高数@三教 201" {
		t.Fatalf("系列标题应整体生效: %s", got)
	}
	if _, err := c.Update(ctx, id, Patch{Start: &moved}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("不指明哪一次就改时间应被拒绝: %v", err)
	}

	// 指错了的各种情况。
	never := at("2026-10-06 10:00") // 周二，没有课
	if _, err := c.Update(ctx, id, Patch{Occurrence: &never, Title: &all}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("系列里没有的那一次应报错: %v", err)
	}
	if _, err := c.Update(ctx, id, Patch{Occurrence: &skip, Title: &all}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已经取消的那一次不能再改: %v", err)
	}
	if _, err := c.Update(ctx, id, Patch{Cancel: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("取消必须指明哪一次: %v", err)
	}
	single, err := c.Create(ctx, Draft{Title: "单次", Start: at("2026-10-08 09:00")}, "")
	if err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	one := at("2026-10-08 09:00")
	if _, err := c.Update(ctx, single.ID, Patch{Occurrence: &one, Title: &all}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("对非重复日程指明哪一次应报错: %v", err)
	}
}
