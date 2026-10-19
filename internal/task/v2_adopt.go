// v2_adopt.go 实现 v2 承接主流程（opencode-v2-provider tasks 6.5 / design D5
// 六步）：只读准入（判定矩阵）→ server 可用性（supervisor 独立有界重试）→ 原
// session 验证（Resume）→ 候选关联（SSE 首帧握手，全内存态）→ 原子提交
// （store.CommitAdoption：归属转换+anchor+binding+状态守卫 CAS 单事务）→ 退役
// 旧关联（best-effort）。
//
// 失败出口按 D5 逐阶段矩阵：步骤 2/3 → 停止承接、保留现状、last_error、提交
// 用户决策（typed RuntimeUnavailable/NotFound）；步骤 4 握手失败 → 候选关联全
// 内存态丢弃、按既有恢复重试；步骤 5 CAS 失配 → 整体回滚零写入（store 保证）、
// 候选流退订、按冲突重试语义。任何失败路径 MUST NOT 进入普通激活失败补偿，
// MUST NOT 自动转入 B 首建。
//
// 承接 pending 丢失如实告知（design Risks/6.3）：提交时写入明确 last_error
// （官方升级语义：pending 为内存态、升级后丢失）——MUST NOT 把空 pending 解释为
// 「原 pending 已解决」。
package task

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"ocdeck/internal/application/runtime"

	application "ocdeck/internal/application"
	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	ocdecktask "ocdeck/internal/domain/task"
	ocdeckstore "ocdeck/internal/infrastructure/store"
)

// adoptionHandshakeTimeout 候选关联首帧握手预算（步骤 4；内存态，超时即丢弃）。
const adoptionHandshakeTimeout = 5 * time.Second

// v2ResumeStore 是 v2 恢复/替换提交的窄能力接口（G3-F2 激活分支；G3-F8：oldBinding
// 为本次读取事实的代际守卫，调用方 MUST 传同一次读取的 binding，不得二次读取；
// fromStatus→active 状态收敛与 binding 替换同事务）。
type v2ResumeStore interface {
	CommitV2Resume(ctx context.Context, taskID string, oldBinding, b domain.TaskBinding, fromStatus string) (application.TransitionResult, error)
}

// adoptionStore 是承接路径的 store 窄能力接口（tasks 6.4；TaskStore 的可选
// 能力视图——store.StoreAdapter 满足；测试 fake 未实现时承接路径 fail-closed
// 拒绝，不影响其余生命周期）。
type adoptionStore interface {
	ListTaskSessionKeys(ctx context.Context, taskID string) ([]domain.SessionKey, error)
	GetTaskAnchor(ctx context.Context, taskID string) (anchorID, providerID, instanceID string, err error)
	CommitAdoption(ctx context.Context, taskID string, b, oldBinding domain.TaskBinding, fromStatus string, lastError *string) (application.TransitionResult, error)
	CommitFirstBuild(ctx context.Context, taskID string, b domain.TaskBinding, key domain.SessionKey, createdAt, firstSeen, lastSeen int64, parentID string) (application.TransitionResult, error)
	CommitV2Resume(ctx context.Context, taskID string, oldBinding, b domain.TaskBinding, fromStatus string) (application.TransitionResult, error)
}

// adoptionPendingLossNotice 是承接提交的如实告知 last_error（官方升级已知限制）。
var adoptionPendingLossNotice = "v2 承接完成：v1 pending attention 为进程内存态，" +
	"官方升级后不保留；如原任务存在未处理 permission/question，请重新触发对应操作（非已解决）"

// adoptionBranch 是 D5 历史判定矩阵结果。
type adoptionBranch int

const (
	branchA    adoptionBranch = iota // A：已有历史 session（目标=anchor）
	branchB                          // B：从未建立上游会话（v2 首建）
	branchStop                       // 停止承接（部分缺失/不一致/混杂——提交用户决策）
)

// classifyAdoption 实现 D5 历史判定矩阵（只读；纯函数）：
//   - binding(v1)/anchor(v1)/归属行三者一致（anchor==binding.UpstreamSessionID
//     且 anchor ∈ v1 归属集合）→ A；
//   - 三者一致为空（无 binding、anchor 三元组完整为空、零归属行）→ B；
//   - 已有 v2 binding → 非承接准入（R1 恢复路径——调用方按 resolver 处置）；
//   - 其余（部分缺失、id 不一致、混杂、未知命名空间）→ stop（评审 I11：部分
//     anchor 三元组 / v2 或未知命名空间归属行 MUST NOT 当作「为空」误归 B）。
func classifyAdoption(hasBinding bool, binding domain.TaskBinding, anchorID, anchorProviderID, anchorInstanceID string, keys []domain.SessionKey) (adoptionBranch, string) {
	v2Binding := hasBinding && binding.ProviderID == ocdeckstore.ProviderIDV2
	if v2Binding {
		return branchStop, "" // 非承接准入（R1 路径）
	}
	bindingIsV1 := hasBinding && binding.ProviderID == ocdeckstore.ProviderIDV1 && binding.ProviderInstanceID == ocdeckstore.ProviderInstanceV1Default
	if hasBinding && !bindingIsV1 {
		return branchStop, "" // 未知命名空间 binding（不猜测）
	}
	// 锚定存在性以 anchor_session_id 为准：命名空间迁移（0018）对存量行把
	// anchor_provider_id/instance 回填为 v1 缺省值——anchor id 为空时这两列是
	// 回填默认值而非事实（评审 I11 的「部分三元组」即指 id 与命名空间列不一致
	// 的行，按 id 权威判定，MUST NOT 当作有 anchor 或误归 B）。
	if anchorID != "" {
		if anchorProviderID != ocdeckstore.ProviderIDV1 || anchorInstanceID != ocdeckstore.ProviderInstanceV1Default {
			return branchStop, "" // anchor 在非 v1 命名空间（已承接过或损坏——非本入口）
		}
	}
	// 归属行命名空间分区：v1 行=历史证据；v2 行=已承接证据（混杂→stop）；未知
	// 命名空间→stop（不猜测）。
	v1Rows := 0
	for _, k := range keys {
		switch {
		case k.ProviderID == ocdeckstore.ProviderIDV1 && k.ProviderInstanceID == ocdeckstore.ProviderInstanceV1Default:
			v1Rows++
		case k.ProviderID == ocdeckstore.ProviderIDV2 && k.ProviderInstanceID == ocdeckstore.ProviderInstanceV2Default:
			return branchStop, "" // v2 归属行存在而 binding 非 v2：混杂/已承接证据
		default:
			return branchStop, "" // 未知命名空间归属行
		}
	}
	anchorIsV1 := anchorID != "" // 命名空间合法性已在上面的 id 权威守卫内验证
	if bindingIsV1 && anchorIsV1 && binding.UpstreamSessionID == anchorID {
		// anchor ∈ v1 归属集合（归属集合可含子 session）。
		for _, k := range keys {
			if k.UpstreamID == anchorID {
				return branchA, anchorID
			}
		}
	}
	if !hasBinding && anchorID == "" && v1Rows == 0 {
		return branchB, ""
	}
	return branchStop, ""
}

// adoptTaskV2 承接主流程入口（activate/recovery 的 R8 分支共用）。mode 为对齐
// 模式（沿用激活语义）。fromStatus 由任务行现状决定：suspended→active（手动
// 激活）/ active→active 守卫（存量承接，均不经 activating）。taskLocked 表示
// 调用方已持任务锁（Activate 全程持锁）——接管注册的串行边界复用之；恢复路径
// （false）由接管点自行取锁（评审 I15：与 Suspend/Delete 同一任务级串行边界）。
func (m *Manager) adoptTaskV2(ctx context.Context, taskID string, row TaskRow, mode AlignMode, taskLocked bool) error {
	// store 承接能力（窄接口；缺失 = 装配不支持承接——fail-closed）。
	astore, ok := m.store.(adoptionStore)
	if !ok {
		err := appagent.NewError("admission.adopt", appagent.ErrKindCapabilityUnavailable,
			errors.New("store 未实现承接原子转换能力（CommitAdoption）"))
		m.recordAdmissionError(ctx, taskID, err)
		return err
	}
	// 步骤 1 只读准入（零副作用）：判定矩阵 + 版本/开关（resolver 已过）。
	binding, hasBinding, err := m.store.GetTaskBinding(ctx, taskID)
	if err != nil {
		return err
	}
	anchorID, anchorPID, anchorIID, aerr := astore.GetTaskAnchor(ctx, taskID)
	if aerr != nil {
		return aerr
	}
	keys, kerr := astore.ListTaskSessionKeys(ctx, taskID)
	if kerr != nil {
		return kerr
	}
	branch, _ := classifyAdoption(hasBinding, binding, anchorID, anchorPID, anchorIID, keys)
	switch branch {
	case branchStop:
		// D5 判定矩阵「其余」：停止承接、保留现状、last_error、提交用户决策。
		err := appagent.NewError("admission.adopt", appagent.ErrKindBindingInvalid,
			errors.New("承接准入判定不一致（binding/anchor/归属行部分缺失或不一致）——停止承接，保留任务现状"))
		m.recordAdmissionError(ctx, taskID, err)
		return err
	case branchB:
		// B：v2 正常首建（钉扎值不变）——R8-B 与 R5 共用 firstBuildV2（评审 I4）。
		supB, _, _, _, berr := m.admitExecution(ctx, taskID)
		if berr != nil {
			m.recordAdmissionError(ctx, taskID, berr)
			return berr
		}
		return m.firstBuildV2(ctx, taskID, row, mode, supB, taskLocked)
	case branchA:
		// target = anchor session id；继续步骤 2-6。
	}

	// 步骤 2+3：server 可用性（supervisor 独立有界重试）+ 原 session 验证
	//（Resume；ResumePolicy 不授权创建替代——v2 supervisor 语义冻结）。
	sup, _, _, _, err := m.admitExecution(ctx, taskID)
	if err != nil {
		m.recordAdmissionError(ctx, taskID, err)
		return err
	}
	candidate, rerr := sup.Resume(ctx,
		appagent.TaskRef{TaskID: taskID, Directory: row.WorktreePath}, binding,
		appagent.ResumePolicy{AllowSessionCreate: false, AllowRebuild: false})
	if rerr != nil {
		// 步骤 2/3 失败出口：停止承接、保留现状、last_error、提交用户决策
		//（阶段标识经 Kind 区分：RuntimeUnavailable=server 可用性、
		// NotFound=原 session 不可读——均已由 supervisor 有界重试/停止语义承载）。
		rerr = appagent.NewError("admission.adopt", appagent.KindOf(rerr), rerr)
		m.recordAdmissionError(ctx, taskID, fmt.Errorf("承接失败（保留现状）: %v", rerr))
		return rerr
	}

	// 步骤 4 候选关联（内存态）：派生凭据连接 + 事件订阅首帧握手；失败 → 全内存
	// 丢弃、零持久化副作用、按既有恢复重试。
	es, serr := m.candidateHandshake(ctx, taskID, candidate)
	if serr != nil {
		m.recordAdmissionError(ctx, taskID, fmt.Errorf("承接候选订阅失败: %v", serr))
		return serr
	}

	// 步骤 5 原子提交：状态现状决定 CAS 形态（suspended→active / active→active
	// 守卫）；last_error 如实告知 pending 丢失。CAS 失配/事务失败 → 整体回滚
	// 零写入（store 保证）+ 候选流退订 + 冲突重试语义。
	var from ocdecktask.Status
	switch row.Status {
	case string(ocdecktask.StatusActive):
		from = ocdecktask.StatusActive
	default:
		from = ocdecktask.StatusSuspended
	}
	res, cerr := astore.CommitAdoption(ctx, taskID, candidate, binding, string(from), &adoptionPendingLossNotice)
	if cerr != nil || !res.Matched {
		if es != nil {
			_ = es.Close()
		}
		if cerr == nil {
			cerr = fmt.Errorf("%w: 承接提交状态守卫失配", ocdeckstore.ErrAdoptionConflict)
		}
		return cerr
	}

	// 步骤 6 退役旧关联（best-effort，失败仅 cleanup notice——v1 runtime 会话
	// 若在；v2 任务无任务进程，正常路径无残留）。
	m.retireLegacyAssociations(ctx, taskID)

	// 候选流复用为正式接管流（评审 I10：保留缓冲事件，不重订阅——close 移交
	// 给 startSSE 的流生命周期）。
	return m.adoptTakeover(ctx, taskID, row, candidate, mode, es, taskLocked)
}

// candidateHandshake 候选关联订阅 + 首帧握手（D4 提交条件四之一：SSE 首帧先于
// 原子提交；D4 替换表：候选阶段仅握手与缓冲——不对齐、零归属/事件写入）。失败/
// 超时关流返回错误（全内存态零持久化）。EventSource 为 nil（minimalProvider/
// 测试接缝形态，与 startSSE 回退约定同形）→ 视为握手满足返回 nil 流。返回流由
// 调用方在提交后关闭（Close 幂等）。
func (m *Manager) candidateHandshake(ctx context.Context, taskID string, candidate domain.TaskBinding) (appagent.EventStream, error) {
	p, err := m.providerRegistry.Get(ocdeckstore.ProviderIDV2)
	if err != nil {
		return nil, err
	}
	es := p.Events()
	if es == nil {
		return nil, nil
	}
	// 订阅挂 Manager 生命周期 ctx（评审 I10：候选流在提交成功后复用为正式 SSE——
	// 保留缓冲事件、消除重订阅失败面；请求 ctx 取消不得拆掉已提交任务的接管流）。
	stream, serr := es.Subscribe(m.lifecycleCtx(), candidate)
	if serr != nil {
		return nil, serr
	}
	handshake := make(chan struct{}, 1)
	stream.SetCallbacks(func() { handshake <- struct{}{} }, nil)
	select {
	case <-handshake:
		return stream, nil
	case <-time.After(adoptionHandshakeTimeout):
		_ = stream.Close()
		return nil, errors.New("候选关联握手超时（全内存态丢弃）")
	}
}

// firstBuildV2 是 v2 首建统一流程（评审 I4/I5；服务 R8-B 零历史承接与 R5 v2
// 钉扎新任务——一份准备+原子提交+candidate 补偿流程）。序列（D4 B 行 + 提交
// 条件）：Acquire（POST 语义，attempt 内失败已由 supervisor 回收）→ 端点 →
// 候选握手（仅订阅+首帧，不对齐零归属写入）→ CommitFirstBuild（candidate
// claim+anchor+binding+activating→active CAS 单事务）→ 提交后正式接管（对齐/
// 事件消费启用点；失败=已提交恢复债务，不回滚）。提交前失败 = candidate 未提
// 交：回收（D3 受限回收例外）+ 上抛（激活入口经既有补偿收敛 suspended）。
func (m *Manager) firstBuildV2(ctx context.Context, taskID string, row TaskRow, mode AlignMode, sup appagent.RuntimeSupervisor, taskLocked bool) error {
	astore := m.adoptionStoreOf()
	if astore == nil {
		err := appagent.NewError("activate.v2", appagent.ErrKindCapabilityUnavailable,
			errors.New("store 未实现首建原子提交能力（CommitFirstBuild）"))
		m.recordAdmissionError(ctx, taskID, err)
		return err
	}
	// Round3 J3：首建 POST 前持久化可跨重启的阻断意图（candidate_cleanup_failed
	// retryable 残债，sessionName=pending）——持久化失败 MUST NOT 发起 POST
	//（零 POST：无 candidate 产生 → 无残留事实可丢失）；notice 在位期间首建
	// 准入门禁持续阻断（跨重启：启动恢复不自动激活 suspended，reaper 重放清债）。
	if aerr := m.armCandidateIntent(ctx, taskID); aerr != nil {
		return appagent.NewError("activate.v2", appagent.ErrKindCapabilityUnavailable,
			fmt.Errorf("首建阻断意图持久化失败（零 POST 防残留事实丢失）: %w", aerr))
	}
	binding, serr := sup.Acquire(ctx, appagent.TaskRef{TaskID: taskID, Directory: row.WorktreePath})
	if serr != nil {
		// 收尾 R1 J3：candidate 回收失败（typed 清理债务）——把占位意图升级为
		// 含真实 candidate sessionID 的持久债务（Round3 J3：跨重启可识别，门禁
		// 持续阻断直至收割/处置）；升级落库失败 → 受管 ledger 兜底。义务安全
		// 登记前阻断再次首建。
		var debt *appagent.CandidateCleanupDebt
		if errors.As(serr, &debt) {
			// 请求 ctx 可能已在 Acquire 期间取消（Round2 J3）——升级落库用独立
			// 有界 ctx，取消不得阻止跨重启身份持久化。
			sCtx, sCancel := withResidualNoticeCtx(ctx)
			uerr := m.settleCandidateIntent(sCtx, taskID, debt.SessionID)
			sCancel()
			if uerr != nil {
				m.registerReapObligations(taskID, []suspendFinishObligation{{
					Kind: "notice", TaskID: taskID, Name: debt.SessionID,
					Reason: noticeReasonCandidateCleanupFailed, Retryable: true,
				}})
				serr = errors.Join(serr, uerr)
			}
			return m.supervisorStageErrorForTyped(serr, taskID)
		}
		// 无 debt（Acquire 前置失败——candidate 未产生）：占位意图清除（独立有界
		// ctx——清除失败仅留待重放，门禁短期保守持有）。
		cCtx, cCancel := withResidualNoticeCtx(ctx)
		cerr := m.clearCandidateIntent(cCtx, taskID)
		cCancel()
		if cerr != nil {
			log.Printf("task %s: clear candidate intent after acquire pre-failure: %v", taskID, cerr)
		}
		return m.supervisorStageErrorForTyped(serr, taskID)
	}
	if _, _, _, eerr := m.bindingEndpoint(binding); eerr != nil {
		return m.rollbackFirstBuild(ctx, taskID, sup, binding,
			m.supervisorStageErrorForTyped(
				appagent.NewStageError("supervisor.binding_endpoint", appagent.ErrKindBindingInvalid, eerr), taskID))
	}
	// 候选握手（提交条件：SSE 首帧先于原子提交；缓冲-only——不对齐/零归属写入）。
	es, herr := m.candidateHandshake(ctx, taskID, binding)
	if herr != nil {
		m.recordAdmissionError(ctx, taskID, fmt.Errorf("首建候选握手失败（全内存态丢弃）: %v", herr))
		return m.rollbackFirstBuild(ctx, taskID, sup, binding, herr)
	}
	// 原子首建提交（candidate claim+anchor+binding+状态 CAS；入口 beginActivation
	// 已置 activating；钉扎值 provider_id 列不变）。
	key := domain.SessionKey{
		ProviderID: binding.ProviderID, ProviderInstanceID: binding.ProviderInstanceID,
		UpstreamID: binding.UpstreamSessionID,
	}
	now := m.nowUnix()
	if _, cerr := astore.CommitFirstBuild(ctx, taskID, binding, key, now, now, now, ""); cerr != nil {
		if es != nil {
			_ = es.Close()
		}
		return m.rollbackFirstBuild(ctx, taskID, sup, binding,
			newOpErr(codeConflict, fmt.Errorf("v2 首建提交: %w", cerr)))
	}
	// Round3 J3：candidate 已正式提交成为 binding 事实——占位阻断意图 MUST
	// 解除（残留会被 reaper 误删已提交 session）。best-effort+CAS 重试。
	if cerr := m.clearCandidateIntent(ctx, taskID); cerr != nil {
		log.Printf("task %s: clear candidate intent after first-build commit: %v", taskID, cerr)
	}
	// 提交后正式接管（候选流复用——评审 I10；正式 SSE/对齐/事件消费启用；
	// 失败=已提交恢复债务 postCommitTakeoverError，禁普通补偿）。
	return m.adoptTakeover(ctx, taskID, row, binding, mode, es, taskLocked)
}

// rollbackFirstBuild 是首建提交前失败的 candidate 回收（D3 受限回收例外）：
// 经 supervisor 的 CandidateSessionRecycler 回收本次 attempt 新建且未提交的
// session——回收 ctx 用 WithoutCancel 有界预算（activate.go 补偿同型：调用方 ctx
// 已取消不得跳过回收）；回收失败聚合进错误链（清理债务，MUST NOT 误判「从未
// 建立」）。recycler 未实现（装配不支持）→ 诊断性 last_error 记残留 candidate，
// 不静默跳过。
func (m *Manager) rollbackFirstBuild(ctx context.Context, taskID string, sup appagent.RuntimeSupervisor, candidate domain.TaskBinding, cause error) error {
	rc, ok := sup.(appagent.CandidateSessionRecycler)
	if !ok {
		m.recordAdmissionError(ctx, taskID, fmt.Errorf(
			"B 首建失败补偿：supervisor 未实现 CandidateSessionRecycler，candidate session %s 未回收（残留事实）",
			candidate.UpstreamSessionID))
		return cause
	}
	rcCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activateCompensationTimeout)
	defer cancel()
	if rerr := rc.RecycleCandidateSession(rcCtx, candidate); rerr != nil {
		// Round2/3 J3：回收失败统一走 CandidateCleanupDebt 持久化门禁链——把
		// 占位意图升级为含真实 sessionID 的持久债务（跨重启可识别，门禁持续
		// 阻断直至收割/处置）；升级落库失败 → 受管 ledger 兜底。
		debt := &appagent.CandidateCleanupDebt{SessionID: candidate.UpstreamSessionID, Err: rerr}
		rcCtx2, rcCancel2 := withResidualNoticeCtx(ctx)
		nerr := m.recordResidualNotice(rcCtx2, taskID, debt.SessionID, nil,
			noticeReasonCandidateCleanupFailed, true)
		if serr := m.settleCandidateIntent(rcCtx2, taskID, candidate.UpstreamSessionID); serr != nil {
			log.Printf("task %s: settle candidate intent to debt session %s: %v", taskID, candidate.UpstreamSessionID, serr)
		}
		rcCancel2()
		if nerr != nil {
			m.registerReapObligations(taskID, []suspendFinishObligation{{
				Kind: "notice", TaskID: taskID, Name: debt.SessionID,
				Reason: noticeReasonCandidateCleanupFailed, Retryable: true,
			}})
		}
		// notice 直写失败 MUST Join 进返回值（不静默——门禁依赖 notice/ledger
		// 至少一个通道承载跨重启阻断）。
		return errors.Join(fmt.Errorf("%w; candidate %s 回收失败（清理债务，残留事实非「从未建立」）: %v",
			cause, candidate.UpstreamSessionID, rerr), nerr)
	}
	// 回收成功：candidate 已不存在——占位阻断意图解除（门禁随之开放；rcCtx 已
	// WithoutCancel——回收 succeeded 但请求已取消时清除仍须完成）。
	if cerr := m.clearCandidateIntent(rcCtx, taskID); cerr != nil {
		log.Printf("task %s: clear candidate intent after successful recycle: %v", taskID, cerr)
	}
	return cause
}

// retireLegacyAssociations 承接提交成功后的旧关联退役（best-effort；失败仅
// cleanup notice——D5 步骤 6）。
func (m *Manager) retireLegacyAssociations(ctx context.Context, taskID string) {
	name := runtimeSessionName(taskID)
	if ok, err := m.proc.HasSession(name); err == nil && ok {
		if _, kerr := m.proc.KillSession(name); kerr != nil {
			logAdoptionNotice(taskID, "退役 v1 runtime 会话失败（cleanup notice）: "+kerr.Error())
		}
	}
}

func logAdoptionNotice(taskID, msg string) {
	log.Printf("task %s: %s", taskID, msg)
}

// V2TaskEnvSeam 实现 v2 配置应用链的任务事实供给（tasks 5.4；结构满足
// opencode2.TaskEnvSeam——task 层不 import provider 传输包，经结构类型在
// 装配方适配）。ResolveSessionEnv 语义：recovery=false（首次路径）= 既有任务
// 环境解析当前值（mergeEnvSnapshot 同源）；recovery=true（created 恢复）=
// 同代 env 快照（loadEnvSnapshot 同代语义，MUST NOT 重新分层）。
type V2TaskEnvSeam struct{ m *Manager }

// NewV2TaskEnvSeam 构造 seam（main.go 生产装配）。
func NewV2TaskEnvSeam(m *Manager) *V2TaskEnvSeam { return &V2TaskEnvSeam{m: m} }

// ResolveSessionEnv 按 phase 返回会话环境事实（键值对；不含 runtime 级事实——
// 共享 serve 进程环境仅含端口/凭据/实例标识，任务配置仅 session 级）。
func (s *V2TaskEnvSeam) ResolveSessionEnv(ctx context.Context, taskID string, recovery bool) (map[string]string, error) {
	row, err := s.m.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if recovery {
		// created 恢复：同代 env 快照（recovery.go 同代语义）。
		return s.m.loadEnvSnapshot(row)
	}
	// 首次路径：既有任务环境解析当前值（不含端口——runtime 级事实不进 session env）。
	env, err := s.m.mergeEnvSnapshot(ctx, row, 0)
	if err != nil {
		return nil, err
	}
	delete(env, "OCDECK_SERVE_PORT")
	return env, nil
}

// beginV2RuntimeTakeover 是 v2 正式接管的提交前段（D4 提交条件：SSE 首帧握手
// 先于原子提交）：runtime 注册 + provider-aware SSE（快照对齐内含）——无 v1 式
// runtime 会话探活/watchServeExit（v2 无任务 tmux 单元）。返回 rt 供提交后段
// 续用；rt 非 nil 的失败（SSE）由调用方决定回退（未提交：clearRuntime）或保留
// （已提交：恢复债务重建）。
func (m *Manager) beginV2RuntimeTakeover(ctx context.Context, taskID string, row TaskRow, port int, password string, binding domain.TaskBinding, mode AlignMode, preStream appagent.EventStream) (*taskRuntime, error) {
	// 收尾 R1 J5（D3 冻结前 fail-closed）：v2 共享 serve 由 supervisor
	// startServer 启动（无 --auto 参数）——SavedMode=all-approve 不能宣称
	// StartedWithAuto=true（权限模式未实际应用）。按启动事实规则收敛为 ask
	// 并透出能力缺口；ask/ai-auto 不受影响（AIAutoEnabled 走冻结的 reply
	// 端点，task 层判定）。
	startMode := row.PermissionMode
	if startMode == PermissionModeAllApprove {
		startMode = PermissionModeAsk
		log.Printf("task %s: v2 takeover cannot apply all-approve at server start "+
			"(no frozen session-level permission apply endpoint); starting fact downgraded to ask", taskID)
	}
	rt := m.newRuntime(taskID)
	if err := initPermState(rt, row, startMode); err != nil {
		return nil, fmt.Errorf("v2 runtime register: init perm state: %v", err)
	}
	m.setRuntime(taskID, rt)
	if err := m.startSSE(ctx, rt, taskID, row.WorktreePath, port, password, mode, &binding, preStream); err != nil {
		return rt, err
	}
	return rt, nil
}

// finishV2RuntimeReady 是 v2 正式接管的提交后段（F3 对齐语义）：active 后启动
// diff review 调度器 + perm judge 就绪扫描（镜像 commitRuntimeReady Matched
// 分支的提交后动作；幂等）。
func (m *Manager) finishV2RuntimeReady(taskID string, rt *taskRuntime) {
	m.StartDiffReviewSchedulerForTask(m.lifeCtx, taskID)
	rt.setPermJudgeReady()
	m.judgeScan(rt)
}

// postCommitTakeoverError 标记「原子提交已完成后的正式接管失败」（评审 I10）：
// 任务已 active/binding 已提交——MUST NOT 回滚、MUST NOT 走普通激活补偿（清
// env/清关联对已提交事实是破坏性的，且 activating 谓词回退不可达）；恢复工作
// 由调用方实际登记（registerPostCommitRecovery——ensureRecovery 调度闭环）。
type postCommitTakeoverError struct{ err error }

func (e *postCommitTakeoverError) Error() string { return e.err.Error() }
func (e *postCommitTakeoverError) Unwrap() error { return e.err }

// registerPostCommitRecovery 登记已提交任务的恢复工作（评审 I10 调度闭环）：
// 有 runtime（已注册接管面）→ 以其令牌触发 ensureRecovery；无 runtime（接管
// 前段即失败——注册未发生）→ 铸造新代令牌后触发（恢复入口经 token 重铸收敛，
// 不因无锚而丢失调度闭环）。
func (m *Manager) registerPostCommitRecovery(taskID string) {
	tok := ""
	if rt := m.getRuntime(taskID); rt != nil {
		tok = string(rt.instVersion)
	} else {
		tok = string(m.runtimeRegistry.NewInstVersion(taskID))
	}
	log.Printf("post-commit takeover failure task %s: registering recovery work", taskID)
	go m.ensureRecovery(taskID, runtime.InstVersion(tok), true)
}

// commitV2RuntimeReady 是 v2 任务运行时就绪提交（gate3 G3-F2/F3；已提交路径：
// A 分支接管/已有 v2 binding 激活/恢复）：前段接管（runtime+SSE）+ 提交后段
// （调度器/判定就绪），无状态 CAS（调用方已完成 binding/状态事务）。失败（SSE
// 建立等）= 正式接管失败：任务已 active/binding 已提交，MUST NOT 回滚——记
// last_error（恢复债务语义），经 postCommitTakeoverError 分类由调用方登记恢复
// 工作（禁普通激活补偿），不得伪装完整可用。preStream 非 nil 时复用候选流
// （评审 I10：保留缓冲事件，不重订阅）。
func (m *Manager) commitV2RuntimeReady(ctx context.Context, taskID string, row TaskRow, port int, password string, binding domain.TaskBinding, mode AlignMode, preStream appagent.EventStream, taskLocked bool) error {
	// 接管守卫（评审 I15）：注册 runtime/启动 SSE 前重验任务状态——已离开
	// active（并发 Suspend/Delete 收敛）→ 放弃接管。串行边界：taskLocked=true
	//（调用方持锁——Activate 全程）复用之；false（恢复路径）在此取锁并把注册
	// 临界区置于锁内（与 Suspend/Delete 同一任务级串行边界，窗口密闭）。
	// I15：锁域覆盖注册→SSE→ready 发布全程（finishV2RuntimeReady 的调度器
	// 启动/perm judge 置位与并发清理互斥，不提前释放）。
	var unlock func()
	if !taskLocked {
		u, lerr := m.lockTaskForRecovery(taskID)
		if lerr != nil {
			// I16：取锁失败出口同样未移交候选流——调用链默认持有关闭责任。
			m.closeAdoptPreStream(taskID, preStream)
			return &postCommitTakeoverError{err: newOpErr(codeConflict,
				fmt.Errorf("承接接管守卫：任务锁不可得（并发生命周期变更）: %w", lerr))}
		}
		unlock = u
	}
	if unlock != nil {
		defer unlock()
	}
	cur, gerr := m.store.GetTask(ctx, taskID)
	if gerr != nil {
		m.closeAdoptPreStream(taskID, preStream)
		return &postCommitTakeoverError{err: newOpErr(codeInternal, gerr)}
	}
	if cur.Status != StatusActive {
		m.closeAdoptPreStream(taskID, preStream)
		return &postCommitTakeoverError{err: newOpErr(codeConflict,
			fmt.Errorf("承接接管守卫：任务状态已收敛 %s（并发 Suspend/Delete 等）——放弃接管，不重建运行关联", cur.Status))}
	}
	row = cur
	rt, err := m.beginV2RuntimeTakeover(ctx, taskID, row, port, password, binding, mode, preStream)
	if err != nil {
		if rt == nil {
			// 接管前段初始化失败（未产生连接/订阅面）——候选流所有权仍在调用链，
			// 关闭；同为已提交分类。
			m.closeAdoptPreStream(taskID, preStream)
			m.recordAdmissionError(ctx, taskID, err)
			return &postCommitTakeoverError{err: newOpErr(codeInternal, err)}
		}
		// 正式接管失败（SSE）：候选流已移交 SSE goroutine（defer Close——由 rt
		// 生命周期收口）；已提交事务不可回滚 → last_error（恢复债务），恢复循环
		// 按既有语义重建（不得伪装完整可用）。
		m.recordAdmissionError(ctx, taskID, fmt.Errorf("v2 runtime SSE 接管失败（已提交，待恢复重建）: %v", err))
		return &postCommitTakeoverError{err: newOpErr(codeProcessError, fmt.Errorf("v2 runtime sse subscribe: %w", err))}
	}
	// I16：成功移交正式消费循环（流由 SSE goroutine defer Close 承担——rt 生命周期）。
	m.finishV2RuntimeReady(taskID, rt)
	return nil
}

// closeAdoptPreStream 关闭尚未移交正式消费循环的候选流（评审 I16：拒绝接管/
// 初始化失败等提前出口统一释放——候选订阅挂 Manager 生命周期，泄漏会存续到
// 关停且继续更新状态缓存）。
func (m *Manager) closeAdoptPreStream(taskID string, es appagent.EventStream) {
	if es == nil {
		return
	}
	if err := es.Close(); err != nil {
		log.Printf("task %s: adopt pre-stream close: %v", taskID, err)
	}
}

// adoptTakeover 是承接提交成功后的正式运行时接管（G3-F3）：接管守卫（评审
// I15：提交与 Suspend/Delete 并发窗口——注册 runtime/启动 SSE 前重验任务仍为
// active，否则放弃接管，不重建「suspended + 运行关联」矛盾事实）→ 基于已提交
// v2 binding 解析端点并落 commitV2RuntimeReady（preStream 复用候选流）。
// taskLocked=调用方已持任务锁（Activate 全程持锁——串行边界复用；守卫仅重验）。
func (m *Manager) adoptTakeover(ctx context.Context, taskID string, row TaskRow, binding domain.TaskBinding, mode AlignMode, preStream appagent.EventStream, taskLocked bool) error {
	port, password, _, eerr := m.bindingEndpoint(binding)
	if eerr != nil {
		m.closeAdoptPreStream(taskID, preStream) // I16：未移交先释放
		m.recordAdmissionError(ctx, taskID, fmt.Errorf("承接接管端点解析失败（已提交，待恢复重建）: %v", eerr))
		return &postCommitTakeoverError{err: newOpErr(codeInternal, eerr)}
	}
	return m.commitV2RuntimeReady(ctx, taskID, row, port, password, binding, mode, preStream, taskLocked)
}

// resumeActivateV2 是已有 v2 binding 任务的激活分支（gate3 G3-F2）：Resume
// 延续原 session（不建替代）→ 恢复/替换提交（binding 替换 + activating→active
// 状态收敛单事务——入口 beginActivation 已置 activating，D4 生命周期矩阵；不走
// 首次语义）→ 正式运行时就绪（G3-F3 同一原语）。
func (m *Manager) resumeActivateV2(ctx context.Context, taskID string, row TaskRow, sup appagent.RuntimeSupervisor, mode AlignMode, taskLocked bool) error {
	astore, ok := m.store.(v2ResumeStore)
	if !ok {
		err := appagent.NewError("activate.v2", appagent.ErrKindCapabilityUnavailable,
			errors.New("store 未实现 v2 恢复/替换提交能力（CommitV2Resume）"))
		m.recordAdmissionError(ctx, taskID, err)
		return err
	}
	cur, has, err := m.store.GetTaskBinding(ctx, taskID)
	if err != nil {
		return err
	}
	if !has {
		err := appagent.NewError("activate.v2", appagent.ErrKindBindingInvalid,
			errors.New("v2 激活分支要求已有 v2 binding"))
		m.recordAdmissionError(ctx, taskID, err)
		return err
	}
	binding, rerr := sup.Resume(ctx,
		appagent.TaskRef{TaskID: taskID, Directory: row.WorktreePath}, cur,
		appagent.ResumePolicy{AllowSessionCreate: false, AllowRebuild: false})
	if rerr != nil {
		return m.supervisorStageErrorForTyped(rerr, taskID)
	}
	port, password, _, eerr := m.bindingEndpoint(binding)
	if eerr != nil {
		return m.supervisorStageErrorForTyped(
			appagent.NewStageError("supervisor.binding_endpoint", appagent.ErrKindBindingInvalid, eerr), taskID)
	}
	// 候选关联握手（评审 I10 / D4 提交条件：SSE 首帧先于原子提交——四就绪缺一
	// 不提交）。失败关候选、保留旧 binding/status 事实（提交未发生），按激活
	// 失败语义上抛。
	es, herr := m.candidateHandshake(ctx, taskID, binding)
	if herr != nil {
		return m.supervisorStageErrorForTyped(
			appagent.NewStageError("activate.v2.handshake", appagent.ErrKindRuntimeUnavailable, herr), taskID)
	}
	if _, cerr := astore.CommitV2Resume(ctx, taskID, cur, binding, row.Status); cerr != nil {
		if es != nil {
			_ = es.Close()
		}
		return newOpErr(codeConflict, fmt.Errorf("v2 激活替换提交: %w", cerr))
	}
	// 候选流复用为正式接管流（评审 I10：保留缓冲事件，不重订阅）。
	// taskLocked=true：Activate 全程持任务锁（接管注册复用该串行边界）。
	return m.commitV2RuntimeReady(ctx, taskID, row, port, password, binding, mode, es, true)
}

// adoptionStoreOf 返回承接/恢复提交窄能力视图（缺失 = nil——调用方 fail-closed）。
func (m *Manager) adoptionStoreOf() adoptionStore {
	if as, ok := m.store.(adoptionStore); ok {
		return as
	}
	return nil
}

// oldBindingIf 返回既有 binding（存在时）或零值（首次安装——代际守卫退化为仅
// 状态 CAS）。
func oldBindingIf(has bool, b domain.TaskBinding) domain.TaskBinding {
	if has {
		return b
	}
	return domain.TaskBinding{}
}
