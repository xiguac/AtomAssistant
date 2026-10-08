package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/store"
)

// Store 是事件与订阅的持久化接口。
type Store interface {
	// SaveEvent 记录一个事件。事件无论有没有人订阅都要留痕，
	// 它是"为什么这个任务在那一刻醒了"的唯一证据。
	SaveEvent(ctx context.Context, e *Event) error
	// AddSubscription 写入一条订阅。
	AddSubscription(ctx context.Context, s *Subscription) error
	// ActiveSubscriptions 返回仍然有效（已激活且未过期）的订阅。
	ActiveSubscriptions(ctx context.Context, now time.Time) ([]*Subscription, error)
	// Deactivate 让一条订阅失效。
	Deactivate(ctx context.Context, id string) error
	// DeactivateForTask 让一个任务的全部订阅失效。
	DeactivateForTask(ctx context.Context, taskID string) error

	// SaveForTask 记录一个事件，并把它放进指定任务的收件箱。
	SaveForTask(ctx context.Context, e *Event, taskID string) error
	// InboxOf 返回任务收件箱里尚未送达的事件，按发生时间升序。
	InboxOf(ctx context.Context, taskID string) ([]*Event, error)
	// ClearInbox 把这些事件标记为已送达。
	ClearInbox(ctx context.Context, eventIDs []string) error
}

// TaskWaker 是事件总线唤醒任务所需的能力，由 store.TaskStore 提供。
type TaskWaker interface {
	WakeWithStep(ctx context.Context, taskID, reason string, s *core.Step) error
}

// Bus 是事件总线：接收事件，匹配订阅，唤醒命中的任务。
//
// 它不常驻内存里的订阅表——订阅在数据库里，进程重启不丢。
// 自部署单实例的订阅量是几十到几百条，每个事件全量匹配一遍足够快。
type Bus struct {
	store Store
	tasks TaskWaker
	now   func() time.Time
}

// NewBus 构造事件总线。
func NewBus(s Store, tasks TaskWaker) *Bus {
	return &Bus{store: s, tasks: tasks, now: func() time.Time { return time.Now().UTC() }}
}

// Subscribe 为一个即将进入等待的任务登记订阅。
//
// 订阅必须带兜底过期时间（w.At）：等不到事件的任务到点会被定时唤醒，
// 订阅也在同一时刻失效，不会留下永远匹配不完的僵尸订阅。
func (b *Bus) Subscribe(ctx context.Context, taskID string, w *core.WakeCondition) error {
	if w == nil || w.Kind != core.WakeEvent {
		return fmt.Errorf("%w: 唤醒条件不是事件等待", ErrInvalidSubscription)
	}
	if w.At == nil {
		return fmt.Errorf("%w: 事件等待必须带兜底超时", ErrInvalidSubscription)
	}
	sub := &Subscription{
		ID:        core.NewID(),
		TaskID:    taskID,
		Expr:      w.Expr,
		Once:      true,
		ExpiresAt: w.At,
		Active:    true,
		CreatedAt: b.now(),
	}
	if err := sub.Validate(); err != nil {
		return err
	}
	// 一个任务同一时刻只等一件事。上一次等待留下的订阅必须先作废，
	// 否则任务会被一个它早已不关心的条件叫醒。
	if err := b.store.DeactivateForTask(ctx, taskID); err != nil {
		return err
	}
	return b.store.AddSubscription(ctx, sub)
}

// delivery 是事件送达任务时写进步骤日志的内容。
type delivery struct {
	EventID    string          `json:"event_id"`
	Topic      string          `json:"topic"`
	Source     string          `json:"source,omitempty"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// Deliver 把一个事件直接送给指定任务，不经过订阅匹配。
//
// 用于"用户对这个任务说了句话"：任务在等待就立刻唤醒；
// 任务此刻正在跑或在排队，消息进收件箱，它下一次推进时取走。
// 两种情况消息都不会丢——用户不需要挑任务正好在等的时候才说话。
// 返回值报告任务是否被立刻唤醒。
func (b *Bus) Deliver(ctx context.Context, taskID string, e *Event) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	out, err := deliveryOf(e)
	if err != nil {
		return false, err
	}
	step := core.NewStep(taskID, 1, core.StepWait, nil)
	step.SideEffect = core.SideEffectNone
	step.Complete(out, e.Trust)

	err = b.tasks.WakeWithStep(ctx, taskID, "收到新消息", step)
	switch {
	case err == nil:
		// 任务醒了，它之前等的东西不再作数。
		if derr := b.store.DeactivateForTask(ctx, taskID); derr != nil {
			return true, derr
		}
		return true, b.store.SaveEvent(ctx, e)
	case errors.Is(err, store.ErrStateConflict):
		return false, b.store.SaveForTask(ctx, e, taskID)
	default:
		return false, err
	}
}

// Pending 实现 kernel.Inbox：返回任务收件箱里尚未送达的消息。
func (b *Bus) Pending(ctx context.Context, taskID string) ([]kernel.InboxMessage, error) {
	events, err := b.store.InboxOf(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]kernel.InboxMessage, 0, len(events))
	for _, e := range events {
		content, err := deliveryOf(e)
		if err != nil {
			return nil, err
		}
		out = append(out, kernel.InboxMessage{ID: e.ID, Content: content, Trust: e.Trust})
	}
	return out, nil
}

// MarkDelivered 实现 kernel.Inbox。
func (b *Bus) MarkDelivered(ctx context.Context, ids []string) error {
	return b.store.ClearInbox(ctx, ids)
}

func deliveryOf(e *Event) (json.RawMessage, error) {
	out, err := json.Marshal(delivery{
		EventID: e.ID, Topic: e.Topic, Source: e.Source,
		OccurredAt: e.OccurredAt, Payload: e.Payload,
	})
	if err != nil {
		return nil, fmt.Errorf("event: 序列化事件失败: %w", err)
	}
	return out, nil
}

// Publish 记录一个事件并唤醒所有命中的等待任务，返回被唤醒的任务数。
//
// 事件内容作为一条已完成的步骤写进任务日志，信任等级沿用事件自身的：
// 一封邮件能把任务叫醒，但它的正文进了上下文依然是不可信材料。
//
// 单条订阅出错不影响其他订阅，错误汇总后一并返回。
func (b *Bus) Publish(ctx context.Context, e *Event) (int, error) {
	if err := e.Validate(); err != nil {
		return 0, err
	}
	if err := b.store.SaveEvent(ctx, e); err != nil {
		return 0, err
	}
	now := b.now()
	subs, err := b.store.ActiveSubscriptions(ctx, now)
	if err != nil {
		return 0, err
	}

	out, err := deliveryOf(e)
	if err != nil {
		return 0, err
	}

	woken := 0
	var errs []error
	for _, sub := range subs {
		ok, err := sub.Matches(e, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("event: 订阅 %s 求值失败: %w", sub.ID, err))
			continue
		}
		if !ok {
			continue
		}

		step := core.NewStep(sub.TaskID, 1, core.StepWait, nil)
		step.SideEffect = core.SideEffectNone
		step.Complete(out, e.Trust)
		err = b.tasks.WakeWithStep(ctx, sub.TaskID, "事件命中: "+e.Topic, step)
		switch {
		case err == nil:
			woken++
		case errors.Is(err, store.ErrStateConflict):
			// 任务此刻不在等待（刚被定时唤醒，或还没来得及进入等待）。
			// 订阅保持有效，由它自己的过期时间收尾。
			continue
		case errors.Is(err, store.ErrNotFound):
			// 任务已经不存在，订阅没有意义了，落到下面作废。
		default:
			errs = append(errs, fmt.Errorf("event: 唤醒任务 %s 失败: %w", sub.TaskID, err))
			continue
		}
		if sub.Once || err != nil {
			if derr := b.store.Deactivate(ctx, sub.ID); derr != nil {
				errs = append(errs, derr)
			}
		}
	}
	return woken, errors.Join(errs...)
}
