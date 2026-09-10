package api

// task-permission-mode D9（api 层）：
//   - 创建请求 permission_mode presence 三态（缺失/null=缺省、显式 trim 透传、
//     非法值/trim 空串 → invalid_input 零副作用）与校验顺序 name → mode → permission_mode；
//   - 输出矩阵透出（创建响应/任务详情、active 任务概览、项目任务摘要）：
//     与持久化同源、存量空值输出 ask、未知持久化值 fail-closed internal。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ocdeck/internal/application"
)

// --- 创建请求 permission_mode presence 与校验顺序 ---

// TestCreateTask_PermissionModePresence_PassedToBackend 验证 presence 语义：缺失/null=
// 缺省（PermissionMode=""，由 task 层归一化 ask），显式值 trim 后透传 task 层；
// 创建响应 DTO permission_mode 与持久化行同源。
func TestCreateTask_PermissionModePresence_PassedToBackend(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	srv := func(row application.TaskRow) (*Server, *modeCaptureBackend) {
		tb := &modeCaptureBackend{fakeTaskBackend: &fakeTaskBackend{}, row: row}
		s := newAPITestServer(t, tb)
		s.projs = projs
		s.RebuildRoutes()
		return s, tb
	}

	t.Run("missing_defaults_empty", func(t *testing.T) {
		s, tb := srv(application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree", PermissionMode: "ask"})
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", `{"name":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		calls := tb.createCalls()
		if len(calls) != 1 || calls[0].PermissionMode != "" {
			t.Errorf("Create opts = %+v, want PermissionMode \"\" (缺失=缺省，归一化归 task 层)", calls)
		}
	})
	t.Run("explicit_null_defaults_empty", func(t *testing.T) {
		s, tb := srv(application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree", PermissionMode: "ask"})
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", `{"name":"x","permission_mode":null}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		calls := tb.createCalls()
		if len(calls) != 1 || calls[0].PermissionMode != "" {
			t.Errorf("Create opts = %+v, want PermissionMode \"\" (null=缺省)", calls)
		}
	})
	t.Run("explicit_value_trimmed_and_dto_same_source", func(t *testing.T) {
		s, tb := srv(application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree", PermissionMode: "all-approve"})
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", `{"name":"x","permission_mode":" all-approve "}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		calls := tb.createCalls()
		if len(calls) != 1 || calls[0].PermissionMode != "all-approve" {
			t.Errorf("Create opts = %+v, want PermissionMode \"all-approve\" (trim 后透传)", calls)
		}
		raw := readAllBody(t, resp.Body)
		if !strings.Contains(raw, `"permission_mode":"all-approve"`) {
			t.Errorf("201 body = %s, want permission_mode all-approve (与持久化同源)", raw)
		}
	})
}

// TestCreateTask_PermissionMode_CreateResponseMatrix 创建响应输出矩阵（tasks 1.5/1.6、
// design D7/D9）：后端返回行三值逐字输出、空值输出 ask、坏值 fail-closed 500 且不输出
// 任务 DTO。请求缺省（不传 permission_mode）而返回行显式非 ask，证明响应与持久化行同源、
// 非回显请求。
func TestCreateTask_PermissionMode_CreateResponseMatrix(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	cases := []struct {
		desc        string
		rowPM       string // 后端 Create 返回行的持久化值
		wantPM      string // 期望响应输出（坏值用例为空串=不输出 DTO）
		wantStatus  int
		wantFailure bool
	}{
		{desc: "row ask outputs ask", rowPM: "ask", wantPM: "ask", wantStatus: http.StatusCreated},
		{desc: "row all-approve verbatim", rowPM: "all-approve", wantPM: "all-approve", wantStatus: http.StatusCreated},
		{desc: "row ai-auto verbatim", rowPM: "ai-auto", wantPM: "ai-auto", wantStatus: http.StatusCreated},
		{desc: "row empty outputs ask", rowPM: "", wantPM: "ask", wantStatus: http.StatusCreated},
		{desc: "row corrupt fail closed", rowPM: "bogus", wantPM: "", wantStatus: http.StatusInternalServerError, wantFailure: true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			tb := &modeCaptureBackend{fakeTaskBackend: &fakeTaskBackend{},
				row: application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree", PermissionMode: tc.rowPM}}
			s := newAPITestServer(t, tb)
			s.projs = projs
			s.RebuildRoutes()
			ts := httptest.NewServer(s.mux)
			defer ts.Close()
			// 请求缺省（不传 permission_mode）→ 后端收到的选项为空串（缺省语义归 task 层）。
			resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", `{"name":"x"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			raw := readAllBody(t, resp.Body)
			if tc.wantFailure {
				// fail-closed：500 标准 internal 错误信封，MUST NOT 输出含坏值的任务 DTO。
				var eb errorBody
				if err := json.Unmarshal([]byte(raw), &eb); err != nil {
					t.Fatalf("decode: %v (body=%s)", err, raw)
				}
				if eb.Error.Code != CodeInternal {
					t.Errorf("error code = %q, want internal", eb.Error.Code)
				}
				// 错误信封的诊断消息含 "permission_mode" 字样，故按 JSON 字段形态断言不泄露坏值元素。
				if strings.Contains(raw, `"permission_mode":"bogus"`) {
					t.Errorf("body leaks corrupt task DTO: %s", raw)
				}
				return
			}
			if !strings.Contains(raw, `"permission_mode":"`+tc.wantPM+`"`) {
				t.Errorf("201 body = %s, want permission_mode %q (与持久化行同源)", raw, tc.wantPM)
			}
			// 同源而非回显：请求缺省时透传空串，响应值只能来自后端返回行。
			calls := tb.createCalls()
			if len(calls) != 1 || calls[0].PermissionMode != "" {
				t.Errorf("Create opts = %+v, want PermissionMode \"\" (请求缺省)", calls)
			}
		})
	}
}

// TestCreateTask_PermissionModeRejects 非法值/trim 空串 → 422 invalid_input 且零副作用
//（MUST NOT 调用 Create 落 creating 行）；三合法值放行。
func TestCreateTask_PermissionModeRejects(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	cases := []struct {
		desc, body string
	}{
		{"unknown value", `{"name":"x","permission_mode":"bogus"}`},
		{"always is not a permission mode", `{"name":"x","permission_mode":"always"}`},
		{"explicit empty string", `{"name":"x","permission_mode":""}`},
		{"blank after trim", `{"name":"x","permission_mode":"   "}`},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			tb := &modeCaptureBackend{fakeTaskBackend: &fakeTaskBackend{}}
			s := newAPITestServer(t, tb)
			s.projs = projs
			s.RebuildRoutes()
			ts := httptest.NewServer(s.mux)
			defer ts.Close()
			resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", tc.body))
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
				t.Errorf("error code = %q, want invalid_input", eb.Error.Code)
			}
			if !strings.Contains(eb.Error.Message, "permission_mode") {
				t.Errorf("error message = %q, want mention permission_mode", eb.Error.Message)
			}
			if calls := tb.createCalls(); len(calls) != 0 {
				t.Errorf("Create called %d times, want 0 (零副作用)", len(calls))
			}
		})
	}
	t.Run("legal values accepted", func(t *testing.T) {
		for _, mode := range []string{"ask", "all-approve", "ai-auto"} {
			tb := &modeCaptureBackend{fakeTaskBackend: &fakeTaskBackend{},
				row: application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree", PermissionMode: mode}}
			s := newAPITestServer(t, tb)
			s.projs = projs
			s.RebuildRoutes()
			ts := httptest.NewServer(s.mux)
			defer ts.Close()
			body := `{"name":"x","permission_mode":"` + mode + `"}`
			resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("permission_mode=%q status = %d, want 201", mode, resp.StatusCode)
			}
		}
	})
}

// TestCreateTask_PermissionModeValidationOrder 校验顺序 name → mode → permission_mode
// （task-permission-mode D2）：空白名称报 name 错误；非法 mode 先于非法 permission_mode 报错。
func TestCreateTask_PermissionModeValidationOrder(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	cases := []struct {
		desc, body, wantMsg string
	}{
		{"blank name precedes permission_mode", `{"name":" ","permission_mode":"bogus"}`, "task name is required"},
		{"invalid mode precedes invalid permission_mode", `{"name":"x","mode":"bogus","permission_mode":"bogus"}`, "mode must be worktree or local-path"},
		{"valid name+mode reaches permission_mode", `{"name":"x","mode":"worktree","permission_mode":"bogus"}`, "permission_mode must be ask, all-approve or ai-auto"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			tb := &modeCaptureBackend{fakeTaskBackend: &fakeTaskBackend{}}
			s := newAPITestServer(t, tb)
			s.projs = projs
			s.RebuildRoutes()
			ts := httptest.NewServer(s.mux)
			defer ts.Close()
			resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", tc.body))
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
			if eb.Error.Code != CodeInvalidInput || eb.Error.Message != tc.wantMsg {
				t.Errorf("error = %v/%q, want invalid_input/%q (校验顺序)", eb.Error.Code, eb.Error.Message, tc.wantMsg)
			}
			if calls := tb.createCalls(); len(calls) != 0 {
				t.Errorf("Create called %d times, want 0 (校验失败零副作用)", len(calls))
			}
		})
	}
}

// --- 输出矩阵：任务详情 / active 概览 / 项目摘要 ---

// permissionGetBackend Get 返回预置任务行（详情读模型测试用）。
type permissionGetBackend struct {
	*fakeTaskBackend
	row application.TaskRow
}

func (b *permissionGetBackend) Get(ctx context.Context, taskID string) (application.TaskRow, error) {
	return b.row, nil
}

// newPermissionServer 构造带 repo 项目的 API 测试服务器。
func newPermissionServer(t *testing.T, tb TaskBackend) (*Server, *httptest.Server) {
	t.Helper()
	s := newAPITestServer(t, tb)
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	s.projs = projs
	s.RebuildRoutes()
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	return s, ts
}

// TestTaskDetail_PermissionMode_OutputMatrix 任务详情 permission_mode 必有且与持久化同源；
// 存量空值输出 ask；未知持久化值 fail-closed 500 internal（task-permission-mode D7）。
func TestTaskDetail_PermissionMode_OutputMatrix(t *testing.T) {
	t.Run("same source three values", func(t *testing.T) {
		for _, mode := range []string{"ask", "all-approve", "ai-auto"} {
			tb := &permissionGetBackend{fakeTaskBackend: &fakeTaskBackend{},
				row: application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree", PermissionMode: mode}}
			_, ts := newPermissionServer(t, tb)
			resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("mode=%q status = %d, want 200", mode, resp.StatusCode)
			}
			raw := readAllBody(t, resp.Body)
			if !strings.Contains(raw, `"permission_mode":"`+mode+`"`) {
				t.Errorf("detail body = %s, want permission_mode %q (与持久化同源)", raw, mode)
			}
		}
	})
	t.Run("empty outputs ask", func(t *testing.T) {
		tb := &permissionGetBackend{fakeTaskBackend: &fakeTaskBackend{},
			row: application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree"}}
		_, ts := newPermissionServer(t, tb)
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		raw := readAllBody(t, resp.Body)
		if !strings.Contains(raw, `"permission_mode":"ask"`) {
			t.Errorf("detail body = %s, want permission_mode ask (存量空值防御)", raw)
		}
	})
	t.Run("unknown value fail closed", func(t *testing.T) {
		tb := &permissionGetBackend{fakeTaskBackend: &fakeTaskBackend{},
			row: application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree", PermissionMode: "bogus"}}
		_, ts := newPermissionServer(t, tb)
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (fail-closed)", resp.StatusCode)
		}
		var eb errorBody
		if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
			t.Fatal(err)
		}
		if eb.Error.Code != CodeInternal {
			t.Errorf("error code = %q, want internal", eb.Error.Code)
		}
	})
}

// TestActiveSessions_PermissionMode_OutputMatrix 活跃任务概览 permission_mode 必有且与
// 持久化同源；未知持久化值 fail-closed 500（REST；task-permission-mode D7）。
func TestActiveSessions_PermissionMode_OutputMatrix(t *testing.T) {
	row := func(id, pm string) application.ActiveTaskOverviewRow {
		r := activeRow(id, "p1", "projA", "task-"+id, "b", "/wt-"+id, 100)
		r.PermissionMode = pm
		return r
	}
	t.Run("same source mixed values", func(t *testing.T) {
		tb := newActiveSessionsBackend(row("t1", "ask"), row("t2", "all-approve"), row("t3", "ai-auto"))
		s := newAPITestServer(t, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/active", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		want := map[string]string{"t1": "ask", "t2": "all-approve", "t3": "ai-auto"}
		for _, elem := range decodeActiveSessions(t, readAllBody(t, resp.Body)) {
			if elem.PermissionMode != want[elem.TaskID] {
				t.Errorf("task %s permission_mode = %q, want %q (与持久化同源)", elem.TaskID, elem.PermissionMode, want[elem.TaskID])
			}
		}
	})
	t.Run("empty outputs ask", func(t *testing.T) {
		tb := newActiveSessionsBackend(row("t1", ""))
		s := newAPITestServer(t, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/active", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got := decodeActiveSessions(t, readAllBody(t, resp.Body))
		if len(got) != 1 || got[0].PermissionMode != "ask" {
			t.Errorf("active sessions = %v, want permission_mode ask (存量空值防御)", got)
		}
	})
	t.Run("unknown value fail closed", func(t *testing.T) {
		tb := newActiveSessionsBackend(row("t1", "bogus"))
		s := newAPITestServer(t, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/active", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (fail-closed)", resp.StatusCode)
		}
		var eb errorBody
		if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
			t.Fatal(err)
		}
		if eb.Error.Code != CodeInternal {
			t.Errorf("error code = %q, want internal", eb.Error.Code)
		}
	})
}

// TestProjects_SummaryPermissionMode_OutputMatrix 项目任务摘要 permission_mode 必有且与
// 持久化同源；未知持久化值 fail-closed 500（task-permission-mode D7）。
func TestProjects_SummaryPermissionMode_OutputMatrix(t *testing.T) {
	newSrv := func(summaries []application.ProjectTaskSummary) (*httptest.Server, string) {
		tb := &projectSummaryBackend{fakeTaskBackend: &fakeTaskBackend{}, summaries: summaries}
		s := newAPITestServer(t, tb)
		projs := newFakeProjectStore()
		projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 50}
		projs.counts["p1"] = storeTaskCounts{Total: len(summaries), ByStatus: map[string]int{}}
		s.projs = projs
		s.RebuildRoutes()
		ts := httptest.NewServer(s.mux)
		t.Cleanup(ts.Close)
		return ts, "/api/v1/projects/p1"
	}
	summary := func(id, pm string) application.ProjectTaskSummary {
		return application.ProjectTaskSummary{TaskID: id, Name: "task-" + id, ProjectID: "p1",
			Status: application.StatusSuspended, InitStatus: application.InitStatusNone,
			Branch: "b", Mode: "worktree", PermissionMode: pm, WorktreePath: "/wt", UpdatedAt: 100}
	}
	t.Run("same source mixed values", func(t *testing.T) {
		ts, url := newSrv([]application.ProjectTaskSummary{
			summary("t1", "ask"), summary("t2", "all-approve"), summary("t3", "ai-auto"),
		})
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+url, ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		raw := readAllBody(t, resp.Body)
		for _, mode := range []string{"ask", "all-approve", "ai-auto"} {
			if !strings.Contains(raw, `"permission_mode":"`+mode+`"`) {
				t.Errorf("summary body missing permission_mode %q: %s", mode, raw)
			}
		}
	})
	t.Run("empty outputs ask", func(t *testing.T) {
		ts, url := newSrv([]application.ProjectTaskSummary{summary("t1", "")})
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+url, ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw := readAllBody(t, resp.Body)
		if !strings.Contains(raw, `"permission_mode":"ask"`) {
			t.Errorf("summary body = %s, want permission_mode ask (存量空值防御)", raw)
		}
	})
	t.Run("unknown value fail closed", func(t *testing.T) {
		ts, url := newSrv([]application.ProjectTaskSummary{summary("t1", "bogus")})
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+url, ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (fail-closed)", resp.StatusCode)
		}
		var eb errorBody
		if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
			t.Fatal(err)
		}
		if eb.Error.Code != CodeInternal {
			t.Errorf("error code = %q, want internal", eb.Error.Code)
		}
	})
}
