package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiguac/atomhelper/internal/connector/weather"
	"github.com/xiguac/atomhelper/internal/core"
)

// WeatherService 提供天气数据，由 *weather.Client 提供。
type WeatherService interface {
	Forecast(ctx context.Context, city, adm string, days int) (weather.Report, error)
}

// ToolWeather 查询天气。
const ToolWeather = "weather.forecast"

// defaultWeatherDays 是不写天数时查的天数。
const defaultWeatherDays = 3

var weatherTool = builtin(ToolWeather,
	`查一个城市或区县的天气：实时天气、从今天起若干天的逐日预报（白天与夜间的天气、气温、降水概率、风），`+
		`以及正在生效的气象预警。查天气用这个，不要用联网搜索，也不要凭印象说。`+
		`只写城市或区县的名字，不要写街道、小区这样的详细地址。`+
		`预报最多 10 天，更远的日子查不到，要如实告诉用户。`+
		`结果里有 other_places_with_this_name 说明有重名的地方：查到的不是想要的那个，就用 adm 指明上级行政区再查一次。`,
	`{"type":"object","properties":{"city":{"type":"string","description":"城市或区县的名字，如 杭州、朝阳"},`+
		`"adm":{"type":"string","description":"可选，上级行政区，用来区分重名，如 北京、辽宁"},`+
		`"days":{"type":"integer","description":"预报几天（含今天），1 到 10，默认 3"}},"required":["city"]}`,
	core.SideEffectReadOnly)

// runWeather 执行天气查询。
func (a *Actuator) runWeather(ctx context.Context, s *core.Step) (json.RawMessage, error) {
	if a.weather == nil {
		return nil, fmt.Errorf("agent: 未接入天气数据")
	}
	var in struct {
		City string `json:"city"`
		Adm  string `json:"adm"`
		Days int    `json:"days"`
	}
	if err := json.Unmarshal(s.Input, &in); err != nil {
		return nil, fmt.Errorf("agent: 解析天气查询参数失败: %w", err)
	}
	if strings.TrimSpace(in.City) == "" {
		return nil, fmt.Errorf("agent: 查天气需要 city")
	}
	if in.Days == 0 {
		in.Days = defaultWeatherDays
	}
	rep, err := a.weather.Forecast(ctx, in.City, in.Adm, in.Days)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rep)
}
