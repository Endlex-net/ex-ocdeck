package task

import (
	"testing"
)

// --- 枚举值逐字一致性（对齐 internal/task/types.go:37-57） ---

func TestStatusConstants(t *testing.T) {
	cases := []struct {
		name string
		s    Status
		want string
	}{
		{"Suspended", StatusSuspended, "suspended"},
		{"Active", StatusActive, "active"},
		{"Archived", StatusArchived, "archived"},
		{"Creating", StatusCreating, "creating"},
		{"CreationFailed", StatusCreationFailed, "creation_failed"},
		{"Activating", StatusActivating, "activating"},
		{"Suspending", StatusSuspending, "suspending"},
		{"Deleting", StatusDeleting, "deleting"},
		{"DeletionFailed", StatusDeletionFailed, "deletion_failed"},
	}
	if len(cases) != 9 {
		t.Fatalf("expected 9 Status constants, got %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if string(c.s) != c.want {
				t.Fatalf("Status %s = %q, want %q", c.name, c.s, c.want)
			}
		})
	}
}

func TestInitStatusConstants(t *testing.T) {
	cases := []struct {
		name string
		s    InitStatus
		want string
	}{
		{"None", InitStatusNone, "none"},
		{"Pending", InitStatusPending, "pending"},
		{"Running", InitStatusRunning, "running"},
		{"Succeeded", InitStatusSucceeded, "succeeded"},
		{"Failed", InitStatusFailed, "failed"},
	}
	if len(cases) != 5 {
		t.Fatalf("expected 5 InitStatus constants, got %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if string(c.s) != c.want {
				t.Fatalf("InitStatus %s = %q, want %q", c.name, c.s, c.want)
			}
		})
	}
}

func TestDeleteModeConstants(t *testing.T) {
	if string(DeleteModeNormal) != "normal" {
		t.Fatalf("DeleteModeNormal = %q", DeleteModeNormal)
	}
	if string(DeleteModeForce) != "force" {
		t.Fatalf("DeleteModeForce = %q", DeleteModeForce)
	}
}

// --- 构造不变量 ---

func TestNewInvariants(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		got, err := New(NewInput{
			ID: "tsk_1", ProjectID: "prj_1", Name: "fix login",
			WorktreePath: "/wt/1",
		})
		if err != nil {
			t.Fatalf("New err: %v", err)
		}
		if got.Status() != StatusCreating {
			t.Fatalf("status = %s, want creating", got.Status())
		}
		if got.InitStatus() != InitStatusNone {
			t.Fatalf("init_status = %s, want none", got.InitStatus())
		}
		if got.DeleteMode() != "" {
			t.Fatalf("delete_mode = %q, want empty", got.DeleteMode())
		}
		if len(got.Notices()) != 0 {
			t.Fatalf("notices = %v, want empty", got.Notices())
		}
		if got.ID() != "tsk_1" || got.ProjectID() != "prj_1" || got.Name() != "fix login" {
			t.Fatalf("identity fields mismatch: %+v", got)
		}
	})
	t.Run("empty ID rejected", func(t *testing.T) {
		if _, err := New(NewInput{ProjectID: "p", Name: "n", WorktreePath: "/w"}); err == nil {
			t.Fatal("expected err for empty ID")
		}
	})
	t.Run("empty ProjectID rejected", func(t *testing.T) {
		if _, err := New(NewInput{ID: "t", Name: "n", WorktreePath: "/w"}); err == nil {
			t.Fatal("expected err for empty ProjectID")
		}
	})
	t.Run("empty Name rejected", func(t *testing.T) {
		if _, err := New(NewInput{ID: "t", ProjectID: "p", WorktreePath: "/w"}); err == nil {
			t.Fatal("expected err for empty Name")
		}
	})
	t.Run("empty WorktreePath rejected", func(t *testing.T) {
		if _, err := New(NewInput{ID: "t", ProjectID: "p", Name: "n"}); err == nil {
			t.Fatal("expected err for empty WorktreePath")
		}
	})
}

// helper 构造一个任意状态的 Task（绕过 New 的 creating 初始化，用于 guard 测试）。
func taskAt(status Status, initStatus InitStatus, notices []Notice) *Task {
	t := &Task{
		id:         "tsk_1",
		projectID:  "prj_1",
		name:       "n",
		worktreePath: "/w",
		status:     status,
		initStatus: initStatus,
	}
	t.SetNotices(notices)
	return t
}

// --- status 状态机矩阵逐行表驱动（design D0 矩阵） ---
//
// 每行断言 from/to/guard 放行或拒绝/是否产生迁移。
// 补偿性落账行（creating→creation_failed、任意→creation_failed、deleting→deletion_failed）
// 在 domain 总是允许（无 guard），但应用层 CAS 未命中路径不调用 Apply。

func TestStatusMatrix(t *testing.T) {
	type row struct {
		name        string
		from        Status
		initStatus  InitStatus
		notices     []Notice
		apply       func(*Task) error
		wantErr     bool   // guard 是否拒绝
		wantTo      Status // 迁移后状态（wantErr=false 时校验）
		wantMigrate bool   // 是否产生真实迁移（guard 通过即迁移）
	}

	// 注意：CanDelete 的 mode 维度单独在 TestCanDelete 覆盖。
	rows := []row{
		{
			name: "creating→suspended (CommitCreated, init=none)",
			from: StatusCreating, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyCommitCreated(InitStatusNone) },
			wantErr: false, wantTo: StatusSuspended, wantMigrate: true,
		},
		{
			name: "creation_failed→suspended (Retry CommitCreated, init=none)",
			from: StatusCreationFailed, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyCommitCreated(InitStatusNone) },
			wantErr: false, wantTo: StatusSuspended, wantMigrate: true,
		},
		{
			name: "CommitCreated rejects non creating|creation_failed",
			from: StatusActive, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyCommitCreated(InitStatusNone) },
			wantErr: true, wantTo: StatusActive, wantMigrate: false,
		},
		{
			name: "CommitCreated rejects init=pending (only none|pending allowed, but pending via ApplyCommitInitPending path)",
			// 这里直接传 pending 给 ApplyCommitCreated，构造函数应拒绝（validateInitStatusForCommit 允许 none|pending，
			// 故 pending 合法）。改为传 succeeded 触发拒绝。
			from: StatusCreating, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyCommitCreated(InitStatusSucceeded) },
			wantErr: true, wantTo: StatusCreating, wantMigrate: false,
		},
		{
			name: "suspended→activating (CanActivate ok, init=none)",
			from: StatusSuspended, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyActivate() },
			wantErr: false, wantTo: StatusActivating, wantMigrate: true,
		},
		{
			name: "suspended→activating (CanActivate ok, init=succeeded)",
			from: StatusSuspended, initStatus: InitStatusSucceeded,
			apply: func(t *Task) error { return t.ApplyActivate() },
			wantErr: false, wantTo: StatusActivating, wantMigrate: true,
		},
		{
			name: "suspended→activating rejected (init=pending)",
			from: StatusSuspended, initStatus: InitStatusPending,
			apply: func(t *Task) error { return t.ApplyActivate() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "suspended→activating rejected (init=running)",
			from: StatusSuspended, initStatus: InitStatusRunning,
			apply: func(t *Task) error { return t.ApplyActivate() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "suspended→activating rejected (init=failed)",
			from: StatusSuspended, initStatus: InitStatusFailed,
			apply: func(t *Task) error { return t.ApplyActivate() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "suspended→activating rejected (init=unknown fail-closed)",
			from: StatusSuspended, initStatus: "bogus",
			apply: func(t *Task) error { return t.ApplyActivate() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "suspended→activating rejected (retryable residual notice)",
			from: StatusSuspended, initStatus: InitStatusNone,
			notices: []Notice{{Code: NoticeCodeResidualProcesses, Data: NoticeData{SessionName: "s1", Retryable: true}}},
			apply: func(t *Task) error { return t.ApplyActivate() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "suspended→activating rejected (wrong status)",
			from: StatusActive, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyActivate() },
			wantErr: true, wantTo: StatusActive, wantMigrate: false,
		},
		{
			name: "activating→active (commit)",
			from: StatusActivating, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyActivateCommit() },
			wantErr: false, wantTo: StatusActive, wantMigrate: true,
		},
		{
			name: "activating→active rejected (wrong status)",
			from: StatusSuspended, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyActivateCommit() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "activating→suspended (compensate)",
			from: StatusActivating, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyActivateCompensate() },
			wantErr: false, wantTo: StatusSuspended, wantMigrate: true,
		},
		{
			name: "active→suspending (CanSuspend ok)",
			from: StatusActive, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplySuspend() },
			wantErr: false, wantTo: StatusSuspending, wantMigrate: true,
		},
		{
			name: "active→suspending rejected (wrong status)",
			from: StatusSuspended, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplySuspend() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "suspending→active (repair)",
			from: StatusSuspending, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplySuspendRepair() },
			wantErr: false, wantTo: StatusActive, wantMigrate: true,
		},
		{
			name: "suspending→suspended (complete)",
			from: StatusSuspending, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplySuspendComplete() },
			wantErr: false, wantTo: StatusSuspended, wantMigrate: true,
		},
		{
			name: "active→suspended (converge)",
			from: StatusActive, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyConvergeSuspended() },
			wantErr: false, wantTo: StatusSuspended, wantMigrate: true,
		},
		{
			name: "active→suspended converge rejected (wrong status)",
			from: StatusSuspending, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyConvergeSuspended() },
			wantErr: true, wantTo: StatusSuspending, wantMigrate: false,
		},
		{
			name: "suspended→archived (CanArchive ok, init=none)",
			from: StatusSuspended, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyArchive() },
			wantErr: false, wantTo: StatusArchived, wantMigrate: true,
		},
		{
			name: "suspended→archived rejected (init=pending)",
			from: StatusSuspended, initStatus: InitStatusPending,
			apply: func(t *Task) error { return t.ApplyArchive() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "suspended→archived rejected (init=running)",
			from: StatusSuspended, initStatus: InitStatusRunning,
			apply: func(t *Task) error { return t.ApplyArchive() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "suspended→archived rejected (wrong status)",
			from: StatusActive, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyArchive() },
			wantErr: true, wantTo: StatusActive, wantMigrate: false,
		},
		{
			name: "archived→suspended (CanRestore ok)",
			from: StatusArchived, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyRestore() },
			wantErr: false, wantTo: StatusSuspended, wantMigrate: true,
		},
		{
			name: "archived→suspended rejected (wrong status)",
			from: StatusSuspended, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyRestore() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
		{
			name: "deletion_failed→deleting (retry reenter)",
			from: StatusDeletionFailed, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyRetryReenterDelete() },
			wantErr: false, wantTo: StatusDeleting, wantMigrate: true,
		},
		{
			name: "deletion_failed→deleting rejected (wrong status)",
			from: StatusSuspended, initStatus: InitStatusNone,
			apply: func(t *Task) error { return t.ApplyRetryReenterDelete() },
			wantErr: true, wantTo: StatusSuspended, wantMigrate: false,
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			tk := taskAt(r.from, r.initStatus, r.notices)
			err := r.apply(tk)
			if r.wantErr {
				if err == nil {
					t.Fatalf("expected guard rejection, got nil err; status=%s", tk.Status())
				}
				if tk.Status() != r.wantTo {
					t.Fatalf("status changed on rejected transition: got %s, want %s", tk.Status(), r.wantTo)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tk.Status() != r.wantTo {
				t.Fatalf("status = %s, want %s", tk.Status(), r.wantTo)
			}
			if !r.wantMigrate {
				t.Fatalf("wantMigrate=false but guard passed (test row misconfigured)")
			}
		})
	}
}

// --- 补偿性落账（无 guard）单独验证 ---

func TestCompensatingTransitions(t *testing.T) {
	t.Run("ApplyCreationFailed always allowed", func(t *testing.T) {
		// 从 creating 落 creation_failed。
		tk := taskAt(StatusCreating, InitStatusNone, nil)
		tk.ApplyCreationFailed(StatusCreating, "boom")
		if tk.Status() != StatusCreationFailed {
			t.Fatalf("status = %s", tk.Status())
		}
		if tk.LastError() != "boom" {
			t.Fatalf("lastError = %q", tk.LastError())
		}
	})
	t.Run("ApplyCreationFailed empty lastError preserves existing", func(t *testing.T) {
		tk := taskAt(StatusCreating, InitStatusNone, nil)
		tk.lastError = "prev"
		tk.ApplyCreationFailed(StatusCreating, "")
		if tk.LastError() != "prev" {
			t.Fatalf("lastError = %q, want prev preserved", tk.LastError())
		}
	})
	t.Run("ApplyDeletionFailed always allowed", func(t *testing.T) {
		tk := taskAt(StatusDeleting, InitStatusNone, nil)
		tk.ApplyDeletionFailed("oc session delete failed")
		if tk.Status() != StatusDeletionFailed {
			t.Fatalf("status = %s", tk.Status())
		}
		if tk.LastError() != "oc session delete failed" {
			t.Fatalf("lastError = %q", tk.LastError())
		}
	})
}

// --- CanDelete guard 覆盖 mode 与 init_status 维度 ---

func TestCanDelete(t *testing.T) {
	cases := []struct {
		name       string
		status     Status
		initStatus InitStatus
		mode       DeleteMode
		want       bool
	}{
		// Normal 模式仅允许 suspended|archived|creation_failed。
		{"Normal suspended", StatusSuspended, InitStatusNone, DeleteModeNormal, true},
		{"Normal archived", StatusArchived, InitStatusNone, DeleteModeNormal, true},
		{"Normal creation_failed", StatusCreationFailed, InitStatusNone, DeleteModeNormal, true},
		{"Normal deletion_failed rejected", StatusDeletionFailed, InitStatusNone, DeleteModeNormal, false},
		{"Normal active rejected", StatusActive, InitStatusNone, DeleteModeNormal, false},
		{"Normal creating rejected", StatusCreating, InitStatusNone, DeleteModeNormal, false},
		{"Normal deleting rejected", StatusDeleting, InitStatusNone, DeleteModeNormal, false},
		{"Normal activating rejected", StatusActivating, InitStatusNone, DeleteModeNormal, false},
		{"Normal suspending rejected", StatusSuspending, InitStatusNone, DeleteModeNormal, false},
		// Force 额外允许 deletion_failed。
		{"Force suspended", StatusSuspended, InitStatusNone, DeleteModeForce, true},
		{"Force archived", StatusArchived, InitStatusNone, DeleteModeForce, true},
		{"Force creation_failed", StatusCreationFailed, InitStatusNone, DeleteModeForce, true},
		{"Force deletion_failed allowed", StatusDeletionFailed, InitStatusNone, DeleteModeForce, true},
		{"Force active rejected", StatusActive, InitStatusNone, DeleteModeForce, false},
		{"Force creating rejected", StatusCreating, InitStatusNone, DeleteModeForce, false},
		{"Force deleting rejected", StatusDeleting, InitStatusNone, DeleteModeForce, false},
		// init 进行中两者均拒绝。
		{"Normal suspended init=pending rejected", StatusSuspended, InitStatusPending, DeleteModeNormal, false},
		{"Normal suspended init=running rejected", StatusSuspended, InitStatusRunning, DeleteModeNormal, false},
		{"Force suspended init=pending rejected", StatusSuspended, InitStatusPending, DeleteModeForce, false},
		{"Force deletion_failed init=running rejected", StatusDeletionFailed, InitStatusRunning, DeleteModeForce, false},
		{"Force creation_failed init=pending rejected", StatusCreationFailed, InitStatusPending, DeleteModeForce, false},
		// init=succeeded|failed|none 不阻断（只要不在 pending|running）。
		{"Normal suspended init=succeeded ok", StatusSuspended, InitStatusSucceeded, DeleteModeNormal, true},
		{"Normal suspended init=failed ok", StatusSuspended, InitStatusFailed, DeleteModeNormal, true},
		{"Force deletion_failed init=failed ok", StatusDeletionFailed, InitStatusFailed, DeleteModeForce, true},
		// 未知 mode fail-closed。
		{"Unknown mode rejected", StatusSuspended, InitStatusNone, "bogus", false},
		{"Empty mode rejected", StatusSuspended, InitStatusNone, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tk := taskAt(c.status, c.initStatus, nil)
			got := tk.CanDelete(c.mode)
			if got != c.want {
				t.Fatalf("CanDelete(%s, %s, init=%s) = %v, want %v", c.status, c.mode, c.initStatus, got, c.want)
			}
			// 对 ApplyBeginDeleteIntent 做对称验证：guard 通过则迁移成功，否则返回 error。
			err := tk.ApplyBeginDeleteIntent(c.mode)
			if c.want {
				if err != nil {
					t.Fatalf("ApplyBeginDeleteIntent err: %v", err)
				}
				if tk.Status() != StatusDeleting {
					t.Fatalf("status = %s, want deleting", tk.Status())
				}
				if tk.DeleteMode() != c.mode {
					t.Fatalf("deleteMode = %q, want %q", tk.DeleteMode(), c.mode)
				}
			} else {
				if err == nil {
					t.Fatal("expected ApplyBeginDeleteIntent err, got nil")
				}
			}
		})
	}
}

// --- CanArchive 覆盖 mode 与 init_status 维度（CanArchive 不依赖 mode，但覆盖 init 维度） ---

func TestCanArchive(t *testing.T) {
	cases := []struct {
		name       string
		status     Status
		initStatus InitStatus
		want       bool
	}{
		{"suspended init=none", StatusSuspended, InitStatusNone, true},
		{"suspended init=succeeded", StatusSuspended, InitStatusSucceeded, true},
		{"suspended init=failed", StatusSuspended, InitStatusFailed, true},
		{"suspended init=pending rejected", StatusSuspended, InitStatusPending, false},
		{"suspended init=running rejected", StatusSuspended, InitStatusRunning, false},
		{"active rejected", StatusActive, InitStatusNone, false},
		{"archived rejected", StatusArchived, InitStatusNone, false},
		{"creation_failed rejected", StatusCreationFailed, InitStatusNone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tk := taskAt(c.status, c.initStatus, nil)
			got := tk.CanArchive()
			if got != c.want {
				t.Fatalf("CanArchive(%s, init=%s) = %v, want %v", c.status, c.initStatus, got, c.want)
			}
		})
	}
}

// --- init_status 合法/非法流转表驱动 ---

func TestInitStatusTransitions(t *testing.T) {
	t.Run("ClaimInitRun pending→running", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusPending, nil)
		if err := tk.ApplyClaimInitRun(); err != nil {
			t.Fatalf("err: %v", err)
		}
		if tk.InitStatus() != InitStatusRunning {
			t.Fatalf("init_status = %s", tk.InitStatus())
		}
	})
	t.Run("ClaimInitRun rejected non-pending", func(t *testing.T) {
		for _, s := range []InitStatus{InitStatusNone, InitStatusRunning, InitStatusSucceeded, InitStatusFailed} {
			tk := taskAt(StatusSuspended, s, nil)
			if err := tk.ApplyClaimInitRun(); err == nil {
				t.Fatalf("expected err for init=%s", s)
			}
		}
	})
	t.Run("FinishInitRun running→succeeded", func(t *testing.T) {
		tk := taskAt(StatusActive, InitStatusRunning, nil)
		if err := tk.ApplyFinishInitRun(true, ""); err != nil {
			t.Fatalf("err: %v", err)
		}
		if tk.InitStatus() != InitStatusSucceeded {
			t.Fatalf("init_status = %s", tk.InitStatus())
		}
		if tk.InitError() != "" {
			t.Fatalf("initError = %q, want empty", tk.InitError())
		}
	})
	t.Run("FinishInitRun running→failed records error", func(t *testing.T) {
		tk := taskAt(StatusActive, InitStatusRunning, nil)
		if err := tk.ApplyFinishInitRun(false, "script exit 1"); err != nil {
			t.Fatalf("err: %v", err)
		}
		if tk.InitStatus() != InitStatusFailed {
			t.Fatalf("init_status = %s", tk.InitStatus())
		}
		if tk.InitError() != "script exit 1" {
			t.Fatalf("initError = %q", tk.InitError())
		}
	})
	t.Run("FinishInitRun rejected non-running", func(t *testing.T) {
		for _, s := range []InitStatus{InitStatusNone, InitStatusPending, InitStatusSucceeded, InitStatusFailed} {
			tk := taskAt(StatusActive, s, nil)
			if err := tk.ApplyFinishInitRun(true, ""); err == nil {
				t.Fatalf("expected err for init=%s", s)
			}
		}
	})
	t.Run("ClaimInitRerun failed→running (status=suspended)", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusFailed, nil)
		if err := tk.ApplyClaimInitRerun(); err != nil {
			t.Fatalf("err: %v", err)
		}
		if tk.InitStatus() != InitStatusRunning {
			t.Fatalf("init_status = %s", tk.InitStatus())
		}
	})
	t.Run("ClaimInitRerun succeeded→running (status=suspended)", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusSucceeded, nil)
		if err := tk.ApplyClaimInitRerun(); err != nil {
			t.Fatalf("err: %v", err)
		}
	})
	t.Run("ClaimInitRerun rejected non suspended", func(t *testing.T) {
		tk := taskAt(StatusActive, InitStatusFailed, nil)
		if err := tk.ApplyClaimInitRerun(); err == nil {
			t.Fatal("expected err for status=active")
		}
	})
	t.Run("ClaimInitRerun rejected non failed|succeeded", func(t *testing.T) {
		for _, s := range []InitStatus{InitStatusNone, InitStatusPending, InitStatusRunning} {
			tk := taskAt(StatusSuspended, s, nil)
			if err := tk.ApplyClaimInitRerun(); err == nil {
				t.Fatalf("expected err for init=%s", s)
			}
		}
	})
	t.Run("ConvergeInterruptedInit pending→failed", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusPending, nil)
		if err := tk.ApplyConvergeInterruptedInit("interrupted"); err != nil {
			t.Fatalf("err: %v", err)
		}
		if tk.InitStatus() != InitStatusFailed {
			t.Fatalf("init_status = %s", tk.InitStatus())
		}
		if tk.InitError() != "interrupted" {
			t.Fatalf("initError = %q", tk.InitError())
		}
	})
	t.Run("ConvergeInterruptedInit running→failed", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusRunning, nil)
		if err := tk.ApplyConvergeInterruptedInit("interrupted"); err != nil {
			t.Fatalf("err: %v", err)
		}
		if tk.InitStatus() != InitStatusFailed {
			t.Fatalf("init_status = %s", tk.InitStatus())
		}
	})
	t.Run("ConvergeInterruptedInit rejected non pending|running", func(t *testing.T) {
		for _, s := range []InitStatus{InitStatusNone, InitStatusSucceeded, InitStatusFailed} {
			tk := taskAt(StatusSuspended, s, nil)
			if err := tk.ApplyConvergeInterruptedInit(""); err == nil {
				t.Fatalf("expected err for init=%s", s)
			}
		}
	})
	t.Run("ApplyCommitInitPending creating→pending", func(t *testing.T) {
		tk := taskAt(StatusCreating, InitStatusNone, nil)
		if err := tk.ApplyCommitInitPending(); err != nil {
			t.Fatalf("err: %v", err)
		}
		if tk.InitStatus() != InitStatusPending {
			t.Fatalf("init_status = %s", tk.InitStatus())
		}
	})
	t.Run("ApplyCommitInitPending rejected non creating|creation_failed", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusNone, nil)
		if err := tk.ApplyCommitInitPending(); err == nil {
			t.Fatal("expected err")
		}
	})
	t.Run("ApplyCommitInitPending rejected non none", func(t *testing.T) {
		tk := taskAt(StatusCreating, InitStatusPending, nil)
		if err := tk.ApplyCommitInitPending(); err == nil {
			t.Fatal("expected err (init already pending)")
		}
	})
}

// --- typed Notice 增删 ---

func TestNoticeResidual(t *testing.T) {
	t.Run("add new returns true", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusNone, nil)
		added := tk.AddResidualNotice(Notice{
			Code: NoticeCodeResidualProcesses,
			Data: NoticeData{SessionName: "s1", Reason: ResidualReasonKillFailed, Retryable: true, CleanupTickets: []string{"tk1"}},
		})
		if !added {
			t.Fatal("expected added=true")
		}
		if !tk.HasRetryableResidual() {
			t.Fatal("HasRetryableResidual should be true")
		}
	})
	t.Run("add duplicate session replaces and unions tickets", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusNone, nil)
		tk.AddResidualNotice(Notice{
			Code: NoticeCodeResidualProcesses,
			Data: NoticeData{SessionName: "s1", CleanupTickets: []string{"tk1"}, Reason: ResidualReasonKillFailed, Retryable: true},
		})
		added := tk.AddResidualNotice(Notice{
			Code: NoticeCodeResidualProcesses,
			Data: NoticeData{SessionName: "s1", CleanupTickets: []string{"tk2", "tk1"}, Reason: ResidualReasonReapFailed, Retryable: false},
		})
		if added {
			t.Fatal("expected added=false for duplicate session")
		}
		notices := tk.Notices()
		if len(notices) != 1 {
			t.Fatalf("notices len = %d, want 1", len(notices))
		}
		// tickets union 保序旧在前新在后去重。
		wantTickets := []string{"tk1", "tk2"}
		if len(notices[0].Data.CleanupTickets) != 2 || notices[0].Data.CleanupTickets[0] != "tk1" || notices[0].Data.CleanupTickets[1] != "tk2" {
			t.Fatalf("tickets = %v, want %v", notices[0].Data.CleanupTickets, wantTickets)
		}
		// reason/retryable 以新为准。
		if notices[0].Data.Reason != ResidualReasonReapFailed {
			t.Fatalf("reason = %q, want reap_failed", notices[0].Data.Reason)
		}
		if notices[0].Data.Retryable {
			t.Fatal("retryable should be false (new value)")
		}
	})
	t.Run("clear removes by session", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusNone, nil)
		tk.AddResidualNotice(Notice{Code: NoticeCodeResidualProcesses, Data: NoticeData{SessionName: "s1", Retryable: true}})
		if !tk.ClearResidualNotice("s1") {
			t.Fatal("expected cleared=true")
		}
		if tk.HasRetryableResidual() {
			t.Fatal("HasRetryableResidual should be false after clear")
		}
		if tk.ClearResidualNotice("s1") {
			t.Fatal("expected cleared=false for missing")
		}
	})
}

func TestNoticeSessionOverflow(t *testing.T) {
	t.Run("add new returns true, duplicate returns false", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusNone, nil)
		if !tk.AddSessionOverflowNotice() {
			t.Fatal("expected added=true")
		}
		if tk.AddSessionOverflowNotice() {
			t.Fatal("expected added=false for duplicate")
		}
		if !tk.HasSessionOverflow() {
			t.Fatal("HasSessionOverflow should be true")
		}
	})
	t.Run("clear", func(t *testing.T) {
		tk := taskAt(StatusSuspended, InitStatusNone, nil)
		tk.AddSessionOverflowNotice()
		if !tk.ClearSessionOverflowNotice() {
			t.Fatal("expected cleared=true")
		}
		if tk.HasSessionOverflow() {
			t.Fatal("HasSessionOverflow should be false after clear")
		}
	})
}

func TestNoticesCopyIsDefensive(t *testing.T) {
	tk := taskAt(StatusSuspended, InitStatusNone, nil)
	tk.AddResidualNotice(Notice{Code: NoticeCodeResidualProcesses, Data: NoticeData{SessionName: "s1"}})
	out := tk.Notices()
	out[0].Data.SessionName = "mutated"
	// 内部不应被外部突变影响。
	got := tk.Notices()
	if got[0].Data.SessionName != "s1" {
		t.Fatalf("internal notice mutated: %q", got[0].Data.SessionName)
	}
}

func TestSetNotices(t *testing.T) {
	tk := taskAt(StatusSuspended, InitStatusNone, nil)
	src := []Notice{{Code: NoticeCodeSessionOverflow}, {Code: NoticeCodeResidualProcesses, Data: NoticeData{SessionName: "s1"}}}
	tk.SetNotices(src)
	// 修改 src 不影响内部。
	src[0].Code = NoticeCodeResidualProcesses
	got := tk.Notices()
	if got[0].Code != NoticeCodeSessionOverflow {
		t.Fatalf("SetNotices did not copy: got[0].Code=%q", got[0].Code)
	}
	t.Run("nil clears", func(t *testing.T) {
		tk.SetNotices(nil)
		if len(tk.Notices()) != 0 {
			t.Fatalf("expected empty after nil, got %v", tk.Notices())
		}
	})
}

func TestTransitionErrorFormatting(t *testing.T) {
	e := &TransitionError{From: StatusSuspended, To: StatusActive, Msg: "guard"}
	if e.Error() == "" {
		t.Fatal("Error() should not be empty")
	}
}

// TestRehydrateGuardEquivalent 断言 Rehydrate 构造的 guard 视图与 taskAt（直接构造）
// 在全部 guard 上决策一致：Rehydrate 不走 New 不变量，按行值直接构造，供 guard 判定。
func TestRehydrateGuardEquivalent(t *testing.T) {
	cases := []struct {
		name       string
		status     Status
		initStatus InitStatus
		mode       DeleteMode
		notices    []Notice
	}{
		{"suspended/none", StatusSuspended, InitStatusNone, DeleteModeNormal, nil},
		{"suspended/pending", StatusSuspended, InitStatusPending, DeleteModeNormal, nil},
		{"suspended/running", StatusSuspended, InitStatusRunning, DeleteModeNormal, nil},
		{"suspended/failed", StatusSuspended, InitStatusFailed, DeleteModeNormal, nil},
		{"succeeded", StatusSuspended, InitStatusSucceeded, DeleteModeNormal, nil},
		{"unknown init", StatusSuspended, "bogus", DeleteModeNormal, nil},
		{"active", StatusActive, InitStatusNone, DeleteModeNormal, nil},
		{"archived", StatusArchived, InitStatusNone, DeleteModeNormal, nil},
		{"creation_failed/normal", StatusCreationFailed, InitStatusNone, DeleteModeNormal, nil},
		{"creation_failed/force", StatusCreationFailed, InitStatusNone, DeleteModeForce, nil},
		{"deletion_failed/normal", StatusDeletionFailed, InitStatusNone, DeleteModeNormal, nil},
		{"deletion_failed/force", StatusDeletionFailed, InitStatusNone, DeleteModeForce, nil},
		{"deletion_failed/force/init-running", StatusDeletionFailed, InitStatusRunning, DeleteModeForce, nil},
		{"with retryable residual notice", StatusSuspended, InitStatusNone, DeleteModeNormal,
			[]Notice{{Code: NoticeCodeResidualProcesses, Data: NoticeData{Retryable: true}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			direct := taskAt(c.status, c.initStatus, c.notices)
			rh := Rehydrate(GuardView{Status: c.status, InitStatus: c.initStatus, Notices: c.notices})

			if direct.CanActivate() != rh.CanActivate() {
				t.Fatalf("CanActivate: direct=%v rehydrate=%v", direct.CanActivate(), rh.CanActivate())
			}
			if direct.CanSuspend() != rh.CanSuspend() {
				t.Fatalf("CanSuspend: direct=%v rehydrate=%v", direct.CanSuspend(), rh.CanSuspend())
			}
			if direct.CanArchive() != rh.CanArchive() {
				t.Fatalf("CanArchive: direct=%v rehydrate=%v", direct.CanArchive(), rh.CanArchive())
			}
			if direct.CanRestore() != rh.CanRestore() {
				t.Fatalf("CanRestore: direct=%v rehydrate=%v", direct.CanRestore(), rh.CanRestore())
			}
			if direct.CanDelete(c.mode) != rh.CanDelete(c.mode) {
				t.Fatalf("CanDelete(%s): direct=%v rehydrate=%v", c.mode, direct.CanDelete(c.mode), rh.CanDelete(c.mode))
			}
		})
	}
}

// TestRehydrateNoticesDefensiveCopy 断言 Rehydrate 对 notices 做防御性拷贝，
// 外部修改不污染内部集合（与 SetNotices 一致）。
func TestRehydrateNoticesDefensiveCopy(t *testing.T) {
	notices := []Notice{{Code: NoticeCodeSessionOverflow}}
	rh := Rehydrate(GuardView{Status: StatusSuspended, InitStatus: InitStatusNone, Notices: notices})
	if !rh.HasSessionOverflow() {
		t.Fatal("Rehydrate must populate notices")
	}
	// 修改外部 slice 不影响内部副本（防御性拷贝）。
	notices[0] = Notice{Code: NoticeCodeResidualProcesses}
	if !rh.HasSessionOverflow() {
		t.Fatal("external mutation leaked into Rehydrate view: internal copy was affected")
	}
}

// TestRehydrateNilNotices 断言 Rehydrate 接受 nil notices 且 guard 行为与空集合一致。
func TestRehydrateNilNotices(t *testing.T) {
	rh := Rehydrate(GuardView{Status: StatusSuspended, InitStatus: InitStatusNone})
	if rh.HasRetryableResidual() {
		t.Fatal("nil notices must not report retryable residual")
	}
	if rh.HasSessionOverflow() {
		t.Fatal("nil notices must not report session overflow")
	}
	if !rh.CanActivate() {
		t.Fatal("suspended/none/nil-notices must CanActivate")
	}
}