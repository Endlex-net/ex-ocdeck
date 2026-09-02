package notification

import (
	"encoding/json"
	"testing"
)

// TestDefaultConfig 验证默认配置逐字对齐 spec「通知配置存储」schema 块：
// 总开关关闭、类别全开、阈值 60、渠道全关（bark endpoint 默认 https://api.day.app）。
func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Enabled || c.LLMSummary || c.BaseURL != "" {
		t.Fatalf("default toggles mismatch: %+v", c)
	}
	if c.IdleTimeoutSeconds != 60 {
		t.Fatalf("idle_timeout_seconds = %d, want 60", c.IdleTimeoutSeconds)
	}
	if cats := c.Categories; !cats.Question || !cats.Permission || !cats.Idle || !cats.Retry || !cats.Error {
		t.Fatalf("default categories must be all-on: %+v", cats)
	}
	ch := c.Channels
	if ch.Web.Enabled || ch.Macos.Enabled || ch.Bark.Enabled {
		t.Fatalf("default channels must be all-off: %+v", ch)
	}
	if ch.Bark.Endpoint != "https://api.day.app" || ch.Bark.Token != "" {
		t.Fatalf("default bark endpoint/token mismatch: %+v", ch.Bark)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

// TestConfigValidate 校验规则表（spec「通知配置存储」：阈值 [10,3600]；URL 非空时
// http(s) hierarchical、非空 host、禁 userinfo/query/fragment、path 仅空或 "/"）。
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
		{"idle lower bound 10", mut(func(c *Config) { c.IdleTimeoutSeconds = 10 }), false},
		{"idle upper bound 3600", mut(func(c *Config) { c.IdleTimeoutSeconds = 3600 }), false},
		{"idle 9 too small", mut(func(c *Config) { c.IdleTimeoutSeconds = 9 }), true},
		{"idle 3601 too large", mut(func(c *Config) { c.IdleTimeoutSeconds = 3601 }), true},
		{"idle 0 missing-key fail-safe", mut(func(c *Config) { c.IdleTimeoutSeconds = 0 }), true},

		{"bark endpoint bare host", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://api.day.app" }), false},
		{"bark endpoint trailing slash path", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://api.day.app/" }), false},
		{"bark endpoint host with port", mut(func(c *Config) { c.Channels.Bark.Endpoint = "http://127.0.0.1:8080" }), false},
		{"bark endpoint deep path rejected", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://api.day.app/push" }), true},
		{"bark endpoint non-http scheme", mut(func(c *Config) { c.Channels.Bark.Endpoint = "ftp://h" }), true},
		{"bark endpoint empty host", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://" }), true},
		{"bark endpoint port-only authority rejected", mut(func(c *Config) { c.Channels.Bark.Endpoint = "http://:8080" }), true},
		{"bark endpoint empty query marker rejected", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://host?" }), true},
		{"bark endpoint query on slash path rejected", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://host/?" }), true},
		{"bark endpoint empty fragment marker rejected", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://host#" }), true},
		{"bark endpoint userinfo rejected", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://u:p@h" }), true},
		{"bark endpoint query rejected", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://h?q=1" }), true},
		{"bark endpoint fragment rejected", mut(func(c *Config) { c.Channels.Bark.Endpoint = "https://h#f" }), true},
		{"bark endpoint opaque rejected", mut(func(c *Config) { c.Channels.Bark.Endpoint = "mailto:a@b" }), true},

		{"base_url valid", mut(func(c *Config) { c.BaseURL = "http://192.168.1.10:7777" }), false},
		{"base_url empty ok", mut(func(c *Config) { c.BaseURL = "" }), false},
		{"base_url deep path rejected", mut(func(c *Config) { c.BaseURL = "https://example.com/base" }), true},
		{"base_url query rejected", mut(func(c *Config) { c.BaseURL = "https://example.com/?x" }), true},
		{"base_url empty query marker rejected", mut(func(c *Config) { c.BaseURL = "https://example.com?" }), true},
		{"base_url empty fragment marker rejected", mut(func(c *Config) { c.BaseURL = "https://example.com#" }), true},
		{"base_url port-only authority rejected", mut(func(c *Config) { c.BaseURL = "http://:9000" }), true},
		{"base_url no scheme", mut(func(c *Config) { c.BaseURL = "//example.com" }), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestConfigJSONShape 验证磁盘 schema 字段名逐字 snake_case 且全键写盘（无 omitempty）。
func TestConfigJSONShape(t *testing.T) {
	raw, err := json.Marshal(DefaultConfig())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal top: %v", err)
	}
	for _, k := range []string{"enabled", "categories", "idle_timeout_seconds", "channels", "llm_summary", "base_url"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, raw)
		}
	}
	var cats map[string]json.RawMessage
	if err := json.Unmarshal(m["categories"], &cats); err != nil {
		t.Fatalf("unmarshal categories: %v", err)
	}
	for _, k := range []string{"question", "permission", "idle", "retry", "error"} {
		if _, ok := cats[k]; !ok {
			t.Errorf("missing categories key %q", k)
		}
	}
	var channels map[string]json.RawMessage
	if err := json.Unmarshal(m["channels"], &channels); err != nil {
		t.Fatalf("unmarshal channels: %v", err)
	}
	for _, k := range []string{"web", "bark", "macos"} {
		if _, ok := channels[k]; !ok {
			t.Errorf("missing channels key %q", k)
		}
	}
	var bark map[string]json.RawMessage
	if err := json.Unmarshal(channels["bark"], &bark); err != nil {
		t.Fatalf("unmarshal bark: %v", err)
	}
	for _, k := range []string{"enabled", "endpoint", "token"} {
		if _, ok := bark[k]; !ok {
			t.Errorf("missing bark key %q", k)
		}
	}
}

// TestConfigJSONRoundTrip 模型经 marshal/unmarshal 无损往返。
func TestConfigJSONRoundTrip(t *testing.T) {
	c := DefaultConfig()
	c.Enabled = true
	c.IdleTimeoutSeconds = 300
	c.Channels.Bark.Token = "tok-12345678"
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != c {
		t.Fatalf("round trip: got %+v want %+v", got, c)
	}
}

// TestConfigUnmarshalUnknownFieldsIgnored 未知字段 MUST 忽略：解码不因 schema 外
// 字段失败，已知字段正常（spec「通知配置存储」）。
func TestConfigUnmarshalUnknownFieldsIgnored(t *testing.T) {
	src := `{"enabled":true,"future_top":123,"categories":{"question":true,"permission":true,"idle":true,"retry":true,"error":false},"idle_timeout_seconds":120,"channels":{"web":{"enabled":true,"future_web":"x"},"bark":{"enabled":false,"endpoint":"https://b.example.com","token":"tok"},"macos":{"enabled":false}},"llm_summary":false,"base_url":""}`
	var c Config
	if err := json.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("unmarshal with unknown fields: %v", err)
	}
	if !c.Enabled || c.IdleTimeoutSeconds != 120 || !c.Channels.Web.Enabled || c.Categories.Error || c.Channels.Bark.Token != "tok" {
		t.Fatalf("known fields mismatch after unknown-field decode: %+v", c)
	}
}
