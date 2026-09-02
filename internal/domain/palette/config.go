package palette

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	DefaultHotkey      = "mod+k"
	DefaultTriggerWord = "new"
	MatchModeExact     = "exact"
	MatchModeExactThen = "exact-then-substring"
	MaxTriggerCodePts  = 32
)

// PaletteCommandId 枚举（spec palette-config「命令面板配置读写 API」）。
const (
	CommandIDCenter          = "command-center"
	CommandIDProjects        = "projects"
	CommandIDSettingsAppear  = "settings-appearance"
	CommandIDSettingsEnv     = "settings-env"
	CommandIDSettingsOC      = "settings-opencode"
	CommandIDSettingsAI      = "settings-ai"
	CommandIDSettingsPalette = "settings-palette"
	CommandIDRegisterProj    = "register-project"
)

var knownCommandIDs = map[string]struct{}{
	CommandIDCenter:          {},
	CommandIDProjects:        {},
	CommandIDSettingsAppear:  {},
	CommandIDSettingsEnv:     {},
	CommandIDSettingsOC:      {},
	CommandIDSettingsAI:      {},
	CommandIDSettingsPalette: {},
	CommandIDRegisterProj:    {},
}

// Config 命令面板配置。磁盘/HTTP camelCase 映射由 adapter 负责，领域层用 Go 导出名。
// CommandTriggers 键为 PaletteCommandId 枚举（恰 8 键），空串值 = 指令未启用。
type Config struct {
	Hotkey          string
	TriggerWord     string
	MatchMode       string
	CommandTriggers map[string]string
}

// DefaultCommandTriggers 默认指令触发词表：cc/pro/reg + 5 空键。
func DefaultCommandTriggers() map[string]string {
	return map[string]string{
		CommandIDCenter:          "cc",
		CommandIDProjects:        "pro",
		CommandIDRegisterProj:    "reg",
		CommandIDSettingsAppear:  "",
		CommandIDSettingsEnv:     "",
		CommandIDSettingsOC:      "",
		CommandIDSettingsAI:      "",
		CommandIDSettingsPalette: "",
	}
}

func DefaultConfig() Config {
	return Config{
		Hotkey:          DefaultHotkey,
		TriggerWord:     DefaultTriggerWord,
		MatchMode:       MatchModeExactThen,
		CommandTriggers: DefaultCommandTriggers(),
	}
}

// Validate 校验完整配置。fold 为大小写折叠函数，由调用方注入：domain 保持
// stdlib-only（import_graph_test.go TestDomainStdlibOnly），与前端
// foldForMatch（ECMAScript toLowerCase）兼容的规范实现 x/text
// cases.Lower(language.Und) 落在 infrastructure/palette.FoldForMatch，
// 生产路径必须注入该实现（design D5）。
func (c Config) Validate(fold func(string) string) error {
	if err := validateCanonicalHotkey(c.Hotkey); err != nil {
		return err
	}
	if err := validateTriggerWord(c.TriggerWord); err != nil {
		return err
	}
	if c.MatchMode != MatchModeExact && c.MatchMode != MatchModeExactThen {
		return fmt.Errorf("matchMode %q must be %q or %q", c.MatchMode, MatchModeExact, MatchModeExactThen)
	}
	return validateCommandTriggers(c.CommandTriggers, c.TriggerWord, fold)
}

// validateCommandTriggers 恰 8 键、键全为已知指令 ID；非空值沿用 triggerWord
// 字符规则，且按 fold 比较互不重复、不与全局 triggerWord 相同；前缀重叠允许
// （解析按最长前缀优先）。
func validateCommandTriggers(triggers map[string]string, globalTrigger string, fold func(string) string) error {
	if len(triggers) != len(knownCommandIDs) {
		return fmt.Errorf("commandTriggers must contain exactly %d command IDs, got %d", len(knownCommandIDs), len(triggers))
	}
	foldedGlobal := fold(globalTrigger)
	folded2ID := make(map[string]string, len(triggers))
	for id, word := range triggers {
		if _, known := knownCommandIDs[id]; !known {
			return fmt.Errorf("commandTriggers has unknown command ID %q", id)
		}
		if word == "" {
			continue
		}
		if err := validateWordChars(word); err != nil {
			return fmt.Errorf("commandTriggers[%s]: %w", id, err)
		}
		folded := fold(word)
		if folded == foldedGlobal {
			return fmt.Errorf("commandTriggers[%s] %q must not equal global triggerWord %q", id, word, globalTrigger)
		}
		if prevID, dup := folded2ID[folded]; dup {
			return fmt.Errorf("commandTriggers[%s] %q duplicates commandTriggers[%s] %q", id, word, prevID, triggers[prevID])
		}
		folded2ID[folded] = id
	}
	return nil
}

var modifierOrder = []string{"mod", "meta", "ctrl", "alt", "shift"}

var modifierSet = map[string]struct{}{
	"mod": {}, "meta": {}, "ctrl": {}, "alt": {}, "shift": {},
}

func validateCanonicalHotkey(canonical string) error {
	if canonical == "" {
		return fmt.Errorf("hotkey must be a canonical combo")
	}
	parts := strings.Split(canonical, "+")
	if len(parts) < 2 {
		return fmt.Errorf("hotkey %q must include a modifier", canonical)
	}
	mods := parts[:len(parts)-1]
	key := parts[len(parts)-1]
	if key == "" || len(key) != 1 {
		return fmt.Errorf("hotkey %q key token must be a single [a-z0-9]", canonical)
	}
	ch := key[0]
	if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
		return fmt.Errorf("hotkey %q key token must be a single [a-z0-9]", canonical)
	}
	seen := map[string]struct{}{}
	hasPrimary := false
	prevIdx := -1
	for _, m := range mods {
		if _, ok := modifierSet[m]; !ok {
			return fmt.Errorf("hotkey %q has unknown modifier %q", canonical, m)
		}
		if _, dup := seen[m]; dup {
			return fmt.Errorf("hotkey %q repeats modifier %q", canonical, m)
		}
		seen[m] = struct{}{}
		idx := modifierIndex(m)
		if idx < prevIdx {
			return fmt.Errorf("hotkey %q modifiers must be in order mod,meta,ctrl,alt,shift", canonical)
		}
		prevIdx = idx
		if m == "mod" || m == "meta" || m == "ctrl" || m == "alt" {
			hasPrimary = true
		}
	}
	if !hasPrimary {
		return fmt.Errorf("hotkey %q must include mod|meta|ctrl|alt", canonical)
	}
	if _, hasMod := seen["mod"]; hasMod {
		if _, hasMeta := seen["meta"]; hasMeta {
			return fmt.Errorf("hotkey %q must not combine mod with meta/ctrl", canonical)
		}
		if _, hasCtrl := seen["ctrl"]; hasCtrl {
			return fmt.Errorf("hotkey %q must not combine mod with meta/ctrl", canonical)
		}
	}
	if reservedCombo(seen, key) {
		return fmt.Errorf("hotkey %q is a reserved browser combo", canonical)
	}
	if sidebarBConflict(seen, key) {
		return fmt.Errorf("hotkey %q conflicts with sidebar ⌘B", canonical)
	}
	return nil
}

func modifierIndex(m string) int {
	for i, name := range modifierOrder {
		if name == m {
			return i
		}
	}
	return -1
}

func reservedCombo(seen map[string]struct{}, key string) bool {
	switch key {
	case "t", "w", "n", "q":
	default:
		return false
	}
	_, hasMeta := seen["meta"]
	_, hasCtrl := seen["ctrl"]
	_, hasMod := seen["mod"]
	return hasMeta || hasCtrl || hasMod
}

func sidebarBConflict(seen map[string]struct{}, key string) bool {
	if key != "b" {
		return false
	}
	if _, hasAlt := seen["alt"]; hasAlt {
		return false
	}
	if _, hasShift := seen["shift"]; hasShift {
		return false
	}
	_, hasMeta := seen["meta"]
	_, hasCtrl := seen["ctrl"]
	_, hasMod := seen["mod"]
	return hasMeta || hasCtrl || hasMod
}

func validateTriggerWord(word string) error {
	if word == "" {
		return fmt.Errorf("triggerWord must be non-empty")
	}
	if err := validateWordChars(word); err != nil {
		return fmt.Errorf("triggerWord %w", err)
	}
	return nil
}

// validateWordChars 全局触发词与指令触发词非空值共用的字符规则
// （空白集合 + 32 code point 上限）。
func validateWordChars(word string) error {
	if utf8.RuneCountInString(word) > MaxTriggerCodePts {
		return fmt.Errorf("exceeds %d Unicode code points", MaxTriggerCodePts)
	}
	for _, r := range word {
		if isECMAScriptSpace(r) {
			return fmt.Errorf("must not contain whitespace")
		}
	}
	return nil
}

// isECMAScriptSpace is ECMAScript WhiteSpace + LineTerminator (not unicode.IsSpace).
func isECMAScriptSpace(r rune) bool {
	switch r {
	case 0x0009, 0x000A, 0x000B, 0x000C, 0x000D, 0x0020, 0x00A0,
		0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005,
		0x2006, 0x2007, 0x2008, 0x2009, 0x200A, 0x2028, 0x2029,
		0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	default:
		return false
	}
}
