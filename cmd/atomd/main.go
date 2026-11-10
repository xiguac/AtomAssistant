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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xiguac/atomassistant/internal/admin"
	"github.com/xiguac/atomassistant/internal/agent"
	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/demo"
	"github.com/xiguac/atomassistant/internal/device"
	"github.com/xiguac/atomassistant/internal/event"
	"github.com/xiguac/atomassistant/internal/files"
	"github.com/xiguac/atomassistant/internal/kernel"
	"github.com/xiguac/atomassistant/internal/llm"
	"github.com/xiguac/atomassistant/internal/market"
	"github.com/xiguac/atomassistant/internal/matter"
	"github.com/xiguac/atomassistant/internal/mcpgw"
	"github.com/xiguac/atomassistant/internal/media"
	"github.com/xiguac/atomassistant/internal/memory"
	"github.com/xiguac/atomassistant/internal/memvec"
	"github.com/xiguac/atomassistant/internal/oplog"
	"github.com/xiguac/atomassistant/internal/policy"
	"github.com/xiguac/atomassistant/internal/schedule"
	"github.com/xiguac/atomassistant/internal/seal"
	"github.com/xiguac/atomassistant/internal/share"
	"github.com/xiguac/atomassistant/internal/skill"
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
	// 退出时先停日志、等剩下的写完，再关库（defer 后进先出，这一条排在 st.Close 之前执行）。
	logCtx, stopLogs := context.WithCancel(context.Background())
	if !migrateOnly {
		logsDone := logs.Start(logCtx, st)
		defer func() { stopLogs(); <-logsDone }()
	} else {
		defer stopLogs()
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
	reloads := make(chan chan error)
	// reload 交给后台：保存配置后调用，等新配置装好并换上（或装不起来）才返回。
	reload := func(rctx context.Context) error {
		done := make(chan error, 1)
		select {
		case reloads <- done:
		case <-rctx.Done():
			return rctx.Err()
		}
		select {
		case err := <-done:
			return err
		case <-rctx.Done():
			return rctx.Err()
		}
	}
	adminOn := cfg.AdminAddr != "" && !checkOnly
	if adminOn {
		if err := startAdmin(ctx, cfg, st, box, bus, reload, func() string { return problem.Load().(string) }, log.With(oplog.Key, oplog.Admin)); err != nil {
			return err
		}
	}

	p, err := assemble(initCtx, cfg, st, box, log)
	if checkOnly {
		if err != nil {
			return err
		}
		p.close()
		log.Info("配置与连接校验通过")
		return nil
	}
	if err != nil && !adminOn {
		return err
	}

	// Webhook 入口只用到事件总线，不随配置变，开一次就行。
	if cfg.WebhookAddr != "" {
		stopWebhook, err := serveWebhook(ctx, cfg, bus, log)
		if err != nil {
			return err
		}
		defer stopWebhook()
	}

	// 助手本身按配置装出来。后台里改了配置，就在进程内按新配置再装一份，装好了才换下旧的；
	// 装不起来就留着旧的接着跑，把原因告诉后台。整个过程不重启服务，后台也不断开。
	var cur *generation
	if err != nil {
		// 助手起不来，但后台要留着：否则用户连修的地方都没有。
		log.Error("助手未能启动，目前只有管理后台在运行", "err", err)
		problem.Store(err.Error())
	} else {
		if cur, err = startGeneration(ctx, cfg, st, bus, p, log); err != nil {
			p.close()
			return err
		}
	}
	for {
		var failed <-chan error
		if cur != nil {
			failed = cur.failed
		}
		select {
		case <-ctx.Done():
			if cur != nil {
				cur.stop()
			}
			return nil
		case err := <-failed:
			cur.stop()
			return err
		case done := <-reloads:
			buildCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			np, err := assemble(buildCtx, cfg, st, box, log)
			cancel()
			if err != nil {
				if cur != nil {
					log.Error("新配置未能生效，助手仍按原来的配置运行", "err", err)
				} else {
					problem.Store(err.Error())
				}
				done <- err
				continue
			}
			if cur != nil {
				cur.stop()
			}
			if cur, err = startGeneration(ctx, cfg, st, bus, np, log); err != nil {
				np.close()
				problem.Store(err.Error())
				done <- err
				continue
			}
			problem.Store("")
			log.Info("已按新配置重新载入，未重启服务")
			done <- nil
		}
	}
}

// generation 是按某一份配置跑起来的助手：调度器、消息渠道、每日摘要、下载入口。
type generation struct {
	// failed 在调度器意外退出时收到它的错误。
	failed chan error
	stop   func()
}

// rollSessionsEvery 是检查会话有没有过了一天的间隔。
const rollSessionsEvery = 5 * time.Minute

// rollSessions 每隔一段时间收尾过去的日子的会话，直到 ctx 结束。
func rollSessions(ctx context.Context, tasks *channel.Kernel, log *slog.Logger) {
	// 有每天的会话之前建的聊天任务，先挂进各自那天的会话。
	if err := tasks.AdoptChatTasks(ctx); err != nil && ctx.Err() == nil {
		log.Error("把以前的聊天任务归进会话失败", "err", err)
	}
	ticker := time.NewTicker(rollSessionsEvery)
	defer ticker.Stop()
	for {
		if err := tasks.RollSessions(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Error("收尾前一天的会话失败，稍后再试", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// startGeneration 用装好的部件把助手跑起来。stop 让它们都停下、等它们退出，再释放部件。
//
// 调度器停下时会把在跑的任务放回队列，下一代接着跑，不必等租约过期。
func startGeneration(parent context.Context, cfg *config.Config, st *pg.Store, bus *event.Bus, parts *parts, log *slog.Logger) (*generation, error) {
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	var closers []func()
	g := &generation{failed: make(chan error, 1)}
	g.stop = func() {
		cancel()
		wg.Wait()
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
		parts.close()
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
		tasks := &channel.Kernel{Store: st, Bus: bus, Budget: taskBudget(cfg, log), Images: st}
		hub, err := channel.NewHub(parts.channels, tasks, st.Channels(), log.With(oplog.Key, oplog.Message))
		if err != nil {
			cancel()
			return nil, err
		}
		// 过了凌晨 4 点：前一天的会话收尾，没办完的事挂到新一天的会话下面。
		wg.Add(1)
		go func() {
			defer wg.Done()
			rollSessions(ctx, tasks, log.With(oplog.Key, oplog.Message))
		}()
		if parts.team {
			hub = hub.WithFeedback(channel.TeamFeedback{Store: st.Team()})
		}
		// 手头有事在办时，新来的一句话归给哪件事由主模型判断。
		if parts.triage != nil {
			hub = hub.WithTriage(parts.triage)
		}
		// 用户和助手说的话记进每天的会话；每天过完写成摘要。
		hub = hub.WithSession(st.Sessions())
		// 用户交代的事记在一张清单里：新来的一句话归给清单里的哪件事，放久了的事淡出、删掉。
		hub = hub.WithMatters(st.Matters(), st)
		if parts.digest != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				parts.digest.Run(ctx)
			}()
		}
		if parts.archive != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				archiveExpiredMemories(ctx, parts.archive, log)
			}()
		}
		if parts.profile != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				parts.profile.Run(ctx)
			}()
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
		phoneHub.Store(hub)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := hub.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("消息渠道异常退出", "err", err)
			}
		}()
	}

	if parts.shares != nil {
		stopShare, err := serveShare(ctx, cfg, parts.shareAddr, parts.shares, log.With(oplog.Key, oplog.Tool))
		if err != nil {
			cancel()
			wg.Wait()
			return nil, err
		}
		closers = append(closers, stopShare)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := sched.Run(ctx); err != nil && ctx.Err() == nil {
			g.failed <- err
		}
	}()
	return g, nil
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
	reload func(context.Context) error, status func() string, log *slog.Logger) error {
	tls := cfg.AdminTLSCert != "" && cfg.AdminTLSKey != ""
	if err := admin.CheckAddr(cfg.AdminAddr, tls); err != nil {
		return err
	}
	safe, err := vault.Open(st.Vault(), cfg.VaultKey)
	if err != nil {
		return err
	}
	devices := &device.Service{Store: st, Bus: bus, Memory: memory.NewPipeline(st.Memories(), nil), Asker: phoneAsker{settings: st}}
	if lookup := openIPRegion(cfg, log); lookup != nil {
		devices.IPRegion = lookup
	}
	srv, err := admin.New(admin.Options{
		Store: st, Vault: safe, Box: box,
		Scanner:      skillscan.Scanner{Command: cfg.SkillScanner, ExtraArgs: cfg.SkillScannerArgs},
		Tasks:        &channel.Kernel{Store: st, Bus: bus, Budget: taskBudget(cfg, log)},
		ConfigPath:   cfg.ConfigPath,
		PasswordHash: cfg.AdminPasswordHash,
		TLS:          tls,
		// 保存配置后在进程内按新配置重新装一遍助手，不重启服务。
		Reload:   reload,
		Status:   status,
		ShareEnv: cfg.ShareAddr,
		// 手机上的 App：上报数据时要发事件唤醒等它的任务，所以带上事件总线。
		Devices: devices,
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

// phoneHub 是手机提问要交给的渠道中枢。每次按配置重新装助手时会换成新的那个。
var phoneHub atomic.Pointer[channel.Hub]

// phoneAsker 把手机上的提问交给渠道中枢，以主人的身份在后台设置的消息渠道里发起一次对话。
type phoneAsker struct {
	settings interface {
		Setting(ctx context.Context, key string) (string, error)
	}
}

func (p phoneAsker) Ask(ctx context.Context, text string, images []device.AskImage) error {
	hub := phoneHub.Load()
	if hub == nil {
		return fmt.Errorf("%w: 消息渠道还没有就绪", device.ErrUnavailable)
	}
	name, err := p.settings.Setting(ctx, device.ReplyChannelSetting)
	if err != nil {
		return err
	}
	if name == "" {
		name = device.DefaultReplyChannel
	}
	atts := make([]channel.Attachment, 0, len(images))
	for i, img := range images {
		img := img
		atts = append(atts, channel.Attachment{
			ContentType: img.MIME, Name: fmt.Sprintf("screenshot-%d", i+1),
			// 图片就在内存里，不用再去下载。
			Fetch: func(context.Context) (channel.Picture, error) { return channel.Picture{MIME: img.MIME, Data: img.Data}, nil },
		})
	}
	if err := hub.InjectOwner(ctx, name, text, atts); err != nil {
		// 渠道没配、没绑主人这类是配置问题，原样告诉手机。
		return fmt.Errorf("%w: %v", device.ErrInvalid, err)
	}
	return nil
}

// defaultIP2RegionDB 是 ip2region 库在服务器上的默认位置，deploy/deploy.sh ip2region 会把它放在这里。
const defaultIP2RegionDB = "/opt/atomassistant/data/ip2region_v4.xdb"

// openIPRegion 打开公网 IP 归属地库。没有这个文件就不开这个功能（不是错误）；文件在但打不开才报。
func openIPRegion(cfg *config.Config, log *slog.Logger) device.RegionLookup {
	path, explicit := cfg.IP2RegionDB, cfg.IP2RegionDB != ""
	if path == "" {
		path = defaultIP2RegionDB
	}
	if _, err := os.Stat(path); err != nil {
		if explicit {
			log.Warn("ATOM_IP2REGION_DB 指的文件打不开，公网 IP 归属地关闭", "路径", path, "err", err)
		} else {
			log.Info("没有 IP 归属地库，公网 IP 归属地关闭（deploy/deploy.sh ip2region 可以装上）", "路径", path)
		}
		return nil
	}
	r, err := device.OpenIP2Region(path)
	if err != nil {
		log.Error("IP 归属地库没能打开，公网 IP 归属地关闭", "路径", path, "err", err)
		return nil
	}
	log.Info("公网 IP 归属地已开启", "库", path)
	return r
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
func serveShare(ctx context.Context, cfg *config.Config, addr string, svc *share.Service, log *slog.Logger) (func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("下载入口监听 %s 失败: %w", addr, err)
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
	// outbox 非空时助手可以往聊天窗口里发文件和提醒，渠道中枢建好后要填进去。
	outbox *chatOutbox
	// team 为真表示配置了协作模型。
	team bool
	// triage 用主模型判断聊天里的一句话该归给哪件事。
	triage channel.Triager
	// workspace 非空表示启用了文件处理。
	workspace *files.Workspace
	// shares 非空表示开启了对外分享，要在 shareAddr 上开下载入口。
	shares    *share.Service
	shareAddr string
	// inputs 是各个模型支持的输入，demo 模式下为空。
	inputs *config.ModelInputs
	// asr 非空时聊天里发来的语音消息转成文字交给助手。
	asr *media.ASR
	// digest 非空时每天过完把那天的会话写成摘要。
	digest *agent.Digest
	// profile 非空时每天把已保存的记忆整合成一段话，放在后台「记忆」页最上面。
	profile *agent.Profile
	// archive 把过期很久的限时记忆归档，返回归档的条数。
	archive func(context.Context) (int, error)
	close   func()
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
	// 下载入口：服务器上设了 ATOM_SHARE_ADDR 就用它，否则看后台「文件」页里开没开。
	shareAddr := cfg.ShareAddr
	if shareAddr == "" {
		if shareAddr, err = st.Setting(ctx, share.ListenSetting); err != nil {
			return nil, err
		}
		if shareAddr != "" {
			if err := share.CheckAddr(shareAddr, cfg.ShareTLSCert != "" && cfg.ShareTLSKey != ""); err != nil {
				return nil, err
			}
		}
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
	if len(file.Connectors.CalendarList()) > 0 {
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
		if shareAddr != "" {
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
	// 向量记忆：记忆的增删改实时同步进本机的向量库，检索时按意思召回。不可用时退回文字重合。
	var similarity memory.Similarity
	stopVectors := func() {}
	if c := file.MemoryVector; c != nil {
		idx := memvec.New(&memvec.Embedder{URL: c.EmbedURL}, &memvec.Qdrant{URL: c.QdrantURL, APIKey: cfg.QdrantKey},
			memStore, key.VectorKey(), log.With(oplog.Key, oplog.System))
		vecCtx, cancelVec := context.WithCancel(context.Background())
		unhook := st.OnMemoryChange(idx.Notify)
		go idx.Run(vecCtx)
		stopVectors = func() { unhook(); cancelVec() }
		similarity = idx
		log.Info("已启用向量记忆", "向量服务", c.EmbedURL, "向量库", c.QdrantURL)
	}
	mem := memory.NewRetriever(memStore, similarity)
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
		WithSessions(st.Sessions()).
		// 用户说的"一件事"常常先后有好几个任务：新任务带上那件事此前的进展。
		WithMatters(matter.Reader{Store: st.Matters()})
	planner = planner.WithSecretRequests(true).WithDevices(true)
	actuator := agent.NewActuator(gw, router).WithMemory(mem).WithCapabilities(installer, st.Skills()).
		WithSecretRequests(requests).WithSessions(st.Sessions()).
		// 助手只读数据、提授权需求；发事件的是后台那一份。
		WithDevices(&device.Service{Store: st})

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
		voice := style.NewLearner(st.Style(), router).WithDir(style.Dir(cfg.ConfigPath))
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
	// 日历和邮箱可以各接好几个账号，对助手仍是一套工具，账号的名字和用途写进工具说明。
	cals, err := file.CalendarAccounts(safe)
	if err != nil {
		return nil, err
	}
	var calAccounts []agent.Account[agent.CalendarService]
	for _, c := range cals {
		calAccounts = append(calAccounts, agent.Account[agent.CalendarService]{Name: c.Name, Tags: c.Tags, Svc: c.Client})
		log.Info("已接入日历", "名称", c.Name, "服务器", c.Client.Endpoint, "账号", c.Client.Username)
	}
	if len(calAccounts) > 0 {
		planner = planner.WithCalendar(true)
		actuator = actuator.WithCalendar(agent.Calendars(calAccounts))
	}
	if workspace != nil {
		planner = planner.WithFiles(true)
		actuator = actuator.WithFiles(workspace)
		log.Info("已启用文件处理", "工作区", workspace.Root, "加密存放", file.Files.Encrypt)
	}
	// 对外分享：给助手生成的文件建临时下载链接。下载入口由 run 在助手装好之后开。
	var shares *share.Service
	if shareAddr != "" {
		if workspace == nil {
			log.Warn("开启了下载入口，但没有启用文件处理（files 一节），对外分享不可用")
		} else {
			shares = &share.Service{Store: st, Workspace: workspace, Settings: st}
			planner = planner.WithShares(true)
			actuator = actuator.WithShares(shares)
			log.Info("已启用对外分享", "下载入口", shareAddr)
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
	feeds, err := file.HolidayFeeds()
	if err != nil {
		return nil, err
	}
	if len(feeds) > 0 {
		var hs []agent.HolidayService
		for _, f := range feeds {
			hs = append(hs, f)
			log.Info("已接入节假日数据", "订阅", f.URL)
		}
		planner = planner.WithHolidays(true)
		actuator = actuator.WithHolidays(agent.Holidays(hs))
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
	boxes, err := file.MailAccounts(safe)
	if err != nil {
		return nil, err
	}
	var mailAccounts []agent.Account[agent.MailService]
	canSend := false
	for _, b := range boxes {
		mailAccounts = append(mailAccounts, agent.Account[agent.MailService]{Name: b.Name, Tags: b.Tags, Svc: b.Client})
		canSend = canSend || b.Client.SMTPAddr != ""
		log.Info("已接入邮件", "名称", b.Name, "收信", b.Client.IMAPAddr, "发信", b.Client.SMTPAddr, "账号", b.Client.Username)
	}
	if len(mailAccounts) > 0 {
		planner = planner.WithMail(true, canSend)
		actuator = actuator.WithMail(agent.Mailboxes(mailAccounts))
	}
	planner = planner.WithAccounts(agent.AccountsNote("日历", calAccounts), agent.AccountsNote("邮箱", mailAccounts))

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
	if len(channels) > 0 {
		outbox = &chatOutbox{}
		// 向用户的手机下发授权需求时，同时在聊天里提醒他。
		actuator = actuator.WithDeviceNotice(outbox)
		if workspace != nil {
			planner = planner.WithChatFiles(true)
			actuator = actuator.WithChatFiles(outbox)
		}
	}

	return &parts{
		team:   len(file.Workers()) > 0,
		inputs: &inputs,
		asr:    file.SpeechRecognizer(),
		triage: agent.NewTriager(router),
		// 每天的摘要取自事项清单：那天办了哪些事、各自办成了什么样。
		digest: agent.NewDigest(router, st.Sessions(), log.With(oplog.Key, oplog.Message)).WithMemory(epilogue).
			WithMatters(matter.Reader{Store: st.Matters()}),
		archive: func(ctx context.Context) (int, error) {
			return pipeline.ArchiveExpired(ctx, expiredMemoryGrace)
		},
		profile: agent.NewProfile(func(ctx context.Context, req llm.Request) (llm.Response, error) {
			return router.Complete(ctx, llm.PurposeSummarize, req)
		}, st.Memories(), st, log.With(oplog.Key, oplog.Message)),
		workspace: workspace,
		shares:    shares,
		shareAddr: shareAddr,
		channels:  channels,
		outbox:    outbox,
		planner:   planner,
		actuator:  actuator,
		approver:  approver,
		// 任务完成后提炼记忆与流程，经写入管线落库。
		epilogue: epilogue,
		close:    func() { stopVectors(); _ = gw.Close() },
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

func (c *chatOutbox) SayToTask(ctx context.Context, tags []string, taskID, text string) error {
	hub := c.hub.Load()
	if hub == nil {
		return fmt.Errorf("消息渠道还没有就绪")
	}
	return hub.SayToTask(ctx, tags, taskID, text)
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

// expiredMemoryGrace 是限时记忆过期之后再留多久才归档：这段时间里还能回溯「上个月那件事是怎么定的」。
const expiredMemoryGrace = 30 * 24 * time.Hour

// archiveExpiredMemories 每隔几小时把过期很久的限时记忆归档，直到 ctx 结束。
func archiveExpiredMemories(ctx context.Context, archive func(context.Context) (int, error), log *slog.Logger) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		if n, err := archive(ctx); err != nil && ctx.Err() == nil {
			log.Error("归档过期的限时记忆失败，稍后再试", "err", err)
		} else if n > 0 {
			log.Info("已归档过期的限时记忆", "条数", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
