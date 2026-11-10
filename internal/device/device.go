// Package device 是手机上的 App 这个数据源：它采集接口拿不到的数据（健康、位置、运动、系统状态……），
// 通过配对过的设备令牌上报到这里，助手按需查询。
//
// 几条不让步的规则：
//
//  1. **配对码一次性、十分钟有效。** 配对成功后换成设备令牌；令牌只存摘要，丢了就吊销重配。
//  2. **最私人的数据按次授权。** 位置、详细健康记录只能挂在一条授权需求上上报：
//     助手提出需求，用户在 App 里看到原因后点同意，App 才去采集。设备自己不能主动上报这类数据。
//  3. **上报的内容落库即加密**，与其他内容列一样。
//  4. **数据按个人域对待**，不参与任何共享。
//  5. **含别人写的文字的数据按外部内容读出**（歌名、应用名、Wi-Fi 名），只是材料，不是指令。
package device

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/event"
)

const (
	// PairTTL 是配对码的有效期。
	PairTTL = 10 * time.Minute
	// DefaultRequestTTL 与 MaxRequestTTL 是授权需求等用户处理的时间。
	DefaultRequestTTL = 24 * time.Hour
	MaxRequestTTL     = 7 * 24 * time.Hour
	// MaxBatch 是一次上报最多的条数；MaxPayload 是单条内容的字节上限。
	MaxBatch   = 200
	MaxPayload = 256 << 10
	// MaxReason 与 MaxParams 限制授权需求里文字的长度：它们会显示在用户手机上。
	MaxReason = 300
	MaxParams = 2 << 10
	// MaxPending 是一台设备上同时等着用户处理的需求数上限。
	MaxPending = 20

	// TopicDataReceived 在设备上报一批数据后发布，内容只有种类和条数，不含数据本身。
	TopicDataReceived = "device.data_received"
	// TopicRequestDone 在用户授权并上传完一条需求的数据后发布，等这条需求的任务靠它醒来。
	TopicRequestDone = "device.request_done"
	// TopicRequestDeclined 在用户拒绝一条需求后发布。
	TopicRequestDeclined = "device.request_declined"
)

var (
	// ErrInvalid 表示参数不合法。
	ErrInvalid = errors.New("device: 参数不合法")
	// ErrNotFound 表示没有这个设备、需求，或者配对码、令牌不对、已失效。不区分是哪一种。
	ErrNotFound = errors.New("device: 不存在或已失效")
	// ErrForbidden 表示这台设备不能上报这种数据。
	ErrForbidden = errors.New("device: 不允许上报这种数据")
)

// Device 是一台配对过（或等待配对）的手机。
type Device struct {
	ID        string
	Name      string
	Model     string
	OS        string
	CreatedAt time.Time
	// PairedAt 为零表示还没配对。
	PairedAt   time.Time
	LastSeenAt time.Time
	RevokedAt  time.Time
	// SyncMinutes 是 App 上报数据的间隔（分钟），在后台设置，App 每次同步时读取。
	SyncMinutes int
	// Admin 为真表示这台手机的 App 可以管理后台（任务、审批等），在后台「手机」页打开。默认关。
	Admin bool
	// PairExpiresAt 非零表示配对码还在等待使用。
	PairExpiresAt time.Time
}

// Paired 报告这台设备是否已配对且没被吊销。
func (d *Device) Paired() bool { return !d.PairedAt.IsZero() && d.RevokedAt.IsZero() }

// Item 是设备上报的一条数据。
type Item struct {
	// Key 是设备给这条数据起的稳定标识，重传同一条不会重复入库。
	Key        string          `json:"key"`
	CapturedAt time.Time       `json:"captured_at"`
	Payload    json.RawMessage `json:"payload"`
}

// Stored 是库里的一条数据。
type Stored struct {
	DeviceID   string          `json:"device_id"`
	Kind       string          `json:"kind"`
	CapturedAt time.Time       `json:"captured_at"`
	ReceivedAt time.Time       `json:"received_at"`
	RequestID  string          `json:"request_id,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// KindStat 是某一种数据的存量概况。
type KindStat struct {
	Kind   string    `json:"kind"`
	Role   string    `json:"role,omitempty"`
	Count  int       `json:"count"`
	Latest time.Time `json:"latest"`
}

// Request 是助手向用户提出的一次按次授权需求。
type Request struct {
	ID       string          `json:"id"`
	DeviceID string          `json:"device_id"`
	Kind     string          `json:"kind"`
	Reason   string          `json:"reason"`
	Params   json.RawMessage `json:"params,omitempty"`
	TaskID   string          `json:"task_id,omitempty"`
	// Status 是 pending、done 或 declined。
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	AnsweredAt time.Time `json:"answered_at,omitzero"`
}

// Query 是查询已上报数据的条件。
type Query struct {
	// DeviceID 为空是全部设备。
	DeviceID string
	Kind     string
	Since time.Time
	Until time.Time
	Limit int
}

// Store 存放设备与数据，由 *pg.Store 提供。令牌与配对码都以摘要传入。
type Store interface {
	CreateDevice(ctx context.Context, d *Device, pairHash string) error
	// PairDevice 用配对码摘要认领一台设备：码有效就清掉它、记下令牌摘要与机型，返回设备。
	PairDevice(ctx context.Context, pairHash, tokenHash, model, os string, now time.Time) (*Device, error)
	// DeviceByToken 按令牌摘要找一台配对过且没被吊销的设备。
	DeviceByToken(ctx context.Context, tokenHash string) (*Device, error)
	TouchDevice(ctx context.Context, id string, now time.Time) error
	// SetSyncMinutes 设置一台设备的上报间隔，返回设备是否存在且没被吊销。
	SetSyncMinutes(ctx context.Context, id string, minutes int) (bool, error)
	// SetAdminAllowed 打开或关闭一台设备的管理权限，返回设备是否存在且没被吊销。
	SetAdminAllowed(ctx context.Context, id string, allowed bool) (bool, error)
	ListDevices(ctx context.Context) ([]*Device, error)
	// RevokeDevice 吊销设备并删掉等它处理的需求，返回它此前是否有效。
	RevokeDevice(ctx context.Context, id string, now time.Time) (bool, error)
	// DeleteDevice 删除设备及它的全部数据。
	DeleteDevice(ctx context.Context, id string) error

	// AddData 存入数据，同一设备同一种类下重复的 Key 跳过，返回实际新增的条数。
	AddData(ctx context.Context, deviceID, kind, requestID string, items []Item, now time.Time) (int, error)
	QueryData(ctx context.Context, q Query) ([]Stored, error)
	DataStats(ctx context.Context, deviceID string) ([]KindStat, error)
	// DeleteData 删掉某台设备某种数据（kind 为空是全部），返回删掉几条。
	DeleteData(ctx context.Context, deviceID, kind string) (int, error)

	CreateRequest(ctx context.Context, r *Request) error
	// PendingRequests 列出一台设备上此刻还有效、等用户处理的需求。deviceID 为空是全部设备。
	PendingRequests(ctx context.Context, deviceID string, now time.Time) ([]*Request, error)
	RequestByID(ctx context.Context, id string) (*Request, error)
	// AnswerRequest 把一条等待中且没过期的需求标成 done 或 declined，返回此前是否确实在等待。
	AnswerRequest(ctx context.Context, id, status string, now time.Time) (bool, error)
}

// Publisher 是发布事件的能力，由 *event.Bus 提供。
type Publisher interface {
	Publish(ctx context.Context, e *event.Event) (int, error)
}

// Service 是设备数据源的业务入口。
type Service struct {
	Store Store
	// Bus 为空时不发事件。
	Bus Publisher
	// Now 为空用当前时间，测试用。
	Now func() time.Time

	// Memory 非空时，设备上报的固定信息（型号、屏幕、系统版本）会写进记忆。
	Memory MemoryWriter
	// IPRegion 非空时，服务器会按请求来源 IP 记下设备此刻的归属地（城市级）。
	IPRegion RegionLookup
	// Asker 非空时，手机可以把提问交给助手（快捷指令）。
	Asker Asker

	bat batteryCache

	remMu      sync.Mutex
	remembered map[string]string

	ipMu   sync.Mutex
	ipLast map[string]ipNote
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("device: 生成随机数失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// 配对码字母表去掉了容易看混的 0 O 1 I L。
const pairAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func newPairCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("device: 生成配对码失败: %w", err)
	}
	for i := range b {
		b[i] = pairAlphabet[int(b[i])%len(pairAlphabet)]
	}
	return string(b), nil
}

// normalizePairCode 去掉用户输入里的空格、连字符并转大写。
func normalizePairCode(code string) string {
	code = strings.ToUpper(code)
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, code)
}

// FormatPairCode 把配对码写成 XXXX-XXXX 的样子给人看。
func FormatPairCode(code string) string {
	if len(code) == 8 {
		return code[:4] + "-" + code[4:]
	}
	return code
}

// DefaultSyncMinutes 是新设备默认的上报间隔。
const DefaultSyncMinutes = 20

// SyncChoices 是后台里能选的上报间隔（分钟）。数据不用太新，间隔长一点省电，也少打扰。
var SyncChoices = []int{5, 10, 20, 30, 60, 120, 240}

// SetSyncMinutes 设置一台设备的上报间隔。
func (s *Service) SetSyncMinutes(ctx context.Context, id string, minutes int) error {
	valid := false
	for _, c := range SyncChoices {
		valid = valid || c == minutes
	}
	if !valid {
		return fmt.Errorf("%w: 上报间隔只能是 %v 分钟之一", ErrInvalid, SyncChoices)
	}
	ok, err := s.Store.SetSyncMinutes(ctx, id, minutes)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// SetAdminAllowed 打开或关闭一台设备的管理权限。
func (s *Service) SetAdminAllowed(ctx context.Context, id string, allowed bool) error {
	ok, err := s.Store.SetAdminAllowed(ctx, id, allowed)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// AddDevice 登记一台待配对的设备，返回它和一次性配对码（只此一次可见）。
func (s *Service) AddDevice(ctx context.Context, name string) (*Device, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		return nil, "", fmt.Errorf("%w: 设备名要在 1 到 40 个字之间", ErrInvalid)
	}
	id, err := randHex(8)
	if err != nil {
		return nil, "", err
	}
	code, err := newPairCode()
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	d := &Device{ID: "dev_" + id, Name: name, CreatedAt: now, PairExpiresAt: now.Add(PairTTL), SyncMinutes: DefaultSyncMinutes}
	if err := s.Store.CreateDevice(ctx, d, hash(code)); err != nil {
		return nil, "", err
	}
	return d, code, nil
}

// Pair 用配对码换设备令牌。码不对、过期、用过，一律 ErrNotFound。
func (s *Service) Pair(ctx context.Context, code, model, os string) (*Device, string, error) {
	code = normalizePairCode(code)
	if len(code) != 8 {
		return nil, "", ErrNotFound
	}
	tok, err := randHex(32)
	if err != nil {
		return nil, "", err
	}
	model, os = clip(model, 60), clip(os, 40)
	d, err := s.Store.PairDevice(ctx, hash(code), hash(tok), model, os, s.now())
	if err != nil {
		return nil, "", err
	}
	return d, tok, nil
}

func clip(v string, n int) string {
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) > n {
		return string([]rune(v)[:n])
	}
	return v
}

// Authenticate 按设备令牌认出设备，并记下它刚刚来过。
func (s *Service) Authenticate(ctx context.Context, token string) (*Device, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	d, err := s.Store.DeviceByToken(ctx, hash(token))
	if err != nil {
		return nil, err
	}
	if err := s.Store.TouchDevice(ctx, d.ID, s.now()); err != nil {
		return nil, err
	}
	return d, nil
}

// Ingest 存下设备主动上报的一批数据。按次授权类的数据不能这样上报。
func (s *Service) Ingest(ctx context.Context, d *Device, kind string, items []Item) (int, error) {
	k, ok := KindByID(kind)
	if !ok {
		return 0, fmt.Errorf("%w: 不认识的数据种类 %q", ErrInvalid, kind)
	}
	if k.PerUse {
		return 0, fmt.Errorf("%w: %s 必须挂在授权需求上上报", ErrForbidden, kind)
	}
	if k.Server {
		return 0, fmt.Errorf("%w: %s 由服务器自己产生，设备不能上报", ErrForbidden, kind)
	}
	n, err := s.add(ctx, d, kind, "", items)
	if err != nil {
		return 0, err
	}
	if kind == "device.system" {
		s.rememberSystem(ctx, d, items)
	}
	s.publish(ctx, TopicDataReceived, map[string]any{"device": d.Name, "kind": kind, "count": n}, "device:"+d.Name)
	return n, nil
}

func (s *Service) add(ctx context.Context, d *Device, kind, requestID string, items []Item) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	if len(items) > MaxBatch {
		return 0, fmt.Errorf("%w: 一次最多上报 %d 条", ErrInvalid, MaxBatch)
	}
	for i := range items {
		it := &items[i]
		if it.Key == "" || len(it.Key) > 200 {
			return 0, fmt.Errorf("%w: 第 %d 条的 key 为空或太长", ErrInvalid, i+1)
		}
		if it.CapturedAt.IsZero() {
			return 0, fmt.Errorf("%w: 第 %d 条缺少 captured_at", ErrInvalid, i+1)
		}
		if len(it.Payload) == 0 || !json.Valid(it.Payload) || !utf8.Valid(it.Payload) {
			return 0, fmt.Errorf("%w: 第 %d 条的 payload 不是合法的 UTF-8 JSON", ErrInvalid, i+1)
		}
		if len(it.Payload) > MaxPayload {
			return 0, fmt.Errorf("%w: 第 %d 条的 payload 超过 %d 字节", ErrInvalid, i+1, MaxPayload)
		}
	}
	return s.Store.AddData(ctx, d.ID, kind, requestID, items, s.now())
}

// RequestInput 是助手提出一条授权需求时给的东西。
type RequestInput struct {
	Kind   string
	Reason string
	Params json.RawMessage
	TaskID string
	TTL    time.Duration
	// DeviceID 为空时选唯一一台可用的设备；有多台时要指定。
	DeviceID string
}

// CreateRequest 提出一条按次授权需求，等用户在 App 里同意。
func (s *Service) CreateRequest(ctx context.Context, in RequestInput) (*Request, error) {
	k, ok := KindByID(in.Kind)
	if !ok || !k.PerUse {
		return nil, fmt.Errorf("%w: %q 不是按次授权的数据种类", ErrInvalid, in.Kind)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > MaxReason {
		return nil, fmt.Errorf("%w: 原因要写清楚，且不超过 %d 个字", ErrInvalid, MaxReason)
	}
	if len(in.Params) > MaxParams || (len(in.Params) > 0 && !json.Valid(in.Params)) {
		return nil, fmt.Errorf("%w: 参数不是合法的 JSON 或太长", ErrInvalid)
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = DefaultRequestTTL
	}
	if ttl > MaxRequestTTL {
		ttl = MaxRequestTTL
	}
	now := s.now()
	dev, err := s.pickDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, err
	}
	pending, err := s.Store.PendingRequests(ctx, dev.ID, now)
	if err != nil {
		return nil, err
	}
	if len(pending) >= MaxPending {
		return nil, fmt.Errorf("%w: 这台设备上已经有 %d 条需求在等用户处理", ErrInvalid, len(pending))
	}
	id, err := randHex(8)
	if err != nil {
		return nil, err
	}
	r := &Request{ID: "req_" + id, DeviceID: dev.ID, Kind: in.Kind, Reason: reason, Params: in.Params,
		TaskID: in.TaskID, Status: "pending", CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := s.Store.CreateRequest(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Service) pickDevice(ctx context.Context, id string) (*Device, error) {
	all, err := s.Store.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	var live []*Device
	for _, d := range all {
		if d.Paired() && (id == "" || d.ID == id) {
			live = append(live, d)
		}
	}
	switch {
	case len(live) == 0:
		return nil, fmt.Errorf("%w: 没有已配对的设备", ErrNotFound)
	case len(live) > 1:
		return nil, fmt.Errorf("%w: 有多台设备，要指定 device_id", ErrInvalid)
	}
	return live[0], nil
}

// Fulfil 用户同意并采集完之后，设备把数据交上来，需求随之办结。
func (s *Service) Fulfil(ctx context.Context, d *Device, requestID string, items []Item) (int, error) {
	r, err := s.ownRequest(ctx, d, requestID)
	if err != nil {
		return 0, err
	}
	n, err := s.add(ctx, d, r.Kind, r.ID, items)
	if err != nil {
		return 0, err
	}
	won, err := s.Store.AnswerRequest(ctx, r.ID, "done", s.now())
	if err != nil {
		return 0, err
	}
	if !won {
		return 0, ErrNotFound
	}
	s.publish(ctx, TopicRequestDone, map[string]any{"request_id": r.ID, "kind": r.Kind, "count": n, "task_id": r.TaskID}, "device:"+d.Name)
	return n, nil
}

// Decline 用户拒绝一条需求。
func (s *Service) Decline(ctx context.Context, d *Device, requestID string) error {
	r, err := s.ownRequest(ctx, d, requestID)
	if err != nil {
		return err
	}
	won, err := s.Store.AnswerRequest(ctx, r.ID, "declined", s.now())
	if err != nil {
		return err
	}
	if !won {
		return ErrNotFound
	}
	s.publish(ctx, TopicRequestDeclined, map[string]any{"request_id": r.ID, "kind": r.Kind, "task_id": r.TaskID}, "device:"+d.Name)
	return nil
}

func (s *Service) ownRequest(ctx context.Context, d *Device, id string) (*Request, error) {
	r, err := s.Store.RequestByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.DeviceID != d.ID || r.Status != "pending" || !r.ExpiresAt.After(s.now()) {
		return nil, ErrNotFound
	}
	return r, nil
}

// Pending 列出一台设备上等用户处理的需求。
func (s *Service) Pending(ctx context.Context, d *Device) ([]*Request, error) {
	return s.Store.PendingRequests(ctx, d.ID, s.now())
}

// Latest 查已上报的数据，新的在前。
func (s *Service) Latest(ctx context.Context, q Query) ([]Stored, error) {
	if q.Kind != "" {
		if _, ok := KindByID(q.Kind); !ok {
			return nil, fmt.Errorf("%w: 不认识的数据种类 %q", ErrInvalid, q.Kind)
		}
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	return s.Store.QueryData(ctx, q)
}

// Trust 是读出某种数据时该用的信任等级：含别人写的文字的按外部内容，其余按工具输出。
func Trust(kind string) core.TrustLevel {
	if k, ok := KindByID(kind); ok && k.Text {
		return core.TrustExternal
	}
	return core.TrustToolOutput
}

// publish 发一个只含概况的事件。发不出去不影响已经存下的数据，也不该让设备以为上报失败了重传。
func (s *Service) publish(ctx context.Context, topic string, payload map[string]any, source string) {
	if s.Bus == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	// 事件总线出错只会让等它的任务晚一点被兜底超时叫醒，数据本身已经落库。
	_, _ = s.Bus.Publish(ctx, event.New(topic, body, core.TrustExternal, source))
}

// Summary 是一台设备上已有数据的概况，给助手判断"有没有、新不新"用。
type Summary struct {
	DeviceID string     `json:"device_id"`
	Name     string     `json:"name"`
	LastSeen time.Time  `json:"last_seen,omitzero"`
	Kinds    []KindStat `json:"kinds"`
	Pending  int        `json:"pending_requests"`
}

// Summary 列出每台已配对设备上的数据概况。
func (s *Service) Summary(ctx context.Context) ([]Summary, error) {
	devs, err := s.Store.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, d := range devs {
		if !d.Paired() {
			continue
		}
		stats, err := s.Store.DataStats(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		pending, err := s.Store.PendingRequests(ctx, d.ID, s.now())
		if err != nil {
			return nil, err
		}
		for i := range stats {
			if k, ok := KindByID(stats[i].Kind); ok {
				stats[i].Role = k.Role
			}
		}
		out = append(out, Summary{DeviceID: d.ID, Name: d.Name, LastSeen: d.LastSeenAt, Kinds: stats, Pending: len(pending)})
	}
	return out, nil
}

// NormalizeBaseURL 校验并整理 App 要连的服务器地址：必须是 https，不带路径。
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: 服务器地址不对，应写成 https://域名", ErrInvalid)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("%w: 服务器地址必须是 https：手机上的数据不能明文传输", ErrInvalid)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("%w: 服务器地址只写到域名（可带端口），不要带路径", ErrInvalid)
	}
	return "https://" + u.Host, nil
}
