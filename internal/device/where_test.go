package device

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var w0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func fix(src string, lat, lon float64, ageMin float64) WhereFix {
	return WhereFix{Source: src, Latitude: lat, Longitude: lon, AgeMinutes: ageMin, At: w0.Add(-time.Duration(ageMin) * time.Minute)}
}

func shanghaiIP() *WhereIP {
	return &WhereIP{Region: Region{Country: "中国", Province: "上海", City: "上海市", ISP: "电信"}, AgeMinutes: 3}
}

func TestParseRegion(t *testing.T) {
	r := ParseRegion("中国|广东省|深圳市|电信|CN")
	if r.Country != "中国" || r.Province != "广东省" || r.City != "深圳市" || r.ISP != "电信" {
		t.Errorf("解析结果 = %+v", r)
	}
	r = ParseRegion("中国|0|0|0|CN")
	if r.Province != "" || r.City != "" || r.ISP != "" || r.Country != "中国" {
		t.Errorf("库里的 0 应当当作没有: %+v", r)
	}
}

func TestDecideWhere_定位正常就用定位(t *testing.T) {
	// 20 分钟里从人民广场走到 1.2 公里外：很正常。
	rep := decideWhere([]WhereFix{fix("gps", 31.2304, 121.4737, 20), fix("gps", 31.2400, 121.4800, 0)}, shanghaiIP())
	if rep.Implausible || rep.Best == nil || rep.Best.Source != "gps" {
		t.Errorf("正常移动应当采信定位: %+v", rep)
	}
}

func TestDecideWhere_瞬移就改用IP(t *testing.T) {
	// 10 分钟里从上海到北京（约 1070 公里）：每秒 1700 多米，不可能。
	rep := decideWhere([]WhereFix{fix("gps", 31.23, 121.47, 10), fix("gps", 39.90, 116.40, 0)}, shanghaiIP())
	if !rep.Implausible || rep.Best == nil || rep.Best.Source != "ip" || rep.Best.City != "上海市" {
		t.Errorf("瞬移应当改用 IP 归属地: %+v", rep)
	}
	if rep.SpeedMS < MaxPlausibleSpeed {
		t.Errorf("隐含速度 = %v", rep.SpeedMS)
	}
}

func TestDecideWhere_瞬移但没有IP时仍给定位并提醒(t *testing.T) {
	rep := decideWhere([]WhereFix{fix("gps", 31.23, 121.47, 10), fix("gps", 39.90, 116.40, 0)}, nil)
	if rep.Best == nil || rep.Best.Source != "gps" || len(rep.Notes) == 0 {
		t.Errorf("没有 IP 可替代时应当给定位并写明不可信: %+v", rep)
	}
}

func TestDecideWhere_飞机的速度不算离谱(t *testing.T) {
	// 上海到北京 1070 公里，飞了 1 小时 55 分，约 155 米/秒。
	rep := decideWhere([]WhereFix{fix("gps", 31.23, 121.47, 115), fix("gps", 39.90, 116.40, 0)}, shanghaiIP())
	if rep.Implausible {
		t.Errorf("飞机的速度不该被判成离谱: %+v", rep)
	}
}

func TestDecideWhere_粗略位置的随机偏移不会被误判成瞬移(t *testing.T) {
	// 人没动，两次粗略位置各偏了 20 公里、方向相反，相距 40 公里，间隔 5 分钟：
	// 不扣偏移是 133 米/秒（没超），这里再加大到 6 分钟内 40 公里 + 真实移动 0，扣掉 40 公里后应当是 0。
	a := fix("coarse", 31.23, 121.47, 5)
	b := fix("coarse", 31.23+0.36, 121.47, 0) // 约 40 公里
	rep := decideWhere([]WhereFix{a, b}, shanghaiIP())
	if rep.Implausible {
		t.Errorf("扣掉随机偏移后不该离谱: %+v", rep)
	}
	// 两次粗略位置相距 1000 公里、间隔 5 分钟：扣掉偏移也远超速度上限。
	rep = decideWhere([]WhereFix{fix("coarse", 31.23, 121.47, 5), fix("coarse", 39.90, 116.40, 0)}, shanghaiIP())
	if !rep.Implausible || rep.Best.Source != "ip" {
		t.Errorf("真正的瞬移还是要判出来: %+v", rep)
	}
}

func TestDecideWhere_没有定位只有IP和什么都没有(t *testing.T) {
	rep := decideWhere(nil, shanghaiIP())
	if rep.Best == nil || rep.Best.Source != "ip" || !strings.Contains(rep.Best.Precision, "市级") {
		t.Errorf("只有 IP 时应当用 IP 并说明只到市级: %+v", rep)
	}
	rep = decideWhere(nil, nil)
	if rep.Best != nil || len(rep.Notes) == 0 {
		t.Errorf("什么都没有时不该编一个: %+v", rep)
	}
}

type fakeRegion map[string]Region

func (f fakeRegion) Lookup(ip string) (Region, bool) { r, ok := f[ip]; return r, ok }

func TestNoteIP_归属地没变不重复写_变了才写(t *testing.T) {
	s, _, now := setup(t)
	s.IPRegion = fakeRegion{
		"1.1.1.1": {Country: "中国", Province: "上海", City: "上海市"},
		"2.2.2.2": {Country: "中国", Province: "北京", City: "北京市"},
	}
	d, _ := pairedDevice(t, s)
	ctx := context.Background()
	count := func() int {
		rows, _ := s.Store.QueryData(ctx, Query{Kind: "ip.region", Limit: 100})
		return len(rows)
	}
	s.NoteIP(ctx, d, "1.1.1.1")
	s.NoteIP(ctx, d, "1.1.1.1")
	if count() != 1 {
		t.Fatalf("同一个归属地短时间内只该写一次，写了 %d", count())
	}
	s.NoteIP(ctx, d, "2.2.2.2")
	if count() != 2 {
		t.Errorf("归属地变了要写，现在 %d 条", count())
	}
	*now = now.Add(ipNoteEvery + time.Minute)
	s.NoteIP(ctx, d, "2.2.2.2")
	if count() != 3 {
		t.Errorf("隔了很久同一个归属地也要再记一条（知道最近一次在这里），现在 %d 条", count())
	}
	s.NoteIP(ctx, d, "192.168.1.5") // 查不到：不写
	if count() != 3 {
		t.Error("查不到的 IP 不该写")
	}
}

func TestIngest_归属地由服务器产生_设备不能上报(t *testing.T) {
	s, _, _ := setup(t)
	d, _ := pairedDevice(t, s)
	_, err := s.Ingest(context.Background(), d, "ip.region", []Item{{Key: "x", CapturedAt: w0, Payload: json.RawMessage(`{"city":"北京市"}`)}})
	if err == nil || !strings.Contains(err.Error(), "服务器自己产生") {
		t.Errorf("设备上报服务器产生的种类应当被拒: %v", err)
	}
}

func TestClientIP_只信来自反代的转发头(t *testing.T) {
	mk := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	if got := clientIP(mk("127.0.0.1:5555", "8.8.8.8")); got != "8.8.8.8" {
		t.Errorf("本机反代转进来的请求应当取转发头，得到 %s", got)
	}
	if got := clientIP(mk("203.0.113.9:5555", "8.8.8.8")); got != "203.0.113.9" {
		t.Errorf("外网直连的请求不能信转发头（谁都能伪造），得到 %s", got)
	}
	if got := clientIP(mk("127.0.0.1:5555", "not-an-ip")); got != "127.0.0.1" {
		t.Errorf("转发头不是 IP 时用连接地址，得到 %s", got)
	}
}
