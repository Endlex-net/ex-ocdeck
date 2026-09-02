// scheduler.go 实现 3.7 队列调度器（design.md D2 调度器/能力门禁/发送/状态映射）。
//
// 调度器归属（D2/D9）：Manager 内每任务一个调度循环，随 runtime 启动、Shutdown join。
// 本文件实现 application 层调度逻辑（单任务循环），task 层 Manager 负责：
//   - 调度循环的启动/停止（随 runtime 生命周期）。
//   - tryLockTask 与生命周期操作互斥。
//   - RuntimePort/PromptPort/DiffSourcePort adapter 注入。
//
// 调度逻辑（D2 唯一）：
//   - 队列非空时 2s 间隔轮询 SessionStatus（idle 或缺席→可投递；busy/retry→等待；查询失败→保持 queued）。
//   - 按最小 seq 串行取队首。
//   - 能力门禁（CAS 前三分支）：supported→继续；缓存已 unsupported→直接 queued→failed；
//     unknown/absent→singleflight 复探（supported→继续/unsupported→failed/仍 unknown→保持 queued）。
//   - CAS 抢占 queued→sending（失败让出）。
//   - 发送：PromptOutcome 状态映射（accepted→sent 清理事务；http_response 按 D1 矩阵；
//     transport_unknown→delivery_unknown；pre_send_failure→准备重试 ≤3 耗尽 failed）。
//   - adapter 获取失败=pre_send_failure 固定 Detail "runtime client unavailable"。
package diffreview

import (
	"context"
	"strconv"
	"time"
)

// schedulerPollInterval 轮询间隔（design.md D2：2s）。
const schedulerPollInterval = 2 * time.Second

// maxPreSendRetries 准备重试上限（design.md D2：≤3 耗尽 failed）。
const maxPreSendRetries = 3

// SchedulerCallbacks 为调度器所需的 task 层协调回调（由 task 层注入，避免 application 反向依赖 task）。
//
// LockTask 获取任务锁（与生命周期操作互斥，tryLockTask 约定）；返回 unlock 函数或 lockErr。
// 调度器在锁内执行 CAS + 发送（design.md D2：调度与 Suspend/Delete 等经任务锁互斥）。
type SchedulerCallbacks interface {
	// LockTask 尝试获取任务锁。成功返回 unlock 函数；冲突返回 error（调度器让出本轮）。
	LockTask(ctx context.Context, taskID string) (unlock func(), err error)
}

// SchedulerOptions 构造调度器。
type SchedulerOptions struct {
	Service     *Service
	Callbacks   SchedulerCallbacks
	TaskID      string
	InstVersion string // 当前 runtime instVersion（能力缓存 fencing 用）
}

// Scheduler 为单任务调度循环（design.md D2）。
type Scheduler struct {
	svc    *Service
	cb     SchedulerCallbacks
	taskID string
}

// NewScheduler 构造单任务调度器。
func NewScheduler(opts SchedulerOptions) *Scheduler {
	return &Scheduler{
		svc:    opts.Service,
		cb:     opts.Callbacks,
		taskID: opts.TaskID,
	}
}

// Run 启动调度循环（阻塞，直到 ctx 取消）。由 task 层在 goroutine 中调用。
// 每轮：取队首→能力门禁→CAS→发送→状态映射；队列空时等待轮询间隔。
func (sch *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(schedulerPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sch.tick(ctx)
		}
	}
}

// tick 执行单轮调度：取队首→能力门禁→CAS→发送。
func (sch *Scheduler) tick(ctx context.Context) {
	unlock, err := sch.cb.LockTask(ctx, sch.taskID)
	if err != nil {
		// 任务锁冲突 → 让出本轮（design.md D2：CAS 抢占与生命周期操作任务锁互斥）。
		return
	}
	defer unlock()

	subs, err := sch.svc.repo.ListDiffReviewQueue(ctx, sch.taskID)
	if err != nil {
		return
	}
	if len(subs) == 0 {
		return
	}
	// 按最小 seq 串行取队首（ListDiffReviewQueue 已按 seq ASC 返回）。
	head := subs[0]
	sch.processHead(ctx, head)
}

// processHead 处理队首提交：能力门禁→CAS→发送→状态映射。
func (sch *Scheduler) processHead(ctx context.Context, sub DiffReviewSubmissionRecord) {
	if sub.Status != "queued" {
		// sending 行（重启恢复前残留或并发）跳过——重启收敛已处理，正常态 sending 由本调度器持有。
		return
	}

	// 能力门禁（D2：CAS 前三分支）。
	st, err := sch.svc.rt.ProbeCapability(ctx, sch.taskID)
	if err != nil {
		return
	}
	switch st {
	case CapabilitySupported:
		// 继续 CAS。
	case CapabilityUnsupported:
		// 缓存已 unsupported → 直接 queued→failed，MUST NOT 复探/进 sending/调 PromptAsync。
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "queued", "failed", "capability unsupported: prompt_async")
		return
	case CapabilityAbsent:
		// 无运行时 → 保持 queued（runtime 启动后恢复）。
		return
	case CapabilityUnknown:
		// unknown → ProbeCapability 已复探（singleflight），仍 unknown → 保持 queued 下轮再试。
		return
	}

	// SessionStatus 门禁（D2：idle 或缺席→可投递；busy/retry→等待；查询失败→保持 queued）。
	status, err := sch.svc.rt.SessionStatus(ctx, sch.taskID, sub.TargetSessionID)
	if err != nil {
		// 查询失败 → 保持 queued 下轮重试（无发送副作用）。
		return
	}
	if status != SessionStatusIdle {
		// busy/retry → 等待。
		return
	}

	// CAS 抢占 queued→sending。
	matched, err := sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "queued", "sending", "")
	if err != nil || !matched {
		// CAS 失败（并发抢占或状态已变）→ 让出。
		return
	}

	// 发送 + 状态映射。
	sch.sendAndMap(ctx, sub)
}

// sendAndMap 发送 PromptAsync 并按 PromptOutcome 状态映射（design.md D1 错误矩阵 + D2 发送）。
func (sch *Scheduler) sendAndMap(ctx context.Context, sub DiffReviewSubmissionRecord) {
	// 准备重试 ≤3（pre_send_failure 耗尽 failed）。
	var outcome PromptOutcome
	for attempt := 0; attempt < maxPreSendRetries; attempt++ {
		outcome = sch.svc.prompt.PromptAsync(ctx, sub.TaskID, sub.TargetSessionID, sub.MessageID, sub.Payload)
		if outcome.Kind != PromptOutcomePreSendFailure {
			break
		}
		// pre_send_failure → 重试（MUST NOT 标 delivery_unknown）。
	}
	if outcome.Kind == PromptOutcomePreSendFailure {
		// 耗尽 → failed（error 记录 Detail）。
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "failed", outcome.Detail)
		return
	}

	sch.mapOutcome(ctx, sub, outcome)
}

// mapOutcome 按 D1 错误矩阵映射 PromptOutcome → 状态转移。
func (sch *Scheduler) mapOutcome(ctx context.Context, sub DiffReviewSubmissionRecord, outcome PromptOutcome) {
	switch outcome.Kind {
	case PromptOutcomeAccepted:
		// 204 → sent 清理事务（sending→sent + 按 id+revision 删批注）。
		// 事务失败 → delivery_unknown（agent 已收，绝不重发，D2）。
		matched, err := sch.svc.repo.CompleteDiffReviewSentCleanup(ctx, sub.ID)
		if err != nil || !matched {
			// sent 本地事务失败 → delivery_unknown（D2）。
			sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "delivery_unknown", "delivery unknown: sent cleanup failed")
		}
	case PromptOutcomeHTTPResponse:
		sch.mapHTTPResponse(ctx, sub, outcome)
	case PromptOutcomeTransportUnknown:
		// 网络错误/超时/断连 → delivery_unknown（MUST NOT 自动重投，D1）。
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "delivery_unknown", "delivery unknown: "+outcome.Detail)
	}
}

// mapHTTPResponse 按 D1 错误矩阵映射 http_response（意外 2xx/400/401/404/其他非 2xx）。
func (sch *Scheduler) mapHTTPResponse(ctx context.Context, sub DiffReviewSubmissionRecord, outcome PromptOutcome) {
	code := outcome.StatusCode
	switch {
	case code == 400:
		// 400 → failed（error 记录响应体）+ 能力复核（置 unknown 复探，D1 事件模型④）。
		// F16：先持久化终态 CAS，再触发 Probe（probe 只更新缓存，最高阻塞 5s 不应阻塞终态落库；
		// 复探期间 ctx 取消时终态已落库，不会残留 sending 被启动收敛误转 delivery_unknown）。
		sch.svc.rt.InvalidateCapability(ctx, sub.TaskID, sch.currentInstVersion())
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "failed", truncateErrorMessage(outcome.Body, 400))
		_, _ = sch.svc.rt.ProbeCapability(ctx, sub.TaskID)
	case code == 401:
		// 401 → failed，MUST NOT 重试（D1）。
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "failed", "unauthorized: "+truncateErrorMessage(outcome.Body, 400))
	case code == 404:
		// 404 → GetSession 穷尽分流（D1）。
		sch.map404(ctx, sub, outcome)
	case code >= 200 && code < 300:
		// 意外 2xx（200/201/202）→ delivery_unknown + 能力置 unknown 复探（D1 事件模型④）。
		// F16：先持久化终态 CAS，再触发 Probe（终态落库优先于复探）。
		sch.svc.rt.InvalidateCapability(ctx, sub.TaskID, sch.currentInstVersion())
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "delivery_unknown", "delivery unknown: unexpected 2xx "+itoa(code))
		_, _ = sch.svc.rt.ProbeCapability(ctx, sub.TaskID)
	default:
		// 其他非 2xx → failed，error 记录状态码与响应体。
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "failed", "http "+itoa(code)+": "+truncateErrorMessage(outcome.Body, 400))
	}
}

// map404 执行 404→GetSession 穷尽分流（design.md D1 错误矩阵 404 行）。
// POST 404 即请求未执行，MUST NOT 标 delivery_unknown、MUST NOT 自动重投。
// 经 RuntimePort.GetSession 结构化结果穷尽三分支：
//   - found（GET 200 会话存在）→ 端点不支持 prompt_async：能力转 unsupported（零重投）、
//     sending→failed error 固定 "capability unsupported: prompt_async"。
//   - missing（GET 404 会话明确不存在）→ sending→failed error "invalid_state: target session not found"。
//   - unknown（其他状态码/网络错误/解码失败）→ sending→failed + 能力置 unknown 触发复探。
func (sch *Scheduler) map404(ctx context.Context, sub DiffReviewSubmissionRecord, outcome PromptOutcome) {
	res, _ := sch.svc.rt.GetSession(ctx, sub.TaskID, sub.TargetSessionID)
	switch res.Status {
	case GetSessionFound:
		// 会话存在 → 端点不支持：能力转 unsupported（零重投）+ failed 固定 error。
		sch.svc.rt.SetCapabilityUnsupported(ctx, sub.TaskID, sch.currentInstVersion())
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "failed", "capability unsupported: prompt_async")
	case GetSessionMissing:
		// 会话明确不存在 → failed invalid_state。
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "failed", "invalid_state: target session not found")
	default:
		// 其余一切（未知）→ failed + 能力置 unknown 触发复探（D1）。
		sch.svc.rt.InvalidateCapability(ctx, sub.TaskID, sch.currentInstVersion())
		errMsg := "404 probe unknown"
		if res.Detail != "" {
			errMsg = "404 probe unknown: " + res.Detail
		}
		sch.svc.repo.CASDiffReviewSubmission(ctx, sub.ID, "sending", "failed", truncateErrorMessage(errMsg, 400))
	}
}

// currentInstVersion 返回当前 runtime instVersion（能力缓存 fencing 用）。
// 调度器构造时未持久化 instVersion（runtime 可能替换），每次从 Snapshot 取。
func (sch *Scheduler) currentInstVersion() string {
	snap, err := sch.svc.rt.Snapshot(context.Background(), sch.taskID)
	if err != nil || !snap.HasRuntime {
		return ""
	}
	return snap.InstVersion
}

// truncateErrorMessage 截断错误消息到 maxBytes 字节的 rune-safe 前缀。
func truncateErrorMessage(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return runeSafePrefix(s, maxBytes)
}

// itoa 整数转字符串。
func itoa(n int) string {
	return strconv.Itoa(n)
}
