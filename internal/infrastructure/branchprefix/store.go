// Package branchprefix 提供全局 worktree 分支前缀配置存储（openspec change
// task-info-editable D4，语义逐条镜像 palette.Store：内存快照 + 写 mutex 串行化
// 「校验 → 原子写 → 快照替换」；启动加载不拒绝启动，损坏降级缺省值 + loadErr）。
//
//   - 配置文件 <dataDir>/branch-prefix.json：临时文件 + 原子 rename、0600。
//   - 结构 {"prefix": "<string>"}；未配置缺省 ocdeck；前缀不含 / 分隔符。
//   - 保存校验为原值匹配（含首尾空白即非法，不做 trim 容错）。
package branchprefix

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
)

const configFileName = "branch-prefix.json"

// DefaultPrefix 缺省前缀（未配置 / 配置文件损坏降级时生效）。
const DefaultPrefix = "ocdeck"

// prefixPattern 前缀合法性：原值匹配（含首尾空白即非法）；长度 1..50。
var prefixPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`)

// Validate 校验前缀合法性（保存与磁盘加载共用；失败返回描述性错误）。
func Validate(prefix string) error {
	if !prefixPattern.MatchString(prefix) {
		return fmt.Errorf("branch prefix %q must match ^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$", prefix)
	}
	return nil
}

func ConfigPath(dataDir string) string {
	return filepath.Join(dataDir, configFileName)
}

type snapshot struct {
	prefix  string
	loadErr error
}

type Store struct {
	dataDir string
	mu      sync.Mutex
	cur     atomic.Pointer[snapshot]
}

// StoreState 暴露当前生效值与启动加载错误（可观察降级，测试断言用）。
type StoreState struct {
	Prefix  string
	LoadErr error
}

// LoadStore 加载配置：未配置 → 缺省值；损坏（JSON 非法 / 键缺失 / 非字符串 / 值非法）→
// 降级缺省值 + 可观察加载错误（日志）。MUST NOT 启动失败。
func LoadStore(dataDir string) *Store {
	s := &Store{dataDir: dataDir}
	prefix, ok, err := loadConfigFile(dataDir)
	switch {
	case !ok && err == nil:
		s.cur.Store(&snapshot{prefix: DefaultPrefix})
	case err != nil:
		log.Printf("warning: branch prefix config load failed for %s: %v", dataDir, err)
		s.cur.Store(&snapshot{prefix: DefaultPrefix, loadErr: err})
	default:
		s.cur.Store(&snapshot{prefix: prefix})
	}
	return s
}

func (s *Store) State() StoreState {
	sn := s.cur.Load()
	if sn == nil {
		return StoreState{Prefix: DefaultPrefix}
	}
	return StoreState{Prefix: sn.prefix, LoadErr: sn.loadErr}
}

// Prefix 返回当前生效前缀（未配置/损坏降级后为 DefaultPrefix）。
func (s *Store) Prefix() string {
	if sn := s.cur.Load(); sn != nil && sn.prefix != "" {
		return sn.prefix
	}
	return DefaultPrefix
}

func (s *Store) LoadError() error {
	if sn := s.cur.Load(); sn != nil {
		return sn.loadErr
	}
	return nil
}

// Put 校验 → 原子写 → 快照替换。校验或写盘失败保持旧快照与旧文件。
func (s *Store) Put(prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := Validate(prefix); err != nil {
		return err
	}
	if err := saveConfigFile(s.dataDir, prefix); err != nil {
		return err
	}
	s.cur.Store(&snapshot{prefix: prefix})
	return nil
}

// diskConfig 磁盘/HTTP 形状（{"prefix": string}）。
type diskConfig struct {
	Prefix string `json:"prefix"`
}

// DecodeConfig 严格单键提取：顶层必须为对象、prefix 键必须存在且为字符串
// （缺失/null/类型错误均为错误；空串交由 Validate 拒绝）。未知键忽略。
func DecodeConfig(data []byte) (string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("branch prefix config: %w", err)
	}
	v, ok := raw["prefix"]
	if !ok {
		return "", fmt.Errorf("branch prefix config: missing required key %q", "prefix")
	}
	if string(v) == "null" {
		return "", fmt.Errorf("branch prefix config: missing required key %q", "prefix")
	}
	var prefix string
	if err := json.Unmarshal(v, &prefix); err != nil {
		return "", fmt.Errorf("branch prefix config: prefix: %w", err)
	}
	return prefix, nil
}

func loadConfigFile(dataDir string) (prefix string, ok bool, err error) {
	path := ConfigPath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read branch prefix config %s: %w", path, err)
	}
	p, err := DecodeConfig(data)
	if err != nil {
		return "", false, fmt.Errorf("parse branch prefix config %s: %w", path, err)
	}
	if vErr := Validate(p); vErr != nil {
		return "", false, fmt.Errorf("branch prefix config %s invalid: %w", path, vErr)
	}
	return p, true, nil
}

func saveConfigFile(dataDir, prefix string) error {
	path := ConfigPath(dataDir)
	data, err := json.MarshalIndent(diskConfig{Prefix: prefix}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal branch prefix config: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("ensure data dir: %w", err)
	}
	tmp, err := os.CreateTemp(dataDir, ".branch-prefix-tmp-*")
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
