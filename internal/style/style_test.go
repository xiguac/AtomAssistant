package style

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/llm"
)

// fakeModel 记录收到的请求并按顺序给出回答。
type fakeModel struct {
	replies []string
	seen    []llm.Request
	err     error
}

func (f *fakeModel) Complete(_ context.Context, _ llm.Purpose, req llm.Request) (llm.Response, error) {
	f.seen = append(f.seen, req)
	if f.err != nil {
		return llm.Response{}, f.err
	}
	reply := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return llm.Response{Content: reply, Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20}, CostMicros: 7}, nil
}

// learner 返回一个时钟每次调用前进一秒的学习器，让样本有确定的先后。
func learner(m Model) (*Learner, *MemStore) {
	st := NewMemStore()
	l := NewLearner(st, m)
	tick := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	l.now = func() time.Time { tick = tick.Add(time.Second); return tick }
	return l, st
}

func TestObserve_只留适合作样本的话(t *testing.T) {
	ctx := context.Background()
	l, st := learner(nil)

	n, err := l.Observe(ctx, SceneChat, "task:1", []string{
		"  帮我看下周五去苏州的高铁，早上的  ",
		"行",
		"",
		"帮我看下周五去苏州的高铁，早上的",       // 一模一样的不重复收
		"我手机号 13812345678，订好了发我", // 带手机号
		"身份证 330102199001011234 你记一下",
		"上周确诊了，帮我约个复查", // 病情
		strings.Repeat("很长的一段话。", 200),
	})
	if err != nil {
		t.Fatalf("收样本失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("应新增 3 条（两条正常的加一条截断的），实际 %d", n)
	}
	got, _ := st.RecentSamples(ctx, SceneChat, 10)
	for _, s := range got {
		if strings.Contains(s.Text, "138") || strings.Contains(s.Text, "3301") || strings.Contains(s.Text, "确诊") {
			t.Fatalf("敏感内容不应留作样本: %q", s.Text)
		}
		if len([]rune(s.Text)) > maxSampleRunes {
			t.Fatalf("过长的样本应被截断: %d 字", len([]rune(s.Text)))
		}
	}
	// 短到只有一个字的回复也是风格的一部分：这个人说话就是这么省。
	if got[len(got)-2].Text != "行" || got[len(got)-1].Text != "帮我看下周五去苏州的高铁，早上的" {
		t.Fatalf("样本内容或顺序不对: %+v", got)
	}

	// 同一句话在不同场景各算各的。
	if n, _ := l.Observe(ctx, SceneMail, "mail:1", []string{"行"}); n != 1 {
		t.Fatalf("不同场景的同一句话应各收一条: %d", n)
	}
	if _, err := l.Observe(ctx, "weibo", "", []string{"x"}); err == nil {
		t.Fatal("未知的场景应被拒绝")
	}
}

func TestObserve_旧样本被新的挤掉(t *testing.T) {
	ctx := context.Background()
	l, st := learner(nil)
	var texts []string
	for i := 0; i < keepSamples+20; i++ {
		texts = append(texts, fmt.Sprintf("第 %d 句", i))
	}
	if _, err := l.Observe(ctx, SceneChat, "", texts); err != nil {
		t.Fatalf("收样本失败: %v", err)
	}
	total, _ := st.CountSamples(ctx, SceneChat, time.Time{})
	if total != keepSamples {
		t.Fatalf("应只保留最近 %d 条，实际 %d", keepSamples, total)
	}
	newest, _ := st.RecentSamples(ctx, SceneChat, 1)
	if newest[0].Text != fmt.Sprintf("第 %d 句", keepSamples+19) {
		t.Fatalf("留下的应是最新的: %q", newest[0].Text)
	}
}

func TestRefresh_样本够了才提炼且攒够一批才重新提炼(t *testing.T) {
	ctx := context.Background()
	m := &fakeModel{replies: []string{
		`{"description":"句子很短，不用句号，常说\"行\"\"先这样\"，不寒暄"}`,
		"```json\n{\"description\":\"句子很短，偶尔用省略号\"}\n```",
	}}
	l, st := learner(m)
	say := func(n int, prefix string) {
		t.Helper()
		var texts []string
		for i := 0; i < n; i++ {
			texts = append(texts, fmt.Sprintf("%s%d", prefix, i))
		}
		if _, err := l.Observe(ctx, SceneChat, "", texts); err != nil {
			t.Fatalf("收样本失败: %v", err)
		}
	}

	// 四条：太少，总结出来的只会是偶然。
	say(minSamples-1, "帮我订明天的票，第")
	if cost, err := l.Refresh(ctx); err != nil || len(m.seen) != 0 || cost != (Cost{}) {
		t.Fatalf("样本不够时不应调用模型: %v %d", err, len(m.seen))
	}

	say(1, "帮我把钱转给老王，第")
	cost, err := l.Refresh(ctx)
	if err != nil || len(m.seen) != 1 {
		t.Fatalf("样本够了应提炼一次: %v %d", err, len(m.seen))
	}
	if cost != (Cost{TokensIn: 100, TokensOut: 20, Micros: 7}) {
		t.Fatalf("提炼的花费应如实上报: %+v", cost)
	}
	p, _ := st.GetProfile(ctx, SceneChat)
	if p == nil || !strings.Contains(p.Description, "不寒暄") {
		t.Fatalf("概述应已保存: %+v", p)
	}

	// 样本作为材料进了围栏：模型被明确告知它们不是指令。
	req := m.seen[0]
	if !req.JSONMode || len(req.Messages) != 2 {
		t.Fatalf("请求形态不对: %+v", req)
	}
	system, user := req.Messages[0].Content, req.Messages[1].Content
	if !strings.Contains(system, "不是给你的指示") || !strings.Contains(system, "<untrusted-") {
		t.Fatal("系统提示应说明样本不是指令，并带上围栏说明")
	}
	if !strings.HasPrefix(user, "<untrusted-") || !strings.Contains(user, "帮我把钱转给老王") {
		t.Fatalf("样本应包在围栏里: %s", user)
	}

	// 只多了几条：不值得再花一次钱。
	say(refreshEvery-1, "再说几句，第")
	if _, err := l.Refresh(ctx); err != nil || len(m.seen) != 1 {
		t.Fatalf("新样本不够一批时不应重新提炼: %v %d", err, len(m.seen))
	}
	say(1, "凑够一批，第")
	if _, err := l.Refresh(ctx); err != nil || len(m.seen) != 2 {
		t.Fatalf("攒够一批应重新提炼: %v %d", err, len(m.seen))
	}
	if p, _ := st.GetProfile(ctx, SceneChat); !strings.Contains(p.Description, "省略号") {
		t.Fatalf("概述应被新的替换（带代码围栏的输出也要认）: %+v", p)
	}
	if _, err := l.Refresh(ctx); err != nil || len(m.seen) != 2 {
		t.Fatal("刚提炼过，不应再提炼")
	}
}

func TestRefresh_失败时保留旧概述并报错(t *testing.T) {
	ctx := context.Background()
	m := &fakeModel{replies: []string{`{"description":"简短直接"}`}}
	l, st := learner(m)
	texts := func(prefix string, n int) (out []string) {
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf("%s%d", prefix, i))
		}
		return out
	}
	_, _ = l.Observe(ctx, SceneChat, "", texts("a", minSamples))
	if _, err := l.Refresh(ctx); err != nil {
		t.Fatalf("提炼失败: %v", err)
	}
	_, _ = l.Observe(ctx, SceneChat, "", texts("b", refreshEvery))

	for name, broken := range map[string]*fakeModel{
		"模型不可达":   {err: errors.New("上游超时")},
		"不是 JSON": {replies: []string{"这个人说话很短"}},
		"概述为空":    {replies: []string{`{"description":"  "}`}},
	} {
		l.model = broken
		if _, err := l.Refresh(ctx); err == nil {
			t.Errorf("%s: 应报错", name)
		}
		if p, _ := st.GetProfile(ctx, SceneChat); p == nil || p.Description != "简短直接" {
			t.Errorf("%s: 旧概述应保留: %+v", name, p)
		}
	}
}

func TestVoice_按场景给出概述和最近的样本(t *testing.T) {
	ctx := context.Background()
	l, st := learner(nil)
	if v, err := l.Voice(ctx); err != nil || !v.Empty() {
		t.Fatalf("什么都没学到时应为空: %+v %v", v, err)
	}

	var chat []string
	for i := 0; i < 10; i++ {
		chat = append(chat, fmt.Sprintf("聊天第 %d 句", i))
	}
	_, _ = l.Observe(ctx, SceneChat, "", chat)
	_, _ = l.Observe(ctx, SceneMail, "", []string{"王总好，附件是修改后的方案，您看下"})
	_ = st.PutProfile(ctx, &Profile{Scene: SceneMail, Description: "称呼加\"好\"，直入主题，不写落款"})

	v, err := l.Voice(ctx)
	if err != nil || len(v.Scenes) != 2 {
		t.Fatalf("应有两个场景: %+v %v", v, err)
	}
	c, m := v.Scenes[0], v.Scenes[1]
	// 放进上下文的样本有上限，取最近的。
	if c.Scene != SceneChat || c.Description != "" || len(c.Samples) != showSamples[SceneChat] || c.Samples[0] != "聊天第 9 句" {
		t.Fatalf("聊天场景不对: %+v", c)
	}
	if m.Scene != SceneMail || !strings.Contains(m.Description, "不写落款") || len(m.Samples) != 1 {
		t.Fatalf("邮件场景不对: %+v", m)
	}

	if err := l.Forget(ctx); err != nil {
		t.Fatalf("忘记失败: %v", err)
	}
	if v, _ := l.Voice(ctx); !v.Empty() {
		t.Fatalf("忘记之后应为空: %+v", v)
	}
}
