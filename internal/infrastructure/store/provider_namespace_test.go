package store

// provider_namespace_test.go 验证 opencode-v1-spi-provider 阶段 1（tasks 2.1/2.2）：
//   - migration 0018 列形状与 DEFAULT（新库路径 + 0017 旧库升级路径）；
//   - 主键不变（旧二进制 ON CONFLICT(task_id, session_id) upsert 兼容）；
//   - 命名空间防御性补齐：NULL/空串行收敛、合法值保留、无锚定行不动、幂等、单事务；
//   - 回滚再升级 fixture：升级 → 旧二进制写入（省略新列 INSERT / 显式空值）→ 再升级
//     （补齐重跑）→ 全部行命名空间正确、归属判定与现状一致。

import (
	"context"
	"strings"
	"testing"
	"time"
)

func seedNamespaceProjectTask(t *testing.T, db *DB, taskID string, anchor string) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES ('p1', 'proj', '/tmp/repo-ns', 'main', ?, 'repo')`, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if anchor == "" {
		if _, err := db.Exec(
			`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, created_at, updated_at)
			 VALUES (?, 'p1', 'task', 'b', 'suspended', ?, ?, ?)`, taskID, "/tmp/w/"+taskID, now, now); err != nil {
			t.Fatalf("insert task %s: %v", taskID, err)
		}
		return
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, anchor_session_id, created_at, updated_at)
		 VALUES (?, 'p1', 'task', 'b', 'suspended', ?, ?, ?, ?)`, taskID, "/tmp/w/"+taskID, anchor, now, now); err != nil {
		t.Fatalf("insert task %s: %v", taskID, err)
	}
}

func seedNamespaceSession(t *testing.T, db *DB, taskID, sessionID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at)
		 VALUES (?, ?, 1, 1, 1)`, taskID, sessionID); err != nil {
		t.Fatalf("insert session %s/%s: %v", taskID, sessionID, err)
	}
}

// nsRow 读取单行命名空间列（断言 helper）。
func nsTaskRow(t *testing.T, db *DB, taskID string) (providerID, taskBinding, anchorPID, anchorIID, anchorSID interface{}) {
	t.Helper()
	err := db.QueryRow(
		`SELECT provider_id, task_binding, anchor_provider_id, anchor_provider_instance_id, anchor_session_id
		 FROM tasks WHERE id = ?`, taskID).
		Scan(&providerID, &taskBinding, &anchorPID, &anchorIID, &anchorSID)
	if err != nil {
		t.Fatalf("select task %s: %v", taskID, err)
	}
	return
}

func nsSessionRow(t *testing.T, db *DB, taskID, sessionID string) (providerID, providerIID interface{}) {
	t.Helper()
	err := db.QueryRow(
		`SELECT provider_id, provider_instance_id FROM task_sessions WHERE task_id = ? AND session_id = ?`,
		taskID, sessionID).Scan(&providerID, &providerIID)
	if err != nil {
		t.Fatalf("select session %s/%s: %v", taskID, sessionID, err)
	}
	return
}

// TestMigration0018_NewDBColumnsAndDefaults 新库路径：0001-0018 全量应用后，
// 省略新列的 INSERT（旧二进制回滚窗口形态）经 DEFAULT 获得正确命名空间；
// task_binding 为 NULL；task_sessions 主键不变（ON CONFLICT(task_id, session_id)
// upsert 兼容）。
func TestMigration0018_NewDBColumnsAndDefaults(t *testing.T) {
	db := openTestDB(t) // Open 内应用全部 migration
	ctx := context.Background()
	seedNamespaceProjectTask(t, db, "t1", "sess-a")
	seedNamespaceSession(t, db, "t1", "sess-a")

	providerID, taskBinding, anchorPID, anchorIID, anchorSID := nsTaskRow(t, db, "t1")
	if providerID != ProviderIDV1 {
		t.Errorf("tasks.provider_id = %v, want default %q", providerID, ProviderIDV1)
	}
	if taskBinding != nil {
		t.Errorf("tasks.task_binding = %v, want NULL", taskBinding)
	}
	if anchorSID != "sess-a" && anchorPID != ProviderIDV1 && anchorIID != ProviderInstanceV1Default {
		t.Errorf("anchor cols = (%v,%v,%v), want namespace defaults with anchor %q", anchorPID, anchorIID, anchorSID, "sess-a")
	}
	// 显式核对 anchor 命名空间默认值（上一条的 && 组合断言易漏）。
	if anchorPID != ProviderIDV1 || anchorIID != ProviderInstanceV1Default {
		t.Errorf("anchor namespace = (%v,%v), want (%q,%q)", anchorPID, anchorIID, ProviderIDV1, ProviderInstanceV1Default)
	}

	sPID, sIID := nsSessionRow(t, db, "t1", "sess-a")
	if sPID != ProviderIDV1 || sIID != ProviderInstanceV1Default {
		t.Errorf("task_sessions namespace = (%v,%v), want (%q,%q)", sPID, sIID, ProviderIDV1, ProviderInstanceV1Default)
	}

	// 主键不变：ON CONFLICT(task_id, session_id) upsert 仍可用（旧二进制写入形态）。
	if _, err := db.Exec(
		`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at)
		 VALUES ('t1', 'sess-a', 2, 2, 2)
		 ON CONFLICT(task_id, session_id) DO UPDATE SET last_seen_at = 2`); err != nil {
		t.Fatalf("ON CONFLICT(task_id, session_id) upsert: %v", err)
	}
	var lastSeen int64
	if err := db.QueryRow(`SELECT last_seen_at FROM task_sessions WHERE task_id='t1' AND session_id='sess-a'`).Scan(&lastSeen); err != nil || lastSeen != 2 {
		t.Errorf("upsert result last_seen = %d err=%v, want 2", lastSeen, err)
	}

	// 新库路径下补齐幂等（全为默认值行，无行受影响）。
	if err := db.BackfillProviderNamespaces(ctx); err != nil {
		t.Fatalf("backfill on fresh db: %v", err)
	}
}

// TestMigration0018_UpgradeFrom0017 旧库升级路径：0017 基线上种子行 → Migrate 到
// 0018 → 存量行命名空间正确（ALTER TABLE DEFAULT 语义）；既有归属判定查询
// （OwnerOf 形状的 session_id 反查）不受影响。
func TestMigration0018_UpgradeFrom0017(t *testing.T) {
	db := openTestDBRaw(t)
	ctx := context.Background()
	applyMigrationsUpto(t, db, 17)

	seedNamespaceProjectTask(t, db, "t-old", "sess-old")
	seedNamespaceSession(t, db, "t-old", "sess-old")

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate 0018: %v", err)
	}

	providerID, _, anchorPID, anchorIID, _ := nsTaskRow(t, db, "t-old")
	if providerID != ProviderIDV1 {
		t.Errorf("upgraded tasks.provider_id = %v, want %q", providerID, ProviderIDV1)
	}
	if anchorPID != ProviderIDV1 || anchorIID != ProviderInstanceV1Default {
		t.Errorf("upgraded anchor namespace = (%v,%v)", anchorPID, anchorIID)
	}
	sPID, sIID := nsSessionRow(t, db, "t-old", "sess-old")
	if sPID != ProviderIDV1 || sIID != ProviderInstanceV1Default {
		t.Errorf("upgraded task_sessions namespace = (%v,%v)", sPID, sIID)
	}
	// 既有归属反查不受影响（claim 冲突查询按 session_id 反查 owner 的现状形状）。
	var owner string
	err := db.QueryRow(`SELECT task_id FROM task_sessions WHERE session_id = 'sess-old' AND task_id != 't-other'`).Scan(&owner)
	if err != nil || owner != "t-old" {
		t.Errorf("owner reverse lookup = %q err=%v, want t-old", owner, err)
	}
}

// TestBackfillProviderNamespaces 补齐语义：显式 NULL/空串行（旧二进制边界写法）
// 收敛；合法值以**不同于默认值的哨兵**表达（custom-provider/custom-inst——
// 无条件覆盖实现无法通过，B1）；anchor 与 task_sessions 分别覆盖「第一列缺失、
// 第二列保留」及反向组合；首次补齐与重跑后均断言哨兵不变、缺失列已修复；
// 无锚定行 anchor 列不动；幂等。
func TestBackfillProviderNamespaces(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// 哨兵常量：非空且 ≠ 补齐默认值（opencode-v1/default），使「保留合法值」可证伪。
	const sentPID, sentIID = "custom-provider", "custom-inst"
	seedNamespaceProjectTask(t, db, "t-null", "sess-n")  // 将显式置 NULL（anchor 列）
	seedNamespaceProjectTask(t, db, "t-empty", "sess-e") // 将显式置空串
	// 组合哨兵：t-sentA = 第一列缺失/第二列保留；t-sentB = 反向。
	seedNamespaceProjectTask(t, db, "t-sentA", "sess-a")
	seedNamespaceProjectTask(t, db, "t-sentB", "sess-b")
	seedNamespaceProjectTask(t, db, "t-noanchor", "") // 无锚定
	seedNamespaceSession(t, db, "t-null", "s1")
	seedNamespaceSession(t, db, "t-empty", "s2")
	seedNamespaceSession(t, db, "t-sentA", "s3")
	seedNamespaceSession(t, db, "t-sentB", "s4")

	// 显式写坏值（旧二进制回滚窗口边界）：
	// NOT NULL 列（tasks/task_sessions 的 provider 命名空间列）仅可能空串——
	// 显式 NULL 被 schema 拒绝（此处一并断言该约束）；nullable 的 anchor 命名空间
	// 列 NULL 与空串两种边界都覆盖。
	if _, err := db.Exec(`UPDATE tasks SET provider_id = NULL WHERE id = 't-null'`); err == nil {
		t.Fatal("tasks.provider_id 显式 NULL MUST 被 NOT NULL 约束拒绝")
	}
	if _, err := db.Exec(`UPDATE tasks SET provider_id = '', anchor_provider_id = NULL, anchor_provider_instance_id = NULL WHERE id = 't-null'`); err != nil {
		t.Fatalf("break t-null: %v", err)
	}
	if _, err := db.Exec(`UPDATE task_sessions SET provider_id = NULL WHERE task_id = 't-null'`); err == nil {
		t.Fatal("task_sessions.provider_id 显式 NULL MUST 被 NOT NULL 约束拒绝")
	}
	if _, err := db.Exec(`UPDATE task_sessions SET provider_id = '', provider_instance_id = '' WHERE task_id = 't-null'`); err != nil {
		t.Fatalf("break sessions t-null: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET provider_id = '', anchor_provider_id = '', anchor_provider_instance_id = '' WHERE id = 't-empty'`); err != nil {
		t.Fatalf("empty out t-empty: %v", err)
	}
	if _, err := db.Exec(`UPDATE task_sessions SET provider_id = '', provider_instance_id = '' WHERE task_id = 't-empty'`); err != nil {
		t.Fatalf("empty out sessions t-empty: %v", err)
	}
	// 哨兵组合 t-sentA：tasks.provider_id 保留哨兵；anchor 第一列空、第二列哨兵；
	// session 第一列空、第二列哨兵。
	if _, err := db.Exec(`UPDATE tasks SET provider_id = ?, anchor_provider_id = '', anchor_provider_instance_id = ? WHERE id = 't-sentA'`, sentPID, sentIID); err != nil {
		t.Fatalf("sentinel t-sentA: %v", err)
	}
	if _, err := db.Exec(`UPDATE task_sessions SET provider_id = '', provider_instance_id = ? WHERE task_id = 't-sentA'`, sentIID); err != nil {
		t.Fatalf("sentinel sessions t-sentA: %v", err)
	}
	// 哨兵组合 t-sentB：anchor 第一列哨兵、第二列空；session 同向。
	if _, err := db.Exec(`UPDATE tasks SET anchor_provider_id = ?, anchor_provider_instance_id = '' WHERE id = 't-sentB'`, sentPID); err != nil {
		t.Fatalf("sentinel t-sentB: %v", err)
	}
	if _, err := db.Exec(`UPDATE task_sessions SET provider_id = ?, provider_instance_id = '' WHERE task_id = 't-sentB'`, sentPID); err != nil {
		t.Fatalf("sentinel sessions t-sentB: %v", err)
	}
	// 无锚定行显式写空 anchor 命名空间（补齐 MUST NOT 触达无锚定行）。
	if _, err := db.Exec(`UPDATE tasks SET anchor_provider_id = '', anchor_provider_instance_id = '' WHERE id = 't-noanchor'`); err != nil {
		t.Fatalf("empty anchor on t-noanchor: %v", err)
	}

	if err := db.BackfillProviderNamespaces(ctx); err != nil {
		t.Fatalf("BackfillProviderNamespaces: %v", err)
	}

	// assertSentinels 断言哨兵行：首次补齐与幂等重跑共用（B1：两轮均须成立）。
	assertSentinels := func() {
		t.Helper()
		// t-sentA：第一列被修复为默认、第二列哨兵保留；tasks.provider_id 哨兵保留。
		if pid, _, apid, aiid, _ := nsTaskRow(t, db, "t-sentA"); pid != sentPID || apid != ProviderIDV1 || aiid != sentIID {
			t.Errorf("t-sentA after backfill = (pid=%v, anchor=%v,%v), want (sentinel, default, sentinel)", pid, apid, aiid)
		}
		if spid, siid := nsSessionRow(t, db, "t-sentA", "s3"); spid != ProviderIDV1 || siid != sentIID {
			t.Errorf("t-sentA session = (%v,%v), want (default, sentinel)", spid, siid)
		}
		// t-sentB：反向组合。
		if _, _, apid, aiid, _ := nsTaskRow(t, db, "t-sentB"); apid != sentPID || aiid != ProviderInstanceV1Default {
			t.Errorf("t-sentB anchor = (%v,%v), want (sentinel, default)", apid, aiid)
		}
		if spid, siid := nsSessionRow(t, db, "t-sentB", "s4"); spid != sentPID || siid != ProviderInstanceV1Default {
			t.Errorf("t-sentB session = (%v,%v), want (sentinel, default)", spid, siid)
		}
	}
	assertSentinels()

	// NULL 行收敛。
	if pid, _, apid, aiid, _ := nsTaskRow(t, db, "t-null"); pid != ProviderIDV1 || apid != ProviderIDV1 || aiid != ProviderInstanceV1Default {
		t.Errorf("t-null after backfill = (%v,%v,%v), want all defaults", pid, apid, aiid)
	}
	if spid, siid := nsSessionRow(t, db, "t-null", "s1"); spid != ProviderIDV1 || siid != ProviderInstanceV1Default {
		t.Errorf("t-null session after backfill = (%v,%v)", spid, siid)
	}
	// 空串行收敛。
	if pid, _, apid, aiid, _ := nsTaskRow(t, db, "t-empty"); pid != ProviderIDV1 || apid != ProviderIDV1 || aiid != ProviderInstanceV1Default {
		t.Errorf("t-empty after backfill = (%v,%v,%v), want all defaults", pid, apid, aiid)
	}
	if spid, siid := nsSessionRow(t, db, "t-empty", "s2"); spid != ProviderIDV1 || siid != ProviderInstanceV1Default {
		t.Errorf("t-empty session after backfill = (%v,%v)", spid, siid)
	}
	// 无锚定行：anchor 列保持空串（未被补齐触碰——仅非空锚定行在范围内）。
	if _, _, apid, aiid, anchorSID := nsTaskRow(t, db, "t-noanchor"); apid != "" || aiid != "" || anchorSID != nil {
		t.Errorf("t-noanchor anchor cols = (%v,%v,anchor=%v), want untouched empty", apid, aiid, anchorSID)
	}

	// 幂等：重跑后哨兵与收敛状态均不变（B1：重跑同样不得覆盖合法值）。
	if err := db.BackfillProviderNamespaces(ctx); err != nil {
		t.Fatalf("backfill rerun: %v", err)
	}
	assertSentinels()
	if spid, siid := nsSessionRow(t, db, "t-empty", "s2"); spid != ProviderIDV1 || siid != ProviderInstanceV1Default {
		t.Errorf("t-empty session after rerun = (%v,%v)", spid, siid)
	}
}

// TestBackfillProviderNamespaces_SingleTransactionRollback（B2）单事务性失败路径：
// 为最后的 task_sessions UPDATE 制造失败（BEFORE UPDATE 触发器 RAISE(ABORT)），
// 预置需修复的 tasks/anchor 行 → 补齐返回错误**且前序 UPDATE 已回滚**（tasks/
// anchor 保持补齐前坏值）；移除失败源后重跑成功收敛。
func TestBackfillProviderNamespaces_SingleTransactionRollback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedNamespaceProjectTask(t, db, "t-rb", "sess-rb")
	seedNamespaceSession(t, db, "t-rb", "s-rb")
	// 预置三类待修复行（tasks.provider_id / anchor 两列 / session 两列全部空串）。
	if _, err := db.Exec(`UPDATE tasks SET provider_id = '', anchor_provider_id = '', anchor_provider_instance_id = ''
		 WHERE id = 't-rb'`); err != nil {
		t.Fatalf("break t-rb: %v", err)
	}
	if _, err := db.Exec(`UPDATE task_sessions SET provider_id = '', provider_instance_id = '' WHERE task_id = 't-rb'`); err != nil {
		t.Fatalf("break session t-rb: %v", err)
	}
	// 失败注入：task_sessions 上的 UPDATE 一律 ABORT（补齐的最后一组语句）。
	if _, err := db.Exec(`CREATE TRIGGER ns_fail_sessions BEFORE UPDATE ON task_sessions
		BEGIN SELECT RAISE(ABORT, 'ns fail injected'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	err := db.BackfillProviderNamespaces(ctx)
	if err == nil || !strings.Contains(err.Error(), "ns fail injected") {
		t.Fatalf("backfill error = %v, want injected trigger failure", err)
	}
	// 前序 UPDATE 已回滚：tasks/anchor 保持补齐前坏值（未部分提交）。
	if pid, _, apid, aiid, _ := nsTaskRow(t, db, "t-rb"); pid != "" || apid != "" || aiid != "" {
		t.Errorf("tasks/anchor MUST stay un-backfilled on tx rollback, got (%v,%v,%v)", pid, apid, aiid)
	}
	if spid, siid := nsSessionRow(t, db, "t-rb", "s-rb"); spid != "" || siid != "" {
		t.Errorf("session MUST stay un-backfilled, got (%v,%v)", spid, siid)
	}

	// 移除失败源 → 重跑成功收敛（幂等覆盖此前回滚的行）。
	if _, err := db.Exec(`DROP TRIGGER ns_fail_sessions`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := db.BackfillProviderNamespaces(ctx); err != nil {
		t.Fatalf("backfill after removing failure source: %v", err)
	}
	if pid, _, apid, aiid, _ := nsTaskRow(t, db, "t-rb"); pid != ProviderIDV1 || apid != ProviderIDV1 || aiid != ProviderInstanceV1Default {
		t.Errorf("t-rb after rerun = (%v,%v,%v), want all defaults", pid, apid, aiid)
	}
	if spid, siid := nsSessionRow(t, db, "t-rb", "s-rb"); spid != ProviderIDV1 || siid != ProviderInstanceV1Default {
		t.Errorf("t-rb session after rerun = (%v,%v)", spid, siid)
	}
}

// TestRollbackThenUpgrade_NamespaceConverges 回滚再升级 fixture（tasks 2.2 验证）：
// 升级到 0018 → 模拟旧二进制回滚窗口写入（省略新列的 INSERT + 显式空值 UPDATE）→
// 再升级（重启：Migrate 幂等 + BackfillProviderNamespaces 重跑）→ 全部行命名空间
// 正确、归属判定（OwnerOf 形状反查）结果与现状一致。
func TestRollbackThenUpgrade_NamespaceConverges(t *testing.T) {
	db := openTestDBRaw(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to 0018: %v", err)
	}
	seedNamespaceProjectTask(t, db, "t-keep", "sess-keep")
	seedNamespaceSession(t, db, "t-keep", "sess-keep")

	// 旧二进制窗口：省略新列 INSERT（DEFAULT 生效）+ 显式空值写（边界情形）。
	now := time.Now().Unix()
	if _, err := db.Exec(
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, anchor_session_id, created_at, updated_at)
		 VALUES ('t-rollback', 'p1', 'old task', 'b', 'suspended', '/tmp/w/t-rollback', 'sess-rb', ?, ?)`, now, now); err != nil {
		t.Fatalf("old-binary insert task: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at)
		 VALUES ('t-rollback', 'sess-rb', 1, 1, 1)`); err != nil {
		t.Fatalf("old-binary insert session: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET provider_id = '', anchor_provider_id = NULL WHERE id = 't-rollback'`); err != nil {
		t.Fatalf("old-binary explicit empty write: %v", err)
	}

	// 再升级：重启序列（Migrate 幂等跳过 0018 → 命名空间补齐）。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-upgrade Migrate: %v", err)
	}
	if err := db.BackfillProviderNamespaces(ctx); err != nil {
		t.Fatalf("re-upgrade backfill: %v", err)
	}

	for _, taskID := range []string{"t-keep", "t-rollback"} {
		if pid, _, apid, aiid, _ := nsTaskRow(t, db, taskID); pid != ProviderIDV1 || apid != ProviderIDV1 || aiid != ProviderInstanceV1Default {
			t.Errorf("%s namespace after re-upgrade = (%v,%v,%v), want all defaults", taskID, pid, apid, aiid)
		}
		if spid, siid := nsSessionRow(t, db, taskID, "sess-"+map[string]string{"t-keep": "keep", "t-rollback": "rb"}[taskID]); spid != ProviderIDV1 || siid != ProviderInstanceV1Default {
			t.Errorf("%s session namespace after re-upgrade = (%v,%v)", taskID, spid, siid)
		}
	}
	// 归属判定与现状一致：按 session_id 反查 owner 命中且唯一。
	for _, sid := range []string{"sess-keep", "sess-rb"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE session_id = ?`, sid).Scan(&n); err != nil || n != 1 {
			t.Errorf("ownership lookup for %s: count=%d err=%v, want 1", sid, n, err)
		}
	}
}
