// Package event 是事件总线。
//
// 它解决的是"任务怎么被唤醒"：长程任务 90% 的时间在等，
// 等待中的任务不占运行时资源，只是一行数据库记录加一个唤醒条件。
// 任务用**订阅表达式**声明它关心什么，而不是每分钟把所有数据捞一遍。
//
// 见 docs/架构方案.md §3.5。
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiguac/atomassistant/internal/core"
)

// 内置事件主题。用点号分层，支持前缀通配。
const (
	// TopicScheduleTick 定时器触发。
	TopicScheduleTick = "schedule.tick"
	// TopicConnectorChanged 同步层发现数据变更。
	TopicConnectorChanged = "connector.changed"
	// TopicUserMessage 用户在 App 上说话。
	TopicUserMessage = "user.message"
	// TopicToolCallback 外部服务回调。
	TopicToolCallback = "tool.callback"
	// TopicTaskFinished 任务结束。
	TopicTaskFinished = "task.finished"
	// TopicMemoryConflict 记忆出现未裁决的分歧。
	TopicMemoryConflict = "memory.conflict"
	// TopicApprovalDecided 用户完成了一次审批。
	TopicApprovalDecided = "approval.decided"
)

// Event 是一次外部或内部的状态变化。
type Event struct {
	ID    string
	Topic string
	// Payload 是事件内容，用于表达式求值与注入任务上下文。
	Payload json.RawMessage

	// Trust 是事件内容的信任等级。
	//
	// 这一条很关键：来自邮件、Webhook、截图的事件是 external，
	// 它们能**唤醒**任务，但唤醒后的内容依然不可信，不能当指令。
	Trust core.TrustLevel
	// Source 描述事件来源，会展示给模型，如 "webhook:日历"。
	Source string

	OccurredAt time.Time
	CreatedAt  time.Time
}

// ErrInvalidEvent 表示事件字段不合法。
var ErrInvalidEvent = errors.New("event: 事件字段不合法")

// Validate 校验事件。
func (e *Event) Validate() error {
	if e.ID == "" {
		return fmt.Errorf("%w: ID 为空", ErrInvalidEvent)
	}
	if err := ValidateTopic(e.Topic); err != nil {
		return err
	}
	if len(e.Payload) > 0 && !json.Valid(e.Payload) {
		return fmt.Errorf("%w: Payload 不是合法 JSON", ErrInvalidEvent)
	}
	// json.Valid 会放过编码不对的字节，数据库不会。在这里拦下，
	// 调用方得到的是一句说得清的拒绝，而不是写库时一个莫名其妙的失败。
	if !utf8.Valid(e.Payload) {
		return fmt.Errorf("%w: Payload 不是 UTF-8 编码", ErrInvalidEvent)
	}
	return nil
}

// ValidateTopic 校验主题格式：小写字母、数字、下划线，用点号分层。
func ValidateTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("%w: 主题为空", ErrInvalidEvent)
	}
	for _, seg := range strings.Split(topic, ".") {
		if seg == "" {
			return fmt.Errorf("%w: 主题 %q 含空层级", ErrInvalidEvent, topic)
		}
		for _, r := range seg {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
			if !ok {
				return fmt.Errorf("%w: 主题 %q 含非法字符 %q", ErrInvalidEvent, topic, r)
			}
		}
	}
	return nil
}

// New 构造一个事件。
func New(topic string, payload json.RawMessage, trust core.TrustLevel, source string) *Event {
	now := time.Now().UTC()
	return &Event{
		ID:         core.NewID(),
		Topic:      topic,
		Payload:    payload,
		Trust:      trust,
		Source:     source,
		OccurredAt: now,
		CreatedAt:  now,
	}
}

// Subscription 是一个任务声明的唤醒条件。
type Subscription struct {
	ID     string
	TaskID string

	// TopicPrefix 是主题前缀过滤，用于建索引先做粗筛。
	// 空表示不限主题（不推荐，会让每个事件都去跑表达式）。
	TopicPrefix string
	// Expr 是完整的订阅表达式，如：
	//   topic == "calendar.event.created" AND payload.attendees CONTAINS "客户张总"
	Expr string

	// Once 为真表示命中一次后自动失效。绝大多数任务唤醒都是一次性的。
	Once bool

	// ExpiresAt 是订阅的兜底过期时间。没有它的话，一个被取消的任务
	// 会永远留着订阅，日积月累把匹配开销拖垮。
	ExpiresAt *time.Time

	Active    bool
	CreatedAt time.Time

	// compiled 是解析后的表达式，惰性编译并缓存。
	compiled Node
}

// ErrInvalidSubscription 表示订阅不合法。
var ErrInvalidSubscription = errors.New("event: 订阅不合法")

// Validate 校验订阅，并预编译表达式——语法错误必须在订阅时就暴露，
// 而不是等到事件到来时才发现任务永远醒不过来。
func (s *Subscription) Validate() error {
	if s.ID == "" || s.TaskID == "" {
		return fmt.Errorf("%w: ID 与 TaskID 不能为空", ErrInvalidSubscription)
	}
	if strings.TrimSpace(s.Expr) == "" {
		return fmt.Errorf("%w: 表达式为空", ErrInvalidSubscription)
	}
	if _, err := s.Compile(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSubscription, err)
	}
	return nil
}

// Compile 解析表达式并缓存结果。
func (s *Subscription) Compile() (Node, error) {
	if s.compiled != nil {
		return s.compiled, nil
	}
	n, err := Parse(s.Expr)
	if err != nil {
		return nil, err
	}
	s.compiled = n
	return n, nil
}

// Matches 判断一个事件是否命中这条订阅。
//
// 求值不会 panic 也不会产生副作用：表达式可能由模型生成，
// 必须当成不可信输入处理。求值失败一律返回 false 并附带原因。
func (s *Subscription) Matches(e *Event, now time.Time) (bool, error) {
	if !s.Active {
		return false, nil
	}
	if s.ExpiresAt != nil && !now.Before(*s.ExpiresAt) {
		return false, nil
	}
	if s.TopicPrefix != "" && !TopicMatches(e.Topic, s.TopicPrefix) {
		return false, nil
	}
	n, err := s.Compile()
	if err != nil {
		return false, err
	}
	env, err := newEnv(e)
	if err != nil {
		return false, err
	}
	return n.Eval(env)
}

// TopicMatches 做主题前缀匹配。
//
// "calendar" 匹配 "calendar.event.created"，但不匹配 "calendarx.foo"——
// 必须按层级边界匹配，否则前缀过滤会误伤。
func TopicMatches(topic, prefix string) bool {
	if prefix == "" || topic == prefix {
		return true
	}
	return strings.HasPrefix(topic, prefix+".")
}
