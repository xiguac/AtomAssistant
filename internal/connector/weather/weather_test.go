package weather

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// redirect 把发往 API Host 的请求改送到测试服务器，并记下请求原本要去的主机。
type redirect struct {
	to    *url.URL
	hosts []string
}

func (r *redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	r.hosts = append(r.hosts, req.URL.Scheme+"://"+req.URL.Host)
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(out)
}

const lookupBody = `{"code":"200","location":[
 {"name":"朝阳","id":"101010300","lat":"39.92149","lon":"116.48641","adm2":"北京","adm1":"北京市","country":"中国","utcOffset":"+08:00"},
 {"name":"朝阳","id":"101071201","lat":"41.57670","lon":"120.45117","adm2":"朝阳","adm1":"辽宁省","country":"中国","utcOffset":"+08:00"}]}`

const currentBody = `{"condition":{"text":"少云","code":"102"},"temperature":{"value":31.71,"unit":"°C"},
 "feelsLike":{"value":33.64,"unit":"°C"},"humidity":0.69,
 "wind":{"direction":{"degree":226,"compass":"sw"},"speed":{"value":4.74,"unit":"m/s"},"scale":3},
 "precipitation":{"amount":{"value":0,"unit":"mm"},"type":"none"},"visibility":{"value":29020,"unit":"m"}}`

const dailyBody = `{"days":[{"forecastStartTime":"2024-08-10T22:00Z","temperatureMax":{"value":29.94,"unit":"°C"},
 "temperatureMin":{"value":20.93,"unit":"°C"},"uvIndexMax":6,
 "daytime":{"condition":{"text":"小雨"},"wind":{"direction":{"compass":"w"},"scale":2},
  "precipitation":{"amount":{"value":0.75,"unit":"mm"},"probability":0.64}},
 "nighttime":{"condition":{"text":"晴间多云"},"wind":{"direction":{"compass":"nnw"},"scale":2},
  "precipitation":{"amount":{"value":0,"unit":"mm"},"probability":0}}}]}`

const alertBody = `{"alerts":[{"senderName":"临桂区气象台","eventType":{"name":"大风","code":"1006"},"severity":"minor",
 "color":{"code":"blue"},"expireTime":"2025-10-25T11:19+08:00","headline":"临桂区气象台更新大风蓝色预警信号",
 "description":"预计未来24小时内临桂将出现6级以上大风，\n请做好防范。"}]}`

// serve 起一个假的和风天气。alertStatus 不是 200 时预警接口报错。
func serve(t *testing.T, alertStatus int) (*Client, *redirect, *[]string) {
	t.Helper()
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		if r.Header.Get("X-QW-Api-Key") != "k-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/geo/v2/city/lookup":
			if r.URL.Query().Get("location") == "乌有乡" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"status":400,"type":"https://dev.qweather.com/docs/resource/error-code/#no-such-location","title":"No Such Location"}}`))
				return
			}
			_, _ = w.Write([]byte(lookupBody))
		case r.URL.Path == "/weather/v1/current/39.92/116.49":
			_, _ = w.Write([]byte(currentBody))
		case r.URL.Path == "/weather/v1/daily/39.92/116.49":
			_, _ = w.Write([]byte(dailyBody))
		case r.URL.Path == "/weatheralert/v1/current/39.92/116.49":
			if alertStatus != http.StatusOK {
				w.WriteHeader(alertStatus)
				_, _ = w.Write([]byte(`{"error":{"status":403,"title":"Forbidden"}}`))
				return
			}
			_, _ = w.Write([]byte(alertBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	to, _ := url.Parse(ts.URL)
	rt := &redirect{to: to}
	c := &Client{
		Host:   "abcxyz.qweatherapi.com",
		Secret: func(context.Context) (string, error) { return "k-123", nil },
		HTTP:   &http.Client{Transport: rt},
	}
	return c, rt, &seen
}

func TestForecast_先找地方再按坐标查(t *testing.T) {
	c, rt, seen := serve(t, http.StatusOK)
	rep, err := c.Forecast(context.Background(), "朝阳", "", 3)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 请求发往用户自己的 API Host，走 https；坐标保留两位小数；天数原样带上。
	for _, h := range rt.hosts {
		if h != "https://abcxyz.qweatherapi.com" {
			t.Fatalf("请求应发往 API Host: %v", rt.hosts)
		}
	}
	if len(*seen) != 4 || !strings.Contains((*seen)[2], "/weather/v1/daily/39.92/116.49?days=3") {
		t.Fatalf("请求不对: %v", *seen)
	}
	if rep.Place.Province != "北京市" || len(rep.Others) != 1 || rep.Others[0] != "朝阳，辽宁省" {
		t.Fatalf("应取最匹配的地方并列出重名的: %+v", rep)
	}
	if rep.Now.Text != "少云" || rep.Now.Temp != "31.7°C" || rep.Now.Humidity != 69 || rep.Now.Wind != "西南风 3 级" {
		t.Fatalf("实时天气不对: %+v", rep.Now)
	}
	// 预报的开始时间是 UTC 的前一天 22 点，在东八区是 8 月 11 日。
	if len(rep.Days) != 1 || rep.Days[0].Date != "2024-08-11" || rep.Days[0].TempMax != "29.9°C" ||
		rep.Days[0].Daytime.Text != "小雨" || rep.Days[0].Daytime.PrecipChance != 64 || rep.Days[0].Night.Wind != "西北偏北风 2 级" {
		t.Fatalf("逐日预报不对: %+v", rep.Days)
	}
	if len(rep.Alerts) != 1 || rep.Alerts[0].Color != "blue" || strings.Contains(rep.Alerts[0].Description, "\n") || rep.AlertsNote != "" {
		t.Fatalf("预警不对: %+v", rep.Alerts)
	}
	// KEY 不出现在结果里。
	if out, _ := json.Marshal(rep); strings.Contains(string(out), "k-123") {
		t.Fatalf("结果里不应有 KEY: %s", out)
	}
}

func TestForecast_出错时说明白(t *testing.T) {
	ctx := context.Background()
	c, _, _ := serve(t, http.StatusForbidden)
	// 预警查不成：天气照常给，但要说明没查成。
	rep, err := c.Forecast(ctx, "朝阳", "北京", 1)
	if err != nil || len(rep.Days) != 1 || !strings.Contains(rep.AlertsNote, "不代表没有预警") {
		t.Fatalf("预警查不成不应让整个查询失败: %v %+v", err, rep)
	}
	if _, err := c.Forecast(ctx, "乌有乡", "", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("找不到的地方应报 ErrNotFound: %v", err)
	}
	if _, err := c.Forecast(ctx, "朝阳", "", 11); !errors.Is(err, ErrInvalid) {
		t.Fatalf("超过 10 天应被拒绝: %v", err)
	}
	// KEY 不对：如实报错，报错里没有 KEY。
	c.Secret = func(context.Context) (string, error) { return "wrong-key", nil }
	if _, err := c.Forecast(ctx, "朝阳", "", 1); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "API KEY") || strings.Contains(err.Error(), "wrong-key") {
		t.Fatalf("认证失败应如实报错: %v", err)
	}
}

func TestCheckHost(t *testing.T) {
	if got := NormalizeHost(" https://abcxyz.qweatherapi.com/ "); got != "abcxyz.qweatherapi.com" || CheckHost(got) != nil {
		t.Fatalf("粘贴来的地址应能整理成主机名: %q", got)
	}
	for _, bad := range []string{"", "localhost", "a.com/path", "user@a.com", "http://a.com", "a.com?x=1"} {
		if err := CheckHost(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q 不是合法的 API Host: %v", bad, err)
		}
	}
}

func TestForecast_地名里带着手机号的不发出去(t *testing.T) {
	c, _, seen := serve(t, http.StatusOK)
	c.HTTP = &http.Client{Transport: egress}
	if _, err := c.Forecast(context.Background(), "杭州 13812345678", "", 1); err == nil || len(*seen) != 0 {
		t.Fatalf("带着手机号的地名不应发出去: %v %v", err, *seen)
	}
}
