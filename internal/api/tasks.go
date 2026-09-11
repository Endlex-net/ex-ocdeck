package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/pty"
)

// TaskBackend 是 api 层调用的 TaskManager 能力（design.md §18 task 行 + §21 路由）。
// api handler 只做 DTO/HTTP 语义，不做编排。返回 application.TaskRow + error（api 做 DTO 转换）。
type TaskBackend interface {
	Create(ctx context.Context, projectID string, opts application.CreateTaskOptions) (application.TaskRow, error)
	Activate(ctx context.Context, taskID string) error
	Suspend(ctx context.Context, taskID string) error
	Archive(ctx context.Context, taskID string) error
	Restore(ctx context.Context, taskID string) error
	Delete(ctx context.Context, taskID string, mode application.DeleteMode, confirmDirty bool) error
	Retry(ctx context.Context, taskID string, confirmDirty bool) error
	ReopenAttach(ctx context.Context, taskID string) (application.TerminalID, error)
	CreateShell(ctx context.Context, taskID string) (application.TerminalID, error)
	CloseShell(ctx context.Context, terminalID application.TerminalID) error
	Get(ctx context.Context, taskID string) (application.TaskRow, error)
	List(ctx context.Context, projectID string) ([]application.TaskRow, error)
	ListTaskSessions(ctx context.Context, taskID string) ([]application.SessionRow, error)
	ListShells(taskID string) ([]application.TerminalID, error)
	ValidateShellTerminal(tid string) error
	AttachPty(sessionName string, cols, rows int) (*pty.Pty, error)
	// AgentStatus 返回任务 agent 运行态（idle/busy/retry/空串，design.md 2.8）。
	// 非 active 或查询失败返回空串（降级不阻塞详情返回）。
	AgentStatus(ctx context.Context, taskID string) string
	// AgentStatusSnapshot 读 agentStatus 内存快照（sse-active-sessions design D4）。
	// 不可用（无 runtime/连接代无效/零 owned）返回空串（omitempty 省略）。
	// 供 active sessions/SSE 与 /projects 任务摘要组装消费（projects-stream）；
	// 与 AgentStatus 实时探测语义并存、互不影响。
	AgentStatusSnapshot(taskID string) string
	// ListAllActiveTaskIDs 返回当前 active 任务 ID
	//（供全局配置保存后受影响任务提示，design.md §13）。
	ListAllActiveTaskIDs(ctx context.Context) ([]string, error)
	// ListActiveTaskOverview 聚合全部 active 任务的跨项目概览
	//（cross-project-active-sessions：GET /api/v1/tasks/active 读模型来源）。
	// 返回不含 agentStatus 的投影行；agentStatus 由 API 层组装读内存快照
	//（AgentStatusSnapshot，sse-active-sessions P2.2）填充到 DTO。
	ListActiveTaskOverview(ctx context.Context) ([]application.ActiveTaskOverviewRow, error)
	// Attention 返回任务注意力信号快照（design.md D6）。非 active/无 runtime 返回空快照。
	// 纯读聚合，不影响任务状态机。API 层据此透出 attention 字段（空数组非 null）。
	Attention(taskID string) (application.Attention, bool)
	// ListProjectTaskSummaries 聚合全部任务摘要（design.md D4 GET /projects tasks 摘要）。
	// 纯读聚合；store 失败返回错误（API 层 500）。active 摘要的 agentStatus 由 API 层
	// 组装读内存快照（AgentStatusSnapshot）填充。
	ListProjectTaskSummaries(ctx context.Context) ([]application.ProjectTaskSummary, error)

	// Git 状态/diff/commit/push 经 TaskManager GitOps（design.md §9/§21）。
	// 持任务锁与 Suspend/Delete 等生命周期操作互斥，避免 api 绕过 TaskManager 致
	// worktree 在 git 操作中被移除（P6 并发竞争修复）。DTO 直接复用 application 包类型
	//（GitDiffDTO 为单文件两侧版本内容八字段契约，codemirror-git-diff design D1）。
	// 错误语义经 *application.OpError 携带：not_found/conflict/invalid_input/invalid_state/
	// git_error/internal，由 mapTaskErr 统一映射 HTTP code/msg。
	GitStatus(ctx context.Context, taskID string) (application.GitStatusDTO, error)
	GitDiff(ctx context.Context, taskID, ref, path string, untracked bool) (application.GitDiffDTO, error)
	GitCommit(ctx context.Context, taskID, message string, paths []string) error
	GitPush(ctx context.Context, taskID string) error
	// RerunInit 手动重跑 init 脚本（design.md §8，tasks 3.6）。
	// 返回 claim 后的任务行（init_status=running），供 API 层 200+DTO。
	// application.OpError 映射：invalid_state → 422、conflict → 409。
	RerunInit(ctx context.Context, taskID string) (application.TaskRow, error)
	// ReadInitLog 读取 init 日志（design.md §7.4/§8）：inherit 警告节 + init.log 拼接，tail ≤64KB。
	// 任务不存在 → not_found；无日志文件返回空串非错误。
	ReadInitLog(ctx context.Context, taskID string) (string, error)
	// ReadPreDeleteLog 读取 pre-delete 日志（design.md §7.4/§8）：pre-delete.log，tail ≤64KB。
	// 任务不存在 → not_found；无日志文件返回空串非错误。
	ReadPreDeleteLog(ctx context.Context, taskID string) (string, error)

	// 用户主动操作上报（idle-reminder-user-activity D5）：发布 task.user_activity
	// 供 Notifier 取消该任务已武装的 idle 计时。无返回值：上报不产生 HTTP 可见副作用，
	// 不触碰任务行（不推进 updated_at、不产生 task.activity_changed）。
	// RecordShellUserActivity 的 tid 为 shell 终端 ID（shell tmux 会话名），
	// 由实现层解析归属任务；解析失败静默忽略。
	RecordUserActivity(ctx context.Context, taskID string)
	RecordShellUserActivity(ctx context.Context, tid string)
}

// registerTaskRoutes 注册 tasks 路由（design.md §21）。
func (s *Server) registerTaskRoutes(mux *http.ServeMux) {
	if s.tasks == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/projects/{id}/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/v1/projects/{id}/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}/stream", s.handleTaskStream)
	mux.HandleFunc("POST /api/v1/tasks/{id}/activate", s.handleTaskAction(s.tasks.Activate))
	mux.HandleFunc("POST /api/v1/tasks/{id}/suspend", s.handleTaskAction(s.tasks.Suspend))
	mux.HandleFunc("POST /api/v1/tasks/{id}/archive", s.handleTaskAction(s.tasks.Archive))
	mux.HandleFunc("POST /api/v1/tasks/{id}/restore", s.handleTaskAction(s.tasks.Restore))
	mux.HandleFunc("POST /api/v1/tasks/{id}/retry", s.handleRetryTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/rerun-init", s.handleRerunInit)
	// 用户活动上报（idle-reminder-user-activity D4）：空请求体，fire-and-forget。
	mux.HandleFunc("POST /api/v1/tasks/{id}/activity", s.handleTaskActivity)
	mux.HandleFunc("GET /api/v1/tasks/{id}/init-log", s.handleReadInitLog)
	mux.HandleFunc("GET /api/v1/tasks/{id}/pre-delete-log", s.handleReadPreDeleteLog)
	mux.HandleFunc("POST /api/v1/tasks/{id}/attach/reopen", s.handleReopenAttach)
	mux.HandleFunc("DELETE /api/v1/tasks/{id}", s.handleDeleteTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}/terminals", s.handleListTerminals)
	mux.HandleFunc("POST /api/v1/tasks/{id}/terminals", s.handleCreateTerminal)
	mux.HandleFunc("DELETE /api/v1/terminals/{tid}", s.handleCloseTerminal)
	// projects-stream task 命名：canonical /tasks/active；旧 /sessions/active 保留为兼容别名
	//（sse-active-sessions design Non-Goals：兼容与调试用途），同一 handler、同一响应。
	mux.HandleFunc("GET /api/v1/tasks/active", s.handleListActiveSessions)
	mux.HandleFunc("GET /api/v1/sessions/active", s.handleListActiveSessions)
	// SSE 流端点（sse-active-sessions P2.3，design D3）：与 /tasks/active 同享
	// Bearer 子 mux；需事件订阅端口注入（SetEventSubscriber 须先于 RebuildRoutes）。
	// projects-stream 改名 /tasks/active/stream（sse-active-sessions 引入未发布，不留旧路径别名）。
	if s.eventSubscriber != nil {
		mux.HandleFunc("GET /api/v1/tasks/active/stream", s.handleActiveSessionsStream)
	}
	// WS 端点由 registerWSRoutes 单独挂载（不走 api 子 mux，design.md §21）。
}

// createTaskReq 创建任务请求体。base_ref 为可选基线分支短名（add-plain-dir-project D10，
// 仅 repo 项目接受；dir 项目提供即 invalid_input，由 task 层校验）。
// Mode 为任务级运行模式（add-local-path-task-mode D6）：指针保留 presence 语义——
// null=缺省（repo→worktree；dir→local-path），非 null=显式提供。
// PermissionMode 为任务级权限模式（task-permission-mode D2）：指针保留 presence 语义——
// null=缺省（→ask），非 null=显式提供（trim 后须为三合法值）。
type createTaskReq struct {
	Name           string  `json:"name"`
	BaseRef        string  `json:"base_ref"`
	Mode           *string `json:"mode"`
	PermissionMode *string `json:"permission_mode"`
}

// 任务级运行模式合法值（add-local-path-task-mode D7）。与 internal/task 的 TaskMode
// 常量同源；api 层不经由 task 包，参照 projectKind* 本地常量先例。
const (
	taskModeWorktree  = "worktree"
	taskModeLocalPath = "local-path"
)

// 任务级权限模式合法值（task-permission-mode D2）。与 internal/task 的 PermissionMode
// 常量同源；api 层不经由 task 包，参照 taskMode* 本地常量先例。
const (
	permissionModeAsk        = "ask"
	permissionModeAllApprove = "all-approve"
	permissionModeAIAuto     = "ai-auto"
)

// validTaskModeForKind 校验 kind+mode 组合（task-lifecycle delta 决策表：合法组合仅
// (repo,worktree)/(repo,local-path)/(dir,local-path)）。未知 kind 或未知 mode 均为
// 持久化损坏，fail-closed（DTO 输出路径不得产出缺/坏 mode 的元素，D7）。
func validTaskModeForKind(kind, mode string) bool {
	switch kind {
	case projectKindRepo:
		return mode == taskModeWorktree || mode == taskModeLocalPath
	case projectKindDir:
		return mode == taskModeLocalPath
	default:
		return false
	}
}

// permissionModeForOutput 归一化输出权限模式（task-permission-mode D7）：空值输出 ask
// （存量防御，列 DEFAULT 之前不会出现，此处兜底）；未知持久化值 fail-closed 返回
// internal（DTO 输出路径不得产出坏值元素，同 validTaskModeForKind 哲学）。
func permissionModeForOutput(taskID, mode string) (string, *ApiError) {
	switch mode {
	case permissionModeAsk, permissionModeAllApprove, permissionModeAIAuto:
		return mode, nil
	case "":
		return permissionModeAsk, nil
	default:
		return "", NewError(CodeInternal, fmt.Sprintf("task %s: invalid permission_mode %q", taskID, mode))
	}
}

func (r createTaskReq) validate() *ApiError {
	// ② name 非空（校验顺序见 add-local-path-task-mode D6）：trim 判空，任何空白名称
	// 均先于 ③mode/④kind 报错；原名称透传 task 层，不在此改写。
	if strings.TrimSpace(r.Name) == "" {
		return NewError(CodeInvalidInput, "task name is required")
	}
	// ③ mode 值域（校验顺序见 add-local-path-task-mode D6）：显式提供时 trim 后为
	// 空串或未知值 → invalid_input；缺省（null）跳过。
	if r.Mode != nil {
		switch strings.TrimSpace(*r.Mode) {
		case taskModeWorktree, taskModeLocalPath:
		default:
			return NewError(CodeInvalidInput, "mode must be worktree or local-path")
		}
	}
	// ④ permission_mode 值域（task-permission-mode D2，校验顺序 name → mode →
	// permission_mode）：显式提供时 trim 后为空串或未知值 → invalid_input；缺省（null）跳过。
	if r.PermissionMode != nil {
		switch strings.TrimSpace(*r.PermissionMode) {
		case permissionModeAsk, permissionModeAllApprove, permissionModeAIAuto:
		default:
			return NewError(CodeInvalidInput, "permission_mode must be ask, all-approve or ai-auto")
		}
	}
	return nil
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
		dto, de := toTaskDTO(t, kind)
		if de != nil {
			// mode 非法为持久化损坏：fail-closed 500，不输出缺 mode 元素（D7）。
			writeApiError(w, de)
			return
		}
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
	// ⑤ 组合校验（add-local-path-task-mode D6）：dir 项目拒绝显式 mode（任意取值）。
	// ⑥ base_ref 与模式的组合校验在 task 层 createInPlace/createRepo 入口（与现状
	// base_ref 校验位置一致），API 层只透传。
	mode := ""
	if req.Mode != nil {
		if kind == projectKindDir {
			writeApiError(w, NewError(CodeInvalidInput, "dir project tasks must not accept mode"))
			return
		}
		mode = strings.TrimSpace(*req.Mode)
	}
	// permission_mode 透传（task-permission-mode D2）：值域已在 validate 校验，null/缺省
	// 传空串（task 层归一化为 ask）。
	permissionMode := ""
	if req.PermissionMode != nil {
		permissionMode = strings.TrimSpace(*req.PermissionMode)
	}
	t, err := s.tasks.Create(r.Context(), projectID, application.CreateTaskOptions{
		Name:           req.Name,
		BaseRef:        strings.TrimSpace(req.BaseRef),
		Mode:           mode,
		PermissionMode: permissionMode,
	})
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	// fail-closed（D7）：组装 DTO 并校验 kind/mode 后才提交 201；损坏行 → 500 标准
	// 错误信封，MUST NOT 先写 201 再失败。
	dto, de := toTaskDTO(t, kind)
	if de != nil {
		writeApiError(w, de)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(dto)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	t, err := s.tasks.Get(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	sessions, _ := s.tasks.ListTaskSessions(r.Context(), taskID)
	dto, ae := s.buildTaskDetailDTOBase(r.Context(), t, sessions)
	if ae != nil {
		writeApiError(w, ae)
		return
	}
	// 2.8：active 时经该任务 serve 实时查询 agentStatus；非 active/查询失败降级为空串。
	dto.AgentStatus = s.tasks.AgentStatus(r.Context(), taskID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dto)
}

// buildTaskDetailDTOBase 共享详情 DTO 纯映射（task-detail-stream D4）：
// requireProjectKind + toTaskDTO + toSessionDTOs + Attention。查询策略不进 helper。
func (s *Server) buildTaskDetailDTOBase(ctx context.Context, t application.TaskRow, sessions []application.SessionRow) (taskRowDTO, *ApiError) {
	kind, ae := s.requireProjectKind(ctx, t.ProjectID)
	if ae != nil {
		return taskRowDTO{}, ae
	}
	dto, de := toTaskDTO(t, kind)
	if de != nil {
		return taskRowDTO{}, de
	}
	dto.Sessions = toSessionDTOs(sessions)
	att, _ := s.tasks.Attention(t.ID)
	dto.Attention = toAttentionDTO(att)
	return dto, nil
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

// handleTaskActivity POST /api/v1/tasks/{id}/activity（idle-reminder-user-activity D4）：
// git 区域用户手势上报。空请求体（不解析 body）；先 Get 存在性校验——错误经既有
// mapTaskErr 映射返回且 MUST NOT 发布；成功调 RecordUserActivity（发布
// task.user_activity，不等待消费者）返回 204。任务存在但非 active 同样 204：
// 消费端无该任务状态/无已武装计时自然 no-op。
func (s *Server) handleTaskActivity(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if _, err := s.tasks.Get(r.Context(), taskID); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	s.tasks.RecordUserActivity(r.Context(), taskID)
	w.WriteHeader(http.StatusNoContent)
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
	mode := application.DeleteNormal
	if m := r.URL.Query().Get("mode"); m == "force" {
		mode = application.DeleteForce
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
// application.OpError 映射走 mapTaskErr（invalid_state → 422、conflict → 409）。
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
	dto, de := toTaskDTO(row, kind)
	if de != nil {
		writeApiError(w, de)
		return
	}
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
	tid := application.TerminalID(r.PathValue("tid"))
	if err := s.tasks.CloseShell(r.Context(), tid); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// taskRowDTO 任务详情 DTO（design.md §21 GET /tasks/:id）。
// project_kind ∈ repo | dir（add-plain-dir-project D6），由 handler 经 requireProjectKind
// fail-closed 解析：projs 未注入/项目不存在/未知 kind → 返回错误信封，不输出 DTO。
// mode 为任务级运行模式（add-local-path-task-mode D7）：必有字段（非 omitempty），
// 非法 kind/mode 组合 fail-closed 不输出（toTaskDTO 返回错误）。
// permission_mode 为任务级权限模式（task-permission-mode D7）：必有字段（非 omitempty），
// 空值输出 ask、未知持久化值 fail-closed 不输出（toTaskDTO 返回错误）。
// base_ref 为来源分支全限定 ref（workbench-base-ref-and-overflow D1）：必有字段（非
// omitempty），toTaskDTO 纯透传落库值；非 worktree 任务与历史空值 worktree 任务为空串，
// 空串为合法响应。
type taskRowDTO struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"project_id"`
	Name           string          `json:"name"`
	Branch         string          `json:"branch"`
	BaseRef        string          `json:"base_ref"`
	Status         string          `json:"status"`
	WorktreePath   string          `json:"worktree_path"`
	LastPort       int             `json:"last_port,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	Notice         json.RawMessage `json:"notice,omitempty"`
	DeleteMode     string          `json:"delete_mode,omitempty"`
	CreatedAt      int64           `json:"created_at"`
	UpdatedAt      int64           `json:"updated_at"`
	InitStatus     string          `json:"init_status"`
	InitError      string          `json:"init_error,omitempty"`
	ProjectKind    string          `json:"project_kind"`
	Mode           string          `json:"mode"`
	PermissionMode string          `json:"permission_mode"`
	Sessions       []sessionRowDTO `json:"sessions,omitempty"`
	AgentStatus    string          `json:"agentStatus,omitempty"`
	Attention      attentionDTO    `json:"attention"`
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

// toAttentionDTO 将 application.Attention 快照转为 DTO（空集合为非 nil 空数组，spec）。
func toAttentionDTO(att application.Attention) attentionDTO {
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
// 读模型（application.ActiveTaskOverviewRow）不含 agentStatus；组装
// （buildActiveSessionsSnapshot，P2.2 起与 SSE 共享）读内存快照填充。
// AgentStatus 快照不可用为空串，经 omitempty 省略（idle/busy/retry 三态）。
// Attention 纯读快照（design.md D6），空数组非 null。
// mode 为必有字段（add-local-path-task-mode D7）；组装遇非法 kind/mode 返回错误
// （REST → 500；SSE 初始组装 500、update 保持 dirty 重试）。
// permission_mode 为必有字段（task-permission-mode D7）：空值输出 ask、未知持久化值
// fail-closed，错误语义与 mode 一致。
type activeSessionDTO struct {
	TaskID         string       `json:"task_id"`
	ProjectID      string       `json:"project_id"`
	ProjectName    string       `json:"project_name"`
	Name           string       `json:"name"`
	Branch         string       `json:"branch"`
	WorktreePath   string       `json:"worktree_path"`
	Mode           string       `json:"mode"`
	PermissionMode string       `json:"permission_mode"`
	LastActiveAt   int64        `json:"last_active_at"`
	AgentStatus    string       `json:"agentStatus,omitempty"`
	Attention      attentionDTO `json:"attention"`
}

// toTaskDTO 任务详情 DTO 纯映射（design.md §21）。mode 为必有字段：非法 kind/mode
// 组合为持久化损坏，返回 internal ApiError fail-closed，调用方不得输出该 DTO（D7）。
// permission_mode 为必有字段（task-permission-mode D7）：空值输出 ask（存量防御）、
// 未知持久化值 fail-closed internal。base_ref 纯透传落库值（workbench-base-ref-and-overflow
// D1），不新增校验。
func toTaskDTO(t application.TaskRow, projectKind string) (taskRowDTO, *ApiError) {
	if !validTaskModeForKind(projectKind, t.Mode) {
		return taskRowDTO{}, NewError(CodeInternal, fmt.Sprintf("task %s: invalid mode %q", t.ID, t.Mode))
	}
	permissionMode, ae := permissionModeForOutput(t.ID, t.PermissionMode)
	if ae != nil {
		return taskRowDTO{}, ae
	}
	dto := taskRowDTO{
		ID: t.ID, ProjectID: t.ProjectID, Name: t.Name, Branch: t.Branch, BaseRef: t.BaseRef,
		Status:       t.Status,
		WorktreePath: t.WorktreePath, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		ProjectKind: projectKind, Mode: t.Mode, PermissionMode: permissionMode,
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
	return dto, nil
}

func toSessionDTOs(rows []application.SessionRow) []sessionRowDTO {
	out := make([]sessionRowDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, sessionRowDTO{SessionID: s.SessionID, LastSeenAt: s.LastSeenAt})
	}
	return out
}

// handleListActiveSessions GET /api/v1/tasks/active（cross-project-active-sessions D3/D4；
// projects-stream 起 canonical，旧 /sessions/active 为兼容别名）。
// 纯读聚合：经 buildActiveSessionsSnapshot 组装（overview + attention + agentStatus 内存
// 快照，P2.2 与 SSE 共享；原实时水合 worker 已移除）。store 失败 → 500，不进入组装；
// 空结果 → JSON `[]`（非 null）；agentStatus 快照不可用时空串经 omitempty 省略。
func (s *Server) handleListActiveSessions(w http.ResponseWriter, r *http.Request) {
	out, err := s.buildActiveSessionsSnapshot(r.Context())
	if err != nil {
		writeError(w, CodeInternal, "list active sessions failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// mapTaskErr 将 application.OpError 映射为 *ApiError（design.md §21）。
func mapTaskErr(err error) *ApiError {
	code := application.OpErrorCode(err)
	if code == "" {
		return NewError(CodeInternal, "internal error")
	}
	return NewError(ErrorCode(code), err.Error())
}
