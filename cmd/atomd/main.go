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

	"github.com/xiguac/atomassistant/internal/admin"
	"github.com/xiguac/atomassistant/internal/agent"
	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/demo"
	"github.com/xiguac/atomassistant/internal/event"
	"github.com/xiguac/atomassistant/internal/files"
	"github.com/xiguac/atomassistant/internal/kernel"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/market"
	"github.com/xiguac/atomassistant/internal/mcpgw"
	"github.com/xiguac/atomassistant/internal/media"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/oplog"
	"github.com/xiguac/atomassistant/internal/policy"
	"github.com/xiguac/atomassistant/internal/schedule"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/skill"
	"github.com/xiguac/atomassistant/internal/share"
	"github.com/xiguac/atomassistant/internal/skillscan"
	"github.com/xiguac/atomassistant/internal/store/pg"
	"github.com/xiguac/atomassistant/internal/style"
	"github.com/xiguac/atomassistant/internal/team"
	"github.com/xiguac/atomassistant/internal/vault"
	"github.com/xiguac/atomassistant/internal/webhook"
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
	log, logs := newLogger(cfg.LogLevel)

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
	// 操作日志：从这里开始写进数据库（之前攒下的一并写进去），后台的「日志」页按类别查看。
	logCtx, stopLogs := context.WithCancel(context.Background())
	defer stopLogs()
	if !migrateOnly {
		logs.Start(logCtx, st)
	}
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
		if err := startAdmin(ctx, cfg, st, box, bus, stop, func() string { return problem.Load().(string) }, log.With(oplog.Key, oplog.Admin)); err != nil {
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
	}, log.With(oplog.Key, oplog.Task)).WithCron(schedule.NewRunner(st.Schedules(), st, taskBudget(cfg, log)))

	// 消息渠道：主人在聊天软件里发来的话变成任务，任务的进展推回去。
	if len(parts.channels) > 0 {
		hub, err := channel.NewHub(parts.channels,
			&channel.Kernel{Store: st, Bus: bus, Budget: taskBudget(cfg, log), Images: st}, st.Channels(), log.With(oplog.Key, oplog.Message))
		if err != nil {
			return err
		}
		if parts.team {
			hub = hub.WithFeedback(channel.TeamFeedback{Store: st.Team()})
		}
		// 手头有事在办时，新来的一句话归给哪件事由主模型判断。
		if parts.triage != nil {
			hub = hub.WithTriage(parts.triage)
		}
		// 用户和助手说的话记进每天的会话；每天过完写成摘要。
		hub = hub.WithSession(st.Sessions())
		if parts.digest != nil {
			go parts.digest.Run(ctx)
		}
		// 启用了文件处理：用户在聊天里发来的文件收进工作区。
		if parts.workspace != nil {
			hub = hub.WithFiles(parts.workspace)
		}
		// 配置的模型都收不了的那几类媒体，收到时直接告诉用户。
		if parts.inputs != nil {
			hub = hub.WithMedia(parts.inputs.Any(llm.InputImage), parts.inputs.Any(llm.InputVideo))
		}
		// 语音消息转成文字交给助手，不把语音交给模型。
		if parts.asr != nil {
			hub = hub.WithASR(parts.asr)
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

	if parts.shares != nil {
		stopShare, err := serveShare(ctx, cfg, parts.shares, log.With(oplog.Key, oplog.Tool))
		if err != nil {
			return err
		}
		defer stopShare()
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
		Tasks:        &channel.Kernel{Store: st, Bus: bus, Budget: taskBudget(cfg, log)},
		ConfigPath:   cfg.ConfigPath,
		PasswordHash: cfg.AdminPasswordHash,
		TLS:          tls,
		// 让进程正常退出，由 systemd 拉起新的。
		Restart: restart,
		Status:  status,
		ShareOn: cfg.ShareAddr != "",
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

// serveShare 开文件下载入口，返回一个用于关停它的函数。和 Webhook 一样先同步监听再异步服务。
func serveShare(ctx context.Context, cfg *config.Config, svc *share.Service, log *slog.Logger) (func(), error) {
	ln, err := net.Listen("tcp", cfg.ShareAddr)
	if err != nil {
		return nil, fmt.Errorf("下载入口监听 %s 失败: %w", cfg.ShareAddr, err)
	}
	srv := &http.Server{
		Handler:           svc.Handler(log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	tls := cfg.ShareTLSCert != "" && cfg.ShareTLSKey != ""
	go func() {
		var err error
		if tls {
			err = srv.ServeTLS(ln, cfg.ShareTLSCert, cfg.ShareTLSKey)
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("下载入口异常退出", "err", err)
		}
	}()
	log.Info("下载入口已开启", "地址", ln.Addr().String(), "https", tls)
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("关闭下载入口失败", "err", err)
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
	team bool
	// triage 用主模型判断聊天里的一句话该归给哪件事。
	triage channel.Triager
	// workspace 非空表示启用了文件处理。
	workspace *files.Workspace
	// shares 非空表示开启了对外分享，要开下载入口。
	shares *share.Service
	// inputs 是各个模型支持的输入，demo 模式下为空。
	inputs *config.ModelInputs
	// asr 非空时聊天里发来的语音消息转成文字交给助手。
	asr *media.ASR
	// digest 非空时每天过完把那天的会话写成摘要。
	digest *agent.Digest
	close  func()
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

	secretLender = safe

	// 模型密钥可以放在保险库里，所以路由排在保险库之后。
	router, err := file.BuildRouter(ctx, safe)
	if err != nil {
		return nil, err
	}
	router.WithLogger(log.With(oplog.Key, oplog.Model))
	// 每次模型调用的用量按天累加，后台概览页显示今天和昨天用了多少 token。
	router.WithUsage(func(u llm.Usage, costMicros int64) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := st.AddUsage(ctx, time.Now(), pg.DayUsage{Calls: 1, PromptTokens: u.PromptTokens,
			CachedTokens: u.CachedTokens, CompletionTokens: u.CompletionTokens, CostMicros: costMicros})
		if err != nil {
			log.Warn("记录模型用量失败", "err", err)
		}
	})
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
		// 一个 MCP server 连不上不应该拖垮整个进程：其余工具照常可用，
		// 但必须让用户知道哪些能力现在不可用。
		connectMCP(ctx, gw, spec, log.With(oplog.Key, oplog.Tool), "已连接 MCP server")
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
		connectMCP(ctx, gw, unreviewedSpec(srv), log.With(oplog.Key, oplog.Tool), "已连接助手安装的 MCP server")
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
		// 建、撤销下载链接直接做：地址要出去，得经过发信、HTTP 请求这些由用户确认的动作。
		if cfg.ShareAddr != "" {
			agent.ShareDefaults(approval)
		}
	}
	// 缺少能力时助手自己找、扫描通过就装。扫描器不可用时装不上，但照样能找。
	agent.CapabilityDefaults(approval)
	// 待填写的环境变量：助手登记名字和用途，值由用户在后台填。
	requests := &vault.Requests{Store: st, Vault: safe}
	installer := &market.Installer{
		AskKey: func(ctx context.Context, label, user, note string) error {
			return requests.Add(ctx, vault.Request{Label: label, Note: note, Tools: []string{user}})
		},
		Client:  &market.Client{},
		Scanner: skillscan.Scanner{Command: cfg.SkillScanner, ExtraArgs: cfg.SkillScannerArgs},
		Skills:  st.Skills(),
		Servers: st.MCPInstalls(),
		Secrets: safe,
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
	// 办事的方法放在 Skill 里，不放在记忆里：出厂的流程写成内置 Skill，记忆里以前总结的流程挪过去。
	if err := skill.SeedBuiltin(ctx, st.Skills(), time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("写入内置流程失败: %w", err)
	}
	if n, err := agent.MovePlaybooks(ctx, memStore, st.Skills(), time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("把记忆里的流程挪成 Skill 失败: %w", err)
	} else if n > 0 {
		log.Info("记忆里的流程已挪成 Skill，原记忆已归档", "条数", n)
	}
	mem := memory.NewRetriever(memStore, nil)
	log.Info("记忆系统就绪")

	planner := agent.NewPlanner(router, gw, agent.PlannerOptions{
		ReplyStyle:  file.Reply.Style,
		ReplyCustom: file.Reply.Custom,
	}).WithMemory(mem).WithVault(safe).WithSkills(st.Skills()).WithCapabilities(st.Skills()).
		WithPrompts(agent.SettingPrompts{Store: st}).
		// 自主模式下改动不再等用户确认，会修改、删除已有东西的动作改由主模型把关。
		WithActionReview(approval.Autonomous).
		// 用户发来的图片原样随上下文交给模型看。
		WithImages(st).
		// 聊天里一句接一句地说：新任务带上同一个会话里最近的对话。
		WithConversation(st).
		// 话记在每天的会话里：新任务带上当天之前的会话和昨天的摘要，任务可以回头查以前的摘要。
		WithSessions(st.Sessions())
	planner = planner.WithSecretRequests(true)
	actuator := agent.NewActuator(gw, router).WithMemory(mem).WithCapabilities(installer, st.Skills()).
		WithSecretRequests(requests).WithSessions(st.Sessions())

	// 联网搜索：用模型服务自带的搜索能力，配置过的服务里有能搜的就自动接上。
	if router.CanSearch() {
		planner = planner.WithWebSearch(true)
		actuator = actuator.WithWebSearch(router)
		log.Info("已启用联网搜索")
	} else {
		log.Warn("配置的模型服务里没有认得的自带联网搜索的，助手不能上网查东西")
	}
	// 配置了 DeepSeek 时接上它的搜索、抓取接口：可以读网页；没有指定用哪个模型搜时，搜索也改走这个接口。
	webTools, err := file.WebTools(ctx, safe)
	if err != nil {
		return nil, fmt.Errorf("接通 DeepSeek 搜索接口失败: %w", err)
	}
	if webTools != nil {
		planner = planner.WithWebFetch(true)
		if file.SearchRouteSet() {
			actuator = actuator.WithWebFetchOnly(webTools)
			log.Info("已启用读网页（DeepSeek 抓取接口），联网搜索按 purpose: search 指定的模型")
		} else {
			planner = planner.WithWebSearch(true)
			actuator = actuator.WithWebTools(webTools)
			log.Info("已启用 DeepSeek 搜索与读网页接口")
		}
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

	pipeline := memory.NewPipeline(memStore, nil)
	// 推断出来的记忆攒多了，由主模型把说的是同一件事的并成一条，免得用户要逐条确认一大堆。
	epilogue := agent.NewEpilogue(router, pipeline).WithMemoryTidy(pipeline).WithSkills(st.Skills()).WithRecall(mem)
	// 表达风格：从用户亲口说的话里学，替他写东西、对他说话时用。
	if file.Reply.LearnVoice() {
		voice := style.NewLearner(st.Style(), router)
		planner = planner.WithVoice(voice)
		epilogue = epilogue.WithVoice(voice)
	}

	// 各个模型支持的输入：图片只交给看得了的模型。
	inputs := file.Inputs()
	planner = planner.WithInputs(inputs.Main, inputs.Workers)
	// 语音、视频的格式转换靠 ffmpeg（微信、QQ 的 silk 语音除外）。
	if len(file.Channels) > 0 && !media.Default.HasFFmpeg() {
		log.Warn("找不到 ffmpeg（它随程序一起发布，应在程序旁边）：飞书发来的语音转不了格式；发语音、视频时会改成按文件发")
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
	// 对外分享：给助手生成的文件建临时下载链接。下载入口由 run 在助手装好之后开。
	var shares *share.Service
	if cfg.ShareAddr != "" {
		if workspace == nil {
			log.Warn("设置了 ATOM_SHARE_ADDR，但没有启用文件处理（files 一节），对外分享不可用")
		} else {
			shares = &share.Service{Store: st, Workspace: workspace, Settings: st}
			planner = planner.WithShares(true)
			actuator = actuator.WithShares(shares)
			log.Info("已启用对外分享", "下载入口", cfg.ShareAddr)
		}
	}
	// 代码沙箱：断网、只读工作区，跑出来的文件收进工作区。机器上跑不了时只记下原因，不影响别的能力。
	if runner, err := file.SandboxRunner(workspace); err != nil {
		return nil, err
	} else if runner != nil {
		if err := runner.Check(); err != nil {
			log.Error("沙箱不可用，助手不能跑代码", "err", err)
		} else {
			planner = planner.WithSandbox(true)
			actuator = actuator.WithSandbox(runner)
			log.Info("已启用沙箱", "解释器", runner.Python, "时间上限", runner.Timeout.String(), "内存上限MB", runner.MemoryMB)
		}
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
	qw, err := file.Weather(safe)
	if err != nil {
		return nil, err
	}
	if qw != nil {
		planner = planner.WithWeather(true)
		actuator = actuator.WithWeather(qw)
		log.Info("已接入天气数据", "来源", "和风天气", "API Host", qw.Host)
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

	channels, err := file.BuildChannels(safe, st, log.With(oplog.Key, oplog.Message))
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
		team:      len(file.Workers()) > 0,
		inputs:    &inputs,
		asr:       file.SpeechRecognizer(),
		triage:    agent.NewTriager(router),
		digest:    agent.NewDigest(router, st.Sessions(), log.With(oplog.Key, oplog.Message)),
		workspace: workspace,
		shares:    shares,
		channels:  channels,
		outbox:    outbox,
		planner:   planner,
		actuator:  actuator,
		approver:  approver,
		// 任务完成后提炼记忆与流程，经写入管线落库。
		epilogue: epilogue,
		close:    func() { _ = gw.Close() },
	}, nil
}

// taskBudget 定出每个任务的预算：配置里写了的用配置的，没写或配置读不出来就用内置的默认值。
func taskBudget(cfg *config.Config, log *slog.Logger) core.Budget {
	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		// 配置有别的问题时助手本来就起不来，那边会报；这里不重复报。
		return core.DefaultBudget()
	}
	return file.TaskBudget()
}

// mcpConnectWait 是启动时等一个 MCP server 连上的最长时间。
const mcpConnectWait = 20 * time.Second

// connectMCP 连接一个 MCP server，最多等 mcpConnectWait；到时还没连上就转到后台继续连，不拖住启动。
//
// 本机运行的 server 第一次启动可能要先下载（npx 取包），慢的时候要几分钟；
// 一个连得慢的 server 不该让整个助手——包括消息源——迟迟起不来。它连上之后工具会自己进目录。
func connectMCP(ctx context.Context, gw *mcpgw.Gateway, spec mcpgw.ServerSpec, log *slog.Logger, done string) {
	result := make(chan error, 1)
	go func() { result <- gw.Connect(ctx, spec) }()
	report := func(err error) {
		if err != nil {
			log.Error("MCP server 连接失败，相关工具不可用", "server", spec.Name, "err", err)
			return
		}
		log.Info(done, "server", spec.Name, "工具数", len(gw.ToolsOf(spec.Name)))
	}
	select {
	case err := <-result:
		report(err)
	case <-time.After(mcpConnectWait):
		log.Warn("MCP server 还没连上，转到后台继续连接，连上之后它的工具才可用", "server", spec.Name, "已等待", mcpConnectWait.String())
		go func() { report(<-result) }()
	}
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

// unreviewedSpec 是对话里装上的远程 server 的连接参数。
//
// 助手自己找来装的按固定的保守边界对待；用户亲口给出地址的按用户接入的对待，
// 它要的密钥连接时从保险库取。
func unreviewedSpec(srv *market.Server) mcpgw.ServerSpec {
	spec := mcpgw.ServerSpec{Name: srv.Name, Transport: mcpgw.TransportHTTP, URL: srv.URL}
	if !srv.ByUser {
		spec.Unreviewed = true
		return spec
	}
	spec.UserAdded = true
	for _, sec := range srv.AllSecrets() {
		if secretLender == nil {
			break
		}
		// 句柄不合规矩时这一项取值会失败：连接因为缺密钥失败并报出来，不会带着错的东西连出去。
		take, err := secretLender.Lend(sec.Secret, "mcp."+srv.Name)
		if err != nil {
			take = func(context.Context) (string, error) { return "", err }
		}
		spec.Secrets = append(spec.Secrets, mcpgw.NamedSecret{Name: sec.Name, Take: take})
	}
	return spec
}

// secretLender 是保险库，build 里装好之后填进来，供 unreviewedSpec 取用户给的 server 的密钥。
var secretLender config.SecretLender

func newLogger(level string) (*slog.Logger, *oplog.Handler) {
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
	// 写到标准输出的同时，把 Info 及以上的记进操作日志，见 oplog。
	logs := oplog.NewHandler(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
	l := slog.New(logs)
	slog.SetDefault(l)
	return l, logs
}
