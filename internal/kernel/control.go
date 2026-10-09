package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/store"
)

// 这个文件是用户对任务的裁决入口：批准、拒绝、确认不可逆操作是否生效。
// 现在由 atomctl 调用，将来 App 的 BFF 调的也是这几个函数。

var (
	// ErrNothingToDecide 表示任务当前没有等待用户裁决的步骤。
	ErrNothingToDecide = errors.New("kernel: 任务没有等待裁决的步骤")
)

// Approve 批准任务当前等待确认的动作，并把任务放回队列。
//
// 可重复调用：步骤已是 approved 而任务还没被唤醒（上次调用中途失败）时，
// 只补做唤醒。
func Approve(ctx context.Context, s store.TaskStore, taskID string) error {
	steps, err := s.ListSteps(ctx, taskID)
	if err != nil {
		return err
	}
	step := lastWithStatus(steps, core.StepAwaiting)
	if step != nil {
		step.Status = core.StepApproved
		if err := s.SaveStep(ctx, step); err != nil {
			return err
		}
		if err := s.Audit(ctx, store.AuditEntry{
			Actor:     "user",
			Action:    "approval.granted",
			TaskID:    taskID,
			Detail:    fmt.Sprintf("seq=%d tool=%s", step.Seq, step.ToolName),
			TrustAtOp: core.TrustUser,
		}); err != nil {
			return err
		}
	} else if lastWithStatus(steps, core.StepApproved) == nil {
		return fmt.Errorf("%w: %s", ErrNothingToDecide, taskID)
	}
	return wake(ctx, s, taskID, core.StateWaiting, "用户已确认")
}

// Reject 拒绝任务当前等待确认的动作。
//
// 被拒绝的步骤标记为 skipped 并留在日志里，任务回到队列——
// Planner 会看到这条拒绝记录，换一个方案或者结束任务。
func Reject(ctx context.Context, s store.TaskStore, taskID, reason string) error {
	steps, err := s.ListSteps(ctx, taskID)
	if err != nil {
		return err
	}
	step := lastWithStatus(steps, core.StepAwaiting)
	if step == nil {
		return fmt.Errorf("%w: %s", ErrNothingToDecide, taskID)
	}
	msg := "用户拒绝执行"
	if reason != "" {
		msg += ": " + reason
	}
	step.Fail(errors.New(msg))
	step.Status = core.StepSkipped
	// 拒绝理由是用户本人说的话。不改的话它会顶着步骤默认的 external 等级进上下文，
	// 模型会把"改成 3000 再订"当成不可信内容里的注入而拒绝照办。
	step.OutputTrust = core.TrustUser
	if err := s.SaveStep(ctx, step); err != nil {
		return err
	}
	if err := s.Audit(ctx, store.AuditEntry{
		Actor:     "user",
		Action:    "approval.rejected",
		TaskID:    taskID,
		Detail:    fmt.Sprintf("seq=%d tool=%s: %s", step.Seq, step.ToolName, msg),
		TrustAtOp: core.TrustUser,
	}); err != nil {
		return err
	}
	return wake(ctx, s, taskID, core.StateWaiting, msg)
}

// Resolve 裁决一个执行中断、状态不明的不可逆步骤（见 ErrNeedsReconciliation）。
//
// happened 为真表示用户确认副作用已经发生，步骤记为完成；
// 为假表示确认没有发生，步骤记为失败，由 Planner 决定是否重做——
// 重做会是一个新步骤，带新的幂等键，并重新经过审批。
func Resolve(ctx context.Context, s store.TaskStore, taskID string, happened bool) error {
	steps, err := s.ListSteps(ctx, taskID)
	if err != nil {
		return err
	}
	step := lastPending(steps)
	if step == nil {
		return fmt.Errorf("%w: %s", ErrNothingToDecide, taskID)
	}
	detail := "用户确认该操作未生效"
	if happened {
		detail = "用户确认该操作已生效"
		out, err := json.Marshal(map[string]string{"note": detail})
		if err != nil {
			return fmt.Errorf("kernel: 序列化裁决结果失败: %w", err)
		}
		// 没有拿到工具的真实返回，只有用户的一句确认，所以信任等级是 user 而不是工具的。
		step.Complete(out, core.TrustUser)
	} else {
		step.Fail(errors.New(detail))
	}
	if err := s.SaveStep(ctx, step); err != nil {
		return err
	}
	if err := s.Audit(ctx, store.AuditEntry{
		Actor:     "user",
		Action:    "step.reconciled",
		TaskID:    taskID,
		Detail:    fmt.Sprintf("seq=%d tool=%s: %s", step.Seq, step.ToolName, detail),
		TrustAtOp: core.TrustUser,
	}); err != nil {
		return err
	}
	return wake(ctx, s, taskID, core.StateSuspended, detail)
}

// wake 把任务从 from 状态放回 ready。任务已经不在 from（已被唤醒或已在跑）不算错误。
func wake(ctx context.Context, s store.TaskStore, taskID string, from core.TaskState, reason string) error {
	err := s.Transition(ctx, taskID, from, core.StateReady, reason)
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	return err
}

// 任务进入等待确认时，原因是按这个格式拼出来的：
// 等待用户确认：<工具>（<策略给的理由>；助手的理由：<助手为什么想这么做>）
const (
	approvalPrefix = "等待用户确认："
	assistantWhy   = "；助手的理由："
)

// SplitApprovalReason 把等待确认的原因拆回两部分：策略为什么拦下，助手为什么想这么做。
// 不是这个格式的原样放进 policy 返回。
func SplitApprovalReason(reason string) (policy, why string) {
	rest, ok := strings.CutPrefix(reason, approvalPrefix)
	if !ok {
		return reason, ""
	}
	open := strings.Index(rest, "（")
	if open < 0 || !strings.HasSuffix(rest, "）") {
		return "", ""
	}
	inner := strings.TrimSuffix(rest[open+len("（"):], "）")
	policy, why, _ = strings.Cut(inner, assistantWhy)
	return policy, why
}
