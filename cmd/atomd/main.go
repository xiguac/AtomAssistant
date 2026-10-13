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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xiguac/atomhelper/internal/admin"
	"github.com/xiguac/atomhelper/internal/agent"
	"github.com/xiguac/atomhelper/internal/channel"
	"github.com/xiguac/atomhelper/internal/config"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/demo"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/kernel"
	"github.com/xiguac/atomhelper/internal/market"
	"github.com/xiguac/atomhelper/internal/mcpgw"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/policy"
	"github.com/xiguac/atomhelper/internal/schedule"
	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/skillscan"
	"github.com/xiguac/atomhelper/internal/store/pg"
	"github.com/xiguac/atomhelper/internal/style"
	"github.com/xiguac/atomhelper/internal/team"
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

	initCtx, cancelInit := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelInit()

	// 数据密钥：第一次启动时生成，之后每次从密钥文件里取。它和这台机器的硬件绑在一起。
	box, created, err := seal.LoadKey(cfg.KeyFile, true)
	if err != nil {
		return fmt.Errorf("打开数据密钥 %s 失败: %w", cfg.KeyFile, err)
	}
	if created {
		log.Warn("已生成数据密钥。它与这台机器绑定，换机器请用 atomctl export / import 迁移；"+
			"建议现在用 atomctl key backup 留一份备份，以防硬件标识意外变化", "密钥文件", cfg.KeyFile)
	}

	st, err := pg.Open(initCtx, cfg.DSN, box)
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

	// 收到信号后取消 ctx，调度器会把在跑的任务放回队列再退出，
	// 下次启动能立刻接着跑，不必等租约过期。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 事件与任务同库：订阅和收件箱都在数据库里，进程重启不丢。
	bus := event.NewBus(st.Events(), st)

	// 管理后台先于助手本身启动：配置没填完、密钥还没存的时候，
	// 用户得有个地方把它们填上——而这个地方就是后台。
	var problem atomic.Value
	problem.Store("")
	adminOn := cfg.AdminAddr != "" && !checkOnly
	if adminOn {
		if err := startAdmin(ctx, cfg, st, box, bus, stop, func() string { return problem.Load().(string) }, log); err != nil {
			return err
		}
	}

	parts, err := assemble(initCtx, cfg, st, box, log)
	if err != nil {
		if !adminOn {
			return err
		}
		// 助手起不来，但后台要留着：否则用户连修的地方都没有。
		log.Error("助手未能启动，目前只有管理后台在运行", "err", err)
		problem.Store(err.Error())
		<-ctx.Done()
		return nil
	}
	defer parts.close()

	if checkOnly {
		log.Info("配置与连接校验通过")
		return nil
	}

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

	// 消息渠道：主人在聊天软件里发来的话变成任务，任务的进展推回去。
	if len(parts.channels) > 0 {
		hub, err := channel.NewHub(parts.channels,
			&channel.Kernel{Store: st, Bus: bus, Budget: core.DefaultBudget()}, st.Channels(), log)
		if err != nil {
			return err
		}
		if parts.team {
			hub = hub.WithFeedback(channel.TeamFeedback{Store: st.Team()})
		}
		if parts.outbox != nil {
			parts.outbox.hub.Store(hub)
		}
		go func() {
			if err := hub.Run(ctx); err != nil {
				log.Error("消息渠道异常退出", "err", err)
			}
		}()
	}

	if cfg.WebhookAddr != "" {
		stopWebhook, err := serveWebhook(ctx, cfg, bus, log)
		if err != nil {
			return err
		}
		defer stopWebhook()
	}

	return sched.Run(ctx)
}

// assemble 读取配置文件并装出各个部件。
func assemble(ctx context.Context, cfg *config.Config, st *pg.Store, box *seal.Box, log *slog.Logger) (*parts, error) {
	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		return nil, err
	}
	return build(ctx, cfg, file, st, box, log)
}

// startAdmin 启动网页管理后台。
func startAdmin(ctx context.Context, cfg *config.Config, st *pg.Store, box *seal.Box, bus *event.Bus,
	restart func(), status func() string, log *slog.Logger) error {
	tls := cfg.AdminTLSCert != "" && cfg.AdminTLSKey != ""
	if err := admin.CheckAddr(cfg.AdminAddr, tls); err != nil {
		return err
	}
	safe, err := vault.Open(st.Vault(), cfg.VaultKey)
	if err != nil {
		return err
	}
	srv, err := admin.New(admin.Options{
		Store: st, Vault: safe, Box: box,
		Scanner:      skillscan.Scanner{Command: cfg.SkillScanner, ExtraArgs: cfg.SkillScannerArgs},
		Tasks:        &channel.Kernel{Store: st, Bus: bus, Budget: core.DefaultBudget()},
		ConfigPath:   cfg.ConfigPath,
		PasswordHash: cfg.AdminPasswordHash,
		TLS:          tls,
		// 让进程正常退出，由 systemd 拉起新的。
		Restart: restart,
		Status:  status,
		Log:     log,
	})
	if err != nil {
		return err
	}
	// 先同步确认端口能用再转到后台去服务：端口被占这类问题要在启动时就报出来。
	ln, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		return fmt.Errorf("管理后台监听 %s 失败: %w", cfg.AdminAddr, err)
	}
	_ = ln.Close()
	go func() {
		if err := srv.Serve(ctx, cfg.AdminAddr, cfg.AdminTLSCert, cfg.AdminTLSKey); err != nil {
			log.Error("管理后台异常退出", "err", err)
		}
	}()
	return nil
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
	channels []channel.Binding
	// outbox 非空时助手可以往聊天窗口里发文件，渠道中枢建好后要填进去。
	outbox *chatOutbox
	// team 为真表示配置了协作模型。
	team  bool
	close func()
}

// build 按配置装出各个部件。
func build(ctx context.Context, cfg *config.Config, file *config.File, st *pg.Store, key *seal.Box, log *slog.Logger) (*parts, error) {
	if file == nil {
		log.Warn("未设置 ATOM_CONFIG，运行在 demo 模式，不会产生任何真实副作用")
		return &parts{planner: demo.Planner{}, actuator: demo.Actuator{}, close: func() {}}, nil
	}

	// 保险库：明文只在网关调用工具的那一刻出现，模型与日志只见句柄。
	safe, err := vault.Open(st.Vault(), cfg.VaultKey)
	if err != nil {
		return nil, err
	}
	if cfg.VaultKey == "" {
		log.Warn("未设置 ATOM_VAULT_KEY，保险库处于锁定状态，引用保险库条目的工具调用会被拒绝")
	}

	// 模型密钥可以放在保险库里，所以路由排在保险库之后。
	router, err := file.BuildRouter(ctx, safe)
	if err != nil {
		return nil, err
	}
	registry, err := file.BuildRegistry()
	if err != nil {
		return nil, err
	}
	guard := policy.NewGuard(policy.NewClassifier(), registry)

	gw, err := mcpgw.New(mcpgw.Options{
		Secrets:  safe,
		Guard:    guard,
		Registry: registry,
		Audit:    agent.NewStoreAudit(st),
	})
	if err != nil {
		return nil, err
	}

	servers, err := file.MCPServers(safe)
	if err != nil {
		return nil, err
	}
	configured := make(map[string]bool, len(servers))
	for _, spec := range servers {
		configured[spec.Name] = true
		if err := gw.Connect(ctx, spec); err != nil {
			// 一个 MCP server 连不上不应该拖垮整个进程：其余工具照常可用，
			// 但必须让用户知道哪些能力现在不可用。
			log.Error("MCP server 连接失败，相关工具不可用", "server", spec.Name, "err", err)
			continue
		}
		log.Info("已连接 MCP server", "server", spec.Name)
	}
	// 助手以前自己装上的 server。配置文件里有同名的就让位：用户亲手批准的优先。
	installed, err := st.MCPInstalls().ListServers(ctx)
	if err != nil {
		return nil, err
	}
	for _, srv := range installed {
		if configured[srv.Name] {
			log.Warn("助手安装的 MCP server 与配置文件里的重名，已跳过", "server", srv.Name)
			continue
		}
		if err := gw.Connect(ctx, unreviewedSpec(srv)); err != nil {
			log.Error("MCP server 连接失败，相关工具不可用", "server", srv.Name, "err", err)
			continue
		}
		log.Info("已连接助手安装的 MCP server", "server", srv.Name, "地址", srv.URL)
	}
	log.Info("工具目录就绪", "工具数", len(gw.Catalog()))

	for purpose, route := range router.Routes() {
		log.Info("模型路由", "用途", purpose, "provider", route.Provider, "模型", route.Model)
	}

	approval := file.BuildApprovalPolicy()
	// 用户亲口交代的日程直接记，不再逐条确认；读过外部内容的任务不适用。
	if file.Connectors.Calendar != nil {
		agent.CalendarDefaults(approval)
	}
	// 写进本机工作区的文件同理：覆盖时旧的会留底，不必逐个确认。
	workspace, err := file.Workspace(key)
	if err != nil {
		return nil, err
	}
	if workspace != nil {
		agent.FileDefaults(approval)
	}
	// 缺少能力时助手自己找、扫描通过就装。扫描器不可用时装不上，但照样能找。
	agent.CapabilityDefaults(approval)
	installer := &market.Installer{
		Client:  &market.Client{},
		Scanner: skillscan.Scanner{Command: cfg.SkillScanner, ExtraArgs: cfg.SkillScannerArgs},
		Skills:  st.Skills(),
		Servers: st.MCPInstalls(),
		Probe: func(ctx context.Context, srv *market.Server) ([]market.ToolBrief, error) {
			tools, err := gw.Probe(ctx, unreviewedSpec(srv))
			if err != nil {
				return nil, err
			}
			out := make([]market.ToolBrief, 0, len(tools))
			for _, t := range tools {
				out = append(out, market.ToolBrief{Name: t.Name, ReadOnly: t.ReadOnly, Params: t.Params})
			}
			return out, nil
		},
		Connect: func(ctx context.Context, srv *market.Server) ([]string, error) {
			if configured[srv.Name] {
				return nil, fmt.Errorf("配置文件里已经有名为 %s 的 MCP server", srv.Name)
			}
			if err := gw.Connect(ctx, unreviewedSpec(srv)); err != nil {
				return nil, err
			}
			return gw.ToolsOf(srv.Name), nil
		},
	}
	approver := agent.NewApprover(approval, policy.NewClassifier(), agent.NewMemoryUsageTracker()).
		WithHistory(st).WithInstallRisks(installer)

	// 记忆与任务同库，共用连接池。
	memStore := st.Memories()
	if err := memory.Seed(ctx, memStore, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("写入内置流程失败: %w", err)
	}
	mem := memory.NewRetriever(memStore, nil)
	log.Info("记忆系统就绪")

	planner := agent.NewPlanner(router, gw, agent.PlannerOptions{
		ReplyStyle:  file.Reply.Style,
		ReplyCustom: file.Reply.Custom,
	}).WithMemory(mem).WithVault(safe).WithSkills(st.Skills()).WithCapabilities(st.Skills()).
		WithPrompts(agent.SettingPrompts{Store: st})
	actuator := agent.NewActuator(gw, router).WithMemory(mem).WithCapabilities(installer, st.Skills())

	// 联网搜索：用模型服务自带的搜索能力，配置过的服务里有能搜的就自动接上。
	if router.CanSearch() {
		planner = planner.WithWebSearch(true)
		actuator = actuator.WithWebSearch(router)
		log.Info("已启用联网搜索")
	} else {
		log.Warn("配置的模型服务里没有认得的自带联网搜索的，助手不能上网查东西")
	}

	// 付款能力默认关闭。开了之后每一笔仍然要过审批，并且要用户本人在支付宝里授权。
	if file.Payment.Alipay.Enabled {
		planner = planner.WithPay(true)
		actuator = actuator.WithPayer(file.Alipay())
		log.Info("已启用支付宝付款能力")
		// 付费资源要另外列出允许的域名：这是让助手能主动向外发请求的唯一入口。
		if domains := file.Payment.Alipay.ResourceDomains; len(domains) > 0 {
			planner = planner.WithPaidResources(true)
			actuator = actuator.WithPaidResources(file.PaidResources())
			log.Info("已允许付费资源", "域名", domains)
		}
	}

	epilogue := agent.NewEpilogue(router, memory.NewPipeline(memStore, nil))
	// 表达风格：从用户亲口说的话里学，替他写东西、对他说话时用。
	if file.Reply.LearnVoice() {
		voice := style.NewLearner(st.Style(), router)
		planner = planner.WithVoice(voice)
		epilogue = epilogue.WithVoice(voice)
	}

	// 协作模型：主模型做预处理和审核，事情交给任务模型去办，交给谁看用户以往的评价。
	if workers := file.Workers(); len(workers) > 0 {
		board := team.NewBoard(st.Team(), workers)
		planner = planner.WithTaskModels(board, router)
		actuator = actuator.WithTeam(board, router)
		epilogue = epilogue.WithTeam(st.Team())
		log.Info("已启用多模型协作", "协作模型数", len(workers))
	}

	// 内建连接器：密码每次连接时从保险库现取，配置里只有句柄。
	actuator = actuator.WithAudit(agent.NewStoreAudit(st))
	cal, err := file.Calendar(safe)
	if err != nil {
		return nil, err
	}
	if cal != nil {
		planner = planner.WithCalendar(true)
		actuator = actuator.WithCalendar(cal)
		log.Info("已接入日历", "服务器", cal.Endpoint, "账号", cal.Username)
	}
	if workspace != nil {
		planner = planner.WithFiles(true)
		actuator = actuator.WithFiles(workspace)
		log.Info("已启用文件处理", "工作区", workspace.Root, "加密存放", file.Files.Encrypt)
	}
	feed, err := file.Holidays()
	if err != nil {
		return nil, err
	}
	if feed != nil {
		planner = planner.WithHolidays(true)
		actuator = actuator.WithHolidays(feed)
		log.Info("已接入节假日数据", "订阅", feed.URL)
	}
	box, err := file.Mail(safe)
	if err != nil {
		return nil, err
	}
	if box != nil {
		planner = planner.WithMail(true, box.SMTPAddr != "")
		actuator = actuator.WithMail(box)
		log.Info("已接入邮件", "收信", box.IMAPAddr, "发信", box.SMTPAddr, "账号", box.Username)
	}

	channels, err := file.BuildChannels(safe, log)
	if err != nil {
		return nil, err
	}
	for _, c := range channels {
		if len(c.Owners) == 0 {
			log.Warn("消息渠道还没有主人，目前不会处理任何消息。给机器人发一句话，它会把你的标识回给你",
				"渠道", c.Adapter.Name())
		} else {
			log.Info("已接入消息渠道", "渠道", c.Adapter.Name(), "主人数", len(c.Owners))
		}
	}

	// 接了消息渠道又启用了文件处理：助手可以把文件直接发到聊天窗口里。
	// 渠道中枢要等这些部件装好之后才建，所以先留一个位置，建好了再填进去。
	var outbox *chatOutbox
	if len(channels) > 0 && workspace != nil {
		outbox = &chatOutbox{}
		planner = planner.WithChatFiles(true)
		actuator = actuator.WithChatFiles(outbox)
	}

	return &parts{
		team:     len(file.Workers()) > 0,
		channels: channels,
		outbox:   outbox,
		planner:  planner,
		actuator: actuator,
		approver: approver,
		// 任务完成后提炼记忆与流程，经写入管线落库。
		epilogue: epilogue,
		close:    func() { _ = gw.Close() },
	}, nil
}

// chatOutbox 把助手要发的文件交给渠道中枢。
type chatOutbox struct {
	hub atomic.Pointer[channel.Hub]
}

func (c *chatOutbox) SendFile(ctx context.Context, tags []string, name string, data []byte) error {
	hub := c.hub.Load()
	if hub == nil {
		return fmt.Errorf("消息渠道还没有就绪")
	}
	return hub.SendFile(ctx, tags, name, data)
}

// unreviewedSpec 是助手自己装上的远程 server 的连接参数。
func unreviewedSpec(srv *market.Server) mcpgw.ServerSpec {
	return mcpgw.ServerSpec{Name: srv.Name, Transport: mcpgw.TransportHTTP, URL: srv.URL, Unreviewed: true}
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
