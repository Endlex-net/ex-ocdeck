package palette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	domainpalette "ocdeck/internal/domain/palette"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	flags := log.Flags()
	prefix := log.Prefix()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	return &buf
}

func writeRaw(t *testing.T, dataDir, content string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(ConfigPath(dataDir), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func validCfg() domainpalette.Config {
	c := domainpalette.DefaultConfig()
	c.Hotkey = "alt+k"
	c.TriggerWord = "create"
	c.MatchMode = domainpalette.MatchModeExact
	return c
}

// configEqual Config 含 map 不可 ==，测试专用相等断言。
func configEqual(a, b domainpalette.Config) bool {
	return a.Hotkey == b.Hotkey && a.TriggerWord == b.TriggerWord && a.MatchMode == b.MatchMode &&
		maps.Equal(a.CommandTriggers, b.CommandTriggers)
}

func TestLoadStore_FileNotExists_Defaults(t *testing.T) {
	s := LoadStore(t.TempDir())
	st := s.State()
	if !configEqual(st.Config, domainpalette.DefaultConfig()) {
		t.Fatalf("missing file must yield defaults, got %+v", st.Config)
	}
	if st.LoadErr != nil {
		t.Fatalf("missing file must not set loadErr, got %v", st.LoadErr)
	}
}

func TestLoadStore_CorruptJSON_DefaultAndLoadErr(t *testing.T) {
	dataDir := t.TempDir()
	writeRaw(t, dataDir, "{not json")
	logs := captureLogs(t)
	s := LoadStore(dataDir)
	st := s.State()
	if st.LoadErr == nil {
		t.Fatal("corrupt json must set loadErr")
	}
	if !configEqual(st.Config, domainpalette.DefaultConfig()) {
		t.Fatalf("corrupt json must degrade to defaults, got %+v", st.Config)
	}
	if !strings.Contains(logs.String(), "palette config load failed") {
		t.Fatalf("degrade must log warning, got %q", logs.String())
	}
}

func TestLoadStore_Unreadable_DefaultAndLoadErr(t *testing.T) {
	dataDir := t.TempDir()
	writeRaw(t, dataDir, `{"hotkey":"mod+k","triggerWord":"new","matchMode":"exact-then-substring"}`)
	if err := os.Chmod(ConfigPath(dataDir), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ConfigPath(dataDir), 0o600) })
	logs := captureLogs(t)
	s := LoadStore(dataDir)
	st := s.State()
	if st.LoadErr == nil {
		t.Fatal("unreadable file must set loadErr")
	}
	if !configEqual(st.Config, domainpalette.DefaultConfig()) {
		t.Fatalf("unreadable must degrade to defaults, got %+v", st.Config)
	}
	if !strings.Contains(logs.String(), "palette config load failed") {
		t.Fatalf("degrade must log warning, got %q", logs.String())
	}
}

func TestLoadStore_InvalidFields_DefaultAndLoadErr(t *testing.T) {
	dataDir := t.TempDir()
	writeRaw(t, dataDir, `{"hotkey":"k","triggerWord":"new","matchMode":"exact-then-substring"}`)
	logs := captureLogs(t)
	s := LoadStore(dataDir)
	st := s.State()
	if st.LoadErr == nil {
		t.Fatal("illegal hotkey must set loadErr")
	}
	if !configEqual(st.Config, domainpalette.DefaultConfig()) {
		t.Fatalf("illegal fields must degrade to defaults, got %+v", st.Config)
	}
	out := logs.String()
	if !strings.Contains(out, "palette config invalid") {
		t.Fatalf("degrade must log invalid reason, got %q", out)
	}
}

func TestLoadStore_WrongCaseKeys_DefaultAndLoadErr(t *testing.T) {
	dataDir := t.TempDir()
	writeRaw(t, dataDir, `{"Hotkey":"alt+k","TriggerWord":"create","MatchMode":"exact"}`)
	logs := captureLogs(t)
	s := LoadStore(dataDir)
	st := s.State()
	if st.LoadErr == nil {
		t.Fatal("wrong-case keys must set loadErr")
	}
	if !configEqual(st.Config, domainpalette.DefaultConfig()) {
		t.Fatalf("wrong-case keys must degrade to defaults, got %+v", st.Config)
	}
	if !strings.Contains(logs.String(), "palette config load failed") {
		t.Fatalf("degrade must log warning, got %q", logs.String())
	}
}

func TestDecodeConfig_ExactCamelCaseKeys(t *testing.T) {
	if _, err := DecodeConfig([]byte(`{"Hotkey":"alt+k","triggerWord":"create","matchMode":"exact"}`)); err == nil {
		t.Fatal("PascalCase Hotkey must not satisfy required hotkey")
	}
	// PUT 语义四键全必填：缺少 commandTriggers 必须报错。
	if _, err := DecodeConfig([]byte(`{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact"}`)); err == nil {
		t.Fatal("missing commandTriggers must be rejected by DecodeConfig")
	}
	cfg, err := DecodeConfig([]byte(`{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"},"Hotkey":"k"}`))
	if err != nil {
		t.Fatalf("unknown Hotkey must be ignored: %v", err)
	}
	if cfg.Hotkey != "alt+k" {
		t.Fatalf("Hotkey extra must not override hotkey, got %q", cfg.Hotkey)
	}
}

// TestFoldForMatch 锁定与前端 foldForMatch（ECMAScript toLowerCase）兼容的
// 折叠行为（design D5：strings.ToLower/EqualFold 均不满足 İ 与 final sigma）。
func TestFoldForMatch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"i", "i"},
		{"I", "i"},
		{"Ä", "ä"},
		{"İ", "i̇"}, // U+0130 → i + U+0307（长度 1→2）
		{"i̇", "i̇"},
		{"ΟΣ", "ος"}, // Greek final sigma 上下文规则
		{"ος", "ος"},
		{"οσ", "οσ"},
		{"ΟΣΣ", "οσς"}, // 仅词尾 Σ → ς，词中 Σ → σ（与 ECMAScript 一致）
		{"ẞ", "ß"},
	}
	for _, tc := range cases {
		if got := FoldForMatch(tc.in); got != tc.want {
			t.Errorf("FoldForMatch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPut_RejectsFoldDuplicateTriggers Store.Put 注入真实 FoldForMatch 复验：
// fold 相同（İ → i\u0307）的非空触发词必须被拒且不落盘。
func TestPut_RejectsFoldDuplicateTriggers(t *testing.T) {
	dir := t.TempDir()
	s := LoadStore(dir)
	cfg := validCfg()
	cfg.CommandTriggers = maps.Clone(domainpalette.DefaultCommandTriggers())
	cfg.CommandTriggers[domainpalette.CommandIDProjects] = "İ"
	cfg.CommandTriggers[domainpalette.CommandIDSettingsAI] = "i̇"
	if err := s.Put(cfg); err == nil {
		t.Fatal("fold duplicate command triggers must be rejected")
	}
	if _, err := os.Stat(ConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("rejected put must not write file, stat err=%v", err)
	}
}

// TestLoadStore_LegacyThreeKeys_MigrateDefaultTriggers 旧三键文件（无
// commandTriggers 键）MUST NOT 降级：按其余三键加载并复制默认词表，与旧
// triggerWord fold 相同的默认项置空，其余保留。
func TestLoadStore_LegacyThreeKeys_MigrateDefaultTriggers(t *testing.T) {
	defaults := domainpalette.DefaultCommandTriggers()
	blanked := func(id string) map[string]string {
		want := maps.Clone(defaults)
		want[id] = ""
		return want
	}
	cases := []struct {
		name        string
		triggerWord string
		want        map[string]string
	}{
		{"no conflict keeps defaults", "create", defaults},
		{"cc conflict blanks command-center", "cc", blanked(domainpalette.CommandIDCenter)},
		{"CC fold conflict blanks command-center", "CC", blanked(domainpalette.CommandIDCenter)},
		{"pro conflict blanks projects", "pro", blanked(domainpalette.CommandIDProjects)},
		{"reg conflict blanks register-project", "reg", blanked(domainpalette.CommandIDRegisterProj)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			writeRaw(t, dataDir, fmt.Sprintf(`{"hotkey":"alt+k","triggerWord":%q,"matchMode":"exact"}`, tc.triggerWord))
			st := LoadStore(dataDir).State()
			if st.LoadErr != nil {
				t.Fatalf("legacy three-key file must not degrade, loadErr=%v", st.LoadErr)
			}
			want := validCfg()
			want.TriggerWord = tc.triggerWord
			want.CommandTriggers = tc.want
			if !configEqual(st.Config, want) {
				t.Fatalf("migrated config = %+v, want %+v", st.Config, want)
			}
		})
	}
}

// TestLoadStore_InvalidCommandTriggers_DefaultAndLoadErr 磁盘含 commandTriggers
// 但非法（未知 ID/键不全/值非法/重复冲突）→ 既有损坏降级语义 + 告警。
func TestLoadStore_InvalidCommandTriggers_DefaultAndLoadErr(t *testing.T) {
	const base = `"hotkey":"alt+k","triggerWord":"create","matchMode":"exact"`
	cases := []struct {
		name            string
		commandTriggers string
	}{
		{"unknown command ID", `{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","unknown-cmd":"reg"}`},
		{"incomplete keys", `{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":""}`},
		{"value whitespace", `{"command-center":"cc","projects":"p r","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}`},
		{"duplicate conflict", `{"command-center":"cc","projects":"CC","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			writeRaw(t, dataDir, fmt.Sprintf(`{%s,"commandTriggers":%s}`, base, tc.commandTriggers))
			logs := captureLogs(t)
			st := LoadStore(dataDir).State()
			if st.LoadErr == nil {
				t.Fatal("illegal commandTriggers must set loadErr")
			}
			if !configEqual(st.Config, domainpalette.DefaultConfig()) {
				t.Fatalf("must degrade to defaults, got %+v", st.Config)
			}
			if !strings.Contains(logs.String(), "palette config invalid") {
				t.Fatalf("degrade must log invalid reason, got %q", logs.String())
			}
		})
	}
}

// TestLoadStore_MalformedCommandTriggers_DefaultAndLoadErr commandTriggers 键
// 存在但形状损坏（非对象/null/值类型错误）→ 解码失败降级 + 告警。
func TestLoadStore_MalformedCommandTriggers_DefaultAndLoadErr(t *testing.T) {
	const base = `"hotkey":"alt+k","triggerWord":"create","matchMode":"exact"`
	cases := []struct {
		name            string
		commandTriggers string
	}{
		{"null", `null`},
		{"array", `["cc"]`},
		{"string", `"cc"`},
		{"value type error", `{"command-center":5,"projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			writeRaw(t, dataDir, fmt.Sprintf(`{%s,"commandTriggers":%s}`, base, tc.commandTriggers))
			logs := captureLogs(t)
			st := LoadStore(dataDir).State()
			if st.LoadErr == nil {
				t.Fatal("malformed commandTriggers must set loadErr")
			}
			if !configEqual(st.Config, domainpalette.DefaultConfig()) {
				t.Fatalf("must degrade to defaults, got %+v", st.Config)
			}
			if !strings.Contains(logs.String(), "palette config load failed") {
				t.Fatalf("degrade must log warning, got %q", logs.String())
			}
		})
	}
}

// TestLoadStore_CommandTriggersRoundTrip 磁盘含合法 commandTriggers 时按原样
// 加载（不走缺键迁移）。
func TestLoadStore_CommandTriggersRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	cfg := validCfg()
	cfg.CommandTriggers = maps.Clone(domainpalette.DefaultCommandTriggers())
	cfg.CommandTriggers[domainpalette.CommandIDProjects] = "pj"
	cfg.CommandTriggers[domainpalette.CommandIDSettingsAI] = "ai"
	if err := LoadStore(dataDir).Put(cfg); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	st := LoadStore(dataDir).State()
	if st.LoadErr != nil {
		t.Fatalf("valid commandTriggers must load, loadErr=%v", st.LoadErr)
	}
	if !configEqual(st.Config, cfg) {
		t.Fatalf("round trip = %+v, want %+v", st.Config, cfg)
	}
}

func TestPut_RoundTrip_CamelCase0600(t *testing.T) {
	dataDir := t.TempDir()
	s := LoadStore(dataDir)
	cfg := validCfg()
	if err := s.Put(cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	st := s.State()
	if st.LoadErr != nil || !configEqual(st.Config, cfg) {
		t.Fatalf("snapshot after put: %+v (loadErr=%v)", st.Config, st.LoadErr)
	}
	path := ConfigPath(dataDir)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm=%o want 0600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal keys: %v", err)
	}
	if len(keys) != 4 {
		t.Fatalf("disk JSON must have exactly 4 keys, got %d: %s", len(keys), raw)
	}
	for _, k := range []string{"hotkey", "triggerWord", "matchMode", "commandTriggers"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("missing camelCase key %q in %s", k, raw)
		}
	}
	for k := range keys {
		if k == "Hotkey" || k == "TriggerWord" || k == "MatchMode" || k == "CommandTriggers" {
			t.Fatalf("PascalCase key %q must not be written: %s", k, raw)
		}
	}
	if strings.Contains(string(raw), `"Hotkey"`) || strings.Contains(string(raw), `"TriggerWord"`) || strings.Contains(string(raw), `"MatchMode"`) || strings.Contains(string(raw), `"CommandTriggers"`) {
		t.Fatalf("PascalCase keys present: %s", raw)
	}
	// validCfg 携带默认词表：默认词表（cc/pro/reg + 5 空键）整体落盘。
	var onDisk struct {
		CommandTriggers map[string]string `json:"commandTriggers"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal disk config: %v", err)
	}
	if !maps.Equal(onDisk.CommandTriggers, domainpalette.DefaultCommandTriggers()) {
		t.Fatalf("default command triggers must persist, got %v", onDisk.CommandTriggers)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dataDir, ".palette-tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("atomic write must not leave temp files: %v", leftovers)
	}
}

func TestPut_InvalidRejected_NoWrite(t *testing.T) {
	s := LoadStore(t.TempDir())
	before := s.State()
	bad := validCfg()
	bad.Hotkey = "k"
	if err := s.Put(bad); err == nil {
		t.Fatal("invalid hotkey must be rejected")
	}
	if got := s.State(); !configEqual(got.Config, before.Config) {
		t.Fatalf("snapshot must be unchanged, got %+v want %+v", got, before)
	}
	if _, err := os.Stat(ConfigPath(s.dataDir)); !os.IsNotExist(err) {
		t.Fatalf("no config file should exist after rejected put, stat err=%v", err)
	}
}

func TestPut_WriteFailureKeepsSnapshotAndFile(t *testing.T) {
	dir := t.TempDir()
	s := LoadStore(dir)
	if err := s.Put(validCfg()); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	old := s.State()
	path := ConfigPath(dir)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	incoming := validCfg()
	incoming.TriggerWord = "other"
	if err := s.Put(incoming); err == nil {
		t.Fatal("write to read-only dir should fail")
	}
	if got := s.State(); !configEqual(got.Config, old.Config) {
		t.Fatalf("snapshot must be unchanged after write failure, got %+v want %+v", got, old)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("config file bytes changed after write failure")
	}
}
