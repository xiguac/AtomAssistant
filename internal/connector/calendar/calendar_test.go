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

	"github.com/emersion/go-ical"
)

var shanghai = time.FixedZone("CST", 8*3600)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, shanghai)
	if err != nil {
		panic(err)
	}
	return t
}

func parse(t *testing.T, text string) *ical.Calendar {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(strings.ReplaceAll(text, "\n", "\r\n"))).Decode()
	if err != nil {
		t.Fatalf("解析测试数据失败: %v", err)
	}
	return cal
}

func TestOccurrences_重复日程逐次展开并套用例外(t *testing.T) {
	c := &Client{Loc: shanghai}
	// 每周三 10:00 的课；10 月 14 日那次取消，10 月 21 日那次改到了下午。
	data := parse(t, `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//
BEGIN:VEVENT
UID:course
DTSTAMP:20260901T000000Z
DTSTART;TZID=Asia/Shanghai:20261007T100000
DTEND;TZID=Asia/Shanghai:20261007T114500
RRULE:FREQ=WEEKLY;BYDAY=WE
EXDATE;TZID=Asia/Shanghai:20261014T100000
SUMMARY:高等数学
LOCATION:三教 201
END:VEVENT
BEGIN:VEVENT
UID:course
DTSTAMP:20260901T000000Z
RECURRENCE-ID;TZID=Asia/Shanghai:20261021T100000
DTSTART;TZID=Asia/Shanghai:20261021T140000
DTEND;TZID=Asia/Shanghai:20261021T154500
SUMMARY:高等数学（调课）
END:VEVENT
END:VCALENDAR
`)
	got := c.occurrences("/cal/course.ics", "课程", data, at("2026-10-01 00:00"), at("2026-11-01 00:00"))
	var lines []string
	for _, e := range got {
		lines = append(lines, e.Start.Format("01-02 15:04")+" "+e.Title)
		if !e.Recurring || e.ID != "/cal/course.ics" {
			t.Errorf("每一次都应标为重复日程并指向同一个对象: %+v", e)
		}
	}
	want := "10-07 10:00 高等数学|10-21 14:00 高等数学（调课）|10-28 10:00 高等数学"
	if strings.Join(lines, "|") != want {
		t.Fatalf("展开结果不对:\n期望 %s\n实际 %s", want, strings.Join(lines, "|"))
	}
	if got[0].End.Sub(got[0].Start) != 105*time.Minute || got[0].Location != "三教 201" {
		t.Fatalf("时长或地点不对: %+v", got[0])
	}
	// 窗口之外的不出现。
	if out := c.occurrences("/x", "课程", data, at("2026-10-08 00:00"), at("2026-10-20 00:00")); len(out) != 0 {
		t.Fatalf("10 月 14 日那次已取消，这段时间不应有课: %+v", out)
	}
}

func TestOccurrences_全天日程与认不出的时区(t *testing.T) {
	c := &Client{Loc: shanghai}
	data := parse(t, `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//
BEGIN:VEVENT
UID:a
DTSTAMP:20260901T000000Z
DTSTART;VALUE=DATE:20261001
DTEND;VALUE=DATE:20261004
SUMMARY:国庆出游
END:VEVENT
END:VCALENDAR
`)
	got := c.occurrences("/a", "个人", data, at("2026-10-03 12:00"), at("2026-10-03 13:00"))
	if len(got) != 1 || !got[0].AllDay || got[0].End.Format("01-02") != "10-04" {
		t.Fatalf("跨多天的全天日程应覆盖中间的日子: %+v", got)
	}

	// Outlook 发来的邀请会写这种不是标准名的时区。认不出来也不能把日程丢掉。
	odd := parse(t, `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//
BEGIN:VEVENT
UID:b
DTSTAMP:20260901T000000Z
DTSTART;TZID=China Standard Time:20261009T150000
DTEND;TZID=China Standard Time:20261009T160000
ORGANIZER;CN=李伟:mailto:liwei@example.com
SUMMARY:周会
END:VEVENT
END:VCALENDAR
`)
	got = c.occurrences("/b", "工作", odd, at("2026-10-09 00:00"), at("2026-10-10 00:00"))
	if len(got) != 1 || got[0].Start.Format("15:04") != "15:00" {
		t.Fatalf("时区名认不出时应按本地时区理解: %+v", got)
	}
	// 别人发来的邀请要看得出是谁发起的。
	if got[0].Organizer != "李伟 <liwei@example.com>" {
		t.Fatalf("应带上发起人: %q", got[0].Organizer)
	}
}

func TestOwner_只认日历集合里面的路径(t *testing.T) {
	c := &Client{calendars: []Calendar{{Path: "/alice/personal/", Name: "个人"}}, cachedAt: time.Now()}
	ctx := context.Background()
	if cal, err := c.owner(ctx, "/alice/personal/abc.ics"); err != nil || cal.Name != "个人" {
		t.Fatalf("日历里的对象应被接受: %v", err)
	}
	bad := map[string]error{
		"/alice/personal/":                 ErrNotFound, // 日历本身，删它等于删掉整个日历
		"/alice/personal":                  ErrNotFound,
		"/alice/":                          ErrNotFound,
		"/alice/personal/../work/x.ics":    ErrInvalid,
		"/alice/personal/./x.ics":          ErrInvalid,
		"/alice/personal-evil/x.ics":       ErrNotFound, // 前缀相同但不是同一个集合
		"/bob/personal/x.ics":              ErrNotFound,
		"alice/personal/x.ics":             ErrInvalid,
		"https://evil.example/alice/x.ics": ErrInvalid,
		"":                                 ErrInvalid,
	}
	for id, want := range bad {
		if _, err := c.owner(ctx, id); !errors.Is(err, want) {
			t.Errorf("%q: 期望 %v，实际 %v", id, want, err)
		}
	}
}

// TestCalDAV_对真实服务器 需要一个 CalDAV 服务器，地址放在 ATOM_TEST_CALDAV，
// 例如 docker run -p 55232:5232 tomsquest/docker-radicale 之后设为 http://127.0.0.1:55232。
func TestCalDAV_对真实服务器(t *testing.T) {
	endpoint := os.Getenv("ATOM_TEST_CALDAV")
	if endpoint == "" {
		t.Skip("未设置 ATOM_TEST_CALDAV，跳过")
	}
	ctx := context.Background()
	user := fmt.Sprintf("u%d", time.Now().UnixNano())
	mkcalendar(t, endpoint, user, "personal", "个人", "VEVENT")
	mkcalendar(t, endpoint, user, "work", "工作", "VEVENT")
	mkcalendar(t, endpoint, user, "todo", "提醒事项", "VTODO")

	asked := 0
	c := &Client{
		Endpoint: endpoint, Username: user, Loc: shanghai,
		Secret: func(context.Context) (string, error) { asked++; return "pw", nil },
	}

	cals, err := c.Calendars(ctx)
	if err != nil {
		t.Fatalf("列出日历失败: %v", err)
	}
	if len(cals) != 2 || cals[0].Name != "个人" || cals[1].Name != "工作" {
		t.Fatalf("应列出两个能放日程的日历，提醒事项不算: %+v", cals)
	}

	// 有多个日历又没指明：不猜。
	if _, err := c.Create(ctx, Draft{Title: "x", Start: at("2026-10-12 10:00")}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("多个日历时应要求指明: %v", err)
	}

	created, err := c.Create(ctx, Draft{
		Calendar: "工作", Title: "和张总吃饭", Location: "外婆家（西湖店）", Notes: "他不吃辣",
		Start: at("2026-10-12 18:30"), End: at("2026-10-12 20:00"),
	}, "task-1/3")
	if err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	// 同一个幂等键再来一次：还是那一条，不是两条。
	again, err := c.Create(ctx, Draft{
		Calendar: "工作", Title: "和张总吃饭", Start: at("2026-10-12 18:30"), End: at("2026-10-12 20:00"),
	}, "task-1/3")
	if err != nil || again.ID != created.ID {
		t.Fatalf("同一个幂等键应写到同一个对象: %v, %s vs %s", err, again.ID, created.ID)
	}
	if _, err := c.Create(ctx, Draft{Calendar: "个人", Title: "国庆出游", AllDay: true,
		Start: at("2026-10-01 00:00"), End: at("2026-10-03 00:00")}, ""); err != nil {
		t.Fatalf("新建全天日程失败: %v", err)
	}

	list, truncated, err := c.Events(ctx, "", at("2026-10-01 00:00"), at("2026-11-01 00:00"))
	if err != nil || truncated {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有两条日程，实际 %d: %+v", len(list), list)
	}
	trip, dinner := list[0], list[1]
	if !trip.AllDay || trip.Start.Format("01-02") != "10-01" || trip.End.Format("01-02") != "10-04" {
		t.Fatalf("10 月 1 日到 3 日的全天日程应存成到 4 日为止（不含）: %+v", trip)
	}
	if dinner.Title != "和张总吃饭" || dinner.Calendar != "工作" || dinner.Location != "" ||
		!dinner.Start.Equal(at("2026-10-12 18:30")) || !dinner.End.Equal(at("2026-10-12 20:00")) {
		t.Fatalf("读回的日程不对（第二次写入没带地点，应以它为准）: %+v", dinner)
	}
	// 只查某一个日历、只查某一段时间。
	if only, _, err := c.Events(ctx, "个人", at("2026-10-01 00:00"), at("2026-11-01 00:00")); err != nil || len(only) != 1 {
		t.Fatalf("按日历筛选不对: %v %+v", err, only)
	}
	if none, _, err := c.Events(ctx, "", at("2026-10-13 00:00"), at("2026-10-20 00:00")); err != nil || len(none) != 0 {
		t.Fatalf("这段时间不应有日程: %v %+v", err, none)
	}

	// 只挪开始时间：时长不变。
	start := at("2026-10-13 12:00")
	title := "和张总午饭"
	moved, err := c.Update(ctx, dinner.ID, Patch{Start: &start, Title: &title})
	if err != nil {
		t.Fatalf("修改失败: %v", err)
	}
	if !moved.Start.Equal(start) || !moved.End.Equal(at("2026-10-13 13:30")) || moved.Title != title {
		t.Fatalf("改期后应保持 90 分钟: %+v", moved)
	}
	if got, err := c.Get(ctx, dinner.ID); err != nil || !got.Start.Equal(start) {
		t.Fatalf("服务器上应已是新时间: %v %+v", err, got)
	}
	// 全天日程延长一天。
	last := at("2026-10-04 00:00")
	longer, err := c.Update(ctx, trip.ID, Patch{End: &last})
	if err != nil || longer.End.Format("01-02") != "10-05" || longer.Start.Format("01-02") != "10-01" {
		t.Fatalf("全天日程延长到 4 日应存成到 5 日为止: %v %+v", err, longer)
	}

	// 日历之外的路径不碰。
	if err := c.Delete(ctx, "/"+user+"/todo/"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不应能删除日历集合本身: %v", err)
	}
	if err := c.Delete(ctx, dinner.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if left, _, _ := c.Events(ctx, "工作", at("2026-10-01 00:00"), at("2026-11-01 00:00")); len(left) != 0 {
		t.Fatalf("删除后不应还在: %+v", left)
	}
	if asked == 0 {
		t.Fatal("密码应在调用时现取")
	}

	locked := &Client{Endpoint: endpoint, Username: user,
		Secret: func(context.Context) (string, error) { return "", errors.New("保险库锁着") }}
	if _, err := locked.Calendars(ctx); err == nil || !strings.Contains(err.Error(), "保险库锁着") {
		t.Fatalf("取不到密码应如实报出来: %v", err)
	}
}

func mkcalendar(t *testing.T, endpoint, user, slug, name, comp string) {
	t.Helper()
	body := `<c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop>` +
		`<d:displayname>` + name + `</d:displayname><c:supported-calendar-component-set><c:comp name="` + comp +
		`"/></c:supported-calendar-component-set></d:prop></d:set></c:mkcalendar>`
	req, err := http.NewRequest("MKCALENDAR", endpoint+"/"+user+"/"+slug+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, "pw")
	req.Header.Set("Content-Type", "application/xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("创建测试日历失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建测试日历失败: %s", resp.Status)
	}
}
