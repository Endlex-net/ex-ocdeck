package api

import (
	"context"
	"encoding/json"
	"net/http"

	"ocdeck/internal/pty"
	"ocdeck/internal/task"
)

// TaskBackend 是 api 层调用的 TaskManager 能力（design.md §18 task 行 + §21 路由）。
// api handler 只做 DTO/HTTP 语义，不做编排。返回 task.TaskRow + error（api 做 DTO 转换）。
type TaskBackend interface {
	Create(ctx context.Context, projectID, taskName string) (task.TaskRow, error)
	Activate(ctx context.Context, taskID string) error
	Suspend(ctx context.Context, taskID string) error
	Archive(ctx context.Context, taskID string) error
	Restore(ctx context.Context, taskID string) error
	Delete(ctx context.Context, taskID string, mode task.DeleteMode, confirmDirty bool) error
	Retry(ctx context.Context, taskID string, confirmDirty bool) error
	ReopenAttach(ctx context.Context, taskID string) (task.TerminalID, error)
	CreateShell(ctx context.Context, taskID string) (task.TerminalID, error)
	CloseShell(ctx context.Context, terminalID task.TerminalID) error
	Get(ctx context.Context, taskID string) (task.TaskRow, error)
	List(ctx context.Context, projectID string) ([]task.TaskRow, error)
	ListTaskSessions(ctx context.Context, taskID string) ([]task.SessionRow, error)
	ListShells(taskID string) ([]task.TerminalID, error)
	ValidateShellTerminal(tid string) error
	AttachPty(sessionName string, cols, rows int) (*pty.Pty, error)
	// AgentStatus 返回任务 agent 运行态（idle/busy/retry/空串，design.md 2.8）。
	// 非 active 或查询失败返回空串（降级不阻塞详情返回）。
	AgentStatus(ctx context.Context, taskID string) string
	// ListAllActiveTaskIDs 返回当前 active 任务 ID
	//（供全局配置保存后受影响任务提示，design.md §13）。
	ListAllActiveTaskIDs(ctx context.Context) ([]string, error)

	// Git 状态/diff/commit/push 经 TaskManager GitOps（design.md §9/§21）。
	// 持任务锁与 Suspend/Delete 等生命周期操作互斥，避免 api 绕过 TaskManager 致
	// worktree 在 git 操作中被移除（P6 并发竞争修复）。DTO 直接复用 task 包类型，
	// 前端 JSON 契约不变（task.GitStatusDTO/GitDiffDTO 字段与既有响应一致）。
	// 错误语义经 *task.OpError 携带：not_found/conflict/invalid_input/git_error，
	// 由 mapTaskErr 统一映射 HTTP code/msg。
	GitStatus(ctx context.Context, taskID string) (task.GitStatusDTO, error)
	GitDiff(ctx context.Context, taskID, ref, path string) (task.GitDiffDTO, error)
	GitCommit(ctx context.Context, taskID, message string, paths []string) error
	GitPush(ctx context.Context, taskID string) error
}

// TaskRowDTO 任务详情 DTO（design.md §21 GET /tasks/:id）。
type TaskRowDTO struct {
	ID           string            `json:"id"`
	ProjectID    string            `json:"project_id"`
	Name         string            `json:"name"`
	Branch       string            `json:"branch"`
	Status       string            `json:"status"`
	WorktreePath string            `json:"worktree_path"`
	LastPort     int               `json:"last_port,omitempty"`
	LastError    string            `json:"last_error,omitempty"`
	Notice       json.RawMessage    `json:"notice,omitempty"`
	DeleteMode   string            `json:"delete_mode,omitempty"`
	CreatedAt    int64             `json:"created_at"`
	UpdatedAt    int64             `json:"updated_at"`
	Sessions     []SessionRowDTO   `json:"sessions,omitempty"`
}

// SessionRowDTO 会话归属 DTO。
type SessionRowDTO struct {
	SessionID string `json:"session_id"`
	LastSeenAt int64 `json:"last_seen_at"`
}

// registerTaskRoutes 注册 tasks 路由（design.md §21）。
func (s *Server) registerTaskRoutes(mux *http.ServeMux) {
	if s.tasks == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/projects/{id}/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/v1/projects/{id}/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/activate", s.handleTaskAction(s.tasks.Activate))
	mux.HandleFunc("POST /api/v1/tasks/{id}/suspend", s.handleTaskAction(s.tasks.Suspend))
	mux.HandleFunc("POST /api/v1/tasks/{id}/archive", s.handleTaskAction(s.tasks.Archive))
	mux.HandleFunc("POST /api/v1/tasks/{id}/restore", s.handleTaskAction(s.tasks.Restore))
	mux.HandleFunc("POST /api/v1/tasks/{id}/retry", s.handleRetryTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/attach/reopen", s.handleReopenAttach)
	mux.HandleFunc("DELETE /api/v1/tasks/{id}", s.handleDeleteTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}/terminals", s.handleListTerminals)
	mux.HandleFunc("POST /api/v1/tasks/{id}/terminals", s.handleCreateTerminal)
	mux.HandleFunc("DELETE /api/v1/terminals/{tid}", s.handleCloseTerminal)
	// WS 端点由 registerWSRoutes 单独挂载（不走 api 子 mux，design.md §21）。
}

// createTaskReq 创建任务请求体。
type createTaskReq struct {
	Name string `json:"name"`
}

func (r createTaskReq) validate() *ApiError {
	if r.Name == "" || r.Name == "  " {
		return NewError(CodeInvalidInput, "task name is required")
	}
	return nil
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	tasks, err := s.tasks.List(r.Context(), projectID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	out := make([]taskRowDTO, 0, len(tasks))
	for _, t := range tasks {
		dto := toTaskDTO(t)
		dto.AgentStatus = s.tasks.AgentStatus(r.Context(), t.ID)
		out = append(out, dto)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeApiError(w, NewError(CodeInvalidInput, "invalid JSON body"))
		return
	}
	if ae := req.validate(); ae != nil {
		writeApiError(w, ae)
		return
	}
	t, err := s.tasks.Create(r.Context(), projectID, req.Name)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toTaskDTO(t))
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	t, err := s.tasks.Get(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	sessions, _ := s.tasks.ListTaskSessions(r.Context(), taskID)
	dto := toTaskDTO(t)
	dto.Sessions = toSessionDTOs(sessions)
	// 2.8：active 时经该任务 serve 实时查询 agentStatus；非 active/查询失败降级为空串。
	dto.AgentStatus = s.tasks.AgentStatus(r.Context(), taskID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dto)
}

// handleTaskAction 通用状态机操作 handler（无 body）。
func (s *Server) handleTaskAction(fn func(ctx context.Context, taskID string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := r.PathValue("id")
		if err := fn(r.Context(), taskID); err != nil {
			writeApiError(w, mapTaskErr(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleReopenAttach(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	tid, err := s.tasks.ReopenAttach(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"terminal_id": string(tid)})
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	mode := task.DeleteNormal
	if m := r.URL.Query().Get("mode"); m == "force" {
		mode = task.DeleteForce
	} else if m != "" && m != "normal" {
		writeApiError(w, NewError(CodeInvalidInput, "mode must be normal or force"))
		return
	}
	// confirmDirty 查询参数（design.md §19/§21：DELETE /tasks/:id?mode=normal|force&confirmDirty=true）。
	// 参数名为 camelCase confirmDirty（非 snake_case confirm_dirty）。
	confirmDirty := r.URL.Query().Get("confirmDirty") == "true"
	if err := s.tasks.Delete(r.Context(), taskID, mode, confirmDirty); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRetryTask 重试 handler（design.md §18/§19）。
// confirmDirty 查询参数透传给 TaskManager.Retry（B1：删除重试的 dirty 门禁与首次 Delete 一致，
// 非空 dirty 需用户显式确认；参数命名与 DELETE 一致 confirmDirty=true）。
func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	confirmDirty := r.URL.Query().Get("confirmDirty") == "true"
	if err := s.tasks.Retry(r.Context(), taskID, confirmDirty); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListTerminals(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	shells, err := s.tasks.ListShells(taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	out := make([]map[string]string, 0, len(shells))
	for _, tid := range shells {
		out = append(out, map[string]string{"terminal_id": string(tid)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleCreateTerminal(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	tid, err := s.tasks.CreateShell(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"terminal_id": string(tid)})
}

func (s *Server) handleCloseTerminal(w http.ResponseWriter, r *http.Request) {
	tid := task.TerminalID(r.PathValue("tid"))
	if err := s.tasks.CloseShell(r.Context(), tid); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// taskRowDTO 任务详情 DTO（design.md §21 GET /tasks/:id）。
type taskRowDTO struct {
	ID           string          `json:"id"`
	ProjectID    string          `json:"project_id"`
	Name         string          `json:"name"`
	Branch       string          `json:"branch"`
	Status       string          `json:"status"`
	WorktreePath string          `json:"worktree_path"`
	LastPort     int             `json:"last_port,omitempty"`
	LastError    string          `json:"last_error,omitempty"`
	Notice       json.RawMessage `json:"notice,omitempty"`
	DeleteMode   string          `json:"delete_mode,omitempty"`
	CreatedAt    int64           `json:"created_at"`
	UpdatedAt    int64           `json:"updated_at"`
	Sessions     []sessionRowDTO `json:"sessions,omitempty"`
	AgentStatus  string          `json:"agentStatus,omitempty"`
}

type sessionRowDTO struct {
	SessionID  string `json:"session_id"`
	LastSeenAt int64  `json:"last_seen_at"`
}

func toTaskDTO(t task.TaskRow) taskRowDTO {
	dto := taskRowDTO{
		ID: t.ID, ProjectID: t.ProjectID, Name: t.Name, Branch: t.Branch, Status: t.Status,
		WorktreePath: t.WorktreePath, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
	if t.LastPort.Valid {
		dto.LastPort = int(t.LastPort.Int64)
	}
	if t.LastError.Valid {
		dto.LastError = t.LastError.String
	}
	if t.Notice.Valid && t.Notice.String != "" {
		dto.Notice = json.RawMessage(t.Notice.String)
	}
	if t.DeleteMode.Valid {
		dto.DeleteMode = t.DeleteMode.String
	}
	return dto
}

func toSessionDTOs(rows []task.SessionRow) []sessionRowDTO {
	out := make([]sessionRowDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, sessionRowDTO{SessionID: s.SessionID, LastSeenAt: s.LastSeenAt})
	}
	return out
}

// mapTaskErr 将 task.OpError 映射为 *ApiError（design.md §21）。
func mapTaskErr(err error) *ApiError {
	code := task.OpErrorCode(err)
	if code == "" {
		return NewError(CodeInternal, "internal error")
	}
	return NewError(ErrorCode(code), err.Error())
}