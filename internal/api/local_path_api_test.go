package api

// add-local-path-task-mode tasks.md 5.6 DTO/API 传播行为测试（mode 必有且与持久化同源；
// 损坏 kind/mode 分派全覆盖）。P2 gate 已覆盖创建校验顺序（name 先于 mode/kind）与
// create 响应损坏行 500（lifecycle_config_api_test.go），本文件不重复。
//
// 覆盖：
//   - 创建请求 mode presence 语义透传（缺失/null=缺省、显式 trim、原样传 task 层）
//     与 API 层决策表拒绝（未知值/trim 空串/dir+显式 mode），零副作用。
//   - 任务详情 REST 与 SSE：mode 必有且与持久化同源；损坏 mode fail-closed
//     （REST 500；SSE 初始 500、update 保持 dirty 重试且 MUST NOT 推缺 mode 帧）。
//   - 活跃任务 REST/SSE snapshot/update：三种合法组合 mode 同源透传；损坏行
//     REST 500、SSE 初始 500（无 SSE 头、订阅释放）、update 保持 dirty 重试。
//   - 项目列表与详情任务摘要：mode 必有透传；损坏摘要 fail-closed 500。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
)

// readAllBody 一次性读取响应体为字符串（测试辅助）。
func readAllBody(t *testing.T, r io.Reader) string {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

// --- 创建请求 mode presence 与决策表 ---

// modeCaptureBackend 记录 Create 收到的 CreateTaskOptions，并返回预置任务行。
type modeCaptureBackend struct {
	*fakeTaskBackend
	mu   sync.Mutex
	opts []application.CreateTaskOptions
	row  application.TaskRow
}

func (b *modeCaptureBackend) Create(ctx context.Context, projectID string, opts application.CreateTaskOptions) (application.TaskRow, error) {
	b.mu.Lock()
	b.opts = append(b.opts, opts)
	b.mu.Unlock()
	return b.row, nil
}

func (b *modeCaptureBackend) createCalls() []application.CreateTaskOptions {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]application.CreateTaskOptions(nil), b.opts...)
}

// TestCreateTask_ModePresence_PassedToBackend 验证 mode presence 语义：缺失/null=缺省
// （Mode=""），显式值 trim 后透传 task 层；base_ref 与 mode 同时透传（组合校验归 task 层）；
// 创建响应 DTO mode 与持久化行同源（repo kind + local-path 行）。
func TestCreateTask_ModePresence_PassedToBackend(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	srv := func(row application.TaskRow) *Server {
		tb := &modeCaptureBackend{fakeTaskBackend: &fakeTaskBackend{}, row: row}
		s := newAPITestServer(t, tb)
		s.projs = projs
		s.RebuildRoutes()
		return s
	}

	t.Run("missing_mode_defaults_empty", func(t *testing.T) {
		s := srv(application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree"})
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
		calls := s.tasks.(*modeCaptureBackend).createCalls()
		if len(calls) != 1 || calls[0].Mode != "" {
			t.Errorf("Create opts = %+v, want Mode \"\" (缺失=缺省)", calls)
		}
	})
	t.Run("explicit_null_defaults_empty", func(t *testing.T) {
		s := srv(application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "worktree"})
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", `{"name":"x","mode":null}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		calls := s.tasks.(*modeCaptureBackend).createCalls()
		if len(calls) != 1 || calls[0].Mode != "" {
			t.Errorf("Create opts = %+v, want Mode \"\" (null=缺省)", calls)
		}
	})
	t.Run("explicit_value_trimmed_and_dto_same_source", func(t *testing.T) {
		s := srv(application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "local-path"})
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", `{"name":"x","mode":" local-path "}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		calls := s.tasks.(*modeCaptureBackend).createCalls()
		if len(calls) != 1 || calls[0].Mode != "local-path" {
			t.Errorf("Create opts = %+v, want Mode \"local-path\" (trim 后透传)", calls)
		}
		raw := readAllBody(t, resp.Body)
		if !strings.Contains(raw, `"mode":"local-path"`) {
			t.Errorf("201 body = %s, want mode local-path (与持久化同源)", raw)
		}
	})
	t.Run("base_ref_and_mode_passthrough", func(t *testing.T) {
		s := srv(application.TaskRow{ID: "t1", ProjectID: "p1", Status: application.StatusSuspended, Mode: "local-path"})
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/p1/tasks", `{"name":"x","mode":"local-path","base_ref":"feature"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		// API 层只透传（⑥ base_ref 与模式组合校验在 task 层 createInPlace 入口）。
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (API 层不拦截 base_ref+mode)", resp.StatusCode)
		}
		calls := s.tasks.(*modeCaptureBackend).createCalls()
		if len(calls) != 1 || calls[0].Mode != "local-path" || calls[0].BaseRef != "feature" {
			t.Errorf("Create opts = %+v, want Mode local-path + BaseRef feature (原样透传)", calls)
		}
	})
}

// TestCreateTask_ModeRejects API 层决策表拒绝（③值域 / ⑤dir+显式 mode）：422 invalid_input
// 且零副作用（MUST NOT 调用 Create 落 creating 行）；dir + mode 缺省仍可创建。
func TestCreateTask_ModeRejects(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["prep"] = storeProjectRow{ID: "prep", Name: "r", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	projs.projects["pdir"] = storeProjectRow{ID: "pdir", Name: "d", Path: "/d", Kind: "dir"}
	newSrv := func() (*Server, *modeCaptureBackend) {
		tb := &modeCaptureBackend{fakeTaskBackend: &fakeTaskBackend{}}
		s := newAPITestServer(t, tb)
		s.projs = projs
		s.RebuildRoutes()
		return s, tb
	}
	cases := []struct {
		desc, url, body string
	}{
		{"unknown mode value", "/api/v1/projects/prep/tasks", `{"name":"x","mode":"bogus"}`},
		{"blank mode after trim", "/api/v1/projects/prep/tasks", `{"name":"x","mode":"   "}`},
		{"dir project with explicit worktree", "/api/v1/projects/pdir/tasks", `{"name":"x","mode":"worktree"}`},
		{"dir project with explicit local-path", "/api/v1/projects/pdir/tasks", `{"name":"x","mode":"local-path"}`},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			s, tb := newSrv()
			ts := httptest.NewServer(s.mux)
			defer ts.Close()
			resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+tc.url, tc.body))
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
			if calls := tb.createCalls(); len(calls) != 0 {
				t.Errorf("Create called %d times, want 0 (零副作用，MUST NOT 落 creating 行)", len(calls))
			}
		})
	}
	t.Run("dir project with null mode creates", func(t *testing.T) {
		tb := &modeCaptureBackend{fakeTaskBackend: &fakeTaskBackend{},
			row: application.TaskRow{ID: "t1", ProjectID: "pdir", Status: application.StatusSuspended, Mode: "local-path"}}
		s := newAPITestServer(t, tb)
		s.projs = projs
		s.RebuildRoutes()
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/pdir/tasks", `{"name":"x","mode":null}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (dir + mode 缺省合法)", resp.StatusCode)
		}
	})
}

// --- 任务详情 REST / SSE：mode 同源 + 损坏分派 ---

// TestTaskDetail_Mode_SameSource_RESTAndSSE 任务详情 REST 与 SSE snapshot/update 帧
// mode 必有且与持久化同源（repo 项目 local-path 任务）。
func TestTaskDetail_Mode_SameSource_RESTAndSSE(t *testing.T) {
	row := fixtureTaskRow("t1")
	row.Mode = "local-path"
	tb := newTaskStreamBackend(row)
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// REST 详情。
	restResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer restResp.Body.Close()
	if restResp.StatusCode != http.StatusOK {
		t.Fatalf("REST status = %d, want 200", restResp.StatusCode)
	}
	restRaw := readAllBody(t, restResp.Body)
	if !strings.Contains(restRaw, `"mode":"local-path"`) {
		t.Errorf("REST body = %s, want mode local-path (必有字段，与持久化同源)", restRaw)
	}

	// SSE snapshot + update。
	streamResp := openTaskStream(t, ts.URL, "t1")
	defer streamResp.Body.Close()
	frames := startSSEFrameReader(streamResp.Body)
	snap := nextFrame(t, frames, "snapshot")
	if dto := decodeTaskDTO(t, snap.data); dto.Mode != "local-path" {
		t.Errorf("snapshot dto mode = %q, want local-path", dto.Mode)
	}
	updated := fixtureTaskRow("t1")
	updated.Mode = "local-path"
	updated.Name = "taskA-v2"
	tb.setRow(updated)
	sub.publish(ocdeckevent.NewTaskActivityChanged("t1"))
	upd := nextFrame(t, frames, "update")
	if dto := decodeTaskDTO(t, upd.data); dto.Mode != "local-path" {
		t.Errorf("update dto mode = %q, want local-path", dto.Mode)
	}
}

// TestTaskDetail_CorruptMode_FailClosed 损坏 mode（repo kind + 空 mode）：REST 详情 500；
// SSE 初始组装 500（不写 SSE 头）。
func TestTaskDetail_CorruptMode_FailClosed(t *testing.T) {
	row := fixtureTaskRow("t1")
	row.Mode = ""
	tb := newTaskStreamBackend(row)
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	restResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer restResp.Body.Close()
	if restResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("REST status = %d, want 500 (损坏 mode fail-closed)", restResp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(restResp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeInternal {
		t.Errorf("error code = %q, want internal", eb.Error.Code)
	}

	streamResp := openTaskStream(t, ts.URL, "t1")
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("SSE initial status = %d, want 500", streamResp.StatusCode)
	}
	if ct := streamResp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (不写 SSE 头)", ct)
	}
	waitFor(t, 2*time.Second, "subscriptions released", func() bool { return sub.liveSubs() == 0 })
}

// TestTaskStream_UpdateCorruptMode_KeepsDirtyRetries update 组装遇损坏 mode：
// 跳过该帧、保持 dirty、由后续事件重试；恢复后送达含 mode 的 update（MUST NOT 推缺 mode 帧）。
func TestTaskStream_UpdateCorruptMode_KeepsDirtyRetries(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	corrupt := fixtureTaskRow("t1")
	corrupt.Mode = "weird"
	tb.setRow(corrupt)
	sub.publish(ocdeckevent.NewTaskActivityChanged("t1"))
	// 损坏帧被跳过且不推送（≈6 个合并窗口内无帧）。
	assertNoFrame(t, frames, 250*time.Millisecond, "update with corrupt mode")

	tb.setRow(fixtureTaskRow("t1"))
	sub.publish(ocdeckevent.NewTaskActivityChanged("t1"))
	upd := nextFrame(t, frames, "update after mode restored")
	if dto := decodeTaskDTO(t, upd.data); dto.Mode != "worktree" {
		t.Errorf("update dto mode = %q, want worktree (恢复后送达合法帧)", dto.Mode)
	}
}

// --- 活跃任务 REST / SSE：mode 同源 + 损坏分派 ---

// localPathActiveRow 构造 repo+local-path / dir+local-path 的活跃概览行。
func localPathActiveRow(id, kind, mode string) application.ActiveTaskOverviewRow {
	row := activeRow(id, "p1", "projA", "task-"+id, "b", "/wt-"+id, 100)
	row.Kind = kind
	row.Mode = mode
	return row
}

// TestActiveSessions_ModeMixed_RESTAndSSE 三种合法组合（repo+worktree / repo+local-path /
// dir+local-path）在活跃 REST 与 SSE snapshot/update 帧中 mode 必有且与持久化同源。
func TestActiveSessions_ModeMixed_RESTAndSSE(t *testing.T) {
	rows := []application.ActiveTaskOverviewRow{
		localPathActiveRow("t1", "repo", "worktree"),
		localPathActiveRow("t2", "repo", "local-path"),
		localPathActiveRow("t3", "dir", "local-path"),
	}
	tb := newStreamBackend(rows...)
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	wantModes := map[string]string{"t1": "worktree", "t2": "local-path", "t3": "local-path"}
	assertModes := func(body string, what string) {
		got := decodeActiveSessions(t, body)
		if len(got) != 3 {
			t.Fatalf("%s: len = %d, want 3", what, len(got))
		}
		for _, elem := range got {
			if elem.Mode != wantModes[elem.TaskID] {
				t.Errorf("%s: task %s mode = %q, want %q (与持久化同源)", what, elem.TaskID, elem.Mode, wantModes[elem.TaskID])
			}
		}
	}

	restResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer restResp.Body.Close()
	if restResp.StatusCode != http.StatusOK {
		t.Fatalf("REST status = %d, want 200", restResp.StatusCode)
	}
	assertModes(readAllBody(t, restResp.Body), "REST")

	streamResp := openActiveSessionsStream(t, ts.URL)
	defer streamResp.Body.Close()
	frames := startSSEFrameReader(streamResp.Body)
	snap := nextFrame(t, frames, "snapshot")
	assertModes(snap.data, "SSE snapshot")
	sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t2"))
	upd := nextFrame(t, frames, "update")
	assertModes(upd.data, "SSE update")
}

// TestActiveSessions_CorruptMode_REST500 活跃 REST 遇持久化 kind/mode 损坏：
// fail-closed 500 标准错误信封，MUST NOT 输出缺/坏 mode 的元素。
func TestActiveSessions_CorruptMode_REST500(t *testing.T) {
	for _, tc := range []struct {
		desc string
		row  application.ActiveTaskOverviewRow
	}{
		{"repo + unknown mode", localPathActiveRow("t1", "repo", "weird")},
		{"dir + worktree", localPathActiveRow("t1", "dir", "worktree")},
		{"repo + empty mode", localPathActiveRow("t1", "repo", "")},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			tb := newActiveSessionsBackend(tc.row)
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
}

// TestActiveSessionsStream_CorruptMode_Initial500 SSE 初始组装遇损坏 kind/mode：
// 500 标准错误信封、不写 SSE 头、四路订阅释放。
func TestActiveSessionsStream_CorruptMode_Initial500(t *testing.T) {
	tb := newStreamBackend(localPathActiveRow("t1", "repo", "weird"))
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (不写 SSE 头)", ct)
	}
	waitFor(t, 2*time.Second, "subscriptions released", func() bool { return sub.liveSubs() == 0 })
}

// setRows 运行时替换 streamBackend 的 overview 行（SSE update 损坏分派测试用）。
func (b *streamBackend) setRows(rows []application.ActiveTaskOverviewRow) {
	b.mu.Lock()
	b.rows = rows
	b.mu.Unlock()
}

// TestActiveSessionsStream_UpdateCorruptMode_KeepsDirtyRetries SSE update 组装遇损坏
// kind/mode：跳过该帧保持 dirty（不推缺 mode 帧），数据恢复后经后续事件重试送达。
func TestActiveSessionsStream_UpdateCorruptMode_KeepsDirtyRetries(t *testing.T) {
	good := []application.ActiveTaskOverviewRow{localPathActiveRow("t1", "repo", "worktree")}
	tb := newStreamBackend(good...)
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	snap := nextFrame(t, frames, "snapshot")
	if got := decodeActiveSessions(t, snap.data); len(got) != 1 || got[0].Mode != "worktree" {
		t.Fatalf("snapshot = %s, want single worktree row", snap.data)
	}

	// 数据损坏 → update 组装失败：帧被跳过、连接保持。
	tb.setRows([]application.ActiveTaskOverviewRow{localPathActiveRow("t1", "repo", "weird")})
	sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t1"))
	assertNoFrame(t, frames, 250*time.Millisecond, "update with corrupt mode")

	// 数据恢复 → dirty 保持 + 后续事件重试送达合法帧。
	tb.setRows(good)
	sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t1"))
	upd := nextFrame(t, frames, "update after data restored")
	got := decodeActiveSessions(t, upd.data)
	if len(got) != 1 || got[0].Mode != "worktree" {
		t.Errorf("update = %s, want single worktree row (重试成功)", upd.data)
	}
}

// --- 项目列表/详情任务摘要：mode 同源 + 损坏分派 ---

// TestProjects_SummaryMode_ListAndDetail 项目列表与详情的 tasks 摘要 mode 必有且与
// 持久化同源（repo 项目 worktree/local-path 并存 + dir 项目 local-path）。
func TestProjects_SummaryMode_ListAndDetail(t *testing.T) {
	tb := &projectSummaryBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		summaries: []application.ProjectTaskSummary{
			{TaskID: "t1", Name: "wt-task", ProjectID: "p1", Status: application.StatusActive,
				InitStatus: application.InitStatusNone, Branch: "b1", Mode: "worktree", WorktreePath: "/wt1", UpdatedAt: 100},
			{TaskID: "t2", Name: "lp-task", ProjectID: "p1", Status: application.StatusSuspended,
				InitStatus: application.InitStatusNone, Branch: "", Mode: "local-path", WorktreePath: "/x", UpdatedAt: 200},
			{TaskID: "t3", Name: "dir-task", ProjectID: "p2", Status: application.StatusSuspended,
				InitStatus: application.InitStatusNone, Branch: "", Mode: "local-path", WorktreePath: "/d", UpdatedAt: 300},
		},
	}
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 50}
	projs.projects["p2"] = storeProjectRow{ID: "p2", Name: "projB", Path: "/d", Kind: "dir", CreatedAt: 60}
	projs.counts["p1"] = storeTaskCounts{Total: 2, ByStatus: map[string]int{application.StatusActive: 1, application.StatusSuspended: 1}}
	projs.counts["p2"] = storeTaskCounts{Total: 1, ByStatus: map[string]int{application.StatusSuspended: 1}}
	s := newAPITestServer(t, tb)
	s.projs = projs
	s.RebuildRoutes()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	wantModes := map[string]string{"t1": "worktree", "t2": "local-path", "t3": "local-path"}
	assertSummaryModes := func(body string, what string) {
		var projects []projectDTO
		if err := json.Unmarshal([]byte(body), &projects); err != nil {
			t.Fatalf("%s: decode: %v (body=%s)", what, err, body)
		}
		for _, p := range projects {
			for _, sm := range p.TaskSummaries {
				if sm.Mode != wantModes[sm.ID] {
					t.Errorf("%s: summary %s mode = %q, want %q (与持久化同源)", what, sm.ID, sm.Mode, wantModes[sm.ID])
				}
			}
		}
	}

	listResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	listRaw := readAllBody(t, listResp.Body)
	assertSummaryModes(listRaw, "list")
	if !strings.Contains(listRaw, `"mode":"local-path"`) || !strings.Contains(listRaw, `"mode":"worktree"`) {
		t.Errorf("list body missing mode values: %s", listRaw)
	}

	detailResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/p1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer detailResp.Body.Close()
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", detailResp.StatusCode)
	}
	// 详情响应为单项目对象（projectDetailDTO）。先断言摘要真实注入（恰为 p1 的
	// t1/t2 两条 + tasks 概况字段存在），防止组装被短路后循环零次执行的真空断言。
	detailRaw := readAllBody(t, detailResp.Body)
	var detail projectDTO
	if err := json.Unmarshal([]byte(detailRaw), &detail); err != nil {
		t.Fatalf("detail decode: %v (body=%s)", err, detailRaw)
	}
	if len(detail.TaskSummaries) != 2 {
		t.Fatalf("detail summaries len = %d, want 2 (t1+t2)：摘要注入被短路时此断言失败", len(detail.TaskSummaries))
	}
	gotIDs := map[string]bool{}
	for _, sm := range detail.TaskSummaries {
		gotIDs[sm.ID] = true
		if sm.Mode != wantModes[sm.ID] {
			t.Errorf("detail: summary %s mode = %q, want %q (与持久化同源)", sm.ID, sm.Mode, wantModes[sm.ID])
		}
	}
	if !gotIDs["t1"] || !gotIDs["t2"] || len(gotIDs) != 2 {
		t.Errorf("detail summary IDs = %v, want exactly {t1,t2}", gotIDs)
	}
	if detail.Tasks == nil {
		t.Error("detail tasks_by_status missing (tasks field must be present)")
	}
}

// TestProjects_CorruptSummaryMode_500 摘要组装遇非法 kind/mode（损坏持久化数据）：
// 列表与详情均 fail-closed 500 标准错误信封，MUST NOT 输出缺 mode 元素。
func TestProjects_CorruptSummaryMode_500(t *testing.T) {
	tb := &projectSummaryBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		summaries: []application.ProjectTaskSummary{
			{TaskID: "t1", Name: "taskA", ProjectID: "p1", Status: application.StatusActive,
				InitStatus: application.InitStatusNone, Branch: "b1", Mode: "weird", WorktreePath: "/wt1", UpdatedAt: 100},
		},
	}
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 50}
	projs.counts["p1"] = storeTaskCounts{Total: 1, ByStatus: map[string]int{application.StatusActive: 1}}
	s := newAPITestServer(t, tb)
	s.projs = projs
	s.RebuildRoutes()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	for _, tc := range []struct{ desc, url string }{
		{"list", "/api/v1/projects"},
		{"detail", "/api/v1/projects/p1"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+tc.url, ""))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("%s status = %d, want 500 (fail-closed)", tc.desc, resp.StatusCode)
			}
			// 先完整读一次 body，再反序列化并做泄漏检查（不得 Decode 后再读——body 已耗尽）。
			raw := readAllBody(t, resp.Body)
			var eb errorBody
			if err := json.Unmarshal([]byte(raw), &eb); err != nil {
				t.Fatalf("%s decode: %v (body=%s)", tc.desc, err, raw)
			}
			if eb.Error.Code != CodeInternal {
				t.Errorf("error code = %q, want internal", eb.Error.Code)
			}
			if strings.Contains(raw, `"tasks":[{`) && strings.Contains(raw, "taskA") {
				t.Errorf("%s body leaks corrupt summary: %s", tc.desc, raw)
			}
		})
	}
}
