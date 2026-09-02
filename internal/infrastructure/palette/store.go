// Package palette 提供命令面板配置存储（quick-create-shortcut-support design D5：
// 平移 notify.LoadStore 模式）。
//
//   - 配置文件 <dataDir>/palette.json：临时文件 + 原子 rename、0600。
//   - Store：内存快照 + 写 mutex 串行化「校验 → 原子写 → 快照替换」。
//   - 启动加载不拒绝启动（文件损坏/不可读/字段非法 → 默认配置 + loadErr 降级）。
//   - 磁盘/HTTP JSON 为 camelCase 四键；领域 Config 无 json tag，由本包 wire DTO 映射。
package palette

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	domainpalette "ocdeck/internal/domain/palette"
)

const configFileName = "palette.json"

// FoldForMatch 与前端 foldForMatch（ECMAScript toLowerCase）折叠语义兼容的
// 规范实现：x/text cases.Lower(language.Und) 覆盖 İ → i\u0307 与 Greek
// final sigma（ΟΣ → ος）；strings.ToLower/EqualFold 两者均不满足，MUST NOT
// 使用（design D5）。domain 保持 stdlib-only，故实现在本层并注入
// Config.Validate。Caser 非并发安全，按调用创建。
func FoldForMatch(s string) string {
	return cases.Lower(language.Und).String(s)
}

func ConfigPath(dataDir string) string {
	return filepath.Join(dataDir, configFileName)
}

type snapshot struct {
	cfg     domainpalette.Config
	loadErr error
}

type Store struct {
	dataDir string
	mu      sync.Mutex
	cur     atomic.Pointer[snapshot]
}

type StoreState struct {
	Config  domainpalette.Config
	LoadErr error
}

func LoadStore(dataDir string) *Store {
	s := &Store{dataDir: dataDir}
	cfg, ok, err := loadConfigFile(dataDir)
	switch {
	case !ok && err == nil:
		s.cur.Store(&snapshot{cfg: domainpalette.DefaultConfig()})
	case err != nil:
		log.Printf("warning: palette config load failed for %s: %v", dataDir, err)
		s.cur.Store(&snapshot{cfg: domainpalette.DefaultConfig(), loadErr: err})
	default:
		if vErr := cfg.Validate(FoldForMatch); vErr != nil {
			log.Printf("warning: palette config invalid in %s: %v", dataDir, vErr)
			s.cur.Store(&snapshot{
				cfg:     domainpalette.DefaultConfig(),
				loadErr: fmt.Errorf("palette config invalid: %w", vErr),
			})
		} else {
			s.cur.Store(&snapshot{cfg: cfg})
		}
	}
	return s
}

func (s *Store) State() StoreState {
	sn := s.cur.Load()
	if sn == nil {
		return StoreState{}
	}
	return StoreState{Config: sn.cfg, LoadErr: sn.loadErr}
}

func (s *Store) Config() domainpalette.Config {
	if sn := s.cur.Load(); sn != nil {
		return sn.cfg
	}
	return domainpalette.Config{}
}

func (s *Store) LoadError() error {
	if sn := s.cur.Load(); sn != nil {
		return sn.loadErr
	}
	return nil
}

// Put 校验 → 原子写 → 快照替换。校验或写盘失败保持旧快照与旧文件。
func (s *Store) Put(incoming domainpalette.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := incoming.Validate(FoldForMatch); err != nil {
		return err
	}
	if err := saveConfigFile(s.dataDir, incoming); err != nil {
		return err
	}
	s.cur.Store(&snapshot{cfg: incoming})
	return nil
}

type diskConfig struct {
	Hotkey          string            `json:"hotkey"`
	TriggerWord     string            `json:"triggerWord"`
	MatchMode       string            `json:"matchMode"`
	CommandTriggers map[string]string `json:"commandTriggers"`
}

// DecodeConfig 仅按精确 camelCase 键名提取四键；未知键（含 PascalCase）忽略。
// 四键全必填（PUT 语义）；磁盘旧三键文件的缺键迁移见 decodeDiskConfig。
func DecodeConfig(data []byte) (domainpalette.Config, error) {
	cfg, hasTriggers, err := decodeConfig(data)
	if err != nil {
		return domainpalette.Config{}, err
	}
	if !hasTriggers {
		return domainpalette.Config{}, fmt.Errorf("palette config: missing required key %q", "commandTriggers")
	}
	return cfg, nil
}

// decodeDiskConfig 磁盘加载解码：commandTriggers 键存在但非法由 LoadStore 的
// Validate 走既有降级；键缺失（旧三键文件）MUST NOT 降级——复制默认词表，但与
// 旧 triggerWord fold 后相同的默认项置空（保自定义词，冲突指令暂不启用），
// 其余默认项保留（spec palette-config「命令面板配置存储」）。
func decodeDiskConfig(data []byte) (domainpalette.Config, error) {
	cfg, hasTriggers, err := decodeConfig(data)
	if err != nil {
		return domainpalette.Config{}, err
	}
	if !hasTriggers {
		cfg.CommandTriggers = migrateDefaultTriggers(cfg.TriggerWord)
	}
	return cfg, nil
}

// decodeConfig 精确键提取；第三返回值表示 commandTriggers 键是否存在且非 null。
func decodeConfig(data []byte) (domainpalette.Config, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return domainpalette.Config{}, false, err
	}
	hotkey, err := requiredString(raw, "hotkey")
	if err != nil {
		return domainpalette.Config{}, false, err
	}
	triggerWord, err := requiredString(raw, "triggerWord")
	if err != nil {
		return domainpalette.Config{}, false, err
	}
	matchMode, err := requiredString(raw, "matchMode")
	if err != nil {
		return domainpalette.Config{}, false, err
	}
	cfg := domainpalette.Config{
		Hotkey:      hotkey,
		TriggerWord: triggerWord,
		MatchMode:   matchMode,
	}
	v, ok := raw["commandTriggers"]
	if !ok {
		return cfg, false, nil
	}
	if string(v) == "null" {
		return domainpalette.Config{}, false, fmt.Errorf("palette config: missing required key %q", "commandTriggers")
	}
	var triggers map[string]string
	if err := json.Unmarshal(v, &triggers); err != nil {
		return domainpalette.Config{}, false, fmt.Errorf("palette config: commandTriggers: %w", err)
	}
	cfg.CommandTriggers = triggers
	return cfg, true, nil
}

// migrateDefaultTriggers 复制默认词表并把与 oldTriggerWord fold 冲突的默认项置空。
func migrateDefaultTriggers(oldTriggerWord string) map[string]string {
	triggers := maps.Clone(domainpalette.DefaultCommandTriggers())
	foldedOld := FoldForMatch(oldTriggerWord)
	for id, word := range triggers {
		if word != "" && FoldForMatch(word) == foldedOld {
			triggers[id] = ""
		}
	}
	return triggers
}

func requiredString(raw map[string]json.RawMessage, key string) (string, error) {
	v, ok := raw[key]
	if !ok {
		return "", fmt.Errorf("palette config: missing required key %q", key)
	}
	if string(v) == "null" {
		return "", fmt.Errorf("palette config: missing required key %q", key)
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", fmt.Errorf("palette config: %s: %w", key, err)
	}
	return s, nil
}

func loadConfigFile(dataDir string) (cfg domainpalette.Config, ok bool, err error) {
	path := ConfigPath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return domainpalette.Config{}, false, nil
		}
		return domainpalette.Config{}, false, fmt.Errorf("read palette config %s: %w", path, err)
	}
	c, err := decodeDiskConfig(data)
	if err != nil {
		return domainpalette.Config{}, false, fmt.Errorf("parse palette config %s: %w", path, err)
	}
	return c, true, nil
}

func saveConfigFile(dataDir string, cfg domainpalette.Config) error {
	path := ConfigPath(dataDir)
	data, err := json.MarshalIndent(diskConfig{
		Hotkey:          cfg.Hotkey,
		TriggerWord:     cfg.TriggerWord,
		MatchMode:       cfg.MatchMode,
		CommandTriggers: cfg.CommandTriggers,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal palette config: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("ensure data dir: %w", err)
	}
	tmp, err := os.CreateTemp(dataDir, ".palette-tmp-*")
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
