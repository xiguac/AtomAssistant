// Package holiday 提供法定节假日与调休安排：哪几天放假，哪几个周末要补班。
//
// 数据来自一个公开的日历订阅（iCalendar 格式），由用户在配置里指定。
// Apple 提供的中国大陆节假日订阅是 https://calendars.icloud.com/holidays/cn_zh.ics ，
// 里面用 X-APPLE-SPECIAL-DAY 标出了放假（WORK-HOLIDAY）和调休上班（ALTERNATE-WORKDAY）。
//
// 调休是每年由国务院另行公布的，算不出来，只能查。没有这份数据，
// 助手排日程时会把补班的周六当成休息日，把放假的周一当成工作日。
package holiday

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-ical"
)

// 日子的种类。
const (
	// KindOff 法定放假。
	KindOff = "off"
	// KindWorkday 调休上班：本该休息的周末要补班。
	KindWorkday = "workday"
	// KindFestival 节日或节气，不影响上不上班。
	KindFestival = "festival"
)

var (
	// ErrInvalid 表示参数或配置不合法。
	ErrInvalid = errors.New("holiday: 参数不合法")
	// ErrUnavailable 表示节假日数据取不到。
	ErrUnavailable = errors.New("holiday: 节假日数据暂时取不到")
)

const (
	// ttl 是数据的缓存时间。节假日安排一年公布一次，没必要每次都去取。
	ttl = 24 * time.Hour
	// maxBytes 是订阅内容的大小上限。
	maxBytes = 4 << 20
	// maxNameRunes 是名称的长度上限。名称是订阅方写的自由文本，只留够认出是哪个节日的长度。
	maxNameRunes = 20
	// MaxRange 是一次查询允许的最长跨度。
	MaxRange = 400 * 24 * time.Hour
)

// Entry 是一段连续的日子。
type Entry struct {
	// Start 与 End 是首尾两天（都含在内），格式 2006-01-02。只有一天时两者相同。
	Start string `json:"start"`
	End   string `json:"end"`
	Name  string `json:"name"`
	// Kind 取值 off（放假）/ workday（调休上班）/ festival（节日或节气，照常上班）。
	Kind string `json:"kind"`

	first, last time.Time
}

// Feed 是一个节假日订阅。
type Feed struct {
	// URL 是订阅地址，必须是 https。
	URL string
	// Client 为空时使用带超时的默认客户端。
	Client *http.Client

	mu        sync.Mutex
	entries   []Entry
	fetchedAt time.Time
}

// Check 校验订阅地址。
func (f *Feed) Check() error {
	u, err := url.Parse(strings.TrimSpace(f.URL))
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: 节假日订阅地址无法解析", ErrInvalid)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: 节假日订阅地址必须是 https", ErrInvalid)
	}
	if u.User != nil {
		return fmt.Errorf("%w: 节假日订阅地址不能带账号密码", ErrInvalid)
	}
	return nil
}

// load 返回订阅里的全部条目，必要时重新取一次。
//
// 取不到新的但手里有旧的，就继续用旧的：节假日安排不会一天一变，
// 订阅服务器抖一下不该让助手突然不知道国庆放几天。
func (f *Feed) load(ctx context.Context) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries != nil && time.Since(f.fetchedAt) < ttl {
		return f.entries, nil
	}
	entries, err := f.fetch(ctx)
	if err != nil {
		if f.entries != nil {
			return f.entries, nil
		}
		return nil, err
	}
	f.entries, f.fetchedAt = entries, time.Now()
	return entries, nil
}

func (f *Feed) fetch(ctx context.Context) ([]Entry, error) {
	if err := f.Check(); err != nil {
		return nil, err
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(f.URL), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: 订阅服务器返回 %s", ErrUnavailable, resp.Status)
	}
	data, err := ical.NewDecoder(io.LimitReader(resp.Body, maxBytes)).Decode()
	if err != nil {
		return nil, fmt.Errorf("%w: 订阅内容不是合法的日历: %v", ErrUnavailable, err)
	}
	return parse(data), nil
}

// parse 把订阅里的日程转成条目。只认全天的；格式不对的跳过。
func parse(data *ical.Calendar) []Entry {
	var out []Entry
	for _, e := range data.Events() {
		startProp := e.Props.Get(ical.PropDateTimeStart)
		if startProp == nil || len(startProp.Value) != len("20060102") {
			continue
		}
		first, err := time.Parse("20060102", startProp.Value)
		if err != nil {
			continue
		}
		last := first
		// 结束日在日历格式里是不含在内的。
		if endProp := e.Props.Get(ical.PropDateTimeEnd); endProp != nil && len(endProp.Value) == len("20060102") {
			if end, err := time.Parse("20060102", endProp.Value); err == nil && end.After(first) {
				last = end.AddDate(0, 0, -1)
			}
		}
		name, err := e.Props.Text(ical.PropSummary)
		if err != nil {
			continue
		}
		if name = cleanName(name); name == "" {
			continue
		}
		kind := KindFestival
		if special := e.Props.Get("X-APPLE-SPECIAL-DAY"); special != nil {
			switch strings.ToUpper(strings.TrimSpace(special.Value)) {
			case "WORK-HOLIDAY":
				kind = KindOff
			case "ALTERNATE-WORKDAY":
				kind = KindWorkday
			}
		}
		out = append(out, Entry{
			Start: first.Format("2006-01-02"), End: last.Format("2006-01-02"),
			Name: name, Kind: kind, first: first, last: last,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].first.Before(out[j].first) })
	return out
}

// cleanName 整理节日名称：去掉控制字符和换行，限制长度。
func cleanName(s string) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > maxNameRunes {
		s = string(r[:maxNameRunes])
	}
	return s
}

// Between 返回与 [from, to] 这些天有交集的条目，按开始日期排序。只看日期，不看钟点。
func (f *Feed) Between(ctx context.Context, from, to time.Time) ([]Entry, error) {
	day := func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	first, last := day(from), day(to)
	if last.Before(first) {
		return nil, fmt.Errorf("%w: 结束日期不能早于开始日期", ErrInvalid)
	}
	if last.Sub(first) > MaxRange {
		return nil, fmt.Errorf("%w: 一次最多查一年", ErrInvalid)
	}
	all, err := f.load(ctx)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, e := range all {
		if !e.last.Before(first) && !e.first.After(last) {
			out = append(out, e)
		}
	}
	return out, nil
}
