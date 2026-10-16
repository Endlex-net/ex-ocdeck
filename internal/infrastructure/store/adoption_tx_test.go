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
// 归属行（父 + 子），status 可指定。
func seedAdoptableTask(t *testing.T, db *DB, taskID, status string) {
	t.Helper()
	seedNamespaceProjectTask(t, db, taskID, "sess-a")
	seedNamespaceSession(t, db, taskID, "sess-a")
	seedNamespaceSession(t, db, taskID, "sess-a-child") // parent 链子 session
	if _, err := db.Exec(`UPDATE task_sessions SET parent_id = 'sess-a' WHERE task_id = ? AND session_id = 'sess-a-child'`, taskID); err != nil {
		t.Fatalf("set parent: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE tasks SET status = ?, task_binding = ?, provider_id = ?, anchor_provider_id = ?, anchor_provider_instance_id = ?
		 WHERE id = ?`,
		status, `{"version":1,"provider_id":"opencode-v1","provider_instance_id":"default","upstream_session_id":"sess-a","terminal_ref":"ocdeck-t1-tui","data":{"port":4000,"directory":"/wt","taskId":"`+taskID+`","auth":{"kind":"derived"}}}`,
		ProviderIDV1, ProviderIDV1, ProviderInstanceV1Default, taskID); err != nil {
		t.Fatalf("seed adoptable task: %v", err)
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
	res, err := q.CommitAdoption(ctx, "t1", binding, ocdecktask.StatusSuspended, nil)
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
	res, err := q.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), ocdecktask.StatusActive, nil)
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
	_, err := q.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil)
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
	if _, err := q.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil); err != nil {
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
			_, err := db.Queries.CommitAdoption(ctx, "t1", v2AdoptionBinding("sess-a"), ocdecktask.StatusSuspended, nil)
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
	if _, err := q.CommitAdoption(ctx, "t1", base, ocdecktask.StatusActive, nil); err != nil {
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
