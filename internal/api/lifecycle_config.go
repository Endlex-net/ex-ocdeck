package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// LifecycleConfigStore 提供 project_lifecycle_configs 读写能力（design.md §2/§8）。
// 复用 internal/store 既有 Queries 方法签名，经 adapter 解耦 store 包结构。
type LifecycleConfigStore interface {
	// GetLifecycleConfig 读取项目生命周期配置。缺行返回空配置（三字段空串），非错误。
	GetLifecycleConfig(ctx context.Context, projectID string) (lifecycleConfigRow, error)
	// UpsertLifecycleConfig 整体替换 upsert（INSERT … ON CONFLICT DO UPDATE）。
	UpsertLifecycleConfig(ctx context.Context, projectID, inheritPatterns, initScript, preDeleteScript string) error
}

// lifecycleConfigRow 解耦 store.LifecycleConfigRow（design.md §2，缺行三字段为空串）。
type lifecycleConfigRow struct {
	ProjectID       string
	InheritPatterns string
	InitScript      string
	PreDeleteScript string
	UpdatedAt       int64
}

// lifecycleConfigDTO 生命周期配置响应体（design.md §8）。
type lifecycleConfigDTO struct {
	InheritPatterns string `json:"inherit_patterns"`
	InitScript      string `json:"init_script"`
	PreDeleteScript string `json:"pre_delete_script"`
}

// lifecycleConfigUpsertReq PUT 请求体（整体替换，design.md §8）。
type lifecycleConfigUpsertReq struct {
	InheritPatterns string `json:"inherit_patterns"`
	InitScript      string `json:"init_script"`
	PreDeleteScript string `json:"pre_delete_script"`
}

// 脚本与 patterns 大小上限（design.md §8：脚本各 ≤64KB、inherit_patterns 整体 ≤16KB）。
// 上限按 JSON 解码后字符串长度计算，故 raw body 上限需留出 JSON 转义膨胀空间。
const (
	lifecycleScriptMax   = 64 * 1024
	lifecyclePatternsMax = 16 * 1024
	// lifecyclePutBodyMax 限制 PUT 请求体读取上限（防 JSON 解码前分配任意大内存）。
	// 字段上限按解码后长度计：脚本各 64KB、patterns 16KB。最坏合法 JSON 转义是每个
	// 字符编成 `\uXXXX`（6 字节），故上限 ≈ (64K*2+16K)*6 ≈ 864KB；1MiB 覆盖该最坏情况，
	// 既能容纳契约内合法配置（即使全反斜杠内容需大量转义），又限制恶意超大 body。
	lifecyclePutBodyMax = 1 << 20
)

// validate 校验 PUT 请求：inherit_patterns 逐行 glob 语法（空行与 # 注释行忽略），
// 脚本各 ≤64KB、inherit_patterns 整体 ≤16KB。非法 → invalid_input + 行号。
func (r lifecycleConfigUpsertReq) validate() *ApiError {
	if len(r.InheritPatterns) > lifecyclePatternsMax {
		return NewError(CodeInvalidInput, "inherit_patterns exceeds 16KB limit")
	}
	if len(r.InitScript) > lifecycleScriptMax {
		return NewError(CodeInvalidInput, "init_script exceeds 64KB limit")
	}
	if len(r.PreDeleteScript) > lifecycleScriptMax {
		return NewError(CodeInvalidInput, "pre_delete_script exceeds 64KB limit")
	}
	// 逐行校验 glob 语法（空行与 # 注释行忽略）。与 internal/lifecycle 执行侧同库
	// （doublestar/v4 ValidatePattern），保证校验与执行一致。
	lineNo := 0
	for _, raw := range strings.Split(r.InheritPatterns, "\n") {
		lineNo++
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !doublestar.ValidatePattern(line) {
			return NewError(CodeInvalidInput, "inherit_patterns line "+strconv.Itoa(lineNo)+" invalid glob syntax")
		}
	}
	return nil
}

// registerLifecycleConfigRoutes 注册 lifecycle-config 路由（design.md §8）。
func (s *Server) registerLifecycleConfigRoutes(mux *http.ServeMux) {
	if s.lifecycleCfgs == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/projects/{id}/lifecycle-config", s.handleGetLifecycleConfig)
	mux.HandleFunc("PUT /api/v1/projects/{id}/lifecycle-config", s.handleUpsertLifecycleConfig)
}

// handleGetLifecycleConfig GET /api/v1/projects/{id}/lifecycle-config（design.md §8）。
// 项目不存在 → not_found；缺行返回三字段空串（200）。
func (s *Server) handleGetLifecycleConfig(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.projs.GetProject(r.Context(), projectID); err != nil {
		writeApiError(w, NewError(CodeNotFound, "project not found"))
		return
	}
	cfg, err := s.lifecycleCfgs.GetLifecycleConfig(r.Context(), projectID)
	if err != nil {
		writeError(w, CodeInternal, "get lifecycle config failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(lifecycleConfigDTO{
		InheritPatterns: cfg.InheritPatterns,
		InitScript:      cfg.InitScript,
		PreDeleteScript: cfg.PreDeleteScript,
	})
}

// handleUpsertLifecycleConfig PUT /api/v1/projects/{id}/lifecycle-config（design.md §8）。
// 项目不存在 → not_found；整体替换 upsert；非法 glob → invalid_input+行号；超限 → invalid_input。
func (s *Server) handleUpsertLifecycleConfig(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.projs.GetProject(r.Context(), projectID); err != nil {
		writeApiError(w, NewError(CodeNotFound, "project not found"))
		return
	}
	var req lifecycleConfigUpsertReq
	r.Body = http.MaxBytesReader(w, r.Body, lifecyclePutBodyMax)
	if err := decodeJSON(r, &req); err != nil {
		writeApiError(w, err)
		return
	}
	if ae := req.validate(); ae != nil {
		writeApiError(w, ae)
		return
	}
	if err := s.lifecycleCfgs.UpsertLifecycleConfig(r.Context(), projectID, req.InheritPatterns, req.InitScript, req.PreDeleteScript); err != nil {
		writeError(w, CodeInternal, "upsert lifecycle config failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(lifecycleConfigDTO{
		InheritPatterns: req.InheritPatterns,
		InitScript:      req.InitScript,
		PreDeleteScript: req.PreDeleteScript,
	})
}
