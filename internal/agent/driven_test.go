package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/connector/holiday"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/policy"
)

// fakeHistory 按任务给出走过的步骤。
type fakeHistory struct {
	steps []*core.Step
	err   error
}

func (f *fakeHistory) ListSteps(context.Context, string) ([]*core.Step, error) {
	return f.steps, f.err
}

func doneStep(tool string, trust core.TrustLevel) *core.Step {
	s := core.NewStep("t", 1, core.StepTool, nil)
	s.ToolName, s.Status, s.OutputTrust = tool, core.StepDone, trust
	return s
}

func TestCalendarDefaults_用户交代的日程直接写读过外部内容的要确认(t *testing.T) {
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).
		WithCalendar(true).WithMail(true, true)
	far := time.Now().Add(72 * time.Hour).Format(time.RFC3339)
	create := `{"title":"周五评审","start_at":"` + far + `"}`
	update := `{"id":"/c/a.ics","occurrence":"` + far + `","cancel":true}`

	build := func(rules []policy.Rule, h *fakeHistory) *Approver {
		pol := &policy.Policy{Rules: rules}
		CalendarDefaults(pol)
		return NewApprover(pol, policy.NewClassifier(), NewMemoryUsageTracker()).WithHistory(h)
	}
	check := func(a *Approver, task *core.Task, tool, params string) (kernel.ApprovalOutcome, string) {
		t.Helper()
		d, err := p.toolDecision(tool, json.RawMessage(params), "")
		if err != nil {
			t.Fatalf("决策失败: %v", err)
		}
		out, reason, err := a.CheckAction(context.Background(), task, d)
		if err != nil {
			t.Fatalf("判定失败: %v", err)
		}
		return out, reason
	}
	mine := core.NewTask("t", "周五下午三点评审 记一下", core.TrustUser)

	// 用户亲口交代的：新建、修改（含调课、取消某一次）直接做。
	clean := build(nil, &fakeHistory{})
	for tool, params := range map[string]string{ToolCalendarCreate: create, ToolCalendarUpdate: update} {
		if out, reason := check(clean, mine, tool, params); out != kernel.ApprovalAuto {
			t.Errorf("%s 应直接执行，实际 %s（%s）", tool, out, reason)
		}
	}
	// 删除整条日程找不回来，仍然要确认。
	if out, _ := check(clean, mine, ToolCalendarDelete, `{"id":"/c/a.ics"}`); out != kernel.ApprovalManual {
		t.Fatalf("删除应要确认: %s", out)
	}
	// 发信不在默认放行之列。
	if out, _ := check(clean, mine, ToolMailSend, `{"to":["a@b.com"],"subject":"x","body":"y"}`); out != kernel.ApprovalManual {
		t.Fatalf("发信应要确认: %s", out)
	}

	// 写之前查过自己的日历、问过用户：都不算读外部内容。
	own := build(nil, &fakeHistory{steps: []*core.Step{
		doneStep(ToolCalendarEvents, core.TrustExternal),
		doneStep(ToolCalendarList, core.TrustToolOutput),
		doneStep("", core.TrustUser),
		doneStep(ToolPlan, core.TrustToolOutput),
	}})
	if out, reason := check(own, mine, ToolCalendarCreate, create); out != kernel.ApprovalAuto {
		t.Fatalf("查过自己的日历之后新建仍应直接执行: %s（%s）", out, reason)
	}

	// 任务读过邮件：日程是邮件里说的，交给用户定夺。
	mailed := build(nil, &fakeHistory{steps: []*core.Step{
		doneStep(ToolCalendarEvents, core.TrustExternal),
		doneStep(ToolMailRead, core.TrustExternal),
	}})
	out, reason := check(mailed, mine, ToolCalendarCreate, create)
	if out != kernel.ApprovalManual || !strings.Contains(reason, ToolMailRead) || !strings.Contains(reason, "定夺") {
		t.Fatalf("读过邮件之后的新建应要确认并说明原因: %s（%s）", out, reason)
	}
	// 没执行成的那一步不算数：读信失败了，就没有外部内容进来。
	failed := doneStep(ToolMailRead, core.TrustExternal)
	failed.Status = core.StepFailed
	if out, _ := check(build(nil, &fakeHistory{steps: []*core.Step{failed}}), mine, ToolCalendarCreate, create); out != kernel.ApprovalAuto {
		t.Fatalf("失败的读取不应影响判定: %s", out)
	}
	// 外部内容同样让用户配的其他放行规则失效，不只是日历。
	ride := []policy.Rule{{Tool: ToolMailSend}}
	if out, _ := check(build(ride, &fakeHistory{}), mine, ToolMailSend, `{"to":["a@b.com"],"subject":"x","body":"y"}`); out != kernel.ApprovalAuto {
		t.Fatalf("用户放行了发信且任务没读过外部内容，应直接执行: %s", out)
	}
	if out, _ := check(build(ride, mailed.history.(*fakeHistory)), mine, ToolMailSend, `{"to":["a@b.com"],"subject":"x","body":"y"}`); out != kernel.ApprovalManual {
		t.Fatalf("读过邮件之后发信应要确认: %s", out)
	}

	// 整理邮件天生是对着外部内容做的，不套这条规则：用户放行了就直接做。
	tidy := []policy.Rule{{Tool: ToolMailMove}}
	if out, _ := check(build(tidy, mailed.history.(*fakeHistory)), mine, ToolMailMove, `{"id":"7.1@INBOX","folder":"trash"}`); out != kernel.ApprovalAuto {
		t.Fatalf("用户放行了整理邮件，应直接执行: %s", out)
	}
	if out, _ := check(mailed, mine, ToolMailMove, `{"id":"7.1@INBOX","folder":"trash"}`); out != kernel.ApprovalManual {
		t.Fatalf("没放行时整理邮件仍要确认: %s", out)
	}

	// 用户想收紧：配置里写了 never，默认放行不覆盖它。
	strict := build([]policy.Rule{{Tool: ToolCalendarCreate, Never: true}}, &fakeHistory{})
	if out, _ := check(strict, mine, ToolCalendarCreate, create); out != kernel.ApprovalManual {
		t.Fatalf("用户写了 never 就必须确认: %s", out)
	}
	if out, _ := check(strict, mine, ToolCalendarUpdate, update); out != kernel.ApprovalAuto {
		t.Fatalf("没写规则的那个工具仍按默认放行: %s", out)
	}

	// 邮件、截图创建的任务，本来就不适用任何放行。
	foreign := core.NewTask("t", "g", core.TrustExternal)
	if out, _ := check(clean, foreign, ToolCalendarCreate, create); out != kernel.ApprovalManual {
		t.Fatalf("外部内容创建的任务应要确认: %s", out)
	}
	// 历史读不出来：不知道读没读过外部内容，就不能放行。
	broken := build(nil, &fakeHistory{err: errors.New("库连不上")})
	if out, _ := check(broken, mine, ToolCalendarCreate, create); out != kernel.ApprovalManual {
		t.Fatalf("读不到历史时应转人工: %s", out)
	}
	// 只读操作不受影响，也不必为它查历史。
	if out, _ := check(broken, mine, ToolCalendarEvents, `{}`); out != kernel.ApprovalAuto {
		t.Fatalf("只读操作应直接执行: %s", out)
	}
}

func TestConnectors_改某一次与取消某一次的参数(t *testing.T) {
	cal := &fakeCalendar{}
	a := NewActuator(&fakeTools{}, &fakeModel{}).WithCalendar(cal)
	task := core.NewTask("t", "g", core.TrustUser)
	res, err := a.Execute(context.Background(), task, connStep(task, 1, ToolCalendarUpdate,
		`{"id":"/c/b.ics","occurrence":"2026-10-12T10:00:00+08:00","start_at":"2026-10-17T10:00:00+08:00"}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if cal.patch.Occurrence == nil || cal.patch.Occurrence.UTC().Format("01-02 15:04") != "10-12 02:00" || cal.patch.Cancel ||
		cal.patch.Start == nil || !strings.Contains(string(res.Output), `"updated"`) {
		t.Fatalf("应只改指明的那一次: %+v %s", cal.patch, res.Output)
	}
	res, err = a.Execute(context.Background(), task, connStep(task, 2, ToolCalendarUpdate,
		`{"id":"/c/b.ics","occurrence":"2026-10-12T10:00:00+08:00","cancel":true}`))
	if err != nil || !cal.patch.Cancel || !strings.Contains(string(res.Output), `"cancelled_occurrence"`) {
		t.Fatalf("应取消指明的那一次: %v %+v %s", err, cal.patch, res.Output)
	}
}

type fakeHolidays struct{ from, to time.Time }

func (f *fakeHolidays) Between(_ context.Context, from, to time.Time) ([]holiday.Entry, error) {
	f.from, f.to = from, to
	return []holiday.Entry{{Start: "2026-10-10", End: "2026-10-10", Name: "国庆节（班）", Kind: holiday.KindWorkday}}, nil
}

func TestHolidays_查调休不算读外部内容(t *testing.T) {
	bare := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{})
	if strings.Contains(bare.toolList(), ToolHolidays) {
		t.Fatal("没接入节假日数据时不应出现这个工具")
	}
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithHolidays(true)
	if d, err := p.toolDecision(ToolHolidays, nil, ""); err != nil || d.SideEffect != core.SideEffectReadOnly {
		t.Fatalf("节假日查询应是只读的: %+v %v", d, err)
	}

	feed := &fakeHolidays{}
	a := NewActuator(&fakeTools{}, &fakeModel{}).WithHolidays(feed)
	task := core.NewTask("t", "g", core.TrustUser)
	res, err := a.Execute(context.Background(), task, connStep(task, 1, ToolHolidays, `{"from":"2026-10-09","to":"2026-10-12"}`))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if feed.from.Format("01-02") != "10-09" || feed.to.Format("01-02") != "10-12" ||
		!strings.Contains(string(res.Output), `"kind":"workday"`) {
		t.Fatalf("查询范围或结果不对: %s ~ %s %s", feed.from, feed.to, res.Output)
	}
	// 不写时间：从今天起 30 天。
	if _, err := a.Execute(context.Background(), task, connStep(task, 2, ToolHolidays, `{}`)); err != nil ||
		feed.to.Sub(feed.from) < 29*24*time.Hour {
		t.Fatalf("默认应查 30 天: %v %s", err, feed.to.Sub(feed.from))
	}
	// 关键在这里：查过调休之后按它改日程，仍然算用户交代的事，直接做。
	if res.Trust == core.TrustExternal {
		t.Fatal("节假日数据不应按外部内容对待")
	}
	pol := &policy.Policy{}
	CalendarDefaults(pol)
	step := doneStep(ToolHolidays, res.Trust)
	approver := NewApprover(pol, policy.NewClassifier(), NewMemoryUsageTracker()).WithHistory(&fakeHistory{steps: []*core.Step{step}})
	cal := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithCalendar(true)
	d, err := cal.toolDecision(ToolCalendarCreate, json.RawMessage(`{"title":"补班站会","start_at":"2026-10-10T09:30:00+08:00"}`), "")
	if err != nil {
		t.Fatalf("决策失败: %v", err)
	}
	if out, reason, _ := approver.CheckAction(context.Background(), task, d); out != kernel.ApprovalAuto {
		t.Fatalf("查过调休之后记日程应直接执行: %s（%s）", out, reason)
	}
}
