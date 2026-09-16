// task_info_patch_test.go 覆盖 PATCH /api/v1/tasks/{id} handler 与
// decodeOptionalTaskInfoPatchJSON 错误矩阵（task-info-editable 3.4）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ocdeck/internal/application"
)

func newTaskInfoPatchServer(t *testing.T, tb TaskBackend) (*Server, *httptest.Server) {
	t.Helper()
	s := newAPITestServer(t, tb)
	s.projs = newFakeProjectStore()
	if err := s.projs.CreateProject(context.Background(), "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	return s, ts
}

func doPatch(t *testing.T, ts *httptest.Server, body string) (int, string, *errorBody) {
	t.Helper()
	req := authedReq("PATCH", ts.URL+"/api/v1/tasks/t1", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var eb errorBody
	_ = json.Unmarshal(raw, &eb)
	return resp.StatusCode, string(raw), &eb
}

func TestPatchTaskInfo_EmptyBody_Idempotent200(t *testing.T) {
	tb := &fakeTaskBackend{}
	_, ts := newTaskInfoPatchServer(t, tb)
	for _, body := range []string{"", "   ", "{}", `{"name":null,"branch_slug":null}`} {
		status, raw, eb := doPatch(t, ts, body)
		if status != http.StatusOK {
			t.Fatalf("PATCH %q status = %d, want 200 (body=%s)", body, status, raw)
		}
		if eb.Error.Code != "" {
			t.Fatalf("PATCH %q unexpected error envelope: %v", body, eb.Error)
		}
		// 幂等成功也经过 backend（零值 options → 全部同值路径）。
		calls := tb.updateInfoCallsSnapshot()
		if len(calls) == 0 {
			t.Fatalf("PATCH %q: backend not called", body)
		}
		last := calls[len(calls)-1]
		if last.Name != nil || last.BranchSlug != nil {
			t.Fatalf("PATCH %q: options = %+v, want nil pointers (not provided)", body, last)
		}
	}
}

func TestPatchTaskInfo_PresencePreserved(t *testing.T) {
	tb := &fakeTaskBackend{}
	_, ts := newTaskInfoPatchServer(t, tb)

	status, raw, _ := doPatch(t, ts, `{"name":"New Name","branch_slug":"feature/x"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", status, raw)
	}
	calls := tb.updateInfoCallsSnapshot()
	last := calls[len(calls)-1]
	if last.Name == nil || *last.Name != "New Name" || last.BranchSlug == nil || *last.BranchSlug != "feature/x" {
		t.Fatalf("options = %+v, want pointers preserved", last)
	}
	if tb.updateInfoTaskID != "t1" {
		t.Fatalf("task id = %q, want t1", tb.updateInfoTaskID)
	}

	// 仅提供单字段：另一字段保持 nil（不修改）。
	status, _, _ = doPatch(t, ts, `{"name":"Only"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	calls2 := tb.updateInfoCallsSnapshot()
	last = calls2[len(calls2)-1]
	if last.Name == nil || *last.Name != "Only" || last.BranchSlug != nil {
		t.Fatalf("options = %+v, want only Name provided", last)
	}
}

func TestPatchTaskInfo_DecodeErrorMatrix(t *testing.T) {
	_, ts := newTaskInfoPatchServer(t, &fakeTaskBackend{})
	cases := []struct {
		name string
		body string
	}{
		{"invalid json", "{"},
		{"trailing value", `{} {}`},
		{"type error name", `{"name":123}`},
		{"type error slug", `{"branch_slug":true}`},
		{"top-level array", `["x"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw, eb := doPatch(t, ts, tc.body)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body=%s)", status, raw)
			}
			if eb.Error.Code != CodeInvalidInput {
				t.Fatalf("code = %q, want invalid_input", eb.Error.Code)
			}
		})
	}
}

func TestPatchTaskInfo_ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		opCode     string
		wantStatus int
		wantCode   ErrorCode
	}{
		{"invalid_input", "invalid_input", http.StatusUnprocessableEntity, CodeInvalidInput},
		{"invalid_state", "invalid_state", http.StatusUnprocessableEntity, CodeInvalidState},
		{"conflict", "conflict", http.StatusConflict, CodeConflict},
		{"git_error", "git_error", http.StatusUnprocessableEntity, CodeGitError},
		{"internal", "internal", http.StatusInternalServerError, CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tb := &fakeTaskBackend{updateInfoErr: &application.OpError{Code: tc.opCode, Err: errors.New("boom")}}
			_, ts := newTaskInfoPatchServer(t, tb)
			status, _, eb := doPatch(t, ts, `{"name":"X"}`)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if eb.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", eb.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestPatchTaskInfo_NotFound(t *testing.T) {
	tb := &fakeTaskBackend{updateInfoErr: &application.OpError{Code: "not_found", Err: errors.New("missing")}}
	_, ts := newTaskInfoPatchServer(t, tb)
	status, _, eb := doPatch(t, ts, `{"name":"X"}`)
	if status != http.StatusNotFound || eb.Error.Code != CodeNotFound {
		t.Fatalf("status = %d code = %q, want 404/not_found", status, eb.Error.Code)
	}
}

// TestPatchTaskInfo_AttentionArraysNotNull 回归断言（黑屏 bug）：PATCH 成功响应的
// attention 必须按 GET 同款填充。taskRowDTO.Attention 为值类型且无 omitempty，未填充时
// 序列化为 null 数组，前端读 questions.length 抛 TypeError；空集合契约为非 null 空数组
//（attentionDTO spec，design.md D6）。
func TestPatchTaskInfo_AttentionArraysNotNull(t *testing.T) {
	tb := &fakeTaskBackend{updateInfoRes: application.TaskRow{
		ID: "t1", ProjectID: "p1", Name: "n", Branch: "ocdeck/a", Status: "suspended",
		Mode: "worktree", PermissionMode: "ask",
	}}
	_, ts := newTaskInfoPatchServer(t, tb)
	status, raw, _ := doPatch(t, ts, `{"name":"X"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", status, raw)
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, raw)
	}
	att, ok := generic["attention"].(map[string]any)
	if !ok {
		t.Fatalf("attention missing or not object: %s", raw)
	}
	for _, key := range []string{"permissions", "questions"} {
		if _, ok := att[key].([]any); !ok {
			t.Fatalf("attention.%s = %v, want JSON array (non-null)", key, att[key])
		}
	}
}

// TestPatchTaskInfo_NoRenamePendingLeak 回归断言：任务 DTO 不含 rename_pending 字段
// （task-info-editable：意图仅内部消费，MUST NOT 进入公共 DTO）。
func TestPatchTaskInfo_NoRenamePendingLeak(t *testing.T) {
	tb := &fakeTaskBackend{updateInfoRes: application.TaskRow{
		ID: "t1", ProjectID: "p1", Name: "n", Branch: "ocdeck/a", Status: "suspended",
		Mode: "worktree", PermissionMode: "ask",
	}}
	_, ts := newTaskInfoPatchServer(t, tb)
	_, raw, _ := doPatch(t, ts, `{"name":"X"}`)
	if len(raw) == 0 {
		t.Fatal("empty response body")
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, raw)
	}
	if _, ok := generic["rename_pending"]; ok {
		t.Fatalf("response leaks rename_pending: %s", raw)
	}
}
