package palette

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Hotkey != "mod+k" || c.TriggerWord != "new" || c.MatchMode != MatchModeExactThen {
		t.Fatalf("default config mismatch: %+v", c)
	}
	if err := c.Validate(); err != nil {
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
			err := c.Validate()
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
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.name == "32 code points" && utf8.RuneCountInString(tc.cfg.TriggerWord) != 32 {
				t.Fatalf("fixture length")
			}
		})
	}
}
