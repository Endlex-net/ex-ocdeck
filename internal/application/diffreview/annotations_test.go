// annotations_test.go 测试 3.5 批注用例（来源组合约束/窗口自洽/stale/编辑三态/删除）。
package diffreview

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
)

// --- mock ports for diffreview tests ---

type mockRepo struct {
	annotations map[string]DiffAnnotationRecord
	submissions map[string]DiffReviewSubmissionRecord
	items       map[string][]DiffReviewSubmissionItemRecord
	nextSeq     int64
	createErr   error
	// 错误注入（3.11 编排测试用）。
	casErr           error
	casErrOnTo       string // 非空时仅 CAS 目标状态等于该值才返回 casErr（F1：只让终态 CAS 失败）
	casMatched       bool   // casMatched=true 时 CAS 强制返回 matched=false（模拟 CAS 失配）
	listItemsErr     error  // 非空时 ListDiffReviewSubmissionItems 返回该错误（批注 7：发送前收集文件路径失败）
	sentCleanupErr   error
	sentCleanupFail  bool // true 时 sent cleanup 返回 matched=false（模拟事务失败 → delivery_unknown）
	convergeErr      error
	getSubmissionErr error
	// getSubmissionCalls 记录 GetDiffReviewSubmission 调用次数（G4：断言创建成功后不再读回）。
	getSubmissionCalls int
	// promptCalls 记录 PromptAsync 调用（供 messageID 复用断言）——在 mockPrompt 中记录。
}

func newMockRepo() *mockRepo {
	return &mockRepo{
		annotations: map[string]DiffAnnotationRecord{},
		submissions: map[string]DiffReviewSubmissionRecord{},
		items:       map[string][]DiffReviewSubmissionItemRecord{},
		nextSeq:     1,
	}
}

func (r *mockRepo) CreateDiffAnnotation(ctx context.Context, in CreateDiffAnnotationInput) (DiffAnnotationRecord, error) {
	if r.createErr != nil {
		return DiffAnnotationRecord{}, r.createErr
	}
	rec := DiffAnnotationRecord{
		ID: in.ID, TaskID: in.TaskID, Path: in.Path, Side: in.Side, Ref: in.Ref, Untracked: in.Untracked,
		StartLine: in.StartLine, EndLine: in.EndLine, SnapshotStartLine: in.SnapshotStartLine,
		Snapshot: in.Snapshot, SnapshotLineCount: in.SnapshotLineCount, Comment: in.Comment,
		Revision: 1, CreatedAt: 100, UpdatedAt: 100,
	}
	r.annotations[in.ID] = rec
	return rec, nil
}
func (r *mockRepo) UpdateDiffAnnotationComment(ctx context.Context, id, comment string) (CommentUpdateResult, error) {
	rec, ok := r.annotations[id]
	if !ok {
		return CommentUpdateResult{Matched: false, Revision: 0}, nil
	}
	if rec.Comment == comment {
		return CommentUpdateResult{Matched: true, Changed: false, Revision: rec.Revision, Record: rec}, nil
	}
	rec.Revision++
	rec.Comment = comment
	r.annotations[id] = rec
	return CommentUpdateResult{Matched: true, Changed: true, Revision: rec.Revision, Record: rec}, nil
}
func (r *mockRepo) DeleteDiffAnnotation(ctx context.Context, id string) (int, error) {
	if _, ok := r.annotations[id]; !ok {
		return 0, nil
	}
	delete(r.annotations, id)
	return 1, nil
}
func (r *mockRepo) ListDiffAnnotationsByTask(ctx context.Context, taskID string) ([]DiffAnnotationRecord, error) {
	var out []DiffAnnotationRecord
	for _, a := range r.annotations {
		if a.TaskID == taskID {
			out = append(out, a)
		}
	}
	return out, nil
}
func (r *mockRepo) GetDiffAnnotation(ctx context.Context, id string) (DiffAnnotationRecord, error) {
	rec, ok := r.annotations[id]
	if !ok {
		return DiffAnnotationRecord{}, ErrAnnotationNotFound
	}
	return rec, nil
}
func (r *mockRepo) CreateDiffReviewSubmission(ctx context.Context, in CreateDiffReviewSubmissionInput) (DiffReviewSubmissionRecord, error) {
	sub := in.Submission
	sub.Seq = r.nextSeq
	r.nextSeq++
	sub.CreatedAt = 100
	r.submissions[sub.ID] = sub
	r.items[sub.ID] = in.Items
	return sub, nil
}
func (r *mockRepo) ListDiffReviewQueue(ctx context.Context, taskID string) ([]DiffReviewSubmissionRecord, error) {
	var out []DiffReviewSubmissionRecord
	for _, s := range r.submissions {
		if s.TaskID == taskID && (s.Status == "queued" || s.Status == "sending") {
			out = append(out, s)
		}
	}
	return out, nil
}
func (r *mockRepo) ListDiffReviewHistory(ctx context.Context, taskID string) ([]DiffReviewSubmissionRecord, error) {
	var out []DiffReviewSubmissionRecord
	for _, s := range r.submissions {
		if s.TaskID == taskID && s.Status == "sent" {
			out = append(out, s)
		}
	}
	return out, nil
}
func (r *mockRepo) ListDiffReviewFailures(ctx context.Context, taskID string) ([]DiffReviewSubmissionRecord, error) {
	var out []DiffReviewSubmissionRecord
	for _, s := range r.submissions {
		if s.TaskID == taskID && (s.Status == "failed" || s.Status == "delivery_unknown") {
			out = append(out, s)
		}
	}
	return out, nil
}
func (r *mockRepo) ListDiffReviewSubmissionPartitions(ctx context.Context, taskID string) (SubmissionPartitions, error) {
	var queue, history, failures []SubmissionView
	for _, s := range r.submissions {
		if s.TaskID != taskID {
			continue
		}
		items := r.items[s.ID]
		if items == nil {
			items = []DiffReviewSubmissionItemRecord{}
		}
		v := SubmissionView{Submission: s, Items: items}
		switch s.Status {
		case "queued", "sending":
			queue = append(queue, v)
		case "sent":
			history = append(history, v)
		case "failed", "delivery_unknown":
			failures = append(failures, v)
		}
	}
	sort.SliceStable(queue, func(i, j int) bool { return queue[i].Submission.Seq < queue[j].Submission.Seq })
	sort.SliceStable(history, func(i, j int) bool {
		if history[i].Submission.SentAt != history[j].Submission.SentAt {
			return history[i].Submission.SentAt > history[j].Submission.SentAt
		}
		return history[i].Submission.Seq > history[j].Submission.Seq
	})
	sort.SliceStable(failures, func(i, j int) bool {
		if failures[i].Submission.CreatedAt != failures[j].Submission.CreatedAt {
			return failures[i].Submission.CreatedAt > failures[j].Submission.CreatedAt
		}
		return failures[i].Submission.Seq > failures[j].Submission.Seq
	})
	if queue == nil {
		queue = []SubmissionView{}
	}
	if history == nil {
		history = []SubmissionView{}
	}
	if failures == nil {
		failures = []SubmissionView{}
	}
	return SubmissionPartitions{Queue: queue, History: history, Failures: failures}, nil
}
func (r *mockRepo) ListDiffReviewSubmissionItems(ctx context.Context, submissionID string) ([]DiffReviewSubmissionItemRecord, error) {
	if r.listItemsErr != nil {
		return nil, r.listItemsErr
	}
	return r.items[submissionID], nil
}
func (r *mockRepo) GetDiffReviewSubmission(ctx context.Context, id string) (DiffReviewSubmissionRecord, error) {
	r.getSubmissionCalls++
	if r.getSubmissionErr != nil {
		return DiffReviewSubmissionRecord{}, r.getSubmissionErr
	}
	s, ok := r.submissions[id]
	if !ok {
		return DiffReviewSubmissionRecord{}, ErrSubmissionNotFound
	}
	return s, nil
}
func (r *mockRepo) CASDiffReviewSubmission(ctx context.Context, id, from, to, errorText string) (bool, error) {
	if r.casErr != nil && (r.casErrOnTo == "" || r.casErrOnTo == to) {
		return false, r.casErr
	}
	if r.casMatched {
		return false, nil
	}
	s, ok := r.submissions[id]
	if !ok || s.Status != from {
		return false, nil
	}
	s.Status = to
	s.Error = errorText
	r.submissions[id] = s
	return true, nil
}
func (r *mockRepo) CompleteDiffReviewSentCleanup(ctx context.Context, submissionID string) (bool, error) {
	if r.sentCleanupErr != nil {
		return false, r.sentCleanupErr
	}
	if r.sentCleanupFail {
		return false, nil
	}
	s, ok := r.submissions[submissionID]
	if !ok || s.Status != "sending" {
		return false, nil
	}
	s.Status = "sent"
	r.submissions[submissionID] = s
	// 删批注（简化：删全部 items 对应批注）
	for _, it := range r.items[submissionID] {
		// 仅删 revision 仍匹配的批注（service 层 revision 语义）。
		if ann, ok := r.annotations[it.AnnotationID]; ok && ann.Revision == it.AnnotationRevision {
			delete(r.annotations, it.AnnotationID)
		}
	}
	return true, nil
}
func (r *mockRepo) CancelDiffReviewSubmission(ctx context.Context, id string) (bool, error) {
	s, ok := r.submissions[id]
	if !ok || s.Status != "queued" {
		return false, nil
	}
	delete(r.submissions, id)
	delete(r.items, id)
	return true, nil
}
func (r *mockRepo) DeleteDiffReviewSubmission(ctx context.Context, id string) (bool, error) {
	s, ok := r.submissions[id]
	if !ok {
		return false, nil
	}
	if s.Status != "sent" && s.Status != "failed" && s.Status != "delivery_unknown" {
		return false, nil
	}
	delete(r.submissions, id)
	delete(r.items, id)
	return true, nil
}
func (r *mockRepo) ConvergeDiffReviewOnStartup(ctx context.Context) (int64, error) {
	if r.convergeErr != nil {
		return 0, r.convergeErr
	}
	var n int64
	for id, s := range r.submissions {
		if s.Status == "sending" {
			s.Status = "delivery_unknown"
			s.Error = "delivery unknown after restart"
			r.submissions[id] = s
			n++
		}
	}
	return n, nil
}

var errNotFound = errors.New("not found")

// errInjected 通用注入错误（F1 终态 CAS 失败注入等）。
var errInjected = errors.New("injected")

type mockScope struct {
	kind  string
	found bool
}

func (s *mockScope) Lookup(ctx context.Context, taskID string) (TaskScopeResult, error) {
	return TaskScopeResult{Found: s.found, Kind: s.kind}, nil
}

// readAllFromSingleCollect 包装旧式单来源读取闭包为批量签名并按读取顺序收集来源（formatter 计数测试用）。
// F14/D7：来源读取失败不短路——错误交给回调汇总后继续遍历剩余来源。
func readAllFromSingleCollect(read func(src sourceTriple) (DiffSourceResult, error), seen *[]sourceTriple) func(srcs []sourceTriple, onSource func(src sourceTriple, result DiffSourceResult, err error) error) error {
	return func(srcs []sourceTriple, onSource func(src sourceTriple, result DiffSourceResult, err error) error) error {
		for _, src := range srcs {
			*seen = append(*seen, src)
			result, err := read(src)
			if cerr := onSource(src, result, err); cerr != nil {
				return cerr
			}
			// err 交回调汇总后继续遍历（D7：来源失败后继续读取剩余来源汇总）。
		}
		return nil
	}
}

type mockDiff struct {
	result DiffSourceResult
	err    error
}

func (d *mockDiff) Read(ctx context.Context, taskID string, src DiffSource) (DiffSourceResult, error) {
	if d.err != nil {
		return DiffSourceResult{}, d.err
	}
	return d.result, nil
}

// ReadLocked 模拟批量读取（F5）：逐来源调用 Read 的语义，回调透传结果与错误。
// F14/D7：来源读取失败不短路——错误交给回调汇总后继续遍历剩余来源。
func (d *mockDiff) ReadLocked(ctx context.Context, taskID string, srcs []DiffSource, fn DiffReadCallback) error {
	for _, src := range srcs {
		result, err := d.Read(ctx, taskID, src)
		if cerr := fn(src, result, err); cerr != nil {
			return cerr
		}
		// err 交回调汇总后继续遍历（D7：来源失败后继续读取剩余来源汇总）。
	}
	return nil
}

type mockRuntime struct {
	snap       RuntimeSnapshot
	probeCap   CapabilityState
	probeErr   error
	probeCalls int
	// probeBarrier：第 N 次（1-based）ProbeCapability 调用时阻塞，通知 started 等待 proceed。
	// 供 F16 顺序测试：第 2 次 probe（复探）阻塞，释放前断言终态已落库。
	probeBlockOn     int
	probeStarted     chan<- struct{}
	probeProceed     <-chan struct{}
	sessStat         SessionStatus
	sessErr          error
	sessErrOn        int // 0=never error; N=error on Nth call (1-based)
	sessCalls        int
	invalidateCalled bool
	// F3：invalidate 携带的版本记录与 Snapshot 调用计数（验证 fencing 用构造期捕获版本、不读 DB）。
	invalidateVersions []string
	snapshotCalls      int
	// F3: GetSession 结果（默认 found）与 setUnsupported 调用追踪。
	getSessionResult     GetSessionResult
	getSessionCalled     bool
	setUnsupportedCalled bool
}

func (r *mockRuntime) Snapshot(ctx context.Context, taskID string) (RuntimeSnapshot, error) {
	r.snapshotCalls++
	return r.snap, nil
}
func (r *mockRuntime) SessionStatus(ctx context.Context, taskID, sessionID string) (SessionStatus, error) {
	r.sessCalls++
	if r.sessErrOn > 0 && r.sessCalls == r.sessErrOn {
		return "", r.sessErr
	}
	if r.sessErr != nil && r.sessErrOn == 0 {
		return "", r.sessErr
	}
	return r.sessStat, nil
}
func (r *mockRuntime) ProbeCapability(ctx context.Context, taskID string) (CapabilityState, error) {
	r.probeCalls++
	// F16：第 N 次 probe 阻塞（供顺序测试断言终态落库先于复探完成）。
	if r.probeBlockOn > 0 && r.probeCalls == r.probeBlockOn && r.probeStarted != nil {
		close(r.probeStarted)
		<-r.probeProceed
	}
	return r.probeCap, r.probeErr
}
func (r *mockRuntime) InvalidateCapability(ctx context.Context, taskID, instVersion string) {
	r.invalidateCalled = true
	r.invalidateVersions = append(r.invalidateVersions, instVersion)
}
func (r *mockRuntime) GetSession(ctx context.Context, taskID, sessionID string) (GetSessionResult, error) {
	r.getSessionCalled = true
	// 默认 found（会话存在→端点不支持）；测试可注入 getSessionResult 覆盖。
	res := r.getSessionResult
	if res.Status == "" {
		res.Status = GetSessionFound
	}
	return res, nil
}
func (r *mockRuntime) SetCapabilityUnsupported(ctx context.Context, taskID, instVersion string) {
	r.setUnsupportedCalled = true
}

type mockPrompt struct {
	outcome PromptOutcome
	// promptCalls 记录每次 PromptAsync 的 (taskID, sessionID, messageID)，供重试复用断言。
	promptCalls []promptCall
	// callMu 保护 promptCalls（-race 下多调用）。
	callMu sync.Mutex
}

type promptCall struct {
	TaskID    string
	SessionID string
	MessageID string
	Text      string
	Files     []string
}

func (p *mockPrompt) PromptAsync(ctx context.Context, taskID, sessionID, messageID, text string, files []string) PromptOutcome {
	p.callMu.Lock()
	p.promptCalls = append(p.promptCalls, promptCall{TaskID: taskID, SessionID: sessionID, MessageID: messageID, Text: text, Files: append([]string{}, files...)})
	p.callMu.Unlock()
	return p.outcome
}

// --- helpers ---

func newTestService(repo DiffReviewRepository, scope TaskScopePort, diff DiffSourcePort, rt RuntimePort, prompt PromptPort) *Service {
	return New(Options{Repo: repo, Scope: scope, Diff: diff, Runtime: rt, Prompt: prompt})
}

// --- tests ---

// TestCreateAnnotationSourceComboUntracked 验证 untracked→ref空+side new 约束。
func TestCreateAnnotationSourceComboUntracked(t *testing.T) {
	repo := newMockRepo()
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})

	// untracked=true, ref非空 → ErrInvalidAnnotationSource
	_, err := svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", Ref: "HEAD", Untracked: true,
		StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c",
	})
	if err != ErrInvalidAnnotationSource {
		t.Errorf("untracked with ref should be rejected, got %v", err)
	}

	// untracked=true, side=old → ErrInvalidAnnotationSource
	_, err = svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a2", TaskID: "t1", Path: "f.go", Side: "old", Untracked: true,
		StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c",
	})
	if err != ErrInvalidAnnotationSource {
		t.Errorf("untracked with side=old should be rejected, got %v", err)
	}

	// untracked=true, ref="", side=new → OK
	_, err = svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a3", TaskID: "t1", Path: "f.go", Side: "new", Untracked: true,
		StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c",
	})
	if err != nil {
		t.Errorf("valid untracked annotation should succeed, got %v", err)
	}
}

// TestCreateAnnotationInvalidSide 验证 side 非 old/new 拒绝。
func TestCreateAnnotationInvalidSide(t *testing.T) {
	svc := newTestService(newMockRepo(), &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "middle", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c",
	})
	if err != ErrInvalidAnnotationSide {
		t.Errorf("invalid side should be rejected, got %v", err)
	}
}

// TestCreateAnnotationRangeValidation 验证 1-based 闭区间。
func TestCreateAnnotationRangeValidation(t *testing.T) {
	svc := newTestService(newMockRepo(), &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	// startLine < 1
	_, err := svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 0, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 2, Snapshot: "x\n", Comment: "c",
	})
	if err != ErrInvalidAnnotationRange {
		t.Errorf("startLine<1 should be rejected, got %v", err)
	}
	// endLine < startLine
	_, err = svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a2", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 5, EndLine: 3,
		SnapshotStartLine: 1, SnapshotLineCount: 10, Snapshot: "x\n", Comment: "c",
	})
	if err != ErrInvalidAnnotationRange {
		t.Errorf("endLine<startLine should be rejected, got %v", err)
	}
}

// TestCreateAnnotationSnapshotWindow 验证窗口自洽校验。
func TestCreateAnnotationSnapshotWindow(t *testing.T) {
	svc := newTestService(newMockRepo(), &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	// startLine 落在窗口外（窗口 [1,3]，startLine=4）
	_, err := svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 4, EndLine: 4,
		SnapshotStartLine: 1, SnapshotLineCount: 3, Snapshot: "a\nb\nc\n", Comment: "c",
	})
	if err != ErrInvalidSnapshotWindow {
		t.Errorf("startLine outside window should be rejected, got %v", err)
	}
}

// TestCreateAnnotationTaskScopeDir 验证 dir 项目拒绝。
func TestCreateAnnotationTaskScopeDir(t *testing.T) {
	svc := newTestService(newMockRepo(), &mockScope{found: true, kind: "dir"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c",
	})
	if err != ErrDirProject {
		t.Errorf("dir project should be rejected, got %v", err)
	}
}

// TestCreateAnnotationTaskNotFound 验证任务不存在。
func TestCreateAnnotationTaskNotFound(t *testing.T) {
	svc := newTestService(newMockRepo(), &mockScope{found: false, kind: ""}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc.CreateAnnotation(context.Background(), CreateDiffAnnotationInput{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x", Comment: "c",
	})
	if err != ErrTaskNotFound {
		t.Errorf("task not found should be rejected, got %v", err)
	}
}

// TestListAnnotationsStaleMatch 验证快照与当前内容匹配 → active。
// F9 行模型：split('\n') join \n 重连——window = ["l1","l2","l3"] join \n = "l1\nl2\nl3"（无末尾 \n）。
func TestListAnnotationsStaleMatch(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 2, EndLine: 2,
		SnapshotStartLine: 1, SnapshotLineCount: 3, Snapshot: "l1\nl2\nl3",
	}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "l1\nl2\nl3\nl4\n", NewExists: true, NewMode: "100644"}}, &mockRuntime{}, &mockPrompt{})
	views, err := svc.ListAnnotations(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ListAnnotations: %v", err)
	}
	if len(views) != 1 || views[0].Stale {
		t.Errorf("matching snapshot should be active, got stale=%v", views[0].Stale)
	}
}

// TestListAnnotationsStaleDrift 验证快照与当前内容不等 → stale。
func TestListAnnotationsStaleDrift(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 2, EndLine: 2,
		SnapshotStartLine: 1, SnapshotLineCount: 3, Snapshot: "l1\nl2\nl3",
	}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{result: DiffSourceResult{NewContent: "l1\nCHANGED\nl3\nl4\n", NewExists: true, NewMode: "100644"}}, &mockRuntime{}, &mockPrompt{})
	views, _ := svc.ListAnnotations(context.Background(), "t1")
	if !views[0].Stale {
		t.Errorf("drifted snapshot should be stale")
	}
}

// TestListAnnotationsStaleReadFailure 验证读取失败 → stale=true 但列表正常返回。
func TestListAnnotationsStaleReadFailure(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Path: "f.go", Side: "new", StartLine: 1, EndLine: 1,
		SnapshotStartLine: 1, SnapshotLineCount: 1, Snapshot: "x",
	}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"},
		&mockDiff{err: errors.New("git error")}, &mockRuntime{}, &mockPrompt{})
	views, err := svc.ListAnnotations(context.Background(), "t1")
	if err != nil {
		t.Errorf("read failure MUST NOT block list, got err %v", err)
	}
	if !views[0].Stale {
		t.Errorf("read failure should set stale=true")
	}
}

// TestUpdateCommentEmptyRejected 验证空白评论拒绝。
func TestUpdateCommentEmptyRejected(t *testing.T) {
	svc := newTestService(newMockRepo(), &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc.UpdateComment(context.Background(), "t1", "a1", "   ")
	if err != ErrEmptyComment {
		t.Errorf("empty comment should be rejected, got %v", err)
	}
}

// TestUpdateCommentSameValueRevisionUnchanged 验证同值 revision 不变。
func TestUpdateCommentSameValueRevisionUnchanged(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Comment: "same", Revision: 3,
	}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	rec, err := svc.UpdateComment(context.Background(), "t1", "a1", "same")
	if err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if rec.Revision != 3 {
		t.Errorf("same value should not change revision, got %d", rec.Revision)
	}
}

// TestUpdateCommentRealChangeRevisionIncremented 验证真实变更 revision+1。
func TestUpdateCommentRealChangeRevisionIncremented(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{
		ID: "a1", TaskID: "t1", Comment: "old", Revision: 1,
	}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	rec, err := svc.UpdateComment(context.Background(), "t1", "a1", "new comment")
	if err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if rec.Revision != 2 {
		t.Errorf("real change should increment revision to 2, got %d", rec.Revision)
	}
}

// TestUpdateCommentNotFound 验证批注不存在。
func TestUpdateCommentNotFound(t *testing.T) {
	svc := newTestService(newMockRepo(), &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	_, err := svc.UpdateComment(context.Background(), "t1", "missing", "comment")
	if err != ErrAnnotationNotFound {
		t.Errorf("missing annotation should return not found, got %v", err)
	}
}

// TestDeleteAnnotationSuccess 验证删除成功。
func TestDeleteAnnotationSuccess(t *testing.T) {
	repo := newMockRepo()
	repo.annotations["a1"] = DiffAnnotationRecord{ID: "a1", TaskID: "t1"}
	svc := newTestService(repo, &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	if err := svc.DeleteAnnotation(context.Background(), "t1", "a1"); err != nil {
		t.Errorf("delete should succeed, got %v", err)
	}
}

// TestDeleteAnnotationNotFound 验证删除不存在的批注。
func TestDeleteAnnotationNotFound(t *testing.T) {
	svc := newTestService(newMockRepo(), &mockScope{found: true, kind: "repo"}, &mockDiff{}, &mockRuntime{}, &mockPrompt{})
	err := svc.DeleteAnnotation(context.Background(), "t1", "missing")
	if err != ErrAnnotationNotFound {
		t.Errorf("delete missing should return not found, got %v", err)
	}
}
