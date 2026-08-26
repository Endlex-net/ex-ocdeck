package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// EnvStore 提供 project/task/global 级 env CRUD 能力（design.md §21、env-management spec）。
// 复用 internal/infrastructure/store 既有 Queries 方法签名，经 adapter 解耦 store 包结构。
type EnvStore interface {
	// 项目级
	ListProjectEnvVars(ctx context.Context, projectID string) ([]envVarRow, error)
	SetProjectEnvVar(ctx context.Context, projectID, key, value string) error
	DeleteProjectEnvVar(ctx context.Context, projectID, key string) error
	// 任务级
	ListTaskEnvVars(ctx context.Context, taskID string) ([]envVarRow, error)
	SetTaskEnvVar(ctx context.Context, taskID, key, value string) error
	DeleteTaskEnvVar(ctx context.Context, taskID, key string) error
	// 全局级（design.md §2/§8/§21：mode ∈ follow_host | manual）
	ListGlobalEnvVars(ctx context.Context) ([]globalEnvVarRow, error)
	SetGlobalEnvVar(ctx context.Context, key, mode, value string) error
	DeleteGlobalEnvVar(ctx context.Context, key string) error
}

// envVarRow 解耦 store.EnvVarRow（key/value 明文存储，env-management spec）。
type envVarRow struct {
	Key   string
	Value string
}

// globalEnvVarRow 解耦 store.GlobalEnvVarRow（design.md §8：全局级 env，mode ∈ follow_host | manual）。
type globalEnvVarRow struct {
	Key   string
	Mode  string
	Value string
}

// envVarDTO 单条 env 变量。
type envVarDTO struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// globalEnvVarDTO 全局级 env 单条（design.md §21：含 mode 与 resolvedValue）。
// resolvedValue：follow_host 项为服务端 os.LookupEnv 当前解析值（未设置为 ""）；
// manual 项同 value。
type globalEnvVarDTO struct {
	Key           string `json:"key"`
	Mode          string `json:"mode"`
	Value         string `json:"value"`
	ResolvedValue string `json:"resolvedValue"`
}

// globalEnvListResponse 全局级 env 列表响应（design.md §21）。
type globalEnvListResponse struct {
	Vars            []globalEnvVarDTO `json:"vars"`
	Warning         string            `json:"warning"`
	RestartRequired bool              `json:"restartRequired"`
}

// globalEnvUpsertReq PUT /api/v1/env 请求体（design.md §21）。
type globalEnvUpsertReq struct {
	Key   string `json:"key"`
	Mode  string `json:"mode"`
	Value string `json:"value"`
}

// env mode 枚举（design.md §8：follow_host | manual）。
const (
	envModeFollowHost = "follow_host"
	envModeManual     = "manual"
)

func validEnvMode(m string) bool {
	return m == envModeFollowHost || m == envModeManual
}

// envListResponse env 列表响应，含重启生效提示（env-management spec：修改仅下次激活生效）。
type envListResponse struct {
	Vars            []envVarDTO `json:"vars"`
	RestartRequired bool        `json:"restartRequired"`
	Warning         string      `json:"warning,omitempty"`
}

// envUpsertReq PUT 请求体（upsert key/value）。
type envUpsertReq struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// envMutationResponse 修改类响应（PUT/DELETE）：提示重启生效与明文存储风险。
type envMutationResponse struct {
	RestartRequired bool   `json:"restartRequired"`
	Warning         string `json:"warning,omitempty"`
}

// envWarning 明文存储风险提示（env-management spec：系统 UI SHALL 提示用户勿存放高敏感凭据）。
const envWarning = "env vars are stored in plaintext; avoid storing high-sensitive credentials"

// envRestartHint 生效时机提示（env-management spec：修改仅在该任务下一次挂起后激活生效）。
const envRestartHint = "changes take effect on next activate (suspend then activate); restartRequired=true"

// envReservedKeyPrefix 内部生命周期/系统变量前缀（env-management spec：
// 生命周期变量 OCDECK_* MUST NOT 被用户 env 覆盖）。
const envReservedKeyPrefix = "OCDECK_"

// envReservedKeyExact 系统内部变量精确名单（env-management spec：用户变量不覆盖内部变量，
// OPENCODE_SERVER_PASSWORD 由系统注入 serve/TUI 会话）。
var envReservedKeyExact = map[string]bool{
	"OPENCODE_SERVER_PASSWORD": true,
}

// envKeyReserved 判断 key 是否为系统保留（OCDECK_* 前缀或精确系统变量），
// 是则用户 MUST NOT 经 API 写入（env-management spec "用户变量不覆盖内部变量" 的 API 侧防线）。
func envKeyReserved(key string) bool {
	if envReservedKeyExact[key] {
		return true
	}
	return strings.HasPrefix(key, envReservedKeyPrefix)
}

// envKeyPattern 进程环境变量命名规则（POSIX：字母/下划线开头，后接字母/数字/下划线）。
// S1：非法 key（如 1BAD、含空格）MUST 在 API 层拒绝（422 invalid_input），否则入库后激活时
// 被 process 层拒绝（tmux/shell setenv 校验），留脏数据。
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envKeyValid 校验 env key 字符集（进程环境变量命名规则）。
func envKeyValid(key string) bool {
	return envKeyPattern.MatchString(key)
}

// hostEnvLookup 读取服务端进程环境变量（design.md §2/§21：全局级 follow_host 解析）。
// 用于 GET /env 的 resolvedValue 展示（未设置返回 ok=false → resolvedValue=""）。
func hostEnvLookup(key string) (string, bool) {
	return os.LookupEnv(key)
}

func (r envUpsertReq) validate() *ApiError {
	if r.Key == "" {
		return NewError(CodeInvalidInput, "env key is required")
	}
	if !envKeyValid(r.Key) {
		return NewError(CodeInvalidInput, "env key "+r.Key+" has invalid characters (must match process env naming rules: ^[A-Za-z_][A-Za-z0-9_]*$)")
	}
	if envKeyReserved(r.Key) {
		return NewError(CodeInvalidInput, "env key "+r.Key+" is reserved by the system and cannot be set by users")
	}
	return nil
}

// validate 校验全局级 env PUT 请求（design.md §21：mode 校验枚举；key 复用保留 key 校验；
// follow_host 时 value 可空）。
func (r globalEnvUpsertReq) validate() *ApiError {
	if r.Key == "" {
		return NewError(CodeInvalidInput, "env key is required")
	}
	if !envKeyValid(r.Key) {
		return NewError(CodeInvalidInput, "env key "+r.Key+" has invalid characters (must match process env naming rules: ^[A-Za-z_][A-Za-z0-9_]*$)")
	}
	if envKeyReserved(r.Key) {
		return NewError(CodeInvalidInput, "env key "+r.Key+" is reserved by the system and cannot be set by users")
	}
	if !validEnvMode(r.Mode) {
		return NewError(CodeInvalidInput, "env mode must be follow_host or manual")
	}
	return nil
}

// registerEnvRoutes 注册 global/project/task 级 env 路由（design.md §21）。
// 路由同构：GET /{scope}/{id}/env、PUT /{scope}/{id}/env、DELETE /{scope}/{id}/env/:key。
// 全局级无 id 段：GET/PUT /api/v1/env、DELETE /api/v1/env/:key。
func (s *Server) registerEnvRoutes(mux *http.ServeMux) {
	if s.envs == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/env", s.handleListGlobalEnv)
	mux.HandleFunc("PUT /api/v1/env", s.handleUpsertGlobalEnv)
	mux.HandleFunc("DELETE /api/v1/env/{key}", s.handleDeleteGlobalEnv)
	mux.HandleFunc("GET /api/v1/projects/{id}/env", s.handleListProjectEnv)
	mux.HandleFunc("PUT /api/v1/projects/{id}/env", s.handleUpsertProjectEnv)
	mux.HandleFunc("DELETE /api/v1/projects/{id}/env/{key}", s.handleDeleteProjectEnv)
	mux.HandleFunc("GET /api/v1/tasks/{id}/env", s.handleListTaskEnv)
	mux.HandleFunc("PUT /api/v1/tasks/{id}/env", s.handleUpsertTaskEnv)
	mux.HandleFunc("DELETE /api/v1/tasks/{id}/env/{key}", s.handleDeleteTaskEnv)
}

// handleListGlobalEnv GET /api/v1/env（design.md §21）。
// resolvedValue：follow_host 项取服务端 os.LookupEnv 当前值（未设置为 ""）；manual 项同 value。
func (s *Server) handleListGlobalEnv(w http.ResponseWriter, r *http.Request) {
	rows, err := s.envs.ListGlobalEnvVars(r.Context())
	if err != nil {
		writeError(w, CodeInternal, "list global env failed")
		return
	}
	vars := make([]globalEnvVarDTO, 0, len(rows))
	for _, e := range rows {
		resolved := e.Value
		if e.Mode == envModeFollowHost {
			if v, ok := hostEnvLookup(e.Key); ok {
				resolved = v
			} else {
				resolved = ""
			}
		}
		vars = append(vars, globalEnvVarDTO{
			Key: e.Key, Mode: e.Mode, Value: e.Value, ResolvedValue: resolved,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(globalEnvListResponse{
		Vars: vars, RestartRequired: false, Warning: envWarning,
	})
}

// handleUpsertGlobalEnv PUT /api/v1/env（upsert，design.md §21）。
func (s *Server) handleUpsertGlobalEnv(w http.ResponseWriter, r *http.Request) {
	var req globalEnvUpsertReq
	if err := decodeJSON(r, &req); err != nil {
		writeApiError(w, err)
		return
	}
	if ae := req.validate(); ae != nil {
		writeApiError(w, ae)
		return
	}
	if err := s.envs.SetGlobalEnvVar(r.Context(), req.Key, req.Mode, req.Value); err != nil {
		writeError(w, CodeInternal, "set global env failed")
		return
	}
	writeEnvMutation(w)
}

// handleDeleteGlobalEnv DELETE /api/v1/env/:key（design.md §21）。
func (s *Server) handleDeleteGlobalEnv(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := s.envs.DeleteGlobalEnvVar(r.Context(), key); err != nil {
		writeError(w, CodeInternal, "delete global env failed")
		return
	}
	writeEnvMutation(w)
}

// handleListProjectEnv GET /api/v1/projects/:id/env。
func (s *Server) handleListProjectEnv(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.projs.GetProject(r.Context(), projectID); err != nil {
		writeApiError(w, NewError(CodeNotFound, "project not found"))
		return
	}
	rows, err := s.envs.ListProjectEnvVars(r.Context(), projectID)
	if err != nil {
		writeError(w, CodeInternal, "list project env failed")
		return
	}
	s.writeEnvList(w, rows)
}

// handleUpsertProjectEnv PUT /api/v1/projects/:id/env（upsert）。
func (s *Server) handleUpsertProjectEnv(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.projs.GetProject(r.Context(), projectID); err != nil {
		writeApiError(w, NewError(CodeNotFound, "project not found"))
		return
	}
	var req envUpsertReq
	if err := decodeJSON(r, &req); err != nil {
		writeApiError(w, err)
		return
	}
	if ae := req.validate(); ae != nil {
		writeApiError(w, ae)
		return
	}
	if err := s.envs.SetProjectEnvVar(r.Context(), projectID, req.Key, req.Value); err != nil {
		writeError(w, CodeInternal, "set project env failed")
		return
	}
	writeEnvMutation(w)
}

// handleDeleteProjectEnv DELETE /api/v1/projects/:id/env/:key。
func (s *Server) handleDeleteProjectEnv(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	key := r.PathValue("key")
	if _, err := s.projs.GetProject(r.Context(), projectID); err != nil {
		writeApiError(w, NewError(CodeNotFound, "project not found"))
		return
	}
	if err := s.envs.DeleteProjectEnvVar(r.Context(), projectID, key); err != nil {
		writeError(w, CodeInternal, "delete project env failed")
		return
	}
	writeEnvMutation(w)
}

// handleListTaskEnv GET /api/v1/tasks/:id/env。
func (s *Server) handleListTaskEnv(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if _, err := s.tasks.Get(r.Context(), taskID); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	rows, err := s.envs.ListTaskEnvVars(r.Context(), taskID)
	if err != nil {
		writeError(w, CodeInternal, "list task env failed")
		return
	}
	s.writeEnvList(w, rows)
}

// handleUpsertTaskEnv PUT /api/v1/tasks/:id/env（upsert）。
func (s *Server) handleUpsertTaskEnv(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if _, err := s.tasks.Get(r.Context(), taskID); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	var req envUpsertReq
	if err := decodeJSON(r, &req); err != nil {
		writeApiError(w, err)
		return
	}
	if ae := req.validate(); ae != nil {
		writeApiError(w, ae)
		return
	}
	if err := s.envs.SetTaskEnvVar(r.Context(), taskID, req.Key, req.Value); err != nil {
		writeError(w, CodeInternal, "set task env failed")
		return
	}
	writeEnvMutation(w)
}

// handleDeleteTaskEnv DELETE /api/v1/tasks/:id/env/:key。
func (s *Server) handleDeleteTaskEnv(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	key := r.PathValue("key")
	if _, err := s.tasks.Get(r.Context(), taskID); err != nil {
		writeApiError(w, mapTaskErr(err))
		return
	}
	if err := s.envs.DeleteTaskEnvVar(r.Context(), taskID, key); err != nil {
		writeError(w, CodeInternal, "delete task env failed")
		return
	}
	writeEnvMutation(w)
}

// writeEnvList 写入 env 列表响应（含重启生效提示 + 明文风险提示）。
func (s *Server) writeEnvList(w http.ResponseWriter, rows []envVarRow) {
	vars := make([]envVarDTO, 0, len(rows))
	for _, e := range rows {
		vars = append(vars, envVarDTO{Key: e.Key, Value: e.Value})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envListResponse{
		Vars: vars, RestartRequired: false, Warning: envWarning,
	})
}

// writeEnvMutation 写入 env 修改响应（PUT/DELETE）：restartRequired=true + 风险提示。
func writeEnvMutation(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envMutationResponse{RestartRequired: true, Warning: envWarning})
}
