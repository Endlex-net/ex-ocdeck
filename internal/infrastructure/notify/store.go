// Package notify 提供通知配置存储（task-notifications design D5：ai-provider-config
// LoadStore 模式平移，参考 internal/infrastructure/ai/config.go）。
//
//   - 配置文件 <dataDir>/notification.json：临时文件 + 原子 rename、0600。
//   - Store：内存快照 + 写 mutex 串行化「token 合并 → 校验 → 原子写 → 快照替换」；
//     启动加载不拒绝启动（文件损坏/非法 → 默认配置 + load_error 降级）。
//   - 行为唯一表述在 spec「通知配置存储 / 读写 API / 配置运行时生效」。
//   - bark token 仅在内存与 0600 文件中存在，MUST NOT 明文进日志或 API 响应
//     （掩码经 MaskToken）。
package notify

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"ocdeck/internal/domain/notification"
)

// configFileName 配置文件名（相对 dataDir）。
const configFileName = "notification.json"

// ConfigPath 返回 dataDir 下配置文件绝对路径（供测试与诊断）。
func ConfigPath(dataDir string) string {
	return filepath.Join(dataDir, configFileName)
}

// snapshot 不可变快照。Store 通过 atomic.Pointer[snapshot] 存指针。
type snapshot struct {
	cfg     notification.Config
	loadErr error
}

// Store 维护通知配置内存快照 + 文件持久化（design D5）。
type Store struct {
	dataDir string
	mu      sync.Mutex // 串行写
	cur     atomic.Pointer[snapshot]
}

// StoreState 快照只读视图。一次投递判定或一次 HTTP 响应构造 MUST 全程使用同一
// State() 返回值，避免并发 PUT 下字段混配（ai-provider-config StoreState 先例）。
type StoreState struct {
	Config  notification.Config
	LoadErr error
}

// LoadStore 启动加载通知配置。
//
// 文件不存在 → 默认配置、loadErr=nil（正常未配置态）。
// 文件损坏/不可读/校验非法 → 默认配置 + loadErr + 日志告警，不返回 error、
// 不拒绝启动（spec「配置运行时生效」：通知按默认配置运行）。
// 字段缺失/null 由 DecodeConfig 直接判非法（spec 磁盘 schema 全键必填）→ loadErr，fail-safe。
func LoadStore(dataDir string) *Store {
	s := &Store{dataDir: dataDir}
	cfg, ok, err := loadConfigFile(dataDir)
	switch {
	case !ok && err == nil:
		s.cur.Store(&snapshot{cfg: notification.DefaultConfig()})
	case err != nil:
		log.Printf("warning: notification config load failed for %s: %v", dataDir, err)
		s.cur.Store(&snapshot{cfg: notification.DefaultConfig(), loadErr: err})
	default:
		if vErr := cfg.Validate(); vErr != nil {
			log.Printf("warning: notification config invalid in %s: %v", dataDir, vErr)
			s.cur.Store(&snapshot{
				cfg:     notification.DefaultConfig(),
				loadErr: fmt.Errorf("notification config invalid: %w", vErr),
			})
		} else {
			s.cur.Store(&snapshot{cfg: cfg})
		}
	}
	return s
}

// State 返回当前快照的一致只读视图。
func (s *Store) State() StoreState {
	sn := s.cur.Load()
	if sn == nil {
		return StoreState{}
	}
	return StoreState{Config: sn.cfg, LoadErr: sn.loadErr}
}

// Config 返回当前快照中的配置（未配置/损坏时为默认配置）。
func (s *Store) Config() notification.Config {
	if sn := s.cur.Load(); sn != nil {
		return sn.cfg
	}
	return notification.Config{}
}

// LoadError 返回当前快照的 load_error（nil 表示无）。
func (s *Store) LoadError() error {
	if sn := s.cur.Load(); sn != nil {
		return sn.loadErr
	}
	return nil
}

// Put 保存配置：bark token 与 wecom url 合并（空串或含 "***" 的掩码值保留
// 已存储原值）→ 校验 → 原子写文件 → 快照替换。校验失败或写文件失败返回 error 并
// 保持旧快照不变。全过程在写锁内串行，并发 PUT 按锁获得顺序 last-writer-wins，
// 内存与磁盘最终一致。旧快照为 loadErr 态（损坏不可读）时无已存储值可合并，按空
// 处理、不因此拒绝保存（spec「通知配置读写 API」token/url 语义）。
// 校验失败返回 *ConfigValidationError（handler 据此 422），写文件失败保持普通 error。
func (s *Store) Put(incoming notification.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.cur.Load()
	merged := incoming
	if tok := merged.Channels.Bark.Token; tok == "" || strings.Contains(tok, "***") {
		merged.Channels.Bark.Token = prevStoredToken(prev)
	}
	if u := merged.Channels.Wecom.URL; u == "" || strings.Contains(u, "***") {
		merged.Channels.Wecom.URL = prevStoredWecomURL(prev)
	}
	if err := merged.Validate(); err != nil {
		return &ConfigValidationError{Err: err}
	}
	if err := saveConfigFile(s.dataDir, merged); err != nil {
		return err
	}
	// 写成功后 loadErr 清零。
	s.cur.Store(&snapshot{cfg: merged})
	return nil
}

// ConfigValidationError Put 路径业务校验失败的可识别包装（spec「通知配置读写 API」：
// handler MUST 能区分校验失败 422 与写盘失败 500）。Error() 委托内层校验文案，
// 原样回传 422 body；Unwrap 支持 errors.Is/As 解包。
type ConfigValidationError struct{ Err error }

func (e *ConfigValidationError) Error() string { return e.Err.Error() }
func (e *ConfigValidationError) Unwrap() error { return e.Err }

// prevStoredToken 从旧快照提取 bark token 用于合并；旧快照为 loadErr 态时
// 损坏文件不可信，按无已存储 token 处理。
func prevStoredToken(prev *snapshot) string {
	if prev == nil || prev.loadErr != nil {
		return ""
	}
	return prev.cfg.Channels.Bark.Token
}

// prevStoredWecomURL 从旧快照提取 wecom webhook URL 用于合并；旧快照为 loadErr
// 态时损坏文件不可信，按无已存储 URL 处理。
func prevStoredWecomURL(prev *snapshot) string {
	if prev == nil || prev.loadErr != nil {
		return ""
	}
	return prev.cfg.Channels.Wecom.URL
}

// loadConfigFile 读取并解码 <dataDir>/notification.json。文件不存在 →
// (zero, false, nil)；读取失败/JSON 损坏/必填键缺失或为 null/类型不匹配 →
// (zero, false, err)（调用方按损坏降级处理）。未知字段忽略。
func loadConfigFile(dataDir string) (cfg notification.Config, ok bool, err error) {
	path := ConfigPath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return notification.Config{}, false, nil
		}
		return notification.Config{}, false, fmt.Errorf("read notification config %s: %w", path, err)
	}
	c, err := DecodeConfig(data)
	if err != nil {
		return notification.Config{}, false, fmt.Errorf("parse notification config %s: %w", path, err)
	}
	return c, true, nil
}

// --- 磁盘 schema 必填键解码（spec「通知配置存储」：字段均为必填键） ---

// wireConfig 指针型 wire DTO：缺键与 JSON null 均得到 nil 指针，二者同按
// 「必填键非法」处理（磁盘加载与 PUT 请求体共用本规则）。未知字段忽略
// （不使用 DisallowUnknownFields，对齐既有 decodeJSON 惯例）。
type wireConfig struct {
	Enabled            *bool           `json:"enabled"`
	Categories         *wireCategories `json:"categories"`
	IdleTimeoutSeconds *int            `json:"idle_timeout_seconds"`
	Channels           *wireChannels   `json:"channels"`
	LLMSummary         *bool           `json:"llm_summary"`
	BaseURL            *string         `json:"base_url"`
}

type wireCategories struct {
	Question   *bool `json:"question"`
	Permission *bool `json:"permission"`
	Idle       *bool `json:"idle"`
	Retry      *bool `json:"retry"`
	Error      *bool `json:"error"`
}

type wireChannels struct {
	Web   *wireWeb   `json:"web"`
	Bark  *wireBark  `json:"bark"`
	Macos *wireMacos `json:"macos"`
	Wecom *wireWecom `json:"wecom"`
}

type wireWeb struct {
	Enabled *bool `json:"enabled"`
}

type wireBark struct {
	Enabled  *bool   `json:"enabled"`
	Endpoint *string `json:"endpoint"`
	Token    *string `json:"token"`
}

type wireMacos struct {
	Enabled *bool `json:"enabled"`
}

// wireWecom wecom 渠道 wire DTO（spec「通知配置存储」兼容规则）：对象缺失或为
// null → enabled=false、url=""；对象在场时嵌套键缺失或 null 各自独立取默认
// （enabled 缺/null → false；url 缺/null → ""）。因此 wecom 不列入必填 checks，
// 缺键填充在 toConfig 组装处完成。类型不匹配仍随 json.Unmarshal 失败 → 损坏。
type wireWecom struct {
	Enabled *bool   `json:"enabled"`
	URL     *string `json:"url"`
}

// DecodeConfig 从 JSON 字节解码通知配置，强制磁盘 schema 的必填键存在性（缺键或
// null 均为错误；类型不匹配随 json 解码报错；未知字段忽略）。供 LoadStore 磁盘
// 加载与 PUT 请求体解码（Lane D，缺键/null → 422）复用同一规则。
func DecodeConfig(data []byte) (notification.Config, error) {
	var w wireConfig
	if err := json.Unmarshal(data, &w); err != nil {
		return notification.Config{}, err
	}
	return w.toConfig()
}

// toConfig 按声明顺序逐一检查必填键（首个缺失即报错，信息确定），全部在场后
// 解引用组装 notification.Config。
func (w *wireConfig) toConfig() (notification.Config, error) {
	checks := []struct {
		key string
		ok  bool
	}{
		{"enabled", w.Enabled != nil},
		{"categories", w.Categories != nil},
		{"categories.question", w.Categories != nil && w.Categories.Question != nil},
		{"categories.permission", w.Categories != nil && w.Categories.Permission != nil},
		{"categories.idle", w.Categories != nil && w.Categories.Idle != nil},
		{"categories.retry", w.Categories != nil && w.Categories.Retry != nil},
		{"categories.error", w.Categories != nil && w.Categories.Error != nil},
		{"idle_timeout_seconds", w.IdleTimeoutSeconds != nil},
		{"channels", w.Channels != nil},
		{"channels.web", w.Channels != nil && w.Channels.Web != nil},
		{"channels.web.enabled", w.Channels != nil && w.Channels.Web != nil && w.Channels.Web.Enabled != nil},
		{"channels.bark", w.Channels != nil && w.Channels.Bark != nil},
		{"channels.bark.enabled", w.Channels != nil && w.Channels.Bark != nil && w.Channels.Bark.Enabled != nil},
		{"channels.bark.endpoint", w.Channels != nil && w.Channels.Bark != nil && w.Channels.Bark.Endpoint != nil},
		{"channels.bark.token", w.Channels != nil && w.Channels.Bark != nil && w.Channels.Bark.Token != nil},
		{"channels.macos", w.Channels != nil && w.Channels.Macos != nil},
		{"channels.macos.enabled", w.Channels != nil && w.Channels.Macos != nil && w.Channels.Macos.Enabled != nil},
		{"llm_summary", w.LLMSummary != nil},
		{"base_url", w.BaseURL != nil},
	}
	for _, c := range checks {
		if !c.ok {
			return notification.Config{}, fmt.Errorf("notification config: missing required key %q", c.key)
		}
	}
	return notification.Config{
		Enabled: *w.Enabled,
		Categories: notification.Categories{
			Question:   *w.Categories.Question,
			Permission: *w.Categories.Permission,
			Idle:       *w.Categories.Idle,
			Retry:      *w.Categories.Retry,
			Error:      *w.Categories.Error,
		},
		IdleTimeoutSeconds: *w.IdleTimeoutSeconds,
		Channels: notification.ChannelsConfig{
			Web:   notification.WebChannelConfig{Enabled: *w.Channels.Web.Enabled},
			Bark:  notification.BarkChannelConfig{Enabled: *w.Channels.Bark.Enabled, Endpoint: *w.Channels.Bark.Endpoint, Token: *w.Channels.Bark.Token},
			Macos: notification.MacosChannelConfig{Enabled: *w.Channels.Macos.Enabled},
			Wecom: buildWecomChannelConfig(w.Channels.Wecom),
		},
		LLMSummary: *w.LLMSummary,
		BaseURL:    *w.BaseURL,
	}, nil
}

// buildWecomChannelConfig 按 spec「通知配置存储」兼容规则填充 wecom 配置：
// 对象缺失或为 nil → 两字段默认（enabled=false、url=""）；对象在场时嵌套键
// 缺失或为 nil 各自独立取默认。已校验非 nil 的字段直接解引用。
func buildWecomChannelConfig(w *wireWecom) notification.WecomChannelConfig {
	if w == nil {
		return notification.WecomChannelConfig{}
	}
	cfg := notification.WecomChannelConfig{}
	if w.Enabled != nil {
		cfg.Enabled = *w.Enabled
	}
	if w.URL != nil {
		cfg.URL = *w.URL
	}
	return cfg
}

// saveConfigFile 原子写入 notification.json，权限 0600（spec「通知配置存储」）。
// 调用方负责字段校验。
func saveConfigFile(dataDir string, cfg notification.Config) error {
	path := ConfigPath(dataDir)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal notification config: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("ensure data dir: %w", err)
	}
	tmp, err := os.CreateTemp(dataDir, ".notification-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

// MaskToken bark token 掩码规则（spec「通知配置读写 API」，与 ai-provider-config
// 的 api_key 一致）：len ≥ 8 → 前 4 位 + `***`；len < 8 → 纯 `***`；无 token → 空串。
// 响应中 MUST NOT 出现完整 token。
func MaskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) < 8 {
		return "***"
	}
	return token[:4] + "***"
}

// MaskWecomURL wecom webhook URL 掩码规则（spec「通知配置读写 API」：无 URL 为
// 空串；非空 URL MUST 固定为 `***`）。MUST NOT 复用 MaskToken（其回显前 4 字符
// 会泄漏 `http`/`https` 前缀）。完整 webhook URL 整体按密钥保护，MUST NOT 回显
// 任何原文片段。
func MaskWecomURL(url string) string {
	if url == "" {
		return ""
	}
	return "***"
}
