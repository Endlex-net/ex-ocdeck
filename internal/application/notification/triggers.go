package notification

import (
	"context"
	"log"
	"sort"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/domain/notification"
)

// --- 事件应用与计时届满判定（仅 run loop / 测试串行调用；design D3 状态机） ---

// onAttentionChanged attention 快照变化（spec「通知触发——等待回答问题/等待权限
// 批准」）：单次组合快照 diff——新增 pending 触发（去重键（类型, request ID）
// 独立）、了结移除去重条目（上界约束）、任意 pending 出现取消 idle 计时；
// 读取失败按无变化处理 MUST NOT 误发。fencing（B3）：快照实例与事件 RID 不一致
// （旧 runtime 迟到事件）时丢弃。
func (n *Notifier) onAttentionChanged(ctx context.Context, taskID, instVersion string) {
	snap, err := n.readSnapshot(ctx, taskID)
	if err != nil {
		log.Printf("notify: task %s: attention snapshot read failed, treat as no change: %v", taskID, err)
		return
	}
	if snap.InstVersion != instVersion {
		return
	}
	st := n.stateFor(taskID, snap.InstVersion, snap)
	if st == nil {
		return
	}
	if len(snap.Attention.Questions) > 0 || len(snap.Attention.Permissions) > 0 {
		st.idleSince = nil // 出现任意 pending：idle 计时取消且本周期不再触发
	}
	// 了结移除：去重集合以当前 pending 集合为上界，不得无限增长。
	pruneDedup(st.notifiedQuestions, snap.Attention.Questions, func(pq application.PendingQuestion) string { return pq.ID })
	pruneDedup(st.notifiedPermissions, snap.Attention.Permissions, func(pp application.PendingPermission) string { return pp.ID })
	// waiting 条目随 pending 消失剪枝（task-permission-mode D1）。
	pruneDedup(st.waitingPerms, snap.Attention.Permissions, func(pp application.PendingPermission) string { return pp.ID })

	// 新增触发：先记去重（触发条件按已消费处理——无论门禁结果），再门禁投递；
	// 门禁复验即本快照（单次组合读取）。B6：每个 pending request 各读一次配置
	// 快照（前一候选判定期间的 PUT 即时约束下一候选）。
	for _, pq := range snap.Attention.Questions {
		if _, done := st.notifiedQuestions[pq.ID]; done {
			continue
		}
		st.notifiedQuestions[pq.ID] = struct{}{}
		cfg := n.opts.Cfg.Config()
		if e := n.evaluate(taskID, notification.CategoryQuestion, cfg, snap, func(s TaskSnapshot) bool {
			return findPendingQuestion(s, pq.ID) != nil
		}); e.stage == gatePass {
			n.dispatch(ctx, e.plan, questionIntent(snap, pq, e.plan.URL))
		}
	}
	for _, pp := range snap.Attention.Permissions {
		if _, done := st.notifiedPermissions[pp.ID]; done {
			continue
		}
		if snap.aiAutoEffective() {
			// ai-auto 延迟触发（task-permission-mode D1）：首次观察不写去重，登记
			// waiting 等待判定结论；重复观察 MUST NOT 延长 deadline。
			if _, waiting := st.waitingPerms[pp.ID]; !waiting {
				st.waitingPerms[pp.ID] = n.opts.Now().Add(permissionVerdictWindow)
			}
			continue
		}
		st.notifiedPermissions[pp.ID] = struct{}{}
		cfg := n.opts.Cfg.Config()
		if e := n.evaluate(taskID, notification.CategoryPermission, cfg, snap, func(s TaskSnapshot) bool {
			return findPendingPermission(s, pp.ID) != nil
		}); e.stage == gatePass {
			n.dispatch(ctx, e.plan, permissionIntent(snap, pp, e.plan.URL))
		}
	}
}

// --- ai-auto 延迟触发（task-permission-mode D1/D7，tasks 3.3-3.5） ---
//
// onPermissionVerdict / onPermissionModeChanged 为两类唤醒事件（事件仅唤醒、
// 不信任载荷）：重读权威快照并重评该任务全部 waiting 条目。fencing（B3）与
// attention 同构：快照实例与事件 RID 不一致丢弃；读取失败保守保留现状
//（MUST NOT 误发，条目待后续事件/tick 重评）。

// onPermissionVerdict 判定结论唤醒（serve_runtime.permission_verdict）。
func (n *Notifier) onPermissionVerdict(ctx context.Context, taskID, instVersion string) {
	snap, err := n.readSnapshot(ctx, taskID)
	if err != nil {
		log.Printf("notify: task %s: verdict snapshot read failed, keep waiting: %v", taskID, err)
		return
	}
	if snap.InstVersion != instVersion {
		return // 旧 runtime 迟到唤醒
	}
	st := n.stateFor(taskID, snap.InstVersion, snap)
	if st == nil {
		return
	}
	n.reevaluateWaitingWithSnapshot(ctx, taskID, st, snap, n.opts.Now())
}

// onPermissionModeChanged 模式变更唤醒（serve_runtime.permission_mode_changed，
// D7：waiting 重评估只由此事件驱动）：有效模式仍 ai-auto 时按快照判定状态立即
// 重评；已非 ai-auto 时全部 waiting 条目按非 ai-auto 语义立即处理（仍 pending 且
// 未通知 → 正常门禁投递）。
func (n *Notifier) onPermissionModeChanged(ctx context.Context, taskID, instVersion string) {
	snap, err := n.readSnapshot(ctx, taskID)
	if err != nil {
		log.Printf("notify: task %s: mode-changed snapshot read failed, keep waiting: %v", taskID, err)
		return
	}
	if snap.InstVersion != instVersion {
		return
	}
	st := n.stateFor(taskID, snap.InstVersion, snap)
	if st == nil {
		return
	}
	n.reevaluateWaitingWithSnapshot(ctx, taskID, st, snap, n.opts.Now())
}

// reevaluateWaiting 事件唤醒后的全量 waiting 重评（重读快照定论）。
func (n *Notifier) reevaluateWaiting(ctx context.Context, taskID string, st *taskState) {
	snap, err := n.readSnapshot(ctx, taskID)
	if err != nil {
		log.Printf("notify: task %s: waiting re-eval snapshot read failed, keep waiting: %v", taskID, err)
		return
	}
	if snap.InstVersion != st.instVersion {
		return
	}
	n.reevaluateWaitingWithSnapshot(ctx, taskID, st, snap, n.opts.Now())
}

// reevaluateWaitingWithSnapshot 按给定快照重评全部 waiting 条目（D1 分支表）：
// 请求消失或 settled_no_notify → 清 waiting 不通知；manual_required 或（有效模式
// 已非 ai-auto）→ 立即按正常门禁投递（不等 deadline）；未定论（inflight/无状态）
// 仅 deadline 到期兜底，未到期继续等待（重复观察不延长）。
func (n *Notifier) reevaluateWaitingWithSnapshot(ctx context.Context, taskID string, st *taskState, snap TaskSnapshot, now time.Time) {
	for id, deadline := range st.waitingPerms {
		pp := findPendingPermission(snap, id)
		rec, hasState := snap.Verdicts[id]
		switch {
		case pp == nil:
			delete(st.waitingPerms, id) // 请求消失：清 waiting 不通知
		case hasState && rec.State == VerdictSettledNoNotify:
			delete(st.waitingPerms, id) // 已自动放行：清 waiting 不通知（即使 attention 瞬时仍 pending）
		case !snap.aiAutoEffective():
			n.fireWaitingPermission(ctx, taskID, st, snap, *pp) // 非 ai-auto：立即正常门禁投递
		case hasState && rec.State == VerdictManualRequired:
			n.fireWaitingPermission(ctx, taskID, st, snap, *pp) // 转人工：立即通知（不等 deadline）
		case !now.Before(deadline):
			n.fireWaitingPermission(ctx, taskID, st, snap, *pp) // 未定论到期：兜底通知
		default:
			// 未定论未到期：继续等待。
		}
	}
}

// fireWaitingPermission waiting 条目定论为需通知时的投递（与直接触发同一记账
// 语义：先记去重再门禁投递，触发条件按已消费处理）。发送前复核：本快照仍
// pending、未通知、非 settled_no_notify（evaluate 条件统一复验）。
func (n *Notifier) fireWaitingPermission(ctx context.Context, taskID string, st *taskState, snap TaskSnapshot, pp application.PendingPermission) {
	delete(st.waitingPerms, pp.ID)
	if _, done := st.notifiedPermissions[pp.ID]; done {
		return
	}
	st.notifiedPermissions[pp.ID] = struct{}{}
	cfg := n.opts.Cfg.Config()
	if e := n.evaluate(taskID, notification.CategoryPermission, cfg, snap, func(s TaskSnapshot) bool {
		if findPendingPermission(s, pp.ID) == nil {
			return false // 仍 pending
		}
		if rec, ok := s.Verdicts[pp.ID]; ok && rec.State == VerdictSettledNoNotify {
			return false // 未被自动放行
		}
		return true
	}); e.stage == gatePass {
		n.dispatch(ctx, e.plan, permissionIntent(snap, pp, e.plan.URL))
	}
}

// pruneDedup 移除已从 pending 集合消失的条目（了结；V 为 struct{} 去重集合或
// time.Time waiting deadline）。
func pruneDedup[T any, V any](dedup map[string]V, pending []T, id func(T) string) {
	pendingIDs := make(map[string]struct{}, len(pending))
	for _, p := range pending {
		pendingIDs[id(p)] = struct{}{}
	}
	for id := range dedup {
		if _, still := pendingIDs[id]; !still {
			delete(dedup, id)
		}
	}
}

func findPendingQuestion(s TaskSnapshot, requestID string) *application.PendingQuestion {
	for i := range s.Attention.Questions {
		if s.Attention.Questions[i].ID == requestID {
			return &s.Attention.Questions[i]
		}
	}
	return nil
}

func findPendingPermission(s TaskSnapshot, requestID string) *application.PendingPermission {
	for i := range s.Attention.Permissions {
		if s.Attention.Permissions[i].ID == requestID {
			return &s.Attention.Permissions[i]
		}
	}
	return nil
}

// onRunStatusChanged 聚合状态迁移（spec idle/retry requirement + design D3
// 状态机）：idle 仅由 available=true 且 from=busy 武装；retry 开 episode +
// 60s 计时，离开 retry（任何目的地）计时取消（触发条件失效）；busy 结束
// episode（名额释放，errorSeen 复位）；回到非 idle/不可用取消 idle；
// from 为空串或 retry 的迁移 MUST NOT 武装 idle。fencing（B3）：以组合快照
// 校验事件所属实例，旧 runtime 迟到事件丢弃；快照读取失败同样丢弃（保守不武装）。
func (n *Notifier) onRunStatusChanged(ctx context.Context, p ocdeckevent.ServeRuntimeRunStatusChangedPayload, instVersion string) {
	snap, err := n.readSnapshot(ctx, p.TaskID)
	if err != nil {
		log.Printf("notify: task %s: run_status snapshot read failed, event dropped: %v", p.TaskID, err)
		return
	}
	if snap.InstVersion != instVersion {
		return
	}
	st := n.stateFor(p.TaskID, snap.InstVersion, snap)
	if st == nil {
		return
	}
	now := n.opts.Now()
	if p.To != runStatusIdle {
		st.idleSince = nil // 回到非 idle / 不可用：idle 计时取消
	}
	if p.To != runStatusRetry {
		st.retryDeadline = nil // 离开 retry：该类别计时取消（episode 是否存续见仲裁表）
	}
	switch p.To {
	case runStatusBusy:
		// 聚合回到 busy：episode 结束（retry/error 已恢复）、名额释放、计时取消。
		st.endEpisode()
	case runStatusRetry:
		st.startEpisode()
		dl := now.Add(retryErrorWindow)
		st.retryDeadline = &dl
	case runStatusIdle:
		if p.Available && p.From == runStatusBusy {
			t := now
			st.idleSince = &t
			st.idleSuppressed = false // 新的满足武装条件的迁移：抑制态复位
		}
	}
}

// onSessionError 观察 session.error（spec「通知触发——错误未恢复」）：聚合已
// busy 为瞬时错误不打开计时；否则开启 episode + 60s error 计时。B1：episode
// 内首个 error 只武装一次（errorSeen），重复 error 仅更新 lastError——即使
// 计时已被消费且未占名额也不重新武装，episode 结束（回 busy）复位。
// fencing（B3）：快照实例与事件 RID 不一致时丢弃；快照读取失败丢弃
// （MUST NOT 误发）。
func (n *Notifier) onSessionError(ctx context.Context, p ocdeckevent.ServeRuntimeSessionErrorPayload, instVersion string) {
	snap, err := n.readSnapshot(ctx, p.TaskID)
	if err != nil {
		log.Printf("notify: task %s: session.error snapshot read failed, event dropped: %v", p.TaskID, err)
		return
	}
	if snap.InstVersion != instVersion {
		return
	}
	if snap.RunStatus == runStatusBusy {
		return // 聚合已 busy 的瞬时错误
	}
	st := n.stateFor(p.TaskID, snap.InstVersion, snap)
	if st == nil {
		return
	}
	st.lastError = p
	st.startEpisode()
	if !st.errorSeen {
		st.errorSeen = true
		dl := n.opts.Now().Add(retryErrorWindow)
		st.errorDeadline = &dl
	}
}

// scan 单次判定周期（10s tick；spec「通知抑制、启动基线与对账」单 run loop
// 串行）：error 优先于 retry（同 tick 两计时届满时，spec retry requirement）；
// idle 按当前配置阈值判定（热更新：缩短立即到期、延长顺延）。
func (n *Notifier) scan(ctx context.Context) {
	if n.mode != modeRunning {
		if n.mode == modeReconciling {
			n.attemptReconcile(ctx) // 对账中：周期重试，抑制全部发送
		}
		return
	}
	now := n.opts.Now()
	for _, id := range n.sortedTaskIDs() {
		st := n.states[id]
		if st.errorDeadline != nil && !now.Before(*st.errorDeadline) {
			st.errorDeadline = nil // 计时消费（无论判定结果）
			if !st.episodeConsumed {
				n.fireError(ctx, id, st)
			}
		}
		if st.retryDeadline != nil && !now.Before(*st.retryDeadline) {
			st.retryDeadline = nil
			if !st.episodeConsumed {
				n.fireRetry(ctx, id, st)
			}
		}
		if st.idleSince != nil && !st.idleSuppressed {
			// B6：该 idle 候选的判定快照——阈值判定、门禁与内容组装同源一份。
			cfg := n.opts.Cfg.Config()
			if now.Sub(*st.idleSince) >= time.Duration(cfg.IdleTimeoutSeconds)*time.Second {
				st.idleSince = nil
				st.idleSuppressed = true // 本周期以消费结束
				n.fireIdle(ctx, id, st, cfg)
			}
		}
		if len(st.waitingPerms) > 0 {
			// ai-auto waiting deadline 到期检查（task-permission-mode D1）：有等待
			// 条目才读快照；重评函数内按快照判定状态定论（manual_required 不等
			// deadline、settled/消失清除、未定论到期兜底）。
			n.reevaluateWaiting(ctx, id, st)
		}
	}
}

// fireError error 届满投递：触发条件 = 任务 active 且未恢复（聚合回到 busy 之外
// 均视为未恢复——含转 idle/不可用，spec「不可重试错误终止后仍触发」）；名额按
// 仲裁表（URL/渠道失败与已投递占名额，条件失效/开关关闭不占）。B6：候选判定
// 开始各读一次配置快照。
func (n *Notifier) fireError(ctx context.Context, taskID string, st *taskState) {
	snap, err := n.readSnapshot(ctx, taskID)
	if err != nil {
		log.Printf("notify: task %s: error gate snapshot read failed, dropped: %v", taskID, err)
		return
	}
	if snap.InstVersion != st.instVersion {
		return // B3：旧实例武装的到期条件不对新实例投递（计时已消费）
	}
	cfg := n.opts.Cfg.Config()
	e := n.evaluate(taskID, notification.CategoryError, cfg, snap, func(s TaskSnapshot) bool {
		return s.RunStatus != runStatusBusy
	})
	n.applyEpisodeOutcome(ctx, st, e, func(url string) notification.Intent {
		return errorIntent(snap, st.lastError, url)
	})
}

// fireRetry retry 届满投递：触发条件 = 任务 active 且聚合仍 retry（复验时已回
// busy/idle 等均为条件失效，spec「投递前状态已恢复」）。B6：候选判定开始各读
// 一次配置快照。
func (n *Notifier) fireRetry(ctx context.Context, taskID string, st *taskState) {
	snap, err := n.readSnapshot(ctx, taskID)
	if err != nil {
		log.Printf("notify: task %s: retry gate snapshot read failed, dropped: %v", taskID, err)
		return
	}
	if snap.InstVersion != st.instVersion {
		return // B3：旧实例武装的到期条件不对新实例投递（计时已消费）
	}
	cfg := n.opts.Cfg.Config()
	e := n.evaluate(taskID, notification.CategoryRetry, cfg, snap, func(s TaskSnapshot) bool {
		return s.RunStatus == runStatusRetry
	})
	n.applyEpisodeOutcome(ctx, st, e, func(url string) notification.Intent {
		return retryIntent(snap, url)
	})
}

// applyEpisodeOutcome retry/error 共用的门禁结果落地（spec 仲裁表）：gatePass
// 先占名额再投递（原子标记先于副作用）；URL/渠道失败占名额；其余失败不占。
func (n *Notifier) applyEpisodeOutcome(ctx context.Context, st *taskState, e evaluation, intent func(url string) notification.Intent) {
	if e.stage.occupiesEpisode() {
		st.episodeConsumed = true
		return
	}
	if e.stage != gatePass {
		return
	}
	st.episodeConsumed = true
	n.dispatch(ctx, e.plan, intent(e.plan.URL))
}

// fireIdle idle 届满投递：触发条件 = 任务 active、聚合仍 idle、无 pending
// 注意力、无存续 episode（spec idle requirement 取消条件的全集复验）。
func (n *Notifier) fireIdle(ctx context.Context, taskID string, st *taskState, cfg notification.Config) {
	snap, err := n.readSnapshot(ctx, taskID)
	if err != nil {
		log.Printf("notify: task %s: idle gate snapshot read failed, dropped: %v", taskID, err)
		return
	}
	if snap.InstVersion != st.instVersion {
		return // B3：旧实例武装的到期条件不对新实例投递（计时已消费）
	}
	e := n.evaluate(taskID, notification.CategoryIdle, cfg, snap, func(s TaskSnapshot) bool {
		return s.RunStatus == runStatusIdle &&
			len(s.Attention.Questions) == 0 && len(s.Attention.Permissions) == 0 &&
			!st.episodeActive
	})
	if e.stage == gatePass {
		n.dispatch(ctx, e.plan, idleIntent(snap, cfg.IdleTimeoutSeconds, e.plan.URL))
	}
}

// sortedTaskIDs 稳定迭代顺序（同 tick 内跨任务的确定性）。
func (n *Notifier) sortedTaskIDs() []string {
	ids := make([]string, 0, len(n.states))
	for id := range n.states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
