package palette

import (
	"maps"
	"strings"
	"testing"
	"unicode/utf8"
)

// testFold 域内用例注入的折叠替身：本文件用例均为 Latin-1/ASCII 可表达；
// 真实 foldForMatch 语义（İ → i\u0307、Greek final sigma）由
// infrastructure/palette 的 FoldForMatch 测试与 api 端到端 422 用例锁定。
var testFold = strings.ToLower

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Hotkey != "mod+k" || c.TriggerWord != "new" || c.MatchMode != MatchModeExactThen {
		t.Fatalf("default config mismatch: %+v", c)
	}
	if !maps.Equal(c.CommandTriggers, DefaultCommandTriggers()) {
		t.Fatalf("default command triggers mismatch: %v", c.CommandTriggers)
	}
	if len(c.CommandTriggers) != 8 {
		t.Fatalf("default command triggers must have 8 keys, got %d", len(c.CommandTriggers))
	}
	if err := c.Validate(testFold); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestHotkeyValidateMatrix(t *testing.T) {
	// 与 web/src/__tests__/hotkey.test.ts hotkeyValidateCases 同组命名。
	cases := []struct {
		name    string
		hotkey  string
		wantErr bool
	}{
		{"mod+k", "mod+k", false},
		{"mod+shift+1", "mod+shift+1", false},
		{"meta+alt+k", "meta+alt+k", false},
		{"ctrl+alt+k", "ctrl+alt+k", false},
		{"alt+k", "alt+k", false},
		{"no modifier", "k", true},
		{"shift only", "shift+k", true},
		{"mod+banana", "mod+banana", true},
		{"mod++", "mod++", true},
		{"mod+K uppercase", "mod+K", true},
		{"duplicate modifier", "mod+mod+k", true},
		{"mod+meta+k", "mod+meta+k", true},
		{"mod+ctrl+k", "mod+ctrl+k", true},
		{"wrong order", "shift+mod+k", true},
		{"mod+t reserved", "mod+t", true},
		{"mod+w reserved", "mod+w", true},
		{"mod+n reserved", "mod+n", true},
		{"mod+q reserved", "mod+q", true},
		{"ctrl+t reserved", "ctrl+t", true},
		{"meta+ctrl+t reserved", "meta+ctrl+t", true},
		{"meta+shift+t reserved", "meta+shift+t", true},
		{"alt+t allowed", "alt+t", false},
		{"mod+b sidebar", "mod+b", true},
		{"ctrl+b sidebar", "ctrl+b", true},
		{"meta+ctrl+b sidebar", "meta+ctrl+b", true},
		{"alt+b allowed", "alt+b", false},
		{"meta+shift+b allowed", "meta+shift+b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			c.Hotkey = tc.hotkey
			err := c.Validate(testFold)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() hotkey=%q err=%v wantErr=%v", tc.hotkey, err, tc.wantErr)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	mut := func(f func(*Config)) Config {
		c := DefaultConfig()
		f(&c)
		return c
	}
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"default valid", DefaultConfig(), false},
		{"empty trigger", mut(func(c *Config) { c.TriggerWord = "" }), true},
		{"tab trigger", mut(func(c *Config) { c.TriggerWord = "new\t" }), true},
		{"nbsp trigger", mut(func(c *Config) { c.TriggerWord = "new\u00a0" }), true},
		{"nel not in ECMAScript set", mut(func(c *Config) { c.TriggerWord = "new\u0085" }), false},
		{"ideographic space", mut(func(c *Config) { c.TriggerWord = "new\u3000" }), true},
		{"bom trigger", mut(func(c *Config) { c.TriggerWord = "new\ufeff" }), true},
		{"33 code points", mut(func(c *Config) { c.TriggerWord = strings.Repeat("a", 33) }), true},
		{"32 code points", mut(func(c *Config) { c.TriggerWord = strings.Repeat("你", 32) }), false},
		{"bad matchMode", mut(func(c *Config) { c.MatchMode = "fuzzy" }), true},
		{"exact matchMode", mut(func(c *Config) { c.MatchMode = MatchModeExact }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate(testFold)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.name == "32 code points" && utf8.RuneCountInString(tc.cfg.TriggerWord) != 32 {
				t.Fatalf("fixture length")
			}
		})
	}
}

// TestCommandTriggersValidate 校验矩阵：恰 8 键、未知 ID 拒绝、非空值沿用
// triggerWord 字符规则、非空值 fold 去重、不可与全局 triggerWord 相同（fold
// 比较）；前缀重叠允许（spec palette-config「命令面板配置读写 API」）。
func TestCommandTriggersValidate(t *testing.T) {
	// withDefaults 以自定义 triggerWord（create，不与默认词表冲突）为基底。
	withTriggers := func(f func(triggers map[string]string)) Config {
		c := DefaultConfig()
		c.TriggerWord = "create"
		f(c.CommandTriggers)
		return c
	}
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"defaults valid", withTriggers(func(map[string]string) {}), false},
		{"nil map", func() Config {
			c := DefaultConfig()
			c.TriggerWord = "create"
			c.CommandTriggers = nil
			return c
		}(), true},
		{"missing key", withTriggers(func(t map[string]string) { delete(t, "settings-ai") }), true},
		{"unknown ID replaces known key", withTriggers(func(t map[string]string) { delete(t, "settings-ai"); t["unknown-cmd"] = "x" }), true},
		{"extra 9th key", withTriggers(func(t map[string]string) { t["unknown-cmd"] = "x" }), true},
		{"value whitespace", withTriggers(func(t map[string]string) { t["projects"] = "p r" }), true},
		{"value NBSP", withTriggers(func(t map[string]string) { t["projects"] = "p\u00a0r" }), true},
		{"value 33 code points", withTriggers(func(t map[string]string) { t["projects"] = strings.Repeat("你", 33) }), true},
		{"value 32 code points", withTriggers(func(t map[string]string) { t["projects"] = strings.Repeat("你", 32) }), false},
		{"dup exact", withTriggers(func(t map[string]string) { t["projects"] = "cc" }), true},
		{"dup fold Ä/ä", withTriggers(func(t map[string]string) { t["projects"] = "ä"; t["command-center"] = "Ä" }), true},
		{"equals global triggerWord exact", withTriggers(func(t map[string]string) { t["projects"] = "create" }), true},
		{"equals global triggerWord fold", withTriggers(func(t map[string]string) { t["projects"] = "CREATE" }), true},
		{"prefix overlap allowed", withTriggers(func(t map[string]string) { t["projects"] = "ccx"; t["command-center"] = "cc" }), false},
		{"multiple empty values allowed", withTriggers(func(t map[string]string) { t["projects"] = ""; t["register-project"] = "" }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(testFold); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
