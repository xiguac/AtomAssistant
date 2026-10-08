// Command atomd 是原子助手的常驻服务进程。
//
// 没有配置 ATOM_CONFIG 时跑在 demo 模式：内核照常运转，
// 但 Planner 与 Actuator 是假实现，不会产生任何真实副作用。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xiguac/atomhelper/internal/agent"
	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/demo"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/schedule"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/vault"
	"github.com/xiguac/atomhelper/internal/webhook"
)

func main() {
	migrateOnly := flag.Bool("migrate-only", false, "只执行数据库迁移然后退出")
	checkOnly := flag.Bool("check", false, "校验配置与连接后退出，不启动调度器")
	flag.Parse()

	if err := run(*migrateOnly, *checkOnly); err != nil {
		fmt.Fprintf(os.Stderr, "atomd 启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run(migrateOnly, checkOnly bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	// 配置错误要在连数据库之前就暴露出来。
	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		return err
	}

	initCtx, cancelInit := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelInit()

	st, err := pg.Open(initCtx, cfg.DSN)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(initCtx); err != nil {
		return err
	}
	log.Info("数据库迁移完成")
	if migrateOnly {
		return nil
	}

	parts, err := build(initCtx, cfg, file, st, log)
	if err != nil {
		return err
	}
	defer parts.close()

	if checkOnly {
		log.Info("配置与连接校验通过")
		return nil
	}

	// 事件与任务同库：订阅和收件箱都在数据库里，进程重启不丢。
	bus := event.NewBus(st.Events(), st)
	engine := kernel.NewEngine(st, parts.planner, parts.actuator).
		WithSubscriber(bus).
		WithInbox(bus)
	if parts.approver != nil {
		engine = engine.WithApprover(parts.approver)
	}
	if parts.epilogue != nil {
		engine = engine.WithEpilogue(parts.epilogue)
	}
	sched := kernel.NewScheduler(st, engine, kernel.SchedulerConfig{
		Owner:            cfg.Owner,
		Workers:          cfg.Workers,
		PollInterval:     cfg.PollInterval,
		LeaseDuration:    cfg.LeaseDuration,
		MaxStepsPerClaim: cfg.MaxStepsPerClaim,
	}, log).WithCron(schedule.NewRunner(st.Schedules(), st, core.DefaultBudget()))

	// 收到信号后取消 ctx，调度器会把在跑的任务放回队列再退出，
	// 下次启动能立刻接着跑，不必等租约过期。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.WebhookAddr != "" {
		stopWebhook, err := serveWebhook(ctx, cfg, bus, log)
		if err != nil {
			return err
		}
		defer stopWebhook()
	}

	return sched.Run(ctx)
}

// serveWebhook 启动 Webhook 入口，返回一个用于关停它的函数。
//
// 先同步监听再异步服务：端口被占、地址写错这类问题要在启动时就报出来，
// 而不是进程看起来起来了、入口其实没开。
func serveWebhook(ctx context.Context, cfg *config.Config, bus *event.Bus, log *slog.Logger) (func(), error) {
	handler, err := webhook.Handler(bus, cfg.WebhookToken, log)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.WebhookAddr)
	if err != nil {
		return nil, fmt.Errorf("Webhook 监听 %s 失败: %w", cfg.WebhookAddr, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("Webhook 入口异常退出", "err", err)
		}
	}()
	log.Info("Webhook 入口已开启", "地址", ln.Addr().String())
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("关闭 Webhook 入口失败", "err", err)
		}
	}, nil
}

// parts 是按配置装出来的可插拔部件。approver 与 epilogue 可以为 nil。
type parts struct {
	planner  kernel.Planner
	actuator kernel.Actuator
	approver kernel.Approver
	epilogue kernel.Epilogue
	close    func()
}

// build 按配置装出各个部件。
func build(ctx context.Context, cfg *config.Config, file *config.File, st *pg.Store, log *slog.Logger) (*parts, error) {
	if file == nil {
		log.Warn("未设置 ATOM_CONFIG，运行在 demo 模式，不会产生任何真实副作用")
		return &parts{planner: demo.Planner{}, actuator: demo.Actuator{}, close: func() {}}, nil
	}

	router, err := file.BuildRouter()
	if err != nil {
		return nil, err
	}
	registry, err := file.BuildRegistry()
	if err != nil {
		return nil, err
	}
	guard := policy.NewGuard(policy.NewClassifier(), registry)

	// 保险库：明文只在网关调用工具的那一刻出现，模型与日志只见句柄。
	safe, err := vault.Open(st.Vault(), cfg.VaultKey)
	if err != nil {
		return nil, err
	}
	if cfg.VaultKey == "" {
		log.Warn("未设置 ATOM_VAULT_KEY，保险库处于锁定状态，引用保险库条目的工具调用会被拒绝")
	}

	gw, err := mcpgw.New(mcpgw.Options{
		Secrets:  safe,
		Guard:    guard,
		Registry: registry,
		Audit:    agent.NewStoreAudit(st),
	})
	if err != nil {
		return nil, err
	}

	servers, err := file.MCPServers()
	if err != nil {
		return nil, err
	}
	for _, spec := range servers {
		if err := gw.Connect(ctx, spec); err != nil {
			// 一个 MCP server 连不上不应该拖垮整个进程：其余工具照常可用，
			// 但必须让用户知道哪些能力现在不可用。
			log.Error("MCP server 连接失败，相关工具不可用", "server", spec.Name, "err", err)
			continue
		}
		log.Info("已连接 MCP server", "server", spec.Name)
	}
	log.Info("工具目录就绪", "工具数", len(gw.Catalog()))

	for purpose, route := range router.Routes() {
		log.Info("模型路由", "用途", purpose, "provider", route.Provider, "模型", route.Model)
	}

	approver := agent.NewApprover(
		file.BuildApprovalPolicy(), policy.NewClassifier(), agent.NewMemoryUsageTracker())

	// 记忆与任务同库，共用连接池。
	memStore := st.Memories()
	if err := memory.Seed(ctx, memStore, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("写入内置流程失败: %w", err)
	}
	mem := memory.NewRetriever(memStore, nil)
	log.Info("记忆系统就绪")

	return &parts{
		planner: agent.NewPlanner(router, gw, agent.PlannerOptions{
			ReplyStyle:  file.Reply.Style,
			ReplyCustom: file.Reply.Custom,
		}).WithMemory(mem).WithVault(safe).WithSkills(st.Skills()),
		actuator: agent.NewActuator(gw, router).WithMemory(mem),
		approver: approver,
		// 任务完成后提炼记忆与流程，经写入管线落库。
		epilogue: agent.NewEpilogue(router, memory.NewPipeline(memStore, nil)),
		close:    func() { _ = gw.Close() },
	}, nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
