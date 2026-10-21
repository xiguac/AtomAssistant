package session

import (
	"testing"
	"time"
)

func TestDay_凌晨四点之前还算前一天(t *testing.T) {
	loc := time.Local
	cases := []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 10, 21, 3, 59, 0, 0, loc), "2026-10-20"},
		{time.Date(2026, 10, 21, 4, 0, 0, 0, loc), "2026-10-21"},
		{time.Date(2026, 10, 21, 23, 30, 0, 0, loc), "2026-10-21"},
		{time.Date(2026, 10, 22, 1, 0, 0, 0, loc), "2026-10-21"},
	}
	for _, c := range cases {
		if got := Day(c.at); got != c.want {
			t.Errorf("Day(%s) = %s，应为 %s", c.at, got, c.want)
		}
	}
}
