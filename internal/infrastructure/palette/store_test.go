package palette

import (
	"bytes"
	"encoding/json"
	"log"
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

func TestLoadStore_FileNotExists_Defaults(t *testing.T) {
	s := LoadStore(t.TempDir())
	st := s.State()
	if st.Config != domainpalette.DefaultConfig() {
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
	if st.Config != domainpalette.DefaultConfig() {
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
	if st.Config != domainpalette.DefaultConfig() {
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
	if st.Config != domainpalette.DefaultConfig() {
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
	if st.Config != domainpalette.DefaultConfig() {
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
	cfg, err := DecodeConfig([]byte(`{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","Hotkey":"k"}`))
	if err != nil {
		t.Fatalf("unknown Hotkey must be ignored: %v", err)
	}
	if cfg.Hotkey != "alt+k" {
		t.Fatalf("Hotkey extra must not override hotkey, got %q", cfg.Hotkey)
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
	if st.LoadErr != nil || st.Config != cfg {
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
	if len(keys) != 3 {
		t.Fatalf("disk JSON must have exactly 3 keys, got %d: %s", len(keys), raw)
	}
	for _, k := range []string{"hotkey", "triggerWord", "matchMode"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("missing camelCase key %q in %s", k, raw)
		}
	}
	for k := range keys {
		if k == "Hotkey" || k == "TriggerWord" || k == "MatchMode" {
			t.Fatalf("PascalCase key %q must not be written: %s", k, raw)
		}
	}
	if strings.Contains(string(raw), `"Hotkey"`) || strings.Contains(string(raw), `"TriggerWord"`) || strings.Contains(string(raw), `"MatchMode"`) {
		t.Fatalf("PascalCase keys present: %s", raw)
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
	if got := s.State(); got != before {
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
	if got := s.State(); got != old {
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
