package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeRawConfig(t *testing.T, dataDir, content string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir dataDir: %v", err)
	}
	if err := os.WriteFile(ConfigPath(dataDir), []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func validCfg() ProviderConfig {
	return ProviderConfig{
		Provider: ProviderOpenAI,
		APIKey:   "sk-test-1234567890",
		Model:    "gpt-4o-mini",
		BaseURL:  "",
	}
}

func TestProviderConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ProviderConfig
		wantErr bool
	}{
		{"valid openai", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m"}, false},
		{"valid anthropic", ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m"}, false},
		{"valid with base_url http", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: "http://localhost:8080"}, false},
		{"valid with base_url https", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: "https://proxy.example.com/"}, false},
		{"valid thinking empty", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", Thinking: ""}, false},
		{"valid thinking off", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", Thinking: "off"}, false},
		{"valid thinking low", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", Thinking: "low"}, false},
		{"valid thinking medium", ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", Thinking: "medium"}, false},
		{"valid thinking high", ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", Thinking: "high"}, false},
		{"invalid provider", ProviderConfig{Provider: "gemini", APIKey: "k", Model: "m"}, true},
		{"empty model", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "  "}, true},
		{"base_url not http(s)", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: "ftp://x"}, true},
		{"base_url empty host", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: "http://"}, true},
		{"invalid thinking value", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", Thinking: "auto"}, true},
		{"invalid thinking whitespace", ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", Thinking: "  "}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate err=%v wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestEffectiveBaseURL(t *testing.T) {
	cases := []struct {
		in   ProviderConfig
		want string
	}{
		{ProviderConfig{Provider: ProviderOpenAI}, "https://api.openai.com"},
		{ProviderConfig{Provider: ProviderAnthropic}, "https://api.anthropic.com"},
		{ProviderConfig{Provider: ProviderOpenAI, BaseURL: "https://proxy/"}, "https://proxy"},
		{ProviderConfig{Provider: ProviderOpenAI, BaseURL: "https://proxy///"}, "https://proxy"},
	}
	for _, c := range cases {
		if got := c.in.EffectiveBaseURL(); got != c.want {
			t.Errorf("EffectiveBaseURL(%+v)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestSaveLoadConfig_FileNotExists(t *testing.T) {
	dataDir := t.TempDir()
	_, ok, err := loadConfigFile(dataDir)
	if err != nil || ok {
		t.Fatalf("file not exists: ok=%v err=%v", ok, err)
	}
}

func TestSaveLoadConfig_RoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	cfg := validCfg()
	if err := saveConfigFile(dataDir, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 权限 0600
	info, err := os.Stat(ConfigPath(dataDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm=%o want 0600", perm)
	}
	got, ok, err := loadConfigFile(dataDir)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got != cfg {
		t.Errorf("round trip: got %+v want %+v", got, cfg)
	}
}

func TestSaveLoadConfig_CorruptJSON(t *testing.T) {
	dataDir := t.TempDir()
	writeRawConfig(t, dataDir, "{not json")
	_, ok, err := loadConfigFile(dataDir)
	if err == nil || ok {
		t.Fatalf("corrupt json should return err: ok=%v err=%v", ok, err)
	}
}

func TestLoadStore_FileNotExists(t *testing.T) {
	s := LoadStore(t.TempDir())
	if s.Configured() {
		t.Errorf("not configured expected")
	}
	if s.LoadError() != nil {
		t.Errorf("no load error expected, got %v", s.LoadError())
	}
}

func TestLoadStore_Valid(t *testing.T) {
	dataDir := t.TempDir()
	cfg := validCfg()
	if err := saveConfigFile(dataDir, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	s := LoadStore(dataDir)
	if !s.Configured() {
		t.Errorf("configured expected")
	}
	if s.LoadError() != nil {
		t.Errorf("no load error, got %v", s.LoadError())
	}
	if got := s.Config(); got != cfg {
		t.Errorf("config mismatch got %+v want %+v", got, cfg)
	}
}

func TestLoadStore_Corrupt_NotConfigured_NotPanic(t *testing.T) {
	dataDir := t.TempDir()
	writeRawConfig(t, dataDir, "garbage")
	s := LoadStore(dataDir)
	if s.Configured() {
		t.Errorf("corrupt should not be configured")
	}
	if s.LoadError() == nil {
		t.Errorf("load error expected")
	}
}

func TestLoadStore_InvalidFields(t *testing.T) {
	dataDir := t.TempDir()
	// 文件可解析但 provider 非法 → loadErr + not configured。
	cfg := ProviderConfig{Provider: "gemini", APIKey: "k", Model: "m"}
	if err := saveConfigFile(dataDir, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	s := LoadStore(dataDir)
	if s.Configured() {
		t.Errorf("invalid fields should not be configured")
	}
	if s.LoadError() == nil {
		t.Errorf("load error expected for invalid fields")
	}
}

func TestStore_Put_SavesAndSnapshots(t *testing.T) {
	s := LoadStore(t.TempDir())
	cfg := validCfg()
	if err := s.Put(cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	if !s.Configured() {
		t.Errorf("configured after put")
	}
	// 文件落盘
	got, ok, err := loadConfigFile(s.dataDir)
	if err != nil || !ok {
		t.Fatalf("load after put: ok=%v err=%v", ok, err)
	}
	if got != cfg {
		t.Errorf("disk mismatch got %+v want %+v", got, cfg)
	}
}

func TestStore_Put_MaskKeyPreservesOld(t *testing.T) {
	s := LoadStore(t.TempDir())
	old := validCfg()
	if err := s.Put(old); err != nil {
		t.Fatalf("put old: %v", err)
	}
	// 仅改 model，key 用掩码值
	incoming := validCfg()
	incoming.APIKey = "sk-t***"
	incoming.Model = "gpt-4o"
	if err := s.Put(incoming); err != nil {
		t.Fatalf("put masked: %v", err)
	}
	got := s.Config()
	if got.APIKey != old.APIKey {
		t.Errorf("api_key should be preserved, got %q want %q", got.APIKey, old.APIKey)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("model not updated, got %q", got.Model)
	}
}

func TestStore_Put_EmptyKeyNoOld_Rejected(t *testing.T) {
	s := LoadStore(t.TempDir())
	incoming := validCfg()
	incoming.APIKey = ""
	if err := s.Put(incoming); err == nil {
		t.Errorf("empty key with no old key should be rejected")
	}
	if s.Configured() {
		t.Errorf("should remain unconfigured")
	}
}

func TestStore_Put_InvalidProvider_Rejected(t *testing.T) {
	s := LoadStore(t.TempDir())
	incoming := validCfg()
	incoming.Provider = "gemini"
	if err := s.Put(incoming); err == nil {
		t.Errorf("invalid provider should be rejected")
	}
}

func TestStore_Put_InvalidBaseURL_Rejected(t *testing.T) {
	s := LoadStore(t.TempDir())
	incoming := validCfg()
	incoming.BaseURL = "ftp://x"
	if err := s.Put(incoming); err == nil {
		t.Errorf("invalid base_url should be rejected")
	}
	if _, err := os.Stat(ConfigPath(s.dataDir)); !os.IsNotExist(err) {
		t.Errorf("no config file should exist after rejected put, stat err=%v", err)
	}
}

func TestStore_Put_WriteFailureKeepsOldSnapshot(t *testing.T) {
	// 使用一个不可写 dataDir（先用 valid Put，再将 dataDir 置为只读模拟写失败）。
	dir := t.TempDir()
	s := LoadStore(dir)
	if err := s.Put(validCfg()); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	oldSnapshot := s.Snapshot()

	// 将 dataDir 设为只读，使 temp 创建失败。注意 macOS 下根 TempDir 可能无法 chmod，
	// 故改用嵌套只读目录 + 直接构造无法创建临时文件的场景。
	roDir := filepath.Join(dir, "readonly")
	if err := os.MkdirAll(roDir, 0o500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	s.dataDir = roDir
	incoming := validCfg()
	incoming.APIKey = "sk-new-key-12345"
	incoming.Model = "gpt-4o"
	err := s.Put(incoming)
	if err == nil {
		t.Errorf("write should fail on read-only dir")
	}
	// 旧快照保持不变
	if s.Snapshot() != oldSnapshot {
		t.Errorf("snapshot should be unchanged after write failure")
	}
}

func TestStore_SnapshotStableAcrossPut(t *testing.T) {
	s := LoadStore(t.TempDir())
	s.Put(validCfg())
	sn1 := s.Snapshot()
	s.Put(func() ProviderConfig {
		c := validCfg()
		c.Model = "gpt-4o"
		return c
	}())
	sn2 := s.Snapshot()
	// 不同快照实例（替换），但 sn1 自身仍可读且不可变。
	if sn1 == nil || sn2 == nil {
		t.Fatalf("snapshots nil")
	}
	if sn1.cfg.Model == sn2.cfg.Model {
		t.Errorf("snapshot not replaced")
	}
}

func TestStore_APIKeyNotInFileBeyondConfig(t *testing.T) {
	// 确保 api_key 原样落盘（仅在 0600 文件内）。这是正面断言：文件里就是 key 明文。
	dataDir := t.TempDir()
	s := LoadStore(dataDir)
	cfg := validCfg()
	cfg.APIKey = "sk-secret-xyz"
	if err := s.Put(cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	raw, err := os.ReadFile(ConfigPath(dataDir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "sk-secret-xyz") {
		t.Errorf("api_key should be present in 0600 file")
	}
	// 但 snapshot.String() MUST NOT contain api_key
	if strings.Contains(s.Snapshot().String(), "sk-secret-xyz") {
		t.Errorf("snapshot.String leaked api_key")
	}
}

func TestSaveConfigFile_JSONShape(t *testing.T) {
	dataDir := t.TempDir()
	cfg := validCfg()
	if err := saveConfigFile(dataDir, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(ConfigPath(dataDir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"provider", "api_key", "base_url", "model"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in file", k)
		}
	}
}

// TestStore_ConcurrentPutPutRace 并发 PUT+PUT：内存快照与磁盘最终一致（-race）。
func TestStore_ConcurrentPutPutRace(t *testing.T) {
	dataDir := t.TempDir()
	s := LoadStore(dataDir)

	const writers = 8
	const iters = 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c := validCfg()
				c.Model = "model-" + itoa(id) + "-" + itoa(i)
				c.APIKey = "sk-key-" + itoa(id*1000+i)
				if err := s.Put(c); err != nil {
					t.Errorf("put: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// 最终：内存快照 == 磁盘内容
	diskCfg, ok, err := loadConfigFile(dataDir)
	if err != nil || !ok {
		t.Fatalf("load disk: ok=%v err=%v", ok, err)
	}
	memCfg := s.Config()
	if diskCfg != memCfg {
		t.Errorf("mem/disk mismatch: mem=%+v disk=%+v", memCfg, diskCfg)
	}
	if !s.Configured() {
		t.Errorf("should be configured after successful puts")
	}
}

// TestStore_ConcurrentPutReadRace 并发 PUT + 快照读：-race 不报 data race。
func TestStore_ConcurrentPutReadRace(t *testing.T) {
	dataDir := t.TempDir()
	s := LoadStore(dataDir)
	_ = s.Put(validCfg())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// readers
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.Snapshot()
				_ = s.Configured()
				_ = s.Config()
				_ = s.LoadError()
			}
		}()
	}

	// writers
	const iters = 100
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c := validCfg()
				c.Model = "m-" + itoa(id) + "-" + itoa(i)
				_ = s.Put(c)
			}
		}(w)
	}

	// 等待 writers 完成
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	// 最终一致性
	diskCfg, ok, err := loadConfigFile(dataDir)
	if err != nil || !ok {
		t.Fatalf("load disk: ok=%v err=%v", ok, err)
	}
	if diskCfg != s.Config() {
		t.Errorf("final mismatch")
	}
}

func itoa(n int) string {
	// 简易 itoa 避免额外 import（strconv 已在 file 中未引入）。
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}