package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/memory/memstore"
)

func TestEpilogue_助手的推断直接以轻分量记下(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	task := core.NewTask("周末去哪", "周末想出去走走，帮我看看去哪", core.TrustUser)
	task.Reason = "建议周六去莫干山"
	// 整个任务只有用户的话和助手自己查的权威数据，没有读过任何外部内容。
	weather := toolStep(task.ID, 2, "demo.weather", `{"city":"杭州","text":"周六晴"}`)
	weather.OutputTrust = core.TrustConnector
	history := []*core.Step{userReplyStep(task.ID, 1, "别太远，开车两小时以内吧"), weather}

	model := &fakeModel{replies: []string{`{"memories":[
	  {"kind":"preference","subject":"user","domain":"出行","condition":{"场景":"周末短途"},
	   "value":"车程两小时以内","temporality":"durable","quote":"开车两小时以内吧"},
	  {"kind":"fact","subject":"user","domain":"居住","value":"住在杭州一带","temporality":"durable","quote":""},
	  {"kind":"entity","subject":"user","domain":"饮食","attribute":"过敏","value":"可能对海鲜过敏",
	   "temporality":"permanent","safety_critical":true,"quote":""}
	]}`}}
	if _, err := NewEpilogue(model, memory.NewPipeline(st, nil)).TaskFinished(ctx, task, history); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}

	byKind := map[memory.Kind]*memory.Memory{}
	active, _ := st.List(ctx, memory.Query{})
	for _, m := range active {
		byKind[m.Kind] = m
	}
	// 用户亲口说的：直接生效，重分量。
	if p := byKind[memory.KindPreference]; p == nil || p.Inferred() || p.Confidence < 0.9 {
		t.Fatalf("用户亲口说的偏好应直接以重分量生效: %+v", p)
	}
	// 助手自己推断的事实：不问用户，直接记下，但分量轻、标明是推断。
	if m := byKind[memory.KindFact]; m == nil || !m.Inferred() || m.Confidence != memory.InferredConfidence {
		t.Fatalf("推断的事实应以轻分量记下: %+v", m)
	}
	// 安全相关的推断是例外：记错一次就是事故，仍然等用户点头。
	pending, _ := st.ListPending(ctx, "")
	if len(pending) != 1 || !pending[0].Candidate.SafetyCritical || byKind[memory.KindEntity] != nil {
		t.Fatalf("安全相关的推断应挂起等确认，且只有它: %+v", pending)
	}

	// 记忆里留下的、助手自己总结的流程进上下文时标成"参考做法"，不是必须遵守的标准流程。
	// 收尾已经不再把流程写进记忆，这里直接放一条，模拟以前留下来的。
	pb := memory.New(memory.KindPlaybook, "user", "周末短途出行", "1. 确认车程上限 2. 查天气 3. 给两个备选",
		memory.Source{Kind: memory.SourceTask, Ref: task.ID, Trust: core.TrustToolOutput}, time.Now().UTC())
	pb.Temporality = memory.TemporalDurable
	if err := st.Put(ctx, pb); err != nil {
		t.Fatalf("写入流程失败: %v", err)
	}
	m := &fakeModel{replies: []string{`{"action":"finish"}`}}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithMemory(memory.NewRetriever(st, nil))
	if _, err := p.Decide(ctx, core.NewTask("周末短途出行", "这周末短途出行去哪好", core.TrustUser), nil); err != nil {
		t.Fatalf("决策失败: %v", err)
	}
	var body strings.Builder
	for _, msg := range m.lastReq.Messages[1:] {
		body.WriteString(msg.Content + "\n")
	}
	text := body.String()
	if !strings.Contains(text, "【周末短途出行·参考做法】") || !strings.Contains(text, "可以借鉴，不必照做") {
		t.Fatalf("助手总结的流程应标为参考做法:\n%s", text)
	}
	if !strings.Contains(text, "助手自己推断的") {
		t.Fatalf("推断出来的事实应标明未经确认:\n%s", text)
	}
}
