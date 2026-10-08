package core

import (
	"sort"
	"testing"
	"time"
)

func TestCanTransition_终态不可再迁移(t *testing.T) {
	for _, s := range []TaskState{StateDone, StateFailed, StateCancelled} {
		if !s.Terminal() {
			t.Fatalf("%s 应为终态", s)
		}
		for _, to := range []TaskState{StateReady, StateRunning, StateWaiting} {
			if CanTransition(s, to) {
				t.Fatalf("终态 %s 不应能迁移到 %s", s, to)
			}
		}
	}
}

func TestCanTransition_崩溃恢复路径存在(t *testing.T) {
	// running → ready 是租约过期后重新入队的路径，缺了它就无法崩溃恢复。
	if !CanTransition(StateRunning, StateReady) {
		t.Fatal("running → ready 必须允许，否则崩溃后的任务无法回到队列")
	}
	if !CanTransition(StateWaiting, StateReady) {
		t.Fatal("waiting → ready 必须允许，否则等待中的任务无法被唤醒")
	}
}

func TestValidate_私人域不能设为可共享(t *testing.T) {
	tk := NewTask("看牙", "预约周三下午", TrustUser)
	tk.Visibility = VisibilityCircle
	tk.VisibilityTarget = "同事"
	if err := tk.Validate(); err == nil {
		t.Fatal("私人域任务设为圈子可见应被拒绝")
	}

	// 改成工作域后才允许。
	tk.Domain = DomainWork
	if err := tk.Validate(); err != nil {
		t.Fatalf("工作域应允许圈子可见: %v", err)
	}
}

func TestValidate_混合域按私人处理(t *testing.T) {
	tk := NewTask("给客户送礼", "用公司预算", TrustUser)
	tk.Domain = DomainMixed
	tk.Visibility = VisibilityCircle
	tk.VisibilityTarget = "同事"
	if err := tk.Validate(); err == nil {
		t.Fatal("mixed 域应按私人处理，不能直接设为圈子可见")
	}
}

func TestNewTask_默认落在最保守的一侧(t *testing.T) {
	tk := NewTask("随手记一笔", "", TrustExternal)
	if tk.Domain != DomainPersonal {
		t.Fatalf("默认域应为 personal，实际 %s", tk.Domain)
	}
	if tk.Visibility != VisibilityPrivate {
		t.Fatalf("默认可见性应为 private，实际 %s", tk.Visibility)
	}
}

func TestEffectiveTrust_自由文本一律降级(t *testing.T) {
	// 日历事件的开始时间是权威的。
	if got := EffectiveTrust(TrustConnector, false); got != TrustConnector {
		t.Fatalf("结构化字段应保持来源等级，实际 %s", got)
	}
	// 但它的标题是创建这条日程的人写的——陌生人的会议邀请可以往标题里塞指令。
	if got := EffectiveTrust(TrustConnector, true); got != TrustExternal {
		t.Fatalf("自由文本必须降级为 external，实际 %s", got)
	}
	if got := EffectiveTrust(TrustUser, true); got != TrustExternal {
		t.Fatalf("即使来源是用户，自由文本也应降级，实际 %s", got)
	}
}

func TestTrustLevel_权限判定(t *testing.T) {
	cases := []struct {
		level      TrustLevel
		instruct   bool
		irrevocabl bool
		writeMem   bool
	}{
		{TrustExternal, false, false, false},
		{TrustToolOutput, false, false, false},
		{TrustFriend, false, false, false},
		{TrustConnector, false, false, true},
		{TrustUser, true, true, true},
		{TrustSystem, true, true, true},
	}
	for _, c := range cases {
		if c.level.CanInstruct() != c.instruct {
			t.Errorf("%s CanInstruct 应为 %v", c.level, c.instruct)
		}
		if c.level.CanTriggerIrreversible() != c.irrevocabl {
			t.Errorf("%s CanTriggerIrreversible 应为 %v", c.level, c.irrevocabl)
		}
		if c.level.CanWriteMemoryDirectly() != c.writeMem {
			t.Errorf("%s CanWriteMemoryDirectly 应为 %v", c.level, c.writeMem)
		}
	}
}

func TestPrivacyLevel_绝密永不出实例(t *testing.T) {
	if !PrivacySecret.NeverLeavesInstance() {
		t.Fatal("L3 必须永不出实例")
	}
	for _, p := range []PrivacyLevel{PrivacyPublic, PrivacyNormal, PrivacySensitive} {
		if p.NeverLeavesInstance() {
			t.Fatalf("%s 不应被判定为永不出实例", p)
		}
	}
}

func TestSideEffect_只有不可逆操作禁止重放(t *testing.T) {
	for _, e := range []SideEffect{SideEffectNone, SideEffectReadOnly, SideEffectReversible} {
		if !e.ReplaySafe() {
			t.Fatalf("%s 应可安全重放", e)
		}
	}
	if SideEffectIrreversible.ReplaySafe() {
		t.Fatal("不可逆操作绝不能被判定为可重放")
	}
}

func TestBudget_三重上限各自生效(t *testing.T) {
	b := Budget{MaxTokens: 100, MaxCostCents: 50, MaxWallClock: time.Hour}
	cases := []struct {
		usage Usage
		want  string
	}{
		{Usage{Tokens: 99, CostCents: 49, WallClock: time.Minute}, ""},
		{Usage{Tokens: 100}, "tokens"},
		{Usage{CostCents: 50}, "cost"},
		{Usage{WallClock: time.Hour}, "wall_clock"},
	}
	for _, c := range cases {
		over, which := b.Exceeded(c.usage)
		if which != c.want || over != (c.want != "") {
			t.Errorf("用量 %+v: 期望 %q，实际 over=%v which=%q", c.usage, c.want, over, which)
		}
	}

	// 上限为 0 表示不限。
	if over, _ := (Budget{}).Exceeded(Usage{Tokens: 1 << 40, CostCents: 1 << 40}); over {
		t.Fatal("零值预算应表示不限")
	}
}

func TestIdempotencyKey_同任务同序号稳定且不泄露任务ID(t *testing.T) {
	k1 := IdempotencyKeyFor("task-abc", 3)
	k2 := IdempotencyKeyFor("task-abc", 3)
	if k1 != k2 {
		t.Fatal("同一 (任务, 序号) 必须得到相同幂等键，否则崩溃重放会重复下单")
	}
	if IdempotencyKeyFor("task-abc", 4) == k1 {
		t.Fatal("不同序号必须得到不同幂等键")
	}
	// 幂等键会随请求发给第三方，不能夹带内部标识。
	if len(k1) != 32 {
		t.Fatalf("幂等键长度异常: %d", len(k1))
	}
	if containsSubstring(k1, "task-abc") {
		t.Fatal("幂等键不得包含任务 ID")
	}
}

func TestNewID_按时间可排序(t *testing.T) {
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, NewID())
		time.Sleep(2 * time.Millisecond)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ID 字典序应等于生成时间序，实际 %v vs %v", ids, sorted)
		}
	}
	if len(ids[0]) != 26 {
		t.Fatalf("ID 长度应为 26，实际 %d", len(ids[0]))
	}
}

func TestNewID_不重复(t *testing.T) {
	seen := make(map[string]bool, 10000)
	for i := 0; i < 10000; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("生成了重复 ID: %s", id)
		}
		seen[id] = true
	}
}

func TestStepValidate_失败步骤必须记录原因(t *testing.T) {
	s := NewStep("t1", 1, StepLLM, nil)
	s.SideEffect = SideEffectNone
	s.Status = StepFailed
	if err := s.Validate(); err == nil {
		t.Fatal("失败步骤缺少原因应被拒绝：静默失败是被禁止的")
	}
	s.Fail(nil)
	if s.Err == "" {
		t.Fatal("Fail(nil) 也必须写入一个原因")
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("补上原因后应通过: %v", err)
	}
}

func TestNewStep_默认副作用等级最保守(t *testing.T) {
	s := NewStep("t1", 1, StepTool, nil)
	if s.SideEffect != SideEffectIrreversible {
		t.Fatalf("默认副作用应为最保守的 irreversible，实际 %s", s.SideEffect)
	}
	if s.OutputTrust != TrustExternal {
		t.Fatalf("默认输出信任等级应为 external，实际 %s", s.OutputTrust)
	}
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
