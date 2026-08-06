package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ocdeck/internal/task"
)

// lifecycleTaskBackend 为 lifecycle-config / rerun-init / logs API 测试提供可控 TaskBackend。
// 嵌入 fakeTaskBackend 复用其余 noop 方法，仅覆盖本任务相关方法。
type lifecycleTaskBackend struct {
	*fakeTaskBackend
	tasks        map[string]task.TaskRow
	rerunFn      func(ctx context.Context, taskID string) (task.TaskRow, error)
	readInitFn   func(ctx context.Context, taskID string) (string, error)
	readPreDelFn func(ctx context.Context, taskID string) (string, error)
}

func newLifecycleTaskBackend(rows ...task.TaskRow) *lifecycleTaskBackend {
	b := &lifecycleTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		tasks:           map[string]task.TaskRow{},
	}
	for _, r := range rows {
		b.tasks[r.ID] = r
	}
	return b
}

func (b *lifecycleTaskBackend) Get(ctx context.Context, taskID string) (task.TaskRow, error) {
	r, ok := b.tasks[taskID]
	if !ok {
		return task.TaskRow{}, &task.OpError{Code: "not_found", Err: errNotFound(taskID)}
	}
	return r, nil
}

func (b *lifecycleTaskBackend) RerunInit(ctx context.Context, taskID string) (task.TaskRow, error) {
	if b.rerunFn != nil {
		return b.rerunFn(ctx, taskID)
	}
	return task.TaskRow{}, nil
}

func (b *lifecycleTaskBackend) ReadInitLog(ctx context.Context, taskID string) (string, error) {
	if b.readInitFn != nil {
		return b.readInitFn(ctx, taskID)
	}
	return "", nil
}

func (b *lifecycleTaskBackend) ReadPreDeleteLog(ctx context.Context, taskID string) (string, error) {
	if b.readPreDelFn != nil {
		return b.readPreDelFn(ctx, taskID)
	}
	return "", nil
}

// fakeLifecycleConfigStore 内存实现 LifecycleConfigStore，用于 lifecycle-config API 测试。
type fakeLifecycleConfigStore struct {
	cfgs map[string]lifecycleConfigRow
}

func newFakeLifecycleConfigStore() *fakeLifecycleConfigStore {
	return &fakeLifecycleConfigStore{cfgs: map[string]lifecycleConfigRow{}}
}

func (s *fakeLifecycleConfigStore) GetLifecycleConfig(ctx context.Context, projectID string) (lifecycleConfigRow, error) {
	c, ok := s.cfgs[projectID]
	if !ok {
		// 缺行返回空配置（非错误）。
		return lifecycleConfigRow{ProjectID: projectID}, nil
	}
	return c, nil
}

func (s *fakeLifecycleConfigStore) UpsertLifecycleConfig(ctx context.Context, projectID, inheritPatterns, initScript, preDeleteScript string) error {
	s.cfgs[projectID] = lifecycleConfigRow{
		ProjectID:       projectID,
		InheritPatterns: inheritPatterns,
		InitScript:      initScript,
		PreDeleteScript: preDeleteScript,
		UpdatedAt:       1,
	}
	return nil
}

// newLifecycleAPIServer 构造带 ProjectStore + LifecycleConfigStore + TaskBackend 的 Server。
func newLifecycleAPIServer(t *testing.T, projs ProjectStore, lc LifecycleConfigStore, tb TaskBackend) *Server {
	t.Helper()
	cfg := testConfig()
	s := &Server{
		cfg:           cfg,
		mux:           http.NewServeMux(),
		auth:          NewTokenAuthenticator(cfg.Token),
		wsClients:     newWSClientRegistry(),
		projs:         projs,
		lifecycleCfgs: lc,
		tasks:         tb,
	}
	s.registerRoutes()
	return s
}

// --- 4.1 lifecycle-config handler 测试 ---

func TestLifecycleAPI_GetEmptyConfig(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main"}
	lc := newFakeLifecycleConfigStore()
	tb := &fakeTaskBackend{}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/p1/lifecycle-config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got lifecycleConfigDTO
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.InheritPatterns != "" || got.InitScript != "" || got.PreDeleteScript != "" {
		t.Errorf("empty config = %+v, want all empty", got)
	}
}

func TestLifecycleAPI_PutInvalidGlob_LineNumber(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1"}
	lc := newFakeLifecycleConfigStore()
	tb := &fakeTaskBackend{}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 第 2 行非法 glob（未闭合 [）。
	body := `{"inherit_patterns":"*.txt\n[\n# comment\n","init_script":"","pre_delete_script":""}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/lifecycle-config", body))
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
		t.Errorf("code = %v, want invalid_input", eb.Error.Code)
	}
	if !strings.Contains(eb.Error.Message, "line 2") {
		t.Errorf("message = %q, want contains line 2", eb.Error.Message)
	}
	// 未落库。
	if _, ok := lc.cfgs["p1"]; ok {
		t.Error("invalid glob should not be stored")
	}
}

func TestLifecycleAPI_PutInheritPatternsOver16KB(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1"}
	lc := newFakeLifecycleConfigStore()
	tb := &fakeTaskBackend{}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 构造 >16KB 的 inherit_patterns（合法 glob 但超限）。
	big := strings.Repeat("*.txt\n", 4000) // ~28KB
	body := `{"inherit_patterns":` + jsonString(big) + `,"init_script":"","pre_delete_script":""}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/lifecycle-config", body))
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
		t.Errorf("code = %v, want invalid_input", eb.Error.Code)
	}
}

func TestLifecycleAPI_ProjectNotFound_GET(t *testing.T) {
	projs := newFakeProjectStore()
	lc := newFakeLifecycleConfigStore()
	tb := &fakeTaskBackend{}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/nope/lifecycle-config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeNotFound {
		t.Errorf("code = %v, want not_found", eb.Error.Code)
	}
}

func TestLifecycleAPI_ProjectNotFound_PUT(t *testing.T) {
	projs := newFakeProjectStore()
	lc := newFakeLifecycleConfigStore()
	tb := &fakeTaskBackend{}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/nope/lifecycle-config", `{"inherit_patterns":"","init_script":"","pre_delete_script":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeNotFound {
		t.Errorf("code = %v, want not_found", eb.Error.Code)
	}
}

// --- 4.2 rerun-init / logs handler 测试 ---

func TestLifecycleAPI_RerunInit_InvalidState_422(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	lc := newFakeLifecycleConfigStore()
	// Get 必须返回带 ProjectID 的行，供 RerunInit 副作用前预取 kind（D6 fail-closed）。
	tb := newLifecycleTaskBackend(task.TaskRow{ID: "t1", ProjectID: "p1", Status: task.StatusSuspended})
	tb.rerunFn = func(ctx context.Context, taskID string) (task.TaskRow, error) {
		return task.TaskRow{}, &task.OpError{Code: "invalid_state", Err: errNotFound(taskID)}
	}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/t1/rerun-init", ""))
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
	if eb.Error.Code != CodeInvalidState {
		t.Errorf("code = %v, want invalid_state", eb.Error.Code)
	}
}

func TestLifecycleAPI_RerunInit_Conflict_409(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	lc := newFakeLifecycleConfigStore()
	tb := newLifecycleTaskBackend(task.TaskRow{ID: "t1", ProjectID: "p1", Status: task.StatusSuspended})
	tb.rerunFn = func(ctx context.Context, taskID string) (task.TaskRow, error) {
		return task.TaskRow{}, &task.OpError{Code: "conflict", Err: errNotFound(taskID)}
	}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/t1/rerun-init", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeConflict {
		t.Errorf("code = %v, want conflict", eb.Error.Code)
	}
}

func TestLifecycleAPI_RerunInit_Success_200WithTaskDTO(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "repo"}
	lc := newFakeLifecycleConfigStore()
	tb := newLifecycleTaskBackend(task.TaskRow{ID: "t1", ProjectID: "p1", Status: task.StatusSuspended})
	tb.rerunFn = func(ctx context.Context, taskID string) (task.TaskRow, error) {
		return task.TaskRow{ID: "t1", ProjectID: "p1", Status: task.StatusSuspended, InitStatus: task.InitStatusRunning}, nil
	}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/t1/rerun-init", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto taskRowDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.ID != "t1" {
		t.Errorf("dto.ID = %q, want t1", dto.ID)
	}
	if dto.InitStatus != task.InitStatusRunning {
		t.Errorf("dto.InitStatus = %q, want running", dto.InitStatus)
	}
}

func TestLifecycleAPI_InitLog_EmptyBody_200(t *testing.T) {
	projs := newFakeProjectStore()
	lc := newFakeLifecycleConfigStore()
	tb := newLifecycleTaskBackend(task.TaskRow{ID: "t1", ProjectID: "p1", Status: task.StatusSuspended})
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1/init-log", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Errorf("init-log body = %q, want empty", body)
	}
}

func TestLifecycleAPI_PreDeleteLog_EmptyBody_200(t *testing.T) {
	projs := newFakeProjectStore()
	lc := newFakeLifecycleConfigStore()
	tb := newLifecycleTaskBackend(task.TaskRow{ID: "t1", ProjectID: "p1", Status: task.StatusSuspended})
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1/pre-delete-log", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Errorf("pre-delete-log body = %q, want empty", body)
	}
}

func TestLifecycleAPI_InitLog_TaskNotFound_404(t *testing.T) {
	projs := newFakeProjectStore()
	lc := newFakeLifecycleConfigStore()
	tb := newLifecycleTaskBackend() // 无任务
	tb.readInitFn = func(ctx context.Context, taskID string) (string, error) {
		return "", &task.OpError{Code: "not_found", Err: errNotFound(taskID)}
	}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/nope/init-log", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeNotFound {
		t.Errorf("code = %v, want not_found", eb.Error.Code)
	}
}

func TestLifecycleAPI_PreDeleteLog_TaskNotFound_404(t *testing.T) {
	projs := newFakeProjectStore()
	lc := newFakeLifecycleConfigStore()
	tb := newLifecycleTaskBackend()
	tb.readPreDelFn = func(ctx context.Context, taskID string) (string, error) {
		return "", &task.OpError{Code: "not_found", Err: errNotFound(taskID)}
	}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/nope/pre-delete-log", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeNotFound {
		t.Errorf("code = %v, want not_found", eb.Error.Code)
	}
}

// jsonString 将字符串编码为 JSON 字符串字面量（含引号）。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestLifecycleAPI_PutBodyCap_AllowsEscaped64KBScripts：两个解码后恰好 64KB、
// 内容需大量 JSON 转义（全反斜杠）的合法脚本，PUT 必须成功——证明 1MiB raw body
// cap 不误拒契约内合法配置（字段上限按解码后长度计，raw body 需容纳 JSON 转义膨胀）。
func TestLifecycleAPI_PutBodyCap_AllowsEscaped64KBScripts(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1"}
	lc := newFakeLifecycleConfigStore()
	tb := &fakeTaskBackend{}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 全反斜杠内容：json.Marshal 后每个 `\` 编成 `\\`（2 字节），raw body 远大于解码后长度。
	// 解码后恰好 64KB（lifecycleScriptMax），合法上限内。
	escaped64KB := strings.Repeat("\\", lifecycleScriptMax)
	body := `{"inherit_patterns":"","init_script":` + jsonString(escaped64KB) + `,"pre_delete_script":` + jsonString(escaped64KB) + `}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/lifecycle-config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (1MiB cap must allow two 64KB scripts needing JSON escaping), raw body size = %d", resp.StatusCode, len(body))
	}
}

// TestLifecycleAPI_PutFieldLimit_64KBPlus1_Still422：任一字段解码后 64KB+1
// → 仍 invalid_input 422（字段上限不受 raw body cap 提高影响）。
func TestLifecycleAPI_PutFieldLimit_64KBPlus1_Still422(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1"}
	lc := newFakeLifecycleConfigStore()
	tb := &fakeTaskBackend{}
	s := newLifecycleAPIServer(t, projs, lc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	overLimit := strings.Repeat("x", lifecycleScriptMax+1) // 解码后 64KB+1，无需转义
	body := `{"inherit_patterns":"","init_script":` + jsonString(overLimit) + `,"pre_delete_script":""}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/lifecycle-config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (field limit independent of body cap)", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeInvalidInput {
		t.Errorf("code = %v, want invalid_input", eb.Error.Code)
	}
}

// --- add-plain-dir-project D6：task DTO project_kind 填充 ---

// projectKindTaskBackend 覆盖 Create/List/Get/RerunInit，返回固定 ProjectID 的任务，
// 供 project_kind 填充测试。
type projectKindTaskBackend struct {
	*fakeTaskBackend
	projectID string
}

func (b *projectKindTaskBackend) Create(ctx context.Context, projectID, taskName, baseRef string) (task.TaskRow, error) {
	return task.TaskRow{ID: "t-new", ProjectID: projectID, Name: taskName, Status: task.StatusSuspended}, nil
}
func (b *projectKindTaskBackend) Get(ctx context.Context, taskID string) (task.TaskRow, error) {
	return task.TaskRow{ID: taskID, ProjectID: b.projectID, Status: task.StatusSuspended}, nil
}
func (b *projectKindTaskBackend) List(ctx context.Context, projectID string) ([]task.TaskRow, error) {
	return []task.TaskRow{{ID: "t1", ProjectID: projectID, Status: task.StatusSuspended}}, nil
}
func (b *projectKindTaskBackend) RerunInit(ctx context.Context, taskID string) (task.TaskRow, error) {
	return task.TaskRow{ID: taskID, ProjectID: b.projectID, Status: task.StatusSuspended, InitStatus: task.InitStatusRunning}, nil
}

// TestTaskDTO_ProjectKind_FilledAtListCreateGetRerunInit 验证 task DTO 的 project_kind 字段
// 在 List/Create/Get/RerunInit 四个响应消费点均从项目详情填充（D6）。
func TestTaskDTO_ProjectKind_FilledAtListCreateGetRerunInit(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["pdir"] = storeProjectRow{ID: "pdir", Name: "d", Path: "/d", Kind: "dir"}
	projs.projects["prep"] = storeProjectRow{ID: "prep", Name: "r", Path: "/r", Kind: "repo"}
	lc := newFakeLifecycleConfigStore()

	newSrv := func(projectID string) *Server {
		tb := &projectKindTaskBackend{fakeTaskBackend: &fakeTaskBackend{}, projectID: projectID}
		return newLifecycleAPIServer(t, projs, lc, tb)
	}

	t.Run("List_dir", func(t *testing.T) {
		s := newSrv("pdir")
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/pdir/tasks", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var list []taskRowDTO
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].ProjectKind != "dir" {
			t.Errorf("list = %+v, want project_kind=dir", list)
		}
	})

	t.Run("Create_repo", func(t *testing.T) {
		s := newSrv("prep")
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/prep/tasks", `{"name":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		var dto taskRowDTO
		if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
			t.Fatal(err)
		}
		if dto.ProjectKind != "repo" {
			t.Errorf("ProjectKind = %q, want 'repo'", dto.ProjectKind)
		}
	})

	t.Run("Get_dir", func(t *testing.T) {
		s := newSrv("pdir")
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var dto taskRowDTO
		if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
			t.Fatal(err)
		}
		if dto.ProjectKind != "dir" {
			t.Errorf("ProjectKind = %q, want 'dir'", dto.ProjectKind)
		}
	})

	t.Run("RerunInit_repo", func(t *testing.T) {
		s := newSrv("prep")
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/t1/rerun-init", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var dto taskRowDTO
		if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
			t.Fatal(err)
		}
		if dto.ProjectKind != "repo" {
			t.Errorf("ProjectKind = %q, want 'repo' (prefetched before RerunInit side-effect)", dto.ProjectKind)
		}
	})
}

// --- add-plain-dir-project D1/D6：API fail-closed 路径 ---

// TestTaskDTO_FailClosed_ProjectNotFound 验证各消费点项目不存在时返回 not_found（404），
// 不输出空 project_kind / 不执行副作用。
func TestTaskDTO_FailClosed_ProjectNotFound(t *testing.T) {
	projs := newFakeProjectStore() // 无项目
	lc := newFakeLifecycleConfigStore()
	tb := newLifecycleTaskBackend(task.TaskRow{ID: "t1", ProjectID: "missing", Status: task.StatusSuspended})
	doReq := func(t *testing.T, method, url string) *http.Response {
		t.Helper()
		resp, err := http.DefaultClient.Do(authedReq(method, url, ""))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("List_404", func(t *testing.T) {
		s := newLifecycleAPIServer(t, projs, lc, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp := doReq(t, "GET", ts.URL+"/api/v1/projects/missing/tasks")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("Get_404", func(t *testing.T) {
		s := newLifecycleAPIServer(t, projs, lc, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp := doReq(t, "GET", ts.URL+"/api/v1/tasks/t1")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("Create_404", func(t *testing.T) {
		s := newLifecycleAPIServer(t, projs, lc, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/missing/tasks", `{"name":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (no create before kind resolved)", resp.StatusCode)
		}
	})
	t.Run("RerunInit_404_no_side_effect", func(t *testing.T) {
		// 项目不存在 → MUST NOT 调用 RerunInit（零 claim/脚本副作用）。
		called := false
		tb2 := newLifecycleTaskBackend(task.TaskRow{ID: "t1", ProjectID: "missing", Status: task.StatusSuspended})
		tb2.rerunFn = func(ctx context.Context, taskID string) (task.TaskRow, error) {
			called = true
			return task.TaskRow{}, nil
		}
		s := newLifecycleAPIServer(t, projs, lc, tb2)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/t1/rerun-init", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if called {
			t.Error("RerunInit was called despite project not found (must fail-closed before side-effect)")
		}
	})
}

// TestTaskDTO_FailClosed_UnknownPersistedKind_500 验证未知持久化 kind → internal（500），
// 不执行副作用（D1：持久化未知 kind 区别于用户请求非法 kind 的 invalid_input）。
func TestTaskDTO_FailClosed_UnknownPersistedKind_500(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["pbad"] = storeProjectRow{ID: "pbad", Name: "p", Path: "/x", Kind: "weird"}
	lc := newFakeLifecycleConfigStore()
	tb := newLifecycleTaskBackend(task.TaskRow{ID: "t1", ProjectID: "pbad", Status: task.StatusSuspended})

	t.Run("List_500", func(t *testing.T) {
		s := newLifecycleAPIServer(t, projs, lc, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/pbad/tasks", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (unknown persisted kind)", resp.StatusCode)
		}
	})
	t.Run("Create_500", func(t *testing.T) {
		s := newLifecycleAPIServer(t, projs, lc, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/pbad/tasks", `{"name":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (no create, unknown persisted kind)", resp.StatusCode)
		}
	})
	t.Run("RerunInit_500_no_side_effect", func(t *testing.T) {
		called := false
		tb2 := newLifecycleTaskBackend(task.TaskRow{ID: "t1", ProjectID: "pbad", Status: task.StatusSuspended})
		tb2.rerunFn = func(ctx context.Context, taskID string) (task.TaskRow, error) {
			called = true
			return task.TaskRow{}, nil
		}
		s := newLifecycleAPIServer(t, projs, lc, tb2)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/t1/rerun-init", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		if called {
			t.Error("RerunInit was called despite unknown persisted kind (must fail-closed)")
		}
	})
}
