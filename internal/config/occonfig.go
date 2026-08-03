// Package config 全局配置管理：~/.config/opencode/ 下 *.json 与 *.jsonc 文件的
// 列表/读取/保存（design.md §13 + global-config-management spec）。
//
// 保存语义：
//   - 按扩展名校验语法：.json 严格 JSON；.jsonc 容忍注释与尾逗号（剥离后校验，不引入第三方库）。
//   - mtime+hash 乐观并发：不匹配返回 ErrConfigConflict。
//   - 写入前 .bak 备份（保留最近一次）：.bak 与主文件同策略（临时文件 + 原子 rename + 保留权限）；
//     既有 .bak 为 symlink 时先 remove 再原子重建普通文件（无 swap 窗口）。
//   - 拒绝路径逃逸与 symlink（主文件 .bak 均不跟随 symlink）。
//   - name 仅文件名，不允许路径分隔符。
//
// JSONC 解析：维持自研单次扫描状态机（stripJSONC），不引入第三方依赖。
//   理由：go 1.24 兼容 + 零依赖策略；仅用于语法校验（不影响保存原文），状态机覆盖
//   行/块注释、字符串字面量内 // 与 /* 保留、尾逗号、未闭合块注释拒绝。
//   测试覆盖清单见 occonfig_test.go：TestStripJSONC_UnclosedBlockCommentRejected、
//   TestStripJSONC_StringLiteralsPreserved、TestStripJSONC_TrailingCommaStripped、
//   TestStripJSONC_LinearTime（大文件单次扫描线性时间）。
//
// 不做业务 schema 校验（配置语义由 opencode 自己负责）；编辑保留原文（含注释原样保存）。
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrConfigConflict 乐观并发冲突：文件已被外部修改（mtime+hash 不匹配）。
var ErrConfigConflict = errors.New("oc-config: file changed externally")

// ErrConfigNotFound 配置文件不存在。
var ErrConfigNotFound = errors.New("oc-config: file not found")

// ErrInvalidName 文件名非法（含路径分隔符或为空）。
var ErrInvalidName = errors.New("oc-config: invalid file name")

// ErrInvalidSyntax 语法校验失败（.json 严格 JSON 或 .jsonc 剥离注释/尾逗号后非法）。
var ErrInvalidSyntax = errors.New("oc-config: invalid syntax")

// OCConfigName 配置文件名（仅文件名，不含路径）。
type OCConfigName struct {
	Name string `json:"name"`
}

// OCConfigContent 配置文件内容（读取/保存 DTO）。
type OCConfigContent struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Mtime   int64  `json:"mtime"`
	Hash    string `json:"hash"`
}

// OCConfigManager 管理某目录下 opencode 全局配置文件（默认 ~/.config/opencode/）。
// dir 由构造方注入，便于测试。
type OCConfigManager struct {
	dir string
}

// NewOCConfigManager 构造指向 dir 的配置管理器。dir 应为 opencode 全局配置目录。
func NewOCConfigManager(dir string) *OCConfigManager {
	return &OCConfigManager{dir: dir}
}

// DefaultOCConfigDir 返回 ~/.config/opencode/（design.md §13）。
// 解析 Home 失败时返回空串，调用方据此拒绝。
func DefaultOCConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for oc-config dir: %w", err)
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

// List 列出 dir 下全部 *.json 与 *.jsonc 文件（仅文件名，按字母序，design.md §13）。
func (m *OCConfigManager) List() ([]OCConfigName, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []OCConfigName{}, nil
		}
		return nil, fmt.Errorf("list oc-config dir %s: %w", m.dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isOCConfigFile(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]OCConfigName, 0, len(names))
	for _, n := range names {
		out = append(out, OCConfigName{Name: n})
	}
	return out, nil
}

// Read 返回配置文件全文 + mtime + content hash（design.md §13）。
// hash 为 content 的 sha256 hex；mtime 为文件修改时间（Unix 秒）。
func (m *OCConfigManager) Read(name string) (OCConfigContent, error) {
	if err := validateConfigName(name); err != nil {
		return OCConfigContent{}, err
	}
	path := m.resolvePath(name)
	// 拒绝 symlink：配置文件 MUST 为普通文件，防止 symlink 逃逸到任意路径。
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OCConfigContent{}, ErrConfigNotFound
		}
		return OCConfigContent{}, fmt.Errorf("stat oc-config %s: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return OCConfigContent{}, fmt.Errorf("oc-config %s: symlink not allowed", name)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return OCConfigContent{}, fmt.Errorf("read oc-config %s: %w", name, err)
	}
	return OCConfigContent{
		Name:    name,
		Content: string(content),
		Mtime:   info.ModTime().Unix(),
		Hash:    hashContent(content),
	}, nil
}

// Save 保存配置文件（design.md §13 + global-config-management spec）。
// 步骤：name 校验 → 语法校验（按扩展名） → 读现有文件做乐观并发比对 → .bak 备份
// → 临时文件原子 rename + 保留原权限 → 返回新 mtime+hash。
//
// 乐观并发：expectedMtime+expectedHash 必须与磁盘当前值一致，否则 ErrConfigConflict。
// 文件不存在时（首次创建）expectedMtime=0 且 expectedHash="" 视为一致。
func (m *OCConfigManager) Save(name, content string, expectedMtime int64, expectedHash string) (OCConfigContent, error) {
	if err := validateConfigName(name); err != nil {
		return OCConfigContent{}, err
	}
	if !isOCConfigFile(name) {
		return OCConfigContent{}, fmt.Errorf("oc-config %s: %w (only .json/.jsonc)", name, ErrInvalidName)
	}
	// 语法校验（按扩展名分流）。
	if err := validateConfigSyntax(name, content); err != nil {
		return OCConfigContent{}, err
	}
	path := m.resolvePath(name)

	// 乐观并发：读现有文件 + mtime + hash。
	info, statErr := os.Lstat(path)
	existing := []byte(nil)
	switch {
	case statErr == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return OCConfigContent{}, fmt.Errorf("oc-config %s: symlink not allowed", name)
		}
		cur, rerr := os.ReadFile(path)
		if rerr != nil {
			return OCConfigContent{}, fmt.Errorf("read existing oc-config %s: %w", name, rerr)
		}
		existing = cur
		curMtime := info.ModTime().Unix()
		curHash := hashContent(cur)
		if curMtime != expectedMtime || curHash != expectedHash {
			return OCConfigContent{}, ErrConfigConflict
		}
	case errors.Is(statErr, os.ErrNotExist):
		// 首次创建：expected 必须为 0/""。
		if expectedMtime != 0 || expectedHash != "" {
			return OCConfigContent{}, ErrConfigConflict
		}
	default:
		return OCConfigContent{}, fmt.Errorf("stat oc-config %s: %w", name, statErr)
	}

	// .bak 备份（保留最近一次）：仅在原文件存在时备份。.bak 与主文件同策略：
	// 临时文件 + 原子 rename + 保留权限。若既有 .bak 为 symlink，先 remove（remove
	// 本身不跟随 symlink，安全）再经临时文件原子 rename 重建普通文件——无 swap 窗口，
	// 既中和 symlink 攻击又保留备份功能。目标文件本身的 symlink 检查见上方。
	if existing != nil {
		bakPath := path + ".bak"
		if bakInfo, lerr := os.Lstat(bakPath); lerr == nil && bakInfo.Mode()&os.ModeSymlink != 0 {
			if rerr := os.Remove(bakPath); rerr != nil {
				return OCConfigContent{}, fmt.Errorf("backup oc-config %s: remove symlink .bak: %w", name, rerr)
			}
		}
		// .bak 权限继承主文件当前权限（与主文件写入策略一致）。
		bakPerm := os.FileMode(0o600)
		if info != nil {
			bakPerm = info.Mode().Perm()
		}
		if err := atomicWriteFile(bakPath, existing, bakPerm); err != nil {
			return OCConfigContent{}, fmt.Errorf("backup oc-config %s: %w", name, err)
		}
	}

	// 临时文件原子 rename + 保留原权限。
	perm := os.FileMode(0o600)
	if info != nil {
		perm = info.Mode().Perm()
	}
	if err := atomicWriteFile(path, []byte(content), perm); err != nil {
		return OCConfigContent{}, fmt.Errorf("write oc-config %s: %w", name, err)
	}
	newInfo, err := os.Lstat(path)
	if err != nil {
		return OCConfigContent{}, fmt.Errorf("stat saved oc-config %s: %w", name, err)
	}
	return OCConfigContent{
		Name:    name,
		Content: content,
		Mtime:   newInfo.ModTime().Unix(),
		Hash:    hashContent([]byte(content)),
	}, nil
}

// resolvePath 解析 name 在管理目录下的绝对路径。name 已经过 validateConfigName 校验，
// 不含路径分隔符，故 filepath.Join 不会逃逸。
func (m *OCConfigManager) resolvePath(name string) string {
	return filepath.Join(m.dir, name)
}

// validateConfigName 校验 name 为合法文件名：非空、不含路径分隔符、不为 "."/".."。
func validateConfigName(name string) error {
	if name == "" {
		return ErrInvalidName
	}
	if strings.ContainsAny(name, `/\`) {
		return ErrInvalidName
	}
	if name == "." || name == ".." {
		return ErrInvalidName
	}
	if filepath.Base(name) != name {
		return ErrInvalidName
	}
	return nil
}

// isOCConfigFile 判断文件名为 *.json 或 *.jsonc（design.md §13）。
func isOCConfigFile(name string) bool {
	return strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".jsonc")
}

// hashContent 返回 content 的 sha256 hex。
func hashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// atomicWriteFile 用临时文件 + 原子 rename 写入，保留 perm 权限。
// 临时文件与目标同目录（保证 rename 原子性，同文件系统）。
func atomicWriteFile(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".oc-config-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	// 失败路径清理临时文件。
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
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

// validateConfigSyntax 按扩展名校验语法（design.md §13 + global-config-management spec）。
// .json 严格 JSON；.jsonc 剥离注释与尾逗号后用 encoding/json 校验（不引入第三方库）。
// 校验失败返回包裹 ErrInvalidSyntax 的错误，附带首条解析错误位置信息。
func validateConfigSyntax(name, content string) error {
	body := content
	if strings.HasSuffix(name, ".jsonc") {
		stripped, ok := stripJSONC(content)
		if !ok {
			return fmt.Errorf("%w: unclosed block comment", ErrInvalidSyntax)
		}
		body = stripped
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSyntax, err)
	}
	return nil
}

// stripJSONC 剥离 JSONC 的行注释、块注释与尾逗号，输出合法 JSON 文本供语法校验。
// 不做完整 JSONC 解析（仅用于校验，不影响保存原文）。保留字符串字面量内容不被误剥离。
//
// 实现：单次扫描的三态状态机（normal/in-string/in-line-comment/in-block-comment），
// 尾逗号在同一扫描中处理（记录最近一次写入的逗号位置，遇到 ] 或 } 时回退截断至该逗号
// 之前，丢弃中间空白）。未闭合块注释视为语法错误返回 ("", false)，由调用方映射为
// ErrInvalidSyntax。
func stripJSONC(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	i, n := 0, len(s)
	// pendingComma 记录最近一次写入的逗号位置；遇到 ] 或 } 时回退到该位置丢弃尾逗号。
	// 写入非空白、非闭合括号的字符时置 -1（逗号不再是尾逗号候选）。
	pendingComma := -1
	for i < n {
		c := s[i]
		switch {
		case c == '"':
			// 字符串字面量：保留字符串内一切字符（含 // 与 /*），仅转义跟踪到闭合引号。
			b.WriteByte(c)
			i++
			for i < n {
				d := s[i]
				if d == '\\' && i+1 < n {
					b.WriteByte(d)
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				b.WriteByte(d)
				i++
				if d == '"' {
					break
				}
			}
			pendingComma = -1
		case c == '/' && i+1 < n && s[i+1] == '/':
			// 行注释：跳到行尾（不含换行符，保留以维持行结构）。
			i += 2
			for i < n && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && s[i+1] == '*':
			// 块注释：跳到 */。未闭合视为语法错误。
			i += 2
			closed := false
			for i+1 < n {
				if s[i] == '*' && s[i+1] == '/' {
					i += 2
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "", false
			}
		case c == ',':
			b.WriteByte(c)
			pendingComma = b.Len() - 1
			i++
		case c == ']' || c == '}':
			if pendingComma >= 0 {
				// 尾逗号：截断 builder 到逗号之前，丢弃逗号与之间空白。
				kept := b.String()[:pendingComma]
				b.Reset()
				b.WriteString(kept)
				b.WriteByte(c)
				pendingComma = -1
			} else {
				b.WriteByte(c)
			}
			i++
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			// 空白不影响 pendingComma 候选状态。
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			pendingComma = -1
			i++
		}
	}
	return b.String(), true
}