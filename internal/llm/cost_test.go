package llm

import (
	"context"
	"testing"
)

func TestRouter_按单价计算开销(t *testing.T) {
	p := &stubProvider{name: "p", resp: Response{
		Content: "ok", Usage: Usage{PromptTokens: 1500, CompletionTokens: 500},
	}}
	r := NewRouter()
	mustAdd(t, r, p)
	// 输入 2 元/百万 token，输出 8 元/百万 token。
	mustRoute(t, r, Route{
		Purpose: PurposePlan, Provider: "p", Model: "m",
		InputYuanPerMTok: 2, OutputYuanPerMTok: 8,
	})
	mustRoute(t, r, Route{Purpose: PurposeExtract, Provider: "p", Model: "free"})

	msg := []Message{{Role: RoleUser, Content: "hi"}}
	resp, err := r.Complete(context.Background(), PurposePlan, Request{Messages: msg})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	// 1500×2 + 500×8 = 7000 百万分之一元，即 0.7 分——用"分"记会被舍成 0。
	if resp.CostMicros != 7000 {
		t.Fatalf("开销应为 7000，实际 %d", resp.CostMicros)
	}

	// 没配单价的路由不计费。
	resp, err = r.Complete(context.Background(), PurposeExtract, Request{Messages: msg})
	if err != nil || resp.CostMicros != 0 {
		t.Fatalf("未配单价应为 0: %d, %v", resp.CostMicros, err)
	}
}
