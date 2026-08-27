package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestMigration0009_BackfillOnlyNullRows(t *testing.T) {
	db := openTestDBRaw(t)
	ctx := context.Background()
	applyMigrationsUpto(t, db, 8)

	if _, err := db.Exec(
		`INSERT INTO projects (id, name, path, default_branch, created_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
		"p1", "proj", "/tmp/repo", "main", time.Now().Unix(), "repo"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	now := time.Now().Unix()
	for _, id := range []string{"t-null", "t-preset", "t-none"} {
		if _, err := db.Exec(
			`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, "p1", "task", "b", "suspended", "/tmp/"+id, now, now); err != nil {
			t.Fatalf("insert task %s: %v", id, err)
		}
	}

	insertSession := func(taskID, sessionID string, lastSeen, created int64, parent string) {
		t.Helper()
		if _, err := db.Exec(
			`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			taskID, sessionID, created, created, lastSeen, parent); err != nil {
			t.Fatalf("insert session %s/%s: %v", taskID, sessionID, err)
		}
	}
	// 多顶层 session + last_seen 更晚的子 session；回填必须取最近顶层（同 ListTopLevelTaskSessions）。
	insertSession("t-null", "sess-old", 10, 1, "")
	insertSession("t-null", "sess-top", 20, 2, "")
	insertSession("t-null", "sess-child", 99, 3, "sess-top")
	insertSession("t-null", "sess-tie-a", 20, 5, "") // last_seen 同 20，created 更新 → 应胜出
	insertSession("t-preset", "sess-owned", 1, 1, "")

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate 0009: %v", err)
	}

	assertAnchor := func(taskID, want string, wantValid bool) {
		t.Helper()
		task, err := db.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("get %s: %v", taskID, err)
		}
		if task.AnchorSessionID.Valid != wantValid || task.AnchorSessionID.String != want {
			t.Errorf("%s anchor = %+v, want valid=%v %q", taskID, task.AnchorSessionID, wantValid, want)
		}
	}
	assertAnchor("t-null", "sess-tie-a", true)
	assertAnchor("t-preset", "sess-owned", true)
	assertAnchor("t-none", "", false)

	// 仅处理 NULL 行：已有锚定不得被回填语句覆盖。
	if _, err := db.Exec(`UPDATE tasks SET anchor_session_id = ? WHERE id = ?`, "sess-preset", "t-preset"); err != nil {
		t.Fatalf("preset anchor: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET anchor_session_id = NULL WHERE id = ?`, "t-null"); err != nil {
		t.Fatalf("clear t-null: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE tasks
		   SET anchor_session_id = (
		       SELECT session_id FROM task_sessions
		        WHERE task_sessions.task_id = tasks.id
		          AND (parent_id IS NULL OR parent_id = '')
		        ORDER BY last_seen_at DESC, session_created_at DESC, session_id DESC
		        LIMIT 1
		   )
		 WHERE anchor_session_id IS NULL`); err != nil {
		t.Fatalf("re-run backfill: %v", err)
	}
	assertAnchor("t-preset", "sess-preset", true)
	assertAnchor("t-null", "sess-tie-a", true)
	assertAnchor("t-none", "", false)
}

func TestMigration0009_NoSessionsStayNull(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	task, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.AnchorSessionID.Valid {
		t.Errorf("new task with no sessions: anchor=%v, want NULL", task.AnchorSessionID)
	}
}

func TestClaimTaskSessionAndSetAnchor_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")

	res, err := db.ClaimTaskSessionAndSetAnchor(ctx, "t1", "sess-new", 1, 1, 1, "")
	if err != nil {
		t.Fatalf("claim+anchor: %v", err)
	}
	if !res.Claimed || !res.Changed {
		t.Fatalf("claimed=%v changed=%v, want true/true", res.Claimed, res.Changed)
	}
	task, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !task.AnchorSessionID.Valid || task.AnchorSessionID.String != "sess-new" {
		t.Errorf("anchor = %+v, want sess-new", task.AnchorSessionID)
	}
	sessions, _ := db.ListTaskSessions(ctx, "t1")
	if len(sessions) != 1 || sessions[0].SessionID != "sess-new" {
		t.Errorf("sessions = %+v, want [sess-new]", sessions)
	}
}

func TestClaimTaskSessionAndSetAnchor_ConflictLeavesBothUnchanged(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2"} {
		if err := db.CreateTask(ctx, TaskRow{ID: id, ProjectID: "p1", Name: "task", Branch: "b",
			Status: "suspended", WorktreePath: "/wt-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := db.ClaimTaskSessionAndSetAnchor(ctx, "t1", "sess-shared", 1, 1, 1, ""); err != nil || !res.Claimed {
		t.Fatalf("t1 claim: %v %v", res.Claimed, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET anchor_session_id = ? WHERE id = ?`, "sess-t2-old", "t2"); err != nil {
		t.Fatalf("preset t2 anchor: %v", err)
	}

	res, err := db.ClaimTaskSessionAndSetAnchor(ctx, "t2", "sess-shared", 2, 2, 2, "")
	if err != nil {
		t.Fatalf("t2 claim: %v", err)
	}
	if res.Claimed {
		t.Fatal("t2 claim should conflict")
	}
	if res.OwnerTaskID != "t1" {
		t.Errorf("owner = %q, want t1", res.OwnerTaskID)
	}

	t2, err := db.GetTask(ctx, "t2")
	if err != nil {
		t.Fatalf("get t2: %v", err)
	}
	if !t2.AnchorSessionID.Valid || t2.AnchorSessionID.String != "sess-t2-old" {
		t.Errorf("t2 anchor = %+v, want sess-t2-old (unchanged)", t2.AnchorSessionID)
	}
	t2Sessions, _ := db.ListTaskSessions(ctx, "t2")
	for _, s := range t2Sessions {
		if s.SessionID == "sess-shared" {
			t.Error("t2 must not own sess-shared")
		}
	}
	t1, _ := db.GetTask(ctx, "t1")
	if !t1.AnchorSessionID.Valid || t1.AnchorSessionID.String != "sess-shared" {
		t.Errorf("t1 anchor = %+v, want sess-shared", t1.AnchorSessionID)
	}
}

func TestClearTaskAnchorConditional_CAS(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	if _, err := db.ClaimTaskSessionAndSetAnchor(ctx, "t1", "sess-a", 1, 1, 1, ""); err != nil {
		t.Fatal(err)
	}

	res, err := db.ClearTaskAnchorConditional(ctx, "t1", "sess-a")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !res.Matched || !res.Changed {
		t.Fatalf("matched=%v changed=%v, want true/true", res.Matched, res.Changed)
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.AnchorSessionID.Valid {
		t.Errorf("after matched clear: %+v, want NULL", task.AnchorSessionID)
	}

	if _, err := db.ExecContext(ctx, `UPDATE tasks SET anchor_session_id = ? WHERE id = ?`, "sess-new", "t1"); err != nil {
		t.Fatal(err)
	}
	miss, err := db.ClearTaskAnchorConditional(ctx, "t1", "sess-a")
	if err != nil {
		t.Fatalf("mismatch clear: %v", err)
	}
	if miss.Matched {
		t.Fatal("CAS mismatch should return !Matched")
	}
	task, _ = db.GetTask(ctx, "t1")
	if !task.AnchorSessionID.Valid || task.AnchorSessionID.String != "sess-new" {
		t.Errorf("mismatch must not overwrite: %+v", task.AnchorSessionID)
	}
}

func TestAcquireRecoveryPermit_OrdinalAndWindow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	if err := db.CreateTask(ctx, TaskRow{ID: "t2", ProjectID: "p1", Name: "task2", Branch: "b",
		Status: "suspended", WorktreePath: "/wt2"}); err != nil {
		t.Fatal(err)
	}

	now := int64(1_000_000)
	for i, want := range []int{1, 2, 3} {
		res, err := db.AcquireRecoveryPermit(ctx, "t1", now+int64(i))
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if !res.Acquired || res.Ordinal != want {
			t.Fatalf("acquire %d: %+v, want acquired ordinal=%d", i, res, want)
		}
	}
	full, err := db.AcquireRecoveryPermit(ctx, "t1", now+3)
	if err != nil {
		t.Fatalf("window full: %v", err)
	}
	if full.Acquired || full.Ordinal != 0 {
		t.Errorf("window full: %+v, want Acquired=false Ordinal=0", full)
	}

	other, err := db.AcquireRecoveryPermit(ctx, "t2", now)
	if err != nil || !other.Acquired || other.Ordinal != 1 {
		t.Fatalf("t2 independent window: %+v err=%v", other, err)
	}

	n, err := db.CountRecoveryAttemptsInWindow(ctx, "t1", now+3)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}

	aged, err := db.AcquireRecoveryPermit(ctx, "t1", now+recoveryPermitWindowSec+10)
	if err != nil {
		t.Fatalf("aged acquire: %v", err)
	}
	if !aged.Acquired || aged.Ordinal != 1 {
		t.Errorf("after window age: %+v, want acquired ordinal=1", aged)
	}

	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_recovery_attempts WHERE task_id = 't1'`).Scan(&remaining); err != nil {
		t.Fatalf("remaining: %v", err)
	}
	if remaining != 1 {
		t.Errorf("expired rows should be pruned; remaining=%d want 1", remaining)
	}
}

func TestPruneExpiredRecoveryAttempts_AllTasks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	now := int64(2_000_000)
	if _, err := db.AcquireRecoveryPermit(ctx, "t1", now-100); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRecoveryPermit(ctx, "t1", now); err != nil {
		t.Fatal(err)
	}
	n, err := db.PruneExpiredRecoveryAttempts(ctx, "", now-50)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}
}

func TestAcquireRecoveryPermit_ConcurrentSameTask(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	now := int64(3_000_000)
	const n = 8
	type result struct {
		res AcquirePermitResult
		err error
	}
	ch := make(chan result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			r, err := db.AcquireRecoveryPermit(ctx, "t1", now)
			ch <- result{r, err}
		}()
	}
	wg.Wait()
	close(ch)
	ordinals := map[int]int{}
	acquired := 0
	for r := range ch {
		if r.err != nil {
			t.Fatalf("concurrent acquire: %v", r.err)
		}
		if r.res.Acquired {
			acquired++
			ordinals[r.res.Ordinal]++
		}
	}
	if acquired != 3 {
		t.Fatalf("acquired = %d, want 3", acquired)
	}
	if len(ordinals) != 3 || ordinals[1] != 1 || ordinals[2] != 1 || ordinals[3] != 1 {
		t.Errorf("ordinals = %v, want exactly {1,2,3} once each", ordinals)
	}
}

func TestMigration0009_AbortTriggerRollsBackColumnAndVersion(t *testing.T) {
	db := openTestDBRaw(t)
	ctx := context.Background()
	applyMigrationsUpto(t, db, 8)
	if _, err := db.Exec(
		`INSERT INTO projects (id, name, path, default_branch, created_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
		"p1", "proj", "/tmp/repo", "main", time.Now().Unix(), "repo"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"t1", "p1", "task", "b", "suspended", "/tmp/wt", now, now); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TRIGGER abort_anchor_backfill
		BEFORE UPDATE OF anchor_session_id ON tasks
		BEGIN
			SELECT RAISE(ABORT, 'forced 0009 abort');
		END`); err != nil {
		t.Fatalf("create abort trigger: %v", err)
	}

	if err := db.Migrate(ctx); err == nil {
		t.Fatal("Migrate should fail when 0009 backfill is aborted")
	}

	var ver int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 9`).Scan(&ver); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if ver != 0 {
		t.Errorf("schema version 9 recorded after abort, want rolled back")
	}
	var colCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'anchor_session_id'`).Scan(&colCount); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	if colCount != 0 {
		t.Errorf("anchor_session_id column survived abort, want rolled back")
	}
}

func TestCompleteRecoveryFailure_CASMatchAndMismatch(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	le := "recovery budget exhausted"
	snap := `{"vars":{"OCDECK_SERVE_PORT":"50001"}}`
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, last_error = ?, env_snapshot = ? WHERE id = ?`,
		"activating", "prior", snap, "t1"); err != nil {
		t.Fatal(err)
	}

	res, err := db.CompleteRecoveryFailure(ctx, "t1", &le)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !res.Matched || !res.Changed || !res.StatusChanged {
		t.Fatalf("matched=%v changed=%v statusChanged=%v, want true/true/true", res.Matched, res.Changed, res.StatusChanged)
	}
	if res.From != "activating" || res.To != "suspended" {
		t.Fatalf("from=%s to=%s, want activating→suspended", res.From, res.To)
	}
	task, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != "suspended" {
		t.Errorf("status=%s want suspended", task.Status)
	}
	if !task.LastError.Valid || task.LastError.String != le {
		t.Errorf("last_error=%v want %q", task.LastError, le)
	}
	if task.EnvSnapshot.Valid {
		t.Errorf("env_snapshot=%v want NULL", task.EnvSnapshot)
	}

	if _, err := db.ExecContext(ctx, `UPDATE tasks SET status = ?, last_error = ?, env_snapshot = ? WHERE id = ?`,
		"active", "keep-me", snap, "t1"); err != nil {
		t.Fatal(err)
	}
	miss, err := db.CompleteRecoveryFailure(ctx, "t1", &le)
	if err != nil {
		t.Fatalf("mismatch: %v", err)
	}
	if miss.Matched {
		t.Fatal("CAS mismatch should return !Matched")
	}
	task, _ = db.GetTask(ctx, "t1")
	if task.Status != "active" {
		t.Errorf("mismatch status=%s want active (unchanged)", task.Status)
	}
	if !task.LastError.Valid || task.LastError.String != "keep-me" {
		t.Errorf("mismatch last_error=%v want keep-me", task.LastError)
	}
	if !task.EnvSnapshot.Valid || task.EnvSnapshot.String != snap {
		t.Errorf("mismatch env_snapshot=%v want preserved", task.EnvSnapshot)
	}
}

func TestCompleteRecoveryFailure_AbortRollsBackAllThreeFields(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	snap := `{"vars":{"OCDECK_SERVE_PORT":"1"}}`
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, last_error = ?, env_snapshot = ? WHERE id = ?`,
		"activating", "prior", snap, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TRIGGER abort_recovery_complete
		BEFORE UPDATE OF env_snapshot ON tasks
		BEGIN
			SELECT RAISE(ABORT, 'forced complete abort');
		END`); err != nil {
		t.Fatalf("create abort trigger: %v", err)
	}
	le := "cause"
	if _, err := db.CompleteRecoveryFailure(ctx, "t1", &le); err == nil {
		t.Fatal("complete should fail when env_snapshot UPDATE aborts")
	}
	task, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != "activating" {
		t.Errorf("status=%s want activating (rolled back)", task.Status)
	}
	if !task.LastError.Valid || task.LastError.String != "prior" {
		t.Errorf("last_error=%v want prior", task.LastError)
	}
	if !task.EnvSnapshot.Valid || task.EnvSnapshot.String != snap {
		t.Errorf("env_snapshot=%v want preserved", task.EnvSnapshot)
	}
}

func TestClaimTaskSessionAndSetAnchor_AnchorUpdateAbortRollsBackClaim(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	if _, err := db.Exec(`
		CREATE TRIGGER abort_anchor_update
		BEFORE UPDATE OF anchor_session_id ON tasks
		BEGIN
			SELECT RAISE(ABORT, 'forced anchor abort');
		END`); err != nil {
		t.Fatalf("create abort trigger: %v", err)
	}
	_, err := db.ClaimTaskSessionAndSetAnchor(ctx, "t1", "sess-new", 1, 1, 1, "")
	if err == nil {
		t.Fatal("claim+anchor should fail when anchor UPDATE aborts")
	}
	sessions, serr := db.ListTaskSessions(ctx, "t1")
	if serr != nil {
		t.Fatalf("list: %v", serr)
	}
	if len(sessions) != 0 {
		t.Errorf("claim row survived abort: %+v", sessions)
	}
}
