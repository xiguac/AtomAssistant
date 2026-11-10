package channel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/matter"
)

func TestRoute_同时有几件事在等用户时一句好由主模型判断是说哪件(t *testing.T) {
	h, _, tasks := hub(t)
	tri := &scriptedTriage{}
	h = h.WithTriage(tri)
	ctx := context.Background()
	tag := Tag("qq", "c2c:OWNER")
	say := func(text string) { h.Handle(ctx, "qq", from("OWNER", text, "m")) }
	last := func() string { return tasks.log[len(tasks.log)-1] }

	mail := tasks.add("给张总发邮件", tag)
	awaitApproval(tasks, mail, "s1", "mail.send", `{"to":"zhang@example.com"}`, "要发这封信")

	// 只有这一件事在等用户：一句"好"就是同意，不用问模型。
	asked := 0
	tri.answer = func(in TriageInput) Verdict { asked++; return Verdict{} }
	say("好")
	if last() != "approve:给张总发邮件" || asked != 0 {
		t.Fatalf("只有一件事在等确认时应直接放行: %s（问了模型 %d 次）", last(), asked)
	}

	// 另一件事刚问了一个问题：这句"可以"是在回答它，不能把等确认的那封信发出去。
	trip := tasks.add("订周五的高铁", tag)
	ask(trip, "要不要顺便订酒店？")
	tri.answer = func(in TriageInput) Verdict { asked++; return pickTitled(in, "订周五的高铁") }
	say("可以")
	if asked != 1 || last() != "reply:订周五的高铁|可以|qq" {
		t.Fatalf("几件事都在等用户时应由模型判断，答的是提问的那件: %s（问了模型 %d 次）", last(), asked)
	}
	for _, l := range tasks.log[1:] {
		if strings.HasPrefix(l, "approve:") {
			t.Fatalf("不应放行等确认的动作: %v", tasks.log)
		}
	}
	// 模型判断这句"好"说的是等确认的那件：按同意放行，而不是当成修改意见。
	tri.answer = func(in TriageInput) Verdict { return pickTitled(in, "给张总发邮件") }
	say("好的")
	if last() != "approve:给张总发邮件" {
		t.Fatalf("判给等确认的那件事的同意应放行: %s", last())
	}
	// 判给它的不是同意：照旧当作修改意见。
	say("收件人换成李总")
	if !strings.HasPrefix(last(), "reject:给张总发邮件") {
		t.Fatalf("不是同意的话应作为修改意见: %s", last())
	}
}

func TestAskWhich_拿不准时只列最近的几件选了清单里的事就接上(t *testing.T) {
	h, qq, tasks := hub(t)
	store, tri := matter.NewMemStore(), &scriptedTriage{}
	h = h.WithTriage(tri).WithMatters(store, &fakeDeleter{})
	ctx := context.Background()
	say := func(text string) { h.Handle(ctx, "qq", from("OWNER", text, "m")) }

	tri.answer = func(TriageInput) Verdict { return Verdict{New: true, Title: "订高铁"} }
	say("订周五去苏州的高铁")
	tasks.tasks[0].State = core.StateDone
	// 过了接续的时间窗，模型又拿不准：问用户。
	now := h.now().Add(3 * continueWindow)
	h.now = func() time.Time { return now }
	tri.answer = func(TriageInput) Verdict { return Verdict{} }
	say("再便宜点的")
	if q := qq.sent[len(qq.sent)-1].Text; !strings.Contains(q, "1. 订高铁") || !strings.Contains(q, "0. 都不是") {
		t.Fatalf("应问用户是哪件事: %s", q)
	}
	n := len(tasks.tasks)
	say("1")
	list, _ := store.List(ctx)
	if len(list) != 1 || len(tasks.tasks) != n+1 || matter.FromTags(tasks.tasks[0].Tags) != list[0].ID || tasks.tasks[0].Goal != "再便宜点的" {
		t.Fatalf("选了清单里的事，应在它名下新开任务，不多出一件事: %d 件事 %+v", len(list), tasks.tasks[0])
	}
}
