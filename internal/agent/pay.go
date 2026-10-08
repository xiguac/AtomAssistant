package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/pay"
)

// Payer 是助手付款所需的能力，由 pay.Alipay 提供。
//
// 这里只有"发起一笔付款"和"查一笔付款"。钱包的开通、绑定、关闭不在其中：
// 那是授予付款能力本身，只能由用户亲手操作。
type Payer interface {
	Submit(ctx context.Context, sessionID, paymentLink string, intent pay.Intent) (string, error)
	Query(ctx context.Context, orderNo string) (string, error)
}

const (
	// ToolPaySubmit 发起一笔支付宝付款。
	ToolPaySubmit = "pay.alipay_submit"
	// ToolPayQuery 查询一笔付款的状态。
	ToolPayQuery = "pay.alipay_query"
)

var paySubmitTool = mcpgw.ToolInfo{
	Name:    ToolPaySubmit,
	Server:  "builtin",
	RawName: "alipay_submit",
	Description: `用支付宝为一笔订单发起付款。只有当别的工具返回了支付宝收银台链接或订单串、` +
		`且这笔钱确实是用户要你花的，才能调用。` +
		`调用成功不等于付款成功：返回的是一个待确认的订单，里面有"订单号"和"支付方式"链接。` +
		`你必须用 ask 把"支付方式"下的链接**一字不改**地发给用户，请他在支付宝里完成授权；` +
		`用户说付好了之后，再用 ` + ToolPayQuery + ` 查询结果。` +
		`参数里的服务内容、金额、收款方必须如实取自订单，不要写用户的其他个人信息。`,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"payment_link":{"type":"string","description":"支付宝收银台链接或订单串，原样照抄"},` +
		`"service":{"type":"string","description":"买的是什么"},` +
		`"amount_cents":{"type":"integer","description":"金额，单位分"},` +
		`"payee":{"type":"string","description":"付给谁"}},` +
		`"required":["payment_link","service","amount_cents","payee"]}`),
	// 钱付出去就收不回来。这个等级决定了它默认每一笔都要用户确认。
	SideEffect: core.SideEffectIrreversible,
}

var payQueryTool = mcpgw.ToolInfo{
	Name:    ToolPayQuery,
	Server:  "builtin",
	RawName: "alipay_query",
	Description: `查询一笔支付宝付款的状态。参数 order_no 取自 ` + ToolPaySubmit +
		` 返回内容里"订单号"或"查询单号"后面的值，原样照抄。用户表示已经付好之后再查。`,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"order_no":{"type":"string"}},"required":["order_no"]}`),
	SideEffect: core.SideEffectReadOnly,
}

// payTools 是接入付款能力后才出现的内置工具。
func payTools(enabled bool) []mcpgw.ToolInfo {
	if !enabled {
		return nil
	}
	return []mcpgw.ToolInfo{paySubmitTool, payQueryTool}
}

type paySubmitInput struct {
	PaymentLink string `json:"payment_link"`
	Service     string `json:"service"`
	AmountCents int64  `json:"amount_cents"`
	Payee       string `json:"payee"`
}

type payQueryInput struct {
	OrderNo string `json:"order_no"`
}

// runPay 执行付款类内置工具。
func (a *Actuator) runPay(ctx context.Context, t *core.Task, s *core.Step) (json.RawMessage, error) {
	if a.payer == nil {
		return nil, fmt.Errorf("agent: 未启用付款能力")
	}
	var text string
	switch s.ToolName {
	case ToolPaySubmit:
		var in paySubmitInput
		if err := json.Unmarshal(s.Input, &in); err != nil {
			return nil, fmt.Errorf("agent: 解析付款参数失败: %w", err)
		}
		// 会话标识用任务 ID：同一个任务里的发起与查询在支付宝那边能关联起来。
		out, err := a.payer.Submit(ctx, t.ID, in.PaymentLink, pay.Intent{
			Service: in.Service, AmountCents: in.AmountCents, Payee: in.Payee,
		})
		if err != nil {
			return nil, err
		}
		text = out
	case ToolPayQuery:
		var in payQueryInput
		if err := json.Unmarshal(s.Input, &in); err != nil {
			return nil, fmt.Errorf("agent: 解析查询参数失败: %w", err)
		}
		out, err := a.payer.Query(ctx, in.OrderNo)
		if err != nil {
			return nil, err
		}
		text = out
	default:
		return nil, fmt.Errorf("agent: 未知的付款工具 %q", s.ToolName)
	}
	return json.Marshal(map[string]string{"text": text})
}

// classifiable 返回一个动作里需要做隐私分级的那部分参数。
//
// 付款工具的支付链接和订单号是例外：它们天生就是几十位的字符串，
// 形态上和密钥、银行卡号分不开，而它们唯一的用途就是交给支付宝。
// 照常分级的话每一笔付款都会被判成"含绝密数据"而直接拒绝。
// 所以这两个字段不分级；服务内容和收款方是自由文本，仍然分级——
// 模型若把证件号之类的东西塞进去，照样会被拦下。
func classifiable(d kernel.Decision) json.RawMessage {
	if !isPayTool(d.ToolName) {
		return d.Input
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(d.Input, &in); err != nil {
		// 解析不了就原样交回去，由分级器按最保守的方式处理。
		return d.Input
	}
	delete(in, "payment_link")
	delete(in, "order_no")
	out, err := json.Marshal(in)
	if err != nil {
		return d.Input
	}
	return out
}

// isPayTool 报告一个工具名是否属于付款类内置工具。
func isPayTool(name string) bool {
	return name == ToolPaySubmit || name == ToolPayQuery
}
