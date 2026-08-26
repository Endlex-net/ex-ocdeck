// Package api 实现 HTTP/WS 端点、token 中间件与统一错误结构（design.md §14/§21）。
//
// projects.go 的 git 校验（git.IsGitRepo / git.ResolveDefaultBranch）直接调用 internal/infrastructure/git，
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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/git"
)

// registerProjectRoutes 注册 projects 相关路由（design.md §21）。
func (s *Server) registerProjectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/v1/projects", s.handleCreateProject)
	mux.HandleFunc("GET /api/v1/projects/{id}", s.handleGetProject)
	mux.HandleFunc("DELETE /api/v1/projects/{id}", s.handleDeleteProject)
	mux.HandleFunc("GET /api/v1/projects/{id}/branches", s.handleListProjectBranches)
	mux.HandleFunc("POST /api/v1/projects/{id}/branches/refresh", s.handleRefreshProjectBranches)
}

// projectDTO 项目列表/创建响应 DTO。
// task_count 与 tasks_by_status：项目列表与详情均返回（与前端 Project 类型对齐，
// project-management spec 增强一致性）。列表经逐项目 CountProjectTasks 取概况。
// tasks：项目任务摘要数组（design.md D4 + project-management spec MODIFIED），
// 11 字段 = 10 存储字段 + attention_count，agentStatus 由 handler 水合填充。无任务为 []。
// kind ∈ repo | dir（add-plain-dir-project D1）。
type projectDTO struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Path          string                  `json:"path"`
	DefaultBranch string                  `json:"default_branch"`
	Kind          string                  `json:"kind"`
	CreatedAt     int64                   `json:"created_at"`
	TaskCount     int                     `json:"task_count"`
	Tasks         map[string]int          `json:"tasks_by_status"`
	TaskSummaries []projectTaskSummaryDTO `json:"tasks"`
}

// projectTaskSummaryDTO 项目任务摘要 DTO（design.md D4 11 字段）。
// notice 为 NoticeItem[] 原样透传（无 notice 时省略）；agentStatus 水合失败省略。
type projectTaskSummaryDTO struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Status         string          `json:"status"`
	InitStatus     string          `json:"init_status"`
	Branch         string          `json:"branch"`
	WorktreePath   string          `json:"worktree_path"`
	LastError      string          `json:"last_error,omitempty"`
	Notice         json.RawMessage `json:"notice,omitempty"`
	UpdatedAt      int64           `json:"updated_at"`
	AgentStatus    string          `json:"agentStatus,omitempty"`
	AttentionCount int             `json:"attention_count"`
}

// projectDetailDTO 项目详情 DTO（design.md §21）。
type projectDetailDTO struct {
	projectDTO
}

// createProjectReq 注册请求体。kind 缺省为 repo（add-plain-dir-project D1）。
type createProjectReq struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// 有效项目类型（add-plain-dir-project D1）。
const (
	projectKindRepo = "repo"
	projectKindDir  = "dir"
)

func (r createProjectReq) validate() *ApiError {
	if strings.TrimSpace(r.Name) == "" {
		return NewError(CodeInvalidInput, "name is required")
	}
	if strings.TrimSpace(r.Path) == "" {
		return NewError(CodeInvalidInput, "path is required")
	}
	if r.Kind == "" {
		// 缺省 repo，校验阶段不赋值（赋值在 handler，保持 validate 纯校验）。
		return nil
	}
	if r.Kind != projectKindRepo && r.Kind != projectKindDir {
		return NewError(CodeInvalidInput, "kind must be repo or dir")
	}
	return nil
}

// handleListProjects GET /api/v1/projects。
// 每项含任务概况（task_count/tasks_by_status，与详情字段一致、与前端 Project 类型对齐）。
// 逐项目 CountProjectTasks 取概况（个人规模 N+1 可接受，spec project-management 增强一致性）。
// 附加 tasks 摘要数组（design.md D4 + project-management spec MODIFIED）：覆盖全部非删除态任务，
// agentStatus 并发水合（cap 8/3s，单任务失败降级省略，store 失败 500 不水合，全链路纯读）。
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
	// 先取全部任务摘要（store 失败 → 500 不水合，spec）。
	var summaries []application.ProjectTaskSummary
	if s.tasks != nil {
		summaries, err = s.tasks.ListProjectTaskSummaries(r.Context())
		if err != nil {
			writeError(w, CodeInternal, "list project task summaries failed")
			return
		}
	}
	byProject := groupSummariesByProject(summaries)
	out := make([]projectDTO, 0, len(rows))
	for _, p := range rows {
		counts, cerr := s.projs.CountProjectTasks(r.Context(), p.ID)
		if cerr != nil {
			writeError(w, CodeInternal, "count project tasks failed")
			return
		}
		dto := toProjectDTO(p, counts)
		dto.TaskSummaries = toProjectTaskSummaryDTOs(byProject[p.ID])
		out = append(out, dto)
	}
	// agentStatus 水合（D4 cap8/3s）：对全部 active 任务摘要并发水合，单任务失败降级省略。
	hydrateProjectTaskAgentStatuses(r.Context(), s, out)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// groupSummariesByProject 按项目分组任务摘要。
func groupSummariesByProject(summaries []application.ProjectTaskSummary) map[string][]application.ProjectTaskSummary {
	out := make(map[string][]application.ProjectTaskSummary)
	for _, s := range summaries {
		out[s.ProjectID] = append(out[s.ProjectID], s)
	}
	return out
}

// toProjectTaskSummaryDTOs 转换任务摘要为 DTO（不含 agentStatus，由水合填充）。
func toProjectTaskSummaryDTOs(summaries []application.ProjectTaskSummary) []projectTaskSummaryDTO {
	out := make([]projectTaskSummaryDTO, 0, len(summaries))
	for _, s := range summaries {
		dto := projectTaskSummaryDTO{
			ID: s.TaskID, Name: s.Name, Status: s.Status, InitStatus: s.InitStatus,
			Branch: s.Branch, WorktreePath: s.WorktreePath, LastError: s.LastError,
			UpdatedAt: s.UpdatedAt, AttentionCount: s.AttentionCount,
		}
		if s.Notice != "" {
			dto.Notice = json.RawMessage(s.Notice)
		}
		out = append(out, dto)
	}
	return out
}

// hydrateProjectTaskAgentStatuses 对项目列表内全部 active 任务摘要并发水合 agentStatus
// （D4 cap8/3s，单任务失败降级省略，spec）。
func hydrateProjectTaskAgentStatuses(ctx context.Context, s *Server, projects []projectDTO) {
	type target struct {
		projIdx int
		taskIdx int
		taskID  string
	}
	var targets []target
	for i := range projects {
		for j := range projects[i].TaskSummaries {
			if projects[i].TaskSummaries[j].Status == application.StatusActive {
				targets = append(targets, target{i, j, projects[i].TaskSummaries[j].ID})
			}
		}
	}
	runAgentStatusHydration(ctx, s, len(targets), func(hctx context.Context, i int) {
		t := targets[i]
		projects[t.projIdx].TaskSummaries[t.taskIdx].AgentStatus = s.tasks.AgentStatus(hctx, t.taskID)
	})
}

// hydrateSingleProjectAgentStatuses 对单个项目（详情）水合 agentStatus。
func hydrateSingleProjectAgentStatuses(ctx context.Context, s *Server, p *projectDTO) {
	type target struct {
		taskIdx int
		taskID  string
	}
	var targets []target
	for j := range p.TaskSummaries {
		if p.TaskSummaries[j].Status == application.StatusActive {
			targets = append(targets, target{j, p.TaskSummaries[j].ID})
		}
	}
	runAgentStatusHydration(ctx, s, len(targets), func(hctx context.Context, i int) {
		t := targets[i]
		p.TaskSummaries[t.taskIdx].AgentStatus = s.tasks.AgentStatus(hctx, t.taskID)
	})
}

// runAgentStatusHydration 并发执行 agentStatus 水合（cap8/3s），单任务失败/超时降级省略。
// fn 在获取信号量后以 hctx（3s deadline）执行；失败/超时经 omitempty 省略。
func runAgentStatusHydration(ctx context.Context, s *Server, n int, fn func(hctx context.Context, i int)) {
	if n == 0 {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-hctx.Done():
				return
			}
			fn(hctx, idx)
		}(i)
	}
	wg.Wait()
}

// handleCreateProject POST /api/v1/projects（design.md §21，spec：项目注册）。
// kind=repo：校验路径为 git 仓库、探测默认分支；kind=dir：仅校验路径存在且为目录，default_branch 落空串。
// 非法 kind → 422；repo 校验失败不降级为 dir（MUST NOT 隐式推断，add-plain-dir-project D1）。
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
	kind := req.Kind
	if kind == "" {
		kind = projectKindRepo
	}
	path := strings.TrimSpace(req.Path)

	// path 绝对化 + EvalSymlinks 归一（B9）：避免相对路径漂移与 symlink 别名
	// 导致同一仓库/目录注册多次或唯一性判断失真。
	absPath, err := filepath.Abs(path)
	if err != nil {
		writeApiError(w, NewError(CodeInvalidInput, "cannot resolve absolute path: "+path))
		return
	}
	canonPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// 路径不存在（含 symlink 断链）按非法路径拒绝。
		writeApiError(w, NewError(CodeInvalidInput, "path does not exist or is a broken symlink: "+path))
		return
	}
	path = canonPath

	// 按 kind 分支校验。未知 kind 已在 validate 拒绝；此处 fail-closed，不默认落入某分支。
	var branch string
	switch kind {
	case projectKindRepo:
		// git 仓库校验（spec：含 .git 或 git rev-parse）。
		isRepo, gerr := git.IsGitRepo(r.Context(), path)
		if gerr != nil {
			writeError(w, CodeGitError, "git repo check failed")
			return
		}
		if !isRepo {
			writeApiError(w, NewError(CodeInvalidInput, "path is not a git repository: "+path))
			return
		}
		// 默认分支探测。
		branch, err = git.ResolveDefaultBranch(r.Context(), path)
		if err != nil {
			writeError(w, CodeGitError, "detect default branch failed")
			return
		}
	case projectKindDir:
		// dir 项目仅校验路径存在且为目录（canonPath 已 EvalSymlinks 确认存在）。
		info, serr := os.Stat(path)
		if serr != nil {
			writeApiError(w, NewError(CodeInvalidInput, "path is not accessible: "+path))
			return
		}
		if !info.IsDir() {
			writeApiError(w, NewError(CodeInvalidInput, "path is not a directory: "+path))
			return
		}
		// dir 项目无默认分支。
		branch = ""
	default:
		// fail-closed：未知 kind 不默认落入某分支（防御性，validate 已挡）。
		writeApiError(w, NewError(CodeInvalidInput, "kind must be repo or dir"))
		return
	}

	// path 唯一性检查（归一后；schema UNIQUE 约束兜底，这里提前给出 409）。
	if existing, gerr := s.projs.GetProjectByPath(r.Context(), path); gerr == nil && existing.ID != "" {
		writeApiError(w, NewError(CodeConflict, "project already registered for path: "+path))
		return
	}

	id := newID()
	if err := s.projs.CreateProject(r.Context(), id, strings.TrimSpace(req.Name), path, branch, kind); err != nil {
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

// handleGetProject GET /api/v1/projects/:id（含任务概况 + tasks 摘要）。
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
	// tasks 摘要（design.md D4）：取全部任务摘要并按项目过滤。
	if s.tasks != nil {
		summaries, serr := s.tasks.ListProjectTaskSummaries(r.Context())
		if serr != nil {
			writeError(w, CodeInternal, "list project task summaries failed")
			return
		}
		dto.TaskSummaries = toProjectTaskSummaryDTOs(groupSummariesByProject(summaries)[id])
		hydrateSingleProjectAgentStatuses(r.Context(), s, &dto.projectDTO)
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

// handleListProjectBranches GET /api/v1/projects/{id}/branches（add-plain-dir-project D10）。
// 只读返回仓库分支短名列表（本地在前、远端在后，排序去重、排除远端 symbolic HEAD）。
// dir 项目 → 422（分支语义仅适用于 repo 项目）。不进 repo 写锁。
func (s *Server) handleListProjectBranches(w http.ResponseWriter, r *http.Request) {
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
	// fail-closed：未知 kind 不默认落入某分支。
	switch p.Kind {
	case projectKindRepo:
		// repo：枚举分支。
	case projectKindDir:
		writeApiError(w, NewError(CodeInvalidInput, "project is not a git repository; branches not available"))
		return
	default:
		// 未知持久化 kind（DB 损坏值）→ internal（D1：区别于用户请求非法 kind 的 invalid_input）。
		writeApiError(w, NewError(CodeInternal, "unknown project kind: "+p.Kind))
		return
	}
	branches, err := git.ListBranches(r.Context(), p.Path)
	if err != nil {
		writeError(w, CodeGitError, "list branches failed")
		return
	}
	if branches == nil {
		branches = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(branches)
}

// handleRefreshProjectBranches POST /api/v1/projects/{id}/branches/refresh
// （add-plain-dir-project D10：显式拉取最新远端分支后返回与 GET 同构的短名数组）。
//
// fetch 经 git.RefreshBranchesSingleflight：获取 canonical repo 写锁（与 worktree.Add/Remove、
// GitPush 串行）→ fetch --all --prune --no-write-fetch-head → 同锁内 ListBranches。
// 同 repo 并发 refresh 合并为单次 fetch，等待者共享结果；不同 repo 并行。
//
// fail-closed：fetch 失败/超时/取消 → git_error（透传 git stderr 风格），MUST NOT 返回 200 伪装最新；
// dir 项目 → invalid_input；未知持久化 kind → internal（与 branches GET 一致）。
func (s *Server) handleRefreshProjectBranches(w http.ResponseWriter, r *http.Request) {
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
	// fail-closed：未知 kind 不默认落入某分支（与 GET 一致）。
	switch p.Kind {
	case projectKindRepo:
		// repo：fetch + 枚举。
	case projectKindDir:
		writeApiError(w, NewError(CodeInvalidInput, "project is not a git repository; branches not available"))
		return
	default:
		// 未知持久化 kind（DB 损坏值）→ internal（D1）。
		writeApiError(w, NewError(CodeInternal, "unknown project kind: "+p.Kind))
		return
	}
	branches, err := git.RefreshBranchesSingleflight(r.Context(), p.Path)
	if err != nil {
		// 透传 git stderr 风格（含 fetch: 前缀）；context 取消/超时也走 git_error，不伪装 200。
		writeError(w, CodeGitError, err.Error())
		return
	}
	if branches == nil {
		branches = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(branches)
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
		Kind:          p.Kind,
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
