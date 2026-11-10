package channel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/matter"
)

// scriptedTriage 每次按 answer 回答，并记下它看到的候选。
type scriptedTriage struct {
	answer func(in TriageInput) Verdict
	seen   []Candidate
}

func (s *scriptedTriage) Triage(_ context.Context, in TriageInput) (Verdict, error) {
	s.seen = in.Candidates
	return s.answer(in), nil
}

// fakeDeleter 记下要删的任务编号。
type fakeDeleter struct {
	calls [][]string
}

func (f *fakeDeleter) DeleteFinishedTasks(_ context.Context, ids []string) (int, error) {
	f.calls = append(f.calls, ids)
	return len(ids), nil
}

// pickTitled 归给清单里标题为 title 的那件事。
func pickTitled(in TriageInput, title string) Verdict {
	for _, c := range in.Candidates {
		if c.Title == title {
			return Verdict{TaskID: c.ID}
		}
	}
	return Verdict{}
}

func TestMatters_隔了几天的话接到原来那件事上(t *testing.T) {
	h, qq, tasks := hub(t)
	store, tri := matter.NewMemStore(), &scriptedTriage{}
	h = h.WithTriage(tri).WithMatters(store, &fakeDeleter{})
	ctx := context.Background()
	now := time.Now()
	h.now = func() time.Time { return now }
	say := func(text string) { h.Handle(ctx, "qq", from("OWNER", text, "m")) }

	// 一件新的事：清单里记下，标题和种类用模型判断的，任务挂在它名下。
	tri.answer = func(TriageInput) Verdict { return Verdict{New: true, Title: "订周五去苏州的高铁"} }
	say("帮我订一张周五去苏州的高铁票，早上的")
	list, _ := store.List(ctx)
	if len(list) != 1 || list[0].Title != "订周五去苏州的高铁" || list[0].Kind != matter.KindOnce || list[0].Channel != "qq" {
		t.Fatalf("新的事应记进清单: %+v", list)
	}
	id := list[0].ID
	first := tasks.tasks[0]
	if matter.FromTags(first.Tags) != id || first.Title != "订周五去苏州的高铁" {
		t.Fatalf("任务应挂在这件事名下、用它的标题: %+v", first)
	}

	// 办完了：结果推给用户，同时记进这件事的进展。
	first.State, first.Reason = core.StateDone, "已订 G7001，周五 07:30 出发"
	if err := h.Notify(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	m, _ := store.Get(ctx, id)
	if len(m.Notes) != 1 || !strings.Contains(m.Notes[0].Result, "G7001") || !strings.Contains(m.Notes[0].Ask, "早上的") {
		t.Fatalf("办完的结果应记进这件事的进展: %+v", m.Notes)
	}

	// 两天后补了一句：任务早就办完了，清单里的事还在，候选里带着上次的结果。
	now = now.Add(48 * time.Hour)
	tri.answer = func(in TriageInput) Verdict { return pickTitled(in, "订周五去苏州的高铁") }
	before := len(qq.sent)
	say("那改成周六呢")
	if len(tri.seen) != 1 || !tri.seen[0].Matter || !strings.Contains(tri.seen[0].Last, "G7001") || tri.seen[0].Dormant != "" {
		t.Fatalf("候选应是清单里的这件事，带着最近的进展: %+v", tri.seen)
	}
	second := tasks.tasks[0]
	if second == first || matter.FromTags(second.Tags) != id || second.Title != "订周五去苏州的高铁" || second.Goal != "那改成周六呢" {
		t.Fatalf("应在这件事名下新开一个任务，标题用这件事的: %+v", second)
	}
	if list, _ = store.List(ctx); len(list) != 1 {
		t.Fatalf("接着说的话不应多出一件事: %d", len(list))
	}
	if len(qq.sent) != before+1 || qq.sent[before].Text != defaultAck {
		t.Fatalf("交给任务后应回一句回执: %+v", qq.sent[before:])
	}
	// 任务还在办的时候再补一句：送给这个任务，不另开。
	n := len(tasks.tasks)
	say("要靠窗的")
	if len(tasks.tasks) != n || tasks.log[len(tasks.log)-1] != "reply:订周五去苏州的高铁|要靠窗的|qq" {
		t.Fatalf("这件事有任务在办时，话应送给那个任务: %v", tasks.log)
	}

	// 新的事可以挂在一件长期的事下面。
	tri.answer = func(TriageInput) Verdict { return Verdict{New: true, Title: "操作系统课", Ongoing: true} }
	say("我这学期选了操作系统课，以后课上的事你帮我记着")
	var course *matter.Matter
	list, _ = store.List(ctx)
	for _, m := range list {
		if m.Title == "操作系统课" {
			course = m
		}
	}
	if course == nil || course.Kind != matter.KindOngoing {
		t.Fatalf("应记下一件长期的事: %+v", list)
	}
	tri.answer = func(TriageInput) Verdict {
		return Verdict{New: true, Title: "收第三次作业", Under: matter.Tag(course.ID)}
	}
	say("帮我收一下操作系统第三次作业")
	found := false
	list, _ = store.List(ctx)
	for _, m := range list {
		if m.Title == "收第三次作业" {
			found = m.ParentID == course.ID
		}
	}
	if !found {
		t.Fatalf("一次性的事应挂在长期的事下面: %+v", list)
	}
}

func TestMatters_放久了的事淡出删除长期的先问用户(t *testing.T) {
	h, qq, tasks := hub(t)
	store, tri, del := matter.NewMemStore(), &scriptedTriage{}, &fakeDeleter{}
	h = h.WithTriage(tri).WithMatters(store, del)
	ctx := context.Background()
	now := time.Now()
	h.now = func() time.Time { return now }
	put := func(title string, kind matter.Kind, parent string, idleDays int) *matter.Matter {
		m := &matter.Matter{ID: core.NewID(), Title: title, Kind: kind, ParentID: parent, Channel: "qq", Chat: "c2c:OWNER",
			LastActiveAt: now.Add(-time.Duration(idleDays) * 24 * time.Hour), CreatedAt: now.Add(-100 * 24 * time.Hour)}
		if err := store.Put(ctx, m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	titles := func() string {
		list, _ := store.List(ctx)
		var out []string
		for _, m := range list {
			out = append(out, m.Title)
		}
		return strings.Join(out, ",")
	}
	fresh := put("刚说的事", matter.KindOnce, "", 1)
	low := put("一周多没动的事", matter.KindOnce, "", 8)
	archived := put("两周多没动的事", matter.KindOnce, "", 20)
	old := put("一个多月没动的事", matter.KindOnce, "", 31)
	course := put("操作系统课", matter.KindOngoing, "", 40)
	put("收第三次作业", matter.KindOnce, course.ID, 45)
	if fresh.Stage(now) != matter.StageActive || low.Stage(now) != matter.StageLow || archived.Stage(now) != matter.StageArchived ||
		old.Stage(now) != matter.StageExpired || course.Stage(now) != matter.StageExpired {
		t.Fatal("阶段应按最后活动的时间算")
	}
	// 一个多月没动的那件事名下有一个办完的任务。
	done := tasks.add("旧任务", Tag("qq", "c2c:OWNER"), matter.Tag(old.ID))
	done.State = core.StateDone

	if err := h.tidyMatters(ctx); err != nil {
		t.Fatal(err)
	}
	// 一次性的、没挂在别的事下面的：直接删，连同它名下的任务。删任务一定带着编号。
	if strings.Contains(titles(), "一个多月没动的事") || len(del.calls) != 1 || len(del.calls[0]) != 1 || del.calls[0][0] != done.ID {
		t.Fatalf("放了 30 天的一次性的事应删掉，连同名下的任务: %s %v", titles(), del.calls)
	}
	// 长期的事先问用户；它下面的事在它问清楚之前不问、不删。
	if n := len(qq.sent); n != 1 || !strings.Contains(qq.sent[0].Text, "操作系统课") || !strings.Contains(qq.sent[0].Text, "下面的 1 件事") {
		t.Fatalf("长期的事应先问用户还继续吗: %+v", qq.sent)
	}
	if got, _ := store.Get(ctx, course.ID); got.AskedAt == nil || !got.LastActiveAt.Equal(course.LastActiveAt) {
		t.Fatalf("问过要记下，但问一句不算有了动静: %+v", got)
	}
	// 没等到回答：再整理一遍也不重复问，什么都不删。
	if err := h.tidyMatters(ctx); err != nil || len(qq.sent) != 1 || !strings.Contains(titles(), "收第三次作业") {
		t.Fatalf("没回答时不应重复问或删除: %v %d %s", err, len(qq.sent), titles())
	}

	// 用户说还上：留着。过一天再整理，才问它下面那件。
	tri.answer = func(in TriageInput) Verdict {
		v := pickTitled(in, "操作系统课")
		v.Keep = true
		return v
	}
	h.Handle(ctx, "qq", from("OWNER", "还上的", "m"))
	if got, _ := store.Get(ctx, course.ID); got.AskedAt != nil || !got.LastActiveAt.Equal(now) || !strings.Contains(qq.sent[len(qq.sent)-1].Text, "留着") {
		t.Fatalf("用户说继续，这件事应留着: %+v", got)
	}
	created := len(tasks.log)
	now = now.Add(25 * time.Hour)
	if err := h.tidyMatters(ctx); err != nil {
		t.Fatal(err)
	}
	if last := qq.sent[len(qq.sent)-1].Text; !strings.Contains(last, "「操作系统课」下面的「收第三次作业」") {
		t.Fatalf("长期的事还继续，再问它下面放久了的那件: %s", last)
	}
	// 用户说不要了：删掉这一件，长期的事还在。回答这个问题不该建任务。
	tri.answer = func(in TriageInput) Verdict {
		v := pickTitled(in, "收第三次作业")
		v.Close = true
		return v
	}
	h.Handle(ctx, "qq", from("OWNER", "删了吧", "m"))
	if strings.Contains(titles(), "收第三次作业") || !strings.Contains(titles(), "操作系统课") || len(tasks.log) != created {
		t.Fatalf("应只删掉这一件，不建任务: %s %v", titles(), tasks.log[created:])
	}

	// 长期的事不上了：连同它下面的事一起删。
	sub := put("收第四次作业", matter.KindOnce, course.ID, 2)
	h.updateMatter(ctx, course.ID, func(m *matter.Matter) { m.AskedAt = &now })
	tri.answer = func(in TriageInput) Verdict {
		v := pickTitled(in, "操作系统课")
		v.Close = true
		return v
	}
	h.Handle(ctx, "qq", from("OWNER", "不上了，结课了", "m"))
	if strings.Contains(titles(), "操作系统课") || strings.Contains(titles(), sub.Title) || !strings.Contains(titles(), "刚说的事") {
		t.Fatalf("长期的事不继续时应连同下面的事一起删，别的不动: %s", titles())
	}
	// 没问过的事，模型说要删也不认：照常当作接着说的话办。
	tri.answer = func(in TriageInput) Verdict {
		v := pickTitled(in, "刚说的事")
		v.Close = true
		return v
	}
	h.Handle(ctx, "qq", from("OWNER", "这个不要了", "m"))
	if !strings.Contains(titles(), "刚说的事") {
		t.Fatalf("没问过的事不应因为一句话被删: %s", titles())
	}
}
