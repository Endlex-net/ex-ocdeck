// permission_mode_test.go 验证任务权限模式运行态（task-permission-mode tasks 2.4-2.8，
// design D5/D6/D8）：共享校验、D5 统一初始化表与有效模式推导全分支（含 DR1）、
// converge epoch 矩阵（同值不开 epoch）、启动事实三路径矩阵（写入/写失败不建进程/
// shell 与 temp serve 负向/NULL 与非法值拒绝注册）、epoch 屏障（切出在途不回复、
// 切入补判、旧 epoch 迟到不写状态）、gate 与 PermissionModeView 同源 AIAutoEnabled。
package task

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"ocdeck/internal/application"
)

// --- tasks 1.2：共享三值校验（创建与模式端点同构） ---

func TestNormalizePermissionModeInput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "ask", in: "ask", want: "ask"},
		{name: "all-approve", in: "all-approve", want: "all-approve"},
		{name: "ai-auto", in: "ai-auto", want: "ai-auto"},
		{name: "trim 后三值", in: "  ai-auto  ", want: "ai-auto"},
		{name: "空串拒绝（缺省归 ask 属端点语义）", in: "", wantErr: true},
		{name: "纯空白拒绝", in: "   ", wantErr: true},
		{name: "未知值拒绝", in: "bogus", wantErr: true},
		{name: "trim 后未知值拒绝", in: " always-approve ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizePermissionModeInput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalize(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- tasks 2.4/2.8：D5 统一初始化表 + D6 有效模式推导（全分支含 DR1） ---

// newPermModeEnv 构造带 (启动事实, 持久化模式) 任务与 runtime 的最小环境。
func newPermModeEnv(t *testing.T, startFact, savedMode string) (*Manager, *mockStore, *taskRuntime) {
	t.Helper()
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.PermissionMode = savedMode })
	store.seedPermissionModeAtStart("t1", startFact)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	row, err := store.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	rt := m.newRuntime("t1")
	if err := initPermState(rt, row, startFact); err != nil {
		t.Fatalf("initPermState: %v", err)
	}
	m.setRuntime("t1", rt)
	return m, store, rt
}

// TestInitPermState_AndDerive_Table D5 统一初始化表 × D6 推导全分支（三条注册路径
// 共用 initPermState 单一公式；DR1：--auto 进程恒 all-approve）。
func TestInitPermState_AndDerive_Table(t *testing.T) {
	cases := []struct {
		name            string
		startFact       string
		savedMode       string
		wantStartedAuto bool
		wantAIAuto      bool
		wantView        application.PermissionModeView
	}{
		{name: "ask 进程 + ask", startFact: "ask", savedMode: "ask",
			wantView: application.PermissionModeView{PermissionMode: "ask", EffectivePermissionMode: "ask"}},
		{name: "ask 进程 + 保存 ai-auto", startFact: "ask", savedMode: "ai-auto",
			wantAIAuto: true, wantView: application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "ai-auto"}},
		{name: "ask 进程 + 保存 all-approve 直至下次激活为 ask", startFact: "ask", savedMode: "all-approve",
			wantView: application.PermissionModeView{PermissionMode: "all-approve", EffectivePermissionMode: "ask"}},
		{name: "ai-auto 进程 + 保存 ask 重启恢复不启用", startFact: "ai-auto", savedMode: "ask",
			wantView: application.PermissionModeView{PermissionMode: "ask", EffectivePermissionMode: "ask"}},
		{name: "ai-auto 进程 + 保存 all-approve 重启恢复不启用", startFact: "ai-auto", savedMode: "all-approve",
			wantView: application.PermissionModeView{PermissionMode: "all-approve", EffectivePermissionMode: "ask"}},
		{name: "DR1：--auto 进程保存 ai-auto 仍不启用（有效 all-approve）", startFact: "all-approve", savedMode: "ai-auto",
			wantStartedAuto: true, wantView: application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "all-approve"}},
		{name: "--auto 进程 + all-approve", startFact: "all-approve", savedMode: "all-approve",
			wantStartedAuto: true, wantView: application.PermissionModeView{PermissionMode: "all-approve", EffectivePermissionMode: "all-approve"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := newPermModeEnv(t, tc.startFact, tc.savedMode)
			state := m.permStateSnapshot("t1")
			if state == nil {
				t.Fatal("permState must be initialized")
			}
			if state.StartedWithAuto != tc.wantStartedAuto || state.AIAutoEnabled != tc.wantAIAuto {
				t.Errorf("state = %+v, want StartedWithAuto=%v AIAutoEnabled=%v",
					state, tc.wantStartedAuto, tc.wantAIAuto)
			}
			view, err := derivePermissionModeView(state, TaskRow{})
			if err != nil {
				t.Fatalf("derive: %v", err)
			}
			if view != tc.wantView {
				t.Errorf("view = %+v, want %+v", view, tc.wantView)
			}
			// Manager.PermissionModeView 同源（同一 state 指针读取）。
			got, err := m.PermissionModeView(context.Background(), "t1")
			if err != nil {
				t.Fatalf("PermissionModeView: %v", err)
			}
			if got != tc.wantView {
				t.Errorf("Manager view = %+v, want %+v", got, tc.wantView)
			}
		})
	}
}

// TestDerivePermissionModeView_NoRuntime 无 runtime（state=nil）时读 DB 行推导；
// 持久化值非法 fail-closed。
func TestDerivePermissionModeView_NoRuntime(t *testing.T) {
	view, err := derivePermissionModeView(nil, TaskRow{ID: "t1", PermissionMode: ""})
	if err != nil || view != (application.PermissionModeView{PermissionMode: "ask", EffectivePermissionMode: "ask"}) {
		t.Fatalf("view=(%+v,%v), want ask/ask（空串归 ask）", view, err)
	}
	view, err = derivePermissionModeView(nil, TaskRow{ID: "t1", PermissionMode: "ai-auto"})
	if err != nil || view != (application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "ai-auto"}) {
		t.Fatalf("view=(%+v,%v), want ai-auto/ai-auto", view, err)
	}
	if _, err := derivePermissionModeView(nil, TaskRow{ID: "t1", PermissionMode: "bogus"}); err == nil {
		t.Fatal("unknown persisted mode must fail closed")
	}
}

// --- tasks 2.8：converge epoch 矩阵 + gate/view 同源 ---

// TestConvergePermissionModeSave_EpochMatrix D5 算法：仅真实「有效 ai-auto 行为」切换
// 才 permEpoch+1；同值保存不开 epoch；切入清空 judgedPerms 并返回 needScan；DR1 不动 epoch。
func TestConvergePermissionModeSave_EpochMatrix(t *testing.T) {
	cases := []struct {
		name       string
		startFact  string
		savedMode  string
		saveMode   string
		wantScan   bool
		wantEpoch  uint64
		wantAIAuto bool
	}{
		{name: "同值保存不开 epoch", startFact: "ai-auto", savedMode: "ai-auto", saveMode: "ai-auto",
			wantScan: false, wantEpoch: 0, wantAIAuto: true},
		{name: "切入 ai-auto epoch+1 补判", startFact: "ask", savedMode: "ask", saveMode: "ai-auto",
			wantScan: true, wantEpoch: 1, wantAIAuto: true},
		{name: "切出 ask epoch+1", startFact: "ai-auto", savedMode: "ai-auto", saveMode: "ask",
			wantScan: false, wantEpoch: 1, wantAIAuto: false},
		{name: "切出 all-approve epoch+1", startFact: "ai-auto", savedMode: "ai-auto", saveMode: "all-approve",
			wantScan: false, wantEpoch: 1, wantAIAuto: false},
		{name: "ask→all-approve 非 ai-auto 行为切换不动 epoch", startFact: "ask", savedMode: "ask", saveMode: "all-approve",
			wantScan: false, wantEpoch: 0, wantAIAuto: false},
		{name: "DR1：--auto 进程保存 ai-auto 不动 epoch 不补判", startFact: "all-approve", savedMode: "ask", saveMode: "ai-auto",
			wantScan: false, wantEpoch: 0, wantAIAuto: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, rt := newPermModeEnv(t, tc.startFact, tc.savedMode)
			rt.judgedPerms["r1"] = struct{}{}
			needScan := m.convergePermissionModeSave("t1", tc.saveMode)
			if needScan != tc.wantScan {
				t.Errorf("needScan = %v, want %v", needScan, tc.wantScan)
			}
			rt.mu.Lock()
			epoch, aiAuto := rt.permEpoch, rt.permState.AIAutoEnabled
			_, judged := rt.judgedPerms["r1"]
			rt.mu.Unlock()
			if epoch != tc.wantEpoch || aiAuto != tc.wantAIAuto {
				t.Errorf("epoch/AIAuto = %d/%v, want %d/%v", epoch, aiAuto, tc.wantEpoch, tc.wantAIAuto)
			}
			if tc.wantScan && judged {
				t.Error("切入 MUST 清空 judgedPerms（per-epoch 计数契约）")
			}
			if !tc.wantScan && !judged {
				t.Error("非切换 MUST NOT 清空 judgedPerms")
			}
		})
	}
}

// TestConvergePermissionModeSave_NoRuntime 无 runtime 时 no-op。
func TestConvergePermissionModeSave_NoRuntime(t *testing.T) {
	m, _, _ := newPermModeEnv(t, "ask", "ask")
	m.clearRuntime("t1")
	if m.convergePermissionModeSave("t1", "ai-auto") {
		t.Fatal("no runtime must be a no-op returning false")
	}
}

// TestJudgeScan_DR1_NoRegisterNoJudge 评审 I1 回归：DR1 runtime（StartedWithAuto=
// true、持久化模式 ai-auto、AIAutoEnabled=false）的持久化模式预检放行，但登记前
// 准入（与 permGate 同构）必须拦截——不登记 judgedPerms、不登记 inflight、不调 LLM、
// 无 verdict 事件；对照组：AIAutoEnabled=true 的 runtime 同一扫描正常登记并判定。
func TestJudgeScan_DR1_NoRegisterNoJudge(t *testing.T) {
	setup := func(t *testing.T, startFact string) (*Manager, *taskRuntime, *fakeJudge, *recordingPublisher) {
		t.Helper()
		m, _, rt := newPermModeEnv(t, startFact, "ai-auto")
		judge := newFakeJudge(PermissionVerdictApprove)
		m.judge = judge
		rec := &recordingPublisher{}
		m.publish = rec
		rt.setPermJudgeReady()
		rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
		return m, rt, judge, rec
	}
	t.Run("AIAutoEnabled=false（DR1）不登记不判定", func(t *testing.T) {
		// StartedWithAuto=true 的 runtime：持久化模式预检（mode==ai-auto）放行，
		// 但登记前准入按 AIAutoEnabled=false 拦截（D5 DR1：不启用 AI、不补判）。
		m, rt, judge, rec := setup(t, PermissionModeAllApprove)
		if st := m.permStateSnapshot("t1"); st == nil || !st.StartedWithAuto || st.AIAutoEnabled {
			t.Fatalf("prereq state = %+v, want StartedWithAuto=true AIAutoEnabled=false", st)
		}
		m.judgeScan(rt)
		time.Sleep(50 * time.Millisecond)
		if got := len(judge.judgeCalls()); got != 0 {
			t.Fatalf("DR1 runtime MUST NOT judge, calls = %d", got)
		}
		rt.mu.Lock()
		_, judged := rt.judgedPerms["r1"]
		rec1, inflight := rt.permVerdicts["r1"]
		rt.mu.Unlock()
		if judged {
			t.Error("DR1 runtime MUST NOT register judgedPerms")
		}
		if inflight {
			t.Errorf("DR1 runtime MUST NOT register inflight state, got %+v", rec1)
		}
		if got := len(rec.snapshot()); got != 0 {
			t.Fatalf("events = %d, want 0（无终态即无唤醒事件）", got)
		}
	})
	t.Run("对照组 AIAutoEnabled=true 正常登记", func(t *testing.T) {
		m, rt, judge, _ := setup(t, PermissionModeAIAuto)
		if st := m.permStateSnapshot("t1"); st == nil || !st.AIAutoEnabled {
			t.Fatalf("prereq state = %+v, want AIAutoEnabled=true", st)
		}
		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
		rt.mu.Lock()
		_, judged := rt.judgedPerms["r1"]
		rec2, _ := rt.permVerdicts["r1"]
		rt.mu.Unlock()
		if !judged {
			t.Fatal("enabled runtime must register judgedPerms")
		}
		if rec2.epoch != 0 {
			t.Errorf("verdict record epoch = %d, want 0", rec2.epoch)
		}
	})
}

// TestPermissionModeView_GateReadsSameAIAutoEnabled 模式切换后 permGate 与
// PermissionModeView 读取同一 AIAutoEnabled 结论（D5：唯一事实源）。
func TestPermissionModeView_GateReadsSameAIAutoEnabled(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, _, rt, _ := newAutoTaskEnv(t, judge)
	ctx := context.Background()
	tok := rt.instVersion

	assertConsistent := func(wantAIAuto bool, epoch uint64) {
		t.Helper()
		view, err := m.PermissionModeView(ctx, "t1")
		if err != nil {
			t.Fatalf("PermissionModeView: %v", err)
		}
		if (view.EffectivePermissionMode == PermissionModeAIAuto) != wantAIAuto {
			t.Fatalf("view = %+v, want effective ai-auto=%v", view, wantAIAuto)
		}
		if got := m.permGate(ctx, rt, tok, epoch); got != wantAIAuto {
			t.Errorf("permGate(epoch=%d) = %v, want %v（与 view 同源）", epoch, got, wantAIAuto)
		}
	}

	assertConsistent(true, 0)
	// 切出：gate 立即拒绝（AIAutoEnabled=false 且 epoch 失配），view 同步 ask。
	if m.convergePermissionModeSave("t1", PermissionModeAsk) {
		t.Fatal("切出 must not request scan")
	}
	assertConsistent(false, 1)
	// 切入：gate 以新 epoch 放行，旧 epoch 捕获仍拒绝。
	if !m.convergePermissionModeSave("t1", PermissionModeAIAuto) {
		t.Fatal("切入 must request scan")
	}
	assertConsistent(true, 2)
	if m.permGate(ctx, rt, tok, 0) {
		t.Error("旧 epoch 捕获 MUST 被拒绝")
	}
}

// TestPermGate_NilState 无状态 runtime 拒绝判定。
func TestPermGate_NilState(t *testing.T) {
	m, _, rt := newPermModeEnv(t, "ask", "ask")
	// 直接构造一个未初始化 permState 的替代 runtime（模拟注册前窗口）。
	blank := m.newRuntime("t1")
	m.setRuntime("t1", blank)
	blank.setPermJudgeReady()
	if m.permGate(context.Background(), blank, blank.instVersion, 0) {
		t.Fatal("nil permState must reject (AIAutoEnabled 无独立布尔，nil 即未启用)")
	}
	_ = rt
}

// --- tasks 2.7：epoch 屏障判定流 ---

// TestEpochBarrier_SwitchedOutLateVerdictThenSwitchIn 切出后在途判定不回复、
// 不写终态（迟到判定按 epoch 失配丢弃，无事件接缝触发）；切回 ai-auto 后 judged-set
// 清空重判恰好一次，终态以新 epoch 提交。
func TestEpochBarrier_SwitchedOutLateVerdictThenSwitchIn(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	release := make(chan struct{})
	judge.block = release
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	fake := &fakePermAudit{}
	m.permAudit = fake
	rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt)
	<-judge.entered // 判定在途（捕获 epoch 0）
	// 切出（保存 ask，仅收敛内存态——DB 提交先于收敛属协调器序，此处聚焦屏障本身）。
	if m.convergePermissionModeSave("t1", PermissionModeAsk) {
		t.Fatal("切出 must not request scan")
	}
	close(release)
	// 迟到终态：发送前 gate 拒绝（epoch 失配 + AIAutoEnabled=false）→ 零回复、
	// 审计留痕 APPROVE + not_applicable、判定状态保持旧 epoch inflight。
	eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Fatalf("switched-out late verdict MUST NOT reply, replies = %d", got)
	}
	rec := fake.snapshot()[0]
	if rec.Verdict != "APPROVE" || rec.ReplyResult != "not_applicable" {
		t.Errorf("audit = %+v, want APPROVE + not_applicable（留痕不受 gate 拦截）", rec)
	}
	rt.mu.Lock()
	got := rt.permVerdicts["r1"]
	rt.mu.Unlock()
	if got.epoch != 0 || got.state != permVerdictInflight {
		t.Errorf("verdict record = %+v, want {epoch 0, inflight}（旧 epoch 终态不写）", got)
	}

	// 切回 ai-auto：judged-set 清空 → 重判恰好一次，本次回复并以新 epoch 提交 settled。
	if !m.convergePermissionModeSave("t1", PermissionModeAIAuto) {
		t.Fatal("切入 must request scan")
	}
	m.judgeScan(rt) // 锁外（converge 返回即已释放 rt.mu）
	eventually(t, 2*time.Second, func() bool {
		return len(judge.judgeCalls()) == 2 && len(oc.replyPermissionCallsSnapshot()) == 1
	})
	rt.mu.Lock()
	got = rt.permVerdicts["r1"]
	rt.mu.Unlock()
	if got.epoch != 2 || got.state != permVerdictSettledNoNotify {
		t.Errorf("verdict record = %+v, want {epoch 2, settled_no_notify}", got)
	}
	// 稳定后不重复判定（per-epoch/per-request 计数契约）。
	time.Sleep(50 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 2 {
		t.Errorf("judge calls = %d, want 2", got)
	}
}

// TestCommitPermVerdict_EpochMismatch 终态提交单元级屏障：epoch 失配 MUST NOT 写状态。
func TestCommitPermVerdict_EpochMismatch(t *testing.T) {
	_, _, rt := newPermModeEnv(t, "ai-auto", "ai-auto")
	if rt.commitPermVerdict(1, "r1", permVerdictManualRequired) {
		t.Fatal("epoch mismatch commit must return false")
	}
	rt.mu.Lock()
	_, exists := rt.permVerdicts["r1"]
	rt.mu.Unlock()
	if exists {
		t.Fatal("epoch mismatch MUST NOT write verdict state")
	}
	if !rt.commitPermVerdict(0, "r1", permVerdictManualRequired) {
		t.Fatal("matching epoch commit must return true")
	}
}

// --- tasks 2.5：启动事实写入矩阵 ---

// TestStartFact_StartRuntimeWritesResolvedMode startRuntimeWithPortRetry 建进程前写入
// 本次 argv 实际模式值。
func TestStartFact_StartRuntimeWritesResolvedMode(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.PermissionMode = PermissionModeAIAuto })
	// mutTask 后重取行（startRuntimeWithPortRetry 以传入行解析 argv 模式）。
	row, err := store.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	withFastServeReady(m)
	env, err := m.mergeEnvSnapshot(context.Background(), row, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, ""); err != nil {
		t.Fatalf("startServeWithPortRetry: %v", err)
	}
	store.mu.Lock()
	got, ok := store.permissionModeAtStart["t1"]
	store.mu.Unlock()
	if !ok || got != PermissionModeAIAuto {
		t.Errorf("permission_mode_at_start = %q (present=%v), want ai-auto", got, ok)
	}
}

// TestStartFact_WriteFailureNoProcess 写失败 MUST NOT 建进程（fail-closed 先于副作用）。
func TestStartFact_WriteFailureNoProcess(t *testing.T) {
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	withFastServeReady(m)
	store.setPermModeAtStartErr = errors.New("db down")
	env, err := m.mergeEnvSnapshot(context.Background(), row, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, ""); err == nil {
		t.Fatal("start fact write failure must fail the start")
	}
	if got := len(proc.newSessionNames); got != 0 {
		t.Errorf("NewSession calls = %d, want 0（写失败不建进程）", got)
	}
}

// TestStartFact_ShellAndTempServeDoNotWrite shell 与 temp serve 的 NewSession 不写启动事实。
func TestStartFact_ShellAndTempServeDoNotWrite(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, store, _, rt, _ := newAutoTaskEnv(t, judge)
	rt.setPermJudgeReady()
	// CreateShell 读 env 快照（与启动事实无关）。
	snapBytes, _ := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t1"}})
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
	store.mu.Lock()
	before := len(store.permissionModeAtStart)
	store.mu.Unlock()
	if _, err := m.CreateShell(context.Background(), "t1"); err != nil {
		t.Fatalf("CreateShell: %v", err)
	}
	store.mu.Lock()
	after := len(store.permissionModeAtStart)
	store.mu.Unlock()
	if after != before {
		t.Errorf("start fact map size %d -> %d（shell NewSession MUST NOT 写启动事实）", before, after)
	}

	// temp serve：直接调用删除清理路径的一次性 serve（无 runtime 场景）。
	store2 := newMockStore()
	row := seedSuspendedTask(store2, "t2", "p1")
	m2 := newTestManager(t, store2, newMockProc(), newMockWorktree(), newMockOC(true))
	if _, _, err := m2.startTempServe(context.Background(), row); err != nil {
		t.Fatalf("startTempServe: %v", err)
	}
	store2.mu.Lock()
	_, written := store2.permissionModeAtStart["t2"]
	store2.mu.Unlock()
	if written {
		t.Error("temp serve NewSession MUST NOT write start fact")
	}
}

// --- tasks 2.6：注册前读校验（NULL/非法 fail-closed，合法值恢复 StartedWithAuto） ---

// newResumeStartFactEnv 构造 resumeActive 前置（active 任务 + env 快照 + 存活健康 runtime 会话）。
func newResumeStartFactEnv(t *testing.T) (*Manager, *mockStore, *mockProc) {
	t.Helper()
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusActive
		r.PermissionMode = PermissionModeAsk
	})
	snap := envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
		"OCDECK_TASK_ID":           "t1",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	return m, store, proc
}

func TestResumeActive_StartFactGate(t *testing.T) {
	t.Run("NULL 拒绝注册", func(t *testing.T) {
		m, store, _ := newResumeStartFactEnv(t) // 不 seed 启动事实
		row, _ := store.GetTask(context.Background(), "t1")
		if err := m.resumeActive(context.Background(), row, AlignModeRepo); err == nil {
			t.Fatal("resumeActive with NULL start fact must fail")
		}
		if m.getRuntime("t1") != nil {
			t.Error("runtime MUST NOT be registered on NULL start fact")
		}
	})
	t.Run("非法值 fail-closed", func(t *testing.T) {
		m, store, _ := newResumeStartFactEnv(t)
		store.seedPermissionModeAtStart("t1", "bogus")
		row, _ := store.GetTask(context.Background(), "t1")
		if err := m.resumeActive(context.Background(), row, AlignModeRepo); err == nil {
			t.Fatal("resumeActive with invalid start fact must fail")
		}
		if m.getRuntime("t1") != nil {
			t.Error("runtime MUST NOT be registered on invalid start fact")
		}
	})
	t.Run("合法 all-approve 恢复 StartedWithAuto", func(t *testing.T) {
		m, store, _ := newResumeStartFactEnv(t)
		store.seedPermissionModeAtStart("t1", PermissionModeAllApprove)
		row, _ := store.GetTask(context.Background(), "t1")
		if err := m.resumeActive(context.Background(), row, AlignModeRepo); err != nil {
			t.Fatalf("resumeActive: %v", err)
		}
		rt := m.getRuntime("t1")
		if rt == nil {
			t.Fatal("runtime must be registered")
		}
		state := m.permStateSnapshot("t1")
		if state == nil || !state.StartedWithAuto || state.AIAutoEnabled {
			t.Errorf("state = %+v, want StartedWithAuto=true AIAutoEnabled=false", state)
		}
		view, err := m.PermissionModeView(context.Background(), "t1")
		if err != nil {
			t.Fatalf("PermissionModeView: %v", err)
		}
		if view.EffectivePermissionMode != PermissionModeAllApprove {
			t.Errorf("effective = %q, want all-approve", view.EffectivePermissionMode)
		}
	})
}

func TestTryRepairRuntime_StartFactGate(t *testing.T) {
	newRepairEnv := func(t *testing.T) (*Manager, *mockStore) {
		t.Helper()
		store := newMockStore()
		seedSuspendedTask(store, "t1", "p1")
		store.mutTask("t1", func(r *TaskRow) {
			r.Status = StatusActive
			r.PermissionMode = PermissionModeAsk
			r.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
			r.LastPort = sql.NullInt64{Int64: 50001, Valid: true}
		})
		proc := newMockProc()
		proc.sessions[serveSessionName("t1")] = true
		proc.envValues[serveSessionName("t1")] = map[string]string{
			"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
		}
		m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
		return m, store
	}
	t.Run("NULL 拒绝注册", func(t *testing.T) {
		m, _ := newRepairEnv(t)
		if fixed, err := m.tryRepairRuntime(context.Background(), "t1", AlignModeRepo); err == nil || fixed {
			t.Fatalf("tryRepairRuntime = (%v, %v), want (false, err)", fixed, err)
		}
		if m.getRuntime("t1") != nil {
			t.Error("runtime MUST NOT be registered on NULL start fact")
		}
	})
	t.Run("合法值修复并恢复 StartedWithAuto", func(t *testing.T) {
		m, store := newRepairEnv(t)
		store.seedPermissionModeAtStart("t1", PermissionModeAllApprove)
		fixed, err := m.tryRepairRuntime(context.Background(), "t1", AlignModeRepo)
		if err != nil || !fixed {
			t.Fatalf("tryRepairRuntime = (%v, %v), want (true, nil)", fixed, err)
		}
		state := m.permStateSnapshot("t1")
		if state == nil || !state.StartedWithAuto {
			t.Errorf("state = %+v, want StartedWithAuto=true", state)
		}
	})
}
