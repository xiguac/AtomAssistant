package skill

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSeedBuiltin_可重复写入且不覆盖用户装的同名Skill(t *testing.T) {
	ctx := context.Background()
	st := &memStore{items: map[string]*Skill{}}
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		if err := SeedBuiltin(ctx, st, now); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i+1, err)
		}
	}
	travel := st.items["builtin.travel-planning"]
	if travel == nil || travel.Source != SourceBuiltin || travel.Learned() || travel.Format != FormatManifest ||
		!strings.Contains(travel.Render(), "查人流") || !strings.Contains(travel.Raw, "id: builtin.travel-planning") {
		t.Fatalf("应写入内置的旅游规划流程: %+v", travel)
	}

	// 内置的内容旧了，换成新的。
	travel.Raw = "旧版本"
	if err := SeedBuiltin(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	if st.items["builtin.travel-planning"].Raw == "旧版本" {
		t.Fatal("内置流程更新了内容，应换成新的")
	}

	// 用户用同一个 ID 装了自己的，不动。
	st.items["builtin.dining"] = &Skill{ID: "builtin.dining", Name: "我的订餐流程", Goal: "g", Steps: []string{"s"}, Source: "后台安装"}
	if err := SeedBuiltin(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	if st.items["builtin.dining"].Name != "我的订餐流程" {
		t.Fatal("用户装的同 ID Skill 被出厂版本覆盖了")
	}
}

func TestCompose_生成原文并校验(t *testing.T) {
	if _, err := Compose(&Skill{ID: "Bad ID", Name: "n", Goal: "g", Steps: []string{"s"}}); err == nil {
		t.Fatal("不合法的 ID 应被拒绝")
	}
	s, err := Compose(&Skill{ID: "calendar.reschedule", Name: "日程改期", Goal: "改期", Steps: []string{"查当天日程"}})
	if err != nil || !strings.Contains(s.Raw, "calendar.reschedule") || s.Format != FormatManifest {
		t.Fatalf("应生成清单原文: %+v %v", s, err)
	}
}
