package event

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
)

func sub(expr string) *Subscription {
	return &Subscription{ID: "s1", TaskID: "t1", Expr: expr, Active: true}
}

func calendarEvent() *Event {
	return New("calendar.event.created", json.RawMessage(`{
		"title": "与客户张总午餐",
		"attendees": ["客户张总", "小李"],
		"duration_minutes": 90,
		"all_day": false,
		"location": {"city": "上海"},
		"start_at": "2026-10-12T12:00:00Z"
	}`), core.TrustConnector, "caldav:工作日历")
}

func TestMatches_表达式求值(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{`topic == "calendar.event.created"`, true},
		{`topic == "calendar.event.created" AND payload.attendees CONTAINS "客户张总"`, true},
		{`payload.attendees CONTAINS "王总"`, false},
		{`payload.title CONTAINS "午餐"`, true},
		{`payload.duration_minutes >= 60`, true},
		{`payload.duration_minutes < 60`, false},
		{`payload.all_day == false`, true},
		{`payload.location.city == "上海"`, true},
		{`payload.start_at > "2026-10-01T00:00:00Z"`, true},
		{`source == "caldav:工作日历"`, true},
		{`NOT payload.attendees CONTAINS "王总"`, true},
		{`topic == "x" OR payload.location.city == "上海"`, true},
		{`topic == "x" OR topic == "y" AND payload.all_day == false`, false},
		{`(topic == "x" OR payload.all_day == false) AND payload.duration_minutes > 10`, true},
		// 字段缺失是常态：不命中，但也不报错。
		{`payload.missing == "a"`, false},
		{`payload.missing != "a"`, true},
		{`payload.missing == null`, true},
		{`payload.missing > 3`, false},
		{`payload.title.deeper == "a"`, false},
		// 类型不一致不命中。
		{`payload.duration_minutes == "90"`, false},
		{`payload.title > 3`, false},
	}
	now := time.Now().UTC()
	for _, tc := range cases {
		got, err := sub(tc.expr).Matches(calendarEvent(), now)
		if err != nil {
			t.Errorf("%s: 求值报错 %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: 期望 %v，实际 %v", tc.expr, tc.want, got)
		}
	}
}

func TestParse_语法错误在订阅时就暴露(t *testing.T) {
	bad := []string{
		`topic`,
		`topic ==`,
		`topic = "a"`,
		`topic == "a" AND`,
		`(topic == "a"`,
		`topic == "a")`,
		`topic == "没有闭合`,
		`topic == 客户A`,
		`attendee == "a"`,
		`topic.sub == "a"`,
		`payload..x == 1`,
		`AND == "a"`,
		`topic == "a" payload.x == 1`,
		strings.Repeat("(", maxExprDepth+2) + `topic == "a"` + strings.Repeat(")", maxExprDepth+2),
		`topic == "` + strings.Repeat("a", maxExprLen) + `"`,
	}
	for _, expr := range bad {
		if _, err := Parse(expr); !errors.Is(err, ErrInvalidExpr) {
			t.Errorf("%.40s: 期望 ErrInvalidExpr，实际 %v", expr, err)
		}
		if err := sub(expr).Validate(); !errors.Is(err, ErrInvalidSubscription) {
			t.Errorf("%.40s: 订阅校验应失败，实际 %v", expr, err)
		}
	}
}

func TestMatches_失效与过期的订阅不命中(t *testing.T) {
	now := time.Now().UTC()
	e := calendarEvent()

	inactive := sub(`topic == "calendar.event.created"`)
	inactive.Active = false
	if ok, _ := inactive.Matches(e, now); ok {
		t.Fatal("未激活的订阅不应命中")
	}

	expired := sub(`topic == "calendar.event.created"`)
	past := now.Add(-time.Minute)
	expired.ExpiresAt = &past
	if ok, _ := expired.Matches(e, now); ok {
		t.Fatal("已过期的订阅不应命中")
	}

	prefix := sub(`payload.all_day == false`)
	prefix.TopicPrefix = "mail"
	if ok, _ := prefix.Matches(e, now); ok {
		t.Fatal("主题前缀不符的订阅不应命中")
	}
	prefix.TopicPrefix = "calendar"
	if ok, _ := prefix.Matches(e, now); !ok {
		t.Fatal("主题前缀相符的订阅应命中")
	}
}

func TestTopicMatches_按层级边界匹配(t *testing.T) {
	if !TopicMatches("calendar.event.created", "calendar") {
		t.Fatal("calendar 应匹配 calendar.event.created")
	}
	if TopicMatches("calendarx.foo", "calendar") {
		t.Fatal("calendar 不应匹配 calendarx.foo")
	}
}

func TestMatches_非法Payload返回错误而不是命中(t *testing.T) {
	e := New("a.b", json.RawMessage(`{不是JSON`), core.TrustExternal, "webhook")
	ok, err := sub(`topic == "a.b"`).Matches(e, time.Now().UTC())
	if ok || err == nil {
		t.Fatalf("期望不命中且报错，实际 ok=%v err=%v", ok, err)
	}
}
