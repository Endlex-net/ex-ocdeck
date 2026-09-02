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

// Config 命令面板配置。磁盘/HTTP camelCase 映射由 adapter 负责，领域层用 Go 导出名。
type Config struct {
	Hotkey      string
	TriggerWord string
	MatchMode   string
}

func DefaultConfig() Config {
	return Config{
		Hotkey:      DefaultHotkey,
		TriggerWord: DefaultTriggerWord,
		MatchMode:   MatchModeExactThen,
	}
}

func (c Config) Validate() error {
	if err := validateCanonicalHotkey(c.Hotkey); err != nil {
		return err
	}
	if err := validateTriggerWord(c.TriggerWord); err != nil {
		return err
	}
	if c.MatchMode != MatchModeExact && c.MatchMode != MatchModeExactThen {
		return fmt.Errorf("matchMode %q must be %q or %q", c.MatchMode, MatchModeExact, MatchModeExactThen)
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
	if utf8.RuneCountInString(word) > MaxTriggerCodePts {
		return fmt.Errorf("triggerWord exceeds %d Unicode code points", MaxTriggerCodePts)
	}
	for _, r := range word {
		if isECMAScriptSpace(r) {
			return fmt.Errorf("triggerWord must not contain whitespace")
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
