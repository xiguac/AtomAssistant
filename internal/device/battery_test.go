package device

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

// series 造一段每 5 分钟一条的记录：level 从 from 按每分钟 perMin 个百分点变化，共 n 条。
func series(start time.Time, from, perMin float64, n int, charging bool) []BatterySample {
	out := make([]BatterySample, n)
	for i := range out {
		lv := from + perMin*5*float64(i)/100
		out[i] = BatterySample{At: start.Add(time.Duration(i) * 5 * time.Minute), Level: lv, Charging: charging, Unplugged: !charging}
	}
	return out
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// simulateCharge 模拟一次从 from 充到 to 的连续充电：每 5 分钟一条，速度由 rate 按当前电量给出（每分钟个百分点）。
func simulateCharge(start time.Time, from, to float64, rate func(level float64) float64) []BatterySample {
	var out []BatterySample
	lv, at := from, start
	for lv < to {
		out = append(out, BatterySample{At: at, Level: lv, Charging: true})
		lv += rate(lv) * 5 / 100
		at = at.Add(5 * time.Minute)
	}
	return out
}

func TestLearnBattery_充电速度按电量区间分开估(t *testing.T) {
	// 0–50% 每分钟 1 个百分点，50–80% 每分钟 0.5，80% 以上每分钟 0.25。
	rate := func(l float64) float64 {
		switch {
		case l < 0.5:
			return 1
		case l < 0.8:
			return 0.5
		}
		return 0.25
	}
	m := LearnBattery(simulateCharge(t0, 0.05, 0.98, rate), minGap)
	want := []float64{1, 0.5, 0.25}
	for i, w := range want {
		if !m.Charge[i].OK || !near(m.Charge[i].PercentPerMinute, w, 0.05) {
			t.Errorf("区间 %d 的充电速度 = %+v，想要约 %v %%/分钟", i, m.Charge[i], w)
		}
	}
}

func TestLearnBattery_数据不够不给结论(t *testing.T) {
	// 只有 20 分钟的充电记录，不到 30 分钟的门槛。
	m := LearnBattery(series(t0, 0.10, 1, 5, true), minGap)
	if m.Charge[0].OK {
		t.Errorf("观测时间不足时不该给出速度: %+v", m.Charge[0])
	}
	if m.DrainOK {
		t.Error("没有放电记录时不该给出掉电速度")
	}
}

func TestLearnBattery_间隔太长的不算(t *testing.T) {
	a := series(t0, 0.90, -0.1, 3, false)
	b := series(t0.Add(5*time.Hour), 0.40, -0.1, 3, false) // 中间断了 5 小时
	m := LearnBattery(append(a, b...), minGap)
	// 只有两段内部的 2+2 个间隔 = 20 分钟，远不到 120 分钟。
	if m.DrainOK {
		t.Errorf("断档不能被算成放电时间: %+v", m)
	}
	if !near(m.DrainObservedHours, 0.3, 0.05) {
		t.Errorf("观测时长 = %v 小时，想要约 0.3", m.DrainObservedHours)
	}
}

func TestLearnBattery_掉电速度(t *testing.T) {
	// 每分钟掉 0.05 个百分点 = 每小时 3 个百分点，观测 3 小时。
	m := LearnBattery(series(t0, 0.90, -0.05, 37, false), minGap)
	if !m.DrainOK || !near(m.DrainPercentPerHour, 3, 0.1) {
		t.Errorf("掉电速度 = %+v，想要约 3 %%/小时", m)
	}
}

func TestLearnBattery_读数抖动被丢掉(t *testing.T) {
	s := series(t0, 0.10, 1, 20, true)
	s[4].Level -= 0.05 // 充电中读数突然往回掉了一格
	m := LearnBattery(s, minGap)
	if !m.Charge[1].OK && !m.Charge[0].OK {
		t.Fatal("抖动不该让整段作废")
	}
	for _, b := range m.Charge {
		if b.OK && b.PercentPerMinute < 0 {
			t.Errorf("速度出现负数: %+v", b)
		}
	}
}

func TestMinutesToCharge_路过的区间缺数据就说不知道(t *testing.T) {
	m := BatteryModel{Charge: []ChargeBand{
		{FromPercent: 0, ToPercent: 50, PercentPerMinute: 1, OK: true},
		{FromPercent: 50, ToPercent: 80, PercentPerMinute: 0.5, OK: true},
		{FromPercent: 80, ToPercent: 100, OK: false},
	}}
	// 20% → 80%：30 分钟 + 60 分钟。
	if v, ok := m.MinutesToCharge(0.2, 0.8); !ok || v != 90 {
		t.Errorf("20→80 = %v %v，想要 90 分钟", v, ok)
	}
	// 充到 100% 要路过没数据的区间。
	if _, ok := m.MinutesToCharge(0.2, 1); ok {
		t.Error("路过没有速度的区间，应该返回不知道")
	}
	// 已经高于目标，不用充。
	if v, ok := m.MinutesToCharge(0.9, 0.5); !ok || v != 0 {
		t.Errorf("已达目标 = %v %v", v, ok)
	}
}

func TestReport_行程撑不住时给出耗尽时间(t *testing.T) {
	m := BatteryModel{DrainOK: true, DrainPercentPerHour: 10, DrainObservedHours: 30, Charge: []ChargeBand{
		{PercentPerMinute: 1, OK: true, FromPercent: 0, ToPercent: 50},
		{PercentPerMinute: 0.5, OK: true, FromPercent: 50, ToPercent: 80},
		{PercentPerMinute: 0.25, OK: true, FromPercent: 80, ToPercent: 100},
	}}
	r := m.Report(&BatteryNow{Percent: 40, State: "unplugged", AgeMinutes: 3}, BatteryRequest{TripHours: 6, TargetPercent: 80})
	if r.Trip == nil || r.Trip.PredictedEndPercent != 0 || r.Trip.RunsOutAfterHours != 4 {
		t.Errorf("行程预测 = %+v，40%% 每小时掉 10 个点撑 4 小时", r.Trip)
	}
	// 40% → 50% 10 分钟，50% → 80% 60 分钟。
	if r.MinutesToTarget == nil || *r.MinutesToTarget != 70 {
		t.Errorf("充到 80%% = %v，想要 70 分钟", r.MinutesToTarget)
	}
	if r.MinutesToFull == nil || *r.MinutesToFull != 150 {
		t.Errorf("充满 = %v，想要 150 分钟", r.MinutesToFull)
	}
}

func TestReport_数据过期和没有记录要写进说明(t *testing.T) {
	r := BatteryModel{}.Report(nil, BatteryRequest{})
	if len(r.Notes) < 2 || r.Now != nil {
		t.Errorf("没有记录时应当说明: %+v", r)
	}
	stale := BatteryModel{}.Report(&BatteryNow{Percent: 50, AgeMinutes: 120}, BatteryRequest{})
	found := false
	for _, n := range stale.Notes {
		if strings.Contains(n, "120 分钟前") {
			found = true
		}
	}
	if !found {
		t.Errorf("过期的电量应当提醒: %v", stale.Notes)
	}
}

func TestService_Battery_从库里读出并学习(t *testing.T) {
	s, _, now := setup(t)
	ctx := context.Background()
	d, _ := pairedDevice(t, s)
	// 造 3 小时的放电记录（每 5 分钟一条）和结尾 10 分钟前的最新一条。
	*now = t0.Add(4 * time.Hour)
	var items []Item
	for i, sm := range series(t0, 0.90, -0.05, 37, false) {
		p, _ := json.Marshal(map[string]any{"batteryLevel": sm.Level, "batteryState": "unplugged"})
		items = append(items, Item{Key: fmt.Sprintf("k%d", i), CapturedAt: sm.At, Payload: p})
	}
	if _, err := s.Ingest(ctx, d, "device.status", items); err != nil {
		t.Fatal(err)
	}
	r, err := s.Battery(ctx, "", BatteryRequest{TripHours: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r.Now == nil || r.Now.State != "unplugged" || r.Now.AgeMinutes != 60 {
		t.Errorf("现状 = %+v", r.Now)
	}
	if r.DrainPerHour == nil || !near(*r.DrainPerHour, 3, 0.2) {
		t.Errorf("掉电速度 = %v", r.DrainPerHour)
	}
	if r.Trip == nil {
		t.Error("应当有行程预测")
	}
}
