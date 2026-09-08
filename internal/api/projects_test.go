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
	"sort"
	"strings"
	"testing"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/store"
)

// fakeProjectStore 内存实现 ProjectStore，用于测试。
type fakeProjectStore struct {
	projects map[string]storeProjectRow
	counts   map[string]storeTaskCounts
}

func newFakeProjectStore() *fakeProjectStore {
	return &fakeProjectStore{
		projects: map[string]storeProjectRow{},
		counts:   map[string]storeTaskCounts{},
	}
}

func (f *fakeProjectStore) CreateProject(ctx context.Context, id, name, path, defaultBranch, kind string) error {
	f.projects[id] = storeProjectRow{ID: id, Name: name, Path: path, DefaultBranch: defaultBranch, Kind: kind, CreatedAt: 1}
	return nil
}

func (f *fakeProjectStore) GetProject(ctx context.Context, id string) (storeProjectRow, error) {
	p, ok := f.projects[id]
	if !ok {
		return storeProjectRow{}, errors.New("not found")
	}
	return p, nil
}

func (f *fakeProjectStore) ListProjects(ctx context.Context) ([]storeProjectRow, error) {
	out := make([]storeProjectRow, 0, len(f.projects))
	for _, p := range f.projects {
		out = append(out, p)
	}
	// 按 ID 排序保证确定性（map 迭代乱序会使快照/REST parity 断言不稳定）。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeProjectStore) DeleteProjectIfEmpty(ctx context.Context, id string) (bool, error) {
	if _, ok := f.projects[id]; !ok {
		return false, nil // 不存在：未删除
	}
	delete(f.projects, id)
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
	runGitCmd(t, dir, "init", "-q", "-b", "main")
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

// TestCreateProject_DuplicatePath_201 验证同一 canonical path 可注册多个项目
// （allow-duplicate-project-path tasks 2.3）：repo+repo、dir+dir、repo/dir 交叉均 201，
// 不返回 409；两次创建 id 不同、两条记录均可按 id 回读、path 均为 canonical 归一结果、
// kind 各自正确；DB 中该 path 恰好两条记录（无 UNIQUE 约束）；响应 DTO 无新增字段。
func TestCreateProject_DuplicatePath_201(t *testing.T) {
	cases := []struct {
		name       string
		firstKind  string
		secondKind string
	}{
		{name: "repo_repo", firstKind: "repo", secondKind: "repo"},
		{name: "dir_dir", firstKind: "dir", secondKind: "dir"},
		{name: "repo_dir_cross", firstKind: "repo", secondKind: "dir"},
		{name: "dir_repo_cross", firstKind: "dir", secondKind: "repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// repo 注册校验要求路径本身为合法 git 仓库，交叉场景同样成立。
			repo := newTestRepo(t)
			repoCanon, err := filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			srv, db := newServerWithRealStore(t)
			ts := httptest.NewServer(srv.mux)
			defer ts.Close()

			create := func(name, kind string) projectDTO {
				t.Helper()
				body := `{"name":` + quoteJSON(name) + `,"kind":"` + kind + `","path":` + quoteJSON(repo) + `}`
				resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusCreated {
					t.Fatalf("create %s (kind=%s) status = %d, want 201", name, kind, resp.StatusCode)
				}
				var dto projectDTO
				if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
					t.Fatal(err)
				}
				return dto
			}
			first := create("p1", tc.firstKind)
			second := create("p2", tc.secondKind)

			if first.ID == "" || second.ID == "" || first.ID == second.ID {
				t.Errorf("ids = %q / %q, want distinct non-empty (各自独立)", first.ID, second.ID)
			}
			for _, c := range []struct {
				dto  projectDTO
				kind string
			}{{first, tc.firstKind}, {second, tc.secondKind}} {
				// 按 id 回读（两条记录均独立存在）。
				p, err := db.GetProject(context.Background(), c.dto.ID)
				if err != nil {
					t.Fatalf("get created project %s: %v", c.dto.ID, err)
				}
				if p.Path != repoCanon {
					t.Errorf("stored path = %q, want canonical %q", p.Path, repoCanon)
				}
				if p.Kind != c.kind {
					t.Errorf("stored kind = %q, want %q", p.Kind, c.kind)
				}
			}
			// dir 项目无默认分支；repo 项目探测到 main。
			if tc.firstKind == "dir" && first.DefaultBranch != "" {
				t.Errorf("first default_branch = %q, want '' (dir)", first.DefaultBranch)
			}
			if tc.secondKind == "repo" && second.DefaultBranch != "main" {
				t.Errorf("second default_branch = %q, want main (repo)", second.DefaultBranch)
			}
			// 该 path 恰好两条记录（projects.path 无 UNIQUE 约束）。
			var n int
			if err := db.QueryRow("SELECT count(*) FROM projects WHERE path = ?", repoCanon).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 2 {
				t.Errorf("projects with path = %d, want 2", n)
			}
		})
	}
}

// TestCreateProject_DuplicatePath_NoWarningField 验证重复 path 注册成功的响应 DTO
// 不新增字段（design D1：无 warning 字段，DTO 形状不变）。
func TestCreateProject_DuplicatePath_NoWarningField(t *testing.T) {
	repo := newTestRepo(t)
	srv, _ := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"p","path":` + quoteJSON(repo) + `}`
	for i := 0; i < 2; i++ {
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create #%d status = %d, want 201", i+1, resp.StatusCode)
		}
		if i == 0 {
			continue
		}
		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["warning"]; ok {
			t.Error("response DTO must not contain warning field")
		}
	}
}

// TestCreateProject_DifferentInputsSameCanonicalPath_201 验证不同输入归一到同一
// canonical path 后同样注册成功（tasks 2.3）：`" <repo>/ "` trim + 归一后与
// 已注册项目 path 相同，仍 201 且存储为归一后路径。
func TestCreateProject_DifferentInputsSameCanonicalPath_201(t *testing.T) {
	repo := newTestRepo(t)
	repoCanon, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	srv, db := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", `{"name":"p1","path":`+quoteJSON(repo)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp.StatusCode)
	}

	// 带前后空白与尾斜杠的输入，归一后与 repo 同一 canonical path。
	variant := " " + repo + "/ "
	resp2, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", `{"name":"p2","path":`+quoteJSON(variant)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("normalized-input create status = %d, want 201", resp2.StatusCode)
	}
	var dto projectDTO
	if err := json.NewDecoder(resp2.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.Path != repoCanon {
		t.Errorf("dto path = %q, want canonical %q", dto.Path, repoCanon)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM projects WHERE path = ?", repoCanon).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("projects with path = %d, want 2", n)
	}
}

// TestCreateProject_DuplicatePath_NotARepo_422 验证重复 path 不免除既有校验
// （spec「重复 path 不免除既有校验」）：路径已注册为 dir 项目，再以 kind=repo
// 提交同一路径（非 git 仓库）→ 422，且 MUST NOT 创建新项目记录。
func TestCreateProject_DuplicatePath_NotARepo_422(t *testing.T) {
	notRepo := t.TempDir() // 空 dir，非 git repo
	srv, db := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", `{"name":"p1","kind":"dir","path":`+quoteJSON(notRepo)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp.StatusCode)
	}

	resp2, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", `{"name":"p2","kind":"repo","path":`+quoteJSON(notRepo)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate non-repo status = %d, want 422", resp2.StatusCode)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM projects").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("projects count = %d, want 1 (校验失败 MUST NOT 创建记录)", n)
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

// TestCreateProject_SymlinkNormalization 验证注册路径经 EvalSymlinks 归一后存储：
// 同一仓库经 symlink 别名注册两次均成功，两条记录存储同一 canonical path
// （allow-duplicate-project-path：归一语义不变，path 不再拒绝重复）。
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

	// 用 symlink 别名注册同一仓库：归一后 path 相同，仍创建成功（不同 id）。
	body2 := `{"name":"p2","path":` + quoteJSON(link) + `}`
	resp2, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body2))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("symlink alias create status = %d, want 201", resp2.StatusCode)
	}
	var dto projectDTO
	if err := json.NewDecoder(resp2.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}

	// DB 中应有两条记录，path 均为真实路径的 canonical 归一结果。
	repoCanon, _ := filepath.EvalSymlinks(repo)
	if dto.Path != repoCanon {
		t.Errorf("alias dto path = %q, want canonical %q", dto.Path, repoCanon)
	}
	rows, err := db.Query("SELECT DISTINCT path FROM projects")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != repoCanon {
		t.Errorf("distinct stored paths = %v, want [%s] (canonical normalization)", paths, repoCanon)
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

// --- add-plain-dir-project Lane 2：项目 kind 注册（D1）与分支只读 API（D10） ---

// TestCreateProject_DirKind_Success 验证 kind=dir 注册成功：仅校验路径存在且为目录，
// default_branch 落空串，DTO 含 kind=dir。
func TestCreateProject_DirKind_Success(t *testing.T) {
	dir := t.TempDir()
	srv, db := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"dir project","kind":"dir","path":` + quoteJSON(dir) + `}`
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
	dirCanon, _ := filepath.EvalSymlinks(dir)
	if dto.Kind != "dir" {
		t.Errorf("kind = %q, want 'dir'", dto.Kind)
	}
	if dto.DefaultBranch != "" {
		t.Errorf("default_branch = %q, want '' (dir 无默认分支)", dto.DefaultBranch)
	}
	if dto.Path != dirCanon {
		t.Errorf("path = %q, want %q", dto.Path, dirCanon)
	}
	// 落库 kind 一致。
	p, _ := db.GetProject(context.Background(), dto.ID)
	if p.Kind != "dir" || p.DefaultBranch != "" {
		t.Errorf("store row = %+v, want kind=dir default_branch=''", p)
	}
}

// TestCreateProject_DirKind_NonexistentPath_422 验证 dir kind 拒绝不存在的路径。
func TestCreateProject_DirKind_NonexistentPath_422(t *testing.T) {
	srv, _ := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"p","kind":"dir","path":"/nonexistent-xyz-abc-123"}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

// TestCreateProject_DirKind_PathIsFile_422 验证 dir kind 拒绝指向文件（非目录）的路径。
func TestCreateProject_DirKind_PathIsFile_422(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "a-file")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"p","kind":"dir","path":` + quoteJSON(filePath) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

// TestCreateProject_RepoKind_NonRepoDirNotDowngraded 验证 kind=repo（含缺省）校验失败时
// MUST NOT 自动降级为 dir，直接 422。
func TestCreateProject_RepoKind_NonRepoDirNotDowngraded(t *testing.T) {
	notRepo := t.TempDir() // 空 dir，非 git repo
	// 显式 kind=repo。
	t.Run("explicit_repo", func(t *testing.T) {
		srv, _ := newServerWithRealStore(t)
		ts := httptest.NewServer(srv.mux)
		defer ts.Close()
		body := `{"name":"p","kind":"repo","path":` + quoteJSON(notRepo) + `}`
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (no downgrade to dir)", resp.StatusCode)
		}
		var eb errorBody
		json.NewDecoder(resp.Body).Decode(&eb)
		if eb.Error.Code != CodeInvalidInput {
			t.Errorf("code = %s, want invalid_input", eb.Error.Code)
		}
	})
	// 缺省 kind（应等价 repo）。
	t.Run("default_kind", func(t *testing.T) {
		srv, _ := newServerWithRealStore(t)
		ts := httptest.NewServer(srv.mux)
		defer ts.Close()
		body := `{"name":"p","path":` + quoteJSON(notRepo) + `}`
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (default repo, no downgrade)", resp.StatusCode)
		}
	})
}

// TestCreateProject_InvalidKind_422 验证非法 kind 值返回 422。
func TestCreateProject_InvalidKind_422(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"p","kind":"bogus","path":` + quoteJSON(dir) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	var eb errorBody
	json.NewDecoder(resp.Body).Decode(&eb)
	if eb.Error.Code != CodeInvalidInput {
		t.Errorf("code = %s, want invalid_input", eb.Error.Code)
	}
}

// TestCreateProject_DTOHasKind 验证 repo 注册响应 DTO 含 kind=repo。
func TestCreateProject_DTOHasKind(t *testing.T) {
	repo := newTestRepo(t)
	srv, _ := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	body := `{"name":"p","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var dto projectDTO
	json.NewDecoder(resp.Body).Decode(&dto)
	if dto.Kind != "repo" {
		t.Errorf("kind = %q, want 'repo'", dto.Kind)
	}
}

// TestListProjectBranches_SortedDedupedExcludesSymbolicHead 验证 repo 项目分支列表：
// 本地在前、远端在后，稳定排序、去重，排除 origin/HEAD 等 symbolic HEAD。
func TestListProjectBranches_SortedDedupedExcludesSymbolicHead(t *testing.T) {
	repo := newTestRepo(t)
	// 建本地分支 feature-x。
	runGitCmd(t, repo, "branch", "feature-x")
	// 添加远端并推送本地与远端分支，制造 origin/HEAD symbolic ref。
	remote := t.TempDir()
	runGitCmd(t, remote, "init", "--bare", "-q")
	runGitCmd(t, repo, "remote", "add", "origin", remote)
	runGitCmd(t, repo, "push", "-q", "origin", "main")
	runGitCmd(t, repo, "push", "-q", "origin", "feature-x")
	// 设置 origin/HEAD 指向 origin/main（产生 symbolic HEAD 远端条目）。
	runGitCmd(t, repo, "remote", "set-head", "origin", "main")

	srv, _ := newServerWithRealStore(t)
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

	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/"+created.ID+"/branches", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp2.StatusCode)
	}
	var branches []string
	if err := json.NewDecoder(resp2.Body).Decode(&branches); err != nil {
		t.Fatal(err)
	}
	// 期望本地在前（feature-x, main）、远端在后（origin/feature-x, origin/main），排除 origin/HEAD。
	want := []string{"feature-x", "main", "origin/feature-x", "origin/main"}
	if len(branches) != len(want) {
		t.Fatalf("branches = %v, want %v", branches, want)
	}
	for i, b := range branches {
		if b != want[i] {
			t.Errorf("branches[%d] = %q, want %q (full=%v)", i, b, want[i], branches)
			break
		}
	}
	// 显式断言不含 origin/HEAD。
	for _, b := range branches {
		if b == "origin/HEAD" {
			t.Errorf("branches contains origin/HEAD, must be excluded: %v", branches)
		}
	}
}

// TestListProjectBranches_DirKind_422 验证 dir 项目请求分支列表返回 422。
func TestListProjectBranches_DirKind_422(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	body := `{"name":"p","kind":"dir","path":` + quoteJSON(dir) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	var created projectDTO
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/"+created.ID+"/branches", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (dir kind has no branches)", resp2.StatusCode)
	}
	var eb errorBody
	json.NewDecoder(resp2.Body).Decode(&eb)
	if eb.Error.Code != CodeInvalidInput {
		t.Errorf("code = %s, want invalid_input", eb.Error.Code)
	}
}

// --- add-plain-dir-project D10：branches refresh API（tasks 2.7） ---

// setupBranchesTestRepo 构造 repo + bare remote + producer clone，注册为项目，返回 server/createdID/producer。
func setupBranchesTestRepo(t *testing.T) (ts *httptest.Server, createdID, repo, remote, producer string) {
	t.Helper()
	repo = newTestRepo(t)
	remote = t.TempDir()
	runGitCmd(t, remote, "init", "--bare", "-q")
	runGitCmd(t, repo, "remote", "add", "origin", remote)
	runGitCmd(t, repo, "push", "-q", "origin", "main")
	runGitCmd(t, repo, "remote", "set-head", "origin", "main")
	// producer clone 在 remote 侧建/删/推分支。
	producer = t.TempDir()
	runGitCmd(t, producer, "clone", "-q", remote, ".")
	runGitCmd(t, producer, "config", "user.email", "t@t.com")
	runGitCmd(t, producer, "config", "user.name", "tester")

	srv, _ := newServerWithRealStore(t)
	ts = httptest.NewServer(srv.mux)
	body := `{"name":"p","path":` + quoteJSON(repo) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	var c projectDTO
	json.NewDecoder(resp.Body).Decode(&c)
	resp.Body.Close()
	createdID = c.ID
	return ts, createdID, repo, remote, producer
}

// TestRefreshBranches_RemoteNewBranchAppears 验证远端新建分支后 refresh API 返回含新分支
// （而 GET 仍不含）。
func TestRefreshBranches_RemoteNewBranchAppears(t *testing.T) {
	ts, id, _, _, producer := setupBranchesTestRepo(t)
	defer ts.Close()

	// producer 在 remote 新建 feature-x 并 push。
	runGitCmd(t, producer, "checkout", "-q", "-b", "feature-x")
	if err := os.WriteFile(filepath.Join(producer, "fx.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, producer, "add", "fx.txt")
	runGitCmd(t, producer, "commit", "-qm", "fx")
	runGitCmd(t, producer, "push", "-q", "origin", "feature-x")

	// GET 仍不含（本地未 fetch）。
	respGET, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/"+id+"/branches", ""))
	if err != nil {
		t.Fatal(err)
	}
	var getBranches []string
	json.NewDecoder(respGET.Body).Decode(&getBranches)
	respGET.Body.Close()
	for _, b := range getBranches {
		if b == "origin/feature-x" {
			t.Fatalf("GET before refresh should not contain origin/feature-x: %v", getBranches)
		}
	}

	// refresh 返回含 origin/feature-x。
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/"+id+"/branches/refresh", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200", resp.StatusCode)
	}
	var branches []string
	if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range branches {
		if b == "origin/feature-x" {
			found = true
		}
	}
	if !found {
		t.Errorf("refresh branches = %v, want origin/feature-x", branches)
	}
}

// TestRefreshBranches_RemoteDeleteBranchPruned 验证远端删除分支后 refresh 移除列表条目。
func TestRefreshBranches_RemoteDeleteBranchPruned(t *testing.T) {
	ts, id, _, _, producer := setupBranchesTestRepo(t)
	defer ts.Close()
	// 先建 feature-x 并 push，再 refresh 拿到。
	runGitCmd(t, producer, "checkout", "-q", "-b", "feature-x")
	if err := os.WriteFile(filepath.Join(producer, "fx.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, producer, "add", "fx.txt")
	runGitCmd(t, producer, "commit", "-qm", "fx")
	runGitCmd(t, producer, "push", "-q", "origin", "feature-x")
	if resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/"+id+"/branches/refresh", "")); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("initial refresh: err=%v status=%d", err, resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// 删除远端分支。
	runGitCmd(t, producer, "push", "-q", "origin", "--delete", "feature-x")
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/"+id+"/branches/refresh", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200", resp.StatusCode)
	}
	var branches []string
	json.NewDecoder(resp.Body).Decode(&branches)
	for _, b := range branches {
		if b == "origin/feature-x" {
			t.Errorf("after prune refresh, branches = %v, want origin/feature-x removed", branches)
		}
	}
}

// TestRefreshBranches_DirKind_422 验证 dir 项目 refresh 返回 422。
func TestRefreshBranches_DirKind_422(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	body := `{"name":"p","kind":"dir","path":` + quoteJSON(dir) + `}`
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects", body))
	if err != nil {
		t.Fatal(err)
	}
	var created projectDTO
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	resp2, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/"+created.ID+"/branches/refresh", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (dir kind)", resp2.StatusCode)
	}
	var eb errorBody
	json.NewDecoder(resp2.Body).Decode(&eb)
	if eb.Error.Code != CodeInvalidInput {
		t.Errorf("code = %s, want invalid_input", eb.Error.Code)
	}
}

// TestRefreshBranches_UnknownPersistedKind_500 验证未知持久化 kind refresh 返回 500。
func TestRefreshBranches_UnknownPersistedKind_500(t *testing.T) {
	srv, db := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	// 直接落库一个未知 kind 的项目。
	ctx := context.Background()
	if err := db.CreateProject(ctx, "pbad", "p", "/x", "main", "weird"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/pbad/branches/refresh", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (unknown persisted kind)", resp.StatusCode)
	}
	var eb errorBody
	json.NewDecoder(resp.Body).Decode(&eb)
	if eb.Error.Code != CodeInternal {
		t.Errorf("code = %s, want internal", eb.Error.Code)
	}
}

// TestRefreshBranches_ProjectNotFound_404 验证项目不存在 refresh 返回 404。
func TestRefreshBranches_ProjectNotFound_404(t *testing.T) {
	srv, _ := newServerWithRealStore(t)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/projects/nope/branches/refresh", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
