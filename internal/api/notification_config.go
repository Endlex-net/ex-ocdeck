// notification_config.go 通知配置读写 API 与测试通知端点（task-notifications
// design D5/D7/D8/D11；spec「通知配置读写 API」「测试通知」）。
//
// GET/PUT /api/v1/notification/config：GET 与 PUT 请求体 JSON 同形（仅 bark 令牌
// 字段名 token_masked / token 差异、load_error 只读仅损坏时出现）；掩码复用
// notify.MaskToken；PUT 委托 Store.Put 完成「token 合并 → 校验 → 原子写 → 快照
// 替换」全序列（写锁串行化，last-writer-wins）。
// POST /api/v1/notification/test：store 快照读取、总开关 422、baseURL 解析后
// 委托 NotificationTester（*application/notification.Notifier 结构性满足）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	domainnotification "ocdeck/internal/domain/notification"
	"ocdeck/internal/infrastructure/notify"
)

// notificationConfigPutBodyMax 限制 PUT 请求体读取上限（通知配置字段短小，
// 4KiB 足够并防滥用）。
const notificationConfigPutBodyMax = 4 << 10

// NotificationTester 测试通知窄端口（design D11 SetNotificationTester）。
// *application/notification.Notifier 结构性满足本接口。
type NotificationTester interface {
	SendTestNotification(ctx context.Context, cfg domainnotification.Config, baseURL string) []domainnotification.ChannelResult
}

// SetNotificationStore 注入通知配置 Store（design D11 wiring；沿
// SetAIConfigStore 模式）。延迟注入需调用 RebuildRoutes。
func (s *Server) SetNotificationStore(store *notify.Store) {
	s.notifyStore = store
}

// SetNotificationTester 注入测试通知端口（design D11：组合根传入 notifier）。
func (s *Server) SetNotificationTester(tester NotificationTester) {
	s.notificationTester = tester
}

// registerNotificationRoutes 注册通知路由（stream/config/test）。
// handler 层对 nil store 做防御（GET 降级默认配置、PUT/test 500），沿
// registerAIConfigRoutes 模式。
func (s *Server) registerNotificationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/notifications/stream", s.handleNotificationsStream)
	mux.HandleFunc("GET /api/v1/notification/config", s.handleGetNotificationConfig)
	mux.HandleFunc("PUT /api/v1/notification/config", s.handlePutNotificationConfig)
	mux.HandleFunc("POST /api/v1/notification/test", s.handleTestNotification)
}

// handleGetNotificationConfig GET /api/v1/notification/config（spec「通知配置
// 读写 API」）：文件不存在 → 默认配置；损坏/不可读 → 默认配置 + load_error
// （MUST NOT 500）；成功 → 200 与存储 schema 同形（token_masked 替代 token）。
func (s *Server) handleGetNotificationConfig(w http.ResponseWriter, r *http.Request) {
	if s.notifyStore == nil {
		writeNotificationConfig(w, buildNotificationConfigDTOFromState(notify.StoreState{
			Config: domainnotification.DefaultConfig(),
		}))
		return
	}
	writeNotificationConfig(w, buildNotificationConfigDTO(s.notifyStore))
}

// handlePutNotificationConfig PUT /api/v1/notification/config（spec「通知配置
// 读写 API」）：非法 JSON → 400；缺必填键/null/业务校验失败 → 422；写文件
// 失败 → 500 且旧内存快照不变；成功 → 200 与 GET 同形响应（含 token_masked、
// 不含 load_error）。
func (s *Server) handlePutNotificationConfig(w http.ResponseWriter, r *http.Request) {
	if s.notifyStore == nil {
		writeJSONError(w, http.StatusInternalServerError, CodeInvalidState, "notification store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, notificationConfigPutBodyMax)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, CodeInvalidInput, "invalid request body")
		return
	}
	cfg, err := notify.DecodeConfig(data)
	if err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			writeJSONError(w, http.StatusBadRequest, CodeInvalidInput, "invalid JSON body")
			return
		}
		writeJSONError(w, http.StatusUnprocessableEntity, CodeInvalidInput, err.Error())
		return
	}
	if err := cfg.Validate(); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, CodeInvalidInput, err.Error())
		return
	}
	if err := s.notifyStore.Put(cfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, CodeInternal, "save notification config failed")
		return
	}
	writeNotificationConfig(w, buildNotificationConfigDTO(s.notifyStore))
}

// handleTestNotification POST /api/v1/notification/test（spec「测试通知」）：
// 总开关关闭 → 422 invalid_state；跳转 URL 不可用 → 422；未注入 tester → 500；
// 成功 → 200 顶层 {"results":[{name,status,error}]}。
func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	if s.notifyStore == nil {
		writeJSONError(w, http.StatusInternalServerError, CodeInvalidState, "notification store not configured")
		return
	}
	st := s.notifyStore.State()
	if !st.Config.Enabled {
		writeJSONError(w, http.StatusUnprocessableEntity, CodeInvalidState, "notification master switch is off, enable it before sending a test")
		return
	}
	base, err := s.NotificationBaseURL(st.Config.BaseURL)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, CodeInvalidState, err.Error())
		return
	}
	if s.notificationTester == nil {
		writeJSONError(w, http.StatusInternalServerError, CodeInvalidState, "notification tester not configured")
		return
	}
	results := s.notificationTester.SendTestNotification(r.Context(), st.Config, base)
	out := make([]ChannelTestResult, len(results))
	for i, r := range results {
		out[i] = ChannelTestResult{Name: r.Name, Status: r.Status, Error: r.Error}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Results []ChannelTestResult `json:"results"`
	}{Results: out})
}

// NotificationBaseURL 跳转 base 推导（design D8/D11：BaseURLResolver）。
// configuredBaseURL 非空 → 剔除尾部 '/' 后用之（由候选判定的 ConfigSnapshot
// 传入，本方法不读 notifyStore）；否则 host 取 ListenAddr（wildcard
// 0.0.0.0/::/空 → 127.0.0.1），port 取 BoundAddr 实际端口。未 Listen 且未配置 → 错误。
func (s *Server) NotificationBaseURL(configuredBaseURL string) (string, error) {
	if configuredBaseURL != "" {
		return strings.TrimRight(configuredBaseURL, "/"), nil
	}
	addr := s.BoundAddr()
	if addr == nil {
		return "", errors.New("notification base url unavailable: server not listening and no base_url configured")
	}
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", errors.New("notification base url unavailable: invalid bound address")
	}
	host := s.cfg.ListenAddr
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, portStr), nil
}

// ChannelTestResult 单渠道测试通知结果（响应顶层 {"results":[...]} 元素）。
type ChannelTestResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

type notificationBarkDTO struct {
	Enabled     bool   `json:"enabled"`
	Endpoint    string `json:"endpoint"`
	TokenMasked string `json:"token_masked"`
}

type notificationChannelsDTO struct {
	Web   domainnotification.WebChannelConfig   `json:"web"`
	Bark  notificationBarkDTO                   `json:"bark"`
	Macos domainnotification.MacosChannelConfig `json:"macos"`
}

type notificationConfigDTO struct {
	Enabled            bool                          `json:"enabled"`
	Categories         domainnotification.Categories `json:"categories"`
	IdleTimeoutSeconds int                           `json:"idle_timeout_seconds"`
	Channels           notificationChannelsDTO       `json:"channels"`
	LLMSummary         bool                          `json:"llm_summary"`
	BaseURL            string                        `json:"base_url"`
	LoadError          string                        `json:"load_error,omitempty"`
}

func buildNotificationConfigDTO(store *notify.Store) notificationConfigDTO {
	return buildNotificationConfigDTOFromState(store.State())
}

func buildNotificationConfigDTOFromState(st notify.StoreState) notificationConfigDTO {
	dto := notificationConfigDTO{
		Enabled:            st.Config.Enabled,
		Categories:         st.Config.Categories,
		IdleTimeoutSeconds: st.Config.IdleTimeoutSeconds,
		Channels: notificationChannelsDTO{
			Web: st.Config.Channels.Web,
			Bark: notificationBarkDTO{
				Enabled:     st.Config.Channels.Bark.Enabled,
				Endpoint:    st.Config.Channels.Bark.Endpoint,
				TokenMasked: notify.MaskToken(st.Config.Channels.Bark.Token),
			},
			Macos: st.Config.Channels.Macos,
		},
		LLMSummary: st.Config.LLMSummary,
		BaseURL:    st.Config.BaseURL,
	}
	if st.LoadErr != nil {
		dto.LoadError = st.LoadErr.Error()
	}
	return dto
}

func writeNotificationConfig(w http.ResponseWriter, dto notificationConfigDTO) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dto)
}
