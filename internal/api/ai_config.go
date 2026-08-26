package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"ocdeck/internal/infrastructure/ai"
)

// aiConfigDTO 为 GET / PUT /api/v1/ai/config 的统一响应体（design.md D6）。
// api_key_masked 永远只含掩码，MUST NOT 携带完整 key。
type aiConfigDTO struct {
	Configured   bool   `json:"configured"`
	Provider     string `json:"provider"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	Thinking     string `json:"thinking"` // 空 = 未设置（不下发思考参数）
	APIKeyMasked string `json:"api_key_masked"`
	LoadError    string `json:"load_error,omitempty"`
}

// aiConfigPutReq PUT 请求体（design.md D6）。
// api_key 为掩码值（含 `***`）或空串时保留已存储原 key（委托 Store.Put 合并语义）。
type aiConfigPutReq struct {
	Provider string `json:"provider"`
	APIKey   string `json:"api_key"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
}

// aiConfigPutBodyMax 限制 PUT 请求体读取上限（AI 配置字段短小，1KiB 足够并防滥用）。
const aiConfigPutBodyMax = 1 << 10

// SetAIConfigStore 注入全局 AI 配置 Store（design.md D7 wiring）。
// 沿用 SetTaskBackend 模式；延迟注入需调用 RebuildRoutes。
func (s *Server) SetAIConfigStore(store *ai.Store) {
	s.aiConfig = store
}

// registerAIConfigRoutes 注册 AI 配置路由（design.md D6）。
// store 未注入时不注册，由 handler 层对 nil 做防御（GET 降级、PUT 500）。
func (s *Server) registerAIConfigRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/ai/config", s.handleGetAIConfig)
	mux.HandleFunc("PUT /api/v1/ai/config", s.handlePutAIConfig)
}

// handleGetAIConfig GET /api/v1/ai/config（design.md D6）。
// 未配置 → configured=false 其余空串；损坏/不可读 → configured=false + load_error，MUST NOT 500。
func (s *Server) handleGetAIConfig(w http.ResponseWriter, r *http.Request) {
	if s.aiConfig == nil {
		writeAIConfig(w, aiConfigDTO{})
		return
	}
	writeAIConfig(w, buildAIConfigDTO(s.aiConfig))
}

// handlePutAIConfig PUT /api/v1/ai/config（design.md D6）。
// 校验失败 → 422 invalid_input；Store.Put 负责旧 key 合并、原子写、快照热替换。
// 成功 → 200 + 与 GET 同形响应。
func (s *Server) handlePutAIConfig(w http.ResponseWriter, r *http.Request) {
	if s.aiConfig == nil {
		// 沿用项目错误结构，但 nil store 属服务端 wiring 缺失（非客户端可修复），
		// 按 design.md D6 要求返回 5xx；code 用 invalid_state 标识 wiring 态。
		writeJSONError(w, http.StatusInternalServerError, CodeInvalidState, "ai config store not configured")
		return
	}
	var req aiConfigPutReq
	r.Body = http.MaxBytesReader(w, r.Body, aiConfigPutBodyMax)
	if ae := decodeJSON(r, &req); ae != nil {
		writeApiError(w, ae)
		return
	}
	if ae := req.validate(s.aiConfig); ae != nil {
		writeApiError(w, ae)
		return
	}
	incoming := ai.ProviderConfig{
		Provider: ai.Provider(req.Provider),
		APIKey:   req.APIKey,
		BaseURL:  req.BaseURL,
		Model:    req.Model,
		Thinking: req.Thinking,
	}
	if err := s.aiConfig.Put(incoming); err != nil {
		// Put 的校验失败（含无旧 key 且空 key）已由 req.validate 提前拦截；
		// 此处残余错误多为写文件失败等系统错误，按 internal 处理不泄露细节。
		writeError(w, CodeInternal, "save ai config failed")
		return
	}
	writeAIConfig(w, buildAIConfigDTO(s.aiConfig))
}

// validate 校验 PUT 请求（design.md D6 + spec）：
//   - provider ∈ {openai, anthropic}；
//   - model 非空；
//   - base_url 可空或合法 http(s) URL；
//   - thinking 可空或枚举值（"" | off | low | medium | high）；
//   - api_key 为掩码值（含 `***`）或空串时，需有已存储原 key，否则 422；
//   - api_key 为非空非掩码值但 trim 后为空（如 "   "）→ 422 invalid_input，
//     不落给 Store 变成 internal 500。
//
// 字段级校验复用 ai.ProviderConfig.Validate，避免与 Store 校验逻辑漂移。
func (r aiConfigPutReq) validate(store *ai.Store) *ApiError {
	cfg := ai.ProviderConfig{
		Provider: ai.Provider(r.Provider),
		BaseURL:  r.BaseURL,
		Model:    r.Model,
		Thinking: r.Thinking,
	}
	if err := cfg.Validate(); err != nil {
		return NewError(CodeInvalidInput, err.Error())
	}
	if r.APIKey == "" || strings.Contains(r.APIKey, "***") {
		// 空串或掩码值 → 依赖已存储原 key；无旧 key 则 422。
		if store == nil || strings.TrimSpace(store.Config().APIKey) == "" {
			return NewError(CodeInvalidInput, "api_key must not be empty")
		}
		return nil
	}
	// 非空非掩码的新 key：trim 后为空视为非法（避免落给 Store 变成 internal 500）。
	if strings.TrimSpace(r.APIKey) == "" {
		return NewError(CodeInvalidInput, "api_key must not be empty")
	}
	return nil
}

// buildAIConfigDTO 由 Store 当前快照构造响应 DTO。
// MUST 在单次快照读内完成全部字段构造（design.md D7：读方单次操作全程同一快照），
// 避免并发 PUT 下 Configured/Config/LoadError 字段混配。
func buildAIConfigDTO(store *ai.Store) aiConfigDTO {
	st := store.State()
	dto := aiConfigDTO{
		Configured:   st.Configured,
		Provider:     string(st.CFG.Provider),
		BaseURL:      st.CFG.BaseURL,
		Model:        st.CFG.Model,
		Thinking:     st.CFG.Thinking,
		APIKeyMasked: maskAPIKey(st.CFG.APIKey),
	}
	if st.LoadErr != nil {
		dto.LoadError = st.LoadErr.Error()
	}
	return dto
}

// maskAPIKey 按 design.md D6 掩码规则：len ≥ 8 → 前 4 位 + `***`；len < 8 → 纯 `***`；无 key → `""`。
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) < 8 {
		return "***"
	}
	return key[:4] + "***"
}

// writeAIConfig 以 200 + application/json 写入 AI 配置响应体。
func writeAIConfig(w http.ResponseWriter, dto aiConfigDTO) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dto)
}