package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"ocdeck/internal/config"
)

// OCConfigService 全局 oc 配置管理服务接口（design.md §13/§21）。
// 由 config.OCConfigManager 实现；api handler 只做 DTO/HTTP 语义。
type OCConfigService interface {
	List() ([]config.OCConfigName, error)
	Read(name string) (config.OCConfigContent, error)
	Save(name, content string, expectedMtime int64, expectedHash string) (config.OCConfigContent, error)
}

// registerOCConfigRoutes 注册全局 oc 配置管理路由（design.md §21）。
func (s *Server) registerOCConfigRoutes(mux *http.ServeMux) {
	if s.ocCfgs == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/oc-configs", s.handleListOCConfigs)
	mux.HandleFunc("GET /api/v1/oc-configs/{name}", s.handleGetOCConfig)
	mux.HandleFunc("PUT /api/v1/oc-configs/{name}", s.handleSaveOCConfig)
}

// ocConfigNameDTO 配置列表项（design.md §21）。
type ocConfigNameDTO struct {
	Name string `json:"name"`
}

// ocConfigListResponse 配置列表响应。
type ocConfigListResponse struct {
	Configs []ocConfigNameDTO `json:"configs"`
}

// ocConfigDTO 配置内容 DTO（读取响应）。
type ocConfigDTO struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Mtime   int64  `json:"mtime"`
	Hash    string `json:"hash"`
}

// ocConfigSaveReq PUT 请求体（design.md §21：携带 content + mtime + hash 乐观并发）。
type ocConfigSaveReq struct {
	Content string `json:"content"`
	Mtime   int64  `json:"mtime"`
	Hash    string `json:"hash"`
}

// ocConfigSaveResponse 保存响应：含受影响活跃任务列表 + 重启生效提示
//（design.md §13/global-config-management spec：保存后列出受影响活跃任务，提示手动重启生效）。
type ocConfigSaveResponse struct {
	Mtime                int64    `json:"mtime"`
	Hash                 string   `json:"hash"`
	AffectedActiveTasks  []string `json:"affectedActiveTasks"`
	RestartRequired      bool     `json:"restartRequired"`
}

// handleListOCConfigs GET /api/v1/oc-configs（design.md §13/§21）。
func (s *Server) handleListOCConfigs(w http.ResponseWriter, r *http.Request) {
	names, err := s.ocCfgs.List()
	if err != nil {
		writeError(w, CodeInternal, "list oc-configs failed")
		return
	}
	configs := make([]ocConfigNameDTO, 0, len(names))
	for _, n := range names {
		configs = append(configs, ocConfigNameDTO{Name: n.Name})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ocConfigListResponse{Configs: configs})
}

// handleGetOCConfig GET /api/v1/oc-configs/:name（design.md §13/§21）。
func (s *Server) handleGetOCConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := s.ocCfgs.Read(name)
	if err != nil {
		writeOCConfigErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ocConfigDTO{
		Name: c.Name, Content: c.Content, Mtime: c.Mtime, Hash: c.Hash,
	})
}

// handleSaveOCConfig PUT /api/v1/oc-configs/:name（design.md §13/§21）。
// 语法校验/乐观并发/原子写入/.bak/symlink 拒绝由 OCConfigService 处理；
// 成功后列出受影响活跃任务并提示手动重启生效。
func (s *Server) handleSaveOCConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req ocConfigSaveReq
	if err := decodeJSON(r, &req); err != nil {
		writeApiError(w, err)
		return
	}
	saved, serr := s.ocCfgs.Save(name, req.Content, req.Mtime, req.Hash)
	if serr != nil {
		writeOCConfigErr(w, serr)
		return
	}
	// 受影响=全部 active 任务（global-config-management spec）。
	affected := s.listAffectedActiveTasks(r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ocConfigSaveResponse{
		Mtime: saved.Mtime, Hash: saved.Hash,
		AffectedActiveTasks: affected, RestartRequired: true,
	})
}

// listAffectedActiveTasks 返回当前全部 active 任务 ID（供全局配置保存后受影响提示）。
// TaskBackend 未注入或查询失败时返回空切片（不阻塞保存响应）。
func (s *Server) listAffectedActiveTasks(r *http.Request) []string {
	if s.tasks == nil {
		return []string{}
	}
	ids, err := s.tasks.ListAllActiveTaskIDs(r.Context())
	if err != nil {
		return []string{}
	}
	return ids
}

// writeOCConfigErr 将 OCConfigService 错误映射为统一错误结构（design.md §21）。
// 语法错误 → 422 invalid_input；乐观并发冲突 → 409 conflict；不存在 → 404 not_found；
// 非法文件名 → 422 invalid_input；其余 → 500 internal。
func writeOCConfigErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, config.ErrConfigConflict):
		writeError(w, CodeConflict, "file changed externally")
	case errors.Is(err, config.ErrConfigNotFound):
		writeError(w, CodeNotFound, "oc-config not found")
	case errors.Is(err, config.ErrInvalidName):
		writeApiError(w, NewError(CodeInvalidInput, err.Error()))
	case errors.Is(err, config.ErrInvalidSyntax):
		writeApiError(w, NewError(CodeInvalidInput, err.Error()))
	default:
		writeError(w, CodeInternal, "oc-config operation failed")
	}
}