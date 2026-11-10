package device

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

// 「我现在大概在哪」：定位优先，定位明显离谱时才改用公网 IP 的归属地。
//
// 判断离谱的办法：把最新一次定位和上一次定位比，算出隐含的移动速度；超过 MaxPlausibleSpeed
// （默认 300 米/秒，比民航客机略快）就认为这次定位不可信。粗略位置每次都带 10 到 20 公里的随机偏移，
// 所以涉及粗略位置的比较要先把可能的偏移量扣掉，不然正常走路也会被算成“瞬移”。

const (
	// MaxPlausibleSpeed 是人能达到的最快移动速度（米/秒）。比这更快就当成定位出了问题。
	MaxPlausibleSpeed = 300.0
	// coarseOffsetMeters 是粗略位置最大的偏移量。
	coarseOffsetMeters = 20_000.0
	// minInterval 是算速度时用的最短间隔：两次定位几乎同时却相隔很远，应当判成离谱，而不是除以零。
	minInterval = 30 * time.Second
	// whereWindow 是往前看多久的定位。
	whereWindow = 24 * time.Hour
)

// WhereFix 是一次定位。
type WhereFix struct {
	Source     string    `json:"source"` // gps（精确）或 coarse（粗略）
	Latitude   float64   `json:"latitude"`
	Longitude  float64   `json:"longitude"`
	City       string    `json:"city,omitempty"`
	AgeMinutes float64   `json:"age_minutes"`
	At         time.Time `json:"-"`
}

// WhereIP 是最近一次记下的公网 IP 归属地。
type WhereIP struct {
	Region
	AgeMinutes float64 `json:"age_minutes"`
}

// WhereBest 是最终采信的答案。
type WhereBest struct {
	// Source 是 gps、coarse、ip 之一。
	Source string `json:"source"`
	// Precision 是人话版的精度说明。
	Precision string   `json:"precision"`
	City      string   `json:"city,omitempty"`
	Province  string   `json:"province,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
}

// WhereReport 是给助手的“现在大概在哪”。
type WhereReport struct {
	Best *WhereBest `json:"best,omitempty"`
	// Location 是最新一次定位（精确或粗略），IP 是最近一次公网 IP 归属地；两个都给，由助手看情况。
	Location *WhereFix `json:"latest_fix,omitempty"`
	IP       *WhereIP  `json:"ip_region,omitempty"`
	// Implausible 为真表示最新一次定位和上一次相比，移动速度不合理，已改用 IP 归属地。
	Implausible bool     `json:"fix_implausible"`
	SpeedMS     float64  `json:"implied_speed_ms,omitempty"`
	Notes       []string `json:"notes"`
}

// Where 综合已有的位置信息，给出现在大概在哪。
func (s *Service) Where(ctx context.Context, deviceID string) (WhereReport, error) {
	dev, err := s.pickDevice(ctx, deviceID)
	if err != nil {
		return WhereReport{}, err
	}
	now := s.now()
	since := now.Add(-whereWindow)
	var fixes []WhereFix
	for _, kind := range []string{"location.now", "location.track", "location.coarse"} {
		rows, err := s.Store.QueryData(ctx, Query{DeviceID: dev.ID, Kind: kind, Since: since, Limit: 50})
		if err != nil {
			return WhereReport{}, err
		}
		for _, r := range rows {
			if f, ok := parseFix(kind, r, now); ok {
				fixes = append(fixes, f)
			}
		}
	}
	var ip *WhereIP
	rows, err := s.Store.QueryData(ctx, Query{DeviceID: dev.ID, Kind: "ip.region", Since: since, Limit: 1})
	if err != nil {
		return WhereReport{}, err
	}
	if len(rows) > 0 {
		var reg Region
		if json.Unmarshal(rows[0].Payload, &reg) == nil {
			ip = &WhereIP{Region: reg, AgeMinutes: minutesSince(now, rows[0].CapturedAt)}
		}
	}
	return decideWhere(fixes, ip), nil
}

func minutesSince(now, t time.Time) float64 { return math.Max(0, math.Round(now.Sub(t).Minutes())) }

func parseFix(kind string, r Stored, now time.Time) (WhereFix, bool) {
	var p struct {
		Lat   *float64 `json:"latitude"`
		Lon   *float64 `json:"longitude"`
		Place struct {
			City string `json:"city"`
		} `json:"place"`
	}
	if json.Unmarshal(r.Payload, &p) != nil || p.Lat == nil || p.Lon == nil {
		return WhereFix{}, false
	}
	src := "gps"
	if kind == "location.coarse" {
		src = "coarse"
	}
	return WhereFix{Source: src, Latitude: *p.Lat, Longitude: *p.Lon, City: p.Place.City,
		AgeMinutes: minutesSince(now, r.CapturedAt), At: r.CapturedAt}, true
}

// decideWhere 是纯逻辑：给定定位和 IP 归属地，决定采信哪个。fixes 不要求有序。
func decideWhere(fixes []WhereFix, ip *WhereIP) WhereReport {
	rep := WhereReport{IP: ip, Notes: []string{}}
	sort.Slice(fixes, func(i, j int) bool { return fixes[i].At.After(fixes[j].At) })

	var latest *WhereFix
	if len(fixes) > 0 {
		latest = &fixes[0]
		rep.Location = latest
	}
	if latest != nil && len(fixes) > 1 {
		if speed, bad := impliedSpeed(*latest, fixes[1]); bad {
			rep.Implausible = true
			rep.SpeedMS = math.Round(speed)
		}
	}

	fromFix := func(f WhereFix) *WhereBest {
		lat, lon := f.Latitude, f.Longitude
		b := &WhereBest{Source: f.Source, City: f.City, Latitude: &lat, Longitude: &lon, Precision: "精确到几十米"}
		if f.Source == "coarse" {
			b.Precision = "粗略位置：已随机偏移 10–20 公里，只能当作城市级"
		}
		return b
	}
	fromIP := func(ip *WhereIP) *WhereBest {
		return &WhereBest{Source: "ip", City: ip.City, Province: ip.Province, Precision: "公网 IP 归属地，只到市级，不一定准（代理、VPN 会偏）"}
	}

	switch {
	case latest != nil && !rep.Implausible:
		rep.Best = fromFix(*latest)
		if ip != nil && ip.AgeMinutes < latest.AgeMinutes {
			rep.Notes = append(rep.Notes, fmt.Sprintf("定位是 %.0f 分钟前的，比最近一次公网 IP 归属地旧；人如果这段时间移动过，可以参考 ip_region", latest.AgeMinutes))
		}
	case latest != nil && rep.Implausible && ip != nil:
		rep.Best = fromIP(ip)
		rep.Notes = append(rep.Notes, fmt.Sprintf("最新一次定位和上一次相比，隐含的移动速度约 %.0f 米/秒，超过 %.0f 米/秒，定位不可信，改用公网 IP 归属地", rep.SpeedMS, MaxPlausibleSpeed))
	case latest != nil && rep.Implausible:
		rep.Best = fromFix(*latest)
		rep.Notes = append(rep.Notes, fmt.Sprintf("最新一次定位的隐含移动速度约 %.0f 米/秒，不可信，但没有公网 IP 归属地可以替代，请谨慎使用", rep.SpeedMS))
	case ip != nil:
		rep.Best = fromIP(ip)
		rep.Notes = append(rep.Notes, "最近 24 小时没有定位，只有公网 IP 归属地，精度只到市级")
	default:
		rep.Notes = append(rep.Notes, "最近 24 小时既没有定位也没有公网 IP 归属地；需要的话用 device.request 请用户授权读取当前位置")
	}
	if ip != nil && ip.AgeMinutes > 120 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("公网 IP 归属地是 %.0f 分钟前的，可能已经过时", ip.AgeMinutes))
	}
	return rep
}

// impliedSpeed 算最新一次定位 a 相对上一次定位 b 的隐含速度，并判断是否离谱。
// 涉及粗略位置时，把它最大 20 公里的随机偏移从距离里扣掉再算。
func impliedSpeed(a, b WhereFix) (float64, bool) {
	dist := haversineMeters(a.Latitude, a.Longitude, b.Latitude, b.Longitude)
	for _, f := range []WhereFix{a, b} {
		if f.Source == "coarse" {
			dist -= coarseOffsetMeters
		}
	}
	if dist < 0 {
		dist = 0
	}
	dt := a.At.Sub(b.At)
	if dt < minInterval {
		dt = minInterval
	}
	speed := dist / dt.Seconds()
	return speed, speed > MaxPlausibleSpeed
}

func haversineMeters(la1, lo1, la2, lo2 float64) float64 {
	const r = 6_371_000.0
	rad := math.Pi / 180
	dLa, dLo := (la2-la1)*rad, (lo2-lo1)*rad
	a := math.Sin(dLa/2)*math.Sin(dLa/2) + math.Cos(la1*rad)*math.Cos(la2*rad)*math.Sin(dLo/2)*math.Sin(dLo/2)
	return 2 * r * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
