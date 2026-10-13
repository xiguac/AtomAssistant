package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
)

type fakeConversation struct{ tasks []*core.Task }

func (f *fakeConversation) TasksByTag(_ context.Context, tag string, _ bool, limit int) ([]*core.Task, error) {
	var out []*core.Task
	// 和真的存储一样：新的在前。
	for i := len(f.tasks) - 1; i >= 0 && len(out) < limit; i-- {
		for _, t := range f.tasks[i].Tags {
			if t == tag {
				out = append(out, f.tasks[i])
			}
		}
	}
	return out, nil
}

func TestConversation_新任务带上同一个会话里最近的对话(t *testing.T) {
	now := time.Now().UTC()
	turn := func(ago time.Duration, goal, reason string, state core.TaskState, tags ...string) *core.Task {
		task := core.NewTask(goal, goal, core.TrustUser)
		task.CreatedAt, task.Reason, task.State, task.Tags = now.Add(-ago), reason, state, tags
		return task
	}
	// 两个消息源的任务都带着会话标签：在 QQ 里说过的，到微信里接着说也接得上。
	old := turn(30*time.Hour, "很久以前的事", "早就办完了", core.StateDone, "chat:qq/1", ConversationTag)
	first := turn(20*time.Minute, "帮我看周五去苏州的高铁", "周五早上有 G7001，07:30 发车", core.StateDone, "chat:qq/1", ConversationTag)
	asking := turn(10*time.Minute, "订个酒店", "等待你的回复：住哪一带？", core.StateWaiting, "chat:weixin/2", ConversationTag)
	other := turn(5*time.Minute, "后台建的任务", "不属于会话", core.StateDone)
	cur := turn(0, "换成周日出发行不行", "", core.StateRunning, "chat:weixin/2", ConversationTag)
	later := turn(-time.Minute, "之后才说的话", "", core.StateReady, "chat:qq/1", ConversationTag)
	store := &fakeConversation{tasks: []*core.Task{old, first, asking, other, cur, later}}

	p := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithConversation(store)
	seg, ok, err := p.conversationSegment(context.Background(), cur)
	if err != nil || !ok {
		t.Fatalf("应带上最近的对话: %v %v", ok, err)
	}
	text := seg.Content
	if !strings.Contains(text, "帮我看周五去苏州的高铁") || !strings.Contains(text, "G7001") || !strings.Contains(text, "住哪一带") {
		t.Fatalf("应包含之前的问答（不分消息源）:\n%s", text)
	}
	if strings.Index(text, "苏州") > strings.Index(text, "订个酒店") {
		t.Fatalf("应按时间先后排列，最早的在前:\n%s", text)
	}
	for _, not := range []string{"很久以前的事", "后台建的任务", "之后才说的话", "换成周日出发行不行"} {
		if strings.Contains(text, not) {
			t.Fatalf("不应包含 %q：太早的、不属于会话的、之后的、这个任务自己都不算:\n%s", not, text)
		}
	}
	// 里面有助手以前的回复，回复可能转述过外部内容：整段不具备指令资格。
	if seg.Trust >= core.TrustUser {
		t.Fatalf("此前的对话不应按用户指令对待: %s", seg.Trust)
	}
	// 完整的上下文里有这一段。
	segs, err := p.contextSegments(context.Background(), cur, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range segs {
		found = found || s.Source == "此前的对话"
	}
	if !found {
		t.Fatal("上下文里应有此前的对话")
	}

	// 不属于会话的任务、会话里的第一句话，都没有这一段。
	if _, ok, _ := p.conversationSegment(context.Background(), other); ok {
		t.Fatal("不属于会话的任务不应带上对话")
	}
	lone := &fakeConversation{tasks: []*core.Task{cur}}
	if _, ok, _ := NewPlanner(&fakeModel{}, catalogWith(core.SideEffectReadOnly), PlannerOptions{}).WithConversation(lone).conversationSegment(context.Background(), cur); ok {
		t.Fatal("会话里的第一句话没有此前的对话")
	}
}
