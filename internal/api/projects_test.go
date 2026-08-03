package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ocdeck/internal/config"
	"ocdeck/internal/store"
)

// fakeProjectStore 内存实现 ProjectStore，用于测试。
type fakeProjectStore struct {
	projects map[string]storeProjectRow
	byPath   map[string]string // path -> projectID
	counts   map[string]storeTaskCounts
}

func newFakeProjectStore() *fakeProjectStore {
	return &fakeProjectStore{
		projects: map[string]storeProjectRow{},
		byPath:   map[string]string{},
		counts:   map[string]storeTaskCounts{},
	}
}

func (f *fakeProjectStore) CreateProject(ctx context.Context, id, name, path, defaultBranch string) error {
	if _, ok := f.byPath[path]; ok {
		return errors.New("UNIQUE constraint failed: projects.path")
	}
	f.projects[id] = storeProjectRow{ID: id, Name: name, Path: path, DefaultBranch: defaultBranch, CreatedAt: 1}
	f.byPath[path] = id
	return nil
}

func (f *fakeProjectStore) GetProject(ctx context.Context, id string) (storeProjectRow, error) {
	p, ok := f.projects[id]
	if !ok {
		return storeProjectRow{}, errors.New("not found")
	}
	return p, nil
}

func (f *fakeProjectStore) GetProjectByPath(ctx context.Context, path string) (storeProjectRow, error) {
	id, ok := f.byPath[path]
	if !ok {
		return storeProjectRow{}, errors.New("not found")
	}
	return f.projects[id], nil
}

func (f *fakeProjectStore) ListProjects(ctx context.Context) ([]storeProjectRow, error) {
	out := make([]storeProjectRow, 0, len(f.projects))
	for _, p := range f.projects {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeProjectStore) DeleteProjectIfEmpty(ctx context.Context, id string) (bool, error) {
	p, ok := f.projects[id]
	if !ok {
		return false, nil // 不存在：未删除
	}
	delete(f.projects, id)
	delete(f.byPath, p.Path)
	return true, nil
}

func (f *fakeProjectStore) CountProjectTasks(ctx context.Context, projectID string) (storeTaskCounts, error) {
	if c, ok := f.counts[projectID]; ok {
		return c, nil
	}
	return storeTaskCounts{Total: 0, ByStatus: map[string]int{}}, nil
}

func (f *fakeProjectStore) HasProjectTasks(ctx context.Context, projectID string) (bool, error) {
	return false, nil
}

// hasTasksStore 包装 fakeProjectStore，模拟项目下有任务：DeleteProjectIfEmpty 拒绝删除。
type hasTasksStore struct {
	*fakeProjectStore
	has bool
}

func (h *hasTasksStore) DeleteProjectIfEmpty(ctx context.Context, id string) (bool, error) {
	if h.has {
		return false, nil // 有任务：原子删除未生效
	}
	return h.fakeProjectStore.DeleteProjectIfEmpty(ctx, id)
}

func newServerWithStore(t *testing.T, projs ProjectStore) *Server {
	t.Helper()
	cfg := &config.Config{Token: "testtoken", ListenAddr: "127.0.0.1", ShutdownPolicy: config.ShutdownPersist}
	return WithProjectStore(cfg, nil, projs)
}

func authedReq(method, url, body string) *http.Request {
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer testtoken")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitCmd(t, dir, "init", "-q")
	runGitCmd(t, dir, "config", "user.email", "t@t.com")
	runGitCmd(t, dir, "config", "user.name", "tester")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("init\n"), 0o644)
	runGitCmd(t, dir, "add", "README.md")
	runGitCmd(t, dir, "commit", "-qm", "init")
	return dir
}

func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCreateProject_Success(t *testing.T) {
	repo := newTestRepo(t)
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"my project","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var dto projectDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	repoCanon, _ := filepath.EvalSymlinks(repo)
	if dto.Name != "my project" || dto.Path != repoCanon || dto.DefaultBranch != "main" {
		t.Errorf("dto = %+v, want path=%s", dto, repoCanon)
	}
}

func TestCreateProject_NotARepo(t *testing.T) {
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	notRepo := t.TempDir() // 空 dir，非 git repo
	body := `{"name":"p","path":` + quoteJSON(notRepo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
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

func TestCreateProject_NonexistentPath(t *testing.T) {
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"p","path":"/nonexistent-xyz-abc"}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestCreateProject_DuplicatePath_409(t *testing.T) {
	repo := newTestRepo(t)
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"p1","path":` + quoteJSON(repo) + `}`
	resp1, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp1.StatusCode)
	}
	resp2, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409", resp2.StatusCode)
	}
}

func TestCreateProject_EmptyName_422(t *testing.T) {
	repo := newTestRepo(t)
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"  ","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestListProjects(t *testing.T) {
	repo := newTestRepo(t)
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	// 创建一个项目。
	body := `{"name":"p","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp2.StatusCode)
	}
	var list []projectDTO
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("list len = %d, want 1", len(list))
	}
}

func TestGetProject_NotFound(t *testing.T) {
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/nope", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetProject_DetailWithTaskCounts(t *testing.T) {
	repo := newTestRepo(t)
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"p","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	var created projectDTO
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/"+created.ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp2.StatusCode)
	}
	var detail projectDetailDTO
	if err := json.NewDecoder(resp2.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail.ID != created.ID {
		t.Errorf("id = %s, want %s", detail.ID, created.ID)
	}
	if detail.TaskCount != 0 {
		t.Errorf("task_count = %d, want 0", detail.TaskCount)
	}
	if detail.Tasks == nil {
		t.Error("tasks_by_status should be non-nil map")
	}
}

func TestDeleteProject_HasTasks_409(t *testing.T) {
	repo := newTestRepo(t)
	projs := &hasTasksStore{fakeProjectStore: newFakeProjectStore(), has: true}
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	// 先注册项目。
	body := `{"name":"p","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	var created projectDTO
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// 删除应 409（有任务）。
	resp2, err := http.DefaultClient.Do(authedReq("DELETE", ts.URL+"/api/v1/projects/"+created.ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp2.StatusCode)
	}
}

func TestDeleteProject_Empty_204(t *testing.T) {
	repo := newTestRepo(t)
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"p","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	var created projectDTO
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	resp2, err := http.DefaultClient.Do(authedReq("DELETE", ts.URL+"/api/v1/projects/"+created.ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp2.StatusCode)
	}
}

func TestDeleteProject_NotFound_404(t *testing.T) {
	projs := newFakeProjectStore()
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("DELETE", ts.URL+"/api/v1/projects/nope", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// quoteJSON 将字符串转为 JSON 字符串字面量（含引号）。
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// 编译期断言 bytes 仍被引用。
var _ = bytes.Buffer{}

// newServerWithRealStore 用真实 *store.DB（经适配器）构造 Server，供原子性/归一化测试。
func newServerWithRealStore(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{Token: "testtoken", ListenAddr: "127.0.0.1", ShutdownPolicy: config.ShutdownPersist}
	return WithProjectStore(cfg, db, NewProjectStoreAdapter(db)), db
}

// TestCreateProject_SymlinkNormalization 验证注册路径经 EvalSymlinks 归一后存储，
// 同一仓库经 symlink 别名注册两次应 409。
func TestCreateProject_SymlinkNormalization(t *testing.T) {
	repo := newTestRepo(t)
	srv, db := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	// 创建指向 repo 的 symlink 别名。
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// 用真实路径注册。
	body := `{"name":"p1","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp.StatusCode)
	}

	// 用 symlink 别名注册同一仓库应 409（归一后 path 相同）。
	body2 := `{"name":"p2","path":` + quoteJSON(link) + `}`
	resp2, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body2))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("symlink alias create status = %d, want 409", resp2.StatusCode)
	}

	// DB 中应只有一条项目记录。
	var n int
	if err := db.QueryRow("SELECT count(*) FROM projects").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("projects count = %d, want 1 (symlink normalized)", n)
	}
}

// TestDeleteProject_AtomicWithRealStore 验证原子删除：有任务 409、空 204、不存在 404。
func TestDeleteProject_AtomicWithRealStore(t *testing.T) {
	srv, db := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	ctx := context.Background()

	// 注册空项目。
	repo := newTestRepo(t)
	body := `{"name":"p","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	var created projectDTO
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// 空 → 204。
	dresp, err := http.DefaultClient.Do(authedReq("DELETE", ts.URL+"/api/v1/projects/"+created.ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty delete status = %d, want 204", dresp.StatusCode)
	}

	// 注册第二个项目并加任务。
	repo2 := newTestRepo(t)
	body2 := `{"name":"p2","path":` + quoteJSON(repo2) + `}`
	resp2, _ := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body2))
	var created2 projectDTO
	json.NewDecoder(resp2.Body).Decode(&created2)
	resp2.Body.Close()
	if err := db.CreateTask(ctx, store.TaskRow{
		ID: "tk1", ProjectID: created2.ID, Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	}); err != nil {
		t.Fatal(err)
	}

	// 有任务 → 409。
	dresp2, err := http.DefaultClient.Do(authedReq("DELETE", ts.URL+"/api/v1/projects/"+created2.ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer dresp2.Body.Close()
	if dresp2.StatusCode != http.StatusConflict {
		t.Fatalf("delete with tasks status = %d, want 409", dresp2.StatusCode)
	}
	// 项目仍在。
	if _, err := db.GetProject(ctx, created2.ID); err != nil {
		t.Errorf("project should still exist after 409: %v", err)
	}

	// 不存在 → 404。
	dresp3, err := http.DefaultClient.Do(authedReq("DELETE", ts.URL+"/api/v1/projects/nonexistent", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer dresp3.Body.Close()
	if dresp3.StatusCode != http.StatusNotFound {
		t.Fatalf("nonexistent delete status = %d, want 404", dresp3.StatusCode)
	}
}

// TestListProjects_WithTaskCounts 验证 GET /api/v1/projects 每项含 task_count 与
// tasks_by_status（B7-backend：列表概况与详情/前端 Project 类型字段一致）。
// 逐项目 CountProjectTasks 取概况（个人规模 N+1 可接受）。
func TestListProjects_WithTaskCounts(t *testing.T) {
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p1", Path: "/x", DefaultBranch: "main", CreatedAt: 1}
	projs.projects["p2"] = storeProjectRow{ID: "p2", Name: "p2", Path: "/y", DefaultBranch: "main", CreatedAt: 2}
	projs.counts["p1"] = storeTaskCounts{Total: 3, ByStatus: map[string]int{"active": 2, "suspended": 1}}
	projs.counts["p2"] = storeTaskCounts{Total: 0, ByStatus: map[string]int{}}
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list []projectDTO
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	byID := map[string]projectDTO{}
	for _, p := range list {
		byID[p.ID] = p
	}
	// p1：概况填充。
	if got := byID["p1"]; got.TaskCount != 3 || got.Tasks["active"] != 2 || got.Tasks["suspended"] != 1 {
		t.Errorf("p1 counts = %+v, want {Total:3 active:2 suspended:1}", got)
	}
	// p2：无任务时 task_count=0、tasks_by_status 非 nil（前端字段稳定）。
	if got := byID["p2"]; got.TaskCount != 0 || got.Tasks == nil {
		t.Errorf("p2 counts = %+v, want Total=0 non-nil Tasks", got)
	}
}

// TestListProjects_CountError_500 验证列表逐项目取概况失败时返回 internal error（不静默吞错）。
func TestListProjects_CountError_500(t *testing.T) {
	projs := &countErrStore{fakeProjectStore: newFakeProjectStore()}
	projs.projects["p1"] = storeProjectRow{ID: "p1", Path: "/x"}
	srv := newServerWithStore(t, projs)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (count error surfaced)", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeInternal {
		t.Errorf("code = %s, want internal", eb.Error.Code)
	}
}

// countErrStore 包装 fakeProjectStore，CountProjectTasks 恒失败。
type countErrStore struct {
	*fakeProjectStore
}

func (c *countErrStore) CountProjectTasks(ctx context.Context, projectID string) (storeTaskCounts, error) {
	return storeTaskCounts{}, errors.New("count boom")
}
