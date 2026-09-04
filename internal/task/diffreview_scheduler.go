// diffreview_scheduler.go 实现 task 层调度器生命周期接线（design.md D2 调度器 + 3.8 重启恢复）。
//
// 归属（D2/D9）：Manager 内每任务一个调度循环，随 runtime 启动、Shutdown join。
// 本文件实现：
//   - SchedulerCallbacks（LockTask = tryLockTask，与生命周期操作互斥）。
//   - 调度器随 active 提交启动、随 clearRuntime 停止（F3 三个真实启动点：
//     activation 的 commitRuntimeReady CAS 后、reconcile 提交 active 后、suspend 修复
//     回 active CAS 后；setRuntime 不再是启动点，避免首探抢跑非 active task）。
//   - Manager Shutdown join 全部调度器 goroutine。
//   - 3.8 重启恢复：服务启动收敛（ConvergeOnStartup）+ runtime 启动恢复本任务 queued。
//
// diffreview.Service 由外部注入（SetDiffReviewService），避免 Manager 直接依赖 store.Queries
//（TaskStore 接口不含 diff_review 原语；Service + 5 ports 由 main/组装层构造后注入）。
package task

import (
	"context"
	"log"
	"sync"

	"ocdeck/internal/application/diffreview"
)

// schedulerHandle 为单任务调度器 goroutine 的控制句柄。
type schedulerHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// schedulerController 管理 per-task 调度器 goroutine 的启动/停止。
// 以 *Manager 为 key 隔离（与 capRegistry 同模式，避免侵入 Manager 结构体）。
type schedulerController struct {
	mu       sync.Mutex
	handles  map[string]*schedulerHandle // taskID → 调度器句柄
	wg       sync.WaitGroup              // 全部调度器 goroutine join（Shutdown 用）
}

// schedControllers 以 *Manager 为 key 存放 per-Manager 调度器控制器（懒初始化）。
var schedControllers sync.Map

// getSchedController 取（或懒初始化）某 Manager 的调度器控制器。
func (m *Manager) getSchedController() *schedulerController {
	if v, ok := schedControllers.Load(m); ok {
		return v.(*schedulerController)
	}
	sc := &schedulerController{handles: map[string]*schedulerHandle{}}
	v, _ := schedControllers.LoadOrStore(m, sc)
	return v.(*schedulerController)
}

// diffReviewServiceField 为 Manager 的 diffreview.Service 注入位（避免改 Manager 结构体定义）。
// 以 *Manager 为 key 存放（与 capRegistry/schedController 同模式）。
var diffReviewServices sync.Map

// SetDiffReviewService 注入 diffreview.Service（供调度器使用）。由 main/组装层在 Manager 构造后调用。
// 未注入时调度器不启动（StartDiffReviewScheduler 为 no-op）。
func (m *Manager) SetDiffReviewService(svc *diffreview.Service) {
	diffReviewServices.Store(m, svc)
}

// getDiffReviewService 取注入的 diffreview.Service（nil = 未注入）。
func (m *Manager) getDiffReviewService() *diffreview.Service {
	if v, ok := diffReviewServices.Load(m); ok {
		return v.(*diffreview.Service)
	}
	return nil
}

// schedulerCallbacks 实现 diffreview.SchedulerCallbacks（LockTask = tryLockTask）。
type schedulerCallbacks struct {
	m *Manager
}

// LockTask 获取任务锁（design.md D2：调度与生命周期操作经 tryLockTask 互斥）。
// 冲突返回 error（调度器让出本轮）。
func (c *schedulerCallbacks) LockTask(ctx context.Context, taskID string) (func(), error) {
	return c.m.tryLockTask(taskID)
}

// StartDiffReviewSchedulerForTask 为指定任务启动调度器 goroutine（design.md D2：随 runtime 启动）。
// 幂等：已有调度器在运行则 no-op。未注入 diffreview.Service 则 no-op。
// 由 commitRuntimeReady（active CAS 提交后）、reconcile（提交 active 后）与 suspend 修复
// （回 active CAS 后）调用（F3/F14：MUST NOT 在 ready 提交前启动——提前启动会让 eager 首探
// 抢跑，taskOcClient 拒绝非 active task）。构造调度器时捕获当前 runtime 的 instVersion
// （能力缓存 fencing 用；runtime 替换时本调度器随 clearRuntime 停止，新调度器以新版本构造）。
func (m *Manager) StartDiffReviewSchedulerForTask(ctx context.Context, taskID string) {
	svc := m.getDiffReviewService()
	if svc == nil {
		return
	}
	sc := m.getSchedController()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if _, running := sc.handles[taskID]; running {
		return
	}
	// 捕获当前 runtime instVersion（F3：响应后 fencing 直接使用该版本，不临时读 DB）。
	instVer := ""
	if rt := m.getRuntime(taskID); rt != nil {
		instVer = string(rt.instVersion)
	}
	schedCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	sched := diffreview.NewScheduler(diffreview.SchedulerOptions{
		Service:     svc,
		Callbacks:   &schedulerCallbacks{m: m},
		TaskID:      taskID,
		InstVersion: instVer,
	})
	sc.wg.Add(1)
	go func() {
		defer sc.wg.Done()
		defer close(done)
		sched.Run(schedCtx)
	}()
	sc.handles[taskID] = &schedulerHandle{cancel: cancel, done: done}

	// 3.8 重启恢复②：runtime 启动恢复本任务 queued（确认队列可被接管）。
	if _, err := svc.RecoverQueuedForTask(ctx, taskID); err != nil {
		log.Printf("diffreview: recover queued for task %s: %v", taskID, err)
	}

	// F2/D1 事件模型①：runtime ready 时 eager 首探一次（空队列也探测，避免依赖调度 tick）。
	// 失败不阻断 runtime 启动；后续准入/列表遇 absent/unknown 仍会复探。
	go func() {
		if err := svc.ProbeFirst(ctx, taskID); err != nil {
			log.Printf("diffreview: first capability probe for task %s: %v", taskID, err)
		}
	}()
}

// StopDiffReviewSchedulerForTask 停止指定任务的调度器 goroutine 并 join（design.md D2：随 runtime 停止）。
// 幂等：无运行中调度器则 no-op。由 clearRuntime 路径调用。
func (m *Manager) StopDiffReviewSchedulerForTask(taskID string) {
	sc := m.getSchedController()
	sc.mu.Lock()
	h, ok := sc.handles[taskID]
	if !ok {
		sc.mu.Unlock()
		return
	}
	delete(sc.handles, taskID)
	sc.mu.Unlock()
	h.cancel()
	<-h.done
}

// JoinAllDiffReviewSchedulers join 全部调度器 goroutine（design.md D2：Shutdown join）。
// 由 Manager.Shutdown 在 stopAndJoinAllRuntimes 前调用。
func (m *Manager) JoinAllDiffReviewSchedulers() {
	sc := m.getSchedController()
	sc.mu.Lock()
	handles := make([]*schedulerHandle, 0, len(sc.handles))
	for taskID, h := range sc.handles {
		handles = append(handles, h)
		delete(sc.handles, taskID)
	}
	sc.mu.Unlock()
	for _, h := range handles {
		h.cancel()
		<-h.done
	}
	sc.wg.Wait()
}

// ConvergeDiffReviewOnStartup 服务启动全局收敛（design.md D2 重启恢复① + 3.8）。
// 调 diffreview.Service.ConvergeOnStartup：单事务 sending→delivery_unknown + 固定 error。
// 写库失败 fail-closed：返回 error，调用方（server 启动序列）MUST 不开放 API/调度器。
// 未注入 diffreview.Service → no-op（无 diff review 能力，不阻断启动）。
func (m *Manager) ConvergeDiffReviewOnStartup(ctx context.Context) (int64, error) {
	svc := m.getDiffReviewService()
	if svc == nil {
		return 0, nil
	}
	return svc.ConvergeOnStartup(ctx)
}