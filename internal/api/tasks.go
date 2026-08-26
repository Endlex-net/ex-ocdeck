package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"ocdeck/internal/pty"
	"ocdeck/internal/task"
)

// TaskBackend 是 api 层调用的 TaskManager 能力（design.md §18 task 行 + §21 路由）。
// api handler 只做 DTO/HTTP 语义，不做编排。返回 task.TaskRow + error（api 做 DTO 转换）。
type TaskBackend interface {
	Create(ctx context.Context, projectID, taskName, baseRef string) (task.TaskRow, error)
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
	// AgentStatusSnapshot 读 agentStatus 内存快照（sse-active-sessions design D4）。
	// 不可用（无 runtime/连接代无效/零 owned）返回空串（omitempty 省略）。
	// 供 active sessions/SSE 组装消费（P2）；与 AgentStatus 实时探测语义并存、互不影响。
	AgentStatusSnapshot(taskID string) string
	// ListAllActiveTaskIDs 返回当前 active 任务 ID
	//（供全局配置保存后受影响任务提示，design.md §13）。
	ListAllActiveTaskIDs(ctx context.Context) ([]string, error)
	// ListActiveTaskOverview 聚合全部 active 任务的跨项目概览
	//（cross-project-active-sessions：GET /api/v1/sessions/active 读模型来源）。
	// 返回不含 agentStatus 的投影行；agentStatus 由 handler 并发 hydration 填充。
	ListActiveTaskOverview(ctx context.Context) ([]task.ActiveTaskOverviewRow, error)
	// Attention 返回任务注意力信号快照（design.md D6）。非 active/无 runtime 返回空快照。
	// 纯读聚合，不影响任务状态机。API 层据此透出 attention 字段（空数组非 null）。
	Attention(taskID string) (task.Attention, bool)
	// ListProjectTaskSummaries 聚合全部任务摘要（design.md D4 GET /projects tasks 摘要）。
	// 纯读聚合；store 失败返回错误（API 层 500 不水合）。agentStatus hydration 在 API 层。
	ListProjectTaskSummaries(ctx context.Context) ([]task.ProjectTaskSummary, error)

	// Git 状态/diff/commit/push 经 TaskManager GitOps（design.md §9/§21）。
	// 持任务锁与 Suspend/Delete 等生命周期操作互斥，避免 api 绕过 TaskManager 致
	// worktree 在 git 操作中被移除（P6 并发竞争修复）。DTO 直接复用 task 包类型，
	// 前端 JSON 契约不变（task.GitStatusDTO/GitDiffDTO 字段与既有响应一致）。
	// 错误语义经 *task.OpError 携带：not_found/conflict/invalid_input/git_error，
	// 由 mapTaskErr 统一映射 HTTP code/msg。
	GitStatus(ctx context.Context, taskID string) (task.GitStatusDTO, error)
	GitDiff(ctx context.Context, taskID, ref, path string, untracked bool) (task.GitDiffDTO, error)
	GitCommit(ctx context.Context, taskID, message string, paths []string) error
	GitPush(ctx context.Context, taskID string) error
	// RerunInit 手动重跑 init 脚本（design.md §8，tasks 3.6）。
	// 返回 claim 后的任务行（init_status=running），供 API 层 200+DTO。
	// task.OpError 映射：invalid_state → 422、conflict → 409。
	RerunInit(ctx context.Context, taskID string) (task.TaskRow, error)
	// ReadInitLog 读取 init 日志（design.md §7.4/§8）：inherit 警告节 + init.log 拼接，tail ≤64KB。
	// 任务不存在 → not_found；无日志文件返回空串非错误。
	ReadInitLog(ctx context.Context, taskID string) (string, error)
	// ReadPreDeleteLog 读取 pre-delete 日志（design.md §7.4/§8）：pre-delete.log，tail ≤64KB。
	// 任务不存在 → not_found；无日志文件返回空串非错误。
	ReadPreDeleteLog(ctx context.Context, taskID string) (string, error)
}

// TaskRowDTO 任务详情 DTO（design.md §21 GET /tasks/:id）。
type TaskRowDTO struct {
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
	InitStatus   string          `json:"init_status"`
	InitError    string          `json:"init_error,omitempty"`
	Sessions     []SessionRowDTO `json:"sessions,omitempty"`
}

// SessionRowDTO 会话归属 DTO。
type SessionRowDTO struct {
	SessionID  string `json:"session_id"`
	LastSeenAt int64  `json:"last_seen_at"`
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
	mux.HandleFunc("POST /api/v1/tasks/{id}/rerun-init", s.handleRerunInit)
	mux.HandleFunc("GET /api/v1/tasks/{id}/init-log", s.handleReadInitLog)
	mux.HandleFunc("GET /api/v1/tasks/{id}/pre-delete-log", s.handleReadPreDeleteLog)
	mux.HandleFunc("POST /api/v1/tasks/{id}/attach/reopen", s.handleReopenAttach)
	mux.HandleFunc("DELETE /api/v1/tasks/{id}", s.handleDeleteTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}/terminals", s.handleListTerminals)
	mux.HandleFunc("POST /api/v1/tasks/{id}/terminals", s.handleCreateTerminal)
	mux.HandleFunc("DELETE /api/v1/terminals/{tid}", s.handleCloseTerminal)
	mux.HandleFunc("GET /api/v1/sessions/active", s.handleListActiveSessions)
	// WS 端点由 registerWSRoutes 单独挂载（不走 api 子 mux，design.md §21）。
}

// createTaskReq 创建任务请求体。base_ref 为可选基线分支短名（add-plain-dir-project D10，
// 仅 repo 项目接受；dir 项目提供即 invalid_input，由 task 层校验）。
type createTaskReq struct {
	Name    string `json:"name"`
	BaseRef string `json:"base_ref"`
}

func (r createTaskReq) validate() *ApiError {
	if r.Name == "" || r.Name == "  " {
		return NewError(CodeInvalidInput, "task name is required")
	}
	return nil
}

// projectKindFor 查询项目 kind（add-plain-dir-project D6）。projs 未注入返回空串
// （兼容测试 fixture 未注入 ProjectStore 的场景；生产路径 projs 必注入）。
// 查询失败或未知 kind 不在此吞错——需 fail-closed 的入口用 requireProjectKind。
func (s *Server) projectKindFor(ctx context.Context, projectID string) string {
	if s.projs == nil {
		return ""
	}
	p, err := s.projs.GetProject(ctx, projectID)
	if err != nil {
		return ""
	}
	return p.Kind
}

// requireProjectKind 解析项目 kind 并做 fail-closed 校验（add-plain-dir-project D1/D6）。
// projs 未注入、项目查询失败、未知持久化 kind → 返回 *ApiError（调用方不得执行后续副作用）：
//   - projs 未注入 → internal（生产配置错误）
//   - 项目不存在   → not_found
//   - 未知持久化 kind → internal（DB 损坏值，D1 区分于用户请求非法 kind 的 invalid_input）
//
// 仅 repo/dir 为合法持久化值。
func (s *Server) requireProjectKind(ctx context.Context, projectID string) (string, *ApiError) {
	if s.projs == nil {
		return "", NewError(CodeInternal, "project store not configured")
	}
	p, err := s.projs.GetProject(ctx, projectID)
	if err != nil {
		return "", NewError(CodeNotFound, "project not found")
	}
	if p.Kind != projectKindRepo && p.Kind != projectKindDir {
		return "", NewError(CodeInternal, "unknown project kind: "+p.Kind)
	}
	return p.Kind, nil
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	// fail-closed：项目查询失败/未知 kind MUST 返回错误，不得输出空 project_kind（D6）。
	kind, ae := s.requireProjectKind(r.Context(), projectID)
	if ae != nil {
		writeApiError(w, ae)
		return
	}
	tasks, err := s.tasks.List(r.Context(), projectID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	// 复用一次项目详情填充 project_kind，避免 N+1（D6）。
	out := make([]taskRowDTO, 0, len(tasks))
	for _, t := range tasks {
		dto := toTaskDTO(t, kind)
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
	// fail-closed：在调用 TaskBackend.Create 副作用前取得并校验项目 kind（D6）。
	// 查询失败/未知 kind → 不创建，返回明确错误（not_found/internal）。
	kind, ae := s.requireProjectKind(r.Context(), projectID)
	if ae != nil {
		writeApiError(w, ae)
		return
	}
	t, err := s.tasks.Create(r.Context(), projectID, req.Name, strings.TrimSpace(req.BaseRef))
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toTaskDTO(t, kind))
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	t, err := s.tasks.Get(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	// fail-closed：项目查询失败/未知 kind MUST 返回错误，不得输出空 project_kind（D6）。
	kind, ae := s.requireProjectKind(r.Context(), t.ProjectID)
	if ae != nil {
		writeApiError(w, ae)
		return
	}
	sessions, _ := s.tasks.ListTaskSessions(r.Context(), taskID)
	dto := toTaskDTO(t, kind)
	dto.Sessions = toSessionDTOs(sessions)
	// 2.8：active 时经该任务 serve 实时查询 agentStatus；非 active/查询失败降级为空串。
	dto.AgentStatus = s.tasks.AgentStatus(r.Context(), taskID)
	// D6 注意力信号快照透出（空数组非 null）。
	att, _ := s.tasks.Attention(taskID)
	dto.Attention = toAttentionDTO(att)
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

// handleRerunInit POST /api/v1/tasks/{id}/rerun-init（design.md §8）。
// 独立 handler（非 handleTaskAction——既有 helper 返回 204）；成功 200 + 任务 DTO。
// task.OpError 映射走 mapTaskErr（invalid_state → 422、conflict → 409）。
//
// fail-closed（D6）：RerunInit 会在 claim/执行脚本产生副作用；project_kind MUST 在副作用前
// 取得。预取（Get + requireProjectKind）失败 MUST NOT 调用 RerunInit，直接返回错误
// （not_found/internal），避免"脚本已跑但 API 500"的部分成功窗口。
func (s *Server) handleRerunInit(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	existing, gerr := s.tasks.Get(r.Context(), taskID)
	if gerr != nil {
		writeApiError(w, mapTaskErr(gerr))
		return
	}
	kind, ae := s.requireProjectKind(r.Context(), existing.ProjectID)
	if ae != nil {
		writeApiError(w, ae)
		return
	}
	row, err := s.tasks.RerunInit(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	dto := toTaskDTO(row, kind)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(dto)
}

// handleReadInitLog GET /api/v1/tasks/{id}/init-log（design.md §7.4/§8）。
// text/plain；任务不存在 → not_found；无日志文件 → 200 空 body；tail ≤64KB。
func (s *Server) handleReadInitLog(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	content, err := s.tasks.ReadInitLog(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(content))
}

// handleReadPreDeleteLog GET /api/v1/tasks/{id}/pre-delete-log（design.md §7.4/§8）。
// text/plain；任务不存在 → not_found；无日志文件 → 200 空 body；tail ≤64KB。
func (s *Server) handleReadPreDeleteLog(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	content, err := s.tasks.ReadPreDeleteLog(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(content))
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
// project_kind ∈ repo | dir（add-plain-dir-project D6），由 handler 从项目详情填充；
// projs 未注入时为空串（API 层降级，不阻塞详情返回）。
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
	InitStatus   string          `json:"init_status"`
	InitError    string          `json:"init_error,omitempty"`
	ProjectKind  string          `json:"project_kind"`
	Sessions     []sessionRowDTO `json:"sessions,omitempty"`
	AgentStatus  string          `json:"agentStatus,omitempty"`
	Attention    attentionDTO    `json:"attention"`
}

type sessionRowDTO struct {
	SessionID  string `json:"session_id"`
	LastSeenAt int64  `json:"last_seen_at"`
}

// attentionDTO 注意力信号透出 DTO（design.md D6）。空数组非 null；unsupported 透出空数组。
type attentionDTO struct {
	Permissions []permissionDTO `json:"permissions"`
	Questions   []questionDTO   `json:"questions"`
}

type permissionDTO struct {
	ID         string   `json:"id"`
	Permission string   `json:"permission"`
	Patterns   []string `json:"patterns"`
	Since      int64    `json:"since"`
}

type questionDTO struct {
	ID        string            `json:"id"`
	Questions []questionItemDTO `json:"questions"`
	Since     int64             `json:"since"`
}

type questionItemDTO struct {
	Header   string `json:"header"`
	Question string `json:"question"`
}

// toAttentionDTO 将 task.Attention 快照转为 DTO（空集合为非 nil 空数组，spec）。
func toAttentionDTO(att task.Attention) attentionDTO {
	perms := make([]permissionDTO, 0, len(att.Permissions))
	for _, p := range att.Permissions {
		perms = append(perms, permissionDTO{
			ID: p.ID, Permission: p.Permission, Patterns: p.Patterns, Since: p.Since,
		})
	}
	quests := make([]questionDTO, 0, len(att.Questions))
	for _, q := range att.Questions {
		items := make([]questionItemDTO, 0, len(q.Questions))
		for _, qi := range q.Questions {
			items = append(items, questionItemDTO{Header: qi.Header, Question: qi.Question})
		}
		quests = append(quests, questionDTO{
			ID: q.ID, Questions: items, Since: q.Since,
		})
	}
	return attentionDTO{Permissions: perms, Questions: quests}
}

// activeSessionDTO 跨项目 active 任务概览 DTO（cross-project-active-sessions D3/D4）。
// 读模型（task.ActiveTaskOverviewRow）不含 agentStatus；handler hydration worker 并发填充。
// AgentStatus 失败/超时为空串，经 omitempty 省略（idle/busy/retry 三态）。
// Attention 纯读快照（design.md D6），空数组非 null。
type activeSessionDTO struct {
	TaskID       string       `json:"task_id"`
	ProjectID    string       `json:"project_id"`
	ProjectName  string       `json:"project_name"`
	Name         string       `json:"name"`
	Branch       string       `json:"branch"`
	WorktreePath string       `json:"worktree_path"`
	LastActiveAt int64        `json:"last_active_at"`
	AgentStatus  string       `json:"agentStatus,omitempty"`
	Attention    attentionDTO `json:"attention"`
}

func toTaskDTO(t task.TaskRow, projectKind string) taskRowDTO {
	dto := taskRowDTO{
		ID: t.ID, ProjectID: t.ProjectID, Name: t.Name, Branch: t.Branch, Status: t.Status,
		WorktreePath: t.WorktreePath, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		ProjectKind: projectKind,
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
	// init_status 始终序列化（none 时前端用于判断徽标/入口），init_error 仅 failed 时有值。
	dto.InitStatus = t.InitStatus
	if t.InitError.Valid {
		dto.InitError = t.InitError.String
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

// handleListActiveSessions GET /api/v1/sessions/active（cross-project-active-sessions D3/D4）。
// 纯读聚合：store 查询 → DTO 转换 → 并发 hydration agentStatus（per-request cap 8、3s budget）。
// store 失败 → 500，不进入 hydration；空结果 → JSON `[]`（非 null）；agentStatus 失败/超时省略字段。
func (s *Server) handleListActiveSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.tasks.ListActiveTaskOverview(r.Context())
	if err != nil {
		writeError(w, CodeInternal, "list active sessions failed")
		return
	}
	out := make([]activeSessionDTO, 0, len(rows))
	for _, row := range rows {
		dto := activeSessionDTO{
			TaskID: row.ID, ProjectID: row.ProjectID, ProjectName: row.ProjectName,
			Name: row.Name, Branch: row.Branch, WorktreePath: row.WorktreePath,
			LastActiveAt: row.LastActiveAt,
		}
		// D6 注意力信号快照透出（空数组非 null）。
		att, _ := s.tasks.Attention(row.ID)
		dto.Attention = toAttentionDTO(att)
		out = append(out, dto)
	}
	// Hydration worker（D4）：per-request 信号量 cap 8、3s budget；每 goroutine 仅写自己的 out[i]。
	hctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-hctx.Done():
				return
			}
			out[i].AgentStatus = s.tasks.AgentStatus(hctx, out[i].TaskID)
		}(i)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// mapTaskErr 将 task.OpError 映射为 *ApiError（design.md §21）。
func mapTaskErr(err error) *ApiError {
	code := task.OpErrorCode(err)
	if code == "" {
		return NewError(CodeInternal, "internal error")
	}
	return NewError(ErrorCode(code), err.Error())
}
