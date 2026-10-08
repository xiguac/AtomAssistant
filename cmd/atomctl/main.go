// Command atomctl 是运维 CLI：创建任务、查看任务与步骤日志、审批与裁决。
//
// 它直接连数据库，不经过 atomd——这是自研步骤日志换来的好处之一：
// 任务状态就是普通的表，排障不需要另一套控制面。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/store/pg"
)

const usage = `用法:
  atomctl create <标题> [目标]   创建一个任务并入队
  atomctl show <任务ID>          查看任务详情
  atomctl steps <任务ID>         查看步骤日志
  atomctl approve <任务ID>       批准任务当前等待确认的动作
  atomctl reject <任务ID> [理由] 拒绝该动作，交回规划器另想办法
  atomctl resolve <任务ID> done|failed
                                 裁决一个执行中断、状态不明的不可逆操作：
                                 done 表示已生效，failed 表示未生效
  atomctl reply <任务ID> <内容>  回答任务的提问，或随时给它补充材料
  atomctl emit <主题> [JSON]     发布一个事件，唤醒订阅命中的等待任务

  atomctl memory list            查看全部记忆
  atomctl memory pending         查看助手提炼出来、等你确认的记忆与流程
  atomctl memory confirm <ID>    确认一条，让它生效
  atomctl memory dismiss <ID>    否决一条

  atomctl export <板块> <文件>   导出数据用于迁移。板块: memory / tasks / events / all
  atomctl import <文件>          导入。已存在的数据不会被覆盖

环境变量: ATOM_DSN（必填）
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := pg.Open(ctx, cfg.DSN)
	if err != nil {
		return err
	}
	defer st.Close()

	switch args[0] {
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("create 需要一个标题")
		}
		goal := ""
		if len(args) > 2 {
			goal = args[2]
		}
		return create(ctx, st, args[1], goal)
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("show 需要一个任务 ID")
		}
		return show(ctx, st, args[1])
	case "steps":
		if len(args) < 2 {
			return fmt.Errorf("steps 需要一个任务 ID")
		}
		return steps(ctx, st, args[1])
	case "approve":
		if len(args) < 2 {
			return fmt.Errorf("approve 需要一个任务 ID")
		}
		return kernel.Approve(ctx, st, args[1])
	case "reject":
		if len(args) < 2 {
			return fmt.Errorf("reject 需要一个任务 ID")
		}
		reason := ""
		if len(args) > 2 {
			reason = args[2]
		}
		return kernel.Reject(ctx, st, args[1], reason)
	case "resolve":
		if len(args) < 3 || (args[2] != "done" && args[2] != "failed") {
			return fmt.Errorf("resolve 需要任务 ID 和 done 或 failed")
		}
		return kernel.Resolve(ctx, st, args[1], args[2] == "done")
	case "reply":
		if len(args) < 3 {
			return fmt.Errorf("reply 需要任务 ID 和回复内容")
		}
		return reply(ctx, st, args[1], args[2])
	case "memory":
		return memoryCmd(ctx, st, args[1:])
	case "export":
		if len(args) < 3 {
			return fmt.Errorf("export 需要板块（memory / tasks / events / all）和文件路径")
		}
		return exportCmd(ctx, st, args[1], args[2])
	case "import":
		if len(args) < 2 {
			return fmt.Errorf("import 需要一个导出文件的路径")
		}
		return importCmd(ctx, st, args[1])
	case "emit":
		if len(args) < 2 {
			return fmt.Errorf("emit 需要一个事件主题")
		}
		var payload json.RawMessage
		if len(args) > 2 {
			payload = json.RawMessage(args[2])
		}
		return emit(ctx, st, args[1], payload)
	default:
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

func create(ctx context.Context, st *pg.Store, title, goal string) error {
	// 从 CLI 创建视为用户直接输入，信任等级为 user。
	// 后续从截图/邮件创建的任务必须用 core.TrustExternal。
	t := core.NewTask(title, goal, core.TrustUser)
	t.Budget = core.Budget{MaxTokens: 200_000, MaxCostCents: 500, MaxWallClock: 24 * time.Hour}

	if err := st.CreateTask(ctx, t); err != nil {
		return err
	}
	if err := st.Transition(ctx, t.ID, core.StateNew, core.StateReady, "用户创建"); err != nil {
		return err
	}
	fmt.Println(t.ID)
	return nil
}

// reply 对一个任务说话：回答它的提问，或者随时补充材料。
//
// 任务不必正好在等——长程任务是断断续续推进的，今天补一份、过几天再补一份都可以。
//
// CLI 是用户亲手使用的渠道，所以消息的信任等级走 ChannelByUser。
// 将来接入其他消息渠道时，等级必须由渠道的来历决定，见 core.ChannelOrigin。
func reply(ctx context.Context, st *pg.Store, taskID, text string) error {
	payload, err := json.Marshal(map[string]string{"task_id": taskID, "text": text})
	if err != nil {
		return fmt.Errorf("序列化回复失败: %w", err)
	}
	t, err := st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.State.Terminal() {
		return fmt.Errorf("任务 %s 已经结束（%s），不能再补充内容", taskID, t.State)
	}
	e := event.New(event.TopicUserMessage, payload, core.ChannelByUser.MessageTrust(), "atomctl")
	woken, err := event.NewBus(st.Events(), st).Deliver(ctx, taskID, e)
	if err != nil {
		return err
	}
	if woken {
		fmt.Println("已送达，任务继续执行")
	} else {
		fmt.Println("已收下，任务下一次推进时会看到")
	}
	return nil
}

func emit(ctx context.Context, st *pg.Store, topic string, payload json.RawMessage) error {
	// 与 create 一致：从 CLI 发出的事件视为用户直接输入。
	// 连接器与 Webhook 产生的事件必须按各自来源标注更低的信任等级。
	e := event.New(topic, payload, core.TrustUser, "atomctl")
	woken, err := event.NewBus(st.Events(), st).Publish(ctx, e)
	fmt.Printf("事件 %s 已发布，唤醒 %d 个任务\n", e.ID, woken)
	return err
}

func show(ctx context.Context, st *pg.Store, id string) error {
	t, err := st.GetTask(ctx, id)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID\t%s\n", t.ID)
	fmt.Fprintf(w, "标题\t%s\n", t.Title)
	fmt.Fprintf(w, "目标\t%s\n", t.Goal)
	fmt.Fprintf(w, "状态\t%s\n", t.State)
	fmt.Fprintf(w, "原因\t%s\n", t.Reason)
	fmt.Fprintf(w, "域/可见性\t%s / %s\n", t.Domain, t.Visibility)
	fmt.Fprintf(w, "来源信任\t%s\n", t.Origin)
	fmt.Fprintf(w, "用量\t%d tokens, %d 分, %s\n",
		t.Usage.Tokens, t.Usage.CostCents, t.Usage.WallClock.Round(time.Second))
	fmt.Fprintf(w, "预算\t%d tokens, %d 分, %s\n",
		t.Budget.MaxTokens, t.Budget.MaxCostCents, t.Budget.MaxWallClock)
	if t.Wake != nil {
		at := "-"
		if t.Wake.At != nil {
			at = t.Wake.At.Local().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "唤醒条件\t%s at=%s ref=%s\n", t.Wake.Kind, at, t.Wake.Ref)
	}
	if t.LeaseOwner != "" {
		fmt.Fprintf(w, "租约\t%s 到 %s\n", t.LeaseOwner, t.LeaseExpiresAt.Local().Format(time.RFC3339))
	}
	fmt.Fprintf(w, "创建/更新\t%s / %s\n",
		t.CreatedAt.Local().Format(time.RFC3339), t.UpdatedAt.Local().Format(time.RFC3339))
	return w.Flush()
}

func steps(ctx context.Context, st *pg.Store, id string) error {
	list, err := st.ListSteps(ctx, id)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("（暂无步骤）")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SEQ\t类型\t状态\t工具\t副作用\t信任\t花费\t输出")
	for _, s := range list {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%d分\t%s\n",
			s.Seq, s.Type, s.Status, dash(s.ToolName), s.SideEffect,
			s.OutputTrust, s.CostCents, summarize(s))
	}
	return w.Flush()
}

func summarize(s *core.Step) string {
	if s.Status == core.StepSkipped {
		return "未执行: " + s.Err
	}
	if s.Err != "" {
		return "失败: " + s.Err
	}
	if len(s.Output) == 0 {
		return "-"
	}
	var v any
	if err := json.Unmarshal(s.Output, &v); err != nil {
		return string(s.Output)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(s.Output)
	}
	max := 60
	if len(b) > max {
		// 退到字符边界再切，否则中文会被切出半个字符。
		for max > 0 && !utf8.RuneStart(b[max]) {
			max--
		}
		return string(b[:max]) + "…"
	}
	return string(b)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
