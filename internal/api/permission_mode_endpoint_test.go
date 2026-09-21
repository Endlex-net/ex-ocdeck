// permission_mode_endpoint_test.go 权限模式独立端点（task-permission-mode D4/tasks
// 4.1/4.3，spec「权限模式创建后变更」）：请求错误矩阵全行（422 含未知字段/类型/尾随
// JSON/缺失/null/超限/非三值，零副作用）、404（ErrNoRows 语义）/409/500（非 ErrNoRows
// 读错误不误包 404）、成功 200 固定两字段（含 DR1 不一致视图）、透出范围负向
// （创建/列表/摘要/active 概览不含 effective_permission_mode）。
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ocdeck/internal/application"
)

// permissionEndpointFixture 端点测试装置：返回注入了预设视图/错误的 fake 与服务器。
func permissionEndpointFixture(t *testing.T) (*fakeTaskBackend, *httptest.Server) {
	t.Helper()
	tb := &fakeTaskBackend{}
	_, ts := newPermissionServer(t, tb)
	return tb, ts
}

// TestPermissionModeEndpoint_ErrorMatrix D4 失败矩阵全行：422 零副作用（后端未被
// 调用）、404/409/500 按协调器 OpError 码映射、非 ErrNoRows 读错误不误包 404。
func TestPermissionModeEndpoint_ErrorMatrix(t *testing.T) {
	t.Run("422 请求矩阵且零副作用", func(t *testing.T) {
		cases := []struct {
			name string
			body string
		}{
			// 值域行（空串/纯空白/trim 后非三值）由 task 层协调器校验，见协调器
			// 单测与本文件 invalid_input 注入行——fake 不经过协调器。
			{name: "空 body", body: ""},
			{name: "纯空白 body", body: "   "},
			{name: "缺失字段", body: `{"name":"x"}`},
			{name: "null", body: `{"permission_mode":null}`},
			{name: "类型错误", body: `{"permission_mode":3}`},
			{name: "未知字段拒绝", body: `{"permission_mode":"ask","extra":1}`},
			{name: "尾随 JSON", body: `{"permission_mode":"ask"} {"permission_mode":"ask"}`},
			{name: "非法 JSON", body: `{`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tb, ts := permissionEndpointFixture(t)
				resp, err := http.DefaultClient.Do(authedReq("PATCH", ts.URL+"/api/v1/tasks/t1/permission-mode", tc.body))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422 (body=%q)", resp.StatusCode, tc.body)
				}
				var eb errorBody
				if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
					t.Fatal(err)
				}
				if eb.Error.Code != CodeInvalidInput {
					t.Errorf("error code = %q, want invalid_input", eb.Error.Code)
				}
				if taskID, mode := tb.permissionModeCallsSnapshot(); taskID != "" || mode != "" {
					t.Errorf("backend must not be called on 422, got (%q, %q)", taskID, mode)
				}
			})
		}
	})
	t.Run("invalid_input 注入（协调器值域校验的映射）422", func(t *testing.T) {
		tb, ts := permissionEndpointFixture(t)
		tb.SetPermissionModeView(application.PermissionModeView{},
			application.NewOpErr(application.CodeInvalidInput, errors.New("unknown permission mode")))
		resp, err := http.DefaultClient.Do(authedReq("PATCH", ts.URL+"/api/v1/tasks/t1/permission-mode", `{"permission_mode":"bogus"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", resp.StatusCode)
		}
	})
	t.Run("超 4KiB body 422", func(t *testing.T) {
		_, ts := permissionEndpointFixture(t)
		big := `{"permission_mode":"ask","pad":"` + strings.Repeat("a", 4096) + `"}`
		resp, err := http.DefaultClient.Do(authedReq("PATCH", ts.URL+"/api/v1/tasks/t1/permission-mode", big))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", resp.StatusCode)
		}
	})
	t.Run("404 not_found（ErrNoRows 语义）", func(t *testing.T) {
		tb, ts := permissionEndpointFixture(t)
		tb.SetPermissionModeView(application.PermissionModeView{},
			application.NewOpErr(application.CodeNotFound, errors.New("task missing")))
		resp, err := http.DefaultClient.Do(authedReq("PATCH", ts.URL+"/api/v1/tasks/missing/permission-mode", `{"permission_mode":"ask"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		var eb errorBody
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		if eb.Error.Code != CodeNotFound {
			t.Errorf("error code = %q, want not_found", eb.Error.Code)
		}
	})
	t.Run("500 非 ErrNoRows 读错误不误包 404", func(t *testing.T) {
		tb, ts := permissionEndpointFixture(t)
		tb.SetPermissionModeView(application.PermissionModeView{},
			application.NewOpErr(application.CodeInternal, errors.New("db busy")))
		resp, err := http.DefaultClient.Do(authedReq("PATCH", ts.URL+"/api/v1/tasks/t1/permission-mode", `{"permission_mode":"ask"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500（MUST NOT 误包 404）", resp.StatusCode)
		}
		var eb errorBody
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		if eb.Error.Code != CodeInternal {
			t.Errorf("error code = %q, want internal", eb.Error.Code)
		}
	})
	t.Run("409 互斥竞争", func(t *testing.T) {
		tb, ts := permissionEndpointFixture(t)
		tb.SetPermissionModeView(application.PermissionModeView{},
			application.NewOpErr(application.CodeConflict, errors.New("busy")))
		resp, err := http.DefaultClient.Do(authedReq("PATCH", ts.URL+"/api/v1/tasks/t1/permission-mode", `{"permission_mode":"ask"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})
}

// TestPermissionModeEndpoint_SuccessResponse 成功 200 固定两字段（输出归一化；
// 含 DR1 不一致视图：保存 ai-auto、有效 all-approve）；响应值经
// permissionModeForOutput 归一化（空值输出 ask）。
func TestPermissionModeEndpoint_SuccessResponse(t *testing.T) {
	t.Run("一致视图逐字透出", func(t *testing.T) {
		tb, ts := permissionEndpointFixture(t)
		tb.SetPermissionModeView(application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "ai-auto"}, nil)
		resp, err := http.DefaultClient.Do(authedReq("PATCH", ts.URL+"/api/v1/tasks/t1/permission-mode", `{"permission_mode":"ai-auto"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		raw := readAllBody(t, resp.Body)
		if !strings.Contains(raw, `"permission_mode":"ai-auto"`) || !strings.Contains(raw, `"effective_permission_mode":"ai-auto"`) {
			t.Fatalf("body = %s, want fixed two fields ai-auto/ai-auto", raw)
		}
		if taskID, mode := tb.permissionModeCallsSnapshot(); taskID != "t1" || mode != "ai-auto" {
			t.Errorf("backend call = (%q, %q), want (t1, ai-auto)", taskID, mode)
		}
	})
	t.Run("DR1 不一致视图（保存 ai-auto 有效 all-approve）", func(t *testing.T) {
		tb, ts := permissionEndpointFixture(t)
		tb.SetPermissionModeView(application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "all-approve"}, nil)
		resp, err := http.DefaultClient.Do(authedReq("PATCH", ts.URL+"/api/v1/tasks/t1/permission-mode", `{"permission_mode":"ai-auto"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw := readAllBody(t, resp.Body)
		if !strings.Contains(raw, `"permission_mode":"ai-auto"`) || !strings.Contains(raw, `"effective_permission_mode":"all-approve"`) {
			t.Fatalf("body = %s, want ai-auto/all-approve（DR1）", raw)
		}
	})
	t.Run("默认注入返回保存值镜像", func(t *testing.T) {
		_, ts := permissionEndpointFixture(t)
		resp, err := http.DefaultClient.Do(authedReq("PATCH", ts.URL+"/api/v1/tasks/t1/permission-mode", `{"permission_mode":"all-approve"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw := readAllBody(t, resp.Body)
		if !strings.Contains(raw, `"permission_mode":"all-approve"`) || !strings.Contains(raw, `"effective_permission_mode":"all-approve"`) {
			t.Fatalf("body = %s, want all-approve/all-approve", raw)
		}
	})
}

// TestPermissionMode_ExposeScope 透出范围（tasks 4.3/D6）：effective_permission_mode
// 仅详情 GET 与本端点响应；创建响应/项目任务列表/项目摘要/active 概览 MUST NOT 含。
func TestPermissionMode_ExposeScope(t *testing.T) {
	t.Run("详情 GET 含 effective 且与视图同源", func(t *testing.T) {
		tb := &permissionGetBackend{fakeTaskBackend: &fakeTaskBackend{},
			row: application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree", PermissionMode: "ai-auto"}}
		tb.SetPermissionModeView(application.PermissionModeView{PermissionMode: "ai-auto", EffectivePermissionMode: "all-approve"}, nil)
		_, ts := newPermissionServer(t, tb)
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw := readAllBody(t, resp.Body)
		if !strings.Contains(raw, `"permission_mode":"ai-auto"`) || !strings.Contains(raw, `"effective_permission_mode":"all-approve"`) {
			t.Fatalf("detail body = %s, want permission_mode + effective_permission_mode（DR1 不一致透出）", raw)
		}
	})
	t.Run("创建响应不含 effective", func(t *testing.T) {
		tb := &modeCaptureBackend{fakeTaskBackend: &fakeTaskBackend{},
			row: application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree", PermissionMode: "ai-auto"}}
		s := newAPITestServer(t, tb)
		projs := newFakeProjectStore()
		projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
		s.projs = projs
		s.RebuildRoutes()
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", `{"name":"x","permission_mode":"ai-auto"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		if raw := readAllBody(t, resp.Body); strings.Contains(raw, "effective_permission_mode") {
			t.Fatalf("create response must not expose effective_permission_mode: %s", raw)
		}
	})
	t.Run("项目任务列表不含 effective", func(t *testing.T) {
		s := newAPITestServer(t, &fakeTaskBackend{})
		projs := newFakeProjectStore()
		projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
		s.projs = projs
		s.RebuildRoutes()
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/p1/tasks", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if raw := readAllBody(t, resp.Body); strings.Contains(raw, "effective_permission_mode") {
			t.Fatalf("list response must not expose effective_permission_mode: %s", raw)
		}
	})
	t.Run("项目摘要不含 effective", func(t *testing.T) {
		sum := application.ProjectTaskSummary{TaskID: "t1", ProjectID: "p1", Name: "n", Status: "suspended",
			InitStatus: application.InitStatusNone, Branch: "b", Mode: "worktree", PermissionMode: "ai-auto",
			WorktreePath: "/wt", UpdatedAt: 100}
		tb := &projectSummaryBackend{fakeTaskBackend: &fakeTaskBackend{}, summaries: []application.ProjectTaskSummary{sum}}
		s := newAPITestServer(t, tb)
		projs := newFakeProjectStore()
		projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 50}
		projs.counts["p1"] = storeTaskCounts{Total: 1, ByStatus: map[string]int{}}
		s.projs = projs
		s.RebuildRoutes()
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/p1", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if raw := readAllBody(t, resp.Body); strings.Contains(raw, "effective_permission_mode") {
			t.Fatalf("summary response must not expose effective_permission_mode: %s", raw)
		}
	})
	t.Run("active 概览不含 effective", func(t *testing.T) {
		row := activeRow("t1", "p1", "projA", "task", "b", "/wt", 100)
		row.PermissionMode = "ai-auto"
		tb := newActiveSessionsBackend(row)
		s := newAPITestServer(t, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/active", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if raw := readAllBody(t, resp.Body); strings.Contains(raw, "effective_permission_mode") {
			t.Fatalf("active overview must not expose effective_permission_mode: %s", raw)
		}
	})
}
