package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ocdeck/internal/application"
)

// mockGitBackend 嵌入 fakeTaskBackend 并允许注入 git 方法的返回值，
// 供 git API 测试验证 handler→backend 映射与错误码透传（前端契约不变）。
// 实际 git 执行（status/diff/commit/push 落盘）由 internal/task gitops 测试覆盖；
// 此处仅断言 API 层：经 mock backend、mapTaskErr 映射、响应 JSON 字段。
type mockGitBackend struct {
	*fakeTaskBackend
	statusFn  func(ctx context.Context, taskID string) (application.GitStatusDTO, error)
	diffFn    func(ctx context.Context, taskID, ref, path string, untracked bool) (application.GitDiffDTO, error)
	commitFn  func(ctx context.Context, taskID, message string, paths []string) error
	pushFn    func(ctx context.Context, taskID string) error
	commitMsg string
	commitPth []string
}

func newMockGitBackend() *mockGitBackend {
	return &mockGitBackend{fakeTaskBackend: &fakeTaskBackend{}}
}

func (m *mockGitBackend) GitStatus(ctx context.Context, taskID string) (application.GitStatusDTO, error) {
	if m.statusFn != nil {
		return m.statusFn(ctx, taskID)
	}
	return application.GitStatusDTO{}, nil
}

func (m *mockGitBackend) GitDiff(ctx context.Context, taskID, ref, path string, untracked bool) (application.GitDiffDTO, error) {
	if m.diffFn != nil {
		return m.diffFn(ctx, taskID, ref, path, untracked)
	}
	return application.GitDiffDTO{}, nil
}

func (m *mockGitBackend) GitCommit(ctx context.Context, taskID, message string, paths []string) error {
	m.commitMsg = message
	m.commitPth = paths
	if m.commitFn != nil {
		return m.commitFn(ctx, taskID, message, paths)
	}
	return nil
}

func (m *mockGitBackend) GitPush(ctx context.Context, taskID string) error {
	if m.pushFn != nil {
		return m.pushFn(ctx, taskID)
	}
	return nil
}

// gitTaskBackend 为 env/task API 测试提供可返回固定 TaskRow 的 TaskBackend。
// （git API 测试改用 mockGitBackend 注入 git 方法，不再经真实 worktree。）
type gitTaskBackend struct {
	*fakeTaskBackend
	tasks    map[string]application.TaskRow
	statusFn func(ctx context.Context, taskID string) string
}

func newGitTaskBackend(rows ...application.TaskRow) *gitTaskBackend {
	g := &gitTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		tasks:           map[string]application.TaskRow{},
	}
	for _, r := range rows {
		g.tasks[r.ID] = r
	}
	return g
}

func (g *gitTaskBackend) Get(ctx context.Context, taskID string) (application.TaskRow, error) {
	r, ok := g.tasks[taskID]
	if !ok {
		return application.TaskRow{}, &application.OpError{Code: "not_found", Err: errNotFound(taskID)}
	}
	return r, nil
}

func (g *gitTaskBackend) List(ctx context.Context, projectID string) ([]application.TaskRow, error) {
	var out []application.TaskRow
	for _, r := range g.tasks {
		if r.ProjectID == projectID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (g *gitTaskBackend) AgentStatus(ctx context.Context, taskID string) string {
	if g.statusFn != nil {
		return g.statusFn(ctx, taskID)
	}
	return ""
}

func errNotFound(id string) error {
	return &notFoundErr{id: id}
}

type notFoundErr struct{ id string }

func (e *notFoundErr) Error() string { return "task not found: " + e.id }

// newGitAPIServer 构造带 git backend 的 Server。
func newGitAPIServer(t *testing.T, tb TaskBackend) *Server {
	t.Helper()
	return newAPITestServer(t, tb)
}

func TestGitAPI_Status_JsonShape(t *testing.T) {
	tb := newMockGitBackend()
	tb.statusFn = func(ctx context.Context, taskID string) (application.GitStatusDTO, error) {
		return application.GitStatusDTO{
			Branch: "feature/x",
			Files: []application.GitFileDTO{
				{Path: "a.txt", X: " ", Y: "M", Staged: false, Unstaged: true, Untracked: false, Additions: 1, Deletions: 0, IsBinary: false},
				{Path: "b.txt", X: "?", Y: "?", Staged: false, Unstaged: false, Untracked: true, Additions: 0, Deletions: 0, IsBinary: false},
			},
		}, nil
	}
	s := newGitAPIServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/tk1/git/status", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var st gitStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Branch != "feature/x" {
		t.Errorf("branch = %q, want feature/x", st.Branch)
	}
	if len(st.Files) != 2 {
		t.Fatalf("files len = %d, want 2", len(st.Files))
	}
	// 字段名/类型契约（与改动前一致）。
	if st.Files[0].Path != "a.txt" || st.Files[0].Y != "M" || !st.Files[0].Unstaged || st.Files[0].Additions != 1 {
		t.Errorf("files[0] = %+v", st.Files[0])
	}
	if !st.Files[1].Untracked || st.Files[1].X != "?" || st.Files[1].Y != "?" {
		t.Errorf("files[1] = %+v", st.Files[1])
	}
}

func TestGitAPI_Diff_JsonShape(t *testing.T) {
	tb := newMockGitBackend()
	tb.diffFn = func(ctx context.Context, taskID, ref, path string, untracked bool) (application.GitDiffDTO, error) {
		return application.GitDiffDTO{
			OldContent: "old v1\n", NewContent: "new v2\n",
			OldExists: true, NewExists: true,
			OldMode: "100644", NewMode: "100755",
			IsBinary: false, Truncated: false,
		}, nil
	}
	s := newGitAPIServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/tk1/git/diff?ref=HEAD&path=a.txt", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// 断言精确键集（codemirror-git-diff 八字段契约，旧 "diff" 字段已移除）。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]bool{
		"oldContent": false, "newContent": false,
		"oldExists": false, "newExists": false,
		"oldMode": false, "newMode": false,
		"isBinary": false, "truncated": false,
	}
	for k := range raw {
		if _, ok := wantKeys[k]; !ok {
			t.Errorf("unexpected JSON key %q (old single-field diff contract leaked?)", k)
		}
		wantKeys[k] = true
	}
	for k, seen := range wantKeys {
		if !seen {
			t.Errorf("missing JSON key %q", k)
		}
	}
	var d gitDiffResponse
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	if d.OldContent != "old v1\n" {
		t.Errorf("oldContent = %q, want 'old v1\\n'", d.OldContent)
	}
	if d.NewContent != "new v2\n" {
		t.Errorf("newContent = %q, want 'new v2\\n'", d.NewContent)
	}
	if !d.OldExists || !d.NewExists {
		t.Errorf("exists flags = (old=%v,new=%v), want both true", d.OldExists, d.NewExists)
	}
	if d.OldMode != "100644" || d.NewMode != "100755" {
		t.Errorf("modes = (old=%q,new=%q), want (100644,100755)", d.OldMode, d.NewMode)
	}
	if d.IsBinary {
		t.Errorf("isBinary = true, want false")
	}
	if d.Truncated {
		t.Errorf("truncated = true, want false")
	}
}

func TestGitAPI_Commit_OK(t *testing.T) {
	tb := newMockGitBackend()
	var gotMsg string
	var gotPaths []string
	tb.commitFn = func(ctx context.Context, taskID, message string, paths []string) error {
		gotMsg = message
		gotPaths = paths
		return nil
	}
	s := newGitAPIServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/tk1/git/commit", `{"message":"add a","paths":["a.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotMsg != "add a" {
		t.Errorf("message passed = %q, want 'add a'", gotMsg)
	}
	if len(gotPaths) != 1 || gotPaths[0] != "a.txt" {
		t.Errorf("paths passed = %v, want [a.txt]", gotPaths)
	}
}

func TestGitAPI_Push_OK(t *testing.T) {
	tb := newMockGitBackend()
	called := false
	tb.pushFn = func(ctx context.Context, taskID string) error {
		called = true
		if taskID != "tk1" {
			t.Errorf("taskID = %q, want tk1", taskID)
		}
		return nil
	}
	s := newGitAPIServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/tk1/git/push", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Error("GitPush not called")
	}
}

// TestGitAPI_ErrorMapping 覆盖 not_found/conflict/invalid_input/git_error 映射。
func TestGitAPI_ErrorMapping(t *testing.T) {
	opErr := func(code, msg string) error {
		return &application.OpError{Code: code, Err: strErr(msg)}
	}
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		inject func(*mockGitBackend)
		want   int
		code   ErrorCode
	}{
		// status
		{"status not_found", "GET", "/api/v1/tasks/nope/git/status", "",
			func(b *mockGitBackend) {
				b.statusFn = func(context.Context, string) (application.GitStatusDTO, error) {
					return application.GitStatusDTO{}, opErr("not_found", "task not found")
				}
			},
			http.StatusNotFound, CodeNotFound},
		{"status conflict", "GET", "/api/v1/tasks/tk1/git/status", "",
			func(b *mockGitBackend) {
				b.statusFn = func(context.Context, string) (application.GitStatusDTO, error) {
					return application.GitStatusDTO{}, opErr("conflict", "task busy")
				}
			},
			http.StatusConflict, CodeConflict},
		{"status git_error", "GET", "/api/v1/tasks/tk1/git/status", "",
			func(b *mockGitBackend) {
				b.statusFn = func(context.Context, string) (application.GitStatusDTO, error) {
					return application.GitStatusDTO{}, opErr("git_error", "fatal: not a git repo")
				}
			},
			http.StatusUnprocessableEntity, CodeGitError},
		// diff
		{"diff not_found", "GET", "/api/v1/tasks/nope/git/diff?ref=HEAD&path=a.txt", "",
			func(b *mockGitBackend) {
				b.diffFn = func(context.Context, string, string, string, bool) (application.GitDiffDTO, error) {
					return application.GitDiffDTO{}, opErr("not_found", "task not found")
				}
			},
			http.StatusNotFound, CodeNotFound},
		// I4：生产 Manager 词法校验先行，空 path 不可达 git_error；请求补 path=a.txt 保持可达形态。
		{"diff git_error", "GET", "/api/v1/tasks/tk1/git/diff?path=a.txt", "",
			func(b *mockGitBackend) {
				b.diffFn = func(context.Context, string, string, string, bool) (application.GitDiffDTO, error) {
					return application.GitDiffDTO{}, opErr("git_error", "fatal: bad ref")
				}
			},
			http.StatusUnprocessableEntity, CodeGitError},
		// codemirror-git-diff：unmerged index path → invalid_state；新侧非 ENOENT IO 错误 → internal。
		{"diff invalid_state (unmerged path)", "GET", "/api/v1/tasks/tk1/git/diff?path=a.txt", "",
			func(b *mockGitBackend) {
				b.diffFn = func(context.Context, string, string, string, bool) (application.GitDiffDTO, error) {
					return application.GitDiffDTO{}, opErr("invalid_state", "unmerged path in index")
				}
			},
			http.StatusUnprocessableEntity, CodeInvalidState},
		{"diff internal (new side IO error)", "GET", "/api/v1/tasks/tk1/git/diff?path=a.txt", "",
			func(b *mockGitBackend) {
				b.diffFn = func(context.Context, string, string, string, bool) (application.GitDiffDTO, error) {
					return application.GitDiffDTO{}, opErr("internal", "open \"a.txt\": permission denied")
				}
			},
			http.StatusInternalServerError, CodeInternal},
		// commit
		{"commit not_found", "POST", "/api/v1/tasks/nope/git/commit", `{"message":"m"}`,
			func(b *mockGitBackend) {
				b.commitFn = func(context.Context, string, string, []string) error { return opErr("not_found", "task not found") }
			},
			http.StatusNotFound, CodeNotFound},
		{"commit invalid_input (empty msg from backend)", "POST", "/api/v1/tasks/tk1/git/commit", `{"message":"","paths":[]}`,
			func(b *mockGitBackend) {
				b.commitFn = func(context.Context, string, string, []string) error {
					return opErr("invalid_input", "commit message is required")
				}
			},
			http.StatusUnprocessableEntity, CodeInvalidInput},
		{"commit conflict", "POST", "/api/v1/tasks/tk1/git/commit", `{"message":"m"}`,
			func(b *mockGitBackend) {
				b.commitFn = func(context.Context, string, string, []string) error { return opErr("conflict", "task busy") }
			},
			http.StatusConflict, CodeConflict},
		{"commit git_error", "POST", "/api/v1/tasks/tk1/git/commit", `{"message":"m"}`,
			func(b *mockGitBackend) {
				b.commitFn = func(context.Context, string, string, []string) error {
					return opErr("git_error", "fatal: nothing to commit")
				}
			},
			http.StatusUnprocessableEntity, CodeGitError},
		// push
		{"push not_found", "POST", "/api/v1/tasks/nope/git/push", "",
			func(b *mockGitBackend) {
				b.pushFn = func(context.Context, string) error { return opErr("not_found", "task not found") }
			},
			http.StatusNotFound, CodeNotFound},
		{"push conflict", "POST", "/api/v1/tasks/tk1/git/push", "",
			func(b *mockGitBackend) {
				b.pushFn = func(context.Context, string) error { return opErr("conflict", "task busy") }
			},
			http.StatusConflict, CodeConflict},
		{"push git_error (detached HEAD 透传 stderr)", "POST", "/api/v1/tasks/tk1/git/push", "",
			func(b *mockGitBackend) {
				b.pushFn = func(context.Context, string) error { return opErr("git_error", "fatal: HEAD is detached") }
			},
			http.StatusUnprocessableEntity, CodeGitError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tb := newMockGitBackend()
			c.inject(tb)
			s := newGitAPIServer(t, tb)
			ts := httptest.NewServer(s.mux)
			defer ts.Close()

			resp, err := http.DefaultClient.Do(authedReq(c.method, ts.URL+c.path, c.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.want)
			}
			var eb errorBody
			if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
				t.Fatal(err)
			}
			if eb.Error.Code != c.code {
				t.Errorf("code = %s, want %s (msg=%q)", eb.Error.Code, c.code, eb.Error.Message)
			}
		})
	}
}

// TestGitAPI_Commit_InvalidJSONBody 验证 decodeJSON 失败→invalid_input。
func TestGitAPI_Commit_InvalidJSONBody(t *testing.T) {
	tb := newGitTaskBackend() // gitTaskBackend 仍提供 Get 等基础方法
	s := newGitAPIServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/tk1/git/commit", "{bad json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeInvalidInput {
		t.Errorf("code = %s, want invalid_input", eb.Error.Code)
	}
}

type strErr string

func (e strErr) Error() string { return string(e) }

// --- fix-git-diff-new-file-and-linenum 任务 1.6：untracked 查询参数契约 ---

// TestGitAPI_Diff_UntrackedParam 验证 untracked 查询参数值域：absent/0/1 透传，非法值 invalid_input。
func TestGitAPI_Diff_UntrackedParam(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantUntr   bool
		wantStatus int
		wantCode   ErrorCode
	}{
		{"absent unchanged", "", false, http.StatusOK, ""},
		{"0 unchanged", "untracked=0", false, http.StatusOK, ""},
		{"1 passthrough", "untracked=1", true, http.StatusOK, ""},
		{"illegal value", "untracked=2", false, http.StatusUnprocessableEntity, CodeInvalidInput},
		{"illegal yes", "untracked=yes", false, http.StatusUnprocessableEntity, CodeInvalidInput},
		{"explicit empty value", "untracked=", false, http.StatusUnprocessableEntity, CodeInvalidInput},
		{"bare key no value", "untracked", false, http.StatusUnprocessableEntity, CodeInvalidInput},
		{"duplicate params", "untracked=0&untracked=1", false, http.StatusUnprocessableEntity, CodeInvalidInput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tb := newMockGitBackend()
			var gotUntracked bool
			var called bool
			tb.diffFn = func(ctx context.Context, taskID, ref, path string, untracked bool) (application.GitDiffDTO, error) {
				called = true
				gotUntracked = untracked
				return application.GitDiffDTO{
					OldContent: "", NewContent: "ok\n",
					OldExists: false, NewExists: true, IsBinary: false, Truncated: false,
				}, nil
			}
			s := newGitAPIServer(t, tb)
			ts := httptest.NewServer(s.mux)
			defer ts.Close()

			url := ts.URL + "/api/v1/tasks/tk1/git/diff"
			if c.query != "" {
				url += "?" + c.query
			}
			resp, err := http.DefaultClient.Do(authedReq("GET", url, ""))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.wantStatus)
			}
			if c.wantStatus != http.StatusOK {
				// 非法值 MUST 在 handler 内拒绝，不调用 backend（词法校验先于锁/git）。
				if called {
					t.Errorf("illegal untracked param: backend called, want zero calls")
				}
				var eb errorBody
				if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
					t.Fatal(err)
				}
				if eb.Error.Code != c.wantCode {
					t.Errorf("code = %s, want %s", eb.Error.Code, c.wantCode)
				}
				return
			}
			if !called {
				t.Fatalf("backend not called for valid query")
			}
			if gotUntracked != c.wantUntr {
				t.Errorf("untracked passed = %v, want %v", gotUntracked, c.wantUntr)
			}
		})
	}
}

// TestGitAPI_Diff_UntrackedInvariants 验证 Manager 层 invariants 经 mapTaskErr 映射：
// untracked=1 & path 空 → invalid_input；untracked=1 & ref 非空 → invalid_input。
// 通过 mock backend 注入 invalid_input OpError 模拟 Manager 层拒绝，并断言收到的参数透传正确。
func TestGitAPI_Diff_UntrackedInvariants(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		ref      string
		wantPath string
		wantRef  string
		wantUntr bool
	}{
		{"untracked+empty path invalid_input", "", "", "", "", true},
		{"untracked+non-empty ref invalid_input", "a.txt", "HEAD", "a.txt", "HEAD", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tb := newMockGitBackend()
			var gotPath, gotRef string
			var gotUntracked bool
			var called bool
			tb.diffFn = func(ctx context.Context, taskID, ref, path string, untracked bool) (application.GitDiffDTO, error) {
				called = true
				gotPath = path
				gotRef = ref
				gotUntracked = untracked
				return application.GitDiffDTO{}, &application.OpError{Code: "invalid_input", Err: strErr("invariant rejected")}
			}
			s := newGitAPIServer(t, tb)
			ts := httptest.NewServer(s.mux)
			defer ts.Close()

			q := "untracked=1"
			if c.path != "" {
				q += "&path=" + c.path
			}
			if c.ref != "" {
				q += "&ref=" + c.ref
			}
			resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/tk1/git/diff?"+q, ""))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", resp.StatusCode)
			}
			var eb errorBody
			if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
				t.Fatal(err)
			}
			if eb.Error.Code != CodeInvalidInput {
				t.Errorf("code = %s, want invalid_input", eb.Error.Code)
			}
			// 参数透传断言：防止 API 层误改/丢弃参数导致回归测试仍绿。
			if !called {
				t.Fatal("diffFn not called; backend params not transparently passed")
			}
			if gotPath != c.wantPath {
				t.Errorf("path passed = %q, want %q", gotPath, c.wantPath)
			}
			if gotRef != c.wantRef {
				t.Errorf("ref passed = %q, want %q", gotRef, c.wantRef)
			}
			if gotUntracked != c.wantUntr {
				t.Errorf("untracked passed = %v, want %v", gotUntracked, c.wantUntr)
			}
		})
	}
}
