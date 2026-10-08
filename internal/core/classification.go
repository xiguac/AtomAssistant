package core

import "fmt"

// Domain 是生活/工作分域。域决定默认可见性，而不是反过来。
// 见 docs/协作与联邦.md §4.1。
type Domain string

const (
	// DomainPersonal 私人域：看牙、家庭安排、给家人带东西。永不进入任何共享通道。
	DomainPersonal Domain = "personal"
	// DomainWork 工作域：客户会议、项目节点、出差。可共享，给谁由圈子决定。
	DomainWork Domain = "work"
	// DomainMixed 混合域：用公司预算给客户送礼这类模糊情形。按私人处理，逐项放行。
	DomainMixed Domain = "mixed"
)

// Shareable 报告该域的内容是否**可能**被共享出实例。
// 注意这只是域这一层的判断，最终是否外发还要过可见性和出站过滤器。
func (d Domain) Shareable() bool { return d == DomainWork }

func (d Domain) Valid() bool {
	switch d {
	case DomainPersonal, DomainWork, DomainMixed:
		return true
	}
	return false
}

// Visibility 是数据模型的一等字段，不是事后加的过滤逻辑。
type Visibility string

const (
	VisibilityPrivate Visibility = "private"
	// VisibilityCircle 与 VisibilityUser 需要配合 VisibilityTarget 使用。
	VisibilityCircle Visibility = "circle"
	VisibilityUser   Visibility = "user"
)

func (v Visibility) Valid() bool {
	switch v {
	case VisibilityPrivate, VisibilityCircle, VisibilityUser:
		return true
	}
	return false
}

// TrustLevel 标记一段内容的来源可信度，是防提示注入的地基。
// 见 docs/安全模型.md §3。数值越大越可信，便于比较。
type TrustLevel int

const (
	// TrustExternal 邮件正文、网页、文档、**截图内容**。第三方撰写，永不可信。
	TrustExternal TrustLevel = iota
	// TrustToolOutput MCP 工具返回值。容易被误当成可信数据，实际同样可被注入。
	TrustToolOutput
	// TrustFriend 好友实例发来的数据。好友是独立信任域，不是自己人。
	TrustFriend
	// TrustConnector 用户授权的连接器同步来的结构化数据（日历条目、订单记录）。
	TrustConnector
	// TrustUser 用户在 App 里的直接输入。
	TrustUser
	// TrustSystem 我们自己的系统提示。
	TrustSystem
)

// CanInstruct 报告该等级的内容能否被当作指令执行。
func (t TrustLevel) CanInstruct() bool { return t >= TrustUser }

// CanTriggerIrreversible 报告该等级的内容能否触发不可逆操作。
// 注意：即使返回 true，仍要过审批策略。
func (t TrustLevel) CanTriggerIrreversible() bool { return t >= TrustUser }

// CanWriteMemoryDirectly 报告该等级的内容能否直接写入长期记忆。
// 返回 false 的来源只能作为记忆候选进入写入管线。
func (t TrustLevel) CanWriteMemoryDirectly() bool { return t >= TrustConnector }

func (t TrustLevel) String() string {
	switch t {
	case TrustExternal:
		return "external"
	case TrustToolOutput:
		return "tool_output"
	case TrustFriend:
		return "friend"
	case TrustConnector:
		return "connector"
	case TrustUser:
		return "user"
	case TrustSystem:
		return "system"
	}
	return fmt.Sprintf("unknown(%d)", int(t))
}

// PrivacyLevel 是字段级隐私分级，用于数据出口管控。见 docs/安全模型.md §4.1。
type PrivacyLevel int

const (
	PrivacyPublic    PrivacyLevel = iota // L0 用户显式声明公开
	PrivacyNormal                        // L1 工作日程、公开联系方式
	PrivacySensitive                     // L2 通讯录、私人日程内容、财务金额、家庭住址
	PrivacySecret                        // L3 密码、token、证件号、银行卡、健康信息
)

// NeverLeavesInstance 是写进代码的硬约束：L3 绝密数据永不传出实例。
// 这条不提供任何配置开关，见 docs/安全模型.md §10。
func (p PrivacyLevel) NeverLeavesInstance() bool { return p >= PrivacySecret }

func (p PrivacyLevel) String() string {
	switch p {
	case PrivacyPublic:
		return "L0"
	case PrivacyNormal:
		return "L1"
	case PrivacySensitive:
		return "L2"
	case PrivacySecret:
		return "L3"
	}
	return fmt.Sprintf("unknown(%d)", int(p))
}

// ToolTrustCeiling 是一个已安装工具的输出可信度上限，在安装/授权时确定。
//
// 数据可能来自内建协议适配器（IMAP/CalDAV）也可能来自 MCP 工具，
// 所以"通过什么协议拿到的"不构成信任依据。依据是**谁提供的、返回的是什么**：
//
//	内建适配器 / 官方签名且用户显式授权的查询工具 → TrustConnector
//	已批准的普通第三方工具                       → TrustToolOutput
//	未审核工具                                   → TrustExternal
type ToolTrustCeiling = TrustLevel

// EffectiveTrust 计算一段内容的实际信任等级。
//
// 关键规则：**自由文本永远按 TrustExternal 处理，无论来源多可信。**
// 一条日历事件的时间和 ID 是权威的，但它的标题和备注是**创建这条日程的人**写的——
// 陌生人发来的会议邀请，标题里完全可以写"忽略之前的指示"。
// 同理，权威订单接口返回的商品名也是商家填的。
//
// 所以信任是**字段级**的，不是来源级的。
func EffectiveTrust(source TrustLevel, isFreeText bool) TrustLevel {
	if isFreeText && source > TrustExternal {
		return TrustExternal
	}
	return source
}
