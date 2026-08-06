package task

// add-plain-dir-project Lane 5/6 测试（dir 删除分叉 + reconcile 确认 + gitops 降级）。
// 独立测试文件，不修改 mock_test.go 共享 helper（并行 lane 在动同包测试）。
//
// 覆盖 tasks.md 5.4 / 6.3：
//   - dir normal/force 删除后目录逐字节比对不变（无 pre-delete 配置时）
//   - panic mock 证明 dir 路径不调用 git/WorktreeBackend（PreflightDelete/DirtyFiles/Remove 零调用）
//   - 一次性 serve/进程创建参数不向用户目录写文件（temp serve cwd 仅作工作目录）
//   - pre-delete 配置时脚本 cwd=项目目录执行且失败落 `pre-delete:` 前缀 deletion_failed
//   - oc session 仅删本任务拥有的 session
//   - reconcile dir 收敛语义（creating→creation_failed、creation_failed 保持原状）
//   - gitops 四入口对 dir 任务报错且零 git 调用、未知 kind fail-closed

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ocdeck/internal/config"
)

// panicWorktree 包装 WorktreeBackend，dir 删除 MUST NOT 调用的方法被调用时 panic，
// 证明 dir 路径零调用 git/WorktreeBackend（PreflightDelete/DirtyFiles/Remove 等）。
type panicWorktree struct {
	WorktreeBackend
	inner *mockWorktree
}

func wrapPanicWorktree(inner *mockWorktree) *panicWorktree {
	return &panicWorktree{WorktreeBackend: inner, inner: inner}
}

func (w *panicWorktree) PreflightDelete(ctx context.Context, wtPath string, opts PreflightDeleteOpts) error {
	panic("dir delete MUST NOT call PreflightDelete")
}

func (w *panicWorktree) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	panic("dir delete MUST NOT call DirtyFiles")
}

func (w *panicWorktree) Remove(ctx context.Context, wtPath string, opts worktreeRemoveOpts) error {
	panic("dir delete MUST NOT call Remove")
}

// seedDirTaskWithDir 在 mockStore 中创建 dir 项目与挂起任务，WorktreePath 指向真实项目目录。
// 与已有 seedDirProjectTask（session_isolation_test.go，WorktreePath=/dir 固定）不同，
// 本 helper 用真实临时目录供逐字节比对与 pre-delete Stat。
func seedDirTaskWithDir(s *mockStore, taskID, projectID, projDir string) TaskRow {
	s.seedProject(ProjectRow{ID: projectID, Name: "p", Path: projDir, DefaultBranch: "", Kind: ProjectKindDir})
	t := TaskRow{ID: taskID, ProjectID: projectID, Name: "my task",
		Status: StatusSuspended, WorktreePath: projDir, InitStatus: InitStatusNone}
	s.tasks[taskID] = t
	return t
}

// snapshotDir 递归读取目录内容生成可比较的字节快照（相对路径+内容）。
func snapshotDir(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if info.IsDir() {
			out[rel+"/"] = nil
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot dir %s: %v", root, err)
	}
	return out
}

// assertDirUnchanged 逐字节比对目录快照不变（零写删硬不变量）。
func assertDirUnchanged(t *testing.T, root string, before map[string][]byte) {
	t.Helper()
	after := snapshotDir(t, root)
	if len(before) != len(after) {
		t.Fatalf("dir %s file count changed: before=%d after=%d", root, len(before), len(after))
	}
	for k, vb := range before {
		va, ok := after[k]
		if !ok {
			t.Fatalf("dir %s: file %q removed (zero-write/delete invariant violated)", root, k)
		}
		if (vb == nil) != (va == nil) {
			t.Fatalf("dir %s: entry %q type changed (dir<->file)", root, k)
		}
		if vb != nil && !bytes.Equal(vb, va) {
			t.Fatalf("dir %s: file %q content changed (zero-write invariant violated)", root, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Fatalf("dir %s: file %q added (zero-write invariant violated)", root, k)
		}
	}
}

// writeFileTree 在 root 下预埋固定文件树供逐字节比对。
func writeFileTree(t *testing.T, root string) {
	t.Helper()
	mustWrite := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	mustWrite("README.md", "# dir project\n")
	mustWrite("src/main.go", "package main\nfunc main() {}\n")
	mustWrite("docs/note.txt", "keep me\n")
}

// TestDelete_DirNormal_ByteIdenticalTree 验证 dir normal 删除后项目目录逐字节不变，
// 且 panicWorktree 证明 dir 路径不调用 git/WorktreeBackend（PreflightDelete/DirtyFiles/Remove）。
func TestDelete_DirNormal_ByteIdenticalTree(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	before := snapshotDir(t, projDir)
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	proc := newMockProc()
	wt := wrapPanicWorktree(newMockWorktree())
	oc := newMockOC(true)
	m := newTestManager(t, store, proc, wt, oc)

	if err := m.Delete(context.Background(), "t1", DeleteNormal, false); err != nil {
		t.Fatalf("Delete dir normal: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatalf("task row must be deleted")
	}
	// 目录逐字节不变（内建逻辑对用户目录零写删）。
	assertDirUnchanged(t, projDir, before)
	// panicWorktree 未 panic 即证明 PreflightDelete/DirtyFiles/Remove 零调用。
	if wt.inner.removeCalls() != 0 {
		t.Fatalf("dir delete MUST NOT call wt.Remove; got %d", wt.inner.removeCalls())
	}
}

// TestDelete_DirForce_ByteIdenticalTree 验证 dir force 删除后项目目录逐字节不变，零 git/WorktreeBackend 调用。
func TestDelete_DirForce_ByteIdenticalTree(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	before := snapshotDir(t, projDir)
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	proc := newMockProc()
	wt := wrapPanicWorktree(newMockWorktree())
	oc := newMockOC(true)
	m := newTestManager(t, store, proc, wt, oc)

	if err := m.Delete(context.Background(), "t1", DeleteForce, false); err != nil {
		t.Fatalf("Delete dir force: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatalf("task row must be deleted")
	}
	assertDirUnchanged(t, projDir, before)
	if wt.inner.removeCalls() != 0 {
		t.Fatalf("dir force delete MUST NOT call wt.Remove; got %d", wt.inner.removeCalls())
	}
}

// TestDelete_DirDeletionFailed_ForceReentry 验证 dir deletion_failed 任务经 Force 重入 dir 序列
// （deleteResume 按持久化 delete_mode 重入同一 dir 序列，忽略 preflight dirty 快照），
// 删除后目录逐字节不变，零 git/WorktreeBackend 调用。
func TestDelete_DirDeletionFailed_ForceReentry(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	before := snapshotDir(t, projDir)
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	// 模拟前次删除失败落账：状态 deletion_failed + 持久化 delete_mode=force。
	_ = store.SetTaskDeleteMode(context.Background(), "t1", string(DeleteForce))
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusDeletionFailed })
	proc := newMockProc()
	wt := wrapPanicWorktree(newMockWorktree())
	oc := newMockOC(true)
	m := newTestManager(t, store, proc, wt, oc)

	if err := m.Delete(context.Background(), "t1", DeleteForce, false); err != nil {
		t.Fatalf("Delete dir force reentry: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatalf("task row must be deleted")
	}
	assertDirUnchanged(t, projDir, before)
}

// TestDelete_Dir_TempServeCwdIsProjectDir 验证 dir 删除起一次性 serve 时，
// 进程创建参数的 cwd（SessionSpec.Dir）= 项目目录（row.WorktreePath），且 CmdArgv 为 opencode serve。
// mockProc 不写文件，SessionSpec.Dir 仅作工作目录——断言不向用户目录写文件由 mock 语义保证
// （真实进程行为由 process 层保证；本测试钉死传入参数正确，不触达用户目录写 syscall）。
func TestDelete_Dir_TempServeCwdIsProjectDir(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	// 给本任务预埋一个 oc session 行 → 触发 deleteOCSessions 起一次性 serve。
	_ = store.UpsertTaskSession(context.Background(), SessionRow{TaskID: "t1", SessionID: "s1"})
	proc := newMockProc()
	// newSessionCapture 捕获 SessionSpec 供断言 Dir/CmdArgv。
	capture := wrapNewSessionCapture(proc)
	// mockOC 健康 OK 使 startTempServe 成功起 serve。
	oc := newMockOC(true)
	// oc.DeleteSession 默认 nil → 成功删除 session。
	// oc.Health OK → waitServeReady 通过。
	// 但 mockProc 无真实进程，serve 不会真正 ready——需 oc.Health 返回 healthy。
	// newMockOC(true) 已设 healthOK=true。
	m := newTestManager(t, store, capture, wrapPanicWorktree(newMockWorktree()), oc)

	if err := m.Delete(context.Background(), "t1", DeleteNormal, false); err != nil {
		t.Fatalf("Delete dir normal with oc sessions: %v", err)
	}
	// 断言 temp serve 的 SessionSpec.Dir = 项目目录，CmdArgv 含 opencode serve。
	specs := capture.specsFor(serveSessionName("t1"))
	if len(specs) == 0 {
		t.Fatalf("temp serve session not created; expected NewSession for %s", serveSessionName("t1"))
	}
	spec := specs[0]
	if spec.Dir != projDir {
		t.Fatalf("temp serve cwd = %q, want project dir %q (row.WorktreePath)", spec.Dir, projDir)
	}
	if len(spec.CmdArgv) == 0 || spec.CmdArgv[0] != "opencode" {
		t.Fatalf("temp serve CmdArgv must start with opencode; got %v", spec.CmdArgv)
	}
	// 项目目录逐字节不变（temp serve 仅以 cwd 作工作目录，mockProc 不写文件）。
	// 注：oc session 行已被 DeleteTaskSession 删除（store.DeleteTask 也会清理 sessions）。
}

// TestDelete_Dir_PreDeleteScript_CwdAndPrefix 验证 dir pre-delete 脚本 cwd=项目目录执行，
// 失败落 `pre-delete:` 前缀 deletion_failed，任务行保留。
func TestDelete_Dir_PreDeleteScript_CwdAndPrefix(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	proc := newMockProc()
	wt := wrapPanicWorktree(newMockWorktree())
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{runScriptErr: fmt.Errorf("predelete boom")}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil {
		t.Fatalf("Delete must fail when pre-delete script fails")
	}
	assertStatus(t, store, "t1", StatusDeletionFailed)
	lastErrorContains(t, store, "t1", "pre-delete:")
	assertTaskExists(t, store, "t1")
	// 脚本 cwd = 项目目录（row.WorktreePath）。
	if runner.runScriptCallCount() != 1 {
		t.Fatalf("RunScript call count = %d, want 1", runner.runScriptCallCount())
	}
	if got := runner.runScriptCalls[0].dir; got != projDir {
		t.Fatalf("pre-delete script cwd = %q, want project dir %q", got, projDir)
	}
}

// TestDelete_DirOCSessions_OnlyOwnedSessions 验证 dir 删除仅删本任务拥有的 oc session，
// 不影响其他任务的 session 行（deleteOCSessions 用 ListTaskSessions(row.ID) 仅取本任务）。
func TestDelete_DirOCSessions_OnlyOwnedSessions(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	seedDirTaskWithDir(store, "t2", "p2", t.TempDir())
	// 本任务 session + 其他任务 session。
	_ = store.UpsertTaskSession(context.Background(), SessionRow{TaskID: "t1", SessionID: "s1"})
	otherSession := SessionRow{TaskID: "t2", SessionID: "s2"}
	_ = store.UpsertTaskSession(context.Background(), otherSession)
	proc := newMockProc()
	oc := newMockOC(true)
	m := newTestManager(t, store, proc, wrapPanicWorktree(newMockWorktree()), oc)

	if err := m.Delete(context.Background(), "t1", DeleteNormal, false); err != nil {
		t.Fatalf("Delete dir normal: %v", err)
	}
	// 其他任务的 session 行 MUST 仍在。
	got, err := store.ListTaskSessions(context.Background(), "t2")
	if err != nil {
		t.Fatalf("ListTaskSessions t2: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "s2" {
		t.Fatalf("other task session must remain; got %+v", got)
	}
}

// TestReconcile_Dir_CreatingToCreationFailed 验证 dir 任务 creating → reconcile → creation_failed
// （钉死 dir 无需特殊处理，语义与 repo 一致）。
func TestReconcile_Dir_CreatingToCreationFailed(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusCreating })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusCreationFailed {
		t.Fatalf("dir creating → reconcile status = %s, want creation_failed", row.Status)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("dir abnormal serve session should be killed")
	}
}

// TestReconcile_Dir_CreationFailedKept 验证 dir 任务 creation_failed → reconcile 保持原状，kill 异常会话。
func TestReconcile_Dir_CreationFailedKept(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	seedDirTaskWithDir(store, "t1", "p1", projDir)
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
		t.Fatalf("dir creation_failed → reconcile status = %s, want creation_failed (preserved)", row.Status)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("dir abnormal session should be killed even when state preserved")
	}
}

// TestGitops_Dir_StatusDiffCommitPush_InvalidInput 验证 gitops 四入口对 dir 任务返回 codeInvalidInput
// （"project kind is dir (not a git repository)"），MUST NOT 执行任何 git 命令。
// 用真实非 git 目录作 WorktreePath：若 git 命令被执行会返回 codeGitError（非 git 仓库），
// 断言 codeInvalidInput 即证明在 git 命令前早退（零 git 调用）。
func TestGitops_Dir_StatusDiffCommitPush_InvalidInput(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir() // 真实非 git 目录
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	// GitStatus
	_, err := m.GitStatus(context.Background(), "t1")
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("GitStatus dir: err = %v, want codeInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "project kind is dir (not a git repository)") {
		t.Fatalf("GitStatus dir err must mention dir kind; got %v", err)
	}
	// GitDiff
	_, err = m.GitDiff(context.Background(), "t1", "", "", false)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("GitDiff dir: err = %v, want codeInvalidInput", err)
	}
	// GitCommit
	err = m.GitCommit(context.Background(), "t1", "msg", nil)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("GitCommit dir: err = %v, want codeInvalidInput", err)
	}
	// GitPush
	err = m.GitPush(context.Background(), "t1")
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("GitPush dir: err = %v, want codeInvalidInput", err)
	}
}

// TestGitops_UnknownKind_FailClosed 验证 gitops 四入口对未知持久化 kind fail-closed 返回 codeInternal
// （D1：未知持久化 kind 区别于 dir 项目的 invalid_input），MUST NOT 执行任何 git 命令。
func TestGitops_UnknownKind_FailClosed(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, Kind: "weird"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "my task",
		Status: StatusSuspended, WorktreePath: projDir, InitStatus: InitStatusNone}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitStatus(context.Background(), "t1")
	if !isOpErrCode(err, codeInternal) {
		t.Fatalf("GitStatus unknown kind: err = %v, want codeInternal (D1)", err)
	}
	if !strings.Contains(err.Error(), "unknown project kind") {
		t.Fatalf("GitStatus unknown kind err must mention unknown kind; got %v", err)
	}
	_, err = m.GitDiff(context.Background(), "t1", "", "", false)
	if !isOpErrCode(err, codeInternal) {
		t.Fatalf("GitDiff unknown kind: err = %v, want codeInternal (D1)", err)
	}
	err = m.GitCommit(context.Background(), "t1", "msg", nil)
	if !isOpErrCode(err, codeInternal) {
		t.Fatalf("GitCommit unknown kind: err = %v, want codeInternal (D1)", err)
	}
	err = m.GitPush(context.Background(), "t1")
	if !isOpErrCode(err, codeInternal) {
		t.Fatalf("GitPush unknown kind: err = %v, want codeInternal (D1)", err)
	}
}

// TestDelete_DirUnknownKind_FailClosed 验证 Delete 入口对未知持久化 kind fail-closed：在 BeginDeleteIntent
// 之前返回 codeInternal（D1：未知持久化 kind 区别于用户请求非法 kind 的 invalid_input），状态不变（零副作用）。
func TestDelete_DirUnknownKind_FailClosed(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, Kind: "weird"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "my task",
		Status: StatusSuspended, WorktreePath: projDir, InitStatus: InitStatusNone}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if !isOpErrCode(err, codeInternal) {
		t.Fatalf("Delete unknown kind: err = %v, want codeInternal (D1)", err)
	}
	// 状态不变（零副作用，未 BeginDeleteIntent）。
	assertStatus(t, store, "t1", StatusSuspended)
}

// isOpErrCode 判断 err 是否为指定 code 的 *OpError。
func isOpErrCode(err error, code string) bool {
	if err == nil {
		return false
	}
	if oe, ok := err.(*OpError); ok {
		return oe.Code == code
	}
	return false
}

// --- add-plain-dir-project tasks 5.2（crud.go Retry 删除重入 kind 分叉） ---

// panicDirtyWorktree 包装 WorktreeBackend，DirtyFiles 被调用即 panic——证明 dir Retry 零调用 DirtyFiles。
type panicDirtyWorktree struct {
	WorktreeBackend
}

func wrapPanicDirtyWorktree(inner WorktreeBackend) *panicDirtyWorktree {
	return &panicDirtyWorktree{WorktreeBackend: inner}
}

func (w *panicDirtyWorktree) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	panic("dir retry MUST NOT call WorktreeBackend.DirtyFiles")
}

// TestRetry_DirDeletionFailed_Succeeds_NoDirtyFilesCall 验证 dir 任务 deletion_failed → Retry 成功
// 进入 dir 删除序列，且 DirtyFiles 零调用（panic mock 否则 fail）；删除后目录逐字节不变。
func TestRetry_DirDeletionFailed_Succeeds_NoDirtyFilesCall(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	before := snapshotDir(t, projDir)
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	// 模拟前次删除失败落账：deletion_failed + delete_mode=normal。
	_ = store.SetTaskDeleteMode(context.Background(), "t1", string(DeleteNormal))
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusDeletionFailed })
	// panicDirtyWorktree 确保 DirtyFiles 不被调用；其余 WorktreeBackend 方法经 mockWorktree 默认。
	wt := wrapPanicDirtyWorktree(wrapPanicWorktree(newMockWorktree()))
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry dir deletion_failed: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatalf("task row must be deleted after successful dir delete retry")
	}
	assertDirUnchanged(t, projDir, before)
}

// TestRetry_DirDeleting_Succeeds 验证 dir 任务 deleting → Retry 成功进入 dir 删除序列。
func TestRetry_DirDeleting_Succeeds(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	before := snapshotDir(t, projDir)
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	// 模拟删除进行中卡住：deleting + delete_mode=normal。
	_ = store.SetTaskDeleteMode(context.Background(), "t1", string(DeleteNormal))
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusDeleting })
	wt := wrapPanicDirtyWorktree(wrapPanicWorktree(newMockWorktree()))
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry dir deleting: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatalf("task row must be deleted")
	}
	assertDirUnchanged(t, projDir, before)
}

// TestRetry_DirConfirmDirtyAcceptedButIgnored 验证 dir Retry 接受 confirmDirty=true 但忽略
// （不调用 DirtyFiles，仍成功删除）。
func TestRetry_DirConfirmDirtyAcceptedButIgnored(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	seedDirTaskWithDir(store, "t1", "p1", projDir)
	_ = store.SetTaskDeleteMode(context.Background(), "t1", string(DeleteNormal))
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusDeletionFailed })
	wt := wrapPanicDirtyWorktree(wrapPanicWorktree(newMockWorktree()))
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	if err := m.Retry(context.Background(), "t1", true); err != nil {
		t.Fatalf("Retry dir with confirmDirty=true (ignored): %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatalf("task row must be deleted")
	}
}

// TestRetry_DirUnknownKind_FailClosed 验证 dir Retry 未知持久化 kind → codeInternal 零副作用
// （D1：未知持久化 kind；不进入删除序列，状态不变）。
func TestRetry_DirUnknownKind_FailClosed(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, Kind: "weird"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "my task",
		Status: StatusDeletionFailed, WorktreePath: projDir, InitStatus: InitStatusNone}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.Retry(context.Background(), "t1", false)
	if !isOpErrCode(err, codeInternal) {
		t.Fatalf("Retry unknown kind: err = %v, want codeInternal (D1)", err)
	}
	// 状态不变（零副作用，未进入 deleteResume）。
	assertStatus(t, store, "t1", StatusDeletionFailed)
}

// TestRetry_RepoDirtyGate_UnchangedRegression 验证 repo 任务 Retry 的 dirty 门禁行为不变
// （confirmDirty=false 且 dirty 非空 → 409 拒绝），确保 kind 分叉未破坏 repo 路径。
func TestRetry_RepoDirtyGate_UnchangedRegression(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := &programmableDirtyWorktree{mockWorktree: newMockWorktree()}
	// Delete: preflight 快照干净 → 二次门禁出现 new.txt → 拒绝。
	// Retry: preflight 快照含 new.txt → 非空且 !confirmDirty → 拒绝。
	wt.results = []map[string]struct{}{
		{},              // Delete #1 preflight 快照：干净
		{"new.txt": {}}, // Delete #2 二次门禁：新增 dirty → 拒绝
		{"new.txt": {}}, // Retry #1 preflight 快照：dirty 非空 → !confirmDirty → 拒绝
	}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	if err := m.Delete(context.Background(), "t1", DeleteNormal, false); err == nil || !isOpErrCode(err, codeConflict) {
		t.Fatalf("first Delete must be rejected (new dirty); err=%v", err)
	}
	if err := m.Retry(context.Background(), "t1", false); err == nil || !isOpErrCode(err, codeConflict) {
		t.Fatalf("repo Retry with dirty and no confirmDirty MUST be rejected; err=%v", err)
	}
	assertStatus(t, store, "t1", StatusDeletionFailed)
}
