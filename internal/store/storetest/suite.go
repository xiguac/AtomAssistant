// Package storetest 是 store.TaskStore 的契约测试套件。
//
// 内存实现和 Postgres 实现跑同一套用例，保证两者行为一致——
// 否则"用内存实现测内核逻辑"这件事就失去意义了。
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/store"
)

// Factory 返回一个干净的存储实例。
type Factory func(t *testing.T) store.TaskStore

// RunSuite 对给定实现执行全部契约用例。
func RunSuite(t *testing.T, newStore Factory) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, store.TaskStore)
	}{
		{"创建与读取", testCreateGet},
		{"私人域不能写入可共享任务", testDomainConstraint},
		{"状态迁移是CAS", testTransitionCAS},
		{"迁移到终态释放租约", testTransitionReleasesLease},
		{"重复序号被拒绝", testDuplicateSeq},
		{"步骤按序号返回", testStepOrdering},
		{"待审批步骤的状态流转可持久化", testStepApprovalStatuses},
		{"领取任务按优先级", testClaimPriority},
		{"租约续期与抢占", testRenewLease},
		{"回收过期租约", testReclaimExpired},
		{"到期的等待任务可被查出", testDueWaiting},
		{"唤醒与写步骤同成同败", testWakeWithStep},
		{"运行时字段可更新", testUpdateRuntime},
		{"审计可写入", testAudit},
		{"不存在的记录返回ErrNotFound", testNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.fn(t, newStore(t))
		})
	}
}

func newTask(t *testing.T, s store.TaskStore, mutate ...func(*core.Task)) *core.Task {
	t.Helper()
	task := core.NewTask("测试任务", "验证存储契约", core.TrustUser)
	for _, m := range mutate {
		m(task)
	}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	return task
}

func ready(t *testing.T, s store.TaskStore, task *core.Task) {
	t.Helper()
	if err := s.Transition(context.Background(), task.ID, core.StateNew, core.StateReady, "入队"); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
}

func testCreateGet(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s, func(tk *core.Task) {
		tk.Tags = []string{"domain:出行", "scene:接待客户"}
		tk.Budget = core.Budget{MaxTokens: 100, MaxCostCents: 200, MaxWallClock: time.Hour}
	})

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Title != task.Title || got.State != core.StateNew {
		t.Fatalf("字段不一致: %+v", got)
	}
	if got.Origin != core.TrustUser {
		t.Fatalf("来源信任等级丢失: %s", got.Origin)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "domain:出行" {
		t.Fatalf("标签丢失: %v", got.Tags)
	}
	if got.Budget.MaxWallClock != time.Hour {
		t.Fatalf("时长字段丢失: %s", got.Budget.MaxWallClock)
	}
}

func testDomainConstraint(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := core.NewTask("看牙", "周三下午", core.TrustUser)
	task.Visibility = core.VisibilityCircle
	task.VisibilityTarget = "同事"
	// 私人域任务不可能被设置成可共享，应用层和数据库两道约束都要挡住。
	if err := s.CreateTask(ctx, task); err == nil {
		t.Fatal("私人域任务设为圈子可见应被拒绝")
	}
}

func testTransitionCAS(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)
	ready(t, s, task)

	if err := s.Transition(ctx, task.ID, core.StateReady, core.StateRunning, "第一个"); err != nil {
		t.Fatalf("首次迁移应成功: %v", err)
	}
	err := s.Transition(ctx, task.ID, core.StateReady, core.StateRunning, "第二个")
	if !errors.Is(err, store.ErrStateConflict) {
		t.Fatalf("期望 ErrStateConflict，实际 %v", err)
	}

	// 非法迁移在到达存储之前就应被拒绝。
	if err := s.Transition(ctx, task.ID, core.StateRunning, core.StateNew, "回退"); !errors.Is(err, core.ErrInvalidTransition) {
		t.Fatalf("期望 ErrInvalidTransition，实际 %v", err)
	}
}

func testTransitionReleasesLease(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)
	ready(t, s, task)
	claimed, err := s.ClaimReady(ctx, "w1", time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("领取失败: %v, %d", err, len(claimed))
	}

	if err := s.Transition(ctx, task.ID, core.StateRunning, core.StateDone, "完成"); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 租约不释放的话，重启后这个任务会被误判为仍在运行。
	if got.LeaseOwner != "" || got.LeaseExpiresAt != nil {
		t.Fatalf("终态应释放租约，实际 owner=%q expires=%v", got.LeaseOwner, got.LeaseExpiresAt)
	}
}

func testDuplicateSeq(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)

	st := core.NewStep(task.ID, 1, core.StepLLM, nil)
	st.SideEffect = core.SideEffectNone
	if err := s.AppendStep(ctx, st); err != nil {
		t.Fatalf("写入步骤失败: %v", err)
	}

	dup := core.NewStep(task.ID, 1, core.StepLLM, nil)
	dup.SideEffect = core.SideEffectNone
	if err := s.AppendStep(ctx, dup); !errors.Is(err, store.ErrDuplicateSeq) {
		t.Fatalf("期望 ErrDuplicateSeq，实际 %v", err)
	}
}

func testStepOrdering(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)

	// 故意乱序写入。
	for _, seq := range []int{3, 1, 2} {
		st := core.NewStep(task.ID, seq, core.StepLLM, nil)
		st.SideEffect = core.SideEffectNone
		if err := s.AppendStep(ctx, st); err != nil {
			t.Fatalf("写入 seq=%d 失败: %v", seq, err)
		}
	}
	list, err := s.ListSteps(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取步骤失败: %v", err)
	}
	for i, st := range list {
		if st.Seq != i+1 {
			t.Fatalf("步骤未按序号排序: %v", stepSeqs(list))
		}
	}

	// 更新终态后应能读回。
	last := list[2]
	last.SideEffect = core.SideEffectNone
	last.Fail(errors.New("演示失败"))
	if err := s.SaveStep(ctx, last); err != nil {
		t.Fatalf("更新步骤失败: %v", err)
	}
	list, _ = s.ListSteps(ctx, task.ID)
	if list[2].Status != core.StepFailed || list[2].Err == "" {
		t.Fatalf("失败状态未持久化: %+v", list[2])
	}
}

func testStepApprovalStatuses(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)

	st := core.NewStep(task.ID, 1, core.StepTool, json.RawMessage(`{"amount":100}`))
	st.ToolName = "ride.book"
	st.Status = core.StepAwaiting
	if err := s.AppendStep(ctx, st); err != nil {
		t.Fatalf("写入 awaiting 步骤失败: %v", err)
	}

	// 审批通过后依次经过 approved → pending，参数必须原样保留。
	for _, status := range []core.StepStatus{core.StepApproved, core.StepPending} {
		st.Status = status
		if err := s.SaveStep(ctx, st); err != nil {
			t.Fatalf("更新为 %s 失败: %v", status, err)
		}
		list, err := s.ListSteps(ctx, task.ID)
		if err != nil {
			t.Fatalf("读取步骤失败: %v", err)
		}
		if len(list) != 1 || list[0].Status != status {
			t.Fatalf("状态 %s 未持久化: %+v", status, list)
		}
		var in struct{ Amount int }
		if err := json.Unmarshal(list[0].Input, &in); err != nil || in.Amount != 100 {
			t.Fatalf("参数未原样保留: %s (%v)", list[0].Input, err)
		}
	}
}

func testClaimPriority(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	low := newTask(t, s, func(tk *core.Task) { tk.Priority = 1; tk.Title = "低优先级" })
	high := newTask(t, s, func(tk *core.Task) { tk.Priority = 9; tk.Title = "高优先级" })
	ready(t, s, low)
	ready(t, s, high)

	claimed, err := s.ClaimReady(ctx, "w1", time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("领取失败: %v, %d", err, len(claimed))
	}
	if claimed[0].ID != high.ID {
		t.Fatalf("应先领取高优先级任务，实际 %s", claimed[0].Title)
	}
	if claimed[0].State != core.StateRunning || claimed[0].LeaseOwner != "w1" {
		t.Fatalf("领取后应为 running 且带租约: %+v", claimed[0])
	}

	// 已被领取的任务不应被重复领取。
	again, err := s.ClaimReady(ctx, "w2", time.Minute, 5)
	if err != nil {
		t.Fatalf("二次领取失败: %v", err)
	}
	if len(again) != 1 || again[0].ID != low.ID {
		t.Fatalf("期望只剩低优先级任务可领，实际 %d 个", len(again))
	}
}

func testRenewLease(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)
	ready(t, s, task)
	if _, err := s.ClaimReady(ctx, "w1", time.Second, 1); err != nil {
		t.Fatalf("领取失败: %v", err)
	}

	if err := s.RenewLease(ctx, task.ID, "w1", time.Hour); err != nil {
		t.Fatalf("持有者续期应成功: %v", err)
	}
	// 续期后不应再被回收。
	n, err := s.ReclaimExpired(ctx, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("续期后不应被回收，实际回收 %d 个", n)
	}

	// 非持有者不能续期，否则两个 worker 会同时推进同一任务。
	if err := s.RenewLease(ctx, task.ID, "w2", time.Hour); !errors.Is(err, store.ErrStateConflict) {
		t.Fatalf("非持有者续期应失败，实际 %v", err)
	}
}

func testReclaimExpired(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)
	ready(t, s, task)
	if _, err := s.ClaimReady(ctx, "w1", time.Minute, 1); err != nil {
		t.Fatalf("领取失败: %v", err)
	}

	n, err := s.ReclaimExpired(ctx, time.Now().UTC().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("期望回收 1 个，实际 %d", n)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.State != core.StateReady || got.LeaseOwner != "" {
		t.Fatalf("期望回到 ready 且释放租约，实际 state=%s owner=%q", got.State, got.LeaseOwner)
	}
}

func testDueWaiting(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	past := newTask(t, s, func(tk *core.Task) { tk.Title = "已到期" })
	future := newTask(t, s, func(tk *core.Task) { tk.Title = "未到期" })

	setWaiting(t, s, past, time.Now().UTC().Add(-time.Minute))
	setWaiting(t, s, future, time.Now().UTC().Add(time.Hour))

	due, err := s.DueWaiting(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("查询到期任务失败: %v", err)
	}
	if len(due) != 1 || due[0].ID != past.ID {
		t.Fatalf("期望只有已到期任务，实际 %d 个", len(due))
	}
	if due[0].Wake == nil || due[0].Wake.Kind != core.WakeTime {
		t.Fatalf("唤醒条件未正确读回: %+v", due[0].Wake)
	}
}

func testWakeWithStep(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)

	first := core.NewStep(task.ID, 1, core.StepLLM, nil)
	first.SideEffect = core.SideEffectNone
	first.Complete(json.RawMessage(`{}`), core.TrustSystem)
	if err := s.AppendStep(ctx, first); err != nil {
		t.Fatalf("写入步骤失败: %v", err)
	}

	delivered := func() *core.Step {
		// Seq 故意给一个错的值：序号必须由存储分配。
		st := core.NewStep(task.ID, 99, core.StepWait, nil)
		st.SideEffect = core.SideEffectNone
		st.Complete(json.RawMessage(`{"topic":"a.b"}`), core.TrustExternal)
		return st
	}

	// 任务还没进入等待：不得唤醒，也不得写入步骤。
	if err := s.WakeWithStep(ctx, task.ID, "事件命中", delivered()); !errors.Is(err, store.ErrStateConflict) {
		t.Fatalf("期望 ErrStateConflict，实际 %v", err)
	}
	if list, _ := s.ListSteps(ctx, task.ID); len(list) != 1 {
		t.Fatalf("唤醒失败时不得留下步骤，实际 %d 条", len(list))
	}

	setWaiting(t, s, task, time.Now().UTC().Add(time.Hour))
	st := delivered()
	if err := s.WakeWithStep(ctx, task.ID, "事件命中", st); err != nil {
		t.Fatalf("唤醒失败: %v", err)
	}
	if st.Seq != 2 {
		t.Fatalf("序号应回填为 2，实际 %d", st.Seq)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if got.State != core.StateReady || got.Reason != "事件命中" || got.LeaseOwner != "" {
		t.Fatalf("期望 ready 且无租约，实际 state=%s reason=%q owner=%q", got.State, got.Reason, got.LeaseOwner)
	}
	list, _ := s.ListSteps(ctx, task.ID)
	if len(list) != 2 || list[1].Seq != 2 || list[1].Status != core.StepDone ||
		list[1].OutputTrust != core.TrustExternal {
		t.Fatalf("事件步骤未正确写入: %v", stepSeqs(list))
	}
	if list[1].IdempotencyKey != core.IdempotencyKeyFor(task.ID, 2) {
		t.Fatal("幂等键应按分配后的序号重新派生")
	}

	if err := s.WakeWithStep(ctx, "不存在", "x", delivered()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
}

func setWaiting(t *testing.T, s store.TaskStore, task *core.Task, at time.Time) {
	t.Helper()
	ctx := context.Background()
	ready(t, s, task)
	if _, err := s.ClaimReady(ctx, "w1", time.Minute, 1); err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	task.Wake = &core.WakeCondition{Kind: core.WakeTime, At: &at}
	if err := s.UpdateTaskRuntime(ctx, task); err != nil {
		t.Fatalf("写入唤醒条件失败: %v", err)
	}
	if err := s.Transition(ctx, task.ID, core.StateRunning, core.StateWaiting, "等待"); err != nil {
		t.Fatalf("进入等待失败: %v", err)
	}
}

func testUpdateRuntime(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)

	task.Usage = core.Usage{Tokens: 123, CostCents: 45, WallClock: 3 * time.Minute}
	task.NextSeq = 7
	task.Priority = 5
	deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	task.Deadline = &deadline
	if err := s.UpdateTaskRuntime(ctx, task); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Usage.Tokens != 123 || got.Usage.CostCents != 45 || got.Usage.WallClock != 3*time.Minute {
		t.Fatalf("用量未持久化: %+v", got.Usage)
	}
	if got.NextSeq != 7 || got.Priority != 5 {
		t.Fatalf("字段未持久化: next_seq=%d priority=%d", got.NextSeq, got.Priority)
	}
	if got.Deadline == nil || !got.Deadline.Equal(deadline) {
		t.Fatalf("截止时间未持久化: %v", got.Deadline)
	}
}

func testAudit(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	task := newTask(t, s)
	err := s.Audit(ctx, store.AuditEntry{
		Actor:     "system",
		Action:    "task.transition",
		TaskID:    task.ID,
		Detail:    "new → ready",
		TrustAtOp: core.TrustUser,
	})
	if err != nil {
		t.Fatalf("写入审计失败: %v", err)
	}
}

func testNotFound(t *testing.T, s store.TaskStore) {
	ctx := context.Background()
	if _, err := s.GetTask(ctx, "NOTEXIST"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
	err := s.Transition(ctx, "NOTEXIST", core.StateNew, core.StateReady, "x")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
}

func stepSeqs(list []*core.Step) []int {
	out := make([]int, len(list))
	for i, s := range list {
		out[i] = s.Seq
	}
	return out
}
