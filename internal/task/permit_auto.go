// permit_auto.go：ai-auto 权限请求自动判定消费者（task-permission-mode D4/D5/D6）。
//
// 核心模型：观察与判定分离——注意力管道（SSE/REST 对账）只负责登记 pending（语义不变），
// 判定由统一入口 judgeScan 在准入条件满足时发起。judgeScan 在全部触发点共用：
//   - (a) SSE asked 应用后（activate.go handleSSEEvent）；
//   - (b1) 三条 runtime 就绪提交路径（commitRuntimeReady CAS/幂等、resumeActive 提交、
//     挂起修复 suspending→active CAS/幂等；先 setPermJudgeReady 再 judgeScan）；
//   - (b2) 已 active 实例重连 align 对账完成后（startSSE onReconnect）；
//   - (c) degraded 后台对账完成后（retryAttentionDegraded permission 分支，
//     覆盖成功替换与非 404 失败缓冲重放，不以 REST 成功或 changed 为前提）。
//
// 准入与 judged-set 登记在 rt.mu 同一同步域完成；LLM 调用、客户端构造、HTTP 发送全部
// 在锁外；回复发送前复核同实例 && 未停止 && 未 unsupported。计数契约（D4）：同一 runtime
// 实例内每个 requestID 最多发起一次判定尝试，失败不清除；runtime 销毁即释放。
package task

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"ocdeck/internal/application/runtime"
	"ocdeck/internal/infrastructure/opencode"
)

// judgeScan 统一扫描入口：对 rt 的 pending 权限请求中未登记 ID 发起判定
//（task-permission-mode D4）。任何失败仅记日志（注意力事件处理永不返回错误的
// 既有约定不变；调用方均为 fire-and-forget 触发位）。
//
// 入口零 DB/零阻塞（task-permission-mode F1）：judgeScan 可能被 SSE 事件处理路径
// 同步调用，DB 预检（SQLite 单连接，可被占用阻塞）MUST NOT 在调用方执行——入口仅在
// rt.mu 临界区完成廉价门禁（实例就绪/未停止/未 unsupported）+ 惰性判定 ctx 初始化 +
// judgeWG.Add + 捕获令牌后立即返回；DB 预检、pending 快照与 judged-set 登记全部在
// 扫描 goroutine 内完成（judgeCtx 绑定 runtime 生命周期：stopAll 先 cancel 它再 join，
// 阻塞中的 DB 读随 ctx 取消收敛，停止流程含 join 不会被无法取消的 DB 读拖住）。
//
// 准入条件（与 taskOcClient 门槛一致）：任务 DB active；mode=ai-auto；实例已就绪提交；
// 未停止；未标记 reply-unsupported。未就绪/非本模式/已停止的准入拒绝属暂缓或终止：
// 不登记 judged-set、不视为失败。judged-set 登记以完整准入通过为前提（rt.mu 同一
// 同步域），并发扫描对同 ID 只有一个登记者（原子去重，不做扫描信号合并——每个触发点
// 各自发起扫描 goroutine，重复由 judged-set 去重）。
func (m *Manager) judgeScan(rt *taskRuntime) {
	if rt == nil || m.judge == nil {
		// nil judge 防御：ai-auto 全部转人工（D5 装配契约）。
		return
	}
	// 入口临界区：廉价门禁 + 惰性判定 ctx + 计数 + 令牌捕获，零 DB/零阻塞。
	// judgeWG.Add MUST 在 rt.mu 内且 judgeStopping 未置位时发生（与 stopAll 的
	// Wait 构成 happens-before：Add 要么先于 Wait 被计数，要么被 judgeStopping 拒绝）。
	rt.mu.Lock()
	if !rt.permJudgeReady || rt.judgeStopping || rt.replyUnsupported {
		rt.mu.Unlock()
		return
	}
	if rt.judgeCancel == nil {
		rt.judgeCtx, rt.judgeCancel = context.WithCancel(m.lifecycleCtx())
	}
	judgeCtx := rt.judgeCtx
	tok := rt.instVersion
	rt.judgeWG.Add(1)
	rt.mu.Unlock()

	// 扫描 goroutine（绑定 rt + instVersion + 判定 ctx）：DB 预检与登记全部在锁外/
	// goroutine 内，调用方（含 SSE 事件流）不被阻塞。
	go func() {
		defer rt.judgeWG.Done()
		m.judgeScanAsync(judgeCtx, rt, tok)
	}()
}

// judgeScanAsync 扫描 goroutine 主体（F1）：DB 预检（judgeCtx 随 runtime 停止取消）
// → pending 快照 → rt.mu 第二临界区（复核门禁含实例一致性 + 同域登记 judged-set）
// → 逐条判定/回复（沿用 judgeAndReply 的条间/构造前/最终发送准入三道复核）。
func (m *Manager) judgeScanAsync(ctx context.Context, rt *taskRuntime, tok runtime.InstVersion) {
	row, err := m.store.GetTask(ctx, rt.taskID)
	if err != nil {
		log.Printf("task %s: ai-auto scan: get task: %v", rt.taskID, err)
		return
	}
	if row.Status != StatusActive {
		return
	}
	mode, perr := resolvePermissionMode(row)
	if perr != nil {
		log.Printf("task %s: ai-auto scan: %v", rt.taskID, perr)
		return
	}
	if mode != PermissionModeAIAuto {
		return
	}

	// pending 快照（attention.mu，与 rt.mu 无嵌套）。
	snap := rt.attentionSnapshot()
	if len(snap.Permissions) == 0 {
		return
	}

	// 实例一致性预检（锁外）：被替换的旧 runtime 不再登记（判 zero 的窗口期由
	// judgeAndReply 的三道 permGate 与「登记以完整准入通过为前提」兜底）。
	if cur := m.getRuntime(rt.taskID); cur != rt {
		return
	}

	// rt.mu 第二临界区：复核门禁 + 同域登记未判定 ID（judged-set 原子去重）。
	rt.mu.Lock()
	if !rt.permJudgeReady || rt.judgeStopping || rt.replyUnsupported || rt.instVersion != tok {
		rt.mu.Unlock()
		return
	}
	var pending []PendingPermission
	for _, p := range snap.Permissions {
		if _, judged := rt.judgedPerms[p.ID]; judged {
			continue
		}
		rt.judgedPerms[p.ID] = struct{}{}
		pending = append(pending, p)
	}
	rt.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	// 平台语境组装（task-permission-mode D12/8.2）：异步 DB 路径锁外、judgeCtx 下；
	// 任务行已读（row），项目信息一次查询按次扫描复用。MUST NOT 进同步 SSE 入口。
	projName, projKind, projDir := "", "", ""
	if proj, perr := m.store.GetProject(ctx, row.ProjectID); perr != nil {
		// 项目语境缺失不阻塞判定：留空（判定仍可依任务名/请求证据进行）。
		log.Printf("task %s: ai-auto scan: get project %s: %v", rt.taskID, row.ProjectID, perr)
	} else {
		projName, projKind, projDir = proj.Name, proj.Kind, proj.Path
	}
	platform := PermissionJudgeInput{
		TaskName:    row.Name,
		ProjectName: projName,
		ProjectKind: projKind,
		TaskMode:    row.Mode,
		TaskDir:     row.WorktreePath,
		ProjectDir:  projDir,
		Branch:      row.Branch,
	}
	for _, p := range pending {
		m.judgeAndReply(ctx, rt, tok, platform, p)
	}
}

// permGate 判定/发送共用准入复核（task-permission-mode F1/F2）：
// Manager 当前实例仍是 rt（旧实例/被替换实例 MUST NOT 判定或发送）且捕获令牌一致、
// 实例就绪、未停止、未标记 reply-unsupported、ctx 未取消。
// 注册表读取（getRuntime 自持 rtMu）在 rt.mu 外；状态读取在 rt.mu 内。
func (m *Manager) permGate(ctx context.Context, rt *taskRuntime, tok runtime.InstVersion) bool {
	if ctx.Err() != nil {
		return false
	}
	if cur := m.getRuntime(rt.taskID); cur != rt {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.permJudgeReady && !rt.judgeStopping && !rt.replyUnsupported && rt.instVersion == tok
}

// permAuditVerdict 把端口判定值映射为审计 verdict 枚举（task-permission-mode D11）。
func permAuditVerdict(v PermissionVerdict) string {
	switch v {
	case PermissionVerdictApprove:
		return "APPROVE"
	case PermissionVerdictReject:
		return "REJECT"
	default:
		return "UNCERTAIN"
	}
}

// summarizeRequestDetail 由本次判定的同一份 RequestDetail 生成审计 detail 摘要
//（task-permission-mode D11）：降级原因作为前缀（MUST NOT 被截掉）；最终串 UTF-8
// 长度 ≤1024 字节（前缀与截断标记均计入——生成时先预留前缀与标记空间再截断主体）；
// 无详情时为空字符串。
func summarizeRequestDetail(d RequestDetail) string {
	var parts []string
	appendField := func(name, v string) {
		if v != "" {
			parts = append(parts, name+"="+v)
		}
	}
	appendField("command", d.Command)
	appendField("filepath", d.Filepath)
	appendField("diff", d.Diff)
	// F11/R1-F2：move 成员的源与目标（movePath）先于 patch，避免不同移动目标
	// 产生相同摘要。
	for i, f := range d.Files {
		parts = append(parts, fmt.Sprintf("files[%d]=%s:%s:%s:%s", i, f.Type, f.RelativePath, f.MovePath, f.Patch))
	}
	appendField("url", d.URL)
	appendField("description", d.Description)
	appendField("subagent_type", d.SubagentType)
	appendField("pattern", d.Pattern)
	appendField("path", d.Path)
	appendField("include", d.Include)
	appendField("parent_dir", d.ParentDir)
	if len(d.Directories) > 0 {
		parts = append(parts, "directories=["+strings.Join(d.Directories, ",")+"]")
	}
	prefix := ""
	if d.Degraded != "" {
		prefix = d.Degraded + ": "
	}
	body := strings.Join(parts, "; ")
	const marker = "…[truncated]"
	if len(prefix)+len(body) > auditDetailMax {
		// 先预留前缀与标记空间再截断主体（D11）；truncateUTF8 已追加标记，
		// 此处不得重复追加（否则超出 1024B 上限）。
		keep := auditDetailMax - len(prefix) - len(marker)
		if keep < 0 {
			keep = 0
		}
		body, _ = truncateUTF8(body, keep)
	}
	if prefix == "" && body == "" {
		return ""
	}
	return prefix + body
}

// recordPermAudit 审计旁路写入（task-permission-mode D11）：锁外、不受 permGate
// 拦截（判定已完成，记录是对已发生事实的留痕）；nil logger 零写入；写入失败仅
// log.Printf，不影响判定与回复（旁路，spec 行为契约）。
// 生命周期界限：Judge/HTTP 有界（10s/5s），审计文件 IO 无独立取消或 deadline——
// Record 同步持 logger mutex 执行 Write，极端情况下缓慢 Write 会拖住本 goroutine
// 与停止路径的 judgeWG join。
func (m *Manager) recordPermAudit(taskID, taskName string, perm PendingPermission, verdict, replyResult, reply, detail string) {
	if m.permAudit == nil {
		return
	}
	rec := PermAuditRecord{
		Time:        time.Now().UTC().Format(time.RFC3339),
		TaskID:      taskID,
		TaskName:    taskName,
		RequestID:   perm.ID,
		Permission:  perm.Permission,
		Patterns:    perm.Patterns,
		Verdict:     verdict,
		ReplyResult: replyResult,
		Reply:       reply,
		Detail:      detail,
	}
	if err := m.permAudit.Record(rec); err != nil {
		// 旁路：写入失败 MUST NOT 影响判定、回复与任务进程，仅普通日志。
		log.Printf("task %s: ai-auto audit write failed: %v", taskID, err)
	}
}

// judgeAndReply 对单条权限请求执行判定与回复（task-permission-mode D4/D5/D6/D11）。
// 仅判定通过/拒绝才回复（approve→once、reject→reject，MUST NOT always）；
// 判定不确定/失败/实例复核失败一律不回复（D6 分类 ①未发送）。
// 审计（D11）：记录计数从调用判定器起算——调用 Judge 前门禁拒绝零记录；调用后所有
// 终结路径恰好一条：UNCERTAIN/FAILED 判定终结即写（not_applicable）；APPROVE/REJECT
// 已发送按结果写（ok 含 reply 字段 / gone / unknown / unsupported），未实际发送写
// not_applicable 且不含 reply。detail 字段为本次判定同一份 RequestDetail 的摘要
//（统一生成，禁止重读 metadata 另生成），所有终结分支含 detail。审计写入自身不再受
// permGate 拦截，写入失败仅记日志；审计文件 IO 无独立取消/deadline，Record 同步执行
//（极端缓慢 Write 会拖住本 goroutine 与停止 join，见 recordPermAudit 生命周期界限）。
func (m *Manager) judgeAndReply(ctx context.Context, rt *taskRuntime, tok runtime.InstVersion, platform PermissionJudgeInput, perm PendingPermission) {
	// 条间复核（F2）：批内逐条进入 Judge 前复核——终止（stop/unsupported）、实例被替换
	// 或 ctx 已取消时剩余条目不再进入 Judge（外层循环继续、逐条在本检查处返回；
	// 新实例经自身触发位重新纳入）。调用 Judge 前门禁拒绝：零审计记录。
	if !m.permGate(ctx, rt, tok) {
		return
	}
	// 请求详情提取（D5 提取表，确定性纯函数零 IO）：Judge 输入与审计 detail 同源。
	detail := extractRequestDetail(perm.Permission, perm.Metadata)
	verdict, err := m.judge.Judge(ctx, PermissionJudgeInput{
		TaskName:    platform.TaskName,
		Permission:  perm.Permission,
		Patterns:    perm.Patterns,
		ProjectName: platform.ProjectName,
		ProjectKind: platform.ProjectKind,
		TaskMode:    platform.TaskMode,
		TaskDir:     platform.TaskDir,
		ProjectDir:  platform.ProjectDir,
		Branch:      platform.Branch,
		Detail:      detail,
	})
	auditDetail := summarizeRequestDetail(detail)
	if err != nil {
		// 判定失败按 uncertain：不回复、不重试（judged-set 已登记，不反复烧 LLM）。
		// 审计：非 nil error → verdict=FAILED（D5 返回语义接缝），判定终结即写。
		log.Printf("task %s: ai-auto judge %s (%s): %v", rt.taskID, perm.ID, perm.Permission, err)
		m.recordPermAudit(rt.taskID, platform.TaskName, perm, "FAILED", "not_applicable", "", auditDetail)
		return
	}
	var reply string
	switch verdict {
	case PermissionVerdictApprove:
		reply = "once"
	case PermissionVerdictReject:
		reply = "reject"
	default:
		// uncertain（无 error，合法输出）：不回复，转人工；审计留痕 not_applicable。
		m.recordPermAudit(rt.taskID, platform.TaskName, perm, permAuditVerdict(verdict), "not_applicable", "", auditDetail)
		return
	}
	auditVerdict := permAuditVerdict(verdict)

	// 发送前复核：permGate（含 Manager 当前实例校验——旧实例延迟返回的结果 MUST NOT 发送）。
	if !m.permGate(ctx, rt, tok) {
		m.recordPermAudit(rt.taskID, platform.TaskName, perm, auditVerdict, "not_applicable", "", auditDetail)
		return
	}

	// taskOcClient：active 门槛 + 5s OpTimeout（回复预算，D5）。
	// 构造含 DB 读与两次会话 env 读，可阻塞。
	oc, dir, ok := m.taskOcClient(ctx, rt.taskID)
	if !ok {
		m.recordPermAudit(rt.taskID, platform.TaskName, perm, auditVerdict, "not_applicable", "", auditDetail)
		return
	}
	// 最终发送准入（F1）：客户端构造期间发生的停止/unsupported/实例替换/ctx 取消
	// MUST NOT 发送；此点之后才停止的请求按「已发送、结果未知」收敛（D6 分类 ②）。
	if !m.permGate(ctx, rt, tok) {
		m.recordPermAudit(rt.taskID, platform.TaskName, perm, auditVerdict, "not_applicable", "", auditDetail)
		return
	}
	rerr := oc.ReplyPermission(ctx, dir, perm.ID, reply)
	switch {
	case rerr == nil:
		log.Printf("task %s: ai-auto replied %s to %s (%s)", rt.taskID, reply, perm.ID, perm.Permission)
		m.recordPermAudit(rt.taskID, platform.TaskName, perm, auditVerdict, "ok", reply, auditDetail)
	case errors.Is(rerr, opencode.ErrPermissionRequestGone):
		// 人工竞态：请求已了结，正常忽略（D6 分类 ③）。
		log.Printf("task %s: ai-auto reply %s: request already resolved (race), ignored", rt.taskID, perm.ID)
		m.recordPermAudit(rt.taskID, platform.TaskName, perm, auditVerdict, "gone", "", auditDetail)
	case errors.Is(rerr, opencode.ErrCapabilityUnsupported):
		// 路由 404：该 runtime 无回复端点 → 标记 reply-unsupported，停止后续判定（D6 分类 ③）。
		rt.mu.Lock()
		rt.replyUnsupported = true
		rt.mu.Unlock()
		log.Printf("task %s: ai-auto reply unsupported on this runtime; stopping further auto judgments", rt.taskID)
		m.recordPermAudit(rt.taskID, platform.TaskName, perm, auditVerdict, "unsupported", "", auditDetail)
	default:
		// 已发送、结果未知（超时/连接失败/5xx 等）：不重试、不补偿、不本地断言状态；
		// 该请求后续由既有 SSE/REST 对账收敛（D6 分类 ②）。
		log.Printf("task %s: ai-auto reply %s: unknown result (no retry): %v", rt.taskID, perm.ID, rerr)
		m.recordPermAudit(rt.taskID, platform.TaskName, perm, auditVerdict, "unknown", "", auditDetail)
	}
}
