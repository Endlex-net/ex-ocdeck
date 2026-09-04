package api

// diff-review-workbench design.md D8 api 层测试（tasks 4.4）：
//   - decodeBoundedJSON：四 body 端点 wire 上限临界 N/N+1、2×/6× 转义膨胀 raw wire 计数、
//     超量尾随空白/超限第二 JSON 值（MaxBytesError 分类）；
//   - 逐端点错误映射：not_found 仅任务（列表/提交）/任务+批注（PATCH/DELETE）、跨任务 id
//     invalid_input、复核 conflict（缺失/revision 不符/repository 创建阶段）、revision 非法值
//     0/-1/小数/溢出 invalid_input 且零业务读取、git/file *application.OpError 透传矩阵；
//   - DTO wire 形状（camelCase、sentAt null、items 非空数组、判别联合互斥分支）。
//
// 领域校验本身归 lane 3（internal/application/diffreview）测试；此处以最小 fake ports
// 走真实 diffreview.Service，断言 api 层解码/映射/形状。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/application/diffreview"
)

// --- fake ports（嵌入接口，仅实现被测路径触达的方法） ---

type fakeDiffReviewScope struct {
	diffreview.TaskScopePort
	found   bool
	kind    string
	lookups int
}

// Lookup 仅对 "t1" 返回 found（其余任务视为不存在，供 not_found 映射测试）。
func (s *fakeDiffReviewScope) Lookup(ctx context.Context, taskID string) (diffreview.TaskScopeResult, error) {
	s.lookups++
	if taskID != "t1" {
		return diffreview.TaskScopeResult{Found: false}, nil
	}
	return diffreview.TaskScopeResult{Found: s.found, Kind: s.kind}, nil
}

type fakeDiffReviewRuntime struct {
	diffreview.RuntimePort
	capability diffreview.CapabilityState
	snapshots  int
	probes     int
}

func (r *fakeDiffReviewRuntime) Snapshot(ctx context.Context, taskID string) (diffreview.RuntimeSnapshot, error) {
	r.snapshots++
	return diffreview.RuntimeSnapshot{
		InstVersion:      "v1",
		HasRuntime:       true,
		AnchorSessionID:  "sess-1",
		HasAnchorSession: true,
		CapabilityState:  r.capability,
	}, nil
}

func (r *fakeDiffReviewRuntime) ProbeCapability(ctx context.Context, taskID string) (diffreview.CapabilityState, error) {
	r.probes++
	if r.capability == "" {
		return diffreview.CapabilityUnknown, nil
	}
	return r.capability, nil
}

type fakeDiffReviewDiffSource struct {
	diffreview.DiffSourcePort
	readResult  diffreview.DiffSourceResult
	readErr     error
	reads       int
	readLockeds int
}

func (d *fakeDiffReviewDiffSource) Read(ctx context.Context, taskID string, src diffreview.DiffSource) (diffreview.DiffSourceResult, error) {
	d.reads++
	if d.readErr != nil {
		return diffreview.DiffSourceResult{}, d.readErr
	}
	return d.readResult, nil
}

func (d *fakeDiffReviewDiffSource) ReadLocked(ctx context.Context, taskID string, srcs []diffreview.DiffSource, fn diffreview.DiffReadCallback) error {
	d.readLockeds++
	if d.readErr != nil {
		return d.readErr
	}
	for _, src := range srcs {
		if err := fn(src, d.readResult, nil); err != nil {
			return err
		}
	}
	return nil
}

type fakeDiffReviewRepo struct {
	diffreview.DiffReviewRepository
	annotations           map[string]diffreview.DiffAnnotationRecord
	submissions           map[string]diffreview.DiffReviewSubmissionRecord
	seq                   int64
	createSubErr          error
	getAnnotationCalls    int
	listAnnotationCalls   int
	createSubmissionCalls int
}

func newFakeDiffReviewRepo() *fakeDiffReviewRepo {
	return &fakeDiffReviewRepo{
		annotations: map[string]diffreview.DiffAnnotationRecord{},
		submissions: map[string]diffreview.DiffReviewSubmissionRecord{},
		seq:         1,
	}
}

func (r *fakeDiffReviewRepo) CreateDiffAnnotation(ctx context.Context, in diffreview.CreateDiffAnnotationInput) (diffreview.DiffAnnotationRecord, error) {
	rec := diffreview.DiffAnnotationRecord{
		ID:                in.ID,
		TaskID:            in.TaskID,
		Path:              in.Path,
		Side:              in.Side,
		Ref:               in.Ref,
		Untracked:         in.Untracked,
		StartLine:         in.StartLine,
		EndLine:           in.EndLine,
		SnapshotStartLine: in.SnapshotStartLine,
		Snapshot:          in.Snapshot,
		SnapshotLineCount: in.SnapshotLineCount,
		Comment:           in.Comment,
		Revision:          1,
		CreatedAt:         1720000000,
		UpdatedAt:         1720000000,
	}
	r.annotations[in.ID] = rec
	return rec, nil
}

func (r *fakeDiffReviewRepo) GetDiffAnnotation(ctx context.Context, id string) (diffreview.DiffAnnotationRecord, error) {
	r.getAnnotationCalls++
	rec, ok := r.annotations[id]
	if !ok {
		return diffreview.DiffAnnotationRecord{}, diffreview.ErrAnnotationNotFound
	}
	return rec, nil
}

func (r *fakeDiffReviewRepo) ListDiffAnnotationsByTask(ctx context.Context, taskID string) ([]diffreview.DiffAnnotationRecord, error) {
	r.listAnnotationCalls++
	var out []diffreview.DiffAnnotationRecord
	for _, rec := range r.annotations {
		if rec.TaskID == taskID {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (r *fakeDiffReviewRepo) UpdateDiffAnnotationComment(ctx context.Context, id, comment string) (diffreview.CommentUpdateResult, error) {
	rec, ok := r.annotations[id]
	if !ok {
		return diffreview.CommentUpdateResult{}, diffreview.ErrAnnotationNotFound
	}
	if rec.Comment == comment {
		return diffreview.CommentUpdateResult{Matched: true, Changed: false, Revision: rec.Revision, Record: rec}, nil
	}
	rec.Comment = comment
	rec.Revision++
	rec.UpdatedAt++
	r.annotations[id] = rec
	return diffreview.CommentUpdateResult{Matched: true, Changed: true, Revision: rec.Revision, Record: rec}, nil
}

func (r *fakeDiffReviewRepo) DeleteDiffAnnotation(ctx context.Context, id string) (int, error) {
	if _, ok := r.annotations[id]; !ok {
		return 0, diffreview.ErrAnnotationNotFound
	}
	delete(r.annotations, id)
	return 1, nil
}

func (r *fakeDiffReviewRepo) CreateDiffReviewSubmission(ctx context.Context, in diffreview.CreateDiffReviewSubmissionInput) (diffreview.DiffReviewSubmissionRecord, error) {
	r.createSubmissionCalls++
	if r.createSubErr != nil {
		return diffreview.DiffReviewSubmissionRecord{}, r.createSubErr
	}
	r.seq++
	sub := in.Submission
	sub.Seq = r.seq
	sub.CreatedAt = 1720000100
	r.submissions[sub.ID] = sub
	return sub, nil
}

func (r *fakeDiffReviewRepo) GetDiffReviewSubmission(ctx context.Context, id string) (diffreview.DiffReviewSubmissionRecord, error) {
	rec, ok := r.submissions[id]
	if !ok {
		return diffreview.DiffReviewSubmissionRecord{}, diffreview.ErrSubmissionNotFound
	}
	return rec, nil
}

func (r *fakeDiffReviewRepo) listByStatuses(ctx context.Context, taskID string, statuses ...string) ([]diffreview.DiffReviewSubmissionRecord, error) {
	var out []diffreview.DiffReviewSubmissionRecord
	for _, rec := range r.submissions {
		if rec.TaskID != taskID {
			continue
		}
		for _, st := range statuses {
			if rec.Status == st {
				out = append(out, rec)
				break
			}
		}
	}
	return out, nil
}

func (r *fakeDiffReviewRepo) ListDiffReviewQueue(ctx context.Context, taskID string) ([]diffreview.DiffReviewSubmissionRecord, error) {
	return r.listByStatuses(ctx, taskID, "queued", "sending")
}

func (r *fakeDiffReviewRepo) ListDiffReviewHistory(ctx context.Context, taskID string) ([]diffreview.DiffReviewSubmissionRecord, error) {
	return r.listByStatuses(ctx, taskID, "sent")
}

func (r *fakeDiffReviewRepo) ListDiffReviewFailures(ctx context.Context, taskID string) ([]diffreview.DiffReviewSubmissionRecord, error) {
	return r.listByStatuses(ctx, taskID, "failed", "delivery_unknown")
}

func (r *fakeDiffReviewRepo) ListDiffReviewSubmissionItems(ctx context.Context, submissionID string) ([]diffreview.DiffReviewSubmissionItemRecord, error) {
	return nil, nil
}

func (r *fakeDiffReviewRepo) ListDiffReviewSubmissionPartitions(ctx context.Context, taskID string) (diffreview.SubmissionPartitions, error) {
	var queue, history, failures []diffreview.SubmissionView
	for _, rec := range r.submissions {
		if rec.TaskID != taskID {
			continue
		}
		v := diffreview.SubmissionView{Submission: rec, Items: []diffreview.DiffReviewSubmissionItemRecord{}}
		switch rec.Status {
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
		queue = []diffreview.SubmissionView{}
	}
	if history == nil {
		history = []diffreview.SubmissionView{}
	}
	if failures == nil {
		failures = []diffreview.SubmissionView{}
	}
	return diffreview.SubmissionPartitions{Queue: queue, History: history, Failures: failures}, nil
}

func (r *fakeDiffReviewRepo) CancelDiffReviewSubmission(ctx context.Context, id string) (bool, error) {
	rec, ok := r.submissions[id]
	if !ok {
		return false, diffreview.ErrSubmissionNotFound
	}
	if rec.Status != "queued" {
		return false, nil
	}
	delete(r.submissions, id)
	return true, nil
}

func (r *fakeDiffReviewRepo) DeleteDiffReviewSubmission(ctx context.Context, id string) (bool, error) {
	rec, ok := r.submissions[id]
	if !ok {
		return false, diffreview.ErrSubmissionNotFound
	}
	switch rec.Status {
	case "sent", "failed", "delivery_unknown":
		delete(r.submissions, id)
		return true, nil
	default:
		return false, nil
	}
}

type fakeFileEditPort struct {
	diffreview.FileEditPort
	readRaw func(ctx context.Context, taskID, path string) (diffreview.FileEditRawFile, error)
	writeFn func(ctx context.Context, taskID string, req diffreview.FileEditWriteRequest) (diffreview.FileEditWriteResult, error)
	writes  int
}

func (p *fakeFileEditPort) ReadRaw(ctx context.Context, taskID, path string) (diffreview.FileEditRawFile, error) {
	if p.readRaw != nil {
		return p.readRaw(ctx, taskID, path)
	}
	return diffreview.FileEditRawFile{Exists: false}, nil
}

func (p *fakeFileEditPort) Write(ctx context.Context, taskID string, req diffreview.FileEditWriteRequest) (diffreview.FileEditWriteResult, error) {
	p.writes++
	if p.writeFn != nil {
		return p.writeFn(ctx, taskID, req)
	}
	return diffreview.FileEditWriteResult{BaseHash: "newhash"}, nil
}

// diffReviewFakes 聚合注入 service 的全部 fake ports（各 port 自带调用计数，供零副作用断言）。
type diffReviewFakes struct {
	repo     *fakeDiffReviewRepo
	scope    *fakeDiffReviewScope
	runtime  *fakeDiffReviewRuntime
	diff     *fakeDiffReviewDiffSource
	fileEdit *fakeFileEditPort
}

// newDiffReviewAPIServer 构造注入真实 diffreview.Service（fake ports）的 Server。
func newDiffReviewAPIServer(t *testing.T) (*Server, *diffReviewFakes) {
	t.Helper()
	repo := newFakeDiffReviewRepo()
	scope := &fakeDiffReviewScope{found: true, kind: "repo"}
	rt := &fakeDiffReviewRuntime{capability: diffreview.CapabilitySupported}
	diffSrc := &fakeDiffReviewDiffSource{readResult: diffreview.DiffSourceResult{OldExists: true, NewExists: true, OldContent: "hello\n", NewContent: "hello\n"}}
	fileEdit := &fakeFileEditPort{}
	svc := diffreview.New(diffreview.Options{
		Repo:     repo,
		Scope:    scope,
		Runtime:  rt,
		Diff:     diffSrc,
		FileEdit: fileEdit,
	})
	cfg := testConfig()
	s := &Server{
		cfg:        cfg,
		mux:        http.NewServeMux(),
		auth:       NewTokenAuthenticator(cfg.Token),
		wsClients:  newWSClientRegistry(),
		tasks:      &fakeTaskBackend{},
		diffreview: svc,
	}
	s.registerRoutes()
	return s, &diffReviewFakes{repo: repo, scope: scope, runtime: rt, diff: diffSrc, fileEdit: fileEdit}
}

func doDiffReviewReq(t *testing.T, s *Server, method, path, body string) (*http.Response, string) {
	t.Helper()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	resp, err := http.DefaultClient.Do(authedReq(method, ts.URL+path, body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp, sb.String()
}

// errCodeOf 从统一错误信封提取 code。
func errCodeOf(t *testing.T, body string) string {
	t.Helper()
	var eb struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return eb.Error.Code
}

// errMsgOf 从统一错误信封提取 message。
func errMsgOf(t *testing.T, body string) string {
	t.Helper()
	var eb struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return eb.Error.Message
}

// validAnnotationBody 合法创建批注请求（snapshot "hello"=1 逻辑行）。
func validAnnotationBody(comment string) string {
	b, _ := json.Marshal(createAnnotationReq{
		Path: "a.go", Side: "new", Untracked: false,
		StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1,
		Snapshot: "hello", Comment: comment,
	})
	return string(b)
}

// padBodyTo 以尾随空白将领域合法的小 body 补齐到恰好 want wire 字节
// （decoded 语义不变，wire 长度精确可控）。
func padBodyTo(body string, want int64) string {
	return body + strings.Repeat(" ", int(want)-len(body))
}

// --- 4.1 decodeBoundedJSON ---

func TestDecodeBoundedJSON_WireLimitBoundaryOneMiB(t *testing.T) {
	s, _ := newDiffReviewAPIServer(t)
	// 以合法 JSON 尾随空白精确控制 wire 字节数（comment 受 65536 decoded 服务上限，不参与填充）。
	base := validAnnotationBody("c1")
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotations", padBodyTo(base, annotationCreateMaxBytes))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("exactly-at-limit status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	resp, body = doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotations", padBodyTo(base, annotationCreateMaxBytes+1))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("limit+1 status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}
}

func TestDecodeBoundedJSON_WireLimitBoundaryPatchComment(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	seedAnnotation(t, f.repo, "a1", "t1", 1)
	// PATCH 同值（seed comment "c"）→ 200 且 revision 不变；wire 由尾随空白补齐到 512KiB。
	base := `{"comment":"c"}`
	resp, body := doDiffReviewReq(t, s, "PATCH", "/api/v1/tasks/t1/annotations/a1", padBodyTo(base, annotationPatchMaxBytes))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exactly-at-limit status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var ann annotationDTO
	if err := json.Unmarshal([]byte(body), &ann); err != nil {
		t.Fatal(err)
	}
	if ann.Revision != 1 {
		t.Fatalf("PATCH same revision = %d, want 1 (unchanged)", ann.Revision)
	}
	resp, body = doDiffReviewReq(t, s, "PATCH", "/api/v1/tasks/t1/annotations/a1", padBodyTo(base, annotationPatchMaxBytes+1))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("limit+1 status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}
}

func TestDecodeBoundedJSON_WireLimitBoundarySubmission(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	seedAnnotation(t, f.repo, "a1", "t1", 1)
	base := `{"annotations":[{"id":"a1","revision":1}],"note":"n"}`
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions", padBodyTo(base, submissionCreateMaxBytes))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("exactly-at-limit status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	resp, body = doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions", padBodyTo(base, submissionCreateMaxBytes+1))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("limit+1 status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}
}

func TestDecodeBoundedJSON_TrailingWhitespaceAndSecondValue(t *testing.T) {
	s, _ := newDiffReviewAPIServer(t)
	// 合法对象后附尾随空白（限内）→ 接受。
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotations", validAnnotationBody("c1")+"\n\n  \t")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("trailing whitespace status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	// 第二 JSON 值 → invalid_input。
	resp, body = doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotations", validAnnotationBody("c1")+` {"x":1}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("second JSON value status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}
	// 超量尾随空白（第二段解码推过 wire 上限）→ MaxBytesError 分类 invalid_input，message 携带上限。
	pad := strings.Repeat(" ", int(annotationCreateMaxBytes)) // 远超 1MiB
	resp, body = doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotations", validAnnotationBody("c1")+pad)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("overflowing whitespace status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code, msg := errCodeOf(t, body), errMsgOf(t, body); code != "invalid_input" || !strings.Contains(msg, "bytes limit") {
		t.Fatalf("code/msg = %q/%q, want invalid_input with bytes limit", code, msg)
	}
	// 超限第二 JSON 值（第二段解码中途超 wire 上限）→ 同 MaxBytesError 分类，而非 trailing data。
	bigSecond := ` {"pad":"` + strings.Repeat("a", int(annotationCreateMaxBytes)) + `"}`
	resp, body = doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotations", validAnnotationBody("c1")+bigSecond)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("overflowing second value status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	code, msg := errCodeOf(t, body), errMsgOf(t, body)
	if code != "invalid_input" || !strings.Contains(msg, "bytes limit") || strings.Contains(msg, "trailing data") {
		t.Fatalf("code/msg = %q/%q, want invalid_input with bytes limit (not trailing data)", code, msg)
	}
}

func TestDecodeBoundedJSON_EscapeInflation(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	// 6× 转义膨胀（控制字符 \u0001=6 wire bytes/char）限内 → 接受。
	// comment decoded 65535 ≤65536（service 上限），wire ≈ 393KiB < 1MiB。
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotations", validAnnotationBody(strings.Repeat("\x01", 65535)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("6x-inflated under-limit status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	// POST /git/file 4MiB：表驱动证明 raw wire 计数——decoded 样例本身超过 D5 512KiB
	// 重建上限，但请求在到达 service 前就被 MaxBytesReader 拒绝（断言 file-edit port 零调用）。
	tmpl := `{"path":"a.txt","content":"%s","baseHash":"%s","lineEnding":"lf","baseMode":"0644"}`
	for _, tc := range []struct {
		name    string
		escaped string
		unit    int // 每个源字符的 wire 字节数
		n       int
	}{
		{name: "6x: control char as \\u0001 = 6 wire bytes/char", escaped: `\u0001`, unit: 6, n: 700_000},
		{name: "2x: quote as \\\" = 2 wire bytes/char", escaped: `\"`, unit: 2, n: 2_100_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			over := fmt.Sprintf(tmpl, strings.Repeat(tc.escaped, tc.n), strings.Repeat("a", 64))
			// raw wire 计数：n×unit 字节的转义序列 + 模板结构，必已越过 4MiB 上限。
			if len(over) < tc.unit*tc.n || int64(len(over)) <= gitFileWriteMaxBytes {
				t.Fatalf("raw wire len = %d, want > %d (%d chars x %d bytes escape expansion)", len(over), gitFileWriteMaxBytes, tc.n, tc.unit)
			}
			f.fileEdit.writes = 0
			resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/git/file", over)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("inflated over-limit status = %d, want 422; body=%s", resp.StatusCode, body)
			}
			if code := errCodeOf(t, body); code != "invalid_input" {
				t.Fatalf("code = %q, want invalid_input", code)
			}
			if f.fileEdit.writes != 0 {
				t.Fatal("over-limit body must not reach service WriteFile")
			}
		})
	}
}

func TestGitFileWrite_WireLimitBoundaryFourMiB(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	// D5：decoded content MUST ≤512KiB（重建上限）；4MiB wire 由尾随空白补齐，
	// 正边界不得构造超限 decoded content。
	var written string
	f.fileEdit.writeFn = func(ctx context.Context, taskID string, req diffreview.FileEditWriteRequest) (diffreview.FileEditWriteResult, error) {
		written = req.Content
		return diffreview.FileEditWriteResult{BaseHash: "newhash"}, nil
	}
	base := fmt.Sprintf(`{"path":"a.txt","content":"hello\n","baseHash":"%s","lineEnding":"lf","baseMode":"0644"}`, strings.Repeat("a", 64))
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/git/file", padBodyTo(base, gitFileWriteMaxBytes))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exactly-at-limit status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var out fileEditWriteResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.BaseHash != "newhash" {
		t.Fatalf("baseHash = %q, want newhash", out.BaseHash)
	}
	if written == "" || int64(len(written)) > diffreview.FileEditMaxBytes {
		t.Fatalf("decoded content len = %d, must stay within D5 rebuild limit %d", len(written), diffreview.FileEditMaxBytes)
	}
	resp, body = doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/git/file", padBodyTo(base, gitFileWriteMaxBytes+1))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("limit+1 status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}
}

// --- 4.4 逐端点错误映射 ---

func seedAnnotation(t *testing.T, repo *fakeDiffReviewRepo, id, taskID string, revision int) {
	t.Helper()
	repo.annotations[id] = diffreview.DiffAnnotationRecord{
		ID: id, TaskID: taskID, Path: "a.go", Side: "new",
		StartLine: 1, EndLine: 1, SnapshotStartLine: 1, SnapshotLineCount: 1,
		Snapshot: "hello", Comment: "c", Revision: revision,
		CreatedAt: 1720000000, UpdatedAt: 1720000000,
	}
}

func TestAnnotationsErrors_TaskScope(t *testing.T) {
	s, _ := newDiffReviewAPIServer(t)
	// not_found：任务不存在（GET annotations / GET submissions / POST submissions 一致）。
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/tasks/t-missing/annotations", ""},
		{"GET", "/api/v1/tasks/t-missing/annotation-submissions", ""},
		{"POST", "/api/v1/tasks/t-missing/annotation-submissions", `{"annotations":[{"id":"a1","revision":1}]}`},
	} {
		resp, body := doDiffReviewReq(t, s, tc.method, tc.path, tc.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404; body=%s", tc.method, tc.path, resp.StatusCode, body)
		}
		if code := errCodeOf(t, body); code != "not_found" {
			t.Fatalf("%s %s code = %q, want not_found", tc.method, tc.path, code)
		}
	}
}

func TestAnnotationsErrors_DirProject(t *testing.T) {
	svcScope := &fakeDiffReviewScope{found: true, kind: "dir"}
	svc := diffreview.New(diffreview.Options{
		Repo:    newFakeDiffReviewRepo(),
		Scope:   svcScope,
		Runtime: &fakeDiffReviewRuntime{capability: diffreview.CapabilitySupported},
		Diff:    &fakeDiffReviewDiffSource{},
	})
	cfg := testConfig()
	s := &Server{cfg: cfg, mux: http.NewServeMux(), auth: NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(), tasks: &fakeTaskBackend{}, diffreview: svc}
	s.registerRoutes()
	resp, body := doDiffReviewReq(t, s, "GET", "/api/v1/tasks/t1/annotations", "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}
}

func TestSubmissionErrors_BatchClassification(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	repo := f.repo
	const reqBody = `{"annotations":[{"id":"a1","revision":%d}],"note":"n"}`

	// 跨任务 id → invalid_input（不区分顺序，先于 conflict）。
	seedAnnotation(t, repo, "a1", "t-other", 1)
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions", fmt.Sprintf(reqBody, 1))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("cross-task status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "invalid_input" {
		t.Fatalf("cross-task code = %q, want invalid_input", code)
	}
	if len(repo.submissions) != 0 {
		t.Fatalf("cross-task must not persist, got %d submissions", len(repo.submissions))
	}

	// revision 不符 → conflict(409)。
	seedAnnotation(t, repo, "a1", "t1", 1)
	resp, body = doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions", fmt.Sprintf(reqBody, 2))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("revision mismatch status = %d, want 409; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "conflict" {
		t.Fatalf("revision mismatch code = %q, want conflict", code)
	}
	if len(repo.submissions) != 0 {
		t.Fatalf("revision mismatch must not persist, got %d submissions", len(repo.submissions))
	}
}

// revision 非法值（0/-1 service 域校验；小数/字符串/溢出 JSON 解码失败）→ invalid_input，
// 且 scope/runtime/diff/repo 业务读 port 零调用（design.md D8：先于任何 task/store/diff 读取）。
func TestSubmissionErrors_InvalidRevisionZeroBusinessReads(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	seedAnnotation(t, f.repo, "a1", "t1", 1)
	for _, rev := range []string{"0", "-1", "1.5", `"` + strings.Repeat("9", 25) + `"`, "9223372036854775808"} {
		b := fmt.Sprintf(`{"annotations":[{"id":"a1","revision":%s}],"note":"n"}`, rev)
		resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions", b)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("revision %s status = %d, want 422; body=%s", rev, resp.StatusCode, body)
		}
		if code := errCodeOf(t, body); code != "invalid_input" {
			t.Fatalf("revision %s code = %q, want invalid_input", rev, code)
		}
	}
	if f.scope.lookups != 0 || f.runtime.snapshots != 0 || f.runtime.probes != 0 ||
		f.diff.reads != 0 || f.diff.readLockeds != 0 ||
		f.repo.getAnnotationCalls != 0 || f.repo.listAnnotationCalls != 0 {
		t.Fatalf("invalid revision must not read any business port: scope=%d runtime=%d/%d diff=%d/%d repo get/list=%d/%d",
			f.scope.lookups, f.runtime.snapshots, f.runtime.probes,
			f.diff.reads, f.diff.readLockeds, f.repo.getAnnotationCalls, f.repo.listAnnotationCalls)
	}
}

// 本任务范围内批注不存在（从未存在）→ conflict 409（D8：不区分"从未存在"与"预览后删除"），零落库。
func TestSubmissionErrors_MissingAnnotationConflict(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions",
		`{"annotations":[{"id":"ghost","revision":1}],"note":"n"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("missing annotation status = %d, want 409; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "conflict" {
		t.Fatalf("missing annotation code = %q, want conflict", code)
	}
	if len(f.repo.submissions) != 0 {
		t.Fatalf("conflict must not persist, got %d submissions", len(f.repo.submissions))
	}
}

// repository 创建阶段（复核事务）返回 ErrRevisionConflict → conflict 409，零落库。
func TestSubmissionErrors_RepoStageRevisionConflict(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	seedAnnotation(t, f.repo, "a1", "t1", 1)
	f.repo.createSubErr = diffreview.ErrRevisionConflict
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions",
		`{"annotations":[{"id":"a1","revision":1}],"note":"n"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("repo-stage conflict status = %d, want 409; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "conflict" {
		t.Fatalf("repo-stage conflict code = %q, want conflict", code)
	}
	if len(f.repo.submissions) != 0 {
		t.Fatalf("conflict must not persist, got %d submissions", len(f.repo.submissions))
	}
}

func TestAnnotationCRUD_Errors(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	repo := f.repo
	// PATCH 未知批注 → not_found。
	resp, body := doDiffReviewReq(t, s, "PATCH", "/api/v1/tasks/t1/annotations/none", `{"comment":"x"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PATCH missing status = %d, want 404; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "not_found" {
		t.Fatalf("PATCH missing code = %q, want not_found", code)
	}
	// PATCH 空白 comment → invalid_input。
	seedAnnotation(t, repo, "a1", "t1", 3)
	resp, body = doDiffReviewReq(t, s, "PATCH", "/api/v1/tasks/t1/annotations/a1", `{"comment":"   "}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH blank status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "invalid_input" {
		t.Fatalf("PATCH blank code = %q, want invalid_input", code)
	}
	// PATCH 同值 → 200、revision 不变。
	resp, body = doDiffReviewReq(t, s, "PATCH", "/api/v1/tasks/t1/annotations/a1", `{"comment":"c"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH same status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var ann annotationDTO
	if err := json.Unmarshal([]byte(body), &ann); err != nil {
		t.Fatal(err)
	}
	if ann.Revision != 3 {
		t.Fatalf("PATCH same revision = %d, want 3 (unchanged)", ann.Revision)
	}
	// DELETE 未知批注 → not_found；已知 → 204。
	resp, body = doDiffReviewReq(t, s, "DELETE", "/api/v1/tasks/t1/annotations/none", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE missing status = %d, want 404; body=%s", resp.StatusCode, body)
	}
	resp, _ = doDiffReviewReq(t, s, "DELETE", "/api/v1/tasks/t1/annotations/a1", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
}

func TestSubmissionLifecycle_Errors(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	repo := f.repo
	mk := func(status string) {
		repo.submissions["s1"] = diffreview.DiffReviewSubmissionRecord{
			ID: "s1", TaskID: "t1", Status: status, Note: "n", CreatedAt: 1720000100,
		}
	}
	// 撤回：missing → 404；非 queued → invalid_state；queued → 204。
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions/s1/cancel", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cancel missing status = %d, want 404; body=%s", resp.StatusCode, body)
	}
	mk("sending")
	resp, body = doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions/s1/cancel", "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("cancel sending status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "invalid_state" {
		t.Fatalf("cancel sending code = %q, want invalid_state", code)
	}
	mk("queued")
	resp, _ = doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions/s1/cancel", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel queued status = %d, want 204", resp.StatusCode)
	}
	// 终态删除：sending → invalid_state；sent → 204。
	mk("sending")
	resp, body = doDiffReviewReq(t, s, "DELETE", "/api/v1/tasks/t1/annotation-submissions/s1", "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("delete sending status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	mk("sent")
	resp, _ = doDiffReviewReq(t, s, "DELETE", "/api/v1/tasks/t1/annotation-submissions/s1", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete sent status = %d, want 204", resp.StatusCode)
	}
}

// payload 组装不读取 diff（design.md D7 现行契约：不附加相关 diff/Context 段）：
// diff 来源读取失败不再阻断提交，有效批次仍 201 落库。
func TestSubmission_PayloadAssemblyIgnoresDiffSourceError(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	repo := f.repo
	seedAnnotation(t, repo, "a1", "t1", 1)
	f.diff.readErr = &application.OpError{Code: "git_error", Err: errors.New("git diff failed")}
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions",
		`{"annotations":[{"id":"a1","revision":1}],"note":"n"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (payload assembly must not read diff); body=%s", resp.StatusCode, body)
	}
	if len(repo.submissions) != 1 {
		t.Fatalf("submission should persist despite diff source error, got %d", len(repo.submissions))
	}
}

// --- wire 形状（design.md D8 DTO 逐字） ---

func TestAnnotationCreateAndList_WireShape(t *testing.T) {
	s, _ := newDiffReviewAPIServer(t)
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotations", validAnnotationBody("fix this"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	var created annotationDTO
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.Stale || created.Comment != "fix this" || created.Path != "a.go" || created.Side != "new" {
		t.Fatalf("created = %+v", created)
	}
	// 键集逐字（camelCase 全字段，无多余/缺失）。
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "path", "side", "ref", "untracked", "startLine", "endLine",
		"snapshotStartLine", "snapshotLineCount", "snapshot", "comment", "revision", "stale",
		"createdAt", "updatedAt"}
	if len(raw) != len(want) {
		t.Fatalf("created keys = %v, want exactly %v", keysOf(raw), want)
	}
	for _, k := range want {
		if _, ok := raw[k]; !ok {
			t.Fatalf("created missing key %q (got %v)", k, keysOf(raw))
		}
	}
	// GET 列表：annotations + submitCapability {state, reason}。
	resp, body = doDiffReviewReq(t, s, "GET", "/api/v1/tasks/t1/annotations", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d; body=%s", resp.StatusCode, body)
	}
	var list annotationsListResponse
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Annotations) != 1 {
		t.Fatalf("list annotations len = %d, want 1", len(list.Annotations))
	}
	got := list.Annotations[0]
	if got.Untracked {
		t.Fatalf("list annotations[0].untracked = true, want false (default from POST body)")
	}
	if got.Snapshot != "hello" || got.Comment != "fix this" {
		t.Fatalf("list annotations[0] = %+v, want snapshot/comment echoed from POST body", got)
	}
	if list.SubmitCapability.State != "supported" {
		t.Fatalf("submitCapability = %+v, want state supported", list.SubmitCapability)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAnnotationPatch_StaleWireTrue F9：已漂移批注的 PATCH 响应必须返回真实 stale=true
// （不得硬编码 false）。创建时 diff 与 snapshot 一致（active），随后 diff 变化再 PATCH。
func TestAnnotationPatch_StaleWireTrue(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotations", validAnnotationBody("first"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", resp.StatusCode, body)
	}
	var created annotationDTO
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.Stale {
		t.Fatalf("created stale = true, want false (diff 与 snapshot 一致)")
	}
	// diff 新侧内容变化 → 同一批注窗口不再匹配 snapshot → stale。
	f.diff.readResult.NewContent = "changed\n"
	resp, body = doDiffReviewReq(t, s, "PATCH", "/api/v1/tasks/t1/annotations/"+created.ID, `{"comment":"second"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d; body=%s", resp.StatusCode, body)
	}
	var updated annotationDTO
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatal(err)
	}
	if !updated.Stale {
		t.Fatalf("patched stale = false, want true（F9：PATCH 响应必须返回真实 stale）")
	}
	if updated.Comment != "second" || updated.Revision != 2 {
		t.Fatalf("patched = %+v, want comment=second revision=2", updated)
	}
}

func TestSubmissionCreateAndPartitions_WireShape(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	repo := f.repo
	seedAnnotation(t, repo, "a1", "t1", 1)
	body := `{"annotations":[{"id":"a1","revision":1}],"note":"please fix"}`
	resp, respBody := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/annotation-submissions", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create submission status = %d, want 201; body=%s", resp.StatusCode, respBody)
	}
	var sub submissionDTO
	if err := json.Unmarshal([]byte(respBody), &sub); err != nil {
		t.Fatal(err)
	}
	if sub.Status != "queued" || sub.SentAt != nil || sub.Note != "please fix" {
		t.Fatalf("submission = %+v (sentAt must be null when queued)", sub)
	}
	if sub.Payload == "" || sub.Truncated {
		t.Fatalf("submission payload/truncated = %q/%v", sub.Payload, sub.Truncated)
	}
	if len(sub.Items) != 1 || sub.Items[0].AnnotationID != "a1" || sub.Items[0].Snapshot != "hello" {
		t.Fatalf("items = %+v", sub.Items)
	}
	// SubmissionItem 键集：不含 snapshotLineCount（D8 DTO 注）。
	var rawItem map[string]any
	itemBytes, _ := json.Marshal(sub.Items[0])
	if err := json.Unmarshal(itemBytes, &rawItem); err != nil {
		t.Fatal(err)
	}
	wantItem := []string{"annotationId", "path", "side", "ref", "untracked", "startLine",
		"endLine", "snapshotStartLine", "snapshot", "comment"}
	if len(rawItem) != len(wantItem) {
		t.Fatalf("item keys = %v, want exactly %v", keysOf(rawItem), wantItem)
	}
	for _, k := range wantItem {
		if _, ok := rawItem[k]; !ok {
			t.Fatalf("item missing key %q", k)
		}
	}
	// 分区列表：queue/history/failures 键恒存在（空数组非 null）。
	resp, respBody = doDiffReviewReq(t, s, "GET", "/api/v1/tasks/t1/annotation-submissions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("partitions status = %d; body=%s", resp.StatusCode, respBody)
	}
	var parts map[string]json.RawMessage
	if err := json.Unmarshal([]byte(respBody), &parts); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"queue", "history", "failures"} {
		v, ok := parts[k]
		if !ok || string(v) == "null" {
			t.Fatalf("partitions[%s] missing or null: %s", k, respBody)
		}
	}
}

func TestGitFileRead_WireUnionShape(t *testing.T) {
	svcFileEdit := &fakeFileEditPort{}
	raw := []byte("hello\r\n")
	svcFileEdit.readRaw = func(ctx context.Context, taskID, path string) (diffreview.FileEditRawFile, error) {
		return diffreview.FileEditRawFile{Exists: true, Mode: 0o644, Bytes: raw}, nil
	}
	svc := diffreview.New(diffreview.Options{
		Repo:     newFakeDiffReviewRepo(),
		Scope:    &fakeDiffReviewScope{found: true, kind: "repo"},
		Runtime:  &fakeDiffReviewRuntime{capability: diffreview.CapabilitySupported},
		Diff:     &fakeDiffReviewDiffSource{},
		FileEdit: svcFileEdit,
	})
	cfg := testConfig()
	s := &Server{cfg: cfg, mux: http.NewServeMux(), auth: NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(), tasks: &fakeTaskBackend{}, diffreview: svc}
	s.registerRoutes()
	// editable=true 分支：六键逐字。
	resp, body := doDiffReviewReq(t, s, "GET", "/api/v1/tasks/t1/git/file?path=a.txt", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read status = %d; body=%s", resp.StatusCode, body)
	}
	var rawResp map[string]any
	if err := json.Unmarshal([]byte(body), &rawResp); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"editable", "content", "baseHash", "lineEnding", "hasBom", "mode"}
	if len(rawResp) != len(wantKeys) {
		t.Fatalf("editable keys = %v, want exactly %v", keysOf(rawResp), wantKeys)
	}
	var editable fileEditEditableDTO
	if err := json.Unmarshal([]byte(body), &editable); err != nil {
		t.Fatal(err)
	}
	if !editable.Editable || editable.Content != "hello\n" || editable.LineEnding != "crlf" ||
		editable.HasBom || editable.Mode != "0644" {
		t.Fatalf("editable = %+v", editable)
	}
	if editable.BaseHash != diffreview.SHA256Hex(raw) {
		t.Fatalf("baseHash = %q, want %q", editable.BaseHash, diffreview.SHA256Hex(raw))
	}
	// editable=false 分支：三键逐字（missing）。
	svcFileEdit.readRaw = func(ctx context.Context, taskID, path string) (diffreview.FileEditRawFile, error) {
		return diffreview.FileEditRawFile{Exists: false}, nil
	}
	resp, body = doDiffReviewReq(t, s, "GET", "/api/v1/tasks/t1/git/file?path=a.txt", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read missing status = %d; body=%s", resp.StatusCode, body)
	}
	rawResp = map[string]any{}
	if err := json.Unmarshal([]byte(body), &rawResp); err != nil {
		t.Fatal(err)
	}
	if len(rawResp) != 3 {
		t.Fatalf("not-editable keys = %v, want exactly [editable reasonCode reason]", keysOf(rawResp))
	}
	var notEditable fileEditNotEditableDTO
	if err := json.Unmarshal([]byte(body), &notEditable); err != nil {
		t.Fatal(err)
	}
	if notEditable.Editable || notEditable.ReasonCode != "missing" || notEditable.Reason == "" {
		t.Fatalf("notEditable = %+v", notEditable)
	}
}

func TestGitFileWrite_DomainFormatError(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	fileEdit := f.fileEdit
	// content 含 \r → service 步骤 1 领域校验 invalid_input，零写盘。
	body := fmt.Sprintf(`{"path":"a.txt","content":"a\r\nb","baseHash":"%s","lineEnding":"lf","baseMode":"0644"}`, strings.Repeat("a", 64))
	resp, respBody := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/git/file", body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("content with CR status = %d, want 422; body=%s", resp.StatusCode, respBody)
	}
	if code := errCodeOf(t, respBody); code != "invalid_input" {
		t.Fatalf("code = %q, want invalid_input", code)
	}
	if fileEdit.writes != 0 {
		t.Fatal("domain format error must not reach port Write (zero disk write)")
	}
}

// git/file 端点 *application.OpError 透传映射矩阵（mapDiffReviewErr → mapTaskErr，code 逐字透传）。
func TestGitFile_OpErrorPassthroughMatrix(t *testing.T) {
	s, f := newDiffReviewAPIServer(t)
	writeBody := fmt.Sprintf(`{"path":"a.txt","content":"x","baseHash":"%s","lineEnding":"lf","baseMode":"0644"}`, strings.Repeat("a", 64))
	for _, tc := range []struct {
		opCode     string
		wantStatus int
	}{
		{"invalid_input", http.StatusUnprocessableEntity},
		{"invalid_state", http.StatusUnprocessableEntity},
		{"conflict", http.StatusConflict},
		{"git_error", http.StatusUnprocessableEntity},
		{"internal", http.StatusInternalServerError},
	} {
		t.Run("write/"+tc.opCode, func(t *testing.T) {
			f.fileEdit.writeFn = func(ctx context.Context, taskID string, req diffreview.FileEditWriteRequest) (diffreview.FileEditWriteResult, error) {
				return diffreview.FileEditWriteResult{}, &application.OpError{Code: tc.opCode, Err: errors.New("adapter failure")}
			}
			resp, body := doDiffReviewReq(t, s, "POST", "/api/v1/tasks/t1/git/file", writeBody)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("op=%s status = %d, want %d; body=%s", tc.opCode, resp.StatusCode, tc.wantStatus, body)
			}
			if code := errCodeOf(t, body); code != tc.opCode {
				t.Fatalf("op=%s code = %q, want passthrough %q", tc.opCode, code, tc.opCode)
			}
		})
	}
	// 读取面：ReadRaw 返回非 FileEditReadRawError 的 *application.OpError → 原样透传。
	f.fileEdit.readRaw = func(ctx context.Context, taskID, path string) (diffreview.FileEditRawFile, error) {
		return diffreview.FileEditRawFile{}, &application.OpError{Code: "git_error", Err: errors.New("read raw failed")}
	}
	resp, body := doDiffReviewReq(t, s, "GET", "/api/v1/tasks/t1/git/file?path=a.txt", "")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("read op error status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	if code := errCodeOf(t, body); code != "git_error" {
		t.Fatalf("read op error code = %q, want git_error", code)
	}
}
