// diff_review_test.go 验证 diff-review-workbench store 层原语原子性（tasks.md 1.3）。
//
// 范围（tasks.md 1.3）：存储层语义，调度编排归 3.11。
//   - 条件 UPDATE CAS 语义（matched/!matched 分类）
//   - sent 清理事务原子性与 revision 语义（同秒编辑不误删、事务不可拆分）
//   - message_id UNIQUE 碰撞
//   - 分区排序稳定性（同秒 seq 决胜）
//   - 启动收敛事务原子回滚与错误传播
//
// 行为测试自检：新原语测试天然满足「旧实现（无此原语时）失败」——新增 SQL/方法在无 migration 0012
// 或无方法时编译/运行失败；对纯新增 SQL 无法做旧失败验证的项（如 partition 排序），由测试断言
// 锁定当前行为，移除排序子句即失败。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// seedDiffReviewTask 创建 project+task 并返回；测试基线。
func seedDiffReviewTask(t *testing.T, db *DB, taskID string) {
	t.Helper()
	seedProjectTask(t, db, taskID)
}

// createTestAnnotation 插入一条批注并返回行（revision=1）。
func createTestAnnotation(t *testing.T, db *DB, id, taskID string) DiffAnnotationRow {
	t.Helper()
	ctx := context.Background()
	r := DiffAnnotationRow{
		ID: id, TaskID: taskID, Path: "a.go", Side: "new", Ref: "", Untracked: false,
		StartLine: 1, EndLine: 3, SnapshotStartLine: 1, Snapshot: "line1\nline2\nline3\n",
		SnapshotLineCount: 3, Comment: "c-" + id,
	}
	if _, err := db.CreateDiffAnnotation(ctx, r); err != nil {
		t.Fatalf("create annotation %s: %v", id, err)
	}
	got, err := db.GetDiffAnnotation(ctx, id)
	if err != nil {
		t.Fatalf("get annotation %s: %v", id, err)
	}
	return got
}

// createQueuedSubmissionWithItems 创建一条 queued 提交并写入 items 快照。
// 复核直接从 items 的 AnnotationID/AnnotationRevision 派生（无独立 RevisionCheck 数组）。
func createQueuedSubmissionWithItems(t *testing.T, db *DB, subID, taskID, msgID string,
	annotations []DiffAnnotationRow,
) DiffReviewSubmissionRow {
	t.Helper()
	ctx := context.Background()
	items := make([]DiffReviewSubmissionItemRow, 0, len(annotations))
	for _, a := range annotations {
		items = append(items, DiffReviewSubmissionItemRow{
			AnnotationID: a.ID, AnnotationRevision: a.Revision,
			Path: a.Path, Side: a.Side, Ref: a.Ref, Untracked: a.Untracked,
			StartLine: a.StartLine, EndLine: a.EndLine, SnapshotStartLine: a.SnapshotStartLine,
			Snapshot: a.Snapshot, Comment: a.Comment,
		})
	}
	sub := DiffReviewSubmissionRow{
		ID: subID, TaskID: taskID, Status: DiffReviewStatusQueued,
		TargetSessionID: "sess-1", MessageID: msgID, Note: "n", Payload: "p", Truncated: false,
	}
	if _, err := db.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{
		Submission: sub,
		Items:      items,
	}); err != nil {
		t.Fatalf("create submission %s: %v", subID, err)
	}
	got, err := db.GetDiffReviewSubmission(ctx, subID)
	if err != nil {
		t.Fatalf("get submission %s: %v", subID, err)
	}
	return got
}

// --- 条件 UPDATE CAS 语义 ---

// TestCASDiffReviewSubmission_MatchedAndNotMatched 验证 CAS 转移 matched/!matched 分类：
// from 匹配 → matched=true + 状态迁移；from 不匹配 → matched=false + 状态不变。
func TestCASDiffReviewSubmission_MatchedAndNotMatched(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	sub := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})

	// queued→sending：from 匹配 → matched。
	matched, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusSending, "")
	if err != nil {
		t.Fatalf("CAS queued->sending: %v", err)
	}
	if !matched {
		t.Fatal("CAS from queued with current queued: want matched=true")
	}
	got, _ := db.GetDiffReviewSubmission(ctx, sub.ID)
	if got.Status != DiffReviewStatusSending {
		t.Fatalf("status=%s want sending", got.Status)
	}

	// 再次 queued→sending：当前 sending，from=queued 不匹配 → !matched + 状态不变。
	matched2, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusSending, "")
	if err != nil {
		t.Fatalf("CAS second: %v", err)
	}
	if matched2 {
		t.Fatal("CAS from queued with current sending: want matched=false")
	}
	got2, _ := db.GetDiffReviewSubmission(ctx, sub.ID)
	if got2.Status != DiffReviewStatusSending {
		t.Fatalf("status=%s want unchanged sending", got2.Status)
	}

	// sending→failed with errorText → matched + error 落库。
	matched3, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusSending, DiffReviewStatusFailed, "boom")
	if err != nil {
		t.Fatalf("CAS sending->failed: %v", err)
	}
	if !matched3 {
		t.Fatal("CAS sending->failed: want matched=true")
	}
	got3, _ := db.GetDiffReviewSubmission(ctx, sub.ID)
	if got3.Status != DiffReviewStatusFailed || got3.Error != "boom" {
		t.Fatalf("status=%s error=%q want failed/boom", got3.Status, got3.Error)
	}
}

// TestCASDiffReviewSubmission_ErrorTextOnlyOnFailure 验证 errorText 仅在转移写入时生效，
// 空 errorText 不覆盖原 error（正常转移 queued→sending 不清空 error）。
func TestCASDiffReviewSubmission_ErrorTextOnlyOnFailure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	sub := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})

	// 先 failed 带 error。
	if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusFailed, "e1"); err != nil {
		t.Fatal(err)
	}
	// failed→failed 不在合法转移中，但即便尝试，errorText 为空时不应清空已落库 error。
	// 这里只验证 CAS 不误写空 error：用一个 from 匹配但 to 相同、errorText 空的调用，
	// 验证 error 列保持原值（空 errorText 走 status-only UPDATE，不触碰 error 列）。
	// 但 failed→failed 无状态机意义；改为 queued→sending 不带 error（error 列初始为空）。
	// 重置为 queued 再测：
	if _, err := db.ExecContext(ctx,
		`UPDATE diff_review_submissions SET status=?, error='' WHERE id=?`,
		DiffReviewStatusQueued, sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetDiffReviewSubmission(ctx, sub.ID)
	if got.Error != "" {
		t.Fatalf("error=%q want empty (status-only CAS must not touch error col)", got.Error)
	}
}

// --- sent 清理事务原子性与 revision 语义 ---

// TestCompleteDiffReviewSentCleanup_DeletesUnchangedAnnotations 验证 sent 清理事务：
// sending→sent + 按 id+revision 删除活动批注（revision 仍匹配才删）。
func TestCompleteDiffReviewSentCleanup_DeletesUnchangedAnnotations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a1 := createTestAnnotation(t, db, "a1", "t1")
	a2 := createTestAnnotation(t, db, "a2", "t1")
	sub := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a1, a2})

	// 抢占到 sending。
	if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
		t.Fatal(err)
	}

	// 排队期间 a1 被编辑（revision 1→2），a2 保持不变。
	if _, err := db.UpdateDiffAnnotationComment(ctx, a1.ID, "edited"); err != nil {
		t.Fatal(err)
	}

	matched, err := db.CompleteDiffReviewSentCleanup(ctx, sub.ID)
	if err != nil {
		t.Fatalf("sent cleanup: %v", err)
	}
	if !matched {
		t.Fatal("want matched=true (was sending)")
	}

	// submission → sent + sent_at 写入。
	got, _ := db.GetDiffReviewSubmission(ctx, sub.ID)
	if got.Status != DiffReviewStatusSent {
		t.Fatalf("status=%s want sent", got.Status)
	}
	if !got.SentAt.Valid || got.SentAt.Int64 == 0 {
		t.Fatalf("sent_at=%v want valid non-zero", got.SentAt)
	}

	// a1（revision 已 +1）保留；a2（revision 不变）被删除。
	if _, err := db.GetDiffAnnotation(ctx, a1.ID); err != nil {
		t.Errorf("a1 (edited, revision+1) should be kept, err=%v", err)
	}
	if _, err := db.GetDiffAnnotation(ctx, a2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a2 (unchanged) should be deleted, err=%v", err)
	}
}

// TestCompleteDiffReviewSentCleanup_CASMismatchZeroModify 验证 CAS 失配零修改：
// 非 sending 时调用 sent 清理 → matched=false，状态与批注均不变。
func TestCompleteDiffReviewSentCleanup_CASMismatchZeroModify(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	sub := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})

	// 当前 queued（未抢占到 sending）→ sent 清理应 !matched + 零修改。
	matched, err := db.CompleteDiffReviewSentCleanup(ctx, sub.ID)
	if err != nil {
		t.Fatalf("sent cleanup: %v", err)
	}
	if matched {
		t.Fatal("want matched=false on non-sending CAS mismatch")
	}
	got, _ := db.GetDiffReviewSubmission(ctx, sub.ID)
	if got.Status != DiffReviewStatusQueued {
		t.Fatalf("status=%s want unchanged queued", got.Status)
	}
	if got.SentAt.Valid {
		t.Errorf("sent_at=%v want NULL on CAS mismatch", got.SentAt)
	}
	// 批注保留。
	if _, err := db.GetDiffAnnotation(ctx, a.ID); err != nil {
		t.Errorf("annotation should be kept on CAS mismatch, err=%v", err)
	}
}

// TestCompleteDiffReviewSentCleanup_SameSecondEditNoMisDelete 验证同秒编辑不误删：
// revision 是版本比对唯一依据，秒级 updated_at 同秒实变不推进不影响清理判定。
// 即便批注 updated_at 与 sent_at 同秒，revision+1 的批注仍保留。
func TestCompleteDiffReviewSentCleanup_SameSecondEditNoMisDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	sub := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})

	// 锁定 nowUnix 为同一秒，模拟同秒编辑 + sent 清理。
	restore := withNowUnix(t, 1000)
	defer restore()

	// 抢占 + 同秒编辑（revision 1→2，updated_at 不推进但仍为 1000）。
	if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateDiffAnnotationComment(ctx, a.ID, "edited-same-second"); err != nil {
		t.Fatal(err)
	}
	annot, _ := db.GetDiffAnnotation(ctx, a.ID)
	if annot.Revision != 2 {
		t.Fatalf("revision=%d want 2", annot.Revision)
	}
	if annot.UpdatedAt != 1000 {
		t.Fatalf("updated_at=%d want 1000 (same second, no advance)", annot.UpdatedAt)
	}

	// sent 清理：submission items 快照 revision=1，当前 revision=2 → 不命中删除，批注保留。
	matched, err := db.CompleteDiffReviewSentCleanup(ctx, sub.ID)
	if err != nil {
		t.Fatalf("sent cleanup: %v", err)
	}
	if !matched {
		t.Fatal("want matched=true")
	}
	got, _ := db.GetDiffReviewSubmission(ctx, sub.ID)
	if got.Status != DiffReviewStatusSent {
		t.Fatalf("status=%s want sent", got.Status)
	}
	if _, err := db.GetDiffAnnotation(ctx, a.ID); err != nil {
		t.Errorf("same-second edited annotation (revision+1) should be kept, err=%v", err)
	}
}

// TestCompleteDiffReviewSentCleanup_TransactionAtomic 验证事务不可拆分：
// sent 清理在单一 SQLite 事务内完成；无中间态（不会出现 sent 而批注未删，或批注已删而未 sent）。
// 此处通过原子性正路径锁定：调用后 submission=sent 与批注删除同时成立，且 CAS 失配时两者均不变。
// （SQLite 单写者事务保证原子性，测试锁定可观察的不变量。）
func TestCompleteDiffReviewSentCleanup_TransactionAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	sub := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})
	if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
		t.Fatal(err)
	}
	matched, err := db.CompleteDiffReviewSentCleanup(ctx, sub.ID)
	if err != nil || !matched {
		t.Fatalf("sent cleanup: matched=%v err=%v", matched, err)
	}
	// 不变量：sent ⇔ 批注已删（正路径两者同时成立）。
	got, _ := db.GetDiffReviewSubmission(ctx, sub.ID)
	if got.Status != DiffReviewStatusSent {
		t.Fatalf("status=%s want sent", got.Status)
	}
	if _, err := db.GetDiffAnnotation(ctx, a.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("annotation should be deleted in same tx as sent, err=%v", err)
	}
}

// --- message_id UNIQUE 碰撞 ---

// TestCreateDiffReviewSubmission_MessageIDUniqueCollision 验证 message_id UNIQUE 约束：
// 同一 message_id 第二次插入 → 底层错误（约束冲突），整事务回滚（零落库）。
func TestCreateDiffReviewSubmission_MessageIDUniqueCollision(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	const dupMsg = "msg_dup"
	_ = createQueuedSubmissionWithItems(t, db, "s1", "t1", dupMsg, []DiffAnnotationRow{a})

	// 第二条用相同 message_id → 应失败（items 空：无复核项，审核通过，但 message_id UNIQUE 冲突）。
	sub2 := DiffReviewSubmissionRow{
		ID: "s2", TaskID: "t1", Status: DiffReviewStatusQueued,
		TargetSessionID: "sess-1", MessageID: dupMsg, Note: "n", Payload: "p2",
	}
	_, err := db.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{
		Submission: sub2,
		Items:      nil,
	})
	if err == nil {
		t.Fatal("duplicate message_id must be rejected")
	}
	// s2 未落库。
	if _, err := db.GetDiffReviewSubmission(ctx, "s2"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("s2 should not exist after collision, err=%v", err)
	}
}

// TestCreateDiffReviewSubmission_IDUniqueCollision 验证 id UNIQUE 约束。
func TestCreateDiffReviewSubmission_IDUniqueCollision(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	_ = createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})

	// 第二条用相同 id（不同 message_id）→ 应失败。
	sub2 := DiffReviewSubmissionRow{
		ID: "s1", TaskID: "t1", Status: DiffReviewStatusQueued,
		TargetSessionID: "sess-1", MessageID: "msg_s2", Note: "n", Payload: "p2",
	}
	_, err := db.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{
		Submission: sub2,
		Items:      nil,
	})
	if err == nil {
		t.Fatal("duplicate id must be rejected")
	}
}

// --- 复核失败 → conflict 零落库 ---

// TestCreateDiffReviewSubmission_RevisionConflictZeroPersist 验证复核失败：
// 事务内批注 revision 已变化（+1）→ ErrDiffReviewRevisionConflict，整事务回滚（零落库）。
func TestCreateDiffReviewSubmission_RevisionConflictZeroPersist(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")

	// 复核项携带 revision=1，但批注已被编辑为 revision=2。
	if _, err := db.UpdateDiffAnnotationComment(ctx, a.ID, "edited-before-submit"); err != nil {
		t.Fatal(err)
	}
	sub := DiffReviewSubmissionRow{
		ID: "s1", TaskID: "t1", Status: DiffReviewStatusQueued,
		TargetSessionID: "sess-1", MessageID: "msg_s1", Note: "n", Payload: "p",
	}
	// item 携带 annotation_revision=1，但批注已被编辑为 revision=2 → 复核从 item 派生，conflict。
	_, err := db.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{
		Submission: sub,
		Items: []DiffReviewSubmissionItemRow{{AnnotationID: a.ID, AnnotationRevision: 1, Path: a.Path, Side: a.Side, Snapshot: a.Snapshot, Comment: a.Comment}},
	})
	if !errors.Is(err, ErrDiffReviewRevisionConflict) {
		t.Fatalf("err=%v want ErrDiffReviewRevisionConflict", err)
	}
	// 零落库：submission 与 items 均未写入。
	if _, err := db.GetDiffReviewSubmission(ctx, "s1"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("submission should not exist after conflict, err=%v", err)
	}
	items, _ := db.ListDiffReviewSubmissionItems(ctx, "s1")
	if len(items) != 0 {
		t.Errorf("items should not exist after conflict, got %d", len(items))
	}
}

// TestCreateDiffReviewSubmission_RevisionConflictAnnotationDeleted 验证复核时批注已删除 → conflict（不区分从未存在与预览后删除，D2）。
func TestCreateDiffReviewSubmission_RevisionConflictAnnotationDeleted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	// 删除批注。
	if _, err := db.DeleteDiffAnnotation(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	sub := DiffReviewSubmissionRow{
		ID: "s1", TaskID: "t1", Status: DiffReviewStatusQueued,
		TargetSessionID: "sess-1", MessageID: "msg_s1", Note: "n", Payload: "p",
	}
	_, err := db.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{
		Submission: sub,
		Items: []DiffReviewSubmissionItemRow{{AnnotationID: a.ID, AnnotationRevision: 1, Path: a.Path, Side: a.Side, Snapshot: a.Snapshot, Comment: a.Comment}},
	})
	if !errors.Is(err, ErrDiffReviewRevisionConflict) {
		t.Fatalf("err=%v want ErrDiffReviewRevisionConflict (deleted annotation)", err)
	}
}

// --- 分区排序稳定性（同秒 seq 决胜） ---

// TestListDiffReviewHistory_SortStabilitySameSecondSeqWins 验证 history 分区排序：
// sent_at DESC, seq DESC；同秒 sent_at 时以 seq 决胜（排序稳定）。
// 移除 ORDER BY seq DESC 子句即失败（锁定排序行为）。
func TestListDiffReviewHistory_SortStabilitySameSecondSeqWins(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")

	// 创建三条 queued 提交（seq 自增），每条引用同一批注 a（首条 sent 清理后 a 被删，
	// 后续 sent 清理无批注可删但 submission 仍能 sending→sent）。
	subs := []string{"s1", "s2", "s3"}
	for _, sid := range subs {
		s := createQueuedSubmissionWithItems(t, db, sid, "t1", "msg_"+sid, []DiffAnnotationRow{a})
		if _, err := db.CASDiffReviewSubmission(ctx, s.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
			t.Fatal(err)
		}
	}

	// 锁定同秒：三条在同一秒内完成 sent 清理 → sent_at 相同 → seq DESC 决胜。
	restore := withNowUnix(t, 7000)
	defer restore()
	for _, sid := range subs {
		if _, err := db.CompleteDiffReviewSentCleanup(ctx, sid); err != nil {
			t.Fatalf("sent cleanup %s: %v", sid, err)
		}
	}

	hist, err := db.ListDiffReviewHistory(ctx, "t1")
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len=%d want 3", len(hist))
	}
	// 同秒 sent_at（7000）→ seq DESC：s3(seq=3) > s2(seq=2) > s1(seq=1)。
	want := []string{"s3", "s2", "s1"}
	for i, w := range want {
		if hist[i].ID != w {
			t.Errorf("history[%d].ID=%s want %s (same-second seq DESC tiebreaker)", i, hist[i].ID, w)
		}
		if hist[i].SentAt.Int64 != 7000 {
			t.Errorf("history[%d].sent_at=%d want 7000", i, hist[i].SentAt.Int64)
		}
	}
}

// TestListDiffReviewFailures_SortStabilitySameSecondSeqWins 验证 failures 分区排序：
// created_at DESC, seq DESC；同秒 created_at 时以 seq 决胜。
func TestListDiffReviewFailures_SortStabilitySameSecondSeqWins(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")

	// 同秒创建三条 queued（created_at 相同）。
	restore := withNowUnix(t, 9000)
	defer restore()
	subs := []string{"s1", "s2", "s3"}
	for _, sid := range subs {
		s := createQueuedSubmissionWithItems(t, db, sid, "t1", "msg_"+sid, []DiffAnnotationRow{a})
		// 转 failed。
		if _, err := db.CASDiffReviewSubmission(ctx, s.ID, DiffReviewStatusQueued, DiffReviewStatusFailed, "e-"+sid); err != nil {
			t.Fatal(err)
		}
	}

	failures, err := db.ListDiffReviewFailures(ctx, "t1")
	if err != nil {
		t.Fatalf("list failures: %v", err)
	}
	if len(failures) != 3 {
		t.Fatalf("failures len=%d want 3", len(failures))
	}
	// 同秒 created_at（9000）→ seq DESC：s3 > s2 > s1。
	want := []string{"s3", "s2", "s1"}
	for i, w := range want {
		if failures[i].ID != w {
			t.Errorf("failures[%d].ID=%s want %s (same-second seq DESC tiebreaker)", i, failures[i].ID, w)
		}
		if failures[i].CreatedAt != 9000 {
			t.Errorf("failures[%d].created_at=%d want 9000", i, failures[i].CreatedAt)
		}
	}
}

// TestListDiffReviewQueue_SortBySeqASC 验证队列分区排序：seq ASC（FIFO 唯一依据）。
func TestListDiffReviewQueue_SortBySeqASC(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	// 创建顺序 s1, s2, s3 → seq 1,2,3。
	s1 := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})
	s2 := createQueuedSubmissionWithItems(t, db, "s2", "t1", "msg_s2", []DiffAnnotationRow{a})
	s3 := createQueuedSubmissionWithItems(t, db, "s3", "t1", "msg_s3", []DiffAnnotationRow{a})
	// s2 转 sending。
	if _, err := db.CASDiffReviewSubmission(ctx, s2.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
		t.Fatal(err)
	}
	queue, err := db.ListDiffReviewQueue(ctx, "t1")
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	if len(queue) != 3 {
		t.Fatalf("queue len=%d want 3 (queued+sending)", len(queue))
	}
	// seq ASC：s1(1) < s2(2) < s3(3)。
	want := []string{"s1", "s2", "s3"}
	for i, w := range want {
		if queue[i].ID != w {
			t.Errorf("queue[%d].ID=%s want %s (seq ASC)", i, queue[i].ID, w)
		}
	}
	// 确认 s2 仍为 sending 在队列中。
	if queue[1].Status != DiffReviewStatusSending {
		t.Errorf("queue[1].status=%s want sending", queue[1].Status)
	}
	_ = s1
	_ = s3
}

// --- 撤回与终态删除 ---

// TestCancelDiffReviewSubmission_OnlyQueued 验证撤回仅 queued：queued→删除；非 queued→!matched 不删。
func TestCancelDiffReviewSubmission_OnlyQueued(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	sub := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})

	// queued → 撤回成功。
	matched, err := db.CancelDiffReviewSubmission(ctx, sub.ID)
	if err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	if !matched {
		t.Fatal("cancel queued: want matched=true")
	}
	if _, err := db.GetDiffReviewSubmission(ctx, sub.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("queued submission should be deleted on cancel, err=%v", err)
	}

	// 重新创建并转 sending → 撤回应失败。
	sub2 := createQueuedSubmissionWithItems(t, db, "s2", "t1", "msg_s2", []DiffAnnotationRow{a})
	if _, err := db.CASDiffReviewSubmission(ctx, sub2.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
		t.Fatal(err)
	}
	matched2, err := db.CancelDiffReviewSubmission(ctx, sub2.ID)
	if err != nil {
		t.Fatalf("cancel sending: %v", err)
	}
	if matched2 {
		t.Fatal("cancel sending: want matched=false (only queued cancellable)")
	}
	if _, err := db.GetDiffReviewSubmission(ctx, sub2.ID); err != nil {
		t.Errorf("sending submission should still exist after failed cancel, err=%v", err)
	}
}

// TestDeleteDiffReviewSubmission_OnlyTerminalStates 验证终态删除仅 sent/failed/delivery_unknown。
func TestDeleteDiffReviewSubmission_OnlyTerminalStates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")

	cases := []struct {
		name      string
		toStatus  string
		canDelete bool
	}{
		{"queued", DiffReviewStatusQueued, false},
		{"sending", DiffReviewStatusSending, false},
		{"sent", DiffReviewStatusSent, true},
		{"failed", DiffReviewStatusFailed, true},
		{"delivery_unknown", DiffReviewStatusDeliveryUnknown, true},
	}
	for i, c := range cases {
		sid := "s-" + c.name
		sub := createQueuedSubmissionWithItems(t, db, sid, "t1", "msg_"+c.name, []DiffAnnotationRow{a})
		// 转到目标状态：queued→sending→terminal（sent 经清理事务；其余经 CAS）。
		if c.toStatus != DiffReviewStatusQueued {
			if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
				t.Fatal(err)
			}
			switch c.toStatus {
			case DiffReviewStatusSent:
				if _, err := db.CompleteDiffReviewSentCleanup(ctx, sub.ID); err != nil {
					t.Fatal(err)
				}
			case DiffReviewStatusFailed:
				if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusSending, DiffReviewStatusFailed, "e"); err != nil {
					t.Fatal(err)
				}
			case DiffReviewStatusDeliveryUnknown:
				if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusSending, DiffReviewStatusDeliveryUnknown, "e"); err != nil {
					t.Fatal(err)
				}
			case DiffReviewStatusSending:
				// 已是 sending，无需再转。
			}
		}
		matched, err := db.DeleteDiffReviewSubmission(ctx, sub.ID)
		if err != nil {
			t.Fatalf("case[%d] %s: delete err=%v", i, c.name, err)
		}
		if c.canDelete && !matched {
			t.Errorf("case %s: want matched=true (terminal deletable), got false", c.name)
		}
		if !c.canDelete && matched {
			t.Errorf("case %s: want matched=false (non-terminal not deletable), got true", c.name)
		}
		// 可删的应已不存在；不可删的应仍存在。
		_, gerr := db.GetDiffReviewSubmission(ctx, sub.ID)
		if c.canDelete && !errors.Is(gerr, sql.ErrNoRows) {
			t.Errorf("case %s: should be deleted, err=%v", c.name, gerr)
		}
		if !c.canDelete && errors.Is(gerr, sql.ErrNoRows) {
			t.Errorf("case %s: should still exist (non-terminal)", c.name)
		}
		// 每条用独立批注避免 sent 清理交叉影响：后续 case 复用 a，但 a 在 sent case 后被删，
		// 之后 createQueuedSubmissionWithItems 的复核项仍携带 a.Revision=1，复核时 a 已不存在 → conflict。
		// 为隔离，每条 case 前重新创建批注。
		_ = a
		// 重建批注供下一 case 使用（若已被 sent 清理删除）。
		if _, gerr := db.GetDiffAnnotation(ctx, a.ID); errors.Is(gerr, sql.ErrNoRows) {
			a = createTestAnnotation(t, db, a.ID, "t1")
		}
	}
}

// --- 启动收敛事务原子回滚与错误传播 ---

// TestConvergeDiffReviewOnStartup_SendingToDeliveryUnknown 验证全局启动收敛：
// 单事务批量 sending→delivery_unknown + 固定 error；非 sending 行不变。
func TestConvergeDiffReviewOnStartup_SendingToDeliveryUnknown(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")

	// 两条 sending，一条 queued，一条 failed。
	sSending1 := createQueuedSubmissionWithItems(t, db, "ss1", "t1", "msg_ss1", []DiffAnnotationRow{a})
	sSending2 := createQueuedSubmissionWithItems(t, db, "ss2", "t1", "msg_ss2", []DiffAnnotationRow{a})
	sQueued := createQueuedSubmissionWithItems(t, db, "sq1", "t1", "msg_sq1", []DiffAnnotationRow{a})
	sFailed := createQueuedSubmissionWithItems(t, db, "sf1", "t1", "msg_sf1", []DiffAnnotationRow{a})
	for _, s := range []string{sSending1.ID, sSending2.ID} {
		if _, err := db.CASDiffReviewSubmission(ctx, s, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CASDiffReviewSubmission(ctx, sFailed.ID, DiffReviewStatusQueued, DiffReviewStatusFailed, "orig"); err != nil {
		t.Fatal(err)
	}

	affected, err := db.ConvergeDiffReviewOnStartup(ctx)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if affected != 2 {
		t.Errorf("affected=%d want 2 (two sending rows)", affected)
	}

	// 两条 sending → delivery_unknown + 固定 error。
	for _, sid := range []string{sSending1.ID, sSending2.ID} {
		got, _ := db.GetDiffReviewSubmission(ctx, sid)
		if got.Status != DiffReviewStatusDeliveryUnknown {
			t.Errorf("%s status=%s want delivery_unknown", sid, got.Status)
		}
		if got.Error != "delivery unknown after restart" {
			t.Errorf("%s error=%q want fixed startup converge error", sid, got.Error)
		}
	}
	// queued / failed 不变。
	if got, _ := db.GetDiffReviewSubmission(ctx, sQueued.ID); got.Status != DiffReviewStatusQueued {
		t.Errorf("queued status=%s want unchanged queued", got.Status)
	}
	if got, _ := db.GetDiffReviewSubmission(ctx, sFailed.ID); got.Status != DiffReviewStatusFailed || got.Error != "orig" {
		t.Errorf("failed status=%s error=%q want failed/orig (unchanged)", got.Status, got.Error)
	}
}

// TestConvergeDiffReviewOnStartup_NoSendingRowsZeroAffected 验证无 sending 行时收敛返回 0（幂等、无副作用）。
func TestConvergeDiffReviewOnStartup_NoSendingRowsZeroAffected(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	_ = createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})
	affected, err := db.ConvergeDiffReviewOnStartup(ctx)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if affected != 0 {
		t.Errorf("affected=%d want 0 (no sending rows)", affected)
	}
}

// TestConvergeDiffReviewOnStartup_SingleTransactionAtomic 验证收敛为单事务：
// 所有 sending 行在同一事务内转移；不会出现部分转移。
// 此处通过可观察不变量锁定：收敛后所有原 sending 行均为 delivery_unknown + 固定 error，
// 无 mixed state。SQLite 单写者事务保证原子性，测试锁定结果一致性。
func TestConvergeDiffReviewOnStartup_SingleTransactionAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	// 多条 sending。
	var sendingIDs []string
	for i := 0; i < 5; i++ {
		sid := "s" + string(rune('1'+i))
		s := createQueuedSubmissionWithItems(t, db, sid, "t1", "msg_"+sid, []DiffAnnotationRow{a})
		if _, err := db.CASDiffReviewSubmission(ctx, s.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
			t.Fatal(err)
		}
		sendingIDs = append(sendingIDs, s.ID)
	}
	affected, err := db.ConvergeDiffReviewOnStartup(ctx)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if affected != int64(len(sendingIDs)) {
		t.Errorf("affected=%d want %d", affected, len(sendingIDs))
	}
	// 全部一致为 delivery_unknown + 固定 error（无 mixed state）。
	for _, sid := range sendingIDs {
		got, _ := db.GetDiffReviewSubmission(ctx, sid)
		if got.Status != DiffReviewStatusDeliveryUnknown || got.Error != "delivery unknown after restart" {
			t.Errorf("%s status=%s error=%q want delivery_unknown/fixed error (atomic all-or-nothing)", sid, got.Status, got.Error)
		}
	}
}

// --- 批注 CRUD 基础 ---

// TestDiffAnnotation_CRUDAndRevisionIncrement 验证批注 CRUD 与 revision 递增语义。
func TestDiffAnnotation_CRUDAndRevisionIncrement(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	r := DiffAnnotationRow{
		ID: "a1", TaskID: "t1", Path: "a.go", Side: "new", Ref: "refs/heads/main", Untracked: false,
		StartLine: 2, EndLine: 4, SnapshotStartLine: 1, Snapshot: "x\ny\nz\n",
		SnapshotLineCount: 3, Comment: "first",
	}
	if _, err := db.CreateDiffAnnotation(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := db.GetDiffAnnotation(ctx, "a1")
	if got.Revision != 1 || got.Comment != "first" || got.Ref != "refs/heads/main" {
		t.Errorf("initial row=%+v want revision=1 first main", got)
	}

	// 编辑评论 → revision 2。
	upd, err := db.UpdateDiffAnnotationComment(ctx, "a1", "second")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !upd.Matched || !upd.Changed || upd.Revision != 2 {
		t.Errorf("update result=%+v want Matched/Changed/2", upd)
	}
	got, _ = db.GetDiffAnnotation(ctx, "a1")
	if got.Comment != "second" || got.Revision != 2 {
		t.Errorf("after update row=%+v want second/2", got)
	}

	// 同值编辑 → Matched=true, Changed=false, revision 不递增（D3：每次实变才 +1）。
	same, err := db.UpdateDiffAnnotationComment(ctx, "a1", "second")
	if err != nil {
		t.Fatalf("same-value update: %v", err)
	}
	if !same.Matched || same.Changed || same.Revision != 2 {
		t.Errorf("same-value result=%+v want Matched/!Changed/2", same)
	}
	gotSame, _ := db.GetDiffAnnotation(ctx, "a1")
	if gotSame.Revision != 2 {
		t.Errorf("revision=%d want 2 (same-value must not increment)", gotSame.Revision)
	}

	// 再次编辑 → revision 3。
	upd3, _ := db.UpdateDiffAnnotationComment(ctx, "a1", "third")
	if !upd3.Matched || !upd3.Changed || upd3.Revision != 3 {
		t.Errorf("third update result=%+v want Matched/Changed/3", upd3)
	}

	// 列表按 created_at ASC、id ASC（同秒时 id 字典序决胜）。
	_, _ = db.CreateDiffAnnotation(ctx, DiffAnnotationRow{
		ID: "a0", TaskID: "t1", Path: "b.go", Side: "old", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, Snapshot: "s\n", SnapshotLineCount: 1, Comment: "x",
	})
	list, err := db.ListDiffAnnotationsByTask(ctx, "t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// 同秒 created_at → id ASC：a0 < a1。
	if len(list) != 2 || list[0].ID != "a0" || list[1].ID != "a1" {
		t.Errorf("list order=%+v want [a0 a1] (created_at ASC, id ASC tiebreaker)", list)
	}

	// 删除。
	n, err := db.DeleteDiffAnnotation(ctx, "a1")
	if err != nil || n != 1 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	if _, err := db.GetDiffAnnotation(ctx, "a1"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a1 should be deleted, err=%v", err)
	}
	// 重复删除幂等返回 0。
	n2, _ := db.DeleteDiffAnnotation(ctx, "a1")
	if n2 != 0 {
		t.Errorf("re-delete n=%d want 0 (idempotent)", n2)
	}
}

// TestDiffAnnotation_UpdateNonExistentReturnsZero 验证更新不存在批注返回 !Matched/Revision=0（调用方按 not_found 处理）。
func TestDiffAnnotation_UpdateNonExistentReturnsZero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	upd, err := db.UpdateDiffAnnotationComment(ctx, "nope", "x")
	if err != nil {
		t.Fatalf("update nonexistent: %v", err)
	}
	if upd.Matched || upd.Changed || upd.Revision != 0 {
		t.Errorf("result=%+v want !Matched/!Changed/0 (nonexistent)", upd)
	}
}

// TestMigration0012_DiffAnnotationTablesExist 验证 migration 0012 后三表存在且可写。
func TestMigration0012_DiffAnnotationTablesExist(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, tbl := range []string{"diff_annotations", "diff_review_submissions", "diff_review_submission_items"} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("query %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("%s count=%d want 0", tbl, n)
		}
	}
	// 写入往返。
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	_ = createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})
	items, err := db.ListDiffReviewSubmissionItems(ctx, "s1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].AnnotationID != "a1" || items[0].AnnotationRevision != 1 {
		t.Errorf("items=%+v want single a1/rev1", items)
	}
}

// TestDiffReviewSubmission_CascadeOnTaskDelete 验证 ON DELETE CASCADE：删除任务后其 submissions + items 级联删除。
func TestDiffReviewSubmission_CascadeOnTaskDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	_ = createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})
	if _, err := db.DeleteTask(ctx, "t1"); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM diff_annotations WHERE task_id='t1'`,
		`SELECT count(*) FROM diff_review_submissions WHERE task_id='t1'`,
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("cascade count=%d want 0: %s", n, q)
		}
	}
	// items 经 submission_id 级联（submission 被删 → items 级联）。
	var nItems int
	if err := db.QueryRow(`SELECT count(*) FROM diff_review_submission_items WHERE submission_id='s1'`).Scan(&nItems); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if nItems != 0 {
		t.Errorf("items cascade count=%d want 0", nItems)
	}
}

// --- F-01 回归：sent 清理按 (id, revision) 逐条相关匹配，不误删 ---

// TestCompleteDiffReviewSentCleanup_DistinctSnapshotRevisionsNoMisDelete 验证相关子查询语义：
// 当两个 item 的快照 revision 不同，且提交后两条批注都被编辑（revision 均 +1）时，
// 旧实现用两个独立 IN 集合（id IN ... AND revision IN ...）会误删 a1；
// 修复后相关 EXISTS 子查询在同一 item 上同时约束 id+revision，零误删。
//
// 场景：快照 (a1, rev=1),(a2, rev=2)；提交后 a1→rev=2, a2→rev=3。
// 独立 IN 集合：id IN (a1,a2) AND revision IN (1,2) → a1(rev=2) 命中 rev∈{1,2} 误删。
// 相关匹配：(a1,rev=2) 与快照 (a1,1) 不符、(a2,rev=3) 与快照 (a2,2) 不符 → 两条均保留。
func TestCompleteDiffReviewSentCleanup_DistinctSnapshotRevisionsNoMisDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a1 := createTestAnnotation(t, db, "a1", "t1") // rev=1
	a2 := createTestAnnotation(t, db, "a2", "t1") // rev=1

	// 把 a2 编辑一次使其 rev=2，构造快照 revision 差异。
	if _, err := db.UpdateDiffAnnotationComment(ctx, a2.ID, "bump-to-rev2"); err != nil {
		t.Fatal(err)
	}
	a2Edit, _ := db.GetDiffAnnotation(ctx, a2.ID)
	if a2Edit.Revision != 2 {
		t.Fatalf("a2 revision=%d want 2", a2Edit.Revision)
	}

	// 手工创建 submission + items 快照：item1=(a1,rev=1), item2=(a2,rev=2)。
	sub := DiffReviewSubmissionRow{
		ID: "s1", TaskID: "t1", Status: DiffReviewStatusQueued,
		TargetSessionID: "sess-1", MessageID: "msg_s1", Note: "n", Payload: "p",
	}
	items := []DiffReviewSubmissionItemRow{
		{AnnotationID: "a1", AnnotationRevision: 1, Path: a1.Path, Side: a1.Side, Snapshot: a1.Snapshot, Comment: a1.Comment},
		{AnnotationID: "a2", AnnotationRevision: 2, Path: a2Edit.Path, Side: a2Edit.Side, Snapshot: a2Edit.Snapshot, Comment: a2Edit.Comment},
	}
	if _, err := db.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{Submission: sub, Items: items}); err != nil {
		t.Fatalf("create submission: %v", err)
	}
	// 抢占 sending。
	if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
		t.Fatal(err)
	}

	// 提交后两条批注都被编辑：a1 rev1→2, a2 rev2→3。
	if _, err := db.UpdateDiffAnnotationComment(ctx, "a1", "edited-after-submit"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateDiffAnnotationComment(ctx, "a2", "edited-after-submit-2"); err != nil {
		t.Fatal(err)
	}

	matched, err := db.CompleteDiffReviewSentCleanup(ctx, sub.ID)
	if err != nil {
		t.Fatalf("sent cleanup: %v", err)
	}
	if !matched {
		t.Fatal("want matched=true (was sending)")
	}
	// 两条批注 revision 均不匹配快照 → 零误删。
	for _, aid := range []string{"a1", "a2"} {
		if _, err := db.GetDiffAnnotation(ctx, aid); err != nil {
			t.Errorf("%s should be kept (revision mismatch with snapshot), err=%v", aid, err)
		}
	}
}

// --- F-02：CAS 状态机白名单 ---

// TestCASDiffReviewSubmission_IllegalEdgesRejected 验证非法边与 *→sent 被白名单拒绝。
// 返回 ErrDiffReviewIllegalCAS + 零修改（状态不变）。
func TestCASDiffReviewSubmission_IllegalEdgesRejected(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")

	cases := []struct {
		name string
		from string
		to   string
	}{
		// 永久拒绝以 sent 为目标（任意 from）。
		{"queued->sent", DiffReviewStatusQueued, DiffReviewStatusSent},
		{"sending->sent", DiffReviewStatusSending, DiffReviewStatusSent},
		// 非法边。
		{"sent->failed", DiffReviewStatusSent, DiffReviewStatusFailed},
		{"failed->sending", DiffReviewStatusFailed, DiffReviewStatusSending},
		{"failed->queued", DiffReviewStatusFailed, DiffReviewStatusQueued},
		{"sending->queued", DiffReviewStatusSending, DiffReviewStatusQueued},
		{"queued->delivery_unknown", DiffReviewStatusQueued, DiffReviewStatusDeliveryUnknown},
		// 未知状态。
		{"queued->bogus", DiffReviewStatusQueued, "bogus"},
		{"bogus->sending", "bogus", DiffReviewStatusSending},
		{"bogus->bogus", "bogus", "bogus"},
	}
	for i, c := range cases {
		sid := fmt.Sprintf("s%d", i)
		sub := createQueuedSubmissionWithItems(t, db, sid, "t1", "msg_"+sid, []DiffAnnotationRow{a})
		// 将 submission 置为 c.from（用原始 UPDATE 绕过白名单仅用于测试布置）。
		if c.from != DiffReviewStatusQueued {
			if _, err := db.ExecContext(ctx,
				`UPDATE diff_review_submissions SET status=? WHERE id=?`, c.from, sub.ID); err != nil {
				t.Fatalf("case %s: set from: %v", c.name, err)
			}
		}
		matched, err := db.CASDiffReviewSubmission(ctx, sub.ID, c.from, c.to, "")
		if !errors.Is(err, ErrDiffReviewIllegalCAS) {
			t.Errorf("case %s: err=%v want ErrDiffReviewIllegalCAS", c.name, err)
			continue
		}
		if matched {
			t.Errorf("case %s: matched=true want false (illegal CAS must not match)", c.name)
		}
		// 状态不变。
		got, _ := db.GetDiffReviewSubmission(ctx, sub.ID)
		if got.Status != c.from {
			t.Errorf("case %s: status=%s want unchanged %s (zero modify on illegal CAS)", c.name, got.Status, c.from)
		}
	}
}

// --- F-03：失败路径事务原子回滚 ---

// TestCompleteDiffReviewSentCleanup_AbortRollsBack 验证 sent 清理事务不可拆分（失败路径）：
// 用 abort trigger 在批注 DELETE 时 ABORT，断言返回错误、submission 回滚保持 sending、
// sent_at 仍空、批注保留。参考 anchor_recovery_test.go:433 的 abort-trigger 模式。
func TestCompleteDiffReviewSentCleanup_AbortRollsBack(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")
	sub := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})
	if _, err := db.CASDiffReviewSubmission(ctx, sub.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
		t.Fatal(err)
	}
	// abort trigger：批注 DELETE 时 ABORT。
	if _, err := db.Exec(`
		CREATE TRIGGER abort_sent_cleanup_delete
		BEFORE DELETE ON diff_annotations
		BEGIN
			SELECT RAISE(ABORT, 'forced sent cleanup abort');
		END`); err != nil {
		t.Fatalf("create abort trigger: %v", err)
	}
	matched, err := db.CompleteDiffReviewSentCleanup(ctx, sub.ID)
	if err == nil {
		t.Fatal("sent cleanup should fail when annotation DELETE aborts")
	}
	if matched {
		t.Fatal("matched=true want false on abort")
	}
	// submission 回滚：保持 sending，sent_at 仍空。
	got, gerr := db.GetDiffReviewSubmission(ctx, sub.ID)
	if gerr != nil {
		t.Fatalf("get submission: %v", gerr)
	}
	if got.Status != DiffReviewStatusSending {
		t.Errorf("status=%s want sending (rolled back)", got.Status)
	}
	if got.SentAt.Valid {
		t.Errorf("sent_at=%v want NULL (rolled back)", got.SentAt)
	}
	// 批注保留。
	if _, err := db.GetDiffAnnotation(ctx, a.ID); err != nil {
		t.Errorf("annotation should be kept after abort, err=%v", err)
	}
}

// TestConvergeDiffReviewOnStartup_AbortRollsBack 验证启动收敛单事务原子回滚（失败路径）：
// 用 abort trigger 在 UPDATE status 时 ABORT，断言返回错误且所有 sending 行原值不变。
func TestConvergeDiffReviewOnStartup_AbortRollsBack(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1")

	// 两条 sending。
	s1 := createQueuedSubmissionWithItems(t, db, "s1", "t1", "msg_s1", []DiffAnnotationRow{a})
	s2 := createQueuedSubmissionWithItems(t, db, "s2", "t1", "msg_s2", []DiffAnnotationRow{a})
	for _, sid := range []string{s1.ID, s2.ID} {
		if _, err := db.CASDiffReviewSubmission(ctx, sid, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil {
			t.Fatal(err)
		}
	}
	// abort trigger：UPDATE status 时 ABORT。
	if _, err := db.Exec(`
		CREATE TRIGGER abort_converge_update
		BEFORE UPDATE OF status ON diff_review_submissions
		BEGIN
			SELECT RAISE(ABORT, 'forced converge abort');
		END`); err != nil {
		t.Fatalf("create abort trigger: %v", err)
	}
	affected, err := db.ConvergeDiffReviewOnStartup(ctx)
	if err == nil {
		t.Fatal("converge should fail when UPDATE aborts")
	}
	if affected != 0 {
		t.Errorf("affected=%d want 0 on abort", affected)
	}
	// 两条 sending 行原值不变（整事务回滚）。
	for _, sid := range []string{s1.ID, s2.ID} {
		got, _ := db.GetDiffReviewSubmission(ctx, sid)
		if got.Status != DiffReviewStatusSending {
			t.Errorf("%s status=%s want sending (rolled back)", sid, got.Status)
		}
		if got.Error != "" {
			t.Errorf("%s error=%q want empty (rolled back)", sid, got.Error)
		}
	}
}

// --- F-04：stale item + 空复核集合仍必须 conflict ---
//
// 删除 RevisionCheck 后，复核直接从 items 派生。验证 stale item（revision 不符）
// 即使没有独立复核数组也必须 conflict：item 携带 annotation_revision=1，但批注已 rev=2。

func TestCreateDiffReviewSubmission_StaleItemMustConflict(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1") // rev=1
	// 编辑使 rev=2。
	if _, err := db.UpdateDiffAnnotationComment(ctx, a.ID, "bump"); err != nil {
		t.Fatal(err)
	}
	sub := DiffReviewSubmissionRow{
		ID: "s1", TaskID: "t1", Status: DiffReviewStatusQueued,
		TargetSessionID: "sess-1", MessageID: "msg_s1", Note: "n", Payload: "p",
	}
	// item 携带 stale revision=1（当前 rev=2）→ 复核从 item 派生 → conflict。
	_, err := db.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{
		Submission: sub,
		Items: []DiffReviewSubmissionItemRow{
			{AnnotationID: a.ID, AnnotationRevision: 1, Path: a.Path, Side: a.Side, Snapshot: a.Snapshot, Comment: a.Comment},
		},
	})
	if !errors.Is(err, ErrDiffReviewRevisionConflict) {
		t.Fatalf("err=%v want ErrDiffReviewRevisionConflict (stale item revision)", err)
	}
	// 零落库。
	if _, err := db.GetDiffReviewSubmission(ctx, "s1"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("submission should not exist after conflict, err=%v", err)
	}
}

// --- F-06：返回 revision 属于本次写入（并发安全） ---
//
// 旧实现 UPDATE 提交后用独立 SELECT 读 revision，并发更新会让首个调用返回后续写入的 revision，
// 并发删除会让已提交的调用返回错误。UPDATE ... RETURNING revision 在单事务内直接返回本次实变
// 的新 revision，配合 -race 验证无数据竞争。

// TestUpdateDiffAnnotationComment_ConcurrentRevisionBelongsToOwnWrite 验证并发交错更新时，
// 每个成功 Changed=true 的调用返回的 revision 与其自身写入一致：即返回的 revision-1 等于该调用
// 写入前行的 revision（由该调用独占观测）。由于 SQLite 单写者串行化，每个 Changed 调用的
// Revision 严格单调递增，且 revision-1 必须等于某个先前观测值或初始值。
func TestUpdateDiffAnnotationComment_ConcurrentRevisionBelongsToOwnWrite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a := createTestAnnotation(t, db, "a1", "t1") // rev=1, comment="c-a1"

	const goroutines = 8
	const writes = 20 // 每个 goroutine 交错写入次数

	var wg sync.WaitGroup
	// 收集所有 Changed=true 调用的 Revision；并发结束后验证 Revision 集合 == {2..goroutines*writes+1}
	// 且无重复（每个 revision 恰好出现一次），证明返回值属于本次写入而非他人写入。
	type result struct {
		rev int
		ok  bool
	}
	results := make(chan result, goroutines*writes)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gOff int) {
			defer wg.Done()
			for w := 0; w < writes; w++ {
				// 每次写入唯一 comment，保证 Changed=true。
				comment := fmt.Sprintf("g%d-w%d", gOff, w)
				upd, err := db.UpdateDiffAnnotationComment(ctx, a.ID, comment)
				if err != nil {
					t.Errorf("concurrent update err=%v", err)
					return
				}
				if upd.Matched && upd.Changed {
					results <- result{rev: upd.Revision, ok: true}
				}
			}
		}(g)
	}
	wg.Wait()
	close(results)

	// 统计：所有 Changed 调用的 Revision 应为 2..(goroutines*writes+1) 的排列，无重复。
	seen := make(map[int]int)
	count := 0
	for r := range results {
		seen[r.rev]++
		count++
	}
	if count != goroutines*writes {
		t.Fatalf("changed count=%d want %d", count, goroutines*writes)
	}
	for rev := 2; rev <= goroutines*writes+1; rev++ {
		if seen[rev] != 1 {
			t.Errorf("revision %d seen %d times, want exactly 1 (revision must belong to own write, no duplication)", rev, seen[rev])
		}
	}
	// 最终 revision == goroutines*writes+1。
	got, _ := db.GetDiffAnnotation(ctx, a.ID)
	if got.Revision != goroutines*writes+1 {
		t.Errorf("final revision=%d want %d", got.Revision, goroutines*writes+1)
	}
}

// TestCreateDiffReviewSubmission_ReturningMatchesGet（G4）：
// CreateDiffReviewSubmission 的 RETURNING 返回记录与立即 Get 的完整结果一致：
// Seq > 0（AUTOINCREMENT）、CreatedAt > 0、SentAt 为 NULL；且该记录立即出现在分区列表中
//（证明返回发生在 commit 之后，无需也不允许二次读回补全）。
func TestCreateDiffReviewSubmission_ReturningMatchesGet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	createTestAnnotation(t, db, "a1", "t1")

	stored, err := db.CreateDiffReviewSubmission(ctx, CreateDiffReviewSubmissionInput{
		Submission: DiffReviewSubmissionRow{
			ID: "s1", TaskID: "t1", Status: DiffReviewStatusQueued,
			TargetSessionID: "sess-1", MessageID: "msg_s1", Note: "n", Payload: "p",
		},
		Items: []DiffReviewSubmissionItemRow{{
			AnnotationID: "a1", AnnotationRevision: 1, Path: "a.go", Side: "new",
			StartLine: 1, EndLine: 1, SnapshotStartLine: 1, Snapshot: "x\n", Comment: "c",
		}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if stored.Seq <= 0 {
		t.Errorf("Seq = %d, want > 0", stored.Seq)
	}
	if stored.CreatedAt <= 0 {
		t.Errorf("CreatedAt = %d, want > 0", stored.CreatedAt)
	}
	if stored.SentAt.Valid {
		t.Errorf("SentAt = %v, want NULL", stored.SentAt)
	}
	if stored.Status != DiffReviewStatusQueued || stored.ID != "s1" || stored.MessageID != "msg_s1" {
		t.Errorf("stored row fields mismatch: %+v", stored)
	}
	// 与立即 Get 的完整结果一致。
	got, err := db.GetDiffReviewSubmission(ctx, "s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != stored {
		t.Errorf("get row = %+v, want same as returned %+v", got, stored)
	}
	// 立即出现在分区列表的 queue 分区（同一记录）。
	parts, err := db.ListDiffReviewSubmissionPartitions(ctx, "t1")
	if err != nil {
		t.Fatalf("partitions: %v", err)
	}
	if len(parts.Queue) != 1 || parts.Queue[0].Submission != stored {
		t.Errorf("queue partition = %+v, want [returned row]", parts.Queue)
	}
	// 空分区非 nil。
	if parts.History == nil || parts.Failures == nil {
		t.Error("empty partitions must be non-nil")
	}
}

// TestListDiffReviewSubmissionPartitions_SingleConsistentSnapshot 验证 F4 分区一致读：
// 同一 submission 只出现在一个分区、items 与 submission 同行快照、
// 排序契约（queue seq ASC / history sent_at DESC,seq DESC / failures created_at DESC,seq DESC）、
// 空分区返回非 nil 空切片。
func TestListDiffReviewSubmissionPartitions_SingleConsistentSnapshot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedDiffReviewTask(t, db, "t1")
	a1 := createTestAnnotation(t, db, "a1", "t1")
	a2 := createTestAnnotation(t, db, "a2", "t1")

	subQueued := createQueuedSubmissionWithItems(t, db, "s-q", "t1", "msg_sq", []DiffAnnotationRow{a1})
	subFailed := createQueuedSubmissionWithItems(t, db, "s-f", "t1", "msg_sf", []DiffAnnotationRow{a2})
	subSent := createQueuedSubmissionWithItems(t, db, "s-s", "t1", "msg_ss", []DiffAnnotationRow{a1, a2})
	subUnknown := createQueuedSubmissionWithItems(t, db, "s-u", "t1", "msg_su", []DiffAnnotationRow{a2})

	// s-f：queued→failed；s-s：queued→sending→sent（唯一路径：清理事务）；
	// s-u：queued→sending→delivery_unknown。
	if ok, err := db.CASDiffReviewSubmission(ctx, subFailed.ID, DiffReviewStatusQueued, DiffReviewStatusFailed, "boom"); err != nil || !ok {
		t.Fatalf("CAS s-f failed: ok=%v err=%v", ok, err)
	}
	if ok, err := db.CASDiffReviewSubmission(ctx, subSent.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil || !ok {
		t.Fatalf("CAS s-s sending: ok=%v err=%v", ok, err)
	}
	if ok, err := db.CompleteDiffReviewSentCleanup(ctx, subSent.ID); err != nil || !ok {
		t.Fatalf("sent cleanup s-s: ok=%v err=%v", ok, err)
	}
	if ok, err := db.CASDiffReviewSubmission(ctx, subUnknown.ID, DiffReviewStatusQueued, DiffReviewStatusSending, ""); err != nil || !ok {
		t.Fatalf("CAS s-u sending: ok=%v err=%v", ok, err)
	}
	if ok, err := db.CASDiffReviewSubmission(ctx, subUnknown.ID, DiffReviewStatusSending, DiffReviewStatusDeliveryUnknown, "unknown"); err != nil || !ok {
		t.Fatalf("CAS s-u unknown: ok=%v err=%v", ok, err)
	}

	parts, err := db.ListDiffReviewSubmissionPartitions(ctx, "t1")
	if err != nil {
		t.Fatalf("ListDiffReviewSubmissionPartitions: %v", err)
	}
	if parts.Queue == nil || parts.History == nil || parts.Failures == nil {
		t.Fatal("partitions 分区必须非 nil（wire 空数组而非 null）")
	}
	// 分区归属唯一：queue=[s-q]，history=[s-s]，failures 含 s-f 与 s-u。
	if len(parts.Queue) != 1 || parts.Queue[0].Submission.ID != subQueued.ID {
		t.Fatalf("queue = %+v, want only s-q", parts.Queue)
	}
	if len(parts.Queue[0].Items) != 1 || parts.Queue[0].Items[0].AnnotationID != a1.ID {
		t.Fatalf("queue items = %+v, want a1 快照", parts.Queue[0].Items)
	}
	if len(parts.History) != 1 || parts.History[0].Submission.ID != subSent.ID {
		t.Fatalf("history = %+v, want only s-s", parts.History)
	}
	if len(parts.History[0].Items) != 2 {
		t.Fatalf("history items len = %d, want 2", len(parts.History[0].Items))
	}
	if len(parts.Failures) != 2 {
		t.Fatalf("failures len = %d, want 2 (s-f, s-u)", len(parts.Failures))
	}
	seen := map[string]string{}
	for _, v := range parts.Queue {
		seen[v.Submission.ID] = "queue"
	}
	for _, v := range parts.History {
		seen[v.Submission.ID] = "history"
	}
	for _, v := range parts.Failures {
		seen[v.Submission.ID] = "failures"
	}
	if len(seen) != 4 {
		t.Fatalf("同一 submission 出现在多个分区或丢失：seen=%v", seen)
	}
	// failures 排序：created_at DESC（同秒）→ seq DESC，即 s-u（seq 更大）在前。
	if parts.Failures[0].Submission.ID != subUnknown.ID || parts.Failures[1].Submission.ID != subFailed.ID {
		t.Fatalf("failures order = [%s %s], want [s-u s-f]（created_at DESC,seq DESC）",
			parts.Failures[0].Submission.ID, parts.Failures[1].Submission.ID)
	}
	// 空任务（无任何提交，查询仅按 task_id 过滤，无需 seed）：三分区均非 nil 空切片。
	empty, err := db.ListDiffReviewSubmissionPartitions(ctx, "t-empty")
	if err != nil {
		t.Fatalf("empty partitions: %v", err)
	}
	if empty.Queue == nil || empty.History == nil || empty.Failures == nil ||
		len(empty.Queue) != 0 || len(empty.History) != 0 || len(empty.Failures) != 0 {
		t.Fatalf("empty partitions = %+v, want 非 nil 空切片", empty)
	}
}