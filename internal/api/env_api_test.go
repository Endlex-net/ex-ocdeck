package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"ocdeck/internal/task"
)

// fakeEnvStore 内存实现 EnvStore，用于 env API 测试。
type fakeEnvStore struct {
	projectEnv map[string]map[string]string // projectID -> key -> value
	taskEnv    map[string]map[string]string // taskID -> key -> value
	globalEnv  map[string]globalEnvVarRow   // key -> row
}

func newFakeEnvStore() *fakeEnvStore {
	return &fakeEnvStore{
		projectEnv: map[string]map[string]string{},
		taskEnv:    map[string]map[string]string{},
		globalEnv:  map[string]globalEnvVarRow{},
	}
}

func (s *fakeEnvStore) ListProjectEnvVars(ctx context.Context, projectID string) ([]envVarRow, error) {
	m := s.projectEnv[projectID]
	out := make([]envVarRow, 0, len(m))
	for k, v := range m {
		out = append(out, envVarRow{Key: k, Value: v})
	}
	return out, nil
}
func (s *fakeEnvStore) SetProjectEnvVar(ctx context.Context, projectID, key, value string) error {
	if s.projectEnv[projectID] == nil {
		s.projectEnv[projectID] = map[string]string{}
	}
	s.projectEnv[projectID][key] = value
	return nil
}
func (s *fakeEnvStore) DeleteProjectEnvVar(ctx context.Context, projectID, key string) error {
	delete(s.projectEnv[projectID], key)
	return nil
}
func (s *fakeEnvStore) ListTaskEnvVars(ctx context.Context, taskID string) ([]envVarRow, error) {
	m := s.taskEnv[taskID]
	out := make([]envVarRow, 0, len(m))
	for k, v := range m {
		out = append(out, envVarRow{Key: k, Value: v})
	}
	return out, nil
}
func (s *fakeEnvStore) SetTaskEnvVar(ctx context.Context, taskID, key, value string) error {
	if s.taskEnv[taskID] == nil {
		s.taskEnv[taskID] = map[string]string{}
	}
	s.taskEnv[taskID][key] = value
	return nil
}
func (s *fakeEnvStore) DeleteTaskEnvVar(ctx context.Context, taskID, key string) error {
	delete(s.taskEnv[taskID], key)
	return nil
}

func (s *fakeEnvStore) ListGlobalEnvVars(ctx context.Context) ([]globalEnvVarRow, error) {
	out := make([]globalEnvVarRow, 0, len(s.globalEnv))
	for _, e := range s.globalEnv {
		out = append(out, e)
	}
	return out, nil
}
func (s *fakeEnvStore) SetGlobalEnvVar(ctx context.Context, key, mode, value string) error {
	s.globalEnv[key] = globalEnvVarRow{Key: key, Mode: mode, Value: value}
	return nil
}
func (s *fakeEnvStore) DeleteGlobalEnvVar(ctx context.Context, key string) error {
	delete(s.globalEnv, key)
	return nil
}

// newEnvAPIServer 构造带 ProjectStore + EnvStore + TaskBackend 的 Server。
func newEnvAPIServer(t *testing.T, projs ProjectStore, envs EnvStore, tb TaskBackend) *Server {
	t.Helper()
	cfg := testConfig()
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
		projs:     projs,
		envs:      envs,
		tasks:     tb,
	}
	s.registerRoutes()
	return s
}

func TestEnvAPI_ProjectCRUD(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main"}
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// PUT upsert。
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/env", `{"key":"FOO","value":"bar"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d, want 200", resp.StatusCode)
	}
	var mut envMutationResponse
	if err := json.NewDecoder(resp.Body).Decode(&mut); err != nil {
		t.Fatal(err)
	}
	if !mut.RestartRequired {
		t.Error("restartRequired should be true")
	}
	if mut.Warning == "" {
		t.Error("warning should be present (plaintext risk)")
	}

	// GET list。
	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/p1/env", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var list envListResponse
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range list.Vars {
		if e.Key == "FOO" && e.Value == "bar" {
			found = true
		}
	}
	if !found {
		t.Errorf("FOO=bar not in list: %+v", list.Vars)
	}

	// DELETE。
	resp3, err := http.DefaultClient.Do(authedReq("DELETE", ts.URL+"/api/v1/projects/p1/env/FOO", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp3.StatusCode)
	}
	if _, ok := envs.projectEnv["p1"]["FOO"]; ok {
		t.Error("FOO should be deleted")
	}
}

func TestEnvAPI_ProjectEnvNotFound_404(t *testing.T) {
	projs := newFakeProjectStore()
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/nope/env", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEnvAPI_ProjectEnvEmptyKey_422(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1"}
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/env", `{"key":"","value":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestEnvAPI_TaskCRUD(t *testing.T) {
	envs := newFakeEnvStore()
	tb := &gitTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		tasks:           map[string]task.TaskRow{"t1": {ID: "t1", ProjectID: "p1", Status: task.StatusSuspended}},
	}
	projs := newFakeProjectStore()
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// PUT task env。
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/tasks/t1/env", `{"key":"K","value":"V"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d, want 200", resp.StatusCode)
	}
	if envs.taskEnv["t1"]["K"] != "V" {
		t.Errorf("task env K not set: %+v", envs.taskEnv)
	}

	// GET task env list。
	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1/env", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var list envListResponse
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range list.Vars {
		if e.Key == "K" {
			found = true
		}
	}
	if !found {
		t.Errorf("K not in task env list: %+v", list.Vars)
	}

	// DELETE task env。
	resp3, err := http.DefaultClient.Do(authedReq("DELETE", ts.URL+"/api/v1/tasks/t1/env/K", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp3.StatusCode)
	}
	if _, ok := envs.taskEnv["t1"]["K"]; ok {
		t.Error("K should be deleted")
	}
}

func TestEnvAPI_TaskEnvNotFound_404(t *testing.T) {
	envs := newFakeEnvStore()
	tb := &gitTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		tasks:           map[string]task.TaskRow{},
	}
	projs := newFakeProjectStore()
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/nope/env", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestEnvAPI_ReservedKeyRejected 验证 PUT（项目级与任务级）MUST 拒绝保留 key：
// 全部 OCDECK_* 前缀与 OPENCODE_SERVER_PASSWORD → 422 invalid_input。
// env-management spec "用户变量不覆盖内部变量" 的 API 侧防线。
func TestEnvAPI_ReservedKeyRejected(t *testing.T) {
	reserved := []string{
		"OCDECK_SERVE_PORT", "OCDECK_TASK_ID", "OCDECK_TASK_NAME",
		"OCDECK_TASK_PATH", "OCDECK_PROJECT_PATH",
		"OPENCODE_SERVER_PASSWORD",
	}
	// 项目级：项目存在 + 完整 PUT。
	t.Run("project", func(t *testing.T) {
		projs := newFakeProjectStore()
		projs.projects["p1"] = storeProjectRow{ID: "p1"}
		envs := newFakeEnvStore()
		tb := &fakeTaskBackend{}
		s := newEnvAPIServer(t, projs, envs, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()

		for _, key := range reserved {
			body := `{"key":"` + key + `","value":"v"}`
			resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/env", body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("project PUT reserved key %q: status = %d, want 422", key, resp.StatusCode)
			}
		}
		// 确认保留 key 未落库。
		for _, key := range reserved {
			if _, ok := envs.projectEnv["p1"][key]; ok {
				t.Errorf("reserved key %q should not be stored", key)
			}
		}
	})

	// 任务级：任务存在 + 完整 PUT。
	t.Run("task", func(t *testing.T) {
		envs := newFakeEnvStore()
		tb := &gitTaskBackend{
			fakeTaskBackend: &fakeTaskBackend{},
			tasks:           map[string]task.TaskRow{"t1": {ID: "t1", ProjectID: "p1", Status: task.StatusSuspended}},
		}
		projs := newFakeProjectStore()
		s := newEnvAPIServer(t, projs, envs, tb)
		ts := httptest.NewServer(s.mux)
		defer ts.Close()

		for _, key := range reserved {
			body := `{"key":"` + key + `","value":"v"}`
			resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/tasks/t1/env", body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("task PUT reserved key %q: status = %d, want 422", key, resp.StatusCode)
			}
		}
		for _, key := range reserved {
			if _, ok := envs.taskEnv["t1"][key]; ok {
				t.Errorf("reserved key %q should not be stored", key)
			}
		}
	})
}

// TestEnvAPI_NonReservedKeyAccepted 验证非保留 key（含 OCDECK 无下划线、小写 ocdeck_）可写入。
func TestEnvAPI_NonReservedKeyAccepted(t *testing.T) {
	allowed := []string{
		"FOO", "MY_VAR", "OCDECK", "OCDECKOTHER", "ocdeck_serve_port",
	}
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1"}
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	for _, key := range allowed {
		body := `{"key":"` + key + `","value":"v"}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/env", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("project PUT non-reserved key %q: status = %d, want 200", key, resp.StatusCode)
		}
	}
}

func TestEnvKeyReserved(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"OCDECK_TASK_ID", true},
		{"OCDECK_X", true},
		{"OPENCODE_SERVER_PASSWORD", true},
		{"FOO", false},
		{"ocdeck_task_id", false}, // 大小写敏感：仅大写 OCDECK_ 前缀保留
		{"OCDECK", false},         // 无下划线：非前缀
		{"MY_OCDECK_VAR", false},  // OCDECK_ 不在开头
		{"", false},
	}
	for _, c := range cases {
		if got := envKeyReserved(c.key); got != c.want {
			t.Errorf("envKeyReserved(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

// --- 全局级 env API（design.md §21：GET/PUT/DELETE /api/v1/env） ---

func TestEnvAPI_GlobalCRUD_Manual(t *testing.T) {
	projs := newFakeProjectStore()
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// PUT manual。
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/env", `{"key":"FOO","mode":"manual","value":"bar"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d, want 200", resp.StatusCode)
	}
	var mut envMutationResponse
	if err := json.NewDecoder(resp.Body).Decode(&mut); err != nil {
		t.Fatal(err)
	}
	if !mut.RestartRequired {
		t.Error("restartRequired should be true on mutation")
	}

	// GET list：manual resolvedValue == value。
	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/env", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var list globalEnvListResponse
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.RestartRequired {
		t.Error("GET restartRequired should be false")
	}
	if list.Warning == "" {
		t.Error("warning should be present")
	}
	var got *globalEnvVarDTO
	for i := range list.Vars {
		if list.Vars[i].Key == "FOO" {
			got = &list.Vars[i]
		}
	}
	if got == nil {
		t.Fatalf("FOO not in list: %+v", list.Vars)
	}
	if got.Mode != "manual" || got.Value != "bar" || got.ResolvedValue != "bar" {
		t.Errorf("FOO = %+v, want mode=manual value=bar resolvedValue=bar", *got)
	}

	// DELETE。
	resp3, err := http.DefaultClient.Do(authedReq("DELETE", ts.URL+"/api/v1/env/FOO", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp3.StatusCode)
	}
	if _, ok := envs.globalEnv["FOO"]; ok {
		t.Error("FOO should be deleted")
	}
}

func TestEnvAPI_GlobalFollowHost_ResolvedValue(t *testing.T) {
	t.Setenv("GLOBAL_VAR_HOST", "host-value")
	projs := newFakeProjectStore()
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/env", `{"key":"GLOBAL_VAR_HOST","mode":"follow_host","value":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put follow_host status = %d, want 200", resp.StatusCode)
	}

	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/env", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var list globalEnvListResponse
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	var got *globalEnvVarDTO
	for i := range list.Vars {
		if list.Vars[i].Key == "GLOBAL_VAR_HOST" {
			got = &list.Vars[i]
		}
	}
	if got == nil {
		t.Fatalf("GLOBAL_VAR_HOST not in list: %+v", list.Vars)
	}
	if got.Mode != "follow_host" || got.Value != "" {
		t.Errorf("stored = %+v, want follow_host/empty value", *got)
	}
	if got.ResolvedValue != "host-value" {
		t.Errorf("resolvedValue = %q, want host-value (from os env)", got.ResolvedValue)
	}
}

func TestEnvAPI_GlobalFollowHost_UnsetResolvedEmpty(t *testing.T) {
	os.Unsetenv("GLOBAL_UNSET_KEY")
	projs := newFakeProjectStore()
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/env", `{"key":"GLOBAL_UNSET_KEY","mode":"follow_host","value":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d, want 200", resp.StatusCode)
	}

	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/env", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var list globalEnvListResponse
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	var got *globalEnvVarDTO
	for i := range list.Vars {
		if list.Vars[i].Key == "GLOBAL_UNSET_KEY" {
			got = &list.Vars[i]
		}
	}
	if got == nil {
		t.Fatalf("GLOBAL_UNSET_KEY not in list")
	}
	if got.ResolvedValue != "" {
		t.Errorf("resolvedValue = %q, want empty (host unset)", got.ResolvedValue)
	}
}

func TestEnvAPI_GlobalInvalidMode_422(t *testing.T) {
	projs := newFakeProjectStore()
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/env", `{"key":"K","mode":"bogus","value":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if _, ok := envs.globalEnv["K"]; ok {
		t.Error("invalid mode should not be stored")
	}
}

func TestEnvAPI_GlobalReservedKey_422(t *testing.T) {
	projs := newFakeProjectStore()
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	reserved := []string{"OCDECK_TASK_ID", "OPENCODE_SERVER_PASSWORD"}
	for _, key := range reserved {
		body := `{"key":"` + key + `","mode":"manual","value":"v"}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/env", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("global PUT reserved key %q: status = %d, want 422", key, resp.StatusCode)
		}
		if _, ok := envs.globalEnv[key]; ok {
			t.Errorf("reserved key %q should not be stored", key)
		}
	}
}

func TestEnvAPI_GlobalEmptyKey_422(t *testing.T) {
	projs := newFakeProjectStore()
	envs := newFakeEnvStore()
	tb := &fakeTaskBackend{}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/env", `{"key":"","mode":"manual","value":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

// TestEnvAPI_InvalidChars_422 验证 S1：非法 env key（不符合 ^[A-Za-z_][A-Za-z0-9_]*$）
// MUST 在 API 层拒绝（422 invalid_input），否则入库后激活时被 process 层拒绝留脏数据。
// 覆盖三级统一校验：全局 / 项目 / 任务。
func TestEnvAPI_InvalidChars_422(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main"}
	envs := newFakeEnvStore()
	tb := &gitTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		tasks:           map[string]task.TaskRow{"t1": {ID: "t1", ProjectID: "p1", Status: task.StatusSuspended}},
	}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 非法 key 样例：数字开头、含空格、含连字符、含特殊字符。
	invalidKeys := []string{"1BAD", "BAD KEY", "BAD-KEY", "BAD.KEY", "BAD=KEY", "你好"}

	// 全局级 PUT（mode=manual，避免 mode 校验先失败）。
	for _, key := range invalidKeys {
		body := `{"key":"` + key + `","mode":"manual","value":"v"}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/env", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("global PUT invalid key %q: status = %d, want 422", key, resp.StatusCode)
		}
		if _, ok := envs.globalEnv[key]; ok {
			t.Errorf("global invalid key %q should not be stored", key)
		}
	}

	// 项目级 PUT。
	for _, key := range invalidKeys {
		body := `{"key":"` + key + `","value":"v"}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/env", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("project PUT invalid key %q: status = %d, want 422", key, resp.StatusCode)
		}
		if _, ok := envs.projectEnv["p1"][key]; ok {
			t.Errorf("project invalid key %q should not be stored", key)
		}
	}

	// 任务级 PUT。
	for _, key := range invalidKeys {
		body := `{"key":"` + key + `","value":"v"}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/tasks/t1/env", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("task PUT invalid key %q: status = %d, want 422", key, resp.StatusCode)
		}
		if _, ok := envs.taskEnv["t1"][key]; ok {
			t.Errorf("task invalid key %q should not be stored", key)
		}
	}
}

// TestEnvAPI_ValidKeys_Stored 验证 S1：合法 env key（符合 ^[A-Za-z_][A-Za-z0-9_]*$）
// MUST 被接受存储，三级均通过校验（正向用例，确保校验未误伤合法 key）。
func TestEnvAPI_ValidKeys_Stored(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/x", DefaultBranch: "main"}
	envs := newFakeEnvStore()
	tb := &gitTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		tasks:           map[string]task.TaskRow{"t1": {ID: "t1", ProjectID: "p1", Status: task.StatusSuspended}},
	}
	s := newEnvAPIServer(t, projs, envs, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	validKeys := []string{"FOO", "_BAR", "a", "A1_B2", "x_1_y_2"}

	for _, key := range validKeys {
		body := `{"key":"` + key + `","mode":"manual","value":"v"}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/env", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("global PUT valid key %q: status = %d, want 200", key, resp.StatusCode)
		}
	}
	for _, key := range validKeys {
		body := `{"key":"` + key + `","value":"v"}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/projects/p1/env", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("project PUT valid key %q: status = %d, want 200", key, resp.StatusCode)
		}
	}
	for _, key := range validKeys {
		body := `{"key":"` + key + `","value":"v"}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/tasks/t1/env", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("task PUT valid key %q: status = %d, want 200", key, resp.StatusCode)
		}
	}
}
