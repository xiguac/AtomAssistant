package device

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// 电量预估：从手机上报的「设备状态」历史里算出这个人这台手机的充电速度和掉电速度。
//
// 学习在服务器上用代码完成，不经过模型：模型只拿到几个现成的数字，规划行程时自己判断
// 要不要提前充电、带不带充电宝。不要求精确——粗略的区间估算够用。
//
// 两条原则：
//  1. 数据不够就说不够，不用一个拍脑袋的默认值顶上。
//  2. 充电速度按电量区间分开估：手机通常 80% 以内快、之后慢，一个平均数会差很远。

const (
	// minGap 是相邻两条记录之间最长能算进速度的间隔的下限。实际取「上报间隔的 2.5 倍」与它中较大的：
	// 间隔太长说明中间断过，手机在这段时间里干了什么不知道，不能拿来算速度。
	minGap = 15 * time.Minute
	// 观测时间不足这么久的区间不给结论。
	minChargeMinutes = 30.0
	minDrainMinutes  = 120.0
	// batteryWindow 是用来学习的历史窗口。
	batteryWindow = 30 * 24 * time.Hour
	// batterySampleLimit 是一次最多读多少条设备状态：每 5 分钟一条（最密的情况），30 天约 8600 条。
	batterySampleLimit = 20000
	// batteryCacheTTL 内重复问不重新学习。
	batteryCacheTTL = 10 * time.Minute
	// staleAfter 之后，最新一条电量就不能当成「现在」了。
	staleAfter = 30 * time.Minute
)

// batteryBands 是充电速度分区的边界（电量比例），三段：到 50%、50%–80%、80% 以上。
var batteryBands = [...]float64{0, 0.5, 0.8, 1.0}

// BatterySample 是一次电量记录。
type BatterySample struct {
	At       time.Time
	Level    float64 // 0 到 1
	Charging bool
	// Unplugged 为真表示明确是「未充电」（排除了充满、未知这两种既不充也不放的状态）。
	Unplugged bool
}

// ChargeBand 是一个电量区间里实测的充电速度。
type ChargeBand struct {
	FromPercent int `json:"from_percent"`
	ToPercent   int `json:"to_percent"`
	// PercentPerMinute 在 OK 为假时没有意义。
	PercentPerMinute float64 `json:"percent_per_minute"`
	ObservedMinutes  float64 `json:"observed_minutes"`
	OK               bool    `json:"ok"`
}

// BatteryModel 是学出来的结果。
type BatteryModel struct {
	Samples int          `json:"samples"`
	Charge  []ChargeBand `json:"charge"`
	// DrainPercentPerHour 是没充电时平均每小时掉多少个百分点；DrainOK 为假时没有意义。
	DrainPercentPerHour float64 `json:"drain_percent_per_hour"`
	DrainObservedHours  float64 `json:"drain_observed_hours"`
	DrainOK             bool    `json:"drain_ok"`
}

// gapFor 是上报间隔为 syncMinutes 分钟时，相邻两条记录最长能算进速度的间隔。
func gapFor(syncMinutes int) time.Duration {
	g := time.Duration(float64(syncMinutes)*2.5) * time.Minute
	if g < minGap {
		return minGap
	}
	return g
}

// LearnBattery 从按时间先后排好的记录里学习。maxGap 是相邻两条记录之间最长能算进速度的间隔，
// 超过就当中间断过，不算。
func LearnBattery(samples []BatterySample, maxGap time.Duration) BatteryModel {
	m := BatteryModel{Samples: len(samples)}
	var delta, minutes [len(batteryBands) - 1]float64
	var drop, dropMinutes float64
	for i := 1; i < len(samples); i++ {
		a, b := samples[i-1], samples[i]
		dt := b.At.Sub(a.At)
		if dt <= 0 || dt > maxGap {
			continue
		}
		dl := b.Level - a.Level
		switch {
		case a.Charging && b.Charging:
			// 充电中电量不会降；降了说明是读数抖动，丢掉这一对。
			if dl < 0 {
				continue
			}
			band := bandOf(a.Level)
			delta[band] += dl
			minutes[band] += dt.Minutes()
		case a.Unplugged && b.Unplugged:
			// 没充电时电量不会升，同理。
			if dl > 0 {
				continue
			}
			drop += -dl
			dropMinutes += dt.Minutes()
		}
	}
	for i := range delta {
		band := ChargeBand{
			FromPercent: int(math.Round(batteryBands[i] * 100)), ToPercent: int(math.Round(batteryBands[i+1] * 100)),
			ObservedMinutes: round1(minutes[i]),
		}
		if minutes[i] >= minChargeMinutes && delta[i] > 0 {
			band.PercentPerMinute = round2(delta[i] * 100 / minutes[i])
			band.OK = true
		}
		m.Charge = append(m.Charge, band)
	}
	m.DrainObservedHours = round1(dropMinutes / 60)
	if dropMinutes >= minDrainMinutes && drop > 0 {
		m.DrainPercentPerHour = round2(drop * 100 / (dropMinutes / 60))
		m.DrainOK = true
	}
	return m
}

func bandOf(level float64) int {
	for i := len(batteryBands) - 2; i > 0; i-- {
		if level >= batteryBands[i] {
			return i
		}
	}
	return 0
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// MinutesToCharge 估算从 from 充到 to（都是 0 到 1 的电量比例）要多少分钟。
// 路过的任何一个区间没有实测速度就返回 false：宁可说不知道，也不拿别的区间的速度顶。
func (m BatteryModel) MinutesToCharge(from, to float64) (float64, bool) {
	if to <= from {
		return 0, true
	}
	total := 0.0
	for i, band := range m.Charge {
		lo, hi := math.Max(from, batteryBands[i]), math.Min(to, batteryBands[i+1])
		if hi <= lo {
			continue
		}
		if !band.OK {
			return 0, false
		}
		total += (hi - lo) * 100 / band.PercentPerMinute
	}
	return math.Round(total), true
}

// BatteryTrip 是对一段行程的电量预测。
type BatteryTrip struct {
	Hours               float64 `json:"hours"`
	PredictedEndPercent float64 `json:"predicted_end_percent"`
	// RunsOutAfterHours 非零表示按这个掉电速度，撑不到行程结束，电会在这么多小时后耗尽。
	RunsOutAfterHours float64 `json:"runs_out_after_hours,omitempty"`
}

// BatteryNow 是最新一条电量记录。
type BatteryNow struct {
	Percent    float64 `json:"percent"`
	State      string  `json:"state"`
	AgeMinutes float64 `json:"age_minutes"`
}

// BatteryReport 是给助手的电量预估：现状、实测速度、充电要多久、这趟行程撑不撑得住。
type BatteryReport struct {
	Now             *BatteryNow  `json:"now,omitempty"`
	Charge          []ChargeBand `json:"charge_rates"`
	DrainPerHour    *float64     `json:"drain_percent_per_hour,omitempty"`
	DrainObservedH  float64      `json:"drain_observed_hours"`
	MinutesToFull   *float64     `json:"minutes_to_full,omitempty"`
	MinutesToTarget *float64     `json:"minutes_to_target,omitempty"`
	Trip            *BatteryTrip `json:"trip,omitempty"`
	// Notes 是需要助手知道的局限：数据不够、数据过期、估算不出来的原因。
	Notes []string `json:"notes"`
}

// BatteryRequest 是预估的参数。
type BatteryRequest struct {
	// TripHours 大于 0 时预测一趟这么久的行程（期间不充电）会不会没电。
	TripHours float64
	// TargetPercent 大于 0 时估算充到这个电量要多久（0–100）。
	TargetPercent float64
}

// Report 按模型和当前电量生成预估。now 为空表示没有任何电量记录。
func (m BatteryModel) Report(now *BatteryNow, req BatteryRequest) BatteryReport {
	r := BatteryReport{Now: now, Charge: m.Charge, DrainObservedH: m.DrainObservedHours, Notes: []string{}}
	if m.DrainOK {
		v := m.DrainPercentPerHour
		r.DrainPerHour = &v
	} else {
		r.Notes = append(r.Notes, fmt.Sprintf("没充电时的掉电速度还估不出来：只观测到 %.1f 小时，至少要 %.0f 小时", m.DrainObservedHours, minDrainMinutes/60))
	}
	if now == nil {
		r.Notes = append(r.Notes, "没有任何电量记录：手机还没上报过设备状态")
		return r
	}
	if now.AgeMinutes > staleAfter.Minutes() {
		r.Notes = append(r.Notes, fmt.Sprintf("最新的电量是 %.0f 分钟前的，手机这段时间没上报，现在的电量可能已经不一样", now.AgeMinutes))
	}
	level := now.Percent / 100
	if mins, ok := m.MinutesToCharge(level, 1); ok {
		r.MinutesToFull = &mins
	} else {
		r.Notes = append(r.Notes, "充满要多久估不出来：有的电量区间还没有足够的充电记录（见 charge_rates 里 ok 为 false 的区间）")
	}
	if req.TargetPercent > 0 {
		target := math.Min(req.TargetPercent, 100) / 100
		if mins, ok := m.MinutesToCharge(level, target); ok {
			r.MinutesToTarget = &mins
		} else {
			r.Notes = append(r.Notes, "充到目标电量要多久估不出来：路过的电量区间缺少足够的充电记录")
		}
	}
	if req.TripHours > 0 && m.DrainOK {
		end := now.Percent - m.DrainPercentPerHour*req.TripHours
		trip := &BatteryTrip{Hours: req.TripHours, PredictedEndPercent: round1(math.Max(end, 0))}
		if end <= 0 {
			trip.RunsOutAfterHours = round1(now.Percent / m.DrainPercentPerHour)
		}
		r.Trip = trip
		r.Notes = append(r.Notes, "掉电速度是这部手机平时没充电时的平均值；导航、拍照、视频这类重度使用时会更快，出行当天按更保守的估")
	}
	return r
}

// batteryCache 缓存最近一次学习的结果和最新一条记录。「现在的电量有多旧」每次现算，不缓存。
type batteryCache struct {
	mu      sync.Mutex
	at      time.Time
	dev     string
	model   BatteryModel
	last    BatterySample
	hasLast bool
}

// Battery 学习电量并给出预估。有多台设备时要指定 deviceID。
func (s *Service) Battery(ctx context.Context, deviceID string, req BatteryRequest) (BatteryReport, error) {
	dev, err := s.pickDevice(ctx, deviceID)
	if err != nil {
		return BatteryReport{}, err
	}
	m, last, has, err := s.learn(ctx, dev)
	if err != nil {
		return BatteryReport{}, err
	}
	var now *BatteryNow
	if has {
		state := "unplugged"
		switch {
		case last.Charging:
			state = "charging"
		case !last.Unplugged:
			state = "full_or_unknown"
		}
		now = &BatteryNow{Percent: math.Round(last.Level * 100), State: state,
			AgeMinutes: math.Max(0, math.Round(s.now().Sub(last.At).Minutes()))}
	}
	return m.Report(now, req), nil
}

func (s *Service) learn(ctx context.Context, dev *Device) (BatteryModel, BatterySample, bool, error) {
	deviceID := dev.ID
	nowT := s.now()
	s.bat.mu.Lock()
	defer s.bat.mu.Unlock()
	if s.bat.dev == deviceID && nowT.Sub(s.bat.at) < batteryCacheTTL {
		return s.bat.model, s.bat.last, s.bat.hasLast, nil
	}
	rows, err := s.Store.QueryData(ctx, Query{DeviceID: deviceID, Kind: "device.status", Since: nowT.Add(-batteryWindow), Limit: batterySampleLimit})
	if err != nil {
		return BatteryModel{}, BatterySample{}, false, err
	}
	samples := make([]BatterySample, 0, len(rows))
	for _, r := range rows {
		if sm, ok := parseBatterySample(r); ok {
			samples = append(samples, sm)
		}
	}
	// 读出来是新的在前；学习要按时间先后。
	sort.Slice(samples, func(i, j int) bool { return samples[i].At.Before(samples[j].At) })
	model := LearnBattery(samples, gapFor(syncMinutes(dev)))
	var last BatterySample
	has := len(samples) > 0
	if has {
		last = samples[len(samples)-1]
	}
	s.bat.at, s.bat.dev, s.bat.model, s.bat.last, s.bat.hasLast = nowT, deviceID, model, last, has
	return model, last, has, nil
}

// parseBatterySample 从一条设备状态里取出电量和充电状态。电量未知（null）的跳过。
func parseBatterySample(r Stored) (BatterySample, bool) {
	var p struct {
		Level *float64 `json:"batteryLevel"`
		State string   `json:"batteryState"`
	}
	if err := json.Unmarshal(r.Payload, &p); err != nil || p.Level == nil || *p.Level < 0 || *p.Level > 1 {
		return BatterySample{}, false
	}
	return BatterySample{At: r.CapturedAt, Level: *p.Level, Charging: p.State == "charging", Unplugged: p.State == "unplugged"}, true
}
