package api

// git-page-enhancements 任务 2.6：分支对比两个新端点的 API 测试。
//
// 覆盖：
//   - 路由接通（GET .../git/branch-diff/files?base= 与 .../git/branch-diff/file?base=&path=）。
//   - files 响应精确 JSON key 集（存在 baseRef/files，不存在任何 OID 字段）且 baseRef
//     精确等于请求原始 base（text ref 透传断言）。
//   - file 响应八字段契约与 base/path 参数透传。
//   - 各错误码 → HTTP 映射（沿用 TestGitAPI_ErrorMapping 断言风格）。
//
// 实际 git 执行（merge-base/numstat/blob 读取落盘）由 internal/task gitops_branch_diff_test.go
// 覆盖；此处仅断言 API 层：handler→backend 映射、参数透传、mapTaskErr 映射、响应 JSON 字段。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ocdeck/internal/application"
)

// TestGitAPI_BranchDiffFiles_JsonShape 验证 files 端点路由接通 + 精确 JSON key 集：
// 有 baseRef/files，无任何 OID 字段（headOID/mergeBaseOID/baseOID 等）；baseRef 精确等于
// 请求原始 base（text ref，不做规范化改写）。
func TestGitAPI_BranchDiffFiles_JsonShape(t *testing.T) {
	tb := newMockGitBackend()
	tb.branchFilesFn = func(ctx context.Context, taskID, base string) (application.GitBranchDiffFilesDTO, error) {
		if taskID != "tk1" {
			t.Errorf("taskID = %q, want tk1", taskID)
		}
		if base != "refs/heads/main" {
			t.Errorf("base passed = %q, want raw %q (transparent passthrough)", base, "refs/heads/main")
		}
		return application.GitBranchDiffFilesDTO{
			BaseRef: base,
			Files: []application.GitBranchDiffFileEntry{
				{Path: "a.txt", Additions: 2, Deletions: 1, IsBinary: false},
				{Path: "bin.dat", Additions: 0, Deletions: 0, IsBinary: true},
			},
		}, nil
	}
	s := newGitAPIServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/tk1/git/branch-diff/files?base=refs%2Fheads%2Fmain", ""))
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

	// 精确 JSON key 集：存在 baseRef/files，不存在任何 OID 字段（D2：OID 仅为服务端内部值）。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]bool{"baseRef": false, "files": false}
	for k := range raw {
		if _, ok := wantKeys[k]; !ok {
			t.Errorf("unexpected JSON key %q (OID field leaked?)", k)
		}
		wantKeys[k] = true
	}
	for k, seen := range wantKeys {
		if !seen {
			t.Errorf("missing JSON key %q", k)
		}
	}
	for _, oidKey := range []string{"baseOID", "headOID", "mergeBaseOID", "oid"} {
		if _, ok := raw[oidKey]; ok {
			t.Errorf("OID key %q present, want absent", oidKey)
		}
	}

	var res application.GitBranchDiffFilesDTO
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.BaseRef != "refs/heads/main" {
		t.Errorf("baseRef = %q, want exactly the raw request base %q", res.BaseRef, "refs/heads/main")
	}
	if len(res.Files) != 2 {
		t.Fatalf("files len = %d, want 2", len(res.Files))
	}
	if res.Files[0].Path != "a.txt" || res.Files[0].Additions != 2 || res.Files[0].Deletions != 1 || res.Files[0].IsBinary {
		t.Errorf("files[0] = %+v", res.Files[0])
	}
	if res.Files[1].Path != "bin.dat" || !res.Files[1].IsBinary || res.Files[1].Additions != 0 {
		t.Errorf("files[1] = %+v", res.Files[1])
	}
}

// TestGitAPI_BranchDiffFile_JsonShape 验证 file 端点路由接通 + GitDiffDTO 八字段契约
// （与 /git/diff 同款精确 key 集）+ base/path 参数透传。
func TestGitAPI_BranchDiffFile_JsonShape(t *testing.T) {
	tb := newMockGitBackend()
	tb.branchFileFn = func(ctx context.Context, taskID, base, path string) (application.GitDiffDTO, error) {
		if taskID != "tk1" {
			t.Errorf("taskID = %q, want tk1", taskID)
		}
		if base != "origin/main" || path != "a.txt" {
			t.Errorf("params = (%q, %q), want raw (origin/main, a.txt)", base, path)
		}
		return application.GitDiffDTO{
			OldContent: "old\n", NewContent: "new\n",
			OldExists: true, NewExists: true,
			OldMode: "100644", NewMode: "100644",
			IsBinary: false, Truncated: false,
		}, nil
	}
	s := newGitAPIServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/tk1/git/branch-diff/file?base=origin%2Fmain&path=a.txt", ""))
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
			t.Errorf("unexpected JSON key %q (eight-field contract broken?)", k)
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
	if d.OldContent != "old\n" || d.NewContent != "new\n" {
		t.Errorf("contents = (%q, %q)", d.OldContent, d.NewContent)
	}
}

// TestGitAPI_BranchDiff_ErrorMapping 覆盖两新端点 not_found/conflict/invalid_input/
// invalid_state/git_error → HTTP 映射（mapTaskErr 统一路径，与既有 git 端点一致）。
func TestGitAPI_BranchDiff_ErrorMapping(t *testing.T) {
	opErr := func(code, msg string) error {
		return &application.OpError{Code: code, Err: strErr(msg)}
	}
	cases := []struct {
		name   string
		path   string
		inject func(*mockGitBackend)
		want   int
		code   ErrorCode
	}{
		// files 端点
		{"files not_found", "/api/v1/tasks/nope/git/branch-diff/files?base=main",
			func(b *mockGitBackend) {
				b.branchFilesFn = func(context.Context, string, string) (application.GitBranchDiffFilesDTO, error) {
					return application.GitBranchDiffFilesDTO{}, opErr("not_found", "task not found")
				}
			},
			http.StatusNotFound, CodeNotFound},
		{"files conflict (task busy)", "/api/v1/tasks/tk1/git/branch-diff/files?base=main",
			func(b *mockGitBackend) {
				b.branchFilesFn = func(context.Context, string, string) (application.GitBranchDiffFilesDTO, error) {
					return application.GitBranchDiffFilesDTO{}, opErr("conflict", "task busy")
				}
			},
			http.StatusConflict, CodeConflict},
		{"files invalid_input (empty base)", "/api/v1/tasks/tk1/git/branch-diff/files",
			func(b *mockGitBackend) {
				b.branchFilesFn = func(context.Context, string, string) (application.GitBranchDiffFilesDTO, error) {
					return application.GitBranchDiffFilesDTO{}, opErr("invalid_input", "branch diff requires a base ref")
				}
			},
			http.StatusUnprocessableEntity, CodeInvalidInput},
		{"files invalid_state (unborn HEAD)", "/api/v1/tasks/tk1/git/branch-diff/files?base=main",
			func(b *mockGitBackend) {
				b.branchFilesFn = func(context.Context, string, string) (application.GitBranchDiffFilesDTO, error) {
					return application.GitBranchDiffFilesDTO{}, opErr("invalid_state", "unborn HEAD")
				}
			},
			http.StatusUnprocessableEntity, CodeInvalidState},
		{"files invalid_state (no merge base)", "/api/v1/tasks/tk1/git/branch-diff/files?base=main",
			func(b *mockGitBackend) {
				b.branchFilesFn = func(context.Context, string, string) (application.GitBranchDiffFilesDTO, error) {
					return application.GitBranchDiffFilesDTO{}, opErr("invalid_state", "no merge base")
				}
			},
			http.StatusUnprocessableEntity, CodeInvalidState},
		{"files git_error", "/api/v1/tasks/tk1/git/branch-diff/files?base=main",
			func(b *mockGitBackend) {
				b.branchFilesFn = func(context.Context, string, string) (application.GitBranchDiffFilesDTO, error) {
					return application.GitBranchDiffFilesDTO{}, opErr("git_error", "fatal: bad object")
				}
			},
			http.StatusUnprocessableEntity, CodeGitError},
		// file 端点
		{"file not_found", "/api/v1/tasks/nope/git/branch-diff/file?base=main&path=a.txt",
			func(b *mockGitBackend) {
				b.branchFileFn = func(context.Context, string, string, string) (application.GitDiffDTO, error) {
					return application.GitDiffDTO{}, opErr("not_found", "task not found")
				}
			},
			http.StatusNotFound, CodeNotFound},
		{"file conflict", "/api/v1/tasks/tk1/git/branch-diff/file?base=main&path=a.txt",
			func(b *mockGitBackend) {
				b.branchFileFn = func(context.Context, string, string, string) (application.GitDiffDTO, error) {
					return application.GitDiffDTO{}, opErr("conflict", "task busy")
				}
			},
			http.StatusConflict, CodeConflict},
		{"file invalid_input (path not in change set)", "/api/v1/tasks/tk1/git/branch-diff/file?base=main&path=unchanged.txt",
			func(b *mockGitBackend) {
				b.branchFileFn = func(context.Context, string, string, string) (application.GitDiffDTO, error) {
					return application.GitDiffDTO{}, opErr("invalid_input", "path not changed")
				}
			},
			http.StatusUnprocessableEntity, CodeInvalidInput},
		{"file invalid_state (unborn HEAD)", "/api/v1/tasks/tk1/git/branch-diff/file?base=main&path=a.txt",
			func(b *mockGitBackend) {
				b.branchFileFn = func(context.Context, string, string, string) (application.GitDiffDTO, error) {
					return application.GitDiffDTO{}, opErr("invalid_state", "unborn HEAD")
				}
			},
			http.StatusUnprocessableEntity, CodeInvalidState},
		{"file git_error (old side read)", "/api/v1/tasks/tk1/git/branch-diff/file?base=main&path=a.txt",
			func(b *mockGitBackend) {
				b.branchFileFn = func(context.Context, string, string, string) (application.GitDiffDTO, error) {
					return application.GitDiffDTO{}, opErr("git_error", "fatal: bad object abc")
				}
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

			resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+c.path, ""))
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

