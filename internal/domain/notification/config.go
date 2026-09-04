package notification

import (
	"fmt"
	"net/url"
	"strings"
)

// idle_timeout_seconds 取值域（spec「通知配置存储」）。
const (
	MinIdleTimeoutSeconds = 10
	MaxIdleTimeoutSeconds = 3600
)

// DefaultBarkEndpoint bark 渠道默认推送端点（spec「Bark 渠道」，支持自建 server）。
const DefaultBarkEndpoint = "https://api.day.app"

// Categories 通知类别开关（磁盘 schema categories 键，五类别独立开关）。
type Categories struct {
	Question   bool `json:"question"`
	Permission bool `json:"permission"`
	Idle       bool `json:"idle"`
	Retry      bool `json:"retry"`
	Error      bool `json:"error"`
}

// WebChannelConfig web 渠道配置（启用即已配置，无参数字段；spec 能力矩阵）。
type WebChannelConfig struct {
	Enabled bool `json:"enabled"`
}

// BarkChannelConfig bark 渠道配置（endpoint 与 token 均非空才算已配置）。
// Token 仅在内存与 0600 文件中存在，MUST NOT 明文进日志或 API 响应
// （掩码规则见基础设施层 notify.MaskToken）。
type BarkChannelConfig struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
}

// MacosChannelConfig macos 渠道配置（可用性由运行环境探测决定，无参数字段）。
type MacosChannelConfig struct {
	Enabled bool `json:"enabled"`
}

// WecomChannelConfig 企业微信群机器人渠道配置（spec「企业微信渠道」）：用户粘贴
// 完整 webhook URL（含 query），系统原样作为 POST 目标。URL 仅在内存与 0600 文件
// 中存在，MUST NOT 明文进日志或 API 响应（整体掩码见基础设施层 notify.MaskWecomURL）。
type WecomChannelConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
}

// ChannelsConfig 渠道配置（磁盘 schema channels 键）。
type ChannelsConfig struct {
	Web   WebChannelConfig   `json:"web"`
	Bark  BarkChannelConfig  `json:"bark"`
	Macos MacosChannelConfig `json:"macos"`
	Wecom WecomChannelConfig `json:"wecom"`
}

// Config 通知配置模型。磁盘 schema 的唯一表述在 spec「通知配置存储」（字段
// snake_case，全键写盘必含、无 omitempty；未知字段解码时忽略）。
type Config struct {
	Enabled            bool           `json:"enabled"`
	Categories         Categories     `json:"categories"`
	IdleTimeoutSeconds int            `json:"idle_timeout_seconds"`
	Channels           ChannelsConfig `json:"channels"`
	LLMSummary         bool           `json:"llm_summary"`
	BaseURL            string         `json:"base_url"`
}

// DefaultConfig 返回默认配置（spec「通知配置读写 API」）：总开关关闭、类别全开、
// 阈值 60、渠道全关（bark endpoint 取 DefaultBarkEndpoint）。
func DefaultConfig() Config {
	return Config{
		Enabled:            false,
		Categories:         Categories{Question: true, Permission: true, Idle: true, Retry: true, Error: true},
		IdleTimeoutSeconds: 60,
		Channels: ChannelsConfig{
			Web:   WebChannelConfig{Enabled: false},
			Bark:  BarkChannelConfig{Enabled: false, Endpoint: DefaultBarkEndpoint},
			Macos: MacosChannelConfig{Enabled: false},
			Wecom: WecomChannelConfig{Enabled: false, URL: ""},
		},
		LLMSummary: false,
		BaseURL:    "",
	}
}

// Validate 校验配置（spec「通知配置存储」）：idle_timeout_seconds ∈ [10,3600]；
// bark endpoint 与 base_url 非空时 MUST 为有非空 host 的 http(s) hierarchical URL，
// 且 MUST NOT 含 userinfo、query、fragment，path 仅允许空或 "/"。
// wecom url 非空时 MUST 为有非空 host 的 https hierarchical URL，允许 query 与
// 非根 path，MUST NOT 含 userinfo、fragment。
func (c Config) Validate() error {
	if c.IdleTimeoutSeconds < MinIdleTimeoutSeconds || c.IdleTimeoutSeconds > MaxIdleTimeoutSeconds {
		return fmt.Errorf("idle_timeout_seconds %d out of range [%d, %d]",
			c.IdleTimeoutSeconds, MinIdleTimeoutSeconds, MaxIdleTimeoutSeconds)
	}
	if err := validateOptionalURL("bark endpoint", c.Channels.Bark.Endpoint); err != nil {
		return err
	}
	if err := validateOptionalURL("base_url", c.BaseURL); err != nil {
		return err
	}
	return validateWecomURL(c.Channels.Wecom.URL)
}

// validateOptionalURL 校验可空 URL 字段（空串合法）。host 判定用 Hostname() 而非
// Host（拒绝 `http://:8080` 这类 port-only authority）；query 拒绝同时覆盖非空
// RawQuery 与 ForceQuery（`https://host?` 的空 query 分隔符）；fragment 以原始串
// 是否含 `#` 判定（`https://host#` 的空 fragment 分隔符解析后 Fragment 为空串，
// 无法从 parsed 值识别）。
func validateOptionalURL(field, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", field, raw, err)
	}
	if u.Opaque != "" {
		return fmt.Errorf("invalid %s %q: must be a hierarchical http(s) URL", field, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid %s %q: scheme must be http(s)", field, raw)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("invalid %s %q: host must not be empty", field, raw)
	}
	if u.User != nil {
		return fmt.Errorf("invalid %s %q: userinfo not allowed", field, raw)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("invalid %s %q: query not allowed", field, raw)
	}
	if strings.Contains(raw, "#") {
		return fmt.Errorf("invalid %s %q: fragment not allowed", field, raw)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("invalid %s %q: path must be empty or \"/\"", field, raw)
	}
	return nil
}

// validateWecomURL 校验企业微信 webhook URL（spec「企业微信渠道」/design D3）。
// 与 validateOptionalURL 的差异：scheme 仅 https、允许 query 与非根 path、错误
// 信息 MUST NOT 包含 URL 原文（PUT 422 原样回传校验文案）、url.Parse 失败 MUST NOT
// 用 %w 包装（parse error 常含原文）。固定文案 `invalid wecom url: <reason>`。
func validateWecomURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid wecom url: invalid url")
	}
	if u.Opaque != "" {
		return fmt.Errorf("invalid wecom url: must be a hierarchical https URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("invalid wecom url: scheme must be https")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("invalid wecom url: host must not be empty")
	}
	if u.User != nil {
		return fmt.Errorf("invalid wecom url: userinfo not allowed")
	}
	if strings.Contains(raw, "#") {
		return fmt.Errorf("invalid wecom url: fragment not allowed")
	}
	return nil
}
