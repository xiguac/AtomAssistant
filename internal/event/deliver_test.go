package event_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/event"
	"github.com/xiguac/atomassistant/internal/event/memstore"
	"github.com/xiguac/atomassistant/internal/store/memory"
)

func userMessage(taskID, text string) *event.Event {
	payload, _ := json.Marshal(map[string]string{"task_id": taskID, "text": text})
	return event.New(event.TopicUserMessage, payload, core.ChannelByUser.MessageTrust(), "atomctl")
}

func TestDeliver_任务在等待时立刻唤醒(t *testing.T) {
	ctx := context.Background()
	tasks, events := memory.New(), memstore.New()
	bus := event.NewBus(events, tasks)
	// 任务在等一封邮件，但用户先来补了一句话。
	task := waitingTask(t, tasks, bus, `topic == "mail.received"`)

	woken, err := bus.Deliver(ctx, task.ID, userMessage(task.ID, "身份证复印件已发你"))
	if err != nil || !woken {
		t.Fatalf("等待中的任务应被立刻唤醒: %v, %v", woken, err)
	}
	if state(t, tasks, task.ID) != core.StateReady {
		t.Fatal("任务应回到 ready")
	}
	steps, _ := tasks.ListSteps(ctx, task.ID)
	if len(steps) != 1 || steps[0].OutputTrust != core.TrustUser ||
		!strings.Contains(string(steps[0].Output), "身份证复印件") {
		t.Fatalf("消息应以 user 等级写进日志: %+v", steps)
	}
	// 任务醒了，它之前等的那封邮件不再作数。
	if subs, _ := events.ActiveSubscriptions(ctx, time.Now().UTC()); len(subs) != 0 {
		t.Fatalf("旧订阅应作废，实际 %d 条", len(subs))
	}
}

func TestDeliver_任务不在等待时进收件箱下次推进取走(t *testing.T) {
	ctx := context.Background()
	tasks, events := memory.New(), memstore.New()
	bus := event.NewBus(events, tasks)

	task := core.NewTask("收集材料", "分几天收齐签证材料", core.TrustUser)
	if err := tasks.CreateTask(ctx, task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if err := tasks.Transition(ctx, task.ID, core.StateNew, core.StateReady, "入队"); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	// 任务在排队（没有在等），用户连着补了两份材料。
	for _, text := range []string{"这是身份证复印件", "这是户口本"} {
		woken, err := bus.Deliver(ctx, task.ID, userMessage(task.ID, text))
		if err != nil || woken {
			t.Fatalf("不在等待的任务不应被唤醒，消息应进收件箱: %v, %v", woken, err)
		}
	}
	if steps, _ := tasks.ListSteps(ctx, task.ID); len(steps) != 0 {
		t.Fatal("不得直接往一个不在等待的任务日志里写")
	}

	msgs, err := bus.Pending(ctx, task.ID)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("收件箱应有 2 条: %v, %d", err, len(msgs))
	}
	if msgs[0].Trust != core.TrustUser || !strings.Contains(string(msgs[0].Content), "身份证复印件") {
		t.Fatalf("消息内容或信任等级不对: %+v", msgs[0])
	}

	ids := []string{msgs[0].ID, msgs[1].ID}
	if err := bus.MarkDelivered(ctx, ids); err != nil {
		t.Fatalf("标记送达失败: %v", err)
	}
	if msgs, _ := bus.Pending(ctx, task.ID); len(msgs) != 0 {
		t.Fatalf("已送达的消息不应再出现，实际 %d 条", len(msgs))
	}
	// 别的任务的收件箱互不相干。
	if msgs, _ := bus.Pending(ctx, "别的任务"); len(msgs) != 0 {
		t.Fatal("收件箱不应串到别的任务")
	}
}
