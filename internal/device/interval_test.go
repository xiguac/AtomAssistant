package device

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetSyncMinutes_只能选给定的几档(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	d, _ := pairedDevice(t, s)
	if d.SyncMinutes != DefaultSyncMinutes {
		t.Errorf("新设备的默认间隔 = %d，想要 %d", d.SyncMinutes, DefaultSyncMinutes)
	}
	if err := s.SetSyncMinutes(ctx, d.ID, 60); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSyncMinutes(ctx, d.ID, 7); !errors.Is(err, ErrInvalid) {
		t.Errorf("不在档位里的值应当被拒: %v", err)
	}
	if err := s.SetSyncMinutes(ctx, "dev_nope", 20); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的设备: %v", err)
	}
}

func TestHandler_请求里带上当前的上报间隔(t *testing.T) {
	s, _, _ := setup(t)
	h := Handler(s, nil)
	d, tok := pairedDevice(t, s)
	if err := s.SetSyncMinutes(context.Background(), d.ID, 60); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"me", "requests"} {
		req := httptest.NewRequest("GET", Prefix+path, bytes.NewBuffer(nil))
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var got struct {
			Minutes int `json:"sync_minutes"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Minutes != 60 {
			t.Errorf("%s 返回 %d %s，想要 sync_minutes=60", path, w.Code, w.Body)
		}
	}
}

func TestGapFor_间隔越长放得越宽(t *testing.T) {
	if gapFor(5) != minGap {
		t.Errorf("5 分钟一条时取下限，得到 %v", gapFor(5))
	}
	if gapFor(20) != 50*time.Minute {
		t.Errorf("20 分钟一条时是 50 分钟，得到 %v", gapFor(20))
	}
	if gapFor(60) != 150*time.Minute {
		t.Errorf("60 分钟一条时是 150 分钟，得到 %v", gapFor(60))
	}
}

func TestLearnBattery_上报间隔拉长后仍能学到(t *testing.T) {
	// 每 20 分钟一条、每小时掉 3 个点，共 6 小时。
	var s []BatterySample
	for i := 0; i < 19; i++ {
		s = append(s, BatterySample{At: t0.Add(time.Duration(i) * 20 * time.Minute), Level: 0.90 - 0.01*float64(i), Unplugged: true})
	}
	if m := LearnBattery(s, minGap); m.DrainOK {
		t.Error("用 15 分钟的门槛，20 分钟一条的记录应当全被当成断档")
	}
	m := LearnBattery(s, gapFor(20))
	if !m.DrainOK || !near(m.DrainPercentPerHour, 3, 0.2) {
		t.Errorf("放宽后掉电速度 = %+v，想要约 3 %%/小时", m)
	}
}
