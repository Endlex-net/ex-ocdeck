// p142_guard_delegation_test.go 验证 P1.4.2 guard 委托 domain 后行为与委托前
// byte-equivalent（design D0 P1.4.2 strangler 第二步）。
//
// 覆盖每个 guard 分支（放行/拒绝/未知状态 fail-closed），断言：
//   - 决策（err 是否为 nil 或 err 的 Code）与委托前一致；
//   - 错误消息与委托前逐字一致（含未知状态 fail-closed）。
//
// 这些测试与既有 p141 trace 测试互补：p141 断言副作用顺序；本文件断言 guard 决策
// 与错误映射的等价性。Manager 现状已委托 domain guard，本测试作为委托后的等价性
// 回归 oracle，锁定决策与错误模板不变。
package task

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	ocdecktask "ocdeck/internal/domain/task"
)

// assertCodeEq 断言 err 的 OpError code 等于 wantCode（err==nil 时 wantCode 必须为空串）。
func assertCodeEq(t *testing.T, label string, err error, wantCode string) {
	t.Helper()
	got := OpErrorCode(err)
	if got != wantCode {
		t.Fatalf("%s: code = %q, want %q (err=%v)", label, got, wantCode, err)
	}
}

// assertErrContains 断言 err 非 nil 且消息包含 substr。
func assertErrContains(t *testing.T, label, substr string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: err nil, want containing %q", label, substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("%s: err=%q must contain %q", label, err.Error(), substr)
	}
}

// --- Suspend guard 等价性 ---

func TestP142_SuspendGuard_Equivalence(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantCode string
		errSub   string
	}{
		{"active allowed", StatusActive, "", ""},
		{"suspended rejected", StatusSuspended, codeInvalidState, "suspend requires active, got suspended"},
		{"creating rejected", StatusCreating, codeInvalidState, "suspend requires active, got creating"},
		{"archived rejected", StatusArchived, codeInvalidState, "suspend requires active, got archived"},
		{"unknown status rejected", "bogus", codeInvalidState, "suspend requires active, got bogus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: c.status, WorktreePath: "/wt"}
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			err := m.Suspend(context.Background(), "t1")
			if c.wantCode == "" {
				if err != nil && strings.Contains(err.Error(), "suspend requires active") {
					t.Fatalf("%s: expected allow, got guard reject: %v", c.name, err)
				}
				// 放行后可能因 mock proc 等返回非 guard 错误，不计入 guard 等价性。
				return
			}
			assertCodeEq(t, c.name, err, c.wantCode)
			assertErrContains(t, c.name, c.errSub, err)
		})
	}
}

// --- Archive guard 等价性（status 维度 + init 维度） ---

func TestP142_ArchiveGuard_Equivalence(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		initStatus string
		wantCode   string
		errSub     string
	}{
		{"suspended/none allowed", StatusSuspended, InitStatusNone, "", ""},
		{"suspended/succeeded allowed", StatusSuspended, InitStatusSucceeded, "", ""},
		{"suspended/pending rejected init", StatusSuspended, InitStatusPending, codeInvalidState, "init in progress (init_status=pending)"},
		{"suspended/running rejected init", StatusSuspended, InitStatusRunning, codeInvalidState, "init in progress (init_status=running)"},
		{"suspended/failed allowed (init not in progress)", StatusSuspended, InitStatusFailed, "", ""},
		{"active rejected status", StatusActive, InitStatusNone, codeInvalidState, "archive requires suspended, got active"},
		{"creating rejected status", StatusCreating, InitStatusNone, codeInvalidState, "archive requires suspended, got creating"},
		{"archived rejected status", StatusArchived, InitStatusNone, codeInvalidState, "archive requires suspended, got archived"},
		{"unknown status rejected", "bogus", InitStatusNone, codeInvalidState, "archive requires suspended, got bogus"},
		// 两个维度都失败：status 优先报错（byte-equivalent）。
		{"active/pending status-first", StatusActive, InitStatusPending, codeInvalidState, "archive requires suspended, got active"},
		{"creating/running status-first", StatusCreating, InitStatusRunning, codeInvalidState, "archive requires suspended, got creating"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: c.status, InitStatus: c.initStatus, WorktreePath: "/wt"}
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			err := m.Archive(context.Background(), "t1")
			if c.wantCode == "" {
				if err != nil {
					// 放行后 Archive 直接落库；mock store 不应失败。
					t.Fatalf("%s: expected allow, got %v", c.name, err)
				}
				return
			}
			assertCodeEq(t, c.name, err, c.wantCode)
			assertErrContains(t, c.name, c.errSub, err)
		})
	}
}

// --- Restore guard 等价性 ---

func TestP142_RestoreGuard_Equivalence(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantCode string
		errSub   string
	}{
		{"archived allowed", StatusArchived, "", ""},
		{"suspended rejected", StatusSuspended, codeInvalidState, "restore requires archived, got suspended"},
		{"active rejected", StatusActive, codeInvalidState, "restore requires archived, got active"},
		{"creating rejected", StatusCreating, codeInvalidState, "restore requires archived, got creating"},
		{"unknown status rejected", "bogus", codeInvalidState, "restore requires archived, got bogus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: c.status, WorktreePath: "/wt"}
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			err := m.Restore(context.Background(), "t1")
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("%s: expected allow, got %v", c.name, err)
				}
				return
			}
			assertCodeEq(t, c.name, err, c.wantCode)
			assertErrContains(t, c.name, c.errSub, err)
		})
	}
}

// --- Delete guard 等价性（status/mode 维度 + init 维度，status/mode 优先） ---

func TestP142_DeleteGuard_Equivalence(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		initStatus string
		mode       DeleteMode
		wantCode   string
		errSub     string
	}{
		// Normal 放行
		{"normal suspended allowed", StatusSuspended, InitStatusNone, DeleteNormal, "", ""},
		{"normal archived allowed", StatusArchived, InitStatusNone, DeleteNormal, "", ""},
		{"normal creation_failed allowed", StatusCreationFailed, InitStatusNone, DeleteNormal, "", ""},
		// Normal 拒绝 deletion_failed
		{"normal deletion_failed rejected status", StatusDeletionFailed, InitStatusNone, DeleteNormal, codeInvalidState, "delete not allowed from deletion_failed with mode normal"},
		// Force 放行 deletion_failed
		{"force deletion_failed allowed", StatusDeletionFailed, InitStatusNone, DeleteForce, "", ""},
		{"force suspended allowed", StatusSuspended, InitStatusNone, DeleteForce, "", ""},
		// 两者拒绝 active/creating/deleting/activating/suspending（status 优先报错）
		{"normal active rejected status", StatusActive, InitStatusNone, DeleteNormal, codeInvalidState, "delete not allowed from active with mode normal"},
		{"force active rejected status", StatusActive, InitStatusNone, DeleteForce, codeInvalidState, "delete not allowed from active with mode force"},
		{"normal creating rejected status", StatusCreating, InitStatusNone, DeleteNormal, codeInvalidState, "delete not allowed from creating with mode normal"},
		// init 进行中拒绝（status 合法时 init 报错）
		{"normal suspended init=pending rejected init", StatusSuspended, InitStatusPending, DeleteNormal, codeInvalidState, "init in progress (init_status=pending)"},
		{"force suspended init=running rejected init", StatusSuspended, InitStatusRunning, DeleteForce, codeInvalidState, "init in progress (init_status=running)"},
		{"force deletion_failed init=pending rejected init", StatusDeletionFailed, InitStatusPending, DeleteForce, codeInvalidState, "init in progress (init_status=pending)"},
		// init=failed|succeeded 不阻断
		{"normal suspended init=failed allowed", StatusSuspended, InitStatusFailed, DeleteNormal, "", ""},
		{"normal suspended init=succeeded allowed", StatusSuspended, InitStatusSucceeded, DeleteNormal, "", ""},
		// 两个维度都失败：status/mode 优先报错（byte-equivalent）。
		{"normal active init=pending status-first", StatusActive, InitStatusPending, DeleteNormal, codeInvalidState, "delete not allowed from active with mode normal"},
		{"normal creating init=running status-first", StatusCreating, InitStatusRunning, DeleteNormal, codeInvalidState, "delete not allowed from creating with mode normal"},
		// 未知 status fail-closed
		{"normal unknown status rejected", "bogus", InitStatusNone, DeleteNormal, codeInvalidState, "delete not allowed from bogus with mode normal"},
		{"force unknown status rejected", "bogus", InitStatusNone, DeleteForce, codeInvalidState, "delete not allowed from bogus with mode force"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: c.status, InitStatus: c.initStatus, Branch: "ocdeck/t", WorktreePath: t.TempDir()}
			// 预置 runtime（Delete 成功后 clearRuntime 需要 runtime 存在）。
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			rt := m.newRuntime("t1")
			m.setRuntime("t1", rt)
			err := m.Delete(context.Background(), "t1", c.mode, false)
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("%s: expected allow, got %v", c.name, err)
				}
				return
			}
			assertCodeEq(t, c.name, err, c.wantCode)
			assertErrContains(t, c.name, c.errSub, err)
		})
	}
}

// --- Activate init_status 五分支 guard 等价性 ---

func TestP142_ActivateInitStatusGuard_Equivalence(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		initStatus string
		initError  sql.NullString
		wantCode   string
		errSub     string
	}{
		// 放行（none/succeeded）—— status 必须 suspended 才走到 init guard。
		{"suspended/none allowed", StatusSuspended, InitStatusNone, sql.NullString{}, "", ""},
		{"suspended/succeeded allowed", StatusSuspended, InitStatusSucceeded, sql.NullString{}, "", ""},
		// 拒绝：init in progress
		{"suspended/pending rejected", StatusSuspended, InitStatusPending, sql.NullString{}, codeInvalidState, "init in progress (init_status=pending)"},
		{"suspended/running rejected", StatusSuspended, InitStatusRunning, sql.NullString{}, codeInvalidState, "init in progress (init_status=running)"},
		// 拒绝：init failed 含 init_error
		{"suspended/failed with error", StatusSuspended, InitStatusFailed, sql.NullString{String: "boom", Valid: true}, codeInvalidState, "init failed: boom"},
		{"suspended/failed no error", StatusSuspended, InitStatusFailed, sql.NullString{}, codeInvalidState, "init failed"},
		// 未知 init_status fail-closed
		{"suspended/unknown rejected fail-closed", StatusSuspended, "bogus", sql.NullString{}, codeInvalidState, "unknown init_status"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: c.status, InitStatus: c.initStatus, InitError: c.initError, WorktreePath: "/wt"}
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			err := m.Activate(context.Background(), "t1")
			if c.wantCode == "" {
				// 放行后可能因 mock serve 健康检查失败等返回非 guard 错误；只要不是 init guard 错误即可。
				if err != nil && (strings.Contains(err.Error(), "init in progress") || strings.Contains(err.Error(), "init failed") || strings.Contains(err.Error(), "unknown init_status")) {
					t.Fatalf("%s: expected allow, got init guard reject: %v", c.name, err)
				}
				return
			}
			assertCodeEq(t, c.name, err, c.wantCode)
			assertErrContains(t, c.name, c.errSub, err)
		})
	}
}

// TestP142_DomainGuardDecisionMatchesLegacy 断言 domain guard 决策与 legacy 内联
// 判定在等价输入下一致（决策层等价性，独立于错误消息）。
func TestP142_DomainGuardDecisionMatchesLegacy(t *testing.T) {
	// Suspend: legacy `status == active` == domain CanSuspend
	for _, s := range []string{StatusSuspended, StatusActive, StatusCreating, StatusArchived, "bogus"} {
		row := TaskRow{Status: s, InitStatus: InitStatusNone}
		legacyAllow := row.Status == StatusActive
		domainAllow := rehydrateGuardView(row).CanSuspend()
		if legacyAllow != domainAllow {
			t.Fatalf("Suspend %s: legacy=%v domain=%v", s, legacyAllow, domainAllow)
		}
	}
	// Restore: legacy `status == archived` == domain CanRestore
	for _, s := range []string{StatusSuspended, StatusActive, StatusArchived, StatusCreating, "bogus"} {
		row := TaskRow{Status: s, InitStatus: InitStatusNone}
		legacyAllow := row.Status == StatusArchived
		domainAllow := rehydrateGuardView(row).CanRestore()
		if legacyAllow != domainAllow {
			t.Fatalf("Restore %s: legacy=%v domain=%v", s, legacyAllow, domainAllow)
		}
	}
	// Archive: legacy `status==suspended && init not in {pending,running}` == domain CanArchive
	for _, s := range []string{StatusSuspended, StatusActive, StatusCreating, StatusArchived, "bogus"} {
		for _, is := range []string{InitStatusNone, InitStatusPending, InitStatusRunning, InitStatusSucceeded, InitStatusFailed, "bogus"} {
			row := TaskRow{Status: s, InitStatus: is}
			legacyAllow := row.Status == StatusSuspended && row.InitStatus != InitStatusPending && row.InitStatus != InitStatusRunning
			domainAllow := rehydrateGuardView(row).CanArchive()
			if legacyAllow != domainAllow {
				t.Fatalf("Archive status=%s init=%s: legacy=%v domain=%v", s, is, legacyAllow, domainAllow)
			}
		}
	}
	// Delete: legacy `deleteAllowedStatus(status,mode) && init not in {pending,running}` == domain CanDelete(mode)
	for _, s := range []string{StatusSuspended, StatusArchived, StatusCreationFailed, StatusDeletionFailed, StatusActive, StatusCreating, StatusDeleting, "bogus"} {
		for _, is := range []string{InitStatusNone, InitStatusPending, InitStatusRunning, InitStatusSucceeded, InitStatusFailed, "bogus"} {
			for _, mode := range []DeleteMode{DeleteNormal, DeleteForce} {
				row := TaskRow{Status: s, InitStatus: is}
				legacyAllow := deleteAllowedStatus(row.Status, mode) && row.InitStatus != InitStatusPending && row.InitStatus != InitStatusRunning
				domainAllow := rehydrateGuardView(row).CanDelete(ocdecktask.DeleteMode(mode))
				if legacyAllow != domainAllow {
					t.Fatalf("Delete status=%s init=%s mode=%s: legacy=%v domain=%v", s, is, mode, legacyAllow, domainAllow)
				}
			}
		}
	}
	// Activate init_status 维度：在 status=suspended 且无阻断 notice 时，
	// legacy `init in {none,succeeded}` == domain CanActivate（notices 为空，等同无阻断）。
	for _, is := range []string{InitStatusNone, InitStatusPending, InitStatusRunning, InitStatusSucceeded, InitStatusFailed, "bogus"} {
		row := TaskRow{Status: StatusSuspended, InitStatus: is}
		legacyAllow := is == InitStatusNone || is == InitStatusSucceeded
		domainAllow := rehydrateGuardView(row).CanActivate()
		if legacyAllow != domainAllow {
			t.Fatalf("Activate init_status=%s: legacy=%v domain=%v", is, legacyAllow, domainAllow)
		}
	}
}