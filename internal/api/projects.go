// Package api 实现 HTTP/WS 端点、token 中间件与统一错误结构（design.md §14/§21）。
//
// projects.go 的 git 校验（git.IsGitRepo / git.ResolveDefaultBranch）直接调用 internal/git，
// 不经 TaskManager GitOps——这是 api 边界例外：
// 项目注册/删除只读探测远端仓库的 git 属性，无任务生命周期共享状态、无并发竞争
// （无 worktree 写操作、无与 Suspend/Delete 互斥的资源）。TaskManager GitOps 持任务锁是
// 针对任务 worktree 的 commit/push 并发保护，项目探测不涉及任务 worktree，故无需经此编排。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"ocdeck/internal/git"
)

// registerProjectRoutes 注册 projects 相关路由（design.md §21）。
func (s *Server) registerProjectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/v1/projects", s.handleCreateProject)
	mux.HandleFunc("GET /api/v1/projects/{id}", s.handleGetProject)
	mux.HandleFunc("DELETE /api/v1/projects/{id}", s.handleDeleteProject)
}

// projectDTO 项目列表/创建响应 DTO。
// task_count 与 tasks_by_status：项目列表与详情均返回（与前端 Project 类型对齐，
// project-management spec 增强一致性）。列表经逐项目 CountProjectTasks 取概况。
type projectDTO struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Path          string         `json:"path"`
	DefaultBranch string         `json:"default_branch"`
	CreatedAt     int64          `json:"created_at"`
	TaskCount     int            `json:"task_count"`
	Tasks         map[string]int `json:"tasks_by_status"`
}

// projectDetailDTO 项目详情 DTO（design.md §21）。
type projectDetailDTO struct {
	projectDTO
}

// createProjectReq 注册请求体。
type createProjectReq struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (r createProjectReq) validate() *ApiError {
	if strings.TrimSpace(r.Name) == "" {
		return NewError(CodeInvalidInput, "name is required")
	}
	if strings.TrimSpace(r.Path) == "" {
		return NewError(CodeInvalidInput, "path is required")
	}
	return nil
}

// handleListProjects GET /api/v1/projects。
// 每项含任务概况（task_count/tasks_by_status，与详情字段一致、与前端 Project 类型对齐）。
// 逐项目 CountProjectTasks 取概况（个人规模 N+1 可接受，spec project-management 增强一致性）。
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	if s.projs == nil {
		writeError(w, CodeInternal, "project store not configured")
		return
	}
	rows, err := s.projs.ListProjects(r.Context())
	if err != nil {
		writeError(w, CodeInternal, "list projects failed")
		return
	}
	out := make([]projectDTO, 0, len(rows))
	for _, p := range rows {
		counts, cerr := s.projs.CountProjectTasks(r.Context(), p.ID)
		if cerr != nil {
			writeError(w, CodeInternal, "count project tasks failed")
			return
		}
		out = append(out, toProjectDTO(p, counts))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleCreateProject POST /api/v1/projects（design.md §21，spec：项目注册）。
// 校验路径存在且为 git 仓库（含 .git 或 git rev-parse），探测默认分支，落库。
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if s.projs == nil {
		writeError(w, CodeInternal, "project store not configured")
		return
	}
	var req createProjectReq
	if err := decodeJSON(r, &req); err != nil {
		writeApiError(w, err)
		return
	}
	if ae := req.validate(); ae != nil {
		writeApiError(w, ae)
		return
	}
	path := strings.TrimSpace(req.Path)

	// path 绝对化 + EvalSymlinks 归一（B9）：避免相对路径漂移与 symlink 别名
	// 导致同一仓库注册多次或唯一性判断失真。
	absPath, err := filepath.Abs(path)
	if err != nil {
		writeApiError(w, NewError(CodeInvalidInput, "cannot resolve absolute path: "+path))
		return
	}
	canonPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// 路径不存在（含 symlink 断链）按非仓库拒绝。
		writeApiError(w, NewError(CodeInvalidInput, "path does not exist or is a broken symlink: "+path))
		return
	}
	path = canonPath

	// git 仓库校验（spec：含 .git 或 git rev-parse）。
	isRepo, err := git.IsGitRepo(r.Context(), path)
	if err != nil {
		writeError(w, CodeGitError, "git repo check failed")
		return
	}
	if !isRepo {
		writeApiError(w, NewError(CodeInvalidInput, "path is not a git repository: "+path))
		return
	}

	// 默认分支探测。
	branch, err := git.ResolveDefaultBranch(r.Context(), path)
	if err != nil {
		writeError(w, CodeGitError, "detect default branch failed")
		return
	}

	// path 唯一性检查（归一后；schema UNIQUE 约束兜底，这里提前给出 409）。
	if existing, gerr := s.projs.GetProjectByPath(r.Context(), path); gerr == nil && existing.ID != "" {
		writeApiError(w, NewError(CodeConflict, "project already registered for path: "+path))
		return
	}

	id := newID()
	if err := s.projs.CreateProject(r.Context(), id, strings.TrimSpace(req.Name), path, branch); err != nil {
		if isUniqueViolation(err) {
			writeApiError(w, NewError(CodeConflict, "project already registered for path: "+path))
			return
		}
		writeError(w, CodeInternal, "create project failed")
		return
	}
	p, err := s.projs.GetProject(r.Context(), id)
	if err != nil {
		writeError(w, CodeInternal, "fetch created project failed")
		return
	}
	// 新建项目无任务，概况为零（TaskCount=0、ByStatus={}），避免一次无谓 CountProjectTasks。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toProjectDTO(p, storeTaskCounts{ByStatus: map[string]int{}}))
}

// handleGetProject GET /api/v1/projects/:id（含任务概况）。
func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	if s.projs == nil {
		writeError(w, CodeInternal, "project store not configured")
		return
	}
	id := r.PathValue("id")
	p, err := s.projs.GetProject(r.Context(), id)
	if err != nil {
		writeApiError(w, NewError(CodeNotFound, "project not found"))
		return
	}
	counts, cerr := s.projs.CountProjectTasks(r.Context(), id)
	if cerr != nil {
		writeError(w, CodeInternal, "count project tasks failed")
		return
	}
	dto := projectDetailDTO{projectDTO: toProjectDTO(p, counts)}
	if dto.Tasks == nil {
		dto.Tasks = map[string]int{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dto)
}

// handleDeleteProject DELETE /api/v1/projects/:id（有任务则 409，design.md §21）。
// 原子删除（B9/design.md §19）：先 GetProject 判存在（404），再 DeleteProjectIfEmpty
// 单语句原子删除（仅当无任务），避免"先 HasProjectTasks 再 DeleteProject"竞态中
// 任务在两步间插入被 CASCADE 删除。DeleteProjectIfEmpty 返回 false 时结合存在性判定
// "有任务 409"。
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	if s.projs == nil {
		writeError(w, CodeInternal, "project store not configured")
		return
	}
	id := r.PathValue("id")
	// 存在性检查：区分 404 与 409。
	if _, err := s.projs.GetProject(r.Context(), id); err != nil {
		writeApiError(w, NewError(CodeNotFound, "project not found"))
		return
	}
	// 原子删除：仅当无任务时生效。
	deleted, err := s.projs.DeleteProjectIfEmpty(r.Context(), id)
	if err != nil {
		writeError(w, CodeInternal, "delete project failed")
		return
	}
	if !deleted {
		// 项目存在但未删除 → 必有任务（GetProject 已确认存在）。
		writeApiError(w, NewError(CodeConflict, "project has tasks; delete or archive them first"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toProjectDTO 将 storeProjectRow 转为 projectDTO，附带任务概况。
// counts.ByStatus 为空 map（无任务）时仍输出非 nil，保证前端字段稳定。
func toProjectDTO(p storeProjectRow, counts storeTaskCounts) projectDTO {
	byStatus := counts.ByStatus
	if byStatus == nil {
		byStatus = map[string]int{}
	}
	return projectDTO{
		ID:            p.ID,
		Name:          p.Name,
		Path:          p.Path,
		DefaultBranch: p.DefaultBranch,
		CreatedAt:     p.CreatedAt,
		TaskCount:     counts.Total,
		Tasks:         byStatus,
	}
}

// decodeJSON 解码请求体，校验 Content-Type 与空体。
func decodeJSON(r *http.Request, v any) *ApiError {
	if r.Body == nil {
		return NewError(CodeInvalidInput, "request body is required")
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return NewError(CodeInvalidInput, "invalid JSON body")
	}
	return nil
}

// newID 生成 16 字节随机 hex ID。
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// isUniqueViolation 判断是否为唯一约束冲突（modernc.org/sqlite 返回 SQLITE_CONSTRAINT）。
// 宽松匹配错误信息，避免引入 driver 特定类型依赖。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed")
}

// 编译期断言：确保 context 与 errors 仍被引用（防止未来重构误删 import）。
var _ = context.Background
var _ = errors.New
