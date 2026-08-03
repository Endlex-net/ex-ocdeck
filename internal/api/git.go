package api

import (
	"encoding/json"
	"net/http"

	"ocdeck/internal/task"
)

// registerGitRoutes 注册任务 worktree 的 git 状态/diff/commit/push 路由（design.md §21、§9）。
// status/diff 为只读，MUST NOT 进 repo 写锁（git-operations spec）。
// commit/push 在 worktree 内执行本机 git，保 hooks/签名行为，错误原样透传 git stderr。
//
// 经 TaskManager GitOps（task.Manager.GitStatus/GitDiff/GitCommit/GitPush）调用，
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
// 复用 task.GitStatusDTO（JSON 字段与既有前端契约一致）。
type gitStatusResponse = task.GitStatusDTO

// gitDiffResponse diff 响应（unified diff 文本 + 截断标记）。
// 复用 task.GitDiffDTO（JSON 字段与既有前端契约一致）。
type gitDiffResponse = task.GitDiffDTO

// gitCommitReq commit 请求体。paths 为空表示提交全部改动。
type gitCommitReq struct {
	Message string   `json:"message"`
	Paths   []string `json:"paths"`
}

// handleGitStatus GET /api/v1/tasks/:id/git/status（design.md §9/§21）。
// 经 TaskManager.GitStatus 持任务锁，DTO 直接复用 task.GitStatusDTO。
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

// handleGitDiff GET /api/v1/tasks/:id/git/diff?ref=&path=（design.md §9/§21）。
// ref 可选（默认工作区 vs 索引/HEAD），path 可选（空=全仓 diff，受 DiffMaxFiles 限制）。
// 经 TaskManager.GitDiff 持任务锁，DTO 直接复用 task.GitDiffDTO。
func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	ref := r.URL.Query().Get("ref")
	path := r.URL.Query().Get("path")
	d, err := s.tasks.GitDiff(r.Context(), taskID, ref, path)
	if err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
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
