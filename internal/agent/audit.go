package agent

import (
	"context"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/store"
)

// StoreAudit 把网关的审计写进任务审计表。
//
// 工具调用的留痕不是可选项：它是"隐私账本"视图的数据来源，
// 也是用户敢把全域数据交给这个系统的唯一理由。
type StoreAudit struct {
	store store.TaskStore
}

var _ mcpgw.AuditSink = (*StoreAudit)(nil)

// NewStoreAudit 构造审计适配器。
func NewStoreAudit(s store.TaskStore) *StoreAudit { return &StoreAudit{store: s} }

// ToolCall 记录一次工具调用。
func (a *StoreAudit) ToolCall(ctx context.Context, taskID, tool string, trust core.TrustLevel, detail string) error {
	return a.store.Audit(ctx, store.AuditEntry{
		Actor:     taskID,
		Action:    "tool.call",
		TaskID:    taskID,
		Detail:    tool + ": " + detail,
		TrustAtOp: trust,
	})
}
