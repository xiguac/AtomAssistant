package matter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/session"
)

func TestStage_一次性的事按时间淡出长期的事不淡出(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	for _, c := range []struct {
		kind Kind
		idle time.Duration
		want Stage
	}{
		{KindOnce, 6 * day, StageActive},
		{KindOnce, 8 * day, StageLow},
		{KindOnce, 15 * day, StageArchived},
		{KindOnce, 31 * day, StageExpired},
		{KindOngoing, 15 * day, StageActive},
		{KindOngoing, 29 * day, StageActive},
		{KindOngoing, 31 * day, StageExpired},
	} {
		m := &Matter{Kind: c.kind, LastActiveAt: now.Add(-c.idle)}
		if got := m.Stage(now); got != c.want {
			t.Fatalf("%s 放了 %v 应是阶段 %d，实际 %d", c.kind, c.idle, c.want, got)
		}
	}
	// 提起来就又活了，等回答的问题也不用等了。
	asked := now
	m := &Matter{Kind: KindOnce, LastActiveAt: now.Add(-20 * day), AskedAt: &asked}
	m.Touch(now)
	if m.Stage(now) != StageActive || m.AskedAt != nil {
		t.Fatalf("说到之后应回到活跃: %+v", m)
	}
	if ParseKind(" Ongoing ") != KindOngoing || ParseKind("别的") != KindOnce {
		t.Fatal("认不得的种类按一次性算")
	}
	if FromTags([]string{"chat:qq/1", Tag("abc")}) != "abc" || FromTags([]string{"chat:qq/1"}) != "" {
		t.Fatal("应从任务标签里取出事项编号")
	}
}

func TestNotes_进展有上限并写给任务和每日摘要(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.Local)
	course := &Matter{ID: "c", Title: "操作系统课", Kind: KindOngoing, CreatedAt: at, LastActiveAt: at}
	hw := &Matter{ID: "h", ParentID: "c", Title: "收第三次作业", Kind: KindOnce, CreatedAt: at.Add(time.Minute), LastActiveAt: at}
	for i := 0; i < maxNotes+5; i++ {
		hw.AddNote(Note{At: at.Add(-48 * time.Hour), Ask: "旧的", Result: "旧结果"})
	}
	hw.AddNote(Note{At: at, Ask: "帮我收一下第三次作业\n周五前", Result: strings.Repeat("长", resultRunes+50)})
	if len(hw.Notes) != maxNotes || !strings.HasSuffix(hw.Notes[maxNotes-1].Result, "…") {
		t.Fatalf("进展应限条数、限长度: %d 条", len(hw.Notes))
	}
	for _, m := range []*Matter{course, hw} {
		if err := store.Put(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	r := Reader{Store: store}

	text, ok, err := r.TaskContext(ctx, []string{Tag("h")})
	if err != nil || !ok || !strings.Contains(text, "收第三次作业（一次性的事），属于「操作系统课」") || !strings.Contains(text, "周五前") {
		t.Fatalf("任务应看到这件事是什么、挂在哪、此前的进展: %v %v\n%s", ok, err, text)
	}
	// 没挂在任何事上的任务、已经不在清单里的事：没有这一段。
	if _, ok, _ := r.TaskContext(ctx, []string{"chat:qq/1"}); ok {
		t.Fatal("不属于任何事的任务不应有这一段")
	}
	if _, ok, err := r.TaskContext(ctx, []string{Tag("gone")}); ok || err != nil {
		t.Fatalf("事项已删时不应报错: %v %v", ok, err)
	}

	// 每天的摘要只有那天有进展的事。
	day := session.Day(at)
	digest, err := r.DayDigest(ctx, day)
	if err != nil || !strings.Contains(digest, "【收第三次作业】一次性的事，属于「操作系统课」") || strings.Contains(digest, "【操作系统课】") ||
		strings.Contains(digest, "旧结果") || !strings.Contains(digest, "帮我收一下第三次作业 周五前") {
		t.Fatalf("那天的摘要应是那天各件事的进展: %v\n%s", err, digest)
	}
	if empty, err := r.DayDigest(ctx, "2020-01-01"); err != nil || empty != "" {
		t.Fatalf("没有进展的日子摘要为空: %q %v", empty, err)
	}
}
