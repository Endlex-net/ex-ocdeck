package api

import (
	"encoding/json"
	"net/http"

	"ocdeck/internal/application"
)

// registerGitRoutes 注册任务 worktree 的 git 状态/diff/commit/push 路由（design.md §21、§9）。
// status/diff 为只读，MUST NOT 进 repo 写锁（git-operations spec）。
// commit/push 在 worktree 内执行本机 git，保 hooks/签名行为，错误原样透传 git stderr。
//
// 经 TaskManager GitOps（task 层 Manager facade 的 GitStatus/GitDiff/GitCommit/GitPush）调用，
// 持任务锁与 Suspend/Delete 等生命周期操作互斥，避免 api 绕过 TaskManager 致
// worktree 在 git 操作中被移除（P6 并发竞争修复）。
func (s *Server) registerGitRoutes(mux *http.ServeMux) {
	if s.tasks == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/tasks/{id}/git/status", s.handleGitStatus)
	mux.HandleFunc("GET /api/v1/tasks/{id}/git/diff", s.handleGitDiff)
	mux.HandleFunc("POST /api/v1/tasks/{id}/git/commit", s.handleGitCommit)
	mux.HandleFunc("POST /api/v1/tasks/{id}/git/push", s.handleGitPush)
}

// gitStatusResponse status 响应（含当前分支）。
// 复用 application.GitStatusDTO（JSON 字段与既有前端契约一致）。
type gitStatusResponse = application.GitStatusDTO

// gitDiffResponse diff 响应（unified diff 文本 + 截断标记）。
// 复用 application.GitDiffDTO（JSON 字段与既有前端契约一致）。
type gitDiffResponse = application.GitDiffDTO

// gitCommitReq commit 请求体。paths 为空表示提交全部改动。
type gitCommitReq struct {
	Message string   `json:"message"`
	Paths   []string `json:"paths"`
}

// handleGitStatus GET /api/v1/tasks/:id/git/status（design.md §9/§21）。
// 经 TaskManager.GitStatus 持任务锁，DTO 直接复用 application.GitStatusDTO。
func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	st, err := s.tasks.GitStatus(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// handleGitDiff GET /api/v1/tasks/:id/git/diff?ref=&path=&untracked=（design.md §9/§21、fix-git-diff）。
// ref 可选（默认工作区 vs 索引/HEAD），path 可选（空=全仓 diff，受 DiffMaxFiles 限制）。
// untracked 查询参数值域：absent / "0" / "1"，其他值 → invalid_input。
// untracked=1 时透传 untracked=true，调用方声明模式，服务端不二次探测。
// 经 TaskManager.GitDiff 持任务锁，DTO 直接复用 application.GitDiffDTO。
func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	ref := r.URL.Query().Get("ref")
	path := r.URL.Query().Get("path")
	untracked, ae := parseUntrackedParam(r)
	if ae != nil {
		writeApiError(w, ae)
		return
	}
	d, err := s.tasks.GitDiff(r.Context(), taskID, ref, path, untracked)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

// parseUntrackedParam 解析 untracked 查询参数。值域：absent / "0" / "1"。
// absent → false；present 且单值 "0"/"1" → 对应 bool；空值、其他值、重复参数 → invalid_input。
// 用 r.URL.Query()["untracked"] 区分 absent 与显式空值（Get 无法区分 ?untracked= 与不存在）。
func parseUntrackedParam(r *http.Request) (bool, *ApiError) {
	values, present := r.URL.Query()["untracked"]
	if !present {
		return false, nil // absent → false
	}
	if len(values) != 1 {
		return false, NewError(CodeInvalidInput, "untracked must be 0 or 1")
	}
	switch values[0] {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, NewError(CodeInvalidInput, "untracked must be 0 or 1")
	}
}

// handleGitCommit POST /api/v1/tasks/:id/git/commit（design.md §9/§21）。
// body: {"message","paths"}；空 paths=全部。git 错误原样透传 stderr。
// 经 TaskManager.GitCommit 持任务锁；message 空→invalid_input（422）。
func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var req gitCommitReq
	if err := decodeJSON(r, &req); err != nil {
		writeApiError(w, err)
		return
	}
	if err := s.tasks.GitCommit(r.Context(), taskID, req.Message, req.Paths); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleGitPush POST /api/v1/tasks/:id/git/push（design.md §9/§21）。
// 执行 git push -u origin <branch>，MUST NOT force-push。git 错误原样透传 stderr。
// 经 TaskManager.GitPush 持任务锁。
func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if err := s.tasks.GitPush(r.Context(), taskID); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}
