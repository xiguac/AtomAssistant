package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/device"
)

func TestRequestNotice_提醒里写明要什么为什么去哪里办(t *testing.T) {
	exp := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	loc := requestNotice(&device.Request{Kind: "location.now", Reason: "查离机场多远", ExpiresAt: exp})
	for _, want := range []string{"当前位置", "查离机场多远", "授权", "同意后才会采集"} {
		if !strings.Contains(loc, want) {
			t.Errorf("位置提醒里缺少 %q:\n%s", want, loc)
		}
	}
}
