package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/memory"
)

type fakeProfileMemories []*memory.Memory

func (f fakeProfileMemories) List(context.Context, memory.Query) ([]*memory.Memory, error) {
	return f, nil
}

type fakeProfileSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func (f *fakeProfileSettings) Setting(_ context.Context, k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[k], nil
}

func (f *fakeProfileSettings) PutSetting(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[k] = v
	return nil
}

func profileMemory(value string, src memory.SourceKind) *memory.Memory {
	m := memory.New(memory.KindFact, "user", "兴趣和偏好", value,
		memory.Source{Kind: src, Trust: core.TrustUser}, time.Now())
	m.Temporality = memory.TemporalDurable
	return m
}

func TestGenerateProfile_把全部记忆交给模型并区分推断(t *testing.T) {
	var got llm.Request
	model := func(_ context.Context, req llm.Request) (llm.Response, error) {
		got = req
		return llm.Response{Content: "  你喜欢长跑。  "}, nil
	}
	mems := fakeProfileMemories{
		profileMemory("用户喜欢长跑。", memory.SourceUser),
		profileMemory("用户可能喜欢咖啡。", memory.SourceTask),
	}
	text, err := GenerateProfile(context.Background(), model, mems)
	if err != nil {
		t.Fatal(err)
	}
	if text != "你喜欢长跑。" {
		t.Errorf("摘要应去掉首尾空白: %q", text)
	}
	var all strings.Builder
	for _, m := range got.Messages {
		all.WriteString(m.Content)
	}
	for _, want := range []string{"用户喜欢长跑。", "用户可能喜欢咖啡。", "unconfirmed"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("交给模型的内容里缺 %q", want)
		}
	}
	if got.JSONMode {
		t.Error("摘要是一段文字，不该用 JSON 模式")
	}
}

func TestGenerateProfile_没有记忆不调用模型(t *testing.T) {
	model := func(context.Context, llm.Request) (llm.Response, error) {
		t.Fatal("没有记忆时不该调用模型")
		return llm.Response{}, nil
	}
	if _, err := GenerateProfile(context.Background(), model, fakeProfileMemories{}); !errors.Is(err, ErrNoMemories) {
		t.Fatalf("应返回 ErrNoMemories，实际 %v", err)
	}
}

func TestGenerateProfile_模型没写出内容算失败(t *testing.T) {
	model := func(context.Context, llm.Request) (llm.Response, error) { return llm.Response{Content: " "}, nil }
	if _, err := GenerateProfile(context.Background(), model, fakeProfileMemories{profileMemory("x", memory.SourceUser)}); err == nil {
		t.Fatal("空摘要不该当成成功，否则会把旧摘要覆盖成空的")
	}
}

func TestProfile_每天只生成一次(t *testing.T) {
	calls := 0
	model := func(context.Context, llm.Request) (llm.Response, error) {
		calls++
		return llm.Response{Content: "摘要"}, nil
	}
	set := &fakeProfileSettings{}
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.Local)
	p := NewProfile(model, fakeProfileMemories{profileMemory("a", memory.SourceUser)}, set, nil)
	p.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		if err := p.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("同一天只该生成一次，实际 %d 次", calls)
	}
	if v, _ := set.Setting(context.Background(), SettingProfile); v != "摘要" {
		t.Errorf("摘要应存进设置: %q", v)
	}

	p.now = func() time.Time { return now.Add(24 * time.Hour) }
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("第二天应再生成一次，实际共 %d 次", calls)
	}
}

func TestGenerateProfile_写长了就压缩一次(t *testing.T) {
	long := strings.Repeat("你喜欢长跑。", 400) // 2400 字
	calls := 0
	model := func(_ context.Context, req llm.Request) (llm.Response, error) {
		calls++
		if calls == 1 {
			return llm.Response{Content: long}, nil
		}
		return llm.Response{Content: "你喜欢长跑。"}, nil
	}
	text, err := GenerateProfile(context.Background(), model, fakeProfileMemories{profileMemory("用户喜欢长跑。", memory.SourceUser)})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || text != "你喜欢长跑。" {
		t.Errorf("写长了应压缩一次: 调用 %d 次，得到 %q", calls, text)
	}
}

func TestGenerateProfile_压缩失败保留原文(t *testing.T) {
	long := strings.Repeat("你喜欢长跑。", 400)
	calls := 0
	model := func(_ context.Context, _ llm.Request) (llm.Response, error) {
		calls++
		if calls == 1 {
			return llm.Response{Content: long}, nil
		}
		return llm.Response{}, errors.New("模型出错")
	}
	text, err := GenerateProfile(context.Background(), model, fakeProfileMemories{profileMemory("用户喜欢长跑。", memory.SourceUser)})
	if err != nil || text != long {
		t.Errorf("压缩失败不该让摘要失败: %v %d 字", err, len([]rune(text)))
	}
}

func TestGenerateProfile_不长就不压缩(t *testing.T) {
	calls := 0
	model := func(_ context.Context, _ llm.Request) (llm.Response, error) {
		calls++
		return llm.Response{Content: strings.Repeat("好", 1000)}, nil
	}
	if _, err := GenerateProfile(context.Background(), model, fakeProfileMemories{profileMemory("用户喜欢长跑。", memory.SourceUser)}); err != nil || calls != 1 {
		t.Errorf("1000 字不该再压缩: 调用 %d 次 %v", calls, err)
	}
}

func TestProfile_已有的摘要太长当天也重新生成一次(t *testing.T) {
	set := &fakeProfileSettings{}
	now := time.Now()
	_ = set.PutSetting(context.Background(), SettingProfile, strings.Repeat("长", 3000))
	_ = set.PutSetting(context.Background(), SettingProfileAt, now.UTC().Format(time.RFC3339))
	calls := 0
	model := func(_ context.Context, _ llm.Request) (llm.Response, error) {
		calls++
		return llm.Response{Content: strings.Repeat("长", 3000)}, nil // 怎么写都长
	}
	p := NewProfile(model, fakeProfileMemories{profileMemory("用户喜欢长跑。", memory.SourceUser)}, set, nil)
	for i := 0; i < 3; i++ {
		if err := p.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// 第一次为太长重新生成（含一次压缩），之后即使仍然太长也不再重来。
	if calls != 2 {
		t.Errorf("应只重新生成一次（生成 + 压缩共 2 次调用），实际 %d 次", calls)
	}
}
