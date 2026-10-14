package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/llm"
)

// prefix 把一次请求的消息拼成一整段，用来比较两次请求的开头。
func prefix(req llm.Request) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(string(m.Role) + "\x00" + m.Content + "\x01")
	}
	return sb.String()
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func ctxStep(seq int, tool, out string, trust core.TrustLevel) *core.Step {
	s := core.NewStep("t", seq, core.StepTool, json.RawMessage(`{"q":"x"}`))
	s.ToolName, s.Status, s.Output, s.OutputTrust = tool, core.StepDone, json.RawMessage(out), trust
	return s
}

func TestTaskContext_同一任务连续几步发出的内容开头一致(t *testing.T) {
	m := &fakeModel{}
	p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{})
	task := core.NewTask("北京四日游", "安排一个北京四日游", core.TrustUser)
	ctx := context.Background()

	var history []*core.Step
	var reqs []llm.Request
	for i := 1; i <= 20; i++ {
		m.replies = []string{`{"action":"tool","tool":"demo.weather","input":{"city":"北京"}}`}
		if _, err := p.Decide(ctx, task, history); err != nil {
			t.Fatalf("第 %d 步决策失败: %v", i, err)
		}
		reqs = append(reqs, m.lastReq)
		// 外部内容（工具返回）每一步都有，围栏标记也就每一步都在。
		history = append(history, ctxStep(i, "demo.weather", fmt.Sprintf(`{"text":"第 %d 次的结果"}`, i), core.TrustExternal))
		task.Usage.Tokens += 1000
	}
	// 相邻两步：前一步发出去的内容，除了末尾的当前状态，原样是后一步的开头。
	// 只有压缩分界挪动的那一步例外（每 compactEvery 步一次）。
	breaks := 0
	for i := 1; i < len(reqs); i++ {
		prev, next := prefix(reqs[i-1]), prefix(reqs[i])
		// 去掉前一步的最后一条（当前状态）再比。
		lastMsg := reqs[i-1].Messages[len(reqs[i-1].Messages)-1]
		head := strings.TrimSuffix(prev, string(lastMsg.Role)+"\x00"+lastMsg.Content+"\x01")
		if !strings.HasPrefix(next, head) {
			breaks++
			if commonPrefix(prev, next) < len(prefix(llm.Request{Messages: reqs[i].Messages[:1]})) {
				t.Fatalf("第 %d 步连系统提示都和上一步不一样", i+1)
			}
		}
	}
	if breaks > len(reqs)/compactEvery+1 {
		t.Fatalf("开头不一致的次数太多：%d 次（预期只在压缩分界挪动时发生）", breaks)
	}
	// 系统提示里没有当前时间和花费；它们在最后一条。
	sys := reqs[len(reqs)-1].Messages[0].Content
	if strings.Contains(sys, "当前时间") || strings.Contains(sys, "已用") {
		t.Fatal("系统提示里不应有每一步都变的东西")
	}
	if last := reqs[len(reqs)-1].Messages; !strings.Contains(last[len(last)-1].Content, "已用: 19000 tokens") {
		t.Fatalf("末尾应是当前状态: %s", last[len(last)-1].Content)
	}
	// 不同的任务用不同的围栏标记。
	other := core.NewTask("x", "y", core.TrustUser)
	if p.contexts.taskNonce(task.ID) == p.contexts.taskNonce(other.ID) || p.contexts.taskNonce(task.ID) != p.contexts.taskNonce(task.ID) {
		t.Fatal("围栏标记应每个任务固定、不同任务不同")
	}
}

// 复现一次北京行程任务：先查了一批地点的坐标，后面几步联网搜索的长结果把上下文占满，
// 坐标那几步被裁掉，模型看不到查过，就拿同样的参数一遍遍重查。
func TestTaskContext_较早查到的工具结果不会因预算被整段丢掉(t *testing.T) {
	places := []string{"北京新世界酒店", "天安门广场", "故宫午门", "故宫神武门", "天坛公园", "颐和园", "北京南站"}
	build := func(before, after int) string {
		t.Helper()
		m := &fakeModel{replies: []string{`{"action":"tool","tool":"demo.weather","input":{"city":"北京"}}`}}
		// 预算给得紧一些，让裁剪一定发生，不依赖默认预算恰好多大。
		p := NewPlanner(m, catalogWith(core.SideEffectReadOnly), PlannerOptions{MaxChars: 12000})
		task := core.NewTask("北京四日游", "安排一个北京四日游，含打车距离和时长", core.TrustUser)
		var history []*core.Step
		add := func(tool, input, output string) {
			s := core.NewStep(task.ID, len(history)+1, core.StepTool, json.RawMessage(input))
			s.ToolName, s.Status, s.Output, s.OutputTrust = tool, core.StepDone, json.RawMessage(output), core.TrustExternal
			history = append(history, s)
		}
		long := strings.Repeat("北京七月下旬的门票、车次与价格说明。", 200)
		search := func(n int) {
			for i := 0; i < n; i++ {
				add(ToolWebSearch, fmt.Sprintf(`{"query":"第 %d 次搜索"}`, i), `{"answer":"`+long+`"}`)
			}
		}
		search(before)
		for i, name := range places {
			// 地图服务的返回：先是一串候选，坐标在每一项里。
			out := fmt.Sprintf(`[{"address":"东城区","display_name":"%s","location":{"lng":116.4%d,"lat":39.9%d}}%s]`,
				name, i, i, strings.Repeat(`,{"address":"东城区某街道某号","display_name":"附近地点"}`, 30))
			add("didi.maps_textsearch", fmt.Sprintf(`{"city":"北京","keywords":"%s"}`, name), out)
		}
		search(after)
		if _, err := p.Decide(context.Background(), task, history); err != nil {
			t.Fatalf("决策失败: %v", err)
		}
		return prefix(m.lastReq)
	}

	// 坐标那几步还在最近的窗口里，但被后面的长结果挤出了预算。
	// 坐标那几步已经压进了较早步骤的摘要。
	for name, sent := range map[string]string{"窗口内被挤出预算": build(0, 6), "已压进摘要": build(2, 20)} {
		for i, place := range places {
			if !strings.Contains(sent, place) || !strings.Contains(sent, fmt.Sprintf(`"lng":116.4%d`, i)) {
				t.Errorf("%s：发给模型的上下文里看不到 %s 的查询和坐标", name, place)
			}
		}
	}
}

func TestTaskContext_背景在任务第一步取下之后不再变(t *testing.T) {
	conv := &fakeConversation{}
	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithConversation(conv)
	task := core.NewTask("换成周日", "换成周日", core.TrustUser)
	task.Tags = []string{ConversationTag}
	before := core.NewTask("订高铁", "订周五去苏州的高铁", core.TrustUser)
	before.Tags, before.CreatedAt, before.State, before.Reason = []string{ConversationTag}, task.CreatedAt.Add(-1e9), core.StateRunning, ""
	conv.tasks = []*core.Task{before, task}
	first, err := p.background(context.Background(), task)
	if err != nil || len(first) != 1 || !strings.Contains(first[0].Content, "还在办") {
		t.Fatalf("第一步应取下此前的对话: %v %+v", err, first)
	}
	// 之后那件事办完了：这个任务的背景不跟着变，免得每一步开头都不一样。
	before.State, before.Reason = core.StateDone, "订好了"
	again, _ := p.background(context.Background(), task)
	if again[0].Content != first[0].Content {
		t.Fatal("同一个任务的背景应冻结")
	}
}
