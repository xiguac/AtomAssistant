// Package weather 查天气：实时天气、逐日预报、正在生效的气象预警。
//
// 数据来自和风天气（QWeather）的开发服务，见 https://dev.qweather.com/docs/ 。
// 每个开发者有自己的 API Host（在控制台的「设置」里看），凭据用的是 API KEY，
// 放在请求头 X-QW-Api-Key 里。两样都由用户自己填，KEY 存在保险库里。
//
// 天气是按经纬度查的，所以每次先用城市搜索把地名换成坐标。
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/policy"
)

var (
	// ErrInvalid 表示参数或配置不合法。
	ErrInvalid = errors.New("weather: 参数不合法")
	// ErrNotFound 表示没有找到这个地方。
	ErrNotFound = errors.New("weather: 没有找到这个地方")
	// ErrUnavailable 表示天气数据取不到。
	ErrUnavailable = errors.New("weather: 天气数据暂时取不到")
)

const (
	// MaxDays 是逐日预报最多能查的天数。
	MaxDays = 10
	// maxBytes 是一次返回内容的大小上限。
	maxBytes = 1 << 20
	// maxAlertRunes 是预警正文保留的长度。它是气象台写的自由文本，只留够看明白是什么事的长度。
	maxAlertRunes = 200
	// maxCandidates 是重名时一并列出的其他地方的个数。
	maxCandidates = 4
)

// NormalizeHost 整理用户填的 API Host：去掉粘贴时带上的 https:// 和结尾的斜杠。
func NormalizeHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	return strings.TrimRight(s, "/")
}

// CheckHost 校验 API Host：只能是一个主机名，不带协议、路径和账号。
func CheckHost(host string) error {
	if host == "" {
		return fmt.Errorf("%w: 缺少 API Host", ErrInvalid)
	}
	u, err := url.Parse("https://" + host)
	if err != nil || u.Host != host || u.Hostname() != host || !strings.Contains(host, ".") {
		return fmt.Errorf("%w: API Host 应是一个主机名，如 abcxyz.qweatherapi.com", ErrInvalid)
	}
	return nil
}

// Client 是和风天气的客户端。
type Client struct {
	// Host 是开发者自己的 API Host，如 abcxyz.qweatherapi.com。
	Host string
	// Secret 在每次请求时给出 API KEY。
	Secret func(ctx context.Context) (string, error)
	// HTTP 为空时使用带超时和出站过滤的默认客户端。
	HTTP *http.Client
}

// egress 是查天气用的出站过滤：地名是模型写的，里面带着个人敏感信息的不发。
var egress = policy.NewEgressFilter(nil, core.PrivacyNormal, "天气查询")

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: egress}
}

// problem 是出错时返回的内容，见 https://dev.qweather.com/docs/resource/error-code/ 。
type problem struct {
	Error struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"error"`
}

// get 请求一个接口，把返回的 JSON 解到 out 里。
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if err := CheckHost(c.Host); err != nil {
		return err
	}
	if c.Secret == nil {
		return fmt.Errorf("%w: 缺少 API KEY", ErrInvalid)
	}
	key, err := c.Secret(ctx)
	if err != nil {
		return fmt.Errorf("weather: 取 API KEY 失败: %w", err)
	}
	u := url.URL{Scheme: "https", Host: c.Host, Path: path, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	req.Header.Set("X-QW-Api-Key", key)
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("%w: 读取返回内容失败: %v", ErrUnavailable, err)
	}
	if len(body) > maxBytes {
		return fmt.Errorf("%w: 返回内容超过 %d KB", ErrUnavailable, maxBytes>>10)
	}
	if resp.StatusCode != http.StatusOK {
		var p problem
		_ = json.Unmarshal(body, &p)
		if strings.HasSuffix(p.Error.Type, "#no-such-location") {
			return ErrNotFound
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("%w: 身份认证失败，请检查 API KEY", ErrUnavailable)
		case http.StatusForbidden:
			return fmt.Errorf("%w: 请求被拒绝（%s），请检查 API Host、额度与凭据的请求限制", ErrUnavailable, p.Error.Title)
		}
		if p.Error.Title != "" {
			return fmt.Errorf("%w: 和风天气返回 %s：%s %s", ErrUnavailable, resp.Status, p.Error.Title, p.Error.Detail)
		}
		return fmt.Errorf("%w: 和风天气返回 %s", ErrUnavailable, resp.Status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: 返回内容不是合法 JSON: %v", ErrUnavailable, err)
	}
	return nil
}

// Place 是城市搜索找到的一个地方。
type Place struct {
	Name string `json:"name"`
	// City 是它的上级行政区划，Province 是一级行政区域。
	City     string `json:"city,omitempty"`
	Province string `json:"province,omitempty"`
	Country  string `json:"country,omitempty"`

	lat, lon float64
	zone     *time.Location
}

func (p Place) String() string {
	parts := []string{p.Name}
	for _, s := range []string{p.City, p.Province} {
		if s != "" && s != parts[len(parts)-1] {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "，")
}

// Lookup 按名字搜索城市或区县，最匹配的在前。adm 可选，用上级行政区划排除重名。
func (c *Client) Lookup(ctx context.Context, name, adm string) ([]Place, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: 缺少城市名", ErrInvalid)
	}
	q := url.Values{"location": {name}, "number": {strconv.Itoa(maxCandidates + 1)}}
	if adm = strings.TrimSpace(adm); adm != "" {
		q.Set("adm", adm)
	}
	var body struct {
		Code     string `json:"code"`
		Location []struct {
			Name      string `json:"name"`
			Lat       string `json:"lat"`
			Lon       string `json:"lon"`
			Adm1      string `json:"adm1"`
			Adm2      string `json:"adm2"`
			Country   string `json:"country"`
			UTCOffset string `json:"utcOffset"`
		} `json:"location"`
	}
	if err := c.get(ctx, "/geo/v2/city/lookup", q, &body); err != nil {
		return nil, err
	}
	if body.Code == "404" || (body.Code == "200" && len(body.Location) == 0) {
		return nil, ErrNotFound
	}
	if body.Code != "200" {
		return nil, fmt.Errorf("%w: 城市搜索返回状态码 %s", ErrUnavailable, body.Code)
	}
	out := make([]Place, 0, len(body.Location))
	for _, l := range body.Location {
		lat, err1 := strconv.ParseFloat(l.Lat, 64)
		lon, err2 := strconv.ParseFloat(l.Lon, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		p := Place{Name: l.Name, City: l.Adm2, Province: l.Adm1, Country: l.Country, lat: lat, lon: lon, zone: time.UTC}
		// 预报里的时间是 UTC，要换成当地的日期得知道它和 UTC 差多少。
		if t, err := time.Parse("-07:00", l.UTCOffset); err == nil {
			_, offset := t.Zone()
			p.zone = time.FixedZone(l.UTCOffset, offset)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// quantity 是带单位的数值。
type quantity struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// String 把数值写成"31.7°C"这样，保留一位小数。
func (q quantity) String() string {
	return strconv.FormatFloat(math.Round(q.Value*10)/10, 'f', -1, 64) + q.Unit
}

type condition struct {
	Text string `json:"text"`
}

type wind struct {
	Direction struct {
		Compass string `json:"compass"`
	} `json:"direction"`
	Scale float64 `json:"scale"`
}

// compass 是风向的方位代码对应的说法。
var compass = map[string]string{
	"n": "北风", "nne": "东北偏北风", "ne": "东北风", "ene": "东北偏东风",
	"e": "东风", "ese": "东南偏东风", "se": "东南风", "sse": "东南偏南风",
	"s": "南风", "ssw": "西南偏南风", "sw": "西南风", "wsw": "西南偏西风",
	"w": "西风", "wnw": "西北偏西风", "nw": "西北风", "nnw": "西北偏北风",
	"none": "无持续风向", "vrb": "风向不定",
}

// String 把风写成"西南风 3 级"这样。
func (w wind) String() string {
	dir, ok := compass[w.Direction.Compass]
	if !ok {
		dir = w.Direction.Compass
	}
	return strings.TrimSpace(fmt.Sprintf("%s %d 级", dir, int(math.Round(w.Scale))))
}

func percent(ratio float64) int { return int(math.Round(ratio * 100)) }

// Now 是实时天气。
type Now struct {
	Text      string `json:"text"`
	Temp      string `json:"temp"`
	FeelsLike string `json:"feels_like"`
	// Humidity 是相对湿度，百分数。
	Humidity int    `json:"humidity_percent"`
	Wind     string `json:"wind"`
	// Precip 是过去一小时的降水量。
	Precip     string `json:"precip_last_hour"`
	Visibility string `json:"visibility"`
}

// Half 是一天里白天或夜间的预报。
type Half struct {
	Text string `json:"text"`
	Wind string `json:"wind"`
	// Precip 是这个时段的累计降水量，PrecipChance 是降水概率（百分数）。
	Precip       string `json:"precip"`
	PrecipChance int    `json:"precip_chance_percent"`
}

// Day 是一天的预报。
type Day struct {
	// Date 是当地的日期，格式 2006-01-02。
	Date    string `json:"date"`
	TempMax string `json:"temp_max"`
	TempMin string `json:"temp_min"`
	// Daytime 是当地 07:00 到 19:00，Night 是 19:00 到次日 07:00。
	Daytime Half `json:"daytime"`
	Night   Half `json:"night"`
	UVIndex int  `json:"uv_index_max"`
}

// Alert 是一条正在生效的气象预警。
type Alert struct {
	Headline string `json:"headline"`
	Event    string `json:"event,omitempty"`
	// Severity 取值 minor / moderate / severe / extreme / unknown，Color 是预警颜色。
	Severity    string `json:"severity,omitempty"`
	Color       string `json:"color,omitempty"`
	Sender      string `json:"sender,omitempty"`
	Expires     string `json:"expires,omitempty"`
	Description string `json:"description,omitempty"`
}

// Report 是一个地方的天气。
type Report struct {
	Place Place `json:"place"`
	// Others 是同名的其他地方。查到的不是想要的那个时，用上级行政区划指明再查。
	Others []string `json:"other_places_with_this_name,omitempty"`
	Now    Now      `json:"now"`
	Days   []Day    `json:"days"`
	Alerts []Alert  `json:"alerts"`
	// AlertsNote 在预警没查成时说明原因：没查成不等于没有预警。
	AlertsNote string `json:"alerts_note,omitempty"`
}

type halfBody struct {
	Condition     condition `json:"condition"`
	Wind          wind      `json:"wind"`
	Precipitation struct {
		Amount      quantity `json:"amount"`
		Probability float64  `json:"probability"`
	} `json:"precipitation"`
}

func (h halfBody) view() Half {
	return Half{
		Text: h.Condition.Text, Wind: h.Wind.String(),
		Precip: h.Precipitation.Amount.String(), PrecipChance: percent(h.Precipitation.Probability),
	}
}

// clip 把一段自由文本压成一行并限制长度。
func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// Forecast 查一个城市或区县的天气：实时天气、从今天起 days 天的逐日预报、正在生效的预警。
func (c *Client) Forecast(ctx context.Context, city, adm string, days int) (Report, error) {
	if days < 1 || days > MaxDays {
		return Report{}, fmt.Errorf("%w: 预报天数只能是 1 到 %d", ErrInvalid, MaxDays)
	}
	places, err := c.Lookup(ctx, city, adm)
	if err != nil {
		return Report{}, err
	}
	place := places[0]
	rep := Report{Place: place, Days: []Day{}, Alerts: []Alert{}}
	for _, p := range places[1:] {
		rep.Others = append(rep.Others, p.String())
	}
	// 坐标最多支持小数点后两位。
	at := fmt.Sprintf("%.2f/%.2f", place.lat, place.lon)

	var now struct {
		Condition     condition `json:"condition"`
		Temperature   quantity  `json:"temperature"`
		FeelsLike     quantity  `json:"feelsLike"`
		Humidity      float64   `json:"humidity"`
		Wind          wind      `json:"wind"`
		Precipitation struct {
			Amount quantity `json:"amount"`
		} `json:"precipitation"`
		Visibility quantity `json:"visibility"`
	}
	if err := c.get(ctx, "/weather/v1/current/"+at, nil, &now); err != nil {
		return Report{}, err
	}
	rep.Now = Now{
		Text: now.Condition.Text, Temp: now.Temperature.String(), FeelsLike: now.FeelsLike.String(),
		Humidity: percent(now.Humidity), Wind: now.Wind.String(),
		Precip: now.Precipitation.Amount.String(), Visibility: now.Visibility.String(),
	}

	var daily struct {
		Days []struct {
			ForecastStartTime string   `json:"forecastStartTime"`
			TemperatureMax    quantity `json:"temperatureMax"`
			TemperatureMin    quantity `json:"temperatureMin"`
			UVIndexMax        float64  `json:"uvIndexMax"`
			Daytime           halfBody `json:"daytime"`
			Nighttime         halfBody `json:"nighttime"`
		} `json:"days"`
	}
	if err := c.get(ctx, "/weather/v1/daily/"+at, url.Values{"days": {strconv.Itoa(days)}}, &daily); err != nil {
		return Report{}, err
	}
	for _, d := range daily.Days {
		start, err := time.Parse("2006-01-02T15:04Z07:00", d.ForecastStartTime)
		if err != nil {
			return Report{}, fmt.Errorf("%w: 预报里的时间 %q 看不懂", ErrUnavailable, d.ForecastStartTime)
		}
		rep.Days = append(rep.Days, Day{
			Date:    start.In(place.zone).Format("2006-01-02"),
			TempMax: d.TemperatureMax.String(), TempMin: d.TemperatureMin.String(),
			Daytime: d.Daytime.view(), Night: d.Nighttime.view(), UVIndex: int(math.Round(d.UVIndexMax)),
		})
	}

	var alerts struct {
		Alerts []struct {
			SenderName string `json:"senderName"`
			EventType  struct {
				Name string `json:"name"`
			} `json:"eventType"`
			Severity string `json:"severity"`
			Color    struct {
				Code string `json:"code"`
			} `json:"color"`
			ExpireTime  string `json:"expireTime"`
			Headline    string `json:"headline"`
			Description string `json:"description"`
		} `json:"alerts"`
	}
	// 预警查不成不该让整个查询失败：天气已经取到了。但要说明白，没查成不等于没有预警。
	if err := c.get(ctx, "/weatheralert/v1/current/"+at, url.Values{"localTime": {"true"}}, &alerts); err != nil {
		rep.AlertsNote = "预警没有查成，不代表没有预警：" + err.Error()
		return rep, nil
	}
	for _, a := range alerts.Alerts {
		rep.Alerts = append(rep.Alerts, Alert{
			Headline: clip(a.Headline, maxAlertRunes), Event: a.EventType.Name, Severity: a.Severity,
			Color: a.Color.Code, Sender: clip(a.SenderName, 40), Expires: a.ExpireTime,
			Description: clip(a.Description, maxAlertRunes),
		})
	}
	return rep, nil
}
