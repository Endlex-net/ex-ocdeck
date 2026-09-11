// Command ocdeck-server 是 ocdeck 服务端入口（design.md §1/§10/§11）。
//
// 启动流程：加载配置 + 启动校验 → 打开 SQLite → 启动 HTTP 服务。
// shutdownPolicy=kill_immediate 时，在任何会话创建之前 SpawnWatchdog（design.md §10）。
// v1 支持 macOS 与 Linux（含 WSL）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"ocdeck/internal/api"
	"ocdeck/internal/application"
	"ocdeck/internal/application/diffreview"
	appnotification "ocdeck/internal/application/notification"
	apptask "ocdeck/internal/application/task"
	appuploads "ocdeck/internal/application/uploads"
	"ocdeck/internal/config"
	"ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
	"ocdeck/internal/infrastructure/ai"
	"ocdeck/internal/infrastructure/auditlog"
	"ocdeck/internal/infrastructure/eventbus"
	"ocdeck/internal/infrastructure/lifecycle"
	"ocdeck/internal/infrastructure/notify"
	"ocdeck/internal/infrastructure/palette"
	"ocdeck/internal/infrastructure/process"
	sqlite "ocdeck/internal/infrastructure/sqlite"
	"ocdeck/internal/infrastructure/store"
	uploads "ocdeck/internal/infrastructure/uploads"
	"ocdeck/internal/infrastructure/worktree"
	"ocdeck/internal/task"
)

func main() {
	// watchdog 自调子进程入口（design.md §10）。
	if len(os.Args) > 1 && os.Args[1] == process.WatchdogSubcommand {
		if err := runWatchdog(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ocdeck-server watchdog: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ocdeck-server: %v\n", err)
		os.Exit(1)
	}
}

// runWatchdog 解析 watchdog 子进程参数并进入轮询循环（design.md §10）。
// 参数：watchdog <ppid> <socketName> <tmpdir>。
func runWatchdog(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("watchdog: need <ppid> <socketName> <tmpdir>")
	}
	ppid, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("watchdog: invalid ppid %q: %w", args[0], err)
	}
	return process.RunWatchdog(ppid, args[1], args[2])
}

func run() error {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	if err := config.ApplyEnvFile(); err != nil {
		return err
	}

	cfg, release, err := config.Load(config.DefaultOptions())
	if err != nil {
		return err
	}
	// release 持有 dataDir 单实例 flock（design.md §10）。MUST 在 store.Close() 之后释放：
	// defer 为 LIFO，故 db.Close()（后注册）先于 release()（先注册）执行，
	// 保证 flock 释放前 SQLite 已关闭、不会与下一实例的 store.Open 竞态。
	defer release()

	log.Printf("ocdeck-server starting: version=%s dataDir=%s listen=%s policy=%s opencode=%s tmux=%s",
		version, cfg.DataDir, cfg.ListenAddr, cfg.ShutdownPolicy, cfg.OpenCodeVersion, cfg.TmuxVersion)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	// store.Close() 在 release()(flock) 之前执行（LIFO defer）。
	defer db.Close()

	// watchdog 接入（design.md §10：kill_immediate 时在任何会话创建前 SpawnWatchdog）。
	var wd *process.WatchdogManager
	if cfg.ShutdownPolicy == config.ShutdownKillImmediate {
		wd, err = spawnWatchdog(cfg)
		if err != nil {
			return fmt.Errorf("spawn watchdog: %w", err)
		}
		// wd.Stop() 在 tm.Shutdown 后显式调用（design.md §10：watchdog 不得先停）。
	}

	// tmux tmpdir（design.md §2：TMUX_TMPDIR=<dataDir>/tmux）。
	tmpdir, err := process.EnsureTmpDir(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("ensure tmux tmpdir: %w", err)
	}
	procMgr := process.New(process.Options{
		SocketName: "ocdeck",
		Tmpdir:     tmpdir,
		BaseEnv:    process.DefaultBaseEnv(os.LookupEnv),
	})
	wtMgr := worktree.New(cfg.DataDir)

	// AI 配置 Store（design.md D7 wiring）：单实例同时注入命名链与 API 层。
	// LoadStore 对不存在/损坏/非法配置均不返回致命错误（保持启动），故无需 err 处理。
	aiStore := ai.LoadStore(cfg.DataDir)
	namer := ai.NewSlugNamer(aiStore, task.Slugify) // fallback 用本包 Slugify（避免 ai→task 循环依赖）
	// task-permission-mode D5 装配：PermJudge 与 SlugNamer 同族（ai 不 import task），
	// 经 composition root 适配为 task.PermissionJudge 端口注入 Manager；
	// LLM 未配置/失败时 Judge 返回 uncertain（转人工），Manager nil judge 防御同型。
	permJudge := ai.NewPermJudge(aiStore)

	// task-permission-mode D11 装配（tasks 7.1）：ai-auto 判定审计日志
	// <LogDir>/ai-permission-audit.jsonl（与 Manager LogDir 同源 cfg.DataDir+"/logs"）。
	// 装配失败仅记日志、注入 nil（MUST NOT 注入持有 nil 指针的非 nil interface，
	// MUST NOT 阻断 server 启动）。
	var permAudit task.PermAuditLogger
	if al, aerr := auditlog.NewPermAuditLogger(filepath.Join(cfg.DataDir, "logs", "ai-permission-audit.jsonl")); aerr != nil {
		log.Printf("warning: ai permission audit logger disabled: %v", aerr)
	} else {
		permAudit = permAuditAdapter{inner: al}
		defer al.Close()
	}

	// TaskManager 构造（design.md §18）。
	adapter := task.NewStoreAdapter(db)
	// P1.4.4/P1.4.5 wiring：sqlite adapter 实现 application ports（TaskRepository +
	// TaskReadRepository + SessionRepository），经 application/task LifecycleService 编排
	// Get/List/Archive/Restore 与 session claim/touch/delete/align/attention 提交位，
	// 注入 Manager facade。P1.6.5：单例 bus 经窄 Publisher 接口注入（commit helper
	// 真实发布领域事件；*eventbus.Bus 结构性满足 application.Publisher），
	// 同一 bus 供 Phase 2 SSE 消费侧订阅。
	bus := eventbus.New()
	appAdapter := sqlite.New(db)
	lifecycleSvc := apptask.New(apptask.Options{
		Tasks:    appAdapter,
		Read:     appAdapter,
		Sessions: appAdapter,
		Publish:  bus,
	})
	// terminal-file-paste-drop 2.4：受管上传编排器（存储 + 编排 + 事件订阅/清理器）。
	// 构造先于 Manager：其 per-task 协调锁与 Manager 生命周期提交共享（2.5）。
	// Conns 数据源（api WS 注册表）构造晚于此处，serveAndShutdown 中 setServer 回填。
	uploadConns := &currentConnPort{}
	uploadOrch := newUploadOrchestrator(cfg, appAdapter, bus, uploadConns)
	tm := task.New(task.Options{
		Cfg:                cfg,
		Store:              adapter,
		Proc:               task.NewProcessAdapter(procMgr),
		Worktree:           task.NewWorktreeAdapter(wtMgr),
		DebtStore:          adapter,         // R7：orphan tickets 持久化跨重启恢复（design.md §10）
		LifecycleRunner:    lifecycle.New(), // design.md §7.1：init/pre-delete 脚本与 inherit
		LogDir:             cfg.DataDir + "/logs",
		Namer:              namer,                              // ai-worktree-naming：Create 经 LLM 提炼分支 slug，未配置时内部回退 Slugify
		PermissionJudge:    permJudgeAdapter{inner: permJudge}, // task-permission-mode D5：ai-auto 权限请求自动判定
		PermAuditLogger:    permAudit,                          // task-permission-mode D11：ai-auto 判定审计日志（旁路）
		Lifecycle:          lifecycleSvc,                       // P1.4.4：Get/List/Archive/Restore 委托
		Publish:            bus,                                // idle-reminder-user-activity：用户活动上报经同一 bus 发布 task.user_activity
		UploadCoordination: uploadOrch.Coordination(),          // terminal-file-paste-drop 2.5：离开 active 的提交入协调锁
	})
	// 注入 Manager 生命周期 context（design.md §4：SSE/退出监视挂进程 ctx，非 HTTP request ctx）。
	tm.SetLifecycleCtx(ctx)

	// diff-review-workbench composition root（design.md D9/F1）。
	// 在 Reconcile/启动调度/HTTP 开放前构造 diffreview service 并注入 Manager。
	// 五个 consumer-owned ports（DiffReviewRepository/TaskScopePort 在 store 包，
	// PromptPort/DiffSourcePort/RuntimePort 在 task 层）+ FileEditPort（task 层）。
	diffRepo := store.NewDiffReviewRepoAdapter(db.Queries)
	diffScope := store.NewTaskScopeAdapter(db.Queries)
	diffPrompt := task.NewPromptPortAdapter(tm)
	diffSource := task.NewDiffSourcePortAdapter(tm)
	diffRuntime := task.NewRuntimePortAdapter(tm)
	diffFileEdit := task.NewFileEditPortAdapter(tm)
	diffSvc := diffreview.New(diffreview.Options{
		Repo:     diffRepo,
		Scope:    diffScope,
		Prompt:   diffPrompt,
		Diff:     diffSource,
		Runtime:  diffRuntime,
		FileEdit: diffFileEdit,
	})
	tm.SetDiffReviewService(diffSvc)

	// 服务启动全局收敛（design.md D2 重启恢复① + F1）。
	// MUST 在 Reconcile/启动调度/HTTP 开放前执行：单事务 sending→delivery_unknown + 固定 error。
	// 写库失败 fail-closed：不开放 API/调度器。未注入 diffreview.Service → no-op。
	// F12①：收敛→开放序列收口在 diffReviewStartupGate，run() 与 main_test.go 共用同一函数
	//（测试断言的是生产编排的 fail-closed 语义，而非复制模拟编排）。
	return diffReviewStartupGate(ctx, tm, func() error {
		return serveAndShutdown(ctx, tm, cfg, db, bus, aiStore, wd, diffSvc, uploadOrch, uploadConns)
	})
}

// serveAndShutdown 执行收敛通过后的启动序列（design.md §5/§10）：
// 上传编排启动（扫描/订阅/清理器）→ Reconcile（失败 fail-closed 拒绝开放 HTTP）→
// 后台周期重试 → API 装配与阻塞服务 → 关停序列。
// diffSvc 注入 API 层（diff-review-workbench D8 路由），须在 RebuildRoutes 前生效。
func serveAndShutdown(ctx context.Context, tm *task.Manager, cfg *config.Config, db *store.DB, bus *eventbus.Bus, aiStore *ai.Store, wd *process.WatchdogManager, diffSvc *diffreview.Service, uploadOrch *appuploads.Orchestrator, uploadConns *currentConnPort) error {
	// 受管上传启动序列（terminal-file-paste-drop 2.4）：启动恢复扫描（失败记日志，
	// 不阻断启动——遗留产物按孤儿规则由周期清理收敛）→ 订阅 task.deleted 与周期清理。
	// 先扫描后订阅：订阅起点覆盖 Reconcile 期间的删除事件。Reconcile 失败提前返回时
	// 同步 Stop（订阅不泄漏）。
	if err := uploadOrch.StartupScan(ctx); err != nil {
		log.Printf("warning: uploads startup scan: %v (deferred to periodic cleanup)", err)
	}
	uploadOrch.Start(ctx)

	// 启动 reconciliation（design.md §5/§10，HTTP 就绪前完成对账）。
	// Reconcile 失败 MUST 拒绝开放 HTTP（fail-closed）：状态不确定时开放管理面会让用户操作
	// 建立在错误状态上（会话/DB 失同步，后续操作可能破坏数据安全边界）。
	if err := tm.Reconcile(ctx); err != nil {
		uploadOrch.Stop()
		return fmt.Errorf("reconcile: %w", err)
	}
	// 后台周期重试（design.md §5：30s 消化 retryable notice）。
	bgStop := tm.StartBackground(ctx)

	srv := api.New(cfg, db)
	srv.SetTaskBackend(tm)
	// terminal-file-paste-drop：Conns 端口数据源回填（api WS 注册表）与投递编排注入
	//（(bool, DeliverErrorCode) → api 窄接口 (bool, string)），须在 RebuildRoutes 前生效。
	uploadConns.setServer(srv)
	srv.SetDeliverOrchestrator(deliverOrchAdapter{inner: uploadOrch})
	// terminal-file-paste-drop：TUI 连接替换提交接入同一把 per-task 协调锁（与投递
	// 准入/finalize 临界区互斥，D5 原子边界；等待 4009 握手不持锁）。
	srv.SetDeliverReplaceCoordination(uploadOrch.Coordination())
	// terminal-file-paste-drop 3.2：上传编排与限额注入（handler 准入决策归编排层，
	// 请求总量兜底 M+1MiB 的 M 与投递路径计算同源 cfg），须在 RebuildRoutes 前生效。
	srv.SetUploadAdmission(uploadOrch)
	srv.SetUploadLimits(cfg.UploadMaxBytes, cfg.UploadDir)
	// P1.6.5：消费侧注入同一 bus（经 eventSubscriberAdapter 适配 api.EventSubscriber；
	// SSE 端点消费在 Phase 2 建立），须在 RebuildRoutes 前生效。
	srv.SetEventSubscriber(eventSubscriberAdapter{bus})
	// AI 配置 Store 注入 API 层（design.md D7 wiring）：单实例同时供给命名链。
	// 沿用 SetTaskBackend 位置模式，须在 RebuildRoutes 前生效。
	srv.SetAIConfigStore(aiStore)
	// diffreview service 注入 API 层（diff-review-workbench D8 批注/提交/git/file 路由）。
	srv.SetDiffReviewService(diffSvc)
	// 全局 oc 配置管理（design.md §13/§21）：~/.config/opencode/ 下 *.json/*.jsonc。
	ocCfgDir, ocCfgErr := config.DefaultOCConfigDir()
	if ocCfgErr == nil {
		srv.SetOCConfigService(config.NewOCConfigManager(ocCfgDir))
	} else {
		log.Printf("warning: oc-config dir unavailable: %v (oc-configs endpoints disabled)", ocCfgErr)
	}

	// 通知装配（task-notifications design D11）：notifyStore 损坏不致命；
	// webHub 与 web 渠道共享同一实例；notifier 经窄端口注入。装配本身无致命
	// 错误（LoadStore 降级默认配置）；Listen 失败拒绝启动（与既有 HTTP 启动一致）。
	notifyStore := notify.LoadStore(cfg.DataDir)
	webHub := srv.NotificationHub()
	notifier := appnotification.New(appnotification.Options{
		Bus:             notifyEventSubscriberAdapter{bus},
		Tasks:           tm,
		ListActive:      tm,
		Cfg:             notifyStore,
		Channels:        notify.BuildChannels(webHub, runtime.GOOS),
		ResolveBaseURL:  srv.NotificationBaseURL,
		Summarizer:      summaryCompleterAdapter{store: aiStore},
		LastAgentOutput: tm,
	})
	srv.SetNotificationStore(notifyStore)
	srv.SetNotificationTester(notifier)
	srv.SetPaletteConfigStore(palette.LoadStore(cfg.DataDir))

	srv.RebuildRoutes()
	if wd != nil {
		srv.SetWatchdogStateProvider(wd.StateString)
	}
	// D11：Listen → notifier.Start → Serve。Listen 失败仍进入统一关停段
	// （G1：不得绕过 notifier.Stop / tm.Shutdown / bgStop / watchdog 收尾；
	// Notifier.Stop 支持 Stop-before-Start）。
	var serveErr error
	if err := srv.Listen(); err != nil {
		serveErr = err
	} else {
		notifier.Start(ctx)
		// HTTP 服务阻塞直到 ctx 取消（信号）或监听出错。
		serveErr = srv.Serve(ctx)
	}

	return shutdownRuntime(shutdownRuntimeArgs{
		notifier:   notifier,
		tm:         tm,
		bgStop:     bgStop,
		uploadStop: uploadOrch.Stop, // terminal-file-paste-drop 2.4：订阅/清理器收尾
		wd:         wd,
		serveErr:   serveErr,
	})
}

// runtimeStopper / runtimeShutdowner 关停窄端口（G1 测试注入；生产为
// *appnotification.Notifier / *task.Manager）。
type runtimeStopper interface{ Stop() }
type runtimeShutdowner interface {
	Shutdown(ctx context.Context) error
}

// shutdownRuntimeArgs 统一关停所需运行时句柄（G1：Listen 失败与 Serve 返回共用）。
type shutdownRuntimeArgs struct {
	notifier   runtimeStopper
	tm         runtimeShutdowner
	bgStop     func()
	uploadStop func() // terminal-file-paste-drop 2.4：上传编排器订阅/清理器收尾
	wd         *process.WatchdogManager
	serveErr   error
}

func shutdownRuntime(a shutdownRuntimeArgs) error {
	// 正常关停（design.md §10 顺序）：notifier.Stop 先于 tm.Shutdown（D11：
	// 不再发通知）；随后 quiesce/TaskManager shutdown——
	// kill 模式：先杀会话、确认空、再 StopWatchdog、退出（watchdog 不得先停）。
	// persist 模式：会话保留，下次启动 reconcile 恢复。
	// tm.Shutdown 内部已 join 后台周期 goroutine 并同步收尾残留 retryable debt（H），
	// 并停并 join 全部 runtime SSE/watch goroutine（G）。bgStop 为幂等兜底。
	// Shutdown 返回错误（kill 模式有残留或 DB retryable debt 未清）MUST NOT 停止 watchdog：
	// kill_immediate 下 watchdog 是 kill -9 窗口的最后兜底，runtime 未净就停它重新打开窗口。
	// 此时让进程退出但保留 watchdog 子进程存活，由其轮询到父亡后执行 kill-server。
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if a.notifier != nil {
		a.notifier.Stop()
	}
	var shutdownErr error
	if a.tm != nil {
		shutdownErr = a.tm.Shutdown(shutdownCtx)
	}
	if shutdownErr != nil {
		log.Printf("warning: taskmanager shutdown: %v (runtime not clean, keeping watchdog alive)", shutdownErr)
	}
	if a.bgStop != nil {
		a.bgStop()
	}
	// terminal-file-paste-drop 2.4：uploadStop 在 tm.Shutdown/bgStop 之后——
	// 关停序列期间的 task.deleted 删除事件仍被消费回收，随后才关闭订阅。
	if a.uploadStop != nil {
		a.uploadStop()
	}
	wd := a.wd
	if wd != nil {
		// 显式分支（design.md §10）：kill_immediate 下 watchdog 是 kill -9 窗口的最后兜底。
		//   - shutdownErr != nil（runtime 未净：残留会话/debt）→ MUST 保持 watchdog 运行，
		//     不得 wd.Stop()；进程退出后 watchdog 轮询父亡执行 kill-server 收割逃逸进程。
		//   - shutdownErr == nil（runtime 已净）→ 按关停顺序 StopWatchdog。
		// 该分支不依赖 defer 顺序隐式达成；此处显式判断让"runtime 未净则保留 watchdog 兜底"可见。
		if shutdownErr != nil {
			// runtime 未净：保留 watchdog 存活兜底，跳过 wd.Stop()。
			// 进程退出后 watchdog 轮询到父亡执行 kill-server。
			// 不在此 log（上方 shutdown warning 已记录）。
		} else {
			// runtime 已净：按关停顺序 StopWatchdog（design.md §10）。
			if stopErr := wd.Stop(); stopErr != nil {
				// Stop 失败记录，但不阻塞退出：runtime 已净，watchdog 残留子进程
				// 由其自身 ppid 轮询在父亡后自退（kill_immediate 兜底仍成立）。
				log.Printf("warning: stop watchdog: %v", stopErr)
			}
		}
	}
	// Shutdown 失败时返回该错误，使进程以非零状态退出（提示运维 runtime 未净、需下次启动 reconcile 兜底）。
	if shutdownErr != nil {
		return shutdownErr
	}
	return a.serveErr
}

// diffReviewStartupGate 启动收敛门禁（design.md D2 重启恢复① + F1/F12①）：
// 开放 API/调度器前执行 ConvergeDiffReviewOnStartup（单事务 sending→delivery_unknown），
// 收敛写库失败 → fail-closed 拒绝启动（MUST NOT 调用 open，不开放 API/调度器）；
// 收敛成功 → 调用 open 并透传其返回错误。
func diffReviewStartupGate(ctx context.Context, converger startupConverger, open func() error) error {
	if _, err := converger.ConvergeDiffReviewOnStartup(ctx); err != nil {
		return fmt.Errorf("converge diff review on startup: %w", err)
	}
	return open()
}

// startupConverger 收敛门禁依赖的最小端口（task.Manager 与 store.DiffReviewRepoAdapter 均满足）。
type startupConverger interface {
	ConvergeDiffReviewOnStartup(ctx context.Context) (int64, error)
}

// spawnWatchdog 构造并启动 watchdog 子进程（design.md §10）。
// 使用 os.Executable() 取当前可执行文件路径自调；TMUX_TMPDIR=<dataDir>/tmux
// 保证 watchdog kill-server 连接到 ocdeck 专属 socket。spawn 失败拒绝启动。
func spawnWatchdog(cfg *config.Config) (*process.WatchdogManager, error) {
	binaryPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	tmpdir, err := process.EnsureTmpDir(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	wd := process.NewWatchdogManager(
		binaryPath, cfg.DataDir, "ocdeck", tmpdir, process.DefaultBaseEnv(os.LookupEnv))
	if err := wd.Spawn(); err != nil {
		return nil, err
	}
	log.Printf("watchdog spawned (kill_immediate): socket=ocdeck tmpdir=%s", tmpdir)
	return wd, nil
}

// newUploadOrchestrator 构造受管上传编排器（terminal-file-paste-drop 2.4）：
// infrastructure 存储注入 StorePort，sqlite adapter 视图注入 TaskPort，bus 适配注入
// task 事件订阅，currentConnPort 注入 Conns 端口（deliver ①连接当前性判定）。
func newUploadOrchestrator(cfg *config.Config, repo application.TaskRepository, bus *eventbus.Bus, conns appuploads.ConnPort) *appuploads.Orchestrator {
	return appuploads.New(appuploads.Options{
		Cfg:    appuploads.Config{UploadDir: cfg.UploadDir, MaxBytes: cfg.UploadMaxBytes, Retention: cfg.UploadRetention},
		Store:  uploads.NewStore(cfg.UploadDir),
		Tasks:  uploadTaskViewPort{inner: repo},
		Conns:  conns,
		Events: uploadEventSubscriber{bus: bus},
	})
}

// currentConnSource 是组合根侧对 api 当前 TUI 连接查询的窄视图
// （*api.Server.CurrentTUIConnID 结构性满足；测试可注 fake）。
type currentConnSource interface {
	CurrentTUIConnID(taskID string) (string, bool)
}

// currentConnPort 将 api WS 注册表适配为 uploads.ConnPort。HTTP 服务构造晚于编排器，
// setServer 回填数据源；回填前（监听前无任何上传/投递流量）按未注册处理。
type currentConnPort struct {
	mu  sync.RWMutex
	src currentConnSource
}

func (p *currentConnPort) setServer(s currentConnSource) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.src = s
}

func (p *currentConnPort) CurrentTUIConn(ctx context.Context, taskID string) (string, bool, error) {
	p.mu.RLock()
	src := p.src
	p.mu.RUnlock()
	if src == nil {
		return "", false, nil
	}
	connID, ok := src.CurrentTUIConnID(taskID)
	return connID, ok, nil
}

// deliverOrchAdapter 将编排器 Deliver 的 (bool, DeliverErrorCode) 适配为 api 层
// 窄接口的 (bool, string)（api 不依赖 application 的 Deliver 签名）。
type deliverOrchAdapter struct {
	inner *appuploads.Orchestrator
}

func (a deliverOrchAdapter) Deliver(ctx context.Context, taskID, connID, uploadID string, inject func(ctx context.Context) error) (bool, string) {
	ok, code := a.inner.Deliver(ctx, taskID, connID, uploadID, inject)
	return ok, string(code)
}

// uploadTaskViewPort 将 application.TaskRepository 适配为 uploads.TaskPort
// （任务存在/活跃查询；sql.ErrNoRows 已被 adapter 归一化为 application.ErrTaskNotFound）。
type uploadTaskViewPort struct {
	inner application.TaskRepository
}

func (p uploadTaskViewPort) TaskActive(ctx context.Context, taskID string) (bool, bool, error) {
	t, err := p.inner.GetTask(ctx, taskID)
	if errors.Is(err, application.ErrTaskNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	// 任务状态准入表共享：仅 status==active 视为活跃。
	return true, t.Status() == ocdecktask.StatusActive, nil
}

// uploadEventSubscriber 将 eventbus.Bus 适配为 uploads.EventSubscriber
// （Bus.Subscribe 返回 *eventbus.Sub 具体类型，经接口适配，同 notification 模式；
// *eventbus.Sub 的 C/Overflow/Close 结构性满足 uploads.EventSubscription）。
type uploadEventSubscriber struct {
	bus *eventbus.Bus
}

func (s uploadEventSubscriber) Subscribe(topic event.Topic) appuploads.EventSubscription {
	return s.bus.Subscribe(topic)
}

// permJudgeAdapter 将 ai.PermJudge 适配为 task.PermissionJudge 端口
// （task-permission-mode D5；ai 包不 import internal/task，BranchNamer 同型约束，
// 端口适配在 composition root 完成）。
type permJudgeAdapter struct {
	inner *ai.PermJudge
}

func (a permJudgeAdapter) Judge(ctx context.Context, in task.PermissionJudgeInput) (task.PermissionVerdict, error) {
	// 全字段映射（task-permission-mode 8.2：漏映射会静默丢增强数据）。
	// Detail → Evidence 全字段逐项透传（含 Degraded 降级标记）。
	ev := ai.PermEvidence{Degraded: in.Detail.Degraded}
	files := make([]ai.PermFileChange, 0, len(in.Detail.Files))
	for _, f := range in.Detail.Files {
		files = append(files, ai.PermFileChange{
			Type: f.Type, RelativePath: f.RelativePath, Patch: f.Patch, MovePath: f.MovePath,
		})
	}
	ev.Files = files
	ev.Command = in.Detail.Command
	ev.Filepath = in.Detail.Filepath
	ev.Diff = in.Detail.Diff
	ev.URL = in.Detail.URL
	ev.Description = in.Detail.Description
	ev.SubagentType = in.Detail.SubagentType
	ev.Pattern = in.Detail.Pattern
	ev.Path = in.Detail.Path
	ev.Include = in.Detail.Include
	ev.ParentDir = in.Detail.ParentDir
	ev.Directories = in.Detail.Directories

	v, err := a.inner.Judge(ctx, ai.PermJudgeInput{
		Platform: ai.PermPlatformContext{
			TaskName:    in.TaskName,
			ProjectName: in.ProjectName,
			ProjectType: in.ProjectKind,
			TaskMode:    in.TaskMode,
			TaskDir:     in.TaskDir,
			ProjectDir:  in.ProjectDir,
			Branch:      in.Branch,
		},
		Request: ai.PermRequest{
			Permission: in.Permission,
			Patterns:   in.Patterns,
		},
		Evidence: ev,
	})
	return task.PermissionVerdict(v), err
}

// permAuditAdapter 将 auditlog.Logger 适配为 task.PermAuditLogger 端口
// （task-permission-mode D11；auditlog 不 import internal/task，组合根适配）。
// err 原样透传（消费方降级为普通日志）。
type permAuditAdapter struct {
	inner *auditlog.Logger
}

func (a permAuditAdapter) Record(rec task.PermAuditRecord) error {
	return a.inner.Record(auditlog.Entry{
		Time:        rec.Time,
		TaskID:      rec.TaskID,
		TaskName:    rec.TaskName,
		RequestID:   rec.RequestID,
		Permission:  rec.Permission,
		Patterns:    rec.Patterns,
		Verdict:     rec.Verdict,
		ReplyResult: rec.ReplyResult,
		Reply:       rec.Reply,
		Detail:      rec.Detail,
	})
}
