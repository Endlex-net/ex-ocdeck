package store

// session_key_test.go 验证 tasks 3.5：SessionKey 类型化 claim（D4 三列谓词 +
// BEGIN IMMEDIATE）、TaskBinding 列读写（D3 版本化 envelope、未知版本 fail-closed）、
// 既有接缝（ClaimTaskSession）与 SessionKey 接口的单命名空间行为等价。

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	domain "ocdeck/internal/domain/agentprovider"
)

func v1SessionKey(upstream string) domain.SessionKey {
	return domain.SessionKey{ProviderID: ProviderIDV1, ProviderInstanceID: ProviderInstanceV1Default, UpstreamID: upstream}
}

func seedKeyTask(t *testing.T, db *DB, taskID string) {
	t.Helper()
	seedNamespaceProjectTask(t, db, taskID, "")
}

func TestClaimSessionKey_ConflictIdempotentChanged(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedKeyTask(t, db, "t1")
	seedKeyTask(t, db, "t2")
	key := v1SessionKey("ses_race")

	// 无归属 → 插入，Claimed+Changed。
	res, err := db.ClaimSessionKey(ctx, "t1", key, 10, 1, 100, "")
	if err != nil || !res.Claimed || !res.Changed {
		t.Fatalf("first claim = (%+v, %v), want claimed+changed", res, err)
	}
	// 他任务持有 → 冲突不写入，OwnerTaskID 指向持有者。
	res, err = db.ClaimSessionKey(ctx, "t2", key, 10, 1, 200, "")
	if err != nil || res.Claimed || res.OwnerTaskID != "t1" {
		t.Fatalf("conflict claim = (%+v, %v), want conflict owned by t1", res, err)
	}
	// 冲突方未写入行。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t2'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("冲突方写入行数 = %d err=%v, want 0", n, err)
	}
	// 本任务幂等 upsert：last_seen 推进 → Changed；同值 → !Changed。
	res, err = db.ClaimSessionKey(ctx, "t1", key, 10, 1, 300, "")
	if err != nil || !res.Claimed || !res.Changed {
		t.Fatalf("advance claim = (%+v, %v), want changed", res, err)
	}
	res, err = db.ClaimSessionKey(ctx, "t1", key, 10, 1, 300, "")
	if err != nil || !res.Claimed || res.Changed {
		t.Fatalf("same-value claim = (%+v, %v), want idempotent !Changed", res, err)
	}
	// 零值键 → 拒绝。
	if _, err := db.ClaimSessionKey(ctx, "t1", domain.SessionKey{}, 1, 1, 1, ""); err == nil {
		t.Error("不完整 SessionKey 必须拒绝")
	}
}

func TestClaimSessionKey_NamespaceScopedConflict(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedKeyTask(t, db, "t1")
	seedKeyTask(t, db, "t2")
	// 三列谓词：同 upstream id、不同命名空间 → 各自独立归属（互不冲突）。
	v1 := v1SessionKey("ses_shared")
	other := domain.SessionKey{ProviderID: "codex", ProviderInstanceID: "pool-a", UpstreamID: "ses_shared"}
	if res, err := db.ClaimSessionKey(ctx, "t1", v1, 1, 1, 1, ""); err != nil || !res.Claimed {
		t.Fatalf("claim v1 = (%+v, %v)", res, err)
	}
	if res, err := db.ClaimSessionKey(ctx, "t2", other, 1, 1, 1, ""); err != nil || !res.Claimed {
		t.Fatalf("claim other namespace = (%+v, %v)，三列谓词下不得冲突", res, err)
	}
	// 反向：同命名空间同 upstream 仍冲突（冲突集合与单命名空间现状一致）。
	if res, err := db.ClaimSessionKey(ctx, "t2", v1, 1, 1, 2, ""); err != nil || res.Claimed || res.OwnerTaskID != "t1" {
		t.Fatalf("same-namespace conflict = (%+v, %v)", res, err)
	}
	// 行命名空间列按 claim 键写入。
	var pid, piid string
	if err := db.QueryRow(`SELECT provider_id, provider_instance_id FROM task_sessions WHERE task_id='t2' AND session_id='ses_shared'`).
		Scan(&pid, &piid); err != nil || pid != "codex" || piid != "pool-a" {
		t.Errorf("命名空间列 = (%q,%q) err=%v", pid, piid, err)
	}
}

func TestClaimSessionKeyAndSetAnchor_AnchorTriple(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedKeyTask(t, db, "t1")
	seedKeyTask(t, db, "t2")
	key := v1SessionKey("ses_anchor")

	// claim + 锚定三元组单事务写入。
	if res, err := db.ClaimSessionKeyAndSetAnchor(ctx, "t1", key, 5, 5, 50, ""); err != nil || !res.Claimed {
		t.Fatalf("claim+anchor = (%+v, %v)", res, err)
	}
	var sid, apid, aiid string
	if err := db.QueryRow(
		`SELECT anchor_session_id, anchor_provider_id, anchor_provider_instance_id FROM tasks WHERE id='t1'`).
		Scan(&sid, &apid, &aiid); err != nil || sid != "ses_anchor" || apid != ProviderIDV1 || aiid != ProviderInstanceV1Default {
		t.Fatalf("锚定三元组 = (%q,%q,%q) err=%v", sid, apid, aiid, err)
	}
	// 冲突路径：归属与锚定均不修改（t2 已有他锚定也不得被覆盖）。
	if _, err := db.Exec(`UPDATE tasks SET anchor_session_id='old-anchor' WHERE id='t2'`); err != nil {
		t.Fatalf("seed t2 anchor: %v", err)
	}
	res, err := db.ClaimSessionKeyAndSetAnchor(ctx, "t2", key, 5, 5, 60, "")
	if err != nil || res.Claimed || res.OwnerTaskID != "t1" {
		t.Fatalf("conflict claim+anchor = (%+v, %v)", res, err)
	}
	if err := db.QueryRow(`SELECT anchor_session_id FROM tasks WHERE id='t2'`).Scan(&sid); err != nil || sid != "old-anchor" {
		t.Fatalf("冲突时锚定被修改: %q err=%v", sid, err)
	}
}

func TestClaimSessionKey_ConcurrentUniqueOwnership(t *testing.T) {
	// D4 验证：真实 SQLite 上两任务并发 claim 同一 SessionKey，仅一个成功。
	// BEGIN IMMEDIATE 经 DSN _txlock=immediate（store.go）；database/sql 单连接
	// 下并发 BeginTx 互斥等待。
	db := openTestDB(t)
	ctx := context.Background()
	seedKeyTask(t, db, "ta")
	seedKeyTask(t, db, "tb")
	key := v1SessionKey("ses_concurrent")

	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i, taskID := range []string{"ta", "tb"} {
		wg.Add(1)
		go func(taskID string, idx int) {
			defer wg.Done()
			res, err := db.ClaimSessionKey(ctx, taskID, key, 1, 1, 100+int64(idx), "")
			if err != nil {
				t.Errorf("claim %s: %v", taskID, err)
				return
			}
			results[idx] = res.Claimed
		}(taskID, i)
	}
	wg.Wait()
	won := 0
	for _, c := range results {
		if c {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("并发 claim 获胜方 = %d, want 恰好 1（results=%v）", won, results)
	}
	// 归属唯一：仅一行。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE session_id='ses_concurrent'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("归属行数 = %d err=%v, want 1", n, err)
	}
}

func TestClaimTaskSession_SeamEquivalenceWithSessionKey(t *testing.T) {
	// 既有接缝（task 层现状）内部按 v1 常量命名空间执行——与 ClaimSessionKey
	// 在单命名空间下行为等价（tasks 3.5「单命名空间下行为等价」验证）。
	db := openTestDB(t)
	ctx := context.Background()
	seedKeyTask(t, db, "t1")
	seedKeyTask(t, db, "t2")

	res, err := db.ClaimTaskSession(ctx, "t1", "ses_seam", 1, 1, 10, "")
	if err != nil || !res.Claimed {
		t.Fatalf("seam claim = (%+v, %v)", res, err)
	}
	// 旧接缝冲突语义保持。
	res, err = db.ClaimTaskSession(ctx, "t2", "ses_seam", 1, 1, 20, "")
	if err != nil || res.Claimed || res.OwnerTaskID != "t1" {
		t.Fatalf("seam conflict = (%+v, %v)", res, err)
	}
	// 新接缝读旧行（命名空间列默认值正确写入）。
	if res, err := db.ClaimSessionKey(ctx, "t1", v1SessionKey("ses_seam"), 1, 1, 30, ""); err != nil || !res.Claimed || !res.Changed {
		t.Fatalf("key claim over seam row = (%+v, %v)", res, err)
	}
	// 锚定旧接缝同样写三元组。
	if res, err := db.ClaimTaskSessionAndSetAnchor(ctx, "t1", "ses_seam2", 1, 1, 1, ""); err != nil || !res.Claimed {
		t.Fatalf("seam anchor = (%+v, %v)", res, err)
	}
	var apid, aiid string
	if err := db.QueryRow(`SELECT anchor_provider_id, anchor_provider_instance_id FROM tasks WHERE id='t1'`).Scan(&apid, &aiid); err != nil || apid != ProviderIDV1 || aiid != ProviderInstanceV1Default {
		t.Fatalf("seam anchor 三元组 = (%q,%q) err=%v", apid, aiid, err)
	}
}

func TestTaskBinding_ReadWriteFailClosed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedKeyTask(t, db, "t1")

	// 无绑定 → (zero, false, nil)。
	b, ok, err := db.GetTaskBinding(ctx, "t1")
	if err != nil || ok || b.UpstreamSessionID != "" || b.ProviderID != "" {
		t.Fatalf("empty binding = (%+v, %v, %v)", b, ok, err)
	}
	// 写入 + 读回 roundtrip。
	want := domain.TaskBinding{
		Version: 1, ProviderID: ProviderIDV1, ProviderInstanceID: ProviderInstanceV1Default,
		UpstreamSessionID: "ses_b", TerminalRef: "ocdeck-t1-runtime",
		Data: []byte(`{"port":50001,"directory":"/wt","auth":{"kind":"derived"}}`),
	}
	if err := db.SetTaskBinding(ctx, "t1", want); err != nil {
		t.Fatalf("SetTaskBinding: %v", err)
	}
	got, ok, err := db.GetTaskBinding(ctx, "t1")
	if err != nil || !ok || got.UpstreamSessionID != want.UpstreamSessionID ||
		got.ProviderID != want.ProviderID || got.TerminalRef != want.TerminalRef || string(got.Data) != string(want.Data) {
		t.Fatalf("roundtrip = (%+v, %v, %v)", got, ok, err)
	}
	// 未知版本落库（模拟旧数据/损坏）→ GetTaskBinding fail-closed。
	if _, err := db.Exec(`UPDATE tasks SET task_binding = ? WHERE id='t1'`,
		`{"version":99,"provider_id":"p","provider_instance_id":"d","upstream_session_id":"s","terminal_ref":"t"}`); err != nil {
		t.Fatalf("corrupt binding: %v", err)
	}
	if _, _, err := db.GetTaskBinding(ctx, "t1"); err == nil {
		t.Fatal("未知版本必须 fail-closed 返回错误")
	}
	// 畸形 JSON 落库 → fail-closed。
	if _, err := db.Exec(`UPDATE tasks SET task_binding = '{bad' WHERE id='t1'`); err != nil {
		t.Fatalf("malformed binding: %v", err)
	}
	if _, _, err := db.GetTaskBinding(ctx, "t1"); err == nil {
		t.Fatal("畸形 JSON 必须 fail-closed")
	}
	// 不完整绑定拒绝落库（Encode 侧）。
	if err := db.SetTaskBinding(ctx, "t1", domain.TaskBinding{Version: 1, ProviderID: "p"}); err == nil {
		t.Fatal("不完整绑定必须拒绝写入")
	}
	// 行不存在 → sql.ErrNoRows。
	if err := db.SetTaskBinding(ctx, "t-nope", want); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetTaskBinding 缺行 = %v, want sql.ErrNoRows", err)
	}
	if _, _, err := db.GetTaskBinding(ctx, "t-nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetTaskBinding 缺行 = %v, want sql.ErrNoRows", err)
	}
}

// TestCommitActivation_BindingAndStatusAtomic 验证激活提交事务（opencode-v1-spi-provider
// tasks 4.3 / design D9 binding 提交协议）：
//   - CAS matched → binding 与状态同事务落库（binding 可读回）；
//   - CAS 失配（状态非 activating）→ 零持久化副作用（binding 不写入、旧值保持）；
//   - 不完整 binding（Encode 拒绝）→ 整体失败零副作用。
func TestCommitActivation_BindingAndStatusAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedKeyTask(t, db, "t1")
	binding := domain.TaskBinding{
		Version: 1, ProviderID: ProviderIDV1, ProviderInstanceID: ProviderInstanceV1Default,
		UpstreamSessionID: "ses_a", TerminalRef: "ocdeck-t1-runtime",
		Data: []byte(`{"port":50001,"directory":"/wt","taskId":"t1","auth":{"kind":"derived"}}`),
	}

	// ① CAS matched：activating→active + binding 同事务。
	if _, err := db.Exec(`UPDATE tasks SET status='activating' WHERE id='t1'`); err != nil {
		t.Fatal(err)
	}
	res, err := db.CommitActivation(ctx, "t1", binding, "activating", "active")
	if err != nil {
		t.Fatalf("CommitActivation: %v", err)
	}
	if !res.Matched {
		t.Fatalf("result = %+v, want Matched", res)
	}
	if _, err := db.Exec(`UPDATE tasks SET status='activating' WHERE id='t1'`); err != nil {
		t.Fatal(err)
	}
	got, ok, gerr := db.GetTaskBinding(ctx, "t1")
	if gerr != nil || !ok || got.UpstreamSessionID != "ses_a" {
		t.Fatalf("binding after matched commit = (%+v, %v, %v), want persisted", got, ok, gerr)
	}

	// ② CAS 失配（状态 suspended，非 activating）→ binding 不落库（原值保持）。
	if _, err := db.Exec(`UPDATE tasks SET status='suspended' WHERE id='t1'`); err != nil {
		t.Fatal(err)
	}
	other := binding
	other.UpstreamSessionID = "ses_other"
	res, err = db.CommitActivation(ctx, "t1", other, "activating", "active")
	if err != nil {
		t.Fatalf("mismatch CommitActivation: %v", err)
	}
	if res.Matched {
		t.Fatalf("mismatch result = %+v, want !Matched", res)
	}
	got, _, _ = db.GetTaskBinding(ctx, "t1")
	if got.UpstreamSessionID != "ses_a" {
		t.Errorf("binding after mismatch = %q, want 原值 ses_a 保持（零持久化副作用）", got.UpstreamSessionID)
	}

	// ③ 不完整 binding → Encode 拒绝、状态零修改。
	if _, err := db.Exec(`UPDATE tasks SET status='activating' WHERE id='t1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitActivation(ctx, "t1", domain.TaskBinding{Version: 1, ProviderID: "p"}, "activating", "active"); err == nil {
		t.Fatal("不完整 binding 必须拒绝（Encode fail-closed）")
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM tasks WHERE id='t1'`).Scan(&status); err != nil || status != "activating" {
		t.Errorf("status after invalid binding = %q err=%v, want activating unchanged", status, err)
	}
}
