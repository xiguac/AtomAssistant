// Package calendar 是日历连接器：用 CalDAV（RFC 4791）直连用户的日历。
//
// iCloud、Fastmail、Nextcloud、群晖、Radicale 用的都是这个协议，所以这里不绑定任何一家。
// 不把它套成 MCP server 的理由见 docs/截图理解与连接器.md §0：协议稳定、两端自控，
// 套一层只会多出进程边界和 schema 转换。
//
// 三条约束：
//
//  1. **密码不落在这里。** 每次调用现取、用完即弃，来源是保险库（见 internal/vault）。
//  2. **只动日历集合里的东西。** 改和删都按对象路径来，而路径是模型抄过来的；
//     这里逐次核对它确实落在某个日历集合之内，否则一条抄错或被诱导的路径
//     就能删掉服务器上别的资源。
//  3. **读回来的标题、地点、备注是别人也能写的。** 陌生人发来的会议邀请会出现在日历里，
//     调用方必须把这些内容当作外部内容对待。
package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	// 容器和 Windows 上常常没有系统时区库，而日程里的 TZID 必须能解析。
	_ "time/tzdata"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/xiguac/atomassistant/internal/core"
)

var (
	// ErrInvalid 表示参数不合法。
	ErrInvalid = errors.New("calendar: 参数不合法")
	// ErrNotFound 表示没有这个日历或日程。
	ErrNotFound = errors.New("calendar: 没有这个日历或日程")
	// ErrUnsupported 表示这个操作目前不支持。
	ErrUnsupported = errors.New("calendar: 暂不支持")
)

// eventProps 是查询日程时向服务器要的属性：展开重复日程和展示所需的全部。
var eventProps = []string{
	ical.PropUID, ical.PropSummary, ical.PropLocation, ical.PropDescription, ical.PropOrganizer,
	ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropDuration,
	ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates, ical.PropRecurrenceID,
	ical.PropStatus,
}

// MaxEvents 是一次查询最多返回的日程数。结果要进模型的上下文。
const MaxEvents = 200

// MaxRange 是一次查询允许的最长时间跨度。重复日程要在本地逐次展开，跨度不能无限大。
const MaxRange = 366 * 24 * time.Hour

// discoveryTTL 是日历列表的缓存时间。发现过程要三次往返，没必要每次调用都做。
const discoveryTTL = 10 * time.Minute

// Client 连接一个 CalDAV 账户。
type Client struct {
	// Endpoint 是服务器地址，如 https://caldav.icloud.com。
	Endpoint string
	Username string
	// Secret 在每次调用时取密码。
	Secret func(ctx context.Context) (string, error)
	// Default 是新建日程时默认写入的日历名，空表示账户里只有一个日历时用那一个。
	Default string
	// Loc 是没有标明时区的时间按哪个时区理解，空值按本机时区。
	Loc *time.Location
	// HTTP 为空时使用带超时的默认客户端。
	HTTP *http.Client

	mu        sync.Mutex
	calendars []Calendar
	cachedAt  time.Time
}

// Calendar 是账户里的一个日历。
type Calendar struct {
	// Path 是日历集合在服务器上的路径。
	Path string `json:"-"`
	Name string `json:"name"`
}

// Event 是一条日程。
type Event struct {
	// ID 是日程对象在服务器上的路径，改和删时用它指明是哪一条。
	ID       string    `json:"id"`
	Calendar string    `json:"calendar"`
	Title    string    `json:"title"`
	Start    time.Time `json:"start"`
	// End 不含在内：10:00–11:00 的日程 End 是 11:00；全天日程的 End 是次日零点。
	End      time.Time `json:"end"`
	AllDay   bool      `json:"all_day,omitempty"`
	Location string    `json:"location,omitempty"`
	Notes    string    `json:"notes,omitempty"`
	// Organizer 是发起这条日程的人。自己建的日程没有这一项，别人发来的邀请才有。
	Organizer string `json:"organizer,omitempty"`
	// Recurring 为真表示这是一条重复日程中的一次。改或删它影响的是整个系列。
	Recurring bool `json:"recurring,omitempty"`
}

func (c *Client) loc() *time.Location {
	if c.Loc != nil {
		return c.Loc
	}
	return time.Local
}

// dial 取出密码并建立一个带认证的客户端。密码只活在这次调用里。
func (c *Client) dial(ctx context.Context) (*caldav.Client, error) {
	if c.Endpoint == "" || c.Username == "" || c.Secret == nil {
		return nil, fmt.Errorf("%w: 日历连接器缺少地址、账号或密码来源", ErrInvalid)
	}
	password, err := c.Secret(ctx)
	if err != nil {
		return nil, fmt.Errorf("calendar: 取日历密码失败: %w", err)
	}
	base := c.HTTP
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	client, err := caldav.NewClient(webdav.HTTPClientWithBasicAuth(base, c.Username, password), c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("calendar: 日历地址不可用: %w", err)
	}
	return client, nil
}

// Calendars 列出账户里能放日程的日历。
func (c *Client) Calendars(ctx context.Context) ([]Calendar, error) {
	c.mu.Lock()
	if c.calendars != nil && time.Since(c.cachedAt) < discoveryTTL {
		out := append([]Calendar(nil), c.calendars...)
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	client, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	principal, err := client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, fmt.Errorf("calendar: 登录日历服务器失败（地址、账号或密码不对？）: %w", err)
	}
	home, err := client.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("calendar: 找不到日历所在位置: %w", err)
	}
	found, err := client.FindCalendars(ctx, home)
	if err != nil {
		return nil, fmt.Errorf("calendar: 读取日历列表失败: %w", err)
	}
	var out []Calendar
	for _, f := range found {
		// 提醒事项也是一种日历集合（只收 VTODO），放不了日程。没声明支持什么的按能放处理。
		if len(f.SupportedComponentSet) > 0 && !contains(f.SupportedComponentSet, ical.CompEvent) {
			continue
		}
		name := strings.TrimSpace(f.Name)
		if name == "" {
			name = path.Base(strings.TrimSuffix(f.Path, "/"))
		}
		out = append(out, Calendar{Path: f.Path, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	c.mu.Lock()
	c.calendars, c.cachedAt = out, time.Now()
	c.mu.Unlock()
	return append([]Calendar(nil), out...), nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

// pick 按名字选出日历。name 为空时用默认日历。
func (c *Client) pick(ctx context.Context, name string) (Calendar, error) {
	cals, err := c.Calendars(ctx)
	if err != nil {
		return Calendar{}, err
	}
	names := make([]string, 0, len(cals))
	for _, cal := range cals {
		names = append(names, cal.Name)
	}
	if name = strings.TrimSpace(name); name == "" {
		name = c.Default
	}
	if name == "" {
		if len(cals) == 1 {
			return cals[0], nil
		}
		return Calendar{}, fmt.Errorf("%w: 账户里有多个日历（%s），需要指明写进哪一个",
			ErrInvalid, strings.Join(names, "、"))
	}
	for _, cal := range cals {
		if cal.Name == name {
			return cal, nil
		}
	}
	return Calendar{}, fmt.Errorf("%w: 没有名为 %q 的日历（现有：%s）", ErrNotFound, name, strings.Join(names, "、"))
}

// owner 核对一个对象路径确实落在某个日历集合之内，返回它所属的日历。
func (c *Client) owner(ctx context.Context, id string) (Calendar, error) {
	id = strings.TrimSpace(id)
	// 路径里不能有 . 和 .. 这样的段：/cal/../别处 清理之后就跑到日历外面去了。
	if id == "" || !strings.HasPrefix(id, "/") || path.Clean(id) != strings.TrimSuffix(id, "/") {
		return Calendar{}, fmt.Errorf("%w: 日程 id 不合法", ErrInvalid)
	}
	cals, err := c.Calendars(ctx)
	if err != nil {
		return Calendar{}, err
	}
	for _, cal := range cals {
		dir := strings.TrimSuffix(cal.Path, "/") + "/"
		if strings.HasPrefix(id, dir) && len(id) > len(dir) {
			return cal, nil
		}
	}
	return Calendar{}, fmt.Errorf("%w: %s 不在任何一个日历里", ErrNotFound, id)
}

// Events 列出 [from, to) 之间的日程，按开始时间排序。calendarName 为空表示查全部日历。
// 重复日程按每一次分别列出。truncated 为真表示结果超过上限被截断了。
func (c *Client) Events(ctx context.Context, calendarName string, from, to time.Time) (events []Event, truncated bool, err error) {
	if !to.After(from) {
		return nil, false, fmt.Errorf("%w: 结束时间必须晚于开始时间", ErrInvalid)
	}
	if to.Sub(from) > MaxRange {
		return nil, false, fmt.Errorf("%w: 一次最多查一年", ErrInvalid)
	}
	var cals []Calendar
	if strings.TrimSpace(calendarName) == "" {
		if cals, err = c.Calendars(ctx); err != nil {
			return nil, false, err
		}
	} else {
		cal, err := c.pick(ctx, calendarName)
		if err != nil {
			return nil, false, err
		}
		cals = []Calendar{cal}
	}
	client, err := c.dial(ctx)
	if err != nil {
		return nil, false, err
	}
	query := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			// 逐个点名要哪些属性，不用"全部属性"：iCloud 对后者返回的是只有外壳、里面空空的日程。
			Name: ical.CompCalendar, Props: []string{ical.PropVersion, ical.PropProductID},
			Comps: []caldav.CalendarCompRequest{{Name: ical.CompEvent, Props: eventProps}},
		},
		CompFilter: caldav.CompFilter{
			Name:  ical.CompCalendar,
			Comps: []caldav.CompFilter{{Name: ical.CompEvent, Start: from.UTC(), End: to.UTC()}},
		},
	}
	for _, cal := range cals {
		objects, err := client.QueryCalendar(ctx, cal.Path, query)
		if err != nil {
			return nil, false, fmt.Errorf("calendar: 查询日历 %s 失败: %w", cal.Name, err)
		}
		for _, obj := range objects {
			if obj.Data == nil {
				continue
			}
			events = append(events, c.occurrences(obj.Path, cal.Name, obj.Data, from, to)...)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
	if len(events) > MaxEvents {
		return events[:MaxEvents], true, nil
	}
	return events, false, nil
}

// occurrences 把一个日程对象展开成落在 [from, to) 之内的各次日程。
//
// 一个对象里可能有多个 VEVENT：一个带重复规则的主体，加上若干个"这一次改了"的例外。
// 解析不了的对象跳过而不是报错：别人发来的一条格式古怪的邀请，不该让整个日历查不出来。
func (c *Client) occurrences(id, calName string, data *ical.Calendar, from, to time.Time) []Event {
	var master *ical.Event
	overrides := make(map[int64]*ical.Event)
	for _, e := range data.Events() {
		if rid := e.Props.Get(ical.PropRecurrenceID); rid != nil {
			if at, err := c.propTime(rid); err == nil {
				overrides[at.Unix()] = &e
			}
			continue
		}
		if master == nil {
			master = &e
		}
	}
	if master == nil {
		return nil
	}
	base, ok := c.decode(id, calName, master)
	if !ok {
		return nil
	}
	overlaps := func(e Event) bool { return e.Start.Before(to) && e.End.After(from) }

	set, err := master.RecurrenceSet(c.loc())
	if err != nil || set == nil {
		// 重复规则解析不了时按单次日程处理，至少第一次还在。
		if overlaps(base) {
			return []Event{base}
		}
		return nil
	}
	length := base.End.Sub(base.Start)
	var out []Event
	for _, at := range set.Between(from.Add(-length), to, true) {
		occ := base
		occ.Recurring = true
		if o, ok := overrides[at.Unix()]; ok {
			changed, ok := c.decode(id, calName, o)
			if !ok {
				continue
			}
			occ = changed
			occ.Recurring = true
		} else {
			occ.Start, occ.End = at.In(c.loc()), at.In(c.loc()).Add(length)
		}
		if overlaps(occ) {
			out = append(out, occ)
		}
		if len(out) > MaxEvents {
			break
		}
	}
	return out
}

// propTime 解析一个时间属性。
//
// 别的日历软件写出来的 TZID 不一定是标准名（Outlook 会写 "China Standard Time"），
// 认不出来时按本连接器的时区理解，而不是把整条日程丢掉。
func (c *Client) propTime(prop *ical.Prop) (time.Time, error) {
	at, err := prop.DateTime(c.loc())
	if err == nil {
		return at, nil
	}
	bare := *prop
	bare.Params = make(ical.Params, len(prop.Params))
	for k, v := range prop.Params {
		if k != ical.PropTimezoneID {
			bare.Params[k] = v
		}
	}
	return bare.DateTime(c.loc())
}

// decode 把一个 VEVENT 转成 Event。
func (c *Client) decode(id, calName string, e *ical.Event) (Event, bool) {
	startProp := e.Props.Get(ical.PropDateTimeStart)
	if startProp == nil {
		return Event{}, false
	}
	start, err := c.propTime(startProp)
	if err != nil {
		return Event{}, false
	}
	allDay := len(startProp.Value) == len("20060102")
	var end time.Time
	switch {
	case e.Props.Get(ical.PropDateTimeEnd) != nil:
		if end, err = c.propTime(e.Props.Get(ical.PropDateTimeEnd)); err != nil {
			return Event{}, false
		}
	case e.Props.Get(ical.PropDuration) != nil:
		d, err := e.Props.Get(ical.PropDuration).Duration()
		if err != nil {
			return Event{}, false
		}
		end = start.Add(d)
	case allDay:
		end = start.AddDate(0, 0, 1)
	default:
		end = start
	}
	text := func(name string) string {
		v, err := e.Props.Text(name)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}
	return Event{
		ID: id, Calendar: calName,
		Title: text(ical.PropSummary), Location: text(ical.PropLocation), Notes: text(ical.PropDescription),
		Start: start.In(c.loc()), End: end.In(c.loc()), AllDay: allDay, Organizer: organizer(e),
		Recurring: e.Props.Get(ical.PropRecurrenceRule) != nil,
	}, true
}

// organizer 取出日程的发起人：有名字就带上名字。
func organizer(e *ical.Event) string {
	prop := e.Props.Get(ical.PropOrganizer)
	if prop == nil {
		return ""
	}
	addr := strings.TrimSpace(prop.Value)
	if len(addr) >= 7 && strings.EqualFold(addr[:7], "mailto:") {
		addr = addr[7:]
	}
	if name := strings.TrimSpace(prop.Params.Get("CN")); name != "" && name != addr {
		return name + " <" + addr + ">"
	}
	return addr
}

// Draft 是新建日程的内容。
type Draft struct {
	Calendar string
	Title    string
	Start    time.Time
	// End 对有钟点的日程是结束时刻；对全天日程是最后一天（含）。零值表示一小时或当天。
	End      time.Time
	AllDay   bool
	Location string
	Notes    string
}

func (d Draft) validate() error {
	if strings.TrimSpace(d.Title) == "" {
		return fmt.Errorf("%w: 日程要有标题", ErrInvalid)
	}
	if d.Start.IsZero() {
		return fmt.Errorf("%w: 日程要有开始时间", ErrInvalid)
	}
	if !d.End.IsZero() && d.End.Before(d.Start) {
		return fmt.Errorf("%w: 结束时间不能早于开始时间", ErrInvalid)
	}
	return nil
}

// setTimes 写入起止时间。end 不含在内；全天日程的 end 不晚于开始那天时按一天算。
//
// 有具体钟点的一律写成 UTC：带时区名的写法要求同时附上那个时区的完整定义（VTIMEZONE），
// 而 UTC 时间在任何日历软件里都会按用户所在时区正确显示。
func (c *Client) setTimes(e *ical.Event, start, end time.Time, allDay bool) {
	e.Props.Del(ical.PropDuration)
	if allDay {
		day := func(t time.Time) time.Time {
			y, m, d := t.In(c.loc()).Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		}
		first, last := day(start), day(end)
		// 全天日程的结束日不含在内。只给了开始、或起止同一天，都是"这一天"。
		if end.IsZero() || !last.After(first) {
			last = first.AddDate(0, 0, 1)
		}
		e.Props.SetDate(ical.PropDateTimeStart, first)
		e.Props.SetDate(ical.PropDateTimeEnd, last)
		return
	}
	if end.IsZero() {
		end = start.Add(time.Hour)
	}
	e.Props.SetDateTime(ical.PropDateTimeStart, start.UTC())
	e.Props.SetDateTime(ical.PropDateTimeEnd, end.UTC())
}

func setText(e *ical.Event, name, value string) {
	if value = strings.TrimSpace(value); value == "" {
		e.Props.Del(name)
		return
	}
	e.Props.SetText(name, value)
}

// Create 新建一条日程。
//
// key 是这次操作的幂等键：同一个键重复调用写的是同一个对象，
// 所以进程在写入途中崩溃后重放，不会在日历里留下两条一样的日程。
func (c *Client) Create(ctx context.Context, d Draft, key string) (Event, error) {
	if err := d.validate(); err != nil {
		return Event{}, err
	}
	cal, err := c.pick(ctx, d.Calendar)
	if err != nil {
		return Event{}, err
	}
	uid := core.NewID()
	if key != "" {
		sum := sha256.Sum256([]byte(key))
		uid = hex.EncodeToString(sum[:16])
	}
	now := time.Now().UTC()
	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, uid+"@atomassistant")
	event.Props.SetDateTime(ical.PropDateTimeStamp, now)
	event.Props.SetDateTime(ical.PropCreated, now)
	event.Props.SetText(ical.PropSummary, strings.TrimSpace(d.Title))
	setText(event, ical.PropLocation, d.Location)
	setText(event, ical.PropDescription, d.Notes)
	end := d.End
	if d.AllDay && !end.IsZero() {
		// 调用方给的是"到哪一天为止"，存的是不含在内的次日。
		end = end.AddDate(0, 0, 1)
	}
	c.setTimes(event, d.Start, end, d.AllDay)

	data := ical.NewCalendar()
	data.Props.SetText(ical.PropVersion, "2.0")
	data.Props.SetText(ical.PropProductID, "-//AtomAssistant//CalDAV//ZH")
	data.Children = append(data.Children, event.Component)

	id := strings.TrimSuffix(cal.Path, "/") + "/" + uid + ".ics"
	client, err := c.dial(ctx)
	if err != nil {
		return Event{}, err
	}
	if _, err := client.PutCalendarObject(ctx, id, data); err != nil {
		return Event{}, fmt.Errorf("calendar: 写入日程失败: %w", err)
	}
	out, _ := c.decode(id, cal.Name, event)
	return out, nil
}

// Patch 是对一条日程的修改。nil 表示这一项不动。
type Patch struct {
	Title    *string
	Start    *time.Time
	End      *time.Time
	AllDay   *bool
	Location *string
	Notes    *string

	// Occurrence 非空表示只改重复日程里的这一次，值是这一次的开始时间。
	// 调课、调休、某一周的例会挪个时间，改的都是"这一次"，系列里的其他各次不动。
	Occurrence *time.Time
	// Cancel 与 Occurrence 一起用：这一次取消，其他各次照常。
	Cancel bool
}

// apply 把修改落到一个 VEVENT 上。current 是它修改前的样子。
func (c *Client) apply(event *ical.Event, current Event, p Patch) error {
	if p.Title != nil {
		if strings.TrimSpace(*p.Title) == "" {
			return fmt.Errorf("%w: 标题不能改成空的", ErrInvalid)
		}
		event.Props.SetText(ical.PropSummary, strings.TrimSpace(*p.Title))
	}
	if p.Location != nil {
		setText(event, ical.PropLocation, *p.Location)
	}
	if p.Notes != nil {
		setText(event, ical.PropDescription, *p.Notes)
	}
	if p.Start != nil || p.End != nil || p.AllDay != nil {
		// 整个系列的时间和它的重复规则、例外日期绑在一起，改一个要连带改一串，
		// 做错了会让整个系列错位。要挪时间，一次一次地挪。
		if current.Recurring {
			return fmt.Errorf("%w: 一次改掉整个重复系列的时间。要改某一次，请指明是哪一次（occurrence）", ErrUnsupported)
		}
		start, end, allDay := current.Start, current.End, current.AllDay
		if p.AllDay != nil {
			allDay = *p.AllDay
		}
		if p.Start != nil {
			// 只挪开始时间时保持原来的时长：把会从 10 点挪到 14 点，没人希望它变成 4 小时的负时长。
			if p.End == nil {
				end = p.Start.Add(current.End.Sub(current.Start))
			}
			start = *p.Start
		}
		if p.End != nil {
			end = *p.End
		}
		if !allDay && end.Before(start) {
			return fmt.Errorf("%w: 结束时间不能早于开始时间", ErrInvalid)
		}
		switch {
		case allDay && p.End != nil:
			// 调用方给的是"到哪一天为止"，存的是不含在内的次日。
			end = p.End.AddDate(0, 0, 1)
		case allDay && !current.AllDay && p.End == nil:
			// 有钟点的日程改成全天且没说到哪天：就是开始那一天。
			end = time.Time{}
		}
		c.setTimes(event, start, end, allDay)
	}
	touch(event)
	return nil
}

// touch 更新修改时间并把序号加一：订阅了这条日程的其他客户端靠序号知道内容变了。
func touch(event *ical.Event) {
	now := time.Now().UTC()
	event.Props.SetDateTime(ical.PropDateTimeStamp, now)
	event.Props.SetDateTime(ical.PropLastModified, now)
	seq := 0
	if prop := event.Props.Get(ical.PropSequence); prop != nil {
		if n, err := prop.Int(); err == nil {
			seq = n
		}
	}
	next := ical.NewProp(ical.PropSequence)
	next.Value = fmt.Sprint(seq + 1)
	event.Props.Set(next)
}

// Update 修改一条日程。p.Occurrence 非空时只改重复日程里的那一次。
func (c *Client) Update(ctx context.Context, id string, p Patch) (Event, error) {
	cal, err := c.owner(ctx, id)
	if err != nil {
		return Event{}, err
	}
	client, err := c.dial(ctx)
	if err != nil {
		return Event{}, err
	}
	obj, err := client.GetCalendarObject(ctx, id)
	if err != nil {
		return Event{}, fmt.Errorf("calendar: 读取日程失败: %w", err)
	}
	var master *ical.Event
	for _, e := range obj.Data.Events() {
		if e.Props.Get(ical.PropRecurrenceID) == nil {
			master = &e
			break
		}
	}
	if master == nil {
		return Event{}, fmt.Errorf("%w: %s 里没有日程", ErrNotFound, id)
	}
	current, ok := c.decode(id, cal.Name, master)
	if !ok {
		return Event{}, fmt.Errorf("calendar: 这条日程的时间无法解析，不能修改")
	}

	target := master
	if p.Occurrence != nil {
		if target, err = c.occurrence(obj.Data, master, current, *p.Occurrence, p.Cancel); err != nil {
			return Event{}, err
		}
	} else if p.Cancel {
		return Event{}, fmt.Errorf("%w: 取消某一次要指明是哪一次（occurrence）；要删掉整条日程请用删除", ErrInvalid)
	}
	if target != nil {
		before, ok := c.decode(id, cal.Name, target)
		if !ok {
			return Event{}, fmt.Errorf("calendar: 这一次日程的时间无法解析，不能修改")
		}
		if err := c.apply(target, before, p); err != nil {
			return Event{}, err
		}
	}
	if _, err := client.PutCalendarObject(ctx, id, obj.Data); err != nil {
		return Event{}, fmt.Errorf("calendar: 写入日程失败: %w", err)
	}
	if target == nil {
		// 这一次被取消了。返回它原来的样子，供调用方说明取消的是哪一次。
		current.Start, current.End = *p.Occurrence, p.Occurrence.Add(current.End.Sub(current.Start))
		current.Recurring = true
		return current, nil
	}
	out, _ := c.decode(id, cal.Name, target)
	out.Recurring = current.Recurring
	return out, nil
}

// sameForm 生成一个与系列开始时间写法一致的时间属性。
//
// 例外日期和"这一次"的标识必须和系列的开始时间用同一种写法（同一个时区、同为日期或同为时刻），
// 日历软件才认得它们指的是系列里的哪一次。
func (c *Client) sameForm(name string, master *ical.Event, at time.Time) *ical.Prop {
	start := master.Props.Get(ical.PropDateTimeStart)
	prop := ical.NewProp(name)
	switch {
	case len(start.Value) == len("20060102"):
		prop.SetDate(at.In(c.loc()))
	case start.Params.Get(ical.PropTimezoneID) != "":
		if loc, err := time.LoadLocation(start.Params.Get(ical.PropTimezoneID)); err == nil {
			prop.SetDateTime(at.In(loc))
		} else {
			prop.SetDateTime(at.UTC())
		}
	default:
		prop.SetDateTime(at.UTC())
	}
	return prop
}

// occurrence 找到（或新建）重复日程里某一次对应的 VEVENT；cancel 为真时把这一次取消并返回 nil。
//
// at 可以是这一次按规则本来的时间，也可以是它被改期之后现在的时间——
// 调用方看到的是现在的时间，不该要求它知道原来是几点。
func (c *Client) occurrence(data *ical.Calendar, master *ical.Event, series Event, at time.Time, cancel bool) (*ical.Event, error) {
	set, err := master.RecurrenceSet(c.loc())
	if err != nil || set == nil {
		return nil, fmt.Errorf("%w: 这不是一条重复日程，不需要指明是哪一次", ErrInvalid)
	}
	// 先看这一次是不是已经被单独改过。
	original := at
	var existing *ical.Component
	for _, child := range data.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		rid := child.Props.Get(ical.PropRecurrenceID)
		if rid == nil {
			continue
		}
		ridAt, err := c.propTime(rid)
		if err != nil {
			continue
		}
		now, ok := c.decode("", "", &ical.Event{Component: child})
		if ridAt.Equal(at) || (ok && now.Start.Equal(at)) {
			existing, original = child, ridAt
			break
		}
	}
	if existing == nil && len(set.Between(at, at, true)) == 0 {
		return nil, fmt.Errorf("%w: 这个系列里没有 %s 开始的那一次（occurrence 要写那一次的开始时间）",
			ErrNotFound, at.In(c.loc()).Format("2006-01-02 15:04"))
	}

	if cancel {
		// 取消 = 在系列上记一个例外日期，并去掉为这一次单独存的内容。
		master.Props.Add(c.sameForm(ical.PropExceptionDates, master, original))
		touch(master)
		if existing != nil {
			kept := data.Children[:0]
			for _, child := range data.Children {
				if child != existing {
					kept = append(kept, child)
				}
			}
			data.Children = kept
		}
		return nil, nil
	}
	if existing != nil {
		return &ical.Event{Component: existing}, nil
	}

	// 为这一次单独存一份：内容照抄系列，去掉重复规则，标明它顶替的是哪一次。
	override := ical.NewEvent()
	for name, props := range master.Props {
		switch name {
		case ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates:
			continue
		}
		override.Props[name] = append([]ical.Prop(nil), props...)
	}
	override.Children = master.Children
	override.Props.Set(c.sameForm(ical.PropRecurrenceID, master, original))
	c.setTimes(override, original, original.Add(series.End.Sub(series.Start)), series.AllDay)
	data.Children = append(data.Children, override.Component)
	return override, nil
}

// Delete 删除一条日程。重复日程会整个系列一起删掉。
func (c *Client) Delete(ctx context.Context, id string) error {
	if _, err := c.owner(ctx, id); err != nil {
		return err
	}
	client, err := c.dial(ctx)
	if err != nil {
		return err
	}
	if err := client.RemoveAll(ctx, id); err != nil {
		return fmt.Errorf("calendar: 删除日程失败: %w", err)
	}
	return nil
}

// Get 读取一条日程。
func (c *Client) Get(ctx context.Context, id string) (Event, error) {
	cal, err := c.owner(ctx, id)
	if err != nil {
		return Event{}, err
	}
	client, err := c.dial(ctx)
	if err != nil {
		return Event{}, err
	}
	obj, err := client.GetCalendarObject(ctx, id)
	if err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	for _, e := range obj.Data.Events() {
		if e.Props.Get(ical.PropRecurrenceID) != nil {
			continue
		}
		if out, ok := c.decode(id, cal.Name, &e); ok {
			return out, nil
		}
	}
	return Event{}, fmt.Errorf("%w: %s 里没有日程", ErrNotFound, id)
}
