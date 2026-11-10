package device

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/memory"
)

type fakeMemory struct{ got [][]memory.Candidate }

func (f *fakeMemory) Ingest(_ context.Context, c []memory.Candidate) ([]memory.Outcome, error) {
	f.got = append(f.got, c)
	return nil, nil
}

func systemItem(key, version string) Item {
	p, _ := json.Marshal(map[string]any{
		"modelIdentifier": "iPhone18,3", "systemName": "iOS", "systemVersion": version,
		"gestalt": map[string]any{"BuildVersion": "24B119", "main-screen-width": 1206, "main-screen-height": 2622, "main-screen-scale": 3},
	})
	return Item{Key: key, CapturedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), Payload: p}
}

func TestIngest_系统信息里的固定信息写进记忆且不重复写(t *testing.T) {
	s, _, _ := setup(t)
	mem := &fakeMemory{}
	s.Memory = mem
	ctx := context.Background()
	d, _ := pairedDevice(t, s)

	if _, err := s.Ingest(ctx, d, "device.system", []Item{systemItem("a", "27.2")}); err != nil {
		t.Fatal(err)
	}
	if len(mem.got) != 1 || len(mem.got[0]) != 2 {
		t.Fatalf("第一次应当写 2 条（硬件、系统），得到 %+v", mem.got)
	}
	var hw, sys memory.Candidate
	for _, c := range mem.got[0] {
		if strings.HasSuffix(c.Domain, "硬件") {
			hw = c
		} else {
			sys = c
		}
	}
	for _, want := range []string{"iPhone18,3", "1206×2622", "缩放 3x"} {
		if !strings.Contains(hw.Value, want) {
			t.Errorf("硬件记忆里缺少 %q: %s", want, hw.Value)
		}
	}
	for _, want := range []string{"iOS 27.2", "24B119"} {
		if !strings.Contains(sys.Value, want) {
			t.Errorf("系统记忆里缺少 %q: %s", want, sys.Value)
		}
	}
	if hw.Source.Kind != memory.SourceConnector || hw.Temporality != memory.TemporalDurable {
		t.Errorf("来源应为权威数据同步、时效为长期可变: %+v", hw)
	}
	for _, c := range mem.got[0] {
		if err := c.Validate(); err != nil {
			t.Errorf("候选不合法: %v", err)
		}
	}

	// 6 小时后又上报了一模一样的内容：不该再写。
	if _, err := s.Ingest(ctx, d, "device.system", []Item{systemItem("b", "27.2")}); err != nil {
		t.Fatal(err)
	}
	if len(mem.got) != 1 {
		t.Errorf("内容没变不该再写，写了 %d 次", len(mem.got))
	}
	// 系统升级：只写变了的那一条。
	if _, err := s.Ingest(ctx, d, "device.system", []Item{systemItem("c", "27.3")}); err != nil {
		t.Fatal(err)
	}
	if len(mem.got) != 2 || len(mem.got[1]) != 1 || !strings.Contains(mem.got[1][0].Value, "27.3") {
		t.Errorf("升级后应只写系统那一条: %+v", mem.got)
	}
}

func TestIngest_系统信息缺字段就不写那一条(t *testing.T) {
	s, _, _ := setup(t)
	mem := &fakeMemory{}
	s.Memory = mem
	d, _ := pairedDevice(t, s)
	p, _ := json.Marshal(map[string]any{"systemName": "iOS", "systemVersion": "27.2"})
	it := Item{Key: "x", CapturedAt: time.Now(), Payload: p}
	if _, err := s.Ingest(context.Background(), d, "device.system", []Item{it}); err != nil {
		t.Fatal(err)
	}
	if len(mem.got) != 1 || len(mem.got[0]) != 1 || !strings.HasSuffix(mem.got[0][0].Domain, "系统") {
		t.Errorf("没有型号时只应写系统那一条: %+v", mem.got)
	}
}
