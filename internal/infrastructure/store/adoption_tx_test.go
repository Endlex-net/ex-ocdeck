// adoption_tx_test.go 落实 tasks 6.4：承接原子转换——命名空间转换/anchor/binding/
// 状态守卫 CAS 单事务；冲突拒绝与回滚零副作用；active→active 守卫。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "ocdeck/internal/domain/agentprovider"
	ocdecktask "ocdeck/internal/domain/task"
)

// seedAdoptableTask 构造承接 A 形态任务：v1 binding + anchor(sess-a)@v1 +
// 归属行（父 + 子），status 可指定。binding envelope 经 Encode 写入（与提交
// 守卫的代际复验同源）。
func seedAdoptableTask(t *testing.T, db *DB, taskID, status string) {
	t.Helper()
	seedNamespaceProjectTask(t, db, taskID, "sess-a")
	seedNamespaceSession(t, db, taskID, "sess-a")
	seedNamespaceSession(t, db, taskID, "sess-a-child") // parent 链子 session
	if _, err := db.Exec(`UPDATE task_sessions SET parent_id = 'sess-a' WHERE task_id = ? AND session_id = 'sess-a-child'`, taskID); err != nil {
		t.Fatalf("set parent: %v", err)
	}
	raw, err := v1AdoptionBinding("sess-a").Encode()
	if err != nil {
		t.Fatalf("encode v1 binding: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE tasks SET status = ?, task_binding = ?, provider_id = ?, anchor_provider_id = ?, anchor_provider_instance_id = ?
		 WHERE id = ?`,
		status, raw, ProviderIDV1, ProviderIDV1, ProviderInstanceV1Default, taskID); err != nil {
		t.Fatalf("seed adoptable task: %v", err)
	}
}

func v1AdoptionBinding(upstream string) domain.TaskBinding {
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ProviderIDV1,
		ProviderInstanceID: ProviderInstanceV1Default,
		UpstreamSessionID:  upstream,
		TerminalRef:        "ocdeck-t1-tui",
		Data:               []byte(`{"port":4000,"directory":"/wt","taskId":"t1","auth":{"kind":"derived"}}`),
	}
}

func v2AdoptionBinding(upstream string) domain.TaskBinding {
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ProviderIDV2,
		ProviderInstanceID: ProviderInstanceV2Default,
		UpstreamSessionID:  upstream,
		TerminalRef:        "ocdeck-v2-tui-t1",
		Data:               []byte(`{"port":4096,"directory":"/wt","taskId":"t1","origin":"adopted","auth":{"kind":"derived"}}`),
	}
}

func TestCommitAdoption_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedAdoptableTask(t, db, "t1", "suspended")
	q := db.Queries

	binding := v2AdoptionBinding("sess-a")
	res, err := q.CommitAdoption(ctx, "t1", binding, v1AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil)
	if err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	if !res.Matched || !res.StatusChanged {
		t.Fatalf("result: %+v", res)
	}
	// 全部归属行（含子 session）已转换、session_id 不变。
	var nV2, nV1 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1' AND provider_id=?`, ProviderIDV2).Scan(&nV2); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1' AND provider_id=?`, ProviderIDV1).Scan(&nV1); err != nil {
		t.Fatal(err)
	}
	if nV2 != 2 || nV1 != 0 {
		t.Fatalf("converted rows: v2=%d v1=%d", nV2, nV1)
	}
	// anchor 三元组 → v2、session_id 不变；binding 已写入。
	got, has, err := q.GetTaskBinding(ctx, "t1")
	if err != nil || !has || got.ProviderID != ProviderIDV2 || got.UpstreamSessionID != "sess-a" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	var anchorPID, anchorSID string
	if err := db.QueryRow(`SELECT anchor_provider_id, anchor_session_id FROM tasks WHERE id='t1'`).Scan(&anchorPID, &anchorSID); err != nil {
		t.Fatal(err)
	}
	if anchorPID != ProviderIDV2 || anchorSID != "sess-a" {
		t.Fatalf("anchor: %s/%s", anchorPID, anchorSID)
	}
}

func TestCommitAdoption_ActiveToActiveGuard(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedAdoptableTask(t, db, "t1", "active")
	q := db.Queries
	// active→active 守卫：状态不变（无同值排除），binding/anchor 原子更新。
	res, err := q.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), v1AdoptionBinding("sess-a"), ocdecktask.StatusActive, nil)
	if err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	if !res.Matched || res.StatusChanged {
		t.Fatalf("guard result: %+v（状态不变）", res)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM tasks WHERE id='t1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("status drifted: %s", status)
	}
}

func TestCommitAdoption_ConflictRejectsZeroSideEffect(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedAdoptableTask(t, db, "t1", "suspended")
	// 他任务以 v2 命名空间持有 sess-a-child → 新键冲突 → 整体拒绝。
	seedNamespaceProjectTask(t, db, "t2", "")
	if _, err := db.Exec(
		`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, provider_id, provider_instance_id)
		 VALUES ('t2', 'sess-a-child', 1, 1, 1, ?, ?)`, ProviderIDV2, ProviderInstanceV2Default); err != nil {
		t.Fatal(err)
	}
	q := db.Queries
	_, err := q.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), v1AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil)
	if !IsAdoptionConflict(err) {
		t.Fatalf("expect adoption conflict, got %v", err)
	}
	// 回滚零副作用：归属行仍 v1、binding/anchor/binding envelope 原值、状态不变。
	var nV2, nV1 int
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1' AND provider_id=?`, ProviderIDV2).Scan(&nV2)
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1' AND provider_id=?`, ProviderIDV1).Scan(&nV1)
	if nV2 != 0 || nV1 != 2 {
		t.Fatalf("rollback leaked rows: v2=%d v1=%d", nV2, nV1)
	}
	var bindingRaw sql.NullString
	var anchorPID sql.NullString
	var status string
	if err := db.QueryRow(`SELECT task_binding, anchor_provider_id, status FROM tasks WHERE id='t1'`).Scan(&bindingRaw, &anchorPID, &status); err != nil {
		t.Fatal(err)
	}
	if !anchorPID.Valid || anchorPID.String != ProviderIDV1 || status != "suspended" {
		t.Fatalf("rollback: anchor=%v status=%s", anchorPID, status)
	}
	if !bindingRaw.Valid || !containsV1Provider(bindingRaw.String) {
		t.Fatalf("rollback: binding must stay v1")
	}
	// 冲突解除后同任务重试成功（回滚未污染事实）。
	if _, err := db.Exec(`DELETE FROM task_sessions WHERE task_id='t2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), v1AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil); err != nil {
		t.Fatalf("retry after conflict resolution: %v", err)
	}
}

func TestCommitAdoption_GuardMismatches(t *testing.T) {
	cases := []struct {
		name  string
		seed  func(t *testing.T, db *DB)
		error func(t *testing.T, err error)
	}{
		{"status guard mismatch", func(t *testing.T, db *DB) {
			seedAdoptableTask(t, db, "t1", "active") // 期望 suspended
		}, func(t *testing.T, err error) {
			if !IsAdoptionConflict(err) {
				t.Fatalf("expect conflict, got %v", err)
			}
		}},
		{"anchor inconsistent with binding upstream", func(t *testing.T, db *DB) {
			seedAdoptableTask(t, db, "t1", "suspended")
			if _, err := db.Exec(`UPDATE tasks SET anchor_session_id='sess-other' WHERE id='t1'`); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, err error) {
			if !IsAdoptionConflict(err) {
				t.Fatalf("expect conflict, got %v", err)
			}
		}},
		{"no prior binding", func(t *testing.T, db *DB) {
			seedAdoptableTask(t, db, "t1", "suspended")
			if _, err := db.Exec(`UPDATE tasks SET task_binding=NULL WHERE id='t1'`); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, err error) {
			if !IsAdoptionConflict(err) {
				t.Fatalf("expect conflict, got %v", err)
			}
		}},
		{"mixed namespace rows", func(t *testing.T, db *DB) {
			seedAdoptableTask(t, db, "t1", "suspended")
			if _, err := db.Exec(
				`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, provider_id, provider_instance_id)
				 VALUES ('t1', 'sess-mixed', 1, 1, 1, ?, ?)`, ProviderIDV2, ProviderInstanceV2Default); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, err error) {
			if !IsAdoptionConflict(err) {
				t.Fatalf("expect conflict, got %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			tc.seed(t, db)
			_, err := db.Queries.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), v1AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil)
			if err == nil {
				t.Fatalf("expected rejection")
			}
			tc.error(t, err)
			// 回滚零副作用（v1 归属行不动；混杂场景的 v2 行为前置种子，不算泄漏）。
			var nV1 int
			_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1' AND provider_id=? AND provider_instance_id=?`, ProviderIDV1, ProviderInstanceV1Default).Scan(&nV1)
			if nV1 != 2 {
				t.Fatalf("rollback must leave v1 rows untouched: %d", nV1)
			}
		})
	}
}

func containsV1Provider(raw string) bool {
	return len(raw) > 0 && stringContains(raw, `"provider_id":"opencode-v1"`)
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ = time.Now

// TestCommitV2Resume_OldBindingGenerationGuard 落实 G3-F8：两个并发恢复持不同
// candidate、同一旧 binding 代际——只有先提交者成功；后提交者不能覆盖新 binding
// （真实 store 路径；守卫为旧 binding 编码精确匹配）。
func TestCommitV2Resume_OldBindingGenerationGuard(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedAdoptableTask(t, db, "t1", "active")
	// 已承接为 v2（代际 base）：candidate A 已提交。
	q := db.Queries
	base := domain.TaskBinding{
		Version: domain.TaskBindingVersion, ProviderID: ProviderIDV2,
		ProviderInstanceID: ProviderInstanceV2Default, UpstreamSessionID: "sess-a",
		TerminalRef: "ocdeck-v2-tui-t1",
		Data:        []byte(`{"port":4096,"directory":"/wt","taskId":"t1","origin":"adopted","auth":{"kind":"derived"}}`),
	}
	if _, err := q.CommitAdoption(ctx, "t1", base, v1AdoptionBinding("sess-a"), ocdecktask.StatusActive, nil); err != nil {
		t.Fatalf("seed base v2 binding: %v", err)
	}

	// 两个恢复方读取同一代际 base，各持不同 candidate（port 区分）。
	candA := base
	candA.Data = []byte(`{"port":4096,"directory":"/wt","taskId":"t1","origin":"created","auth":{"kind":"derived"}}`)
	candB := base
	candB.Data = []byte(`{"port":4097,"directory":"/wt","taskId":"t1","origin":"created","auth":{"kind":"derived"}}`)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var aWon, bWon int32
	for i, cand := range []domain.TaskBinding{candA, candB} {
		wg.Add(1)
		go func(idx int, c domain.TaskBinding) {
			defer wg.Done()
			<-start
			_, err := q.CommitV2Resume(ctx, "t1", base, c, ocdecktask.StatusActive)
			if err == nil {
				if idx == 0 {
					atomic.StoreInt32(&aWon, 1)
				} else {
					atomic.StoreInt32(&bWon, 1)
				}
			}
		}(i, cand)
	}
	close(start)
	wg.Wait()
	// 恰有一方成功。
	if aWon+bWon != 1 {
		t.Fatalf("exactly one committer must win: a=%d b=%d", aWon, bWon)
	}
	// 落库 binding 必为胜者（不存在混合/倒置）。
	got, has, err := q.GetTaskBinding(ctx, "t1")
	if err != nil || !has {
		t.Fatalf("binding: %v %v", has, err)
	}
	var data struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(got.Data, &data); err != nil {
		t.Fatal(err)
	}
	if (aWon == 1 && data.Port != 4096) || (bWon == 1 && data.Port != 4097) {
		t.Fatalf("winner binding corrupted: port=%d a=%d b=%d", data.Port, aWon, bWon)
	}
	// 败者以同 old 代际重试必失败（base 已被覆盖——代际守卫精确拒绝）。
	if _, err := q.CommitV2Resume(ctx, "t1", base, candA, ocdecktask.StatusActive); !IsAdoptionConflict(err) {
		t.Fatalf("stale generation must conflict, got %v", err)
	}
}

// TestCommitV2Resume_ActivatingConvergesToActive 落实 D4 生命周期矩阵：已有 v2
// binding 的激活/恢复提交 = activating→active CAS——from=activating 时 binding
// 替换与 active 收敛同事务（MUST NOT 卡 activating）。
func TestCommitV2Resume_ActivatingConvergesToActive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedAdoptableTask(t, db, "t1", "active")
	q := db.Queries
	base := domain.TaskBinding{
		Version: domain.TaskBindingVersion, ProviderID: ProviderIDV2,
		ProviderInstanceID: ProviderInstanceV2Default, UpstreamSessionID: "sess-a",
		TerminalRef: "ocdeck-v2-tui-t1",
		Data:        []byte(`{"port":4096,"directory":"/wt","taskId":"t1","origin":"adopted","auth":{"kind":"derived"}}`),
	}
	if _, err := q.CommitAdoption(ctx, "t1", base, v1AdoptionBinding("sess-a"), ocdecktask.StatusActive, nil); err != nil {
		t.Fatalf("seed v2 binding: %v", err)
	}
	// 恢复入口 beginActivation 已置 activating；from=activating 提交 → active。
	if _, err := db.Exec(`UPDATE tasks SET status='activating' WHERE id='t1'`); err != nil {
		t.Fatal(err)
	}
	cand := base
	cand.Data = []byte(`{"port":4096,"directory":"/wt","taskId":"t1","origin":"created","auth":{"kind":"derived"}}`)
	res, err := q.CommitV2Resume(ctx, "t1", base, cand, ocdecktask.StatusActivating)
	if err != nil {
		t.Fatalf("CommitV2Resume(activating): %v", err)
	}
	if !res.Matched || !res.StatusChanged || res.To != ocdecktask.StatusActive {
		t.Fatalf("result: %+v（activating→active 收敛）", res)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM tasks WHERE id='t1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("status=%s, want active（不得卡 activating）", status)
	}
}

// seedZeroHistoryTask 构造 B 首建形态任务：零 binding/anchor/归属行，status 可指定。
func seedZeroHistoryTask(t *testing.T, db *DB, taskID, status string) {
	t.Helper()
	seedNamespaceProjectTask(t, db, taskID, "")
	if _, err := db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID); err != nil {
		t.Fatal(err)
	}
}

// TestCommitFirstBuild_Success 落实评审 I5：首建「candidate claim+anchor+binding+
// 状态 CAS」单事务——归属行 v2 命名空间、anchor 三元组、binding envelope、active
// 同事务落库。
func TestCommitFirstBuild_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedZeroHistoryTask(t, db, "t1", "activating")
	q := db.Queries

	binding := v2AdoptionBinding("ses-new-1")
	key := domain.SessionKey{ProviderID: ProviderIDV2, ProviderInstanceID: ProviderInstanceV2Default, UpstreamID: "ses-new-1"}
	res, err := q.CommitFirstBuild(ctx, "t1", binding, key, 100, 100, 100, "")
	if err != nil {
		t.Fatalf("CommitFirstBuild: %v", err)
	}
	if !res.Matched || !res.StatusChanged || res.To != ocdecktask.StatusActive {
		t.Fatalf("result: %+v", res)
	}
	// claim 行 v2 命名空间。
	var nV2 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1' AND provider_id=? AND provider_instance_id=? AND session_id='ses-new-1'`,
		ProviderIDV2, ProviderInstanceV2Default).Scan(&nV2); err != nil {
		t.Fatal(err)
	}
	if nV2 != 1 {
		t.Fatalf("claim rows=%d, want 1", nV2)
	}
	// anchor 三元组 + binding + active。
	var anchorSID, anchorPID string
	if err := db.QueryRow(`SELECT anchor_session_id, anchor_provider_id FROM tasks WHERE id='t1'`).Scan(&anchorSID, &anchorPID); err != nil {
		t.Fatal(err)
	}
	if anchorSID != "ses-new-1" || anchorPID != ProviderIDV2 {
		t.Fatalf("anchor: %s/%s", anchorSID, anchorPID)
	}
	got, has, err := q.GetTaskBinding(ctx, "t1")
	if err != nil || !has || got.UpstreamSessionID != "ses-new-1" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	var status string
	_ = db.QueryRow(`SELECT status FROM tasks WHERE id='t1'`).Scan(&status)
	if status != "active" {
		t.Fatalf("status=%s, want active", status)
	}
}

// TestCommitFirstBuild_Guards 落实首建提交守卫：非 activating / 既有归属行漂入 /
// candidate 被他任务持有 / key 与 binding 不一致 → 拒绝且零写入。
func TestCommitFirstBuild_Guards(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, db *DB)
	}{
		{"non-activating status", func(t *testing.T, db *DB) {
			seedZeroHistoryTask(t, db, "t1", "suspended")
		}},
		{"existing ownership rows drifted in", func(t *testing.T, db *DB) {
			seedZeroHistoryTask(t, db, "t1", "activating")
			seedNamespaceSession(t, db, "t1", "sess-late")
		}},
		{"candidate held by other task", func(t *testing.T, db *DB) {
			seedZeroHistoryTask(t, db, "t1", "activating")
			seedNamespaceProjectTask(t, db, "t2", "")
			if _, err := db.Exec(
				`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, provider_id, provider_instance_id)
				 VALUES ('t2', 'ses-new-1', 1, 1, 1, ?, ?)`, ProviderIDV2, ProviderInstanceV2Default); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			tc.seed(t, db)
			binding := v2AdoptionBinding("ses-new-1")
			key := domain.SessionKey{ProviderID: ProviderIDV2, ProviderInstanceID: ProviderInstanceV2Default, UpstreamID: "ses-new-1"}
			_, err := db.Queries.CommitFirstBuild(ctx, "t1", binding, key, 100, 100, 100, "")
			if !IsAdoptionConflict(err) {
				t.Fatalf("expect adoption conflict, got %v", err)
			}
			// 回滚零写入：无归属行、binding/anchor 仍空、状态保持。
			var nRows int
			_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1'`).Scan(&nRows)
			var bindingRaw, anchorSID sql.NullString
			var status string
			_ = db.QueryRow(`SELECT task_binding, anchor_session_id, status FROM tasks WHERE id='t1'`).Scan(&bindingRaw, &anchorSID, &status)
			if bindingRaw.Valid || anchorSID.Valid {
				t.Fatalf("rollback must not write binding/anchor: %v %v", bindingRaw, anchorSID)
			}
			if tc.name != "existing ownership rows drifted in" && nRows != 0 {
				t.Fatalf("rollback must not leak claim rows: %d", nRows)
			}
		})
	}
	t.Run("key binding mismatch", func(t *testing.T) {
		db := openTestDB(t)
		ctx := context.Background()
		seedZeroHistoryTask(t, db, "t1", "activating")
		binding := v2AdoptionBinding("ses-new-1")
		key := domain.SessionKey{ProviderID: ProviderIDV2, ProviderInstanceID: ProviderInstanceV2Default, UpstreamID: "ses-other"}
		if _, err := db.Queries.CommitFirstBuild(ctx, "t1", binding, key, 100, 100, 100, ""); !IsAdoptionConflict(err) {
			t.Fatalf("expect conflict, got %v", err)
		}
	})
}

// TestCommitAdoption_OldBindingGenerationGuard 落实评审 I11：提交携带同次读取旧
// binding 并在事务内代际复验——判定后 binding 被并发改写 → 拒绝零写入。
func TestCommitAdoption_OldBindingGenerationGuard(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedAdoptableTask(t, db, "t1", "suspended")
	stale := v1AdoptionBinding("sess-a")
	stale.Data = []byte(`{"port":4001,"directory":"/wt","taskId":"t1","auth":{"kind":"derived"}}`)
	_, err := db.Queries.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), stale, ocdecktask.StatusSuspended, nil)
	if !IsAdoptionConflict(err) {
		t.Fatalf("stale old binding must conflict, got %v", err)
	}
	// 零写入复验：状态/binding 原值。
	var status string
	_ = db.QueryRow(`SELECT status FROM tasks WHERE id='t1'`).Scan(&status)
	if status != "suspended" {
		t.Fatalf("status drifted: %s", status)
	}
}

// TestCommitAdoption_AnchorOwnershipMissing 落实评审 I11：目标 anchor 归属存在性
// 事务内复验——归属行被并发删除 → 拒绝零写入（MUST NOT 承接到无归属的 id）。
func TestCommitAdoption_AnchorOwnershipMissing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedAdoptableTask(t, db, "t1", "suspended")
	if _, err := db.Exec(`DELETE FROM task_sessions WHERE task_id='t1' AND session_id='sess-a'`); err != nil {
		t.Fatal(err)
	}
	_, err := db.Queries.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), v1AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil)
	if !IsAdoptionConflict(err) {
		t.Fatalf("missing anchor ownership must conflict, got %v", err)
	}
}

// ---- legacy-A：无 binding 老任务形态（v1-legacy-anchor-adopt）----

// seedLegacyAnchorTask 构造 legacy-A 形态任务：task_binding 为空 + anchor 三元组
// v1 完整（anchor_session_id='sess-a'）+ 归属行（父+子）全 v1——binding 持久化
// （迁移 0018）之前的存量形态。
func seedLegacyAnchorTask(t *testing.T, db *DB, taskID, status string) {
	t.Helper()
	seedNamespaceProjectTask(t, db, taskID, "sess-a")
	seedNamespaceSession(t, db, taskID, "sess-a")
	seedNamespaceSession(t, db, taskID, "sess-a-child") // parent 链子 session
	if _, err := db.Exec(`UPDATE task_sessions SET parent_id = 'sess-a' WHERE task_id = ? AND session_id = 'sess-a-child'`, taskID); err != nil {
		t.Fatalf("set parent: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE tasks SET status = ?, provider_id = ?, anchor_provider_id = ?, anchor_provider_instance_id = ?
		 WHERE id = ?`,
		status, ProviderIDV1, ProviderIDV1, ProviderInstanceV1Default, taskID); err != nil {
		t.Fatalf("seed legacy anchor task: %v", err)
	}
}

// TestCommitAdoptionLegacy_Success 落实 tasks 2.4：NULL binding 代际守卫（task_binding
// IS NULL）下提交成功——归属转换/anchor 翻转/binding 写入与既有 A 同事务语义。
func TestCommitAdoptionLegacy_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedLegacyAnchorTask(t, db, "t1", "suspended")
	q := db.Queries

	res, err := q.CommitAdoptionLegacy(ctx, "t1", v2AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil)
	if err != nil {
		t.Fatalf("CommitAdoptionLegacy: %v", err)
	}
	if !res.Matched || !res.StatusChanged {
		t.Fatalf("result: %+v", res)
	}
	// 全部归属行（含子 session）已转换、session_id 不变。
	var nV2, nV1 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1' AND provider_id=?`, ProviderIDV2).Scan(&nV2); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1' AND provider_id=?`, ProviderIDV1).Scan(&nV1); err != nil {
		t.Fatal(err)
	}
	if nV2 != 2 || nV1 != 0 {
		t.Fatalf("converted rows: v2=%d v1=%d", nV2, nV1)
	}
	// anchor 三元组 → v2、session_id 不变；binding 已写入（NULL → v2 envelope）。
	got, has, err := q.GetTaskBinding(ctx, "t1")
	if err != nil || !has || got.ProviderID != ProviderIDV2 || got.UpstreamSessionID != "sess-a" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	var anchorPID, anchorSID string
	if err := db.QueryRow(`SELECT anchor_provider_id, anchor_session_id FROM tasks WHERE id='t1'`).Scan(&anchorPID, &anchorSID); err != nil {
		t.Fatal(err)
	}
	if anchorPID != ProviderIDV2 || anchorSID != "sess-a" {
		t.Fatalf("anchor: %s/%s", anchorPID, anchorSID)
	}
}

// TestCommitAdoptionLegacy_ActiveGuard 落实 active→active 守卫形态（状态不变，
// binding/anchor 原子更新）。
func TestCommitAdoptionLegacy_ActiveGuard(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedLegacyAnchorTask(t, db, "t1", "active")
	res, err := db.Queries.CommitAdoptionLegacy(ctx, "t1", v2AdoptionBinding("sess-a"), ocdecktask.StatusActive, nil)
	if err != nil {
		t.Fatalf("CommitAdoptionLegacy: %v", err)
	}
	if !res.Matched || res.StatusChanged {
		t.Fatalf("active→active: matched=%v changed=%v", res.Matched, res.StatusChanged)
	}
}

// TestCommitAdoptionLegacy_ConcurrentBindingWriteRejects 落实 delta spec「并发写入
// binding 后承接拒绝」：提交时 task_binding 已被并发路径写入非空值 → CAS 失配
// 拒绝（ErrAdoptionConflict），整体回滚零写入。
func TestCommitAdoptionLegacy_ConcurrentBindingWriteRejects(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedLegacyAnchorTask(t, db, "t1", "suspended")
	// 并发路径写入非空 binding（判定阶段读到 NULL 之后）。
	concurrent, err := v1AdoptionBinding("sess-a").Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET task_binding = ? WHERE id = 't1'`, concurrent); err != nil {
		t.Fatal(err)
	}

	_, cerr := db.Queries.CommitAdoptionLegacy(ctx, "t1", v2AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil)
	if !IsAdoptionConflict(cerr) {
		t.Fatalf("concurrent binding write must conflict, got %v", cerr)
	}
	// 零写入复验：归属行仍 v1、task_binding 保持并发写入值、anchor/status 未漂移。
	var nV2 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t1' AND provider_id=?`, ProviderIDV2).Scan(&nV2); err != nil {
		t.Fatal(err)
	}
	if nV2 != 0 {
		t.Fatalf("ownership must stay v1 after rollback, v2 rows=%d", nV2)
	}
	var anchorPID, status string
	var binding sql.NullString
	if err := db.QueryRow(`SELECT anchor_provider_id, status, task_binding FROM tasks WHERE id='t1'`).Scan(&anchorPID, &status, &binding); err != nil {
		t.Fatal(err)
	}
	if anchorPID != ProviderIDV1 || status != "suspended" || !binding.Valid || binding.String != string(concurrent) {
		t.Fatalf("zero-write violation: anchor=%s status=%s binding=%v", anchorPID, status, binding)
	}
}
