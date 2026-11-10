package device

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
)

// MemoryWriter 是写记忆的能力，由 *memory.Pipeline 提供。
type MemoryWriter interface {
	Ingest(ctx context.Context, cands []memory.Candidate) ([]memory.Outcome, error)
}

// 手机上不会自己变、或只在升级时才变的信息，直接写进记忆：助手之后办事时不用再去查。
// 来源标成「权威数据同步」：值变了（系统升级、换了手机）会直接更新，不会来问用户。
//
// 只写这几样：型号、屏幕分辨率与缩放、系统版本、构建号。电量、存储这类会变的不写——它们该查，不该记。
const (
	domainHardware = "设备/%s/硬件"
	domainSystem   = "设备/%s/系统"
)

// rememberSystem 从最新一条「系统信息」里取出固定的设备信息，写进记忆。
// 和上次写过的一样就不再写；写失败只记日志，不影响数据本身已经存下。
func (s *Service) rememberSystem(ctx context.Context, d *Device, items []Item) {
	if s.Memory == nil || len(items) == 0 {
		return
	}
	// 取采集时间最新的一条。
	sort.SliceStable(items, func(i, j int) bool { return items[i].CapturedAt.After(items[j].CapturedAt) })
	cands := systemCandidates(d, items[0].Payload)
	if len(cands) == 0 {
		return
	}
	var fresh []memory.Candidate
	s.remMu.Lock()
	for _, c := range cands {
		if s.remembered[c.Key()] != c.Value {
			fresh = append(fresh, c)
		}
	}
	s.remMu.Unlock()
	if len(fresh) == 0 {
		return
	}
	if _, err := s.Memory.Ingest(ctx, fresh); err != nil {
		slog.Warn("把手机的固定信息写进记忆失败", "设备", d.Name, "err", err)
		return
	}
	s.remMu.Lock()
	if s.remembered == nil {
		s.remembered = map[string]string{}
	}
	for _, c := range fresh {
		s.remembered[c.Key()] = c.Value
	}
	s.remMu.Unlock()
}

// systemCandidates 把一条「系统信息」变成要记的候选。字段缺了就跳过那一条，不拿猜的值顶。
func systemCandidates(d *Device, payload json.RawMessage) []memory.Candidate {
	var p struct {
		Model   string `json:"modelIdentifier"`
		OS      string `json:"systemName"`
		Version string `json:"systemVersion"`
		Gestalt struct {
			Build  string  `json:"BuildVersion"`
			Width  float64 `json:"main-screen-width"`
			Height float64 `json:"main-screen-height"`
			Scale  float64 `json:"main-screen-scale"`
		} `json:"gestalt"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	src := memory.Source{Kind: memory.SourceConnector, Ref: "device:" + d.ID, Trust: core.TrustConnector}
	mk := func(domain, value string) memory.Candidate {
		return memory.Candidate{
			Kind: memory.KindFact, Subject: "user", Domain: fmt.Sprintf(domain, d.Name), Value: value,
			Temporality: memory.TemporalDurable, Source: src, Tags: []string{"设备"},
		}
	}
	var out []memory.Candidate
	if p.Model != "" {
		parts := []string{fmt.Sprintf("型号 %s", p.Model)}
		if p.Gestalt.Width > 0 && p.Gestalt.Height > 0 {
			screen := fmt.Sprintf("屏幕分辨率 %d×%d", int(p.Gestalt.Width), int(p.Gestalt.Height))
			if p.Gestalt.Scale > 0 {
				screen += fmt.Sprintf("，缩放 %gx", p.Gestalt.Scale)
			}
			parts = append(parts, screen)
		}
		out = append(out, mk(domainHardware, fmt.Sprintf("用户的手机「%s」：%s。", d.Name, strings.Join(parts, "，"))))
	}
	if p.OS != "" && p.Version != "" {
		v := fmt.Sprintf("用户的手机「%s」的系统是 %s %s", d.Name, p.OS, p.Version)
		if p.Gestalt.Build != "" {
			v += "，构建号 " + p.Gestalt.Build
		}
		out = append(out, mk(domainSystem, v+"。"))
	}
	return out
}
