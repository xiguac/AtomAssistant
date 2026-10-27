package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/share"
)

// fakeShares 记下收到的调用。
type fakeShares struct {
	created []string
	ttl     time.Duration
	copied  []string
	revoked []string
}

func (f *fakeShares) Create(_ context.Context, taskID, path string, ttl time.Duration) (share.Created, error) {
	f.created, f.ttl = append(f.created, taskID+":"+path), ttl
	return share.Created{ID: "s1", URL: "https://files.example.com/s/TOKEN", Path: path, Files: []string{path}}, nil
}

func (f *fakeShares) List(context.Context) ([]*share.Link, error) {
	return []*share.Link{{ID: "s1", Path: "a.txt"}}, nil
}

func (f *fakeShares) Revoke(_ context.Context, id string) error {
	f.revoked = append(f.revoked, id)
	return nil
}

func (f *fakeShares) CopyReceived(path string) (string, error) {
	f.copied = append(f.copied, path)
	return "分享副本/" + path, nil
}

func TestShare_要开了文件处理和下载入口才有分享工具(t *testing.T) {
	for _, p := range []*Planner{
		NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithShares(true),
		NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithFiles(true),
	} {
		if list := p.toolList(); strings.Contains(list, "share.") {
			t.Fatalf("不应出现分享工具:\n%s", list)
		}
	}
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithFiles(true).WithShares(true)
	for tool, effect := range map[string]core.SideEffect{
		ToolShareCreate: core.SideEffectReversible, ToolShareList: core.SideEffectReadOnly,
		ToolShareRevoke: core.SideEffectReversible, ToolShareCopy: core.SideEffectReversible,
	} {
		d, err := p.toolDecision(tool, nil, "")
		if err != nil || d.SideEffect != effect {
			t.Errorf("%s: 期望 %s，实际 %s (%v)", tool, effect, d.SideEffect, err)
		}
	}
}

func TestShare_审批(t *testing.T) {
	ctx := context.Background()
	task := core.NewTask("t", "g", core.TrustUser)
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithFiles(true).WithShares(true)
	check := func(pol *policy.Policy, tool, params string) (kernel.ApprovalOutcome, string) {
		t.Helper()
		d, err := p.toolDecision(tool, json.RawMessage(params), "")
		if err != nil {
			t.Fatalf("决策失败: %v", err)
		}
		out, reason, err := NewApprover(pol, policy.NewClassifier(), NewMemoryUsageTracker()).WithHistory(&fakeHistory{}).CheckAction(ctx, task, d)
		if err != nil {
			t.Fatalf("判定失败: %v", err)
		}
		return out, reason
	}

	defaults := &policy.Policy{}
	ShareDefaults(defaults)
	if out, _ := check(defaults, ToolShareCreate, `{"path":"报告.pdf","minutes":30}`); out != kernel.ApprovalAuto {
		t.Fatalf("建链接应直接执行: %s", out)
	}
	if out, _ := check(defaults, ToolShareRevoke, `{"id":"s1"}`); out != kernel.ApprovalAuto {
		t.Fatalf("撤销链接应直接执行: %s", out)
	}
	// 复制用户的文件：不管默认规则、用户写的放行规则还是自主模式，都要用户批准。
	allow := &policy.Policy{Rules: []policy.Rule{{Tool: ToolShareCopy}}}
	ShareDefaults(allow)
	for name, pol := range map[string]*policy.Policy{"默认": defaults, "写了放行规则": allow, "自主模式": {Autonomous: true}} {
		out, reason := check(pol, ToolShareCopy, `{"path":"收到的文件/合同.pdf"}`)
		if out != kernel.ApprovalManual || !strings.Contains(reason, "收到的文件/合同.pdf") {
			t.Errorf("%s: 复制用户的文件应要用户批准: %s %q", name, out, reason)
		}
	}
	// 建链接、撤销链接不算对外发送：地址要出去得经过别的动作。
	for _, tool := range []string{ToolShareCreate, ToolShareRevoke, ToolShareCopy} {
		if outbound(tool, core.SideEffectReversible) {
			t.Errorf("%s 不应算对外发送", tool)
		}
	}
}

func TestShare_执行(t *testing.T) {
	ctx := context.Background()
	svc := &fakeShares{}
	a := NewActuator(&fakeTools{}, &fakeModel{}).WithShares(svc)
	task := core.NewTask("t", "g", core.TrustUser)

	res, err := a.Execute(ctx, task, connStep(task, 1, ToolShareCreate, `{"path":"报告.pdf","minutes":90}`))
	if err != nil || !strings.Contains(string(res.Output), "https://files.example.com/s/TOKEN") || res.Trust != core.TrustSystem {
		t.Fatalf("建链接不对: %v %s %s", err, res.Output, res.Trust)
	}
	if len(svc.created) != 1 || svc.created[0] != task.ID+":报告.pdf" || svc.ttl != 90*time.Minute {
		t.Fatalf("参数没有原样交过去: %v %s", svc.created, svc.ttl)
	}
	res, err = a.Execute(ctx, task, connStep(task, 2, ToolShareList, `{}`))
	if err != nil || !strings.Contains(string(res.Output), `"id":"s1"`) || strings.Contains(string(res.Output), "TOKEN") {
		t.Fatalf("列链接不对: %v %s", err, res.Output)
	}
	if _, err := a.Execute(ctx, task, connStep(task, 3, ToolShareRevoke, `{"id":"s1"}`)); err != nil || len(svc.revoked) != 1 {
		t.Fatalf("撤销不对: %v %v", err, svc.revoked)
	}
	res, err = a.Execute(ctx, task, connStep(task, 4, ToolShareCopy, `{"path":"收到的文件/合同.pdf"}`))
	if err != nil || !strings.Contains(string(res.Output), "分享副本/") {
		t.Fatalf("复制不对: %v %s", err, res.Output)
	}
	if _, err := NewActuator(&fakeTools{}, &fakeModel{}).Execute(ctx, task, connStep(task, 5, ToolShareCreate, `{"path":"a"}`)); err == nil {
		t.Fatal("没开对外分享时执行应失败")
	}
}
