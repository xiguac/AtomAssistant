package device

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/event"
)

// fakeStore 是 Store 的内存实现，只给本包的测试用。
type fakeStore struct {
	devs  map[string]*Device
	pair  map[string]string // pairHash -> id
	token map[string]string // tokenHash -> id
	data  []Stored
	keys  map[string]bool
	reqs  map[string]*Request
}

func newFake() *fakeStore {
	return &fakeStore{devs: map[string]*Device{}, pair: map[string]string{}, token: map[string]string{},
		keys: map[string]bool{}, reqs: map[string]*Request{}}
}

func (f *fakeStore) CreateDevice(_ context.Context, d *Device, ph string) error {
	c := *d
	f.devs[d.ID] = &c
	f.pair[ph] = d.ID
	return nil
}

func (f *fakeStore) PairDevice(_ context.Context, ph, th, model, os string, now time.Time) (*Device, error) {
	id, ok := f.pair[ph]
	if !ok || !f.devs[id].PairExpiresAt.After(now) {
		return nil, ErrNotFound
	}
	delete(f.pair, ph)
	d := f.devs[id]
	d.PairedAt, d.Model, d.OS, d.PairExpiresAt = now, model, os, time.Time{}
	f.token[th] = id
	c := *d
	return &c, nil
}

func (f *fakeStore) DeviceByToken(_ context.Context, th string) (*Device, error) {
	id, ok := f.token[th]
	if !ok || !f.devs[id].RevokedAt.IsZero() {
		return nil, ErrNotFound
	}
	c := *f.devs[id]
	return &c, nil
}

func (f *fakeStore) TouchDevice(_ context.Context, id string, now time.Time) error {
	f.devs[id].LastSeenAt = now
	return nil
}

func (f *fakeStore) SetSyncMinutes(_ context.Context, id string, minutes int) (bool, error) {
	d := f.devs[id]
	if d == nil || !d.RevokedAt.IsZero() {
		return false, nil
	}
	d.SyncMinutes = minutes
	return true, nil
}

func (f *fakeStore) SetAdminAllowed(_ context.Context, id string, allowed bool) (bool, error) {
	d := f.devs[id]
	if d == nil || !d.RevokedAt.IsZero() {
		return false, nil
	}
	d.Admin = allowed
	return true, nil
}

func (f *fakeStore) ListDevices(context.Context) ([]*Device, error) {
	var out []*Device
	for _, d := range f.devs {
		c := *d
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeStore) RevokeDevice(_ context.Context, id string, now time.Time) (bool, error) {
	d := f.devs[id]
	if d == nil || !d.RevokedAt.IsZero() {
		return false, nil
	}
	d.RevokedAt = now
	return true, nil
}

func (f *fakeStore) DeleteDevice(_ context.Context, id string) error { delete(f.devs, id); return nil }

func (f *fakeStore) AddData(_ context.Context, dev, kind, req string, items []Item, now time.Time) (int, error) {
	n := 0
	for _, it := range items {
		k := dev + "|" + kind + "|" + it.Key
		if f.keys[k] {
			continue
		}
		f.keys[k] = true
		f.data = append(f.data, Stored{DeviceID: dev, Kind: kind, RequestID: req, CapturedAt: it.CapturedAt, ReceivedAt: now, Payload: it.Payload})
		n++
	}
	return n, nil
}

func (f *fakeStore) QueryData(_ context.Context, q Query) ([]Stored, error) {
	var out []Stored
	for _, d := range f.data {
		if q.Kind == "" || d.Kind == q.Kind {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeStore) DataStats(context.Context, string) ([]KindStat, error) { return nil, nil }
func (f *fakeStore) DeleteData(context.Context, string, string) (int, error) {
	return 0, nil
}

func (f *fakeStore) CreateRequest(_ context.Context, r *Request) error {
	c := *r
	f.reqs[r.ID] = &c
	return nil
}

func (f *fakeStore) PendingRequests(_ context.Context, dev string, now time.Time) ([]*Request, error) {
	var out []*Request
	for _, r := range f.reqs {
		if r.Status == "pending" && r.ExpiresAt.After(now) && (dev == "" || r.DeviceID == dev) {
			c := *r
			out = append(out, &c)
		}
	}
	return out, nil
}

func (f *fakeStore) RequestByID(_ context.Context, id string) (*Request, error) {
	r, ok := f.reqs[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *r
	return &c, nil
}

func (f *fakeStore) AnswerRequest(_ context.Context, id, status string, now time.Time) (bool, error) {
	r := f.reqs[id]
	if r == nil || r.Status != "pending" || !r.ExpiresAt.After(now) {
		return false, nil
	}
	r.Status, r.AnsweredAt = status, now
	return true, nil
}

type fakeBus struct{ topics []string }

func (b *fakeBus) Publish(_ context.Context, e *event.Event) (int, error) {
	b.topics = append(b.topics, e.Topic)
	return 0, nil
}

func setup(t *testing.T) (*Service, *fakeBus, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	bus := &fakeBus{}
	return &Service{Store: newFake(), Bus: bus, Now: func() time.Time { return now }}, bus, &now
}

func pairedDevice(t *testing.T, s *Service) (*Device, string) {
	t.Helper()
	ctx := context.Background()
	_, code, err := s.AddDevice(ctx, "我的 iPhone")
	if err != nil {
		t.Fatal(err)
	}
	d, tok, err := s.Pair(ctx, FormatPairCode(strings.ToLower(code)), "iPhone15,2", "iOS 18")
	if err != nil {
		t.Fatalf("配对失败: %v", err)
	}
	return d, tok
}

func TestPair_码只能用一次且会过期(t *testing.T) {
	s, _, now := setup(t)
	ctx := context.Background()
	_, code, _ := s.AddDevice(ctx, "手机")
	if _, _, err := s.Pair(ctx, code, "m", "o"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Pair(ctx, code, "m", "o"); !errors.Is(err, ErrNotFound) {
		t.Errorf("配对码被用第二次应当失败，得到 %v", err)
	}
	_, code2, _ := s.AddDevice(ctx, "另一台")
	*now = now.Add(PairTTL + time.Second)
	if _, _, err := s.Pair(ctx, code2, "m", "o"); !errors.Is(err, ErrNotFound) {
		t.Errorf("过期的配对码应当失败，得到 %v", err)
	}
}

func TestAuthenticate_吊销后令牌失效(t *testing.T) {
	s, _, now := setup(t)
	ctx := context.Background()
	d, tok := pairedDevice(t, s)
	if _, err := s.Authenticate(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "bad"); !errors.Is(err, ErrNotFound) {
		t.Errorf("错误令牌应当失败: %v", err)
	}
	if ok, _ := s.Store.RevokeDevice(ctx, d.ID, *now); !ok {
		t.Fatal("吊销应当成功")
	}
	if _, err := s.Authenticate(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("吊销后令牌应当失效: %v", err)
	}
}

func item(key string) Item {
	return Item{Key: key, CapturedAt: time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC), Payload: json.RawMessage(`{"level":0.8}`)}
}

func TestIngest_按次授权类不能主动上报且重复的key跳过(t *testing.T) {
	s, bus, _ := setup(t)
	ctx := context.Background()
	d, _ := pairedDevice(t, s)

	if _, err := s.Ingest(ctx, d, "location.now", []Item{item("a")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("位置不能主动上报: %v", err)
	}
	if _, err := s.Ingest(ctx, d, "nope", []Item{item("a")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("未知种类应当被拒: %v", err)
	}
	if n, err := s.Ingest(ctx, d, "device.status", []Item{item("a"), item("b")}); err != nil || n != 2 {
		t.Fatalf("首次上报 n=%d err=%v", n, err)
	}
	if n, _ := s.Ingest(ctx, d, "device.status", []Item{item("a"), item("c")}); n != 1 {
		t.Errorf("重复的 key 应当跳过，新增 %d", n)
	}
	if len(bus.topics) != 2 || bus.topics[0] != TopicDataReceived {
		t.Errorf("事件 = %v", bus.topics)
	}
	bad := Item{Key: "x", CapturedAt: time.Now(), Payload: json.RawMessage(`{`)}
	if _, err := s.Ingest(ctx, d, "device.status", []Item{bad}); !errors.Is(err, ErrInvalid) {
		t.Errorf("非法 JSON 应当被拒: %v", err)
	}
}

func TestRequest_同意后交付并发事件_过期或拒绝后不能再交(t *testing.T) {
	s, bus, now := setup(t)
	ctx := context.Background()
	d, _ := pairedDevice(t, s)

	if _, err := s.CreateRequest(ctx, RequestInput{Kind: "device.status", Reason: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("非按次授权类不能提需求: %v", err)
	}
	if _, err := s.CreateRequest(ctx, RequestInput{Kind: "location.now", Reason: " "}); !errors.Is(err, ErrInvalid) {
		t.Errorf("没写原因应当被拒: %v", err)
	}
	r, err := s.CreateRequest(ctx, RequestInput{Kind: "location.now", Reason: "查你现在离机场多远", TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Pending(ctx, d); len(list) != 1 {
		t.Fatalf("待处理 = %d", len(list))
	}
	if n, err := s.Fulfil(ctx, d, r.ID, []Item{item("loc1")}); err != nil || n != 1 {
		t.Fatalf("交付 n=%d err=%v", n, err)
	}
	if bus.topics[len(bus.topics)-1] != TopicRequestDone {
		t.Errorf("应当发出办结事件，得到 %v", bus.topics)
	}
	if _, err := s.Fulfil(ctx, d, r.ID, []Item{item("loc2")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("办结的需求不能再交付: %v", err)
	}

	r2, _ := s.CreateRequest(ctx, RequestInput{Kind: "motion.query", Reason: "看上周走了多少", TTL: time.Hour})
	*now = now.Add(2 * time.Hour)
	if _, err := s.Fulfil(ctx, d, r2.ID, []Item{item("h1")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("过期的需求不能交付: %v", err)
	}
	r3, _ := s.CreateRequest(ctx, RequestInput{Kind: "location.track", Reason: "路上轨迹"})
	if err := s.Decline(ctx, d, r3.ID); err != nil {
		t.Fatal(err)
	}
	if bus.topics[len(bus.topics)-1] != TopicRequestDeclined {
		t.Errorf("应当发出拒绝事件，得到 %v", bus.topics)
	}
}

func TestCreateRequest_没有设备或多台设备时要说清(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	if _, err := s.CreateRequest(ctx, RequestInput{Kind: "location.now", Reason: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("没有设备: %v", err)
	}
	pairedDevice(t, s)
	pairedDevice(t, s)
	if _, err := s.CreateRequest(ctx, RequestInput{Kind: "location.now", Reason: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("多台设备要指定: %v", err)
	}
}

func TestHandler_配对后上报(t *testing.T) {
	s, _, _ := setup(t)
	h := Handler(s, nil)
	_, code, _ := s.AddDevice(context.Background(), "手机")

	do := func(method, path, tok, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := do("POST", Prefix+"pair", "", `{"code":"WRONG123"}`); w.Code != http.StatusNotFound {
		t.Errorf("错误配对码 = %d", w.Code)
	}
	w := do("POST", Prefix+"pair", "", `{"code":"`+code+`","model":"iPhone","os":"iOS 18"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("配对 = %d %s", w.Code, w.Body)
	}
	var got struct{ Token string }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Token == "" {
		t.Fatalf("没有拿到令牌: %s", w.Body)
	}
	if w := do("GET", Prefix+"me", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("没带令牌 = %d", w.Code)
	}
	body := `{"kind":"device.status","items":[{"key":"k1","captured_at":"2026-10-31T11:00:00Z","payload":{"battery":0.5}}]}`
	if w := do("POST", Prefix+"data", got.Token, body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"added":1`) {
		t.Errorf("上报 = %d %s", w.Code, w.Body)
	}
	if w := do("POST", Prefix+"data", got.Token, strings.Replace(body, "device.status", "location.now", 1)); w.Code != http.StatusForbidden {
		t.Errorf("主动上报位置应当 403，得到 %d", w.Code)
	}
}

func TestHandler_配对失败太多会被限速(t *testing.T) {
	s, _, _ := setup(t)
	h := Handler(s, nil)
	var last int
	for range pairFailLimit + 2 {
		req := httptest.NewRequest("POST", Prefix+"pair", bytes.NewBufferString(`{"code":"AAAAAAAA"}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		last = w.Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("连续失败后应当 429，得到 %d", last)
	}
}
