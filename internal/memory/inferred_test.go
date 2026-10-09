package memory_test

import (
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/memory/memstore"
)

// inferred 是助手在某个任务里自己推断出来的一条候选。
func inferred(task, domain, value string) memory.Candidate {
	return memory.Candidate{
		Kind: memory.KindFact, Subject: "user", Domain: domain, Value: value,
		Temporality: memory.TemporalDurable,
		Source:      memory.Source{Kind: memory.SourceTask, Ref: task, Trust: core.TrustToolOutput},
		Evidence:    []string{"task:" + task},
	}
}

// stated 是用户亲口说的一条候选。
func stated(task, domain, value string) memory.Candidate {
	c := inferred(task, domain, value)
	c.Source = memory.Source{Kind: memory.SourceUser, Ref: task, Trust: core.TrustUser}
	return c
}

func ingest(t *testing.T, p *memory.Pipeline, c memory.Candidate) memory.Outcome {
	t.Helper()
	out, err := p.Ingest(ctx(), []memory.Candidate{c})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("应有一条结果，实际 %d", len(out))
	}
	return out[0]
}

func TestInferred_推断先以较轻的分量记下不打扰用户(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)

	o := ingest(t, p, inferred("t1", "居住", "住在杭州"))
	if o.PendingID != "" || o.MemoryID == "" {
		t.Fatalf("助手的推断应直接记下而不是挂起等确认: %+v", o)
	}
	m, _ := st.Get(ctx(), o.MemoryID)
	if m.Confidence != memory.InferredConfidence || !m.Inferred() || m.Status != memory.StatusActive {
		t.Fatalf("推断的起始分量应较轻且标明是推断: %v %v %s", m.Confidence, m.Inferred(), m.Status)
	}
	// 给模型看的时候要说明这是推断，不能和用户说过的混为一谈。
	if text := memory.RenderFull(m); !strings.Contains(text, "助手自己推断的") || !strings.Contains(text, "先向用户核实") {
		t.Fatalf("上下文里应标明这是未经确认的推断:\n%s", text)
	}
	if pend, _ := st.ListPending(ctx(), ""); len(pend) != 0 {
		t.Fatal("推断不应产生待确认的条目")
	}

	// 用户亲口说的：照旧直接生效，分量重，不标推断。
	u := ingest(t, p, stated("t1", "职业", "做后端开发"))
	um, _ := st.Get(ctx(), u.MemoryID)
	if u.PendingID != "" || um.Confidence < 0.9 || um.Inferred() {
		t.Fatalf("用户说的应直接以重分量生效: %+v %v", u, um.Confidence)
	}

	// 两种情况仍然要用户点头：安全相关的推断，和读过外部内容的任务里提炼出来的东西。
	allergy := inferred("t1", "饮食", "对花生过敏")
	allergy.SafetyCritical = true
	if o := ingest(t, p, allergy); o.PendingID == "" || o.MemoryID != "" {
		t.Fatalf("安全相关的推断必须等用户确认: %+v", o)
	}
	fromMail := inferred("t2", "财务", "工资卡是招行")
	fromMail.Source.Trust = core.TrustExternal
	if o := ingest(t, p, fromMail); o.PendingID == "" || o.MemoryID != "" {
		t.Fatalf("外部内容里提炼出来的必须等用户确认: %+v", o)
	}
}

func TestInferred_交叉印证加重分量但越不过用户亲口说的(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)
	id := ingest(t, p, inferred("t1", "出行", "去上海习惯坐高铁")).MemoryID
	conf := func() float64 { m, _ := st.Get(ctx(), id); return m.Confidence }

	// 同一个任务把同一件事再说一遍：不是新的印证。
	ingest(t, p, inferred("t1", "出行", "去上海习惯坐高铁"))
	if conf() != memory.InferredConfidence {
		t.Fatalf("同一个任务重复提炼不应加重: %v", conf())
	}
	// 另一个任务里又推断出同样的事：印证一次，加重一档。
	ingest(t, p, inferred("t2", "出行", "去上海习惯坐高铁"))
	if got := conf(); got <= memory.InferredConfidence || got >= 0.6 {
		t.Fatalf("被另一个任务印证后应加重一档: %v", got)
	}
	// 印证得再多，也到不了用户亲口说的那个分量。
	for _, task := range []string{"t3", "t4", "t5", "t6", "t7", "t8"} {
		ingest(t, p, inferred(task, "出行", "去上海习惯坐高铁"))
	}
	if got := conf(); got != memory.InferredCeiling {
		t.Fatalf("推断靠印证只能涨到上限 %v，实际 %v", memory.InferredCeiling, got)
	}
	if m, _ := st.Get(ctx(), id); !m.Inferred() {
		t.Fatal("没有用户确认，它始终是推断")
	}

	// 用户亲口说中了：这条记忆从此算在用户名下。
	ingest(t, p, stated("t9", "出行", "去上海习惯坐高铁"))
	m, _ := st.Get(ctx(), id)
	if m.Inferred() || m.Confidence < 0.9 || m.Current.Source.Trust != core.TrustUser {
		t.Fatalf("用户说中之后应转到用户名下: %v %+v", m.Confidence, m.Current.Source)
	}
	if strings.Contains(memory.RenderFull(m), "助手自己推断的") {
		t.Fatal("用户确认过的不应再标成推断")
	}
}

func TestInferred_与旧记忆不一致时(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)

	// 推断对推断：新的取代旧的，旧值留在历史里，分量重新从轻的算起。
	id := ingest(t, p, inferred("t1", "作息", "一般十二点睡")).MemoryID
	ingest(t, p, inferred("t2", "作息", "一般十二点睡"))
	o := ingest(t, p, inferred("t3", "作息", "最近改成十一点睡了"))
	m, _ := st.Get(ctx(), id)
	if o.PendingID != "" || m.Current.Value != "最近改成十一点睡了" || len(m.History) != 1 || m.Confidence != memory.InferredConfidence {
		t.Fatalf("新的推断应取代旧的推断并从轻分量重新算起: %+v %v %d", o, m.Confidence, len(m.History))
	}

	// 推断对用户说过的话：不动用户的话，也不给它打上"有分歧"，只把推断挂起来请用户核实。
	said := ingest(t, p, stated("t1", "居住", "住在杭州")).MemoryID
	o = ingest(t, p, inferred("t4", "居住", "住在苏州"))
	sm, _ := st.Get(ctx(), said)
	if o.PendingID == "" || sm.Current.Value != "住在杭州" || sm.Status != memory.StatusActive {
		t.Fatalf("推断不能改动用户说过的话: %+v %s %s", o, sm.Current.Value, sm.Status)
	}
	pend, _ := st.ListPending(ctx(), "")
	if len(pend) != 1 || !strings.Contains(pend[0].Arb.Reason, "核实") {
		t.Fatalf("应挂起一条等用户核实: %+v", pend)
	}

	// 用户说的和自己以前说的不一致：这才是要和用户核实的冲突。
	o = ingest(t, p, stated("t5", "居住", "住在苏州"))
	sm, _ = st.Get(ctx(), said)
	if o.PendingID == "" && sm.Current.Value != "住在苏州" {
		t.Fatalf("用户前后说法不一致时，要么核实要么按时效更替更新，不能悄悄丢掉: %+v", o)
	}
}

func TestAffirmForget_用户对已记下的记忆表态(t *testing.T) {
	st := memstore.New()
	p := memory.NewPipeline(st, nil)
	a := ingest(t, p, inferred("t1", "饮食", "不太吃辣")).MemoryID
	b := ingest(t, p, inferred("t1", "作息", "习惯早起")).MemoryID

	if err := p.Affirm(ctx(), a); err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	m, _ := st.Get(ctx(), a)
	if m.Inferred() || m.Confidence < 0.9 {
		t.Fatalf("用户确认后应转到用户名下、分量加重: %v", m.Confidence)
	}

	if err := p.Forget(ctx(), b); err != nil {
		t.Fatalf("忘掉失败: %v", err)
	}
	// 退出检索，但不物理删除：之后还能回溯助手当时为什么那样判断。
	if list, _ := st.List(ctx(), memory.Query{}); len(list) != 1 || list[0].ID != a {
		t.Fatalf("忘掉的记忆不应再被检索到: %+v", list)
	}
	if all, _ := st.List(ctx(), memory.Query{IncludeInactive: true}); len(all) != 2 {
		t.Fatalf("忘掉的记忆应仍在库里可回溯: %d", len(all))
	}
	if err := p.Affirm(ctx(), "不存在"); err == nil {
		t.Fatal("不存在的记忆应报错")
	}
}
