package task

// charx_sensordata_prod_test.go 生产装配形态敏感数据审查（opencode-v1-spi-provider
// tasks 5.2 / design D3 敏感数据保护；Phase D 门禁 F3）：既有
// TestCharxContract_SensitiveDataAudit 锁定内联成功路径，本文件锁定 supervisor
// 生产装配路径（cmd/ocdeck-server main.go 同形：真实 opencode.Supervisor +
// WithHealthReporter + HealthTracker + OnRuntimeReady/Release 换代回调）下的
// 目标分支：
//  1. derived 激活（supervisor Acquire → 提交事务 binding 写入）；
//  2. Suspend → persist 恢复（resumeActive → supervisor.Resume 复用分支）；
//  3. legacy 迁移导入成功 + legacy 健康检查失败（last_error、binding 零写入）；
//  4. 激活失败补偿（健康失败 + kill 非净注入 → cleanup notice）；
//  5. 恢复 debt（生产写入入口 persistRecoveryCleanupDebt → recovery_debts 行）；
//  6. resume 对畸形 binding（kind=weird + legacy 密码）→ supervisor 解码错误
//    （decodeV1BindingEndpoint）→ 错误返回文本。
// 断言各目标分支确实发生（binding/last_error/notice/debt/事件非空），再扫描
// 错误返回、存储、日志与事件 json.Marshal 序列化载荷：派生密码、legacy 密码、
// master secret 零出现（legacy 密码仅允许存在于 t-legacy 的 task_binding 列——
// 唯一存储事实）。
//
// mutation 证据（红→还原，生产上报点）：decodeV1BindingEndpoint「未知 auth.kind」
// 错误文本临时夹带 data.Auth.Password → 场景 ⑥ 错误返回文本含 legacy 密码 → 红
// → 还原。capabilities.reason 通道的同类 mutation（decodeV1Binding 夹带）见
// internal/api/sensordata_audit_test.go（F1/F3 api 面）。
import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

func TestCharxContract_SensitiveDataAudit_ProdAssembly(t *testing.T) {
	ctx := context.Background()
	const legacyPW = "legacy-prod-audit-pw"

	// 日志捕获（健康迁移 warn / 激活补偿 / runner 落账均经 std log）。
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	// 生产装配形态：真实 supervisor + health reporter + tracker（main.go 装配同形）。
	// harness 与 supervisor 统一同一 fake server（supervisor 健康检查直连分配端口，
	// 端口分配恒返该 server 真端口——健康失败注入对两条链同时生效）。
	tr := appagent.NewHealthTracker()
	srv := newFakeOCServer(t)
	e := newCharxEnvOnServer(t, srv, newMockStore(), newCharxProc(), charxEnvOpts{})
	e.m.allocateServePortFn = func(_ sql.NullInt64, _ int) (int, error) { return srv.port, nil }
	seam := &mockSupStoreSeam{store: e.store}
	deps := opencode.SupervisorDeps{
		Proc:             e.proc,
		Store:            seam,
		SideFx:           &mockSupSideFx{m: e.m},
		AllocatePort:     func(int, bool, int) (int, error) { return srv.port, nil },
		OnRuntimeReady:   func(taskID string) { tr.BumpRuntimeEpoch(taskID) },
		OnRuntimeRelease: func(taskID string) { tr.ResetRuntime(taskID) },
		ReadyTimeout:     60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
		ProbeColdStartBackoff: func() []time.Duration { return []time.Duration{time.Millisecond} },
	}
	prov := opencode.NewSPIProviderWithDeps(deps)
	prov.WithHealthReporter(func(h appagent.CapabilityHealth) {
		if h.ProviderID == "" {
			h.ProviderID = opencode.ProviderID
		}
		tr.Transition(h)
	}, tr.RuntimeEpoch)
	e.m.SetRuntimeSupervisor(prov.Supervisor())

	var errReturns []string

	// ① derived 激活（supervisor Acquire）。
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	e.srv.expectDirectory("/data/worktrees/p1/t1")
	if aerr := e.charxActivate("t1"); aerr != nil {
		t.Fatalf("Activate t1 (supervisor): %v", aerr)
	}
	if _, ok := e.store.bindingRawOf("t1"); !ok {
		t.Fatal("binding 未提交（Acquire 提交分支未发生）")
	}

	// ③② legacy 导入成功 → Suspend → persist 恢复（Resume 复用分支）。
	wt := "/data/worktrees/p1/mi-prod-audit"
	e.store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
	e.store.tasks["mi-legacy"] = TaskRow{ID: "mi-legacy", ProjectID: "p1", Name: "t", Status: StatusActive,
		WorktreePath: wt, AnchorSessionID: sql.NullString{String: "ses-prod-audit", Valid: true}}
	e.srv.seedSession(wt, "ses-prod-audit", 1, 1)
	legacyName := runtimeSessionName("mi-legacy")
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.sessions[legacyName] = true
	e.proc.mockProc.envValues[legacyName] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": legacyPW,
		"OCDECK_SERVE_PORT":        fmt.Sprintf("%d", e.portHealthy()),
	}
	e.proc.mockProc.mu.Unlock()
	// ③② legacy 导入成功（runner 真实写入，密码仅 binding 列）→ persist 恢复
	//（Resume 复用分支——「重启后按绑定恢复」形态：快照/归属/binding 延续，
	// 不经 Suspend——Suspend 会清空快照破坏 resume 前置）。
	if err := e.m.RunMigrationImport(ctx); err != nil {
		t.Fatalf("RunMigrationImport: %v", err)
	}
	if raw, ok := e.store.bindingRawOf("mi-legacy"); !ok || !strings.Contains(raw, legacyPW) {
		t.Fatal("legacy 导入分支未发生（binding 缺失或密码不在位）")
	}
	if serr := e.m.persistEnvSnapshot(ctx, "t1",
		map[string]string{"OCDECK_TASK_ID": "t1", "OCDECK_SERVE_PORT": fmt.Sprintf("%d", e.portHealthy())}); serr != nil {
		t.Fatalf("persist t1 env snapshot: %v", serr)
	}
	if rerr := e.m.resumeActive(ctx, mustGet(e.store, "t1"), AlignModeRepo); rerr != nil {
		t.Fatalf("resumeActive (supervisor.Resume 复用): %v", rerr)
	}
	if row := mustGet(e.store, "t1"); row.Status != StatusActive {
		t.Fatalf("resume 后 status = %s, want active", row.Status)
	}

	// ④ legacy 健康检查失败：标记缺失候选 + server 503 → last_error、binding 零写入。
	e.store.tasks["mi-bad"] = TaskRow{ID: "mi-bad", ProjectID: "p1", Name: "t", Status: StatusActive,
		WorktreePath: wt, AnchorSessionID: sql.NullString{String: "ses-prod-audit-bad", Valid: true}}
	e.srv.seedSession(wt, "ses-prod-audit-bad", 1, 1)
	badName := runtimeSessionName("mi-bad")
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.sessions[badName] = true
	e.proc.mockProc.envValues[badName] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": legacyPW,
		"OCDECK_SERVE_PORT":        fmt.Sprintf("%d", e.portHealthy()),
	}
	e.proc.mockProc.mu.Unlock()
	e.srv.setHealthFailFirst(1 << 30)
	if err := e.m.RunMigrationImport(ctx); err != nil {
		t.Fatalf("RunMigrationImport (fail leg): %v", err)
	}
	if raw, ok := e.store.bindingRawOf("mi-bad"); ok {
		t.Fatalf("legacy 健康失败 MUST NOT 写 binding，got %q", raw)
	}
	badErr := e.store.tasks["mi-bad"].LastError
	if !badErr.Valid || badErr.String == "" {
		t.Fatal("legacy 健康失败 last_error 空（导入失败分支未落账）")
	}
	errReturns = append(errReturns, badErr.String)

	// ⑤ 激活失败补偿：健康 503（延续）+ kill 非净注入 → cleanup notice。
	charxSeedRepoWorktreeTask(e.store, "t-fail", "p1")
	e.srv.expectDirectory("/data/worktrees/p1/t-fail")
	failName := runtimeSessionName("t-fail")
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.killResults[failName] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-prod-audit"},
	}
	e.proc.mockProc.mu.Unlock()
	if aerr := e.charxActivate("t-fail"); aerr == nil {
		t.Fatal("健康失败激活必须失败（失败分支发生前置）")
	} else {
		errReturns = append(errReturns, aerr.Error())
		// 健康失败分支确实发生：supervisor 健康请求已发生（503 预算未尽——每次
		// 健康检查都失败），错误对应 supervisor 健康失败阶段（健康超时 → kill
		// 门禁非净 → serve_rotate 轮换终止，C12 分派形态）。
		if hc := srv.healthCallsSnapshot(); hc == 0 {
			t.Fatal("supervisor 健康请求未发生（健康失败分支未触发）")
		}
		if !strings.Contains(aerr.Error(), "supervisor.serve_rotate") {
			t.Errorf("激活失败错误缺失健康失败阶段标识: %v", aerr)
		}
		// 预期 cleanup ticket 落账（非净 kill 的 tickets 进入 residual notice）。
		if row := mustGet(e.store, "t-fail"); !row.Notice.Valid ||
			!strings.Contains(row.Notice.String, "tk-prod-audit") {
			t.Errorf("cleanup ticket 未落账: %+v", row.Notice)
		}
	}
	if entries := e.charxNoticesOf("t-fail"); len(entries) == 0 {
		t.Fatal("激活失败补偿 notice 空（cleanup 补偿分支未落账）")
	}

	// ⑥ 恢复 debt（生产写入入口）。
	if derr := e.m.persistRecoveryCleanupDebt(ctx, "t1", runtimeSessionName("t1"),
		[]string{"tk-prod-audit-debt"}, noticeReasonReapFailed, true, "prod audit debt fixture"); derr != nil {
		t.Fatalf("persist recovery debt: %v", derr)
	}
	if len(e.store.recoveryDebts) == 0 {
		t.Fatal("recovery_debts 空（debt 补偿分支未落账）")
	}

	// ⑦ resume 对畸形 binding：supervisor 解码错误（错误返回文本通道）。内存
	// binding 直调 supervisor.Resume（生产 seam；不写存储——测试提供的是解码
	// 失败输入，非持久化事实）；TerminalRef 会话在场 → 复用分支 decode。
	tbadProcName := runtimeSessionName("t-bad")
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.sessions[tbadProcName] = true
	e.proc.mockProc.mu.Unlock()
	if _, rerr := e.m.runtimeSupervisor.Resume(ctx,
		appagent.TaskRef{TaskID: "t-bad", Directory: "/data/worktrees/p1/t-bad"},
		domain.TaskBinding{
			Version: domain.TaskBindingVersion, ProviderID: opencode.ProviderID,
			ProviderInstanceID: opencode.ProviderInstanceID,
			UpstreamSessionID:  "ses-bad", TerminalRef: runtimeSessionName("t-bad"),
			Data: []byte(fmt.Sprintf(
				`{"port":%d,"directory":"/data/worktrees/p1/t-bad","taskId":"t-bad","auth":{"kind":"weird","password":%q}}`,
				e.portHealthy(), legacyPW)),
		},
		appagent.ResumePolicy{AllowSessionCreate: false, AllowRebuild: false}); rerr == nil {
		t.Fatal("畸形 binding resume 必须失败（解码错误分支前置）")
	} else {
		if !strings.Contains(rerr.Error(), "未知 auth.kind") {
			t.Fatalf("⑦ 未到达解码分支（fixture 错位），错误: %v", rerr)
		}
		errReturns = append(errReturns, rerr.Error())
	}

	// --- 扫描面 ---
	// 秘密集合覆盖全部实际生成凭据的任务：t1（激活成功）与 t-fail（激活失败
	// 但进程已派生凭据——错误文本若夹带其派生值同样必须检出）。
	derivedPW := charxDerivedOf("t1")
	secrets := []string{derivedPW, charxDerivedOf("t-fail"), legacyPW, charxMasterSecret}

	// 错误返回文本。
	for i, msg := range errReturns {
		for _, s := range secrets {
			if strings.Contains(msg, s) {
				t.Errorf("错误返回[%d] 泄露凭据材料（%.8s…）", i, s)
			}
		}
	}
	// 存储：tasks 列 + bindings + recoveryDebts（legacy 密码仅允许 t-legacy binding 列）。
	e.store.mu.Lock()
	for taskID, row := range e.store.tasks {
		for name, val := range map[string]string{
			"last_error":   row.LastError.String,
			"notice":       row.Notice.String,
			"env_snapshot": row.EnvSnapshot.String,
		} {
			if val == "" {
				continue
			}
			for _, s := range secrets {
				if strings.Contains(val, s) {
					t.Errorf("tasks.%s(%s) 泄露凭据材料（%.8s…）", name, taskID, s)
				}
			}
		}
	}
	for taskID, raw := range e.store.bindings {
		if taskID == "mi-legacy" {
			if strings.Contains(raw, derivedPW) || strings.Contains(raw, charxMasterSecret) {
				t.Errorf("legacy binding（唯一允许位置）出现非 legacy 凭据材料")
			}
			continue
		}
		for _, s := range secrets {
			if strings.Contains(raw, s) {
				t.Errorf("task_binding(%s) 泄露凭据材料（%.8s…）", taskID, s)
			}
		}
	}
	for key, debt := range e.store.recoveryDebts {
		joined := fmt.Sprintf("%+v", debt)
		for _, s := range secrets {
			if strings.Contains(joined, s) {
				t.Errorf("recovery_debts(%s) 泄露凭据材料（%.8s…）", key, s)
			}
		}
	}
	e.store.mu.Unlock()
	// 日志。
	for _, s := range secrets {
		if strings.Contains(logBuf.String(), s) {
			t.Errorf("日志输出泄露凭据材料（%.8s…）", s)
		}
	}
	// 事件序列化载荷（json.Marshal）。
	e.pub.mu.Lock()
	var evDump strings.Builder
	for _, ev := range e.pub.events {
		b, merr := json.Marshal(ev.Payload)
		if merr != nil {
			e.pub.mu.Unlock()
			t.Fatalf("marshal event payload: %v", merr)
		}
		evDump.Write(b)
	}
	e.pub.mu.Unlock()
	for _, s := range secrets {
		if strings.Contains(evDump.String(), s) {
			t.Errorf("领域事件序列化载荷泄露凭据材料（%.8s…）", s)
		}
	}
}
