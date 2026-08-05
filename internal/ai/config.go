// Package ai 提供平台 LLM 集成底座：全局 provider 配置存储、通用 Completer 抽象、
// 以及第一个 adapter（分支命名 SlugNamer）。
//
// 设计来源：openspec/changes/ai-worktree-naming/design.md（D1/D2/D7）。
//
// - 配置文件 <dataDir>/ai.json：provider/api_key/base_url/model，临时文件+原子 rename、0600。
// - Store：内存快照 + 写 mutex 串行化，启动加载不拒绝启动。
// - Completer：net/http 自实现薄 client，失败一律 error 不重试，调用方各自回退。
package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Provider 枚举（design.md D1）。
type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
)

// ProviderDefaultBaseURL 返回 provider 默认端点（design.md D2 契约表）。
func ProviderDefaultBaseURL(p Provider) string {
	switch p {
	case ProviderOpenAI:
		return "https://api.openai.com"
	case ProviderAnthropic:
		return "https://api.anthropic.com"
	default:
		return ""
	}
}

// ProviderConfig 为 ai.json 的内存结构（design.md D1）。
// JSON tag 与磁盘文件字段一一对应；api_key 仅在内存与 0600 文件中存在，MUST NOT 进日志。
//
// Thinking 为可选枚举（design.md D2 思考强度映射）："" | off | low | medium | high。
// 空串表示不下发任何思考参数（跟随模型/网关默认，即现状行为）。
type ProviderConfig struct {
	Provider Provider `json:"provider"`
	APIKey   string   `json:"api_key"`
	BaseURL  string   `json:"base_url"` // 空 = provider 默认端点
	Model    string   `json:"model"`    // 必填非空
	Thinking string   `json:"thinking"` // 可选枚举，空 = 不下发思考参数
}

// Thinking 枚举常量（design.md D2 思考强度映射表）。
const (
	ThinkingOff    = "off"
	ThinkingLow    = "low"
	ThinkingMedium = "medium"
	ThinkingHigh   = "high"
)

// ThinkingBudgetTokens Anthropic 各档位 budget_tokens（design.md D2 映射表）。
// low=1024, medium=4096, high=16384。
var thinkingBudgetTokens = map[string]int{
	ThinkingLow:    1024,
	ThinkingMedium: 4096,
	ThinkingHigh:   16384,
}

// AnthropicThinkingBudget 返回 Anthropic 协议下 thinking 档位对应的 budget_tokens。
// 仅对 low/medium/high 有意义；off 走 disabled 路径无 budget。
func AnthropicThinkingBudget(thinking string) (int, bool) {
	b, ok := thinkingBudgetTokens[thinking]
	return b, ok
}

// configFileName 配置文件名（相对 dataDir）。
const configFileName = "ai.json"

// ConfigPath 返回 dataDir 下配置文件绝对路径（供测试与诊断）。
func ConfigPath(dataDir string) string {
	return filepath.Join(dataDir, configFileName)
}

// Validate 校验配置字段（design.md D1 + spec）：
//   - provider 必须为枚举值；
//   - model 必填非空（仅去首尾空白后判空）；
//   - base_url 可空；非空时必须为合法 http(s) URL；
//   - thinking 可空或枚举值（"" | off | low | medium | high）。
//
// 不校验 api_key 是否非空（configured 判定由 Store 负责，配置文件本身允许 key 缺失）。
func (c ProviderConfig) Validate() error {
	switch c.Provider {
	case ProviderOpenAI, ProviderAnthropic:
	default:
		return fmt.Errorf("invalid provider %q (must be openai|anthropic)", c.Provider)
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("model must not be empty")
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("invalid base_url %q (must be http(s) URL)", c.BaseURL)
		}
	}
	if err := validateThinking(c.Thinking); err != nil {
		return err
	}
	return nil
}

// validateThinking 校验 thinking 为合法枚举值。
func validateThinking(thinking string) error {
	switch thinking {
	case "", ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh:
		return nil
	default:
		return fmt.Errorf("invalid thinking %q (must be off|low|medium|high or empty)", thinking)
	}
}

// EffectiveBaseURL 返回归一化后的 base URL：空 → provider 默认端点；否则去尾 '/'。
func (c ProviderConfig) EffectiveBaseURL() string {
	if c.BaseURL == "" {
		return ProviderDefaultBaseURL(c.Provider)
	}
	return strings.TrimRight(c.BaseURL, "/")
}

// loadConfigFile 读取并解析 <dataDir>/ai.json。
// 文件不存在 → 返回 ok=false（正常未配置态），err=nil。
// JSON 损坏/读取失败 → ok=false + err（loadErr）。
func loadConfigFile(dataDir string) (cfg ProviderConfig, ok bool, err error) {
	path := ConfigPath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProviderConfig{}, false, nil
		}
		return ProviderConfig{}, false, fmt.Errorf("read ai config %s: %w", path, err)
	}
	var c ProviderConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return ProviderConfig{}, false, fmt.Errorf("parse ai config %s: %w", path, err)
	}
	return c, true, nil
}

// saveConfigFile 原子写入 ai.json，权限 0600（design.md D1）。
// 调用方负责字段校验。
func saveConfigFile(dataDir string, cfg ProviderConfig) error {
	path := ConfigPath(dataDir)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal ai config: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("ensure data dir: %w", err)
	}
	tmp, err := os.CreateTemp(dataDir, ".ai-config-tmp-*")
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

// --- Store（design.md D7） ---

// snapshot 不可变快照。Store 通过 atomic.Pointer[snapshot] 存指针。
type snapshot struct {
	cfg        ProviderConfig
	configured bool
	loadErr    error
}

// String 仅供测试/日志诊断用；MUST NOT 包含 api_key。
func (sn *snapshot) String() string {
	if sn == nil {
		return "<nil snapshot>"
	}
	return fmt.Sprintf("snapshot{provider=%s model=%s configured=%v loadErr=%v}",
		sn.cfg.Provider, sn.cfg.Model, sn.configured, sn.loadErr)
}

// Store 维护 AI 配置内存快照 + 文件持久化（design.md D7）。
//
// 语义：
//   - 启动 LoadStore：文件不存在=正常未配置；损坏=configured=false + loadErr + 日志告警，不拒绝启动。
//   - 读：atomic 加载快照，单次 LLM 操作全程用同一快照。
//   - 写：写 mutex 串行「读旧 key 合并 → 校验 → 原子 rename → 快照替换」；
//     文件写入失败保持旧快照并返回 error。并发 PUT 按锁序 last-writer-wins，
//     内存与磁盘最终一致。
//
// configured = provider 合法 && api_key 非空 && model 非空 && loadErr==nil。
type Store struct {
	dataDir string
	mu      sync.Mutex // 串行写
	cur     atomic.Pointer[snapshot]
}

// LoadStore 启动加载 AI 配置（design.md D7）。
//
// 文件不存在 → configured=false，loadErr=nil（正常未配置）。
// 文件损坏/不可读 → configured=false，loadErr 非 nil，日志告警，**不返回 error、不拒绝启动**。
// 文件正常 → 校验后构建快照；若字段非法也视为 loadErr（保持启动）。
func LoadStore(dataDir string) *Store {
	s := &Store{dataDir: dataDir}
	cfg, ok, err := loadConfigFile(dataDir)
	switch {
	case !ok && err == nil:
		// 文件不存在：正常未配置。
		s.cur.Store(&snapshot{})
	case err != nil:
		// 损坏/不可读：记录 loadErr，日志告警，不拒绝启动。
		log.Printf("warning: ai config load failed for %s: %v", dataDir, err)
		s.cur.Store(&snapshot{loadErr: err})
	default:
		// 文件存在且可解析：构建可用快照。字段非法时同样降级为 loadErr。
		if vErr := cfg.Validate(); vErr != nil {
			log.Printf("warning: ai config invalid in %s: %v", dataDir, vErr)
			s.cur.Store(&snapshot{loadErr: fmt.Errorf("ai config invalid: %w", vErr)})
		} else {
			s.cur.Store(buildSnapshot(cfg, nil))
		}
	}
	return s
}

// Snapshot 返回当前快照（不可变，读方单次操作全程复用）。
func (s *Store) Snapshot() *snapshot {
	return s.cur.Load()
}

// StoreState 为外部包（如 api handler）提供的快照只读视图。
// 一次 LLM 操作或一次 HTTP 响应构造 MUST 全程使用同一 StoreState（来自单次 State() 调用），
// 避免并发 PUT 下字段混配。
type StoreState struct {
	CFG        ProviderConfig
	Configured bool
	LoadErr    error
}

// State 返回当前快照的一致只读视图（design.md D7：读方单次操作全程复用同一快照）。
// 调用方应在构造响应/调用 LLM 时保存返回值，不要分多次调用 Config()/Configured()/LoadError()
// 造成字段混配。
func (s *Store) State() StoreState {
	sn := s.cur.Load()
	if sn == nil {
		return StoreState{}
	}
	return StoreState{CFG: sn.cfg, Configured: sn.configured, LoadErr: sn.loadErr}
}

// Config 返回当前快照中的 ProviderConfig（即便未配置也返回零值）。
func (s *Store) Config() ProviderConfig {
	if sn := s.cur.Load(); sn != nil {
		return sn.cfg
	}
	return ProviderConfig{}
}

// Configured 返回当前可用性判定。
func (s *Store) Configured() bool {
	if sn := s.cur.Load(); sn != nil {
		return sn.configured
	}
	return false
}

// LoadError 返回当前快照的 load_error（nil 表示无）。
func (s *Store) LoadError() error {
	if sn := s.cur.Load(); sn != nil {
		return sn.loadErr
	}
	return nil
}

// Put 保存配置（design.md D7）：读旧 key 合并 → 校验 → 原子写文件 → 快照替换。
//
// 当 incoming.APIKey 为空或掩码值（含 "***"）时，保留已存储的旧 key；
// 此时若旧快照不可用或旧 key 为空 → 返回错误（configured 永远要求 key 非空）。
//
// 写文件失败时保持旧快照不变并返回 error。整个流程在写锁内串行，
// 并发 PUT 按锁获得顺序 last-writer-wins，内存与磁盘最终一致。
func (s *Store) Put(incoming ProviderConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.cur.Load()
	merged := incoming

	// 合并 api_key：空或掩码值保留旧 key。
	if merged.APIKey == "" || strings.Contains(merged.APIKey, "***") {
		merged.APIKey = prevKeyForMerge(prev)
	}

	// 校验。注意：Validate 不校验 api_key 非空，configured 要求 key 非空，故在此显式检查。
	if err := merged.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(merged.APIKey) == "" {
		return errors.New("api_key must not be empty")
	}

	// 写文件：失败保持旧快照不变。
	if err := saveConfigFile(s.dataDir, merged); err != nil {
		return err
	}

	// 替换快照。loadErr 在写成功后清零。
	s.cur.Store(buildSnapshot(merged, nil))
	return nil
}

// prevKeyForMerge 从旧快照提取 key 用于合并。旧快照 nil 或字段非法时返回空。
func prevKeyForMerge(prev *snapshot) string {
	if prev == nil {
		return ""
	}
	return prev.cfg.APIKey
}

// buildSnapshot 由已校验通过的 cfg 构建可用快照。
// err 为可选的 loadErr（写成功后传 nil）。
func buildSnapshot(cfg ProviderConfig, err error) *snapshot {
	configured := err == nil &&
		(cfg.Provider == ProviderOpenAI || cfg.Provider == ProviderAnthropic) &&
		strings.TrimSpace(cfg.APIKey) != "" &&
		strings.TrimSpace(cfg.Model) != ""
	return &snapshot{cfg: cfg, configured: configured, loadErr: err}
}