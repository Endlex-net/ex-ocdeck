package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/domain/notification"
)

func writeRawNotificationConfig(t *testing.T, dataDir, content string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir dataDir: %v", err)
	}
	if err := os.WriteFile(ConfigPath(dataDir), []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// validNotificationConfig 一份合法已启用 bark 的配置（token 足够长以便掩码断言）。
func validNotificationConfig() notification.Config {
	c := notification.DefaultConfig()
	c.Enabled = true
	c.Channels.Bark.Enabled = true
	c.Channels.Bark.Token = "bark-token-123456"
	return c
}

// --- LoadStore ---

func TestLoadStore_FileNotExists_Defaults(t *testing.T) {
	s := LoadStore(t.TempDir())
	st := s.State()
	if st.Config != notification.DefaultConfig() {
		t.Fatalf("missing file must yield defaults, got %+v", st.Config)
	}
	if st.LoadErr != nil {
		t.Fatalf("missing file must not set loadErr, got %v", st.LoadErr)
	}
}

func TestLoadStore_Valid(t *testing.T) {
	dataDir := t.TempDir()
	cfg := validNotificationConfig()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeRawNotificationConfig(t, dataDir, string(raw))
	s := LoadStore(dataDir)
	st := s.State()
	if st.LoadErr != nil {
		t.Fatalf("no load err expected, got %v", st.LoadErr)
	}
	if st.Config != cfg {
		t.Fatalf("config mismatch: got %+v want %+v", st.Config, cfg)
	}
}

func TestLoadStore_CorruptJSON_DefaultAndLoadErr(t *testing.T) {
	dataDir := t.TempDir()
	writeRawNotificationConfig(t, dataDir, "{not json")
	s := LoadStore(dataDir)
	st := s.State()
	if st.LoadErr == nil {
		t.Fatal("corrupt json must set loadErr")
	}
	if st.Config != notification.DefaultConfig() {
		t.Fatalf("corrupt json must degrade to defaults, got %+v", st.Config)
	}
}

// TestLoadStore_InvalidIdleTimeout 文件可解析但阈值越界 → loadErr + 默认配置
// （fail-safe：通知按默认总开关关闭运行）。
func TestLoadStore_InvalidIdleTimeout(t *testing.T) {
	dataDir := t.TempDir()
	cfg := validNotificationConfig()
	cfg.IdleTimeoutSeconds = 5
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeRawNotificationConfig(t, dataDir, string(raw))
	s := LoadStore(dataDir)
	st := s.State()
	if st.LoadErr == nil {
		t.Fatal("out-of-range idle timeout must set loadErr")
	}
	if st.Config != notification.DefaultConfig() {
		t.Fatalf("invalid config must degrade to defaults, got %+v", st.Config)
	}
}

// TestLoadStore_UnknownFieldsIgnored 文件含 schema 外字段：解码不失败，已知字段生效。
func TestLoadStore_UnknownFieldsIgnored(t *testing.T) {
	dataDir := t.TempDir()
	writeRawNotificationConfig(t, dataDir, `{"enabled":true,"future_top":1,"categories":{"question":true,"permission":true,"idle":true,"retry":true,"error":true},"idle_timeout_seconds":90,"channels":{"web":{"enabled":true},"bark":{"enabled":false,"endpoint":"https://api.day.app","token":""},"macos":{"enabled":false}},"llm_summary":false,"base_url":""}`)
	s := LoadStore(dataDir)
	st := s.State()
	if st.LoadErr != nil {
		t.Fatalf("unknown fields must be ignored, got loadErr %v", st.LoadErr)
	}
	if !st.Config.Enabled || st.Config.IdleTimeoutSeconds != 90 || !st.Config.Channels.Web.Enabled {
		t.Fatalf("known fields mismatch: %+v", st.Config)
	}
}

// --- Put ---

// TestPut_RoundTrip_Atomic0600 保存合法配置：0600 权限原子落盘（无临时文件残留）、
// 快照替换、往返一致。
func TestPut_RoundTrip_Atomic0600(t *testing.T) {
	dataDir := t.TempDir()
	s := LoadStore(dataDir)
	cfg := validNotificationConfig()
	if err := s.Put(cfg); err != nil {
		t.Fatalf("put: %v", err)
	}
	st := s.State()
	if st.LoadErr != nil || st.Config != cfg {
		t.Fatalf("snapshot after put: %+v (loadErr=%v)", st.Config, st.LoadErr)
	}
	info, err := os.Stat(ConfigPath(dataDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm=%o want 0600", perm)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dataDir, ".notification-tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("atomic write must not leave temp files: %v", leftovers)
	}
}

// TestPut_ClearsLoadError 损坏文件加载后再保存成功：loadErr 清零、快照替换。
func TestPut_ClearsLoadError(t *testing.T) {
	dataDir := t.TempDir()
	writeRawNotificationConfig(t, dataDir, "garbage")
	s := LoadStore(dataDir)
	if s.State().LoadErr == nil {
		t.Fatal("prereq: corrupt seed must set loadErr")
	}
	if err := s.Put(validNotificationConfig()); err != nil {
		t.Fatalf("put after corrupt load: %v", err)
	}
	if s.State().LoadErr != nil {
		t.Fatalf("loadErr must clear after successful put, got %v", s.State().LoadErr)
	}
}

// TestPut_TokenMerge 掩码/空 token 保留已存储原值；新明文 token 替换。
func TestPut_TokenMerge(t *testing.T) {
	s := LoadStore(t.TempDir())
	old := validNotificationConfig()
	if err := s.Put(old); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	masked := validNotificationConfig()
	masked.Channels.Bark.Token = "bark***"
	if err := s.Put(masked); err != nil {
		t.Fatalf("put masked token: %v", err)
	}
	if got := s.State().Config.Channels.Bark.Token; got != old.Channels.Bark.Token {
		t.Fatalf("masked token must preserve stored token, got %q want %q", got, old.Channels.Bark.Token)
	}

	empty := validNotificationConfig()
	empty.Channels.Bark.Token = ""
	if err := s.Put(empty); err != nil {
		t.Fatalf("put empty token: %v", err)
	}
	if got := s.State().Config.Channels.Bark.Token; got != old.Channels.Bark.Token {
		t.Fatalf("empty token must preserve stored token, got %q want %q", got, old.Channels.Bark.Token)
	}

	refreshed := validNotificationConfig()
	refreshed.Channels.Bark.Token = "new-token-9876"
	if err := s.Put(refreshed); err != nil {
		t.Fatalf("put new token: %v", err)
	}
	if got := s.State().Config.Channels.Bark.Token; got != "new-token-9876" {
		t.Fatalf("new plaintext token must replace stored token, got %q", got)
	}
}

// TestPut_MaskedTokenNoStoredTokenNotRejected 无已存储 token（损坏态不可恢复）时
// 掩码提交按空处理，不因此拒绝保存（spec「通知配置读写 API」）。
func TestPut_MaskedTokenNoStoredTokenNotRejected(t *testing.T) {
	dataDir := t.TempDir()
	writeRawNotificationConfig(t, dataDir, "garbage")
	s := LoadStore(dataDir)
	incoming := validNotificationConfig()
	incoming.Channels.Bark.Token = "***"
	if err := s.Put(incoming); err != nil {
		t.Fatalf("masked token without stored token must not be rejected: %v", err)
	}
	if got := s.State().Config.Channels.Bark.Token; got != "" {
		t.Fatalf("token must be empty when nothing stored, got %q", got)
	}
}

// TestPut_InvalidRejected_NoWrite 校验失败：返回错误、旧快照不变、无文件写入。
func TestPut_InvalidRejected_NoWrite(t *testing.T) {
	s := LoadStore(t.TempDir())
	before := s.State()

	bad := validNotificationConfig()
	bad.IdleTimeoutSeconds = 3601
	if err := s.Put(bad); err == nil {
		t.Fatal("out-of-range idle timeout must be rejected")
	}
	badURL := validNotificationConfig()
	badURL.Channels.Bark.Endpoint = "https://api.day.app/push"
	if err := s.Put(badURL); err == nil {
		t.Fatal("illegal endpoint must be rejected")
	}
	if got := s.State(); got != before {
		t.Fatalf("snapshot must be unchanged after rejected puts, got %+v want %+v", got, before)
	}
	if _, err := os.Stat(ConfigPath(s.dataDir)); !os.IsNotExist(err) {
		t.Fatalf("no config file should exist after rejected puts, stat err=%v", err)
	}
}

// TestPut_WriteFailureKeepsSnapshot 写文件失败：返回 error，内存快照保持旧值。
func TestPut_WriteFailureKeepsSnapshot(t *testing.T) {
	dir := t.TempDir()
	s := LoadStore(dir)
	if err := s.Put(validNotificationConfig()); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	old := s.State()

	roDir := filepath.Join(dir, "readonly")
	if err := os.MkdirAll(roDir, 0o500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	s.dataDir = roDir
	incoming := validNotificationConfig()
	incoming.IdleTimeoutSeconds = 300
	if err := s.Put(incoming); err == nil {
		t.Fatal("write to read-only dir should fail")
	}
	if got := s.State(); got != old {
		t.Fatalf("snapshot must be unchanged after write failure, got %+v want %+v", got, old)
	}
}

// TestPut_ConcurrentSerialization 并发 PUT 由写锁串行化：最终内存快照 == 磁盘内容，
// 终值为某一次写入的完整配置（last-writer-wins，无字段混配）。
func TestPut_ConcurrentSerialization(t *testing.T) {
	dataDir := t.TempDir()
	s := LoadStore(dataDir)

	const writers = 8
	const iters = 25
	writtenIdle := make(map[int]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c := validNotificationConfig()
				c.IdleTimeoutSeconds = 10 + (id*37+i)%3591
				if err := s.Put(c); err != nil {
					t.Errorf("put: %v", err)
					return
				}
				mu.Lock()
				writtenIdle[c.IdleTimeoutSeconds] = true
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	st := s.State()
	if st.LoadErr != nil {
		t.Fatalf("loadErr after successful puts: %v", st.LoadErr)
	}
	diskSt := LoadStore(dataDir).State()
	if diskSt.Config != st.Config {
		t.Fatalf("mem/disk mismatch: mem=%+v disk=%+v", st.Config, diskSt.Config)
	}
	mu.Lock()
	defer mu.Unlock()
	if !writtenIdle[st.Config.IdleTimeoutSeconds] {
		t.Fatalf("final idle timeout %d is not any single written value (torn write)", st.Config.IdleTimeoutSeconds)
	}
}

// requiredConfigKeys 磁盘 schema 全部必填键（spec「通知配置存储」：字段均为必填键），
// 路径点分表示，用于生成缺键/null 变体。
var requiredConfigKeys = []string{
	"enabled",
	"categories", "categories.question", "categories.permission", "categories.idle",
	"categories.retry", "categories.error",
	"idle_timeout_seconds",
	"channels", "channels.web", "channels.web.enabled",
	"channels.bark", "channels.bark.enabled", "channels.bark.endpoint", "channels.bark.token",
	"channels.macos", "channels.macos.enabled",
	"llm_summary",
	"base_url",
}

// mutateRawConfig 对完整合法配置 JSON 按点分路径删除或置 null 键，返回变体 JSON。
func mutateRawConfig(t *testing.T, action string, keyPath string) string {
	t.Helper()
	var root map[string]interface{}
	if err := json.Unmarshal([]byte(fullConfigJSON), &root); err != nil {
		t.Fatalf("base json: %v", err)
	}
	segs := strings.Split(keyPath, ".")
	m := root
	for _, s := range segs[:len(segs)-1] {
		next, ok := m[s].(map[string]interface{})
		if !ok {
			t.Fatalf("path %q: segment %q not an object", keyPath, s)
		}
		m = next
	}
	last := segs[len(segs)-1]
	switch action {
	case "delete":
		delete(m, last)
	case "null":
		m[last] = nil
	default:
		t.Fatalf("unknown action %q", action)
	}
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal variant: %v", err)
	}
	return string(out)
}

// fullConfigJSON 一份含全部必填键的合法配置原文（掩码合并语义不影响解码）。
const fullConfigJSON = `{"enabled":true,"categories":{"question":true,"permission":true,"idle":true,"retry":true,"error":true},"idle_timeout_seconds":120,"channels":{"web":{"enabled":false},"bark":{"enabled":true,"endpoint":"https://api.day.app","token":"bark-token-123456"},"macos":{"enabled":false}},"llm_summary":false,"base_url":""}`

// TestLoadStore_MissingOrNullRequiredKeys 每个必填键缺失或为 null → 视为损坏：
// 默认配置 + loadErr，不拒绝启动（spec「通知配置存储」字段均为必填键）。
func TestLoadStore_MissingOrNullRequiredKeys(t *testing.T) {
	for _, key := range requiredConfigKeys {
		for _, action := range []string{"delete", "null"} {
			name := action + " " + key
			t.Run(name, func(t *testing.T) {
				dataDir := t.TempDir()
				writeRawNotificationConfig(t, dataDir, mutateRawConfig(t, action, key))
				s := LoadStore(dataDir)
				st := s.State()
				if st.LoadErr == nil {
					t.Fatalf("missing/null required key %q must set loadErr", key)
				}
				if st.Config != notification.DefaultConfig() {
					t.Fatalf("invalid config must degrade to defaults, got %+v", st.Config)
				}
			})
		}
	}
}

// TestDecodeConfig 导出的 schema 解码规则：完整合法 → 成功；未知字段忽略；
// 类型不匹配 → 错误（供 PUT 路径复用同一规则）。
func TestDecodeConfig(t *testing.T) {
	cfg, err := DecodeConfig([]byte(fullConfigJSON))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cfg.Enabled || cfg.IdleTimeoutSeconds != 120 || !cfg.Channels.Bark.Enabled ||
		cfg.Channels.Bark.Token != "bark-token-123456" || !cfg.Categories.Retry {
		t.Fatalf("decoded fields mismatch: %+v", cfg)
	}

	// 未知字段忽略（顶层与嵌套）。
	withUnknown := `{"enabled":false,"future_top":1,"categories":{"question":true,"permission":true,"idle":true,"retry":true,"error":true,"future_cat":2},"idle_timeout_seconds":60,"channels":{"web":{"enabled":false},"bark":{"enabled":false,"endpoint":"https://api.day.app","token":"","future":"x"},"macos":{"enabled":false}},"llm_summary":false,"base_url":""}`
	if _, err := DecodeConfig([]byte(withUnknown)); err != nil {
		t.Fatalf("unknown fields must be ignored: %v", err)
	}

	// 类型不匹配 → 解码错误。
	badType := `{"enabled":"yes","categories":{"question":true,"permission":true,"idle":true,"retry":true,"error":true},"idle_timeout_seconds":60,"channels":{"web":{"enabled":false},"bark":{"enabled":false,"endpoint":"https://api.day.app","token":""},"macos":{"enabled":false}},"llm_summary":false,"base_url":""}`
	if _, err := DecodeConfig([]byte(badType)); err == nil {
		t.Fatal("type mismatch must fail decode")
	}
	if _, err := DecodeConfig([]byte("{not json")); err == nil {
		t.Fatal("corrupt json must fail decode")
	}

	// 缺必填键 → 错误（每键逐一覆盖见 TestLoadStore_MissingOrNullRequiredKeys，
	// 此处抽样直接断言 DecodeConfig 契约）。
	if _, err := DecodeConfig([]byte(`{"enabled":false}`)); err == nil {
		t.Fatal("missing required keys must fail decode")
	}
}

// --- 掩码 ---

// TestMaskToken 掩码规则与 ai-provider-config 的 api_key 一致（spec「通知配置读写 API」）：
// ≥8 位回显前 4 位 + `***`；<8 位纯 `***`；无 token 为空串。
func TestMaskToken(t *testing.T) {
	cases := []struct{ token, want string }{
		{"", ""},
		{"short", "***"},
		{"1234567", "***"},
		{"12345678", "1234***"},
		{"bark-token-123456", "bark***"},
	}
	for _, c := range cases {
		if got := MaskToken(c.token); got != c.want {
			t.Errorf("MaskToken(%q) = %q, want %q", c.token, got, c.want)
		}
	}
}

// TestMaskedTokenNotPlaintext 端到端：掩码输出 MUST NOT 含完整 token 明文。
func TestMaskedTokenNotPlaintext(t *testing.T) {
	token := "super-secret-bark-token"
	if masked := MaskToken(token); strings.Contains(masked, token) {
		t.Fatalf("masked token leaks plaintext: %q", masked)
	}
}
