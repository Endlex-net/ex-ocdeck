package task

// charx_baseline_activate_test.go 落地 opencode-v1-spi-provider 阶段 0 基线组
// fixture（tasks.md 1.2）：激活/端口重试、anchor bootstrap 三支、非法组合零副作用、
// 重启恢复。全部经 charx harness（真实 OCClient → fake opencode server）驱动
// Manager 公共入口，断言六观察面。
//
// 有效性证据思路：fixture 走生产 Activate→waitServeReadyOrDead→bootstrapRuntime
// →startSSE 全链路；端口序列来自 mockProc 记录的真实 NewSession argv；fake server
// 的 health 503 / 相邻死端口脚本决定 waitServeReadyOrDead 的分派（健康超时→kill+换端口；
// 进程死亡→无 kill 直接换端口），断言以 kill/new 计数差异区分两条分派。

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"strings"
	"testing"
)

// --- 1.2 激活/端口重试 ---

// TestCharx_PortRetry_OccupiedPortsSkipped 端口占用脚本：区间 [M-2, M]（M 为映射
// 端口）中占用 M-2 → 分配跳过被占端口落在 M-1；锚定任务单次启动。
func TestCharx_PortRetry_OccupiedPortsSkipped(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{retryRange: true})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	e.store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "ses-anchor", Valid: true}
	})
	e.srv.seedSession("/data/worktrees/p1/t1", "ses-anchor", 10, 20)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(e.fakeMapped-2))
	if err != nil {
		t.Fatalf("occupy port %d: %v", e.fakeMapped-2, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	argv := strings.Join(runtimeCmdArgvOf(e.proc.mockProc, "t1"), " ")
	// 扫描顺序从区间最小值起：M-2（占用）→ M-1（空闲）——被占端口跳过后顺延。
	wantArgv := "opencode --port " + strconv.Itoa(e.fakeMapped-1) + " --hostname 127.0.0.1 --session ses-anchor"
	if argv != wantArgv {
		t.Errorf("argv = %q, want %q", argv, wantArgv)
	}
	if n := countNewServe(e.proc.mockProc, "t1"); n != 1 {
		t.Errorf("NewSession(runtime) = %d, want 1 (anchored, no rotation)", n)
	}
	assertStatus(t, e.store, "t1", StatusActive)
}

// TestCharx_PortRetry_HealthTimeoutRotates 健康超时脚本：deadPorts 使 P-2、P-1
// 连接拒绝 → 健康超时分派（KillSession clean + 换端口）→ 在 fake 端口 P 成功。
// 无锚定双启动：P 上 bootstrap（new→claim→kill 确认→formal new）。
func TestCharx_PortRetry_HealthTimeoutRotates(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{retryRange: true, deadPorts: true})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	assertStatus(t, e.store, "t1", StatusActive)
	// 死端口×2（new+kill 各 2）+ fake 端口双启动（new + kill 确认 + new）。
	if n := countNewServe(e.proc.mockProc, "t1"); n != 4 {
		t.Errorf("NewSession(runtime) = %d, want 4", n)
	}
	if n := countKillServe(e.proc.mockProc, "t1"); n != 3 {
		t.Errorf("KillSession(runtime) = %d, want 3 (2 rotation + 1 bootstrap-confirm)", n)
	}
	row := e.charxTaskRow("t1")
	if row.LastError.Valid && row.LastError.String != "" {
		t.Errorf("last_error = %q, want empty on success", row.LastError.String)
	}
	if entries := e.charxNoticesOf("t1"); len(entries) != 0 {
		t.Errorf("notices = %+v, want none (clean kill rotation)", entries)
	}
	if !row.AnchorSessionID.Valid || !strings.HasPrefix(row.AnchorSessionID.String, "ses_fake_") {
		t.Errorf("anchor = %+v, want fake-server created session", row.AnchorSessionID)
	}
	// 最终 formal 进程 argv 落在映射端口。
	argv := strings.Join(runtimeCmdArgvOf(e.proc.mockProc, "t1"), " ")
	if !strings.Contains(argv, "--port "+strconv.Itoa(e.fakeMapped)+" ") {
		t.Errorf("formal argv = %q, want mapped fake port %d", argv, e.fakeMapped)
	}
}

// TestCharx_PortRetry_ProcessDiedRotatesWithoutKill 进程死亡脚本：前两次 NewSession
// 后会话立即死亡 → waitServeReadyOrDead 判死分派（无 KillSession 直接换端口）。
// 与健康超时的差异面：kill 仅剩双启动 bootstrap 确认一次。
func TestCharx_PortRetry_ProcessDiedRotatesWithoutKill(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{retryRange: true, deadPorts: true, killAfterNew: 2})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	assertStatus(t, e.store, "t1", StatusActive)
	if n := countKillServe(e.proc.mockProc, "t1"); n != 1 {
		t.Errorf("KillSession(runtime) = %d, want 1 (死亡轮换不 kill；仅双启动 bootstrap 确认)", n)
	}
	if n := countNewServe(e.proc.mockProc, "t1"); n != 4 {
		t.Errorf("NewSession(runtime) = %d, want 4", n)
	}
}

// TestCharx_PortRetry_AllUnhealthyExhausted 健康始终失败脚本：fake 端口也持续 503 →
// 3 次尝试全部健康超时 → Activate 失败 → 统一补偿落 suspended + last_error、
// 零归属、零锚定、零残留会话。
func TestCharx_PortRetry_AllUnhealthyExhausted(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{retryRange: true, deadPorts: true})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	e.srv.setHealthFailFirst(1 << 30)

	err := e.charxActivate("t1")
	if err == nil {
		t.Fatal("Activate must fail when health never ready")
	}
	assertStatus(t, e.store, "t1", StatusSuspended)
	row := e.charxTaskRow("t1")
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "health check timeout") {
		t.Errorf("last_error = %+v, want contains health check timeout", row.LastError)
	}
	if owned := e.charxOwnedSessions("t1"); len(owned) != 0 {
		t.Errorf("ownership = %v, want empty", owned)
	}
	if row.AnchorSessionID.Valid {
		t.Errorf("anchor = %+v, want unset", row.AnchorSessionID)
	}
	if n := countKillServe(e.proc.mockProc, "t1"); n != 3 {
		t.Errorf("KillSession(runtime) = %d, want 3", n)
	}
	if e.proc.mockProc.sessions[runtimeSessionName("t1")] {
		t.Error("runtime session must be killed after failed activation")
	}
}

// --- 1.2 anchor bootstrap 三支 ---

// TestCharx_AnchorBootstrap_ValidAnchorDirectStart 有效锚定：带 --session 直接启动，
// 列表校验命中即返回；无 POST /session、无双启动。
func TestCharx_AnchorBootstrap_ValidAnchorDirectStart(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	e.store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "ses-existing", Valid: true}
	})
	e.srv.seedSession("/data/worktrees/p1/t1", "ses-existing", 1, 1)

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if n := countNewServe(e.proc.mockProc, "t1"); n != 1 {
		t.Errorf("NewSession = %d, want 1 (direct start)", n)
	}
	argv := strings.Join(runtimeCmdArgvOf(e.proc.mockProc, "t1"), " ")
	if !strings.Contains(argv, "--session ses-existing") {
		t.Errorf("argv = %q, want --session ses-existing", argv)
	}
	for _, req := range e.srv.requests() {
		if strings.HasPrefix(req, "POST /session") {
			t.Errorf("unexpected POST /session: %v", e.srv.requests())
		}
	}
	// 六观察面完整快照（A2：默认全面比较——六面 golden 全部字面量）。
	// 有锚定直启：单次 NewSession（--session），无 POST、无双启动、无 kill；
	// agentStatus 对账后为 idle（owned 集合在 /session/status 无记录 → idle 缺省）。
	got := e.captureCharx("anchored", "t1")
	assertCharxMatches(t, "anchored", charxSnapshot{
		tasks: []string{
			"t1|name=task t1|active|worktree|anchor=ses-existing|last_port=PT1|delete_mode=-|last_error=|notices=[]|" +
				"env={OCDECK_PROJECT_PATH=/repo-p1,OCDECK_SERVE_PORT=<PORT>,OCDECK_TASK_BASE_BRANCH=main," +
				"OCDECK_TASK_HEAD_BRANCH=ocdeck/t1,OCDECK_TASK_ID=t1,OCDECK_TASK_NAME=task t1,OCDECK_TASK_PATH=/data/worktrees/p1/t1}",
		},
		sessions: []string{"t1|ses-existing|created=1|first_seen=<TS>|last_seen=1|parent=-"},
		owner:    []string{"t1 -> [ses-existing]"},
		proc: []string{
			"new ocdeck-t1-runtime cwd=/data/worktrees/p1/t1 auth=pw(64) argv=opencode --port PT1 --hostname 127.0.0.1 --session ses-existing",
		},
		notices: nil,
		dto:     []string{`t1|attention={"Permissions":[],"Questions":[]}|agentStatus=idle`},
	}, got)
}

// TestCharx_AnchorBootstrap_StaleAnchorCASClears 失效锚定：列表校验缺席 → CAS 清空
// 旧锚定 → 转无锚定流程（POST /session 创建 + claim + 双启动）。
func TestCharx_AnchorBootstrap_StaleAnchorCASClears(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	e.store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "ses-gone", Valid: true}
	})
	e.srv.seedSession("/data/worktrees/p1/t1", "unrelated", 1, 1)

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	row := e.charxTaskRow("t1")
	if !row.AnchorSessionID.Valid || !strings.HasPrefix(row.AnchorSessionID.String, "ses_fake_") {
		t.Errorf("anchor = %+v, want fake-server created", row.AnchorSessionID)
	}
	// 锚定启动（new #1）→ 列表缺席 → CAS 清空 → kill 确认（kill #1）→ 转无锚定：
	// 双启动（new #2 bootstrap → kill 确认（kill #2）→ new #3 formal）。
	if n := countNewServe(e.proc.mockProc, "t1"); n != 3 {
		t.Errorf("NewSession = %d, want 3 (anchored start + dual start)", n)
	}
	if n := countKillServe(e.proc.mockProc, "t1"); n != 2 {
		t.Errorf("KillSession = %d, want 2 (stale-anchor confirm + bootstrap confirm)", n)
	}
	argv := strings.Join(runtimeCmdArgvOf(e.proc.mockProc, "t1"), " ")
	if !strings.Contains(argv, "--session "+row.AnchorSessionID.String) {
		t.Errorf("formal argv = %q, want --session %s", argv, row.AnchorSessionID.String)
	}
	// 全量对齐 claim 锚定 + 同目录无关 session（worktree 目录私有 → claim）。
	owned := e.charxOwnedSessions("t1")
	if !owned[row.AnchorSessionID.String] || !owned["unrelated"] {
		t.Errorf("ownership = %v, want anchor+unrelated claimed", owned)
	}
}

// TestCharx_AnchorBootstrap_NoAnchorDualStarts 无锚定：直接双启动子协议。
// proc 序列（观察面 3）：bootstrap（无 --session）→ kill 确认 → formal（--session）。
func TestCharx_AnchorBootstrap_NoAnchorDualStarts(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")

	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	row := e.charxTaskRow("t1")
	if !row.AnchorSessionID.Valid {
		t.Fatal("anchor must be created and persisted")
	}
	if n := countNewServe(e.proc.mockProc, "t1"); n != 2 {
		t.Errorf("NewSession = %d, want 2 (dual start)", n)
	}
	if n := countKillServe(e.proc.mockProc, "t1"); n != 1 {
		t.Errorf("KillSession = %d, want 1", n)
	}
	proc := e.proc.calls()
	if len(proc) != 3 {
		t.Fatalf("proc = %q, want 3 calls", proc)
	}
	if strings.Contains(proc[0], "--session") {
		t.Errorf("bootstrap argv MUST NOT carry --session: %q", proc[0])
	}
	if proc[1] != "kill ocdeck-t1-runtime" {
		t.Errorf("proc[1] = %q, want bootstrap termination kill", proc[1])
	}
	if !strings.Contains(proc[2], "--session "+row.AnchorSessionID.String) {
		t.Errorf("formal argv = %q, want --session %s", proc[2], row.AnchorSessionID.String)
	}
}

// --- 1.2 非法组合零副作用 ---

// TestCharx_IllegalCombo_ZeroSideEffect dir+worktree / 未知 kind / 未知 mode：
// Activate 与 Reconcile（persist 重启恢复入口）在任何状态修改、SSE、对齐、锚定或
// 进程副作用之前报错且零副作用（不写 last_error）。
func TestCharx_IllegalCombo_ZeroSideEffect(t *testing.T) {
	cases := []struct {
		name string
		seed func(s *mockStore, taskID string)
	}{
		{"dir+worktree", func(s *mockStore, taskID string) {
			s.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: "/dir", Kind: ProjectKindDir})
			s.tasks[taskID] = TaskRow{ID: taskID, ProjectID: "pdir", Name: "t", Status: StatusSuspended,
				WorktreePath: "/dir", Mode: TaskModeWorktree}
		}},
		{"unknown kind", func(s *mockStore, taskID string) {
			s.seedProject(ProjectRow{ID: "px", Name: "x", Path: "/x", Kind: "weird"})
			s.tasks[taskID] = TaskRow{ID: taskID, ProjectID: "px", Name: "t", Status: StatusSuspended,
				WorktreePath: "/x", Mode: TaskModeWorktree}
		}},
		{"unknown mode", func(s *mockStore, taskID string) {
			s.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/r"})
			s.tasks[taskID] = TaskRow{ID: taskID, ProjectID: "prep", Name: "t", Status: StatusSuspended,
				WorktreePath: "/r/w", Mode: "strange"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCharxEnv(t, charxEnvOpts{})
			tc.seed(e.store, "t1")
			rowBefore, _ := e.store.GetTask(context.Background(), "t1")

			err := e.charxActivate("t1")
			if err == nil {
				t.Fatal("Activate must fail on illegal kind/mode combination")
			}
			if OpErrorCode(err) != codeInternal {
				t.Errorf("code = %s, want internal (persisted corruption)", OpErrorCode(err))
			}

			// 零副作用：状态/last_error/notice/锚定不变；零归属；零进程；零 SSE；零上游请求。
			rowAfter, _ := e.store.GetTask(context.Background(), "t1")
			if rowAfter.Status != StatusSuspended {
				t.Errorf("status = %s, want suspended", rowAfter.Status)
			}
			if rowAfter.LastError.Valid && rowAfter.LastError.String != "" {
				t.Errorf("last_error = %q, MUST NOT be written (零副作用)", rowAfter.LastError.String)
			}
			if rowAfter.Notice != rowBefore.Notice || rowAfter.AnchorSessionID != rowBefore.AnchorSessionID {
				t.Errorf("notice/anchor drifted: %+v → %+v", rowBefore, rowAfter)
			}
			if owned := e.charxOwnedSessions("t1"); len(owned) != 0 {
				t.Errorf("ownership = %v, want empty", owned)
			}
			if calls := e.proc.calls(); len(calls) != 0 {
				t.Errorf("proc calls = %q, want none", calls)
			}
			if n := e.srv.subscriberCount(); n != 0 {
				t.Errorf("sse subscribers = %d, want 0", n)
			}
			if reqs := e.srv.requests(); len(reqs) != 0 {
				t.Errorf("upstream requests = %q, want none", reqs)
			}

			// Reconcile（persist 重启恢复入口）同样在任何副作用前 fail-closed；
			// 重查全部零副作用面：任务行/归属/SSE/进程/上游请求（A5④）。
			if rerr := e.m.Reconcile(context.Background()); rerr == nil {
				t.Error("Reconcile must fail on illegal kind/mode combination")
			}
			rowAfterReconcile, _ := e.store.GetTask(context.Background(), "t1")
			if rowAfterReconcile.Status != StatusSuspended || rowAfterReconcile.LastError.Valid && rowAfterReconcile.LastError.String != "" {
				t.Errorf("task row drifted after Reconcile: %+v", rowAfterReconcile)
			}
			if rowAfterReconcile.Notice != rowBefore.Notice || rowAfterReconcile.AnchorSessionID != rowBefore.AnchorSessionID {
				t.Errorf("notice/anchor drifted after Reconcile: %+v", rowAfterReconcile)
			}
			if owned := e.charxOwnedSessions("t1"); len(owned) != 0 {
				t.Errorf("ownership after Reconcile = %v, want empty", owned)
			}
			if calls := e.proc.calls(); len(calls) != 0 {
				t.Errorf("proc calls after Reconcile = %q, want none", calls)
			}
			if n := e.srv.subscriberCount(); n != 0 {
				t.Errorf("sse subscribers after Reconcile = %d, want 0", n)
			}
			if reqs := e.srv.requests(); len(reqs) != 0 {
				t.Errorf("upstream requests after Reconcile = %q, want none", reqs)
			}
		})
	}
}

// --- 1.2 重启恢复 ---

// TestCharx_RestartRecovery_ProcessAlive 进程存活：同 store+proc 重建 Manager →
// Reconcile resumeActive：env 反查凭据/端口 → 健康检查 → SSE+对齐 → 保持 active。
func TestCharx_RestartRecovery_ProcessAlive(t *testing.T) {
	srv := newFakeOCServer(t)
	store := newMockStore()
	proc := newCharxProc()
	e1 := newCharxEnvOnServer(t, srv, store, proc, charxEnvOpts{})
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	if err := e1.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	anchor := e1.charxTaskRow("t1").AnchorSessionID.String
	ownedBefore := e1.charxOwnedSessions("t1")

	// server 重启：旧 Manager 生命周期终止，新 Manager 复用同一 store/proc/server。
	e1.lifeCancel()
	e2 := newCharxEnvOnServer(t, srv, store, proc, charxEnvOpts{})

	if err := e2.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusActive)
	// resumeActive 不重建进程（runtime 会话沿用，激活期双启动共 2 次）。
	if n := countNewServe(proc.mockProc, "t1"); n != 2 {
		t.Errorf("NewSession total = %d, want 2（激活期双启动，重启不重建）", n)
	}
	// SSE 重新订阅 + 全量对齐幂等：归属集合不变。
	srv.waitSubscribers(t, 1)
	ownedAfter := e2.charxOwnedSessions("t1")
	if len(ownedAfter) != len(ownedBefore) || !ownedAfter[anchor] {
		t.Errorf("ownership after restart = %v, before = %v (anchor %s)", ownedAfter, ownedBefore, anchor)
	}
	row := e2.charxTaskRow("t1")
	if row.LastError.Valid && row.LastError.String != "" {
		t.Errorf("last_error = %q, want empty", row.LastError.String)
	}
}

// TestCharx_RestartRecovery_ProcessGone 进程消失：Reconcile 恢复失败分类 →
// 完整运行时清理 + suspended + last_error="runtime gone during reconcile"。
func TestCharx_RestartRecovery_ProcessGone(t *testing.T) {
	srv := newFakeOCServer(t)
	store := newMockStore()
	proc := newCharxProc()
	e1 := newCharxEnvOnServer(t, srv, store, proc, charxEnvOpts{})
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	if err := e1.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	e1.lifeCancel()

	// server 重启前进程消失（tmux 会话不在）。
	proc.mockProc.mu.Lock()
	delete(proc.mockProc.sessions, runtimeSessionName("t1"))
	proc.mockProc.mu.Unlock()

	e2 := newCharxEnvOnServer(t, srv, store, proc, charxEnvOpts{})
	if err := e2.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	row := e2.charxTaskRow("t1")
	if !row.LastError.Valid || row.LastError.String != "runtime gone during reconcile" {
		t.Errorf("last_error = %+v, want runtime gone during reconcile", row.LastError)
	}
	// 预算计数：Reconcile 恢复失败路径不消耗恢复 permit（重拉属 watcher 路径）。
	if n := store.recoveryPermitCount("t1"); n != 0 {
		t.Errorf("recovery permits = %d, want 0", n)
	}
}
