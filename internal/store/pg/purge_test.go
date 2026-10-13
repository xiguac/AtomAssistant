package pg_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/store"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/style"
	"github.com/xiguac/atomhelper/internal/team"
)

// TestPurge 需要真实数据库，设置 ATOM_TEST_DSN 后运行。
func TestPurge_清掉点名的记录别的一条不动(t *testing.T) {
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过清理测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := pg.Open(ctx, dsn, seal.TestBox())
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer s.Close()
	if err := s.ResetForTest(ctx); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)

	// 两件事：要清的"北京行程"，和不相干的"订餐厅"。各有任务、步骤、审计、评分、说话样本和记忆。
	finish := func(title string, to core.TaskState) *core.Task {
		t.Helper()
		tk := core.NewTask(title, title, core.TrustUser)
		must(s.CreateTask(ctx, tk))
		must(s.Transition(ctx, tk.ID, core.StateNew, core.StateReady, "测试"))
		if to == core.StateReady {
			return tk
		}
		must(s.Transition(ctx, tk.ID, core.StateReady, core.StateRunning, "测试"))
		must(s.Transition(ctx, tk.ID, core.StateRunning, to, "测试"))
		step := core.NewStep(tk.ID, 1, core.StepTool, json.RawMessage(`{"q":"x"}`))
		step.ToolName, step.SideEffect = "web.search", core.SideEffectReadOnly
		step.Complete(json.RawMessage(`{"text":"..."}`), core.TrustExternal)
		must(s.AppendStep(ctx, step))
		must(s.Audit(ctx, store.AuditEntry{Actor: "agent", Action: "tool.call", TaskID: tk.ID, Detail: title}))
		must(s.Team().PutWork(ctx, &team.Work{ID: team.WorkID(tk.ID, 1, 0), TaskID: tk.ID, StepSeq: 1,
			Worker: "分析师", Category: team.CatAnalysis, Title: title, CreatedAt: now}))
		must(s.Channels().PutNotice(ctx, tk.ID, "done"))
		if _, err := s.PutTaskImage(ctx, tk.ID, "image/png", []byte("PNG")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Style().AddSample(ctx, &style.Sample{ID: "s-" + tk.ID, Scene: style.SceneChat,
			Text: "关于" + title + "说的话", Ref: "task:" + tk.ID, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	beijing, dinner, running := finish("北京行程", core.StateDone), finish("订餐厅", core.StateDone), finish("写周报", core.StateReady)

	pipe := memory.NewPipeline(s.Memories(), nil)
	remember := func(task *core.Task, domain, value string) string {
		t.Helper()
		out, err := pipe.Ingest(ctx, []memory.Candidate{{Kind: memory.KindFact, Subject: "user", Domain: domain, Value: value,
			Temporality: memory.TemporalDurable, Source: memory.Source{Kind: memory.SourceTask, Ref: task.ID, Trust: core.TrustToolOutput}}})
		if err != nil || len(out) != 1 || out[0].MemoryID == "" {
			t.Fatalf("写入记忆失败: %+v %v", out, err)
		}
		return out[0].MemoryID
	}
	trip, taste := remember(beijing, "旅游出行规划", "计划去北京四日游"), remember(dinner, "饮食", "不太吃辣")
	pend := memory.Pending{ID: core.NewID(), CreatedAt: now,
		Candidate: memory.Candidate{Kind: memory.KindPlaybook, Subject: "user", Domain: "旅游出行规划", Value: "先确认日期",
			Temporality: memory.TemporalDurable, Source: memory.Source{Kind: memory.SourceTask, Ref: beijing.ID, Trust: core.TrustExternal}},
		Arb: memory.Arbitration{Kind: memory.ConflictNone, Resolution: memory.ResolveHoldAsCandidate}}
	must(s.Memories().PutPending(ctx, pend))

	spec := pg.PurgeSpec{Tasks: []string{beijing.ID}, Memories: []string{trip}, Pending: []string{pend.ID}}

	// 先清点：数得出来，但什么都没删。
	res, err := s.Purge(ctx, spec, true)
	if err != nil || res.Tasks != 1 || res.Steps != 1 || res.Images != 1 || res.Audit < 1 || res.Works != 1 ||
		res.Notices != 1 || res.Samples != 1 || res.Memories != 1 || res.Revisions != 1 || res.Pending != 1 {
		t.Fatalf("清点结果不对: %+v %v", res, err)
	}
	if _, err := s.GetTask(ctx, beijing.ID); err != nil {
		t.Fatalf("清点不应删除任何东西: %v", err)
	}

	// 对象有问题时整次不做：没结束的任务、不存在的编号。
	for name, bad := range map[string]pg.PurgeSpec{
		"没结束的任务": {Tasks: []string{beijing.ID, running.ID}},
		"不存在的任务": {Tasks: []string{"no-such-task"}},
		"不存在的记忆": {Tasks: []string{beijing.ID}, Memories: []string{"no-such-memory"}},
	} {
		if _, err := s.Purge(ctx, bad, false); err == nil {
			t.Fatalf("%s应让清理整个不做", name)
		}
		if _, err := s.GetTask(ctx, beijing.ID); err != nil {
			t.Fatalf("%s时不应有任何东西被删: %v", name, err)
		}
	}

	// 真的清：点名的都没了。
	if _, err := s.Purge(ctx, spec, false); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if _, err := s.GetTask(ctx, beijing.ID); err == nil {
		t.Fatal("任务应被清掉")
	}
	if steps, _ := s.ListSteps(ctx, beijing.ID); len(steps) != 0 {
		t.Fatal("任务的步骤应被清掉")
	}
	if _, err := s.Memories().Get(ctx, trip); err == nil {
		t.Fatal("记忆应被清掉")
	}
	if list, _ := s.Memories().ListPending(ctx, ""); len(list) != 0 {
		t.Fatalf("候选应被清掉: %d", len(list))
	}
	again, err := s.Purge(ctx, pg.PurgeSpec{Tasks: []string{dinner.ID}}, true)
	if err != nil || again.Steps != 1 || again.Audit < 1 || again.Works != 1 || again.Samples != 1 || again.Images != 1 {
		t.Fatalf("不相干的那件事的记录应原样都在: %+v %v", again, err)
	}
	// 没点名的一条不动：另一件事的任务和记忆，还在排队的任务。
	if _, err := s.GetTask(ctx, dinner.ID); err != nil {
		t.Fatalf("不相干的任务不应被动: %v", err)
	}
	if _, err := s.GetTask(ctx, running.ID); err != nil {
		t.Fatalf("还在排队的任务不应被动: %v", err)
	}
	if m, err := s.Memories().Get(ctx, taste); err != nil || m.Current.Value != "不太吃辣" {
		t.Fatalf("不相干的记忆不应被动: %v", err)
	}
	if tally, err := s.Team().Tally(ctx); err != nil || len(tally) != 1 || tally[0].Runs != 1 {
		t.Fatalf("不相干的评分记录不应被动: %+v %v", tally, err)
	}
}
