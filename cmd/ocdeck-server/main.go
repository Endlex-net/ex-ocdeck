// Command ocdeck-server 是 ocdeck 服务端入口（design.md §1/§10/§11）。
//
// 启动流程：加载配置 + 启动校验 → 打开 SQLite → 启动 HTTP 服务。
// shutdownPolicy=kill_immediate 时，在任何会话创建之前 SpawnWatchdog（design.md §10）。
// v1 仅 macOS/Darwin。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"ocdeck/internal/api"
	"ocdeck/internal/config"
	"ocdeck/internal/process"
	"ocdeck/internal/store"
	"ocdeck/internal/task"
	"ocdeck/internal/worktree"
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
	cfg, release, err := config.Load(config.DefaultOptions())
	if err != nil {
		return err
	}
	// release 持有 dataDir 单实例 flock（design.md §10）。MUST 在 store.Close() 之后释放：
	// defer 为 LIFO，故 db.Close()（后注册）先于 release()（先注册）执行，
	// 保证 flock 释放前 SQLite 已关闭、不会与下一实例的 store.Open 竞态。
	defer release()

	log.Printf("ocdeck-server starting: dataDir=%s listen=%s policy=%s opencode=%s tmux=%s",
		cfg.DataDir, cfg.ListenAddr, cfg.ShutdownPolicy, cfg.OpenCodeVersion, cfg.TmuxVersion)

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

	// TaskManager 构造（design.md §18）。
	adapter := task.NewStoreAdapter(db)
	tm := task.New(task.Options{
		Cfg:       cfg,
		Store:     adapter,
		Proc:      task.NewProcessAdapter(procMgr),
		Worktree:  task.NewWorktreeAdapter(wtMgr),
		DebtStore: adapter, // R7：orphan tickets 持久化跨重启恢复（design.md §10）
	})
	// 注入 Manager 生命周期 context（design.md §4：SSE/退出监视挂进程 ctx，非 HTTP request ctx）。
	tm.SetLifecycleCtx(ctx)

	// 启动 reconciliation（design.md §5/§10，HTTP 就绪前完成对账）。
	// Reconcile 失败 MUST 拒绝开放 HTTP（fail-closed）：状态不确定时开放管理面会让用户操作
	// 建立在错误状态上（会话/DB 失同步，后续操作可能破坏数据安全边界）。
	if err := tm.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	// 后台周期重试（design.md §5：30s 消化 retryable notice）。
	bgStop := tm.StartBackground(ctx)

	srv := api.New(cfg, db)
	srv.SetTaskBackend(tm)
	// 全局 oc 配置管理（design.md §13/§21）：~/.config/opencode/ 下 *.json/*.jsonc。
	ocCfgDir, ocCfgErr := config.DefaultOCConfigDir()
	if ocCfgErr == nil {
		srv.SetOCConfigService(config.NewOCConfigManager(ocCfgDir))
	} else {
		log.Printf("warning: oc-config dir unavailable: %v (oc-configs endpoints disabled)", ocCfgErr)
	}
	srv.RebuildRoutes()
	if wd != nil {
		srv.SetWatchdogStateProvider(wd.StateString)
	}
	// HTTP 服务阻塞直到 ctx 取消（信号）或监听出错。
	serveErr := srv.Start(ctx)

	// 正常关停（design.md §10 顺序）：quiesce/TaskManager shutdown——
	// kill 模式：先杀会话、确认空、再 StopWatchdog、退出（watchdog 不得先停）。
	// persist 模式：会话保留，下次启动 reconcile 恢复。
	// tm.Shutdown 内部已 join 后台周期 goroutine 并同步收尾残留 retryable debt（H），
	// 并停并 join 全部 runtime SSE/watch goroutine（G）。bgStop 为幂等兜底。
	// Shutdown 返回错误（kill 模式有残留或 DB retryable debt 未清）MUST NOT 停止 watchdog：
	// kill_immediate 下 watchdog 是 kill -9 窗口的最后兜底，runtime 未净就停它重新打开窗口。
	// 此时让进程退出但保留 watchdog 子进程存活，由其轮询到父亡后执行 kill-server。
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	shutdownErr := tm.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		log.Printf("warning: taskmanager shutdown: %v (runtime not clean, keeping watchdog alive)", shutdownErr)
	}
	bgStop()
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
	return serveErr
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