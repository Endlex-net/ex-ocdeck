// submission_test.go 测试 3.6 提交用例（准入/批次id分类优先级/payload/落库/messageID/撤回/删除）。
package diffreview

import (
	"context"
	"strings"
	"testing"
)

// newSubmissionTestService 构造带完整 mock 的 service，runtime=supported+running+anchored。
func newSubmissionTestService(repo *mockRepo, diff *mockDiff, prompt *mockPrompt) *Service {
	rt := &mockRuntime{
		snap:     RuntimeSnapshot{HasRuntime: true, HasAnchorSession: true, AnchorSessionID: "sess1", InstVersion: "v1"},
		probeCap: CapabilitySupported,
		sessStat: SessionStatusIdle,
	}
	return newTestService(repo, &mockScope{found: true, kind: "repo"}, diff, rt, prompt)
}

// TestCreateSubmissionEmptyRejected 验证空批注列表拒绝。
func TestCreateSubmissionEmptyRejected(t *testing.T) {
	svc := newSubmissionTestService(newMockRepo(), &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID: "t1", Annotations: nil,
	})
	if err != ErrEmptySubmission {
		t.Errorf("empty submission should be rejected, got %v", err)
	}
}

// TestCreateSubmissionDuplicateIDRejected 验证重复 id 拒绝。
func TestCreateSubmissionDuplicateIDRejected(t *testing.T) {
	svc := newSubmissionTestService(newMockRepo(), &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID: "t1",
		Annotations: []SubmissionItemRequest{
			{ID: "a1", Revision: 1},
			{ID: "a1", Revision: 1},
		},
	})
	if err != ErrDuplicateAnnotationID {
		t.Errorf("duplicate id should be rejected, got %v", err)
	}
}

// TestCreateSubmissionInvalidRevisionRejected 验证非法 revision 拒绝。
func TestCreateSubmissionInvalidRevisionRejected(t *testing.T) {
	svc := newSubmissionTestService(newMockRepo(), &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 0}},
	})
	if err != ErrInvalidAnnotationRevision {
		t.Errorf("revision 0 should be rejected, got %v", err)
	}
}

// TestCreateSubmissionTaskNotRunning 验证任务未运行拒绝。
func TestCreateSubmissionTaskNotRunning(t *testing.T) {
	repo := newMockRepo()
	rt := &mockRuntime{snap: RuntimeSnapshot{HasRuntime: false}, probeCap: CapabilityAbsent}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, rt, &mockPrompt{})
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
	})
	if err != ErrTaskNotRunning {
		t.Errorf("task not running should be rejected, got %v", err)
	}
}

// TestCreateSubmissionNoAnchorSession 验证无锚定会话拒绝。
func TestCreateSubmissionNoAnchorSession(t *testing.T) {
	repo := newMockRepo()
	rt := &mockRuntime{snap: RuntimeSnapshot{HasRuntime: true, HasAnchorSession: false}, probeCap: CapabilitySupported}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, rt, &mockPrompt{})
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
	})
	if err != ErrNoAnchorSession {
		t.Errorf("no anchor session should be rejected, got %v", err)
	}
}

// TestCreateSubmissionCapabilityNotSupported 验证能力非 supported 拒绝。
func TestCreateSubmissionCapabilityNotSupported(t *testing.T) {
	repo := newMockRepo()
	rt := &mockRuntime{snap: RuntimeSnapshot{HasRuntime: true, HasAnchorSession: true, AnchorSessionID: "s1"}, probeCap: CapabilityUnsupported}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, rt, &mockPrompt{})
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
	})
	if err != ErrCapabilityNotReady {
		t.Errorf("capability not supported should be rejected, got %v", err)
	}
}

// TestCreateSubmissionRevisionConflict 验证 revision 不符 → conflict。
func TestCreateSubmissionRevisionConflict(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{ID: "a1", TaskID: "t1", Revision: 2}
	svc := newSubmissionTestService(repo, &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
	})
	if err != ErrRevisionConflict {
		t.Errorf("revision mismatch should be conflict, got %v", err)
	}
}

// TestCreateSubmissionMissingAnnotation 验证本任务范围内缺失 → conflict。
func TestCreateSubmissionMissingAnnotation(t *testing.T) {
	repo := newMockRepo()
	svc := newSubmissionTestService(repo, &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "missing", Revision: 1}},
	})
	if err != ErrRevisionConflict {
		t.Errorf("missing annotation should be conflict, got %v", err)
	}
}

// TestCreateSubmissionCrossTaskAnnotation 验证跨任务 id → invalid_input。
// mockRepo.GetDiffAnnotation 对不存在 id 返回 errNotFound → classifyBatchIDs 跳过（视为本任务缺失）。
// 要测跨任务，需 mock 返回其他任务批注。用自定义 repo。
func TestCreateSubmissionCrossTaskAnnotation(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{ID: "a1", TaskID: "other-task", Revision: 1}
	svc := newSubmissionTestService(repo, &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	_, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
	})
	if err != ErrCrossTaskAnnotation {
		t.Errorf("cross-task annotation should be invalid_input, got %v", err)
	}
}

// TestCreateSubmissionSuccess 验证成功落库 + messageID 格式。
func TestCreateSubmissionSuccess(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", Revision: 1, CreatedAt: 100,
	}
	svc := newSubmissionTestService(repo, &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	sub, items, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
		Note:        "please fix",
	})
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if sub.Status != "queued" {
		t.Errorf("status should be queued, got %s", sub.Status)
	}
	if !strings.HasPrefix(sub.MessageID, "msg_") {
		t.Errorf("messageID should start with msg_, got %s", sub.MessageID)
	}
	if strings.Contains(sub.MessageID, "-") {
		t.Errorf("messageID should have hyphens removed, got %s", sub.MessageID)
	}
	if len(items) != 1 || items[0].AnnotationID != "a1" {
		t.Errorf("items should contain a1 snapshot")
	}
	if sub.Payload == "" {
		t.Errorf("payload should be assembled")
	}
}

// TestCreateSubmissionNoReadBackAfterCreate（G4）：创建成功后 service MUST NOT 再读回——
// GetDiffReviewSubmission 注入必然失败且计数必须为 0；创建仍成功且返回记录含 seq/createdAt。
// （G1 窗口：写成功但读回失败会让客户端把已存在的 queued submission 当作创建失败并重试，
// 产生第二条 submission/messageID 重复投递。）
func TestCreateSubmissionNoReadBackAfterCreate(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x\n", Comment: "c", Revision: 1, CreatedAt: 100,
	}
	repo.getSubmissionErr = errInjected // 任何 Get 必然失败：若 service 读回，创建必然报错。
	svc := newSubmissionTestService(repo, &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	sub, _, err := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID:      "t1",
		Annotations: []SubmissionItemRequest{{ID: "a1", Revision: 1}},
	})
	if err != nil {
		t.Fatalf("CreateSubmission must succeed without read-back: %v", err)
	}
	if repo.getSubmissionCalls != 0 {
		t.Fatalf("GetDiffReviewSubmission must not be called after create, got %d", repo.getSubmissionCalls)
	}
	if sub.Seq <= 0 || sub.CreatedAt <= 0 {
		t.Errorf("returned record must carry seq/createdAt from RETURNING: %+v", sub)
	}
	if sub.SentAt != 0 {
		t.Errorf("SentAt must be zero for queued, got %d", sub.SentAt)
	}
}

// TestCreateSubmissionBatchPriorityCrossTaskOverMissing 验证批次混合错误优先级：
// 跨任务 id + 缺失 id 同批，恒 invalid_input（与数组顺序无关）。
func TestCreateSubmissionBatchPriorityCrossTaskOverMissing(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["cross"] = DiffAnnotationRecord{ID: "cross", TaskID: "other", Revision: 1}
	svc := newSubmissionTestService(repo, &mockDiff{result: DiffSourceResult{NewContent: "x\n", NewExists: true, NewMode: "100644"}}, &mockPrompt{})
	// 顺序1: cross-task first, then missing
	_, _, err1 := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID: "t1",
		Annotations: []SubmissionItemRequest{
			{ID: "cross", Revision: 1},
			{ID: "missing", Revision: 1},
		},
	})
	if err1 != ErrCrossTaskAnnotation {
		t.Errorf("batch with cross-task first should be invalid_input, got %v", err1)
	}
	// 顺序2: missing first, then cross-task
	_, _, err2 := svc.CreateSubmission(context.Background(), CreateSubmissionRequest{
		TaskID: "t1",
		Annotations: []SubmissionItemRequest{
			{ID: "missing", Revision: 1},
			{ID: "cross", Revision: 1},
		},
	})
	if err2 != ErrCrossTaskAnnotation {
		t.Errorf("batch with missing first should still be invalid_input (order-independent), got %v", err2)
	}
}

// TestCancelSubmissionQueued 验证撤回 queued 提交。
func TestCancelSubmissionQueued(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued"}
	svc := newSubmissionTestService(repo, &mockDiff{}, &mockPrompt{})
	if err := svc.CancelSubmission(context.Background(), "t1", "s1"); err != nil {
		t.Errorf("cancel queued should succeed, got %v", err)
	}
}

// TestCancelSubmissionNonQueued 验证撤回非 queued → invalid_state。
func TestCancelSubmissionNonQueued(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "sent"}
	svc := newSubmissionTestService(repo, &mockDiff{}, &mockPrompt{})
	if err := svc.CancelSubmission(context.Background(), "t1", "s1"); err != ErrInvalidState {
		t.Errorf("cancel non-queued should be invalid_state, got %v", err)
	}
}

// TestDeleteSubmissionTerminal 验证删除终态提交。
func TestDeleteSubmissionTerminal(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "sent"}
	svc := newSubmissionTestService(repo, &mockDiff{}, &mockPrompt{})
	if err := svc.DeleteSubmission(context.Background(), "t1", "s1"); err != nil {
		t.Errorf("delete terminal should succeed, got %v", err)
	}
}

// TestDeleteSubmissionNonTerminal 验证删除非终态 → invalid_state。
func TestDeleteSubmissionNonTerminal(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "queued"}
	svc := newSubmissionTestService(repo, &mockDiff{}, &mockPrompt{})
	if err := svc.DeleteSubmission(context.Background(), "t1", "s1"); err != ErrInvalidState {
		t.Errorf("delete non-terminal should be invalid_state, got %v", err)
	}
}

// TestMessageIDFormat 验证 messageID = msg_ + UUID 去连字符小写。
func TestMessageIDFormat(t *testing.T) {
	got := newMessageID("A1B2C3D4-E5F6-4789-ABCD-1234567890AB")
	want := "msg_a1b2c3d4e5f64789abcd1234567890ab"
	if got != want {
		t.Errorf("newMessageID=%q want %q", got, want)
	}
}

// TestConvergeOnStartup 验证重启收敛 sending→delivery_unknown。
func TestConvergeOnStartup(t *testing.T) {
	repo := newMockRepo()
	repo.submissions["s1"] = DiffReviewSubmissionRecord{ID: "s1", TaskID: "t1", Status: "sending"}
	repo.submissions["s2"] = DiffReviewSubmissionRecord{ID: "s2", TaskID: "t2", Status: "queued"}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	n, err := svc.ConvergeOnStartup(context.Background())
	if err != nil {
		t.Fatalf("ConvergeOnStartup: %v", err)
	}
	if n != 1 {
		t.Errorf("should converge 1 sending, got %d", n)
	}
	if repo.submissions["s1"].Status != "delivery_unknown" {
		t.Errorf("sending should become delivery_unknown")
	}
	if repo.submissions["s1"].Error != "delivery unknown after restart" {
		t.Errorf("converged error should be fixed text")
	}
	if repo.submissions["s2"].Status != "queued" {
		t.Errorf("queued should be untouched")
	}
}
