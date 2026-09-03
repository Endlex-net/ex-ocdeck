package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// 本文件补齐 tasks.md 1.16-1.19 P1 门禁覆盖差距（审计后真实缺口）。
// 复用 mock_test.go 的 mockStore/mockProc/mockWorktree/mockOC/newTestManager 基础设施。
// 覆盖项：
//   1.16 mergeEnvSnapshot 合并优先级 + reserved keys 不进快照 + envSnapshot 不含 OPENCODE_SERVER_PASSWORD
//   1.17 dispositionToNotice 五值映射 + newRuntime generation 单调
//   1.17 reconcile persist 矩阵补例（creating→creation_failed、creation_failed 保持、suspending→suspended、
//        archived/deletion_failed 无 runtime 保持、resumeActive 中途失败→suspended）
//   1.17 snapshot_failed 无 tickets + 会话消失 + 立即 Activate 被拒（notice 存在性门禁）
//   1.17 absent-at-entry 幂等短路（killTaskSessions 对已不存在会话视为幂等成功）
//   1.19 并发 ReopenAttach 幂等复用（不产生 409，复用同一 TUI 会话）

// --- env store（覆盖 ListProjectEnvVars/ListTaskEnvVars 供 mergeEnvSnapshot 优先级测试） ---

// envVarsStore 包装 mockStore，注入项目级/任务级/全局级 env 变量。
type envVarsStore struct {
	*mockStore
	projVars   map[string][]EnvVarRow
	taskVars   map[string][]EnvVarRow
	globalVars []GlobalEnvVarRow
}

func (s *envVarsStore) ListProjectEnvVars(ctx context.Context, projectID string) ([]EnvVarRow, error) {
	return s.projVars[projectID], nil
}

func (s *envVarsStore) ListTaskEnvVars(ctx context.Context, taskID string) ([]EnvVarRow, error) {
	return s.taskVars[taskID], nil
}

func (s *envVarsStore) ListGlobalEnvVars(ctx context.Context) ([]GlobalEnvVarRow, error) {
	return s.globalVars, nil
}

// TestMergeEnvSnapshot_PriorityAndReservedKeys 验证 design.md §2 env 合并优先级：
// 基础集 < 项目级 < 任务级 < 生命周期变量(OCDECK_*)；reserved keys 不可被用户 env 覆盖且不进快照。
func TestMergeEnvSnapshot_PriorityAndReservedKeys(t *testing.T) {
	// 控制宿主基础集 env（hostEnv 读 os.LookupEnv）。
	t.Setenv("TERM", "xterm-host")
	t.Setenv("HOME", "/home/host")
	t.Setenv("PATH", "/usr/host/bin")
	t.Setenv("LANG", "C")

	store := &envVarsStore{
		mockStore: newMockStore(),
		projVars: map[string][]EnvVarRow{
			"p1": {
				{Key: "TERM", Value: "xterm-proj"},                    // 覆盖基础集
				{Key: "MY_PROJ_VAR", Value: "proj"},                   // 项目级独有
				{Key: "OPENCODE_SERVER_PASSWORD", Value: "leak-proj"}, // reserved 不得进快照
				{Key: "OCDECK_TASK_ID", Value: "leak-proj"},           // reserved 不得被用户覆盖
			},
		},
		taskVars: map[string][]EnvVarRow{
			"t1": {
				{Key: "TERM", Value: "xterm-task"},                 // 覆盖项目级
				{Key: "MY_TASK_VAR", Value: "task"},                // 任务级独有
				{Key: "MY_PROJ_VAR", Value: "task-overrides-proj"}, // 覆盖项目级
				{Key: "OCDECK_SERVE_PORT", Value: "leak-task"},     // reserved 不得被用户覆盖
			},
		},
	}
	seedSuspendedTask(store.mockStore, "t1", "p1")
	// mergeEnvSnapshot 依赖 ListAllTasks? 不直接；依赖 row.ProjectID/row.ID 已在 seed。
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	merged, err := m.mergeEnvSnapshot(context.Background(), store.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("mergeEnvSnapshot: %v", err)
	}

	// 基础集（未覆盖项保留 host 值）。
	if merged["HOME"] != "/home/host" {
		t.Errorf("HOME base = %q, want /home/host", merged["HOME"])
	}
	if merged["PATH"] != "/usr/host/bin" {
		t.Errorf("PATH base = %q, want /usr/host/bin", merged["PATH"])
	}
	if merged["LANG"] != "C" {
		t.Errorf("LANG base = %q, want C", merged["LANG"])
	}
	// 优先级：任务级 > 项目级 > 基础集。
	if merged["TERM"] != "xterm-task" {
		t.Errorf("TERM priority = %q, want xterm-task (task > project > base)", merged["TERM"])
	}
	if merged["MY_PROJ_VAR"] != "task-overrides-proj" {
		t.Errorf("MY_PROJ_VAR = %q, want task-overrides-proj (task overrides project)", merged["MY_PROJ_VAR"])
	}
	if merged["MY_TASK_VAR"] != "task" {
		t.Errorf("MY_TASK_VAR = %q, want task", merged["MY_TASK_VAR"])
	}
	// 生命周期变量由内部注入（reserved 不得被用户 env 覆盖）。
	if merged["OCDECK_SERVE_PORT"] != "50001" {
		t.Errorf("OCDECK_SERVE_PORT = %q, want 50001 (internal injection, not user-overridable)", merged["OCDECK_SERVE_PORT"])
	}
	if merged["OCDECK_TASK_ID"] != "t1" {
		t.Errorf("OCDECK_TASK_ID = %q, want t1 (internal injection, not user-overridable)", merged["OCDECK_TASK_ID"])
	}
	// role-specific secret MUST NOT 进合并结果（也不进持久化快照）。
	if _, ok := merged["OPENCODE_SERVER_PASSWORD"]; ok {
		t.Error("OPENCODE_SERVER_PASSWORD MUST NOT be in merged env (reserved secret)")
	}

	// 持久化快照不含 reserved keys / secret。
	row := store.tasks["t1"]
	if !row.EnvSnapshot.Valid {
		t.Fatal("env_snapshot not persisted")
	}
	var snap envSnapshot
	if err := json.Unmarshal([]byte(row.EnvSnapshot.String), &snap); err != nil {
		t.Fatalf("unmarshal persisted snapshot: %v", err)
	}
	if _, ok := snap.Vars["OPENCODE_SERVER_PASSWORD"]; ok {
		t.Error("OPENCODE_SERVER_PASSWORD MUST NOT be in persisted env_snapshot (role-specific secret)")
	}
	// OCDECK_SERVE_PORT/OCDECK_TASK_ID 属生命周期变量，进快照（§2：通用生命周期变量进快照）。
	if snap.Vars["OCDECK_SERVE_PORT"] != "50001" {
		t.Errorf("snapshot OCDECK_SERVE_PORT = %q, want 50001", snap.Vars["OCDECK_SERVE_PORT"])
	}
	if snap.Vars["OCDECK_TASK_ID"] != "t1" {
		t.Errorf("snapshot OCDECK_TASK_ID = %q, want t1", snap.Vars["OCDECK_TASK_ID"])
	}
	// 合并结果与快照一致（同源 merged）。
	if snap.Vars["TERM"] != "xterm-task" {
		t.Errorf("snapshot TERM = %q, want xterm-task", snap.Vars["TERM"])
	}
}

// TestMergeEnvSnapshot_GlobalLayer 验证全局级 env 层（design.md §2：基础集 < 全局级 < 项目级 < 任务级）。
// 覆盖：manual 存值、follow_host 从服务端进程 env 解析、follow_host 宿主未设置跳过、
// 全局级被项目级/任务级覆盖、reserved key 在全局级同样被忽略并跳过。
func TestMergeEnvSnapshot_GlobalLayer(t *testing.T) {
	// 控制宿主基础集与 follow_host 解析（hostEnv 读 os.LookupEnv）。
	t.Setenv("TERM", "xterm-host")
	t.Setenv("HOME", "/home/host")
	t.Setenv("PATH", "/usr/host/bin")
	t.Setenv("GLOBAL_FOLLOW", "resolved-from-host")
	// GLOBAL_FOLLOW_UNSET 不设置 → follow_host 跳过。

	store := &envVarsStore{
		mockStore: newMockStore(),
		globalVars: []GlobalEnvVarRow{
			{Key: "GLOBAL_MANUAL", Mode: "manual", Value: "manual-val"},
			{Key: "GLOBAL_FOLLOW", Mode: "follow_host", Value: "ignored"},
			{Key: "GLOBAL_FOLLOW_UNSET", Mode: "follow_host", Value: "ignored"},
			{Key: "HOME", Mode: "manual", Value: "/home/global"},                    // 全局级覆盖基础集 HOME
			{Key: "SHARED", Mode: "manual", Value: "global"},                        // 同时在项目/任务级，验证优先级
			{Key: "OPENCODE_SERVER_PASSWORD", Mode: "manual", Value: "leak-global"}, // reserved 跳过
			{Key: "OCDECK_TASK_ID", Mode: "manual", Value: "leak-global"},           // reserved 跳过
		},
		projVars: map[string][]EnvVarRow{
			"p1": {{Key: "SHARED", Value: "project"}}, // 项目级覆盖全局级
		},
		taskVars: map[string][]EnvVarRow{
			"t1": {{Key: "SHARED", Value: "task"}}, // 任务级覆盖项目级
		},
	}
	seedSuspendedTask(store.mockStore, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	merged, err := m.mergeEnvSnapshot(context.Background(), store.tasks["t1"], 50001)
	if err != nil {
		t.Fatalf("mergeEnvSnapshot: %v", err)
	}

	// manual 存值。
	if merged["GLOBAL_MANUAL"] != "manual-val" {
		t.Errorf("GLOBAL_MANUAL = %q, want manual-val", merged["GLOBAL_MANUAL"])
	}
	// follow_host 从服务端进程 env 解析（value 字段被忽略）。
	if merged["GLOBAL_FOLLOW"] != "resolved-from-host" {
		t.Errorf("GLOBAL_FOLLOW = %q, want resolved-from-host", merged["GLOBAL_FOLLOW"])
	}
	// follow_host 宿主未设置 → 跳过（不注入空值）。
	if _, ok := merged["GLOBAL_FOLLOW_UNSET"]; ok {
		t.Error("GLOBAL_FOLLOW_UNSET should be skipped (host unset)")
	}
	// 全局级覆盖基础集。
	if merged["HOME"] != "/home/global" {
		t.Errorf("HOME = %q, want /home/global (global > base)", merged["HOME"])
	}
	// 优先级：任务级 > 项目级 > 全局级。
	if merged["SHARED"] != "task" {
		t.Errorf("SHARED = %q, want task (task > project > global)", merged["SHARED"])
	}
	// reserved key 在全局级同样被忽略（不覆盖内部变量）。
	if merged["OCDECK_TASK_ID"] != "t1" {
		t.Errorf("OCDECK_TASK_ID = %q, want t1 (reserved, internal injection)", merged["OCDECK_TASK_ID"])
	}
	if _, ok := merged["OPENCODE_SERVER_PASSWORD"]; ok {
		t.Error("OPENCODE_SERVER_PASSWORD MUST NOT be in merged env (reserved secret)")
	}

	// 快照同源 merged（含全局级解析后的最终值，含未设置的跳过项不存在）。
	row := store.tasks["t1"]
	if !row.EnvSnapshot.Valid {
		t.Fatal("env_snapshot not persisted")
	}
	var snap envSnapshot
	if err := json.Unmarshal([]byte(row.EnvSnapshot.String), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.Vars["GLOBAL_FOLLOW"] != "resolved-from-host" {
		t.Errorf("snapshot GLOBAL_FOLLOW = %q, want resolved-from-host", snap.Vars["GLOBAL_FOLLOW"])
	}
	if _, ok := snap.Vars["GLOBAL_FOLLOW_UNSET"]; ok {
		t.Error("snapshot should not contain GLOBAL_FOLLOW_UNSET (skipped)")
	}
}

// TestIsReservedEnvKey 验证保留 key 集合（design.md §2）。
func TestIsReservedEnvKey(t *testing.T) {
	reserved := []string{"OPENCODE_SERVER_PASSWORD", "OCDECK_SERVE_PORT", "OCDECK_TASK_ID"}
	for _, k := range reserved {
		if !isReservedEnvKey(k) {
			t.Errorf("isReservedEnvKey(%q) = false, want true", k)
		}
	}
	nonReserved := []string{"TERM", "HOME", "PATH", "MY_VAR", "MY_PROJ_VAR", ""}
	for _, k := range nonReserved {
		if isReservedEnvKey(k) {
			t.Errorf("isReservedEnvKey(%q) = true, want false", k)
		}
	}
}

// --- dispositionToNotice 五值映射（1.17 KillSession disposition） ---

func TestDispositionToNotice_FiveValues(t *testing.T) {
	cases := []struct {
		name        string
		disposition process.CleanupDisposition
		wantReason  string
		wantRetry   bool
		wantOk      bool
	}{
		{"snapshot_failed", process.DispositionSnapshotFailed, noticeReasonSnapshotFailed, true, true},
		{"kill_failed", process.DispositionKillFailed, noticeReasonKillFailed, true, true},
		{"reap_failed", process.DispositionReapFailed, noticeReasonReapFailed, true, true},
		{"snapshot_missing_degraded", process.DispositionSnapshotMissingDegraded, noticeReasonSnapshotMissing, false, true},
		{"clean_no_notice", process.DispositionClean, "", false, false},
		{"empty_no_notice", "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, retry, ok := dispositionToNotice(tc.disposition)
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if retry != tc.wantRetry {
				t.Errorf("retryable = %v, want %v", retry, tc.wantRetry)
			}
			if ok != tc.wantOk {
				t.Errorf("ok = %v, want %v", ok, tc.wantOk)
			}
		})
	}
}

// TestNewRuntime_InstVersionUnique 验证 instVersion 唯一性（B4 语义，P1.4.9 单字符串
// 令牌取代原 generation 单调）：连续分配（含同毫秒）与 clearRuntime 后再分配均不重复。
func TestNewRuntime_InstVersionUnique(t *testing.T) {
	m := newTestManager(t, newMockStore(), newMockProc(), newMockWorktree(), newMockOC(true))

	rt1 := m.newRuntime("t1")
	rt2 := m.newRuntime("t1")
	if rt1.instVersion == rt2.instVersion {
		t.Errorf("consecutive allocations must differ, both %q", rt1.instVersion)
	}
	// clearRuntime 后再创建：令牌不得与既有令牌重复（B4 fencing 等值判定基础）。
	m.clearRuntime("t1")
	rt3 := m.newRuntime("t1")
	if rt3.instVersion == rt2.instVersion || rt3.instVersion == rt1.instVersion {
		t.Errorf("post-clear instVersion %q must differ from prior tokens %q/%q", rt3.instVersion, rt1.instVersion, rt2.instVersion)
	}
}

// --- reconcile persist 矩阵补例（1.17） ---

// TestReconcilePersist_Creating_ToCreationFailed 验证 creating×任意→kill 会话+creation_failed。
func TestReconcilePersist_Creating_ToCreationFailed(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusCreating })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true // 异常会话
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusCreationFailed {
		t.Errorf("status = %s, want creation_failed", row.Status)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("abnormal serve session should be killed")
	}
}

// TestReconcilePersist_CreationFailed_KeptWithKill 验证 creation_failed 保持原状，kill 异常会话。
func TestReconcilePersist_CreationFailed_KeptWithKill(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusCreationFailed })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusCreationFailed {
		t.Errorf("status = %s, want creation_failed (preserved)", row.Status)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("abnormal session should be killed even when state preserved")
	}
}

// TestReconcilePersist_Suspending_ToSuspended 验证 suspending+persist→完成清理落 suspended。
func TestReconcilePersist_Suspending_ToSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusSuspending })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (suspending persisted → finish cleanup)", row.Status)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("serve should be killed on suspending reconcile")
	}
	if proc.sessions[tuiSessionName("t1")] {
		t.Error("tui should be killed on suspending reconcile")
	}
}

// TestReconcilePersist_Archived_KeptWithKill 验证 archived 保持原状，kill 残留会话。
func TestReconcilePersist_Archived_KeptWithKill(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusArchived })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusArchived {
		t.Errorf("status = %s, want archived (preserved)", row.Status)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("residual session should be killed even when archived")
	}
}

// TestReconcilePersist_DeletionFailed_KeptWithKill 验证 deletion_failed 保持原状，kill 会话。
func TestReconcilePersist_DeletionFailed_KeptWithKill(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusDeletionFailed })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Errorf("status = %s, want deletion_failed (preserved)", row.Status)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("residual session should be killed even when deletion_failed")
	}
}

// TestReconcilePersist_Deleting_KeptWithKill 验证 deleting 保持原状，kill 会话（提示用户 Retry）。
func TestReconcilePersist_Deleting_KeptWithKill(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusDeleting })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeleting {
		t.Errorf("status = %s, want deleting (preserved)", row.Status)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("residual session should be killed even when deleting")
	}
}

// TestReconcilePersist_ResumeActiveFailure_ToSuspended 验证 resumeActive 中途失败→停 runtime→suspended。
// 构造 serve 健康（env 读回 OK）但 OCDECK_TASK_ID 不匹配（resumeActive 校验失败）。
func TestReconcilePersist_ResumeActiveFailure_ToSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	snap := envSnapshot{Vars: map[string]string{"OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	// env 读回：密码端口 OK，但 OCDECK_TASK_ID 不匹配（模拟 serve 被其他任务占用或损坏）。
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "other-task",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	// Reconcile 返回聚合 error（resume 失败 MUST 传播，main fail-closed），但状态 MUST 落 suspended。
	_ = m.Reconcile(context.Background())
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (resume failure → cleanup + suspended)", row.Status)
	}
	if m.getRuntime("t1") != nil {
		t.Error("runtime MUST be cleared after resume failure (no unattended serve)")
	}
}

// TestReconcilePersist_ActiveWithDebt_ToSuspended 验证 active 有 retryable debt→kill runtime→suspended。
// pre-pass 拿不到锁或 RetryReap 残留时，debt 阻塞恢复。
// 这里构造 RetryReap 仍有残留（reapPartialProc），pre-pass 不清空 → reconcilePersist hasDebt 走 kill+suspended。
func TestReconcilePersist_ActiveWithDebt_ToSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	snap := envSnapshot{Vars: map[string]string{"OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
	// 注入 retryable debt，sessionName 指向已消失的 shell（pre-pass 跳过 kill，RetryReap 仍有残留）。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": shellSessionName("t1", 1), "reason": "kill_failed", "retryable": true,
		"cleanupTickets": []interface{}{"tk1"},
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })

	proc := &reapPartialProc{mockProc: newMockProc(), remaining: []string{"tk1"}}
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (debt blocks resume)", row.Status)
	}
	if m.getRuntime("t1") != nil {
		t.Error("runtime MUST be cleared when debt blocks resume")
	}
}

// --- snapshot_failed 无 tickets + 会话消失 + 立即 Activate 被拒（1.17 notice 存在性门禁） ---

// TestSnapshotFailed_NoTickets_SessionVanished_ActivateRejected 验证：
// snapshot_failed notice（无 tickets）+ 会话已消失 + 立即 Activate 必须被拒（hasRetryableNotice 门禁）。
// retryable=true 的 snapshot_failed 即便无会话也不得放行 Activate（§8/§19）。
func TestSnapshotFailed_NoTickets_SessionVanished_ActivateRejected(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// snapshot_failed，无 tickets（snapshot 失败但会话已消失），retryable=true。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": serveSessionName("t1"), "reason": noticeReasonSnapshotFailed, "retryable": true,
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })
	proc := newMockProc() // serve 不存在（已消失）
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("Activate MUST be rejected when retryable notice exists (debt gate)")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code = %s, want conflict (retryable notice blocks Activate)", OpErrorCode(err))
	}
	// 状态保持 suspended。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (not activated)", row.Status)
	}
}

// TestRetryTaskNotices_SnapshotFailedSessionVanished_ToDegraded 验证 retryTaskNotices：
// snapshot_failed 历史会话在重试前自行消失 → 转 snapshot_missing_degraded（非清除，B6/§5）。
func TestRetryTaskNotices_SnapshotFailedSessionVanished_ToDegraded(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// snapshot_failed（会话已消失），无 tickets，retryable=true。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": serveSessionName("t1"), "reason": noticeReasonSnapshotFailed, "retryable": true,
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })
	proc := newMockProc() // serve 不存在（已消失）
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	if err := m.retryTaskNotices(context.Background(), row, entries); err != nil {
		t.Fatalf("retryTaskNotices: %v", err)
	}
	// 读回 notice：应存在 snapshot_missing_degraded 项（retryable=false），保留告警非清除。
	finalRow, _ := store.GetTask(context.Background(), "t1")
	finalEntries, _ := parseNotices(finalRow.Notice)
	if len(finalEntries) == 0 {
		t.Fatal("notice cleared, want snapshot_missing_degraded retained (not cleared)")
	}
	foundDegraded := false
	for _, e := range finalEntries {
		if e.Code == noticeCodeResidual && e.Data["reason"] == noticeReasonSnapshotMissing {
			if retry, _ := e.Data["retryable"].(bool); !retry {
				foundDegraded = true
			}
		}
	}
	if !foundDegraded {
		t.Errorf("no snapshot_missing_degraded (retryable=false) entry in notice; got %+v", finalEntries)
	}
}

// --- absent-at-entry 幂等短路（1.17） ---

// TestKillTaskSessions_AbsentAtEntry_IdempotentShortCircuit 验证 killTaskSessions 对已不存在会话
// 视为幂等成功（不调用 KillSession，不产生结果项，design.md §18 absent-at-entry）。
func TestKillTaskSessions_AbsentAtEntry_IdempotentShortCircuit(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	// serve/tui 均不存在（已消失）。
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	results := m.killTaskSessions(context.Background(), "t1", []string{
		serveSessionName("t1"), tuiSessionName("t1"), shellSessionName("t1", 1),
	})
	if len(results) != 0 {
		t.Errorf("results = %d items, want 0 (absent-at-entry idempotent short-circuit)", len(results))
	}
	if len(proc.killOrderSnapshot()) != 0 {
		t.Errorf("KillSession called %d times, want 0 (absent sessions not killed)", len(proc.killOrderSnapshot()))
	}
}

// TestSuspend_AbsentAtEntry_AllDeadBranchA 验证 Suspend 分支 A：
// serve/tui/shell 全部已不存在（absent-at-entry）→ killTaskSessions 全短路 → finishSuspend 落 suspended。
func TestSuspend_AbsentAtEntry_AllDeadBranchA(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc() // 无任何会话
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (all absent-at-entry → idempotent success)", row.Status)
	}
	if len(proc.killOrderSnapshot()) != 0 {
		t.Errorf("KillSession calls = %d, want 0 (no live sessions)", len(proc.killOrderSnapshot()))
	}
}

// --- 并发 ReopenAttach 幂等复用（1.19） ---

// TestReopenAttach_ConcurrentIdempotentReuse 验证：并发 ReopenAttach 幂等复用同一 TUI 会话。
// 已有 tui 时两个并发调用都应返回同一 terminalID（不产生 409，不重复建 TUI）。
func TestReopenAttach_ConcurrentIdempotentReuse(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	const n = 4
	results := make([]TerminalID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			tid, err := m.ReopenAttach(context.Background(), "t1")
			results[i] = tid
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	want := runtimeSessionName("t1")
	ok := 0
	for i, err := range errs {
		if err == nil {
			if string(results[i]) != want {
				t.Errorf("ReopenAttach[%d] tid = %s, want %s", i, results[i], want)
			}
			ok++
			continue
		}
		if OpErrorCode(err) != codeConflict {
			t.Errorf("ReopenAttach[%d]: %v (want success or conflict from tryLock)", i, err)
		}
	}
	if ok == 0 {
		t.Fatal("at least one concurrent ReopenAttach must reuse existing runtime")
	}
	// 已存在 tui 时不得重复创建（NewSession 不应被调用）。
	if newNames := proc.newSessionNamesSnapshot(); len(newNames) != 0 {
		t.Errorf("NewSession calls = %v, want none (existing tui reused idempotently)", newNames)
	}
}

// TestReopenAttach_RecreatesMissingTUI 验证 runtime 缺失时 ReopenAttach 的 D8 新语义：
// active + 会话缺失 → typed recovering + 异步触发恢复；恢复收敛后 runtime 在位 →
// 返回 -runtime terminal id，再调用幂等复用。
// 旧语义（重建独立 TUI 会话）已随单进程化移除；本测试同步等待后台恢复 goroutine
// （Phase 4 起 ReopenAttach 对缺失 runtime 异步 ensureRecoveryFromAttach）收敛后再
// 断言——不得在恢复未结束时直接改 mockProc 字段（-race 下与后台 goroutine 竞争）。
func TestReopenAttach_RecreatesMissingTUI(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	tid, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil {
		t.Fatal("ReopenAttach without runtime must return recovering")
	}
	if OpErrorCode(err) != codeRecovering {
		t.Fatalf("code=%s want recovering, err=%v", OpErrorCode(err), err)
	}
	_ = tid
	// 等待后台恢复收敛。单一三条件谓词（status==active && 无进行中 incident &&
	// runtime 会话存活）同时成立才判定完成——分段等待（先 status 再 incidents）在
	// 「恢复尚未启动」窗口会被初始 active 状态误判为已收敛。收敛前不得直接写
	// proc 字段（-race 下与后台 goroutine 竞争）。
	waitFor(t, 10*time.Second, func() bool {
		row, _ := store.GetTask(context.Background(), "t1")
		m.recoveryIncidentsMu.Lock()
		busy := len(m.recoveryIncidents) > 0
		m.recoveryIncidentsMu.Unlock()
		alive, _ := proc.HasSession(runtimeSessionName("t1"))
		return row.Status == StatusActive && !busy && alive
	})
	tid, err = m.ReopenAttach(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReopenAttach (runtime present): %v", err)
	}
	if string(tid) != runtimeSessionName("t1") {
		t.Errorf("tid = %s, want %s", tid, runtimeSessionName("t1"))
	}
	// 再调用一次：复用已恢复的 runtime（幂等）。
	namesBefore := proc.newSessionNamesSnapshot()
	tid2, err := m.ReopenAttach(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReopenAttach (reuse): %v", err)
	}
	if string(tid2) != runtimeSessionName("t1") {
		t.Errorf("reuse tid = %s, want %s", string(tid2), runtimeSessionName("t1"))
	}
	if got := proc.newSessionNamesSnapshot(); len(got) != len(namesBefore) {
		t.Errorf("NewSession after reuse: %d calls, want %d (idempotent)", len(got), len(namesBefore))
	}
}

// _ 保持导入引用（部分 case 未直接用到但保留以备扩展）。
var _ = fmt.Sprintf
var _ = strings.Contains
var _ = os.Setenv
var _ = errors.New
var _ = time.Second
var _ = opencode.HealthResponse{}
