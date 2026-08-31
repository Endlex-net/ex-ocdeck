// notification_config_test.go 通知配置 API 与测试通知端点测试（task-notifications
// Lane D 4.2/4.7；spec「通知配置读写 API」「测试通知」）：状态码矩阵（400/422/
// 500）、掩码（token_masked/无完整 token）、load_error 只读、并发 PUT last-
// writer-wins、测试通知 DTO 与总开关关闭 422、Listen/Serve 拆分语义。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	appnotification "ocdeck/internal/application/notification"
	"ocdeck/internal/domain/notification"
	"ocdeck/internal/infrastructure/notify"
)

// newNotificationConfigServer 构造注入 notify.Store 与测试通知 tester 的 Server
// （路由注册前注入，沿用 main wiring 顺序）。
func newNotificationConfigServer(t *testing.T, store *notify.Store, tester NotificationTester) *Server {
	t.Helper()
	cfg := testConfig()
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
		webHub:    newWebHub(),
	}
	s.SetNotificationStore(store)
	s.SetNotificationTester(tester)
	s.registerRoutes()
	return s
}

func newNotifierTester(channels []notification.Channel) *appnotification.Notifier {
	return appnotification.New(appnotification.Options{Channels: channels})
}

// mustParseNotificationConfigDTO 解码响应体为 notificationConfigDTO。
func mustParseNotificationConfigDTO(t *testing.T, body interface{ Read(p []byte) (int, error) }) notificationConfigDTO {
	t.Helper()
	var dto notificationConfigDTO
	if err := json.NewDecoder(body).Decode(&dto); err != nil {
		t.Fatalf("decode notification config dto: %v", err)
	}
	return dto
}

// validNotificationConfigJSON 一份完整合法的 PUT 请求体（总开关开 + bark 配置）。
func validNotificationConfigJSON(enabled bool) string {
	return fmt.Sprintf(`{
  "enabled": %t,
  "categories": {"question": true, "permission": true, "idle": true, "retry": true, "error": true},
  "idle_timeout_seconds": 60,
  "channels": {
    "web":   {"enabled": false},
    "bark":  {"enabled": true, "endpoint": "https://api.day.app", "token": "bark-token-1234567890"},
    "macos": {"enabled": false}
  },
  "llm_summary": false,
  "base_url": ""
}`, enabled)
}

// --- GET ---

func TestNotificationConfig_GET_DefaultsWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/notification/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	dto := mustParseNotificationConfigDTO(t, resp.Body)
	def := notification.DefaultConfig()
	if dto.Enabled {
		t.Error("enabled = true, want false (default)")
	}
	if dto.IdleTimeoutSeconds != def.IdleTimeoutSeconds {
		t.Errorf("idle_timeout_seconds = %d, want %d", dto.IdleTimeoutSeconds, def.IdleTimeoutSeconds)
	}
	if !dto.Categories.Question || !dto.Categories.Error {
		t.Errorf("default categories all on, got %+v", dto.Categories)
	}
	if dto.Channels.Bark.Endpoint != notification.DefaultBarkEndpoint {
		t.Errorf("bark endpoint = %q, want default", dto.Channels.Bark.Endpoint)
	}
	if dto.Channels.Bark.TokenMasked != "" {
		t.Errorf("token_masked = %q, want empty", dto.Channels.Bark.TokenMasked)
	}
	if dto.LoadError != "" {
		t.Errorf("load_error = %q, want empty when file missing", dto.LoadError)
	}
}

func TestNotificationConfig_GET_CorruptFileLoadErrorNo500(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notification.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/notification/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (MUST NOT 500)", resp.StatusCode)
	}
	dto := mustParseNotificationConfigDTO(t, resp.Body)
	if dto.LoadError == "" {
		t.Error("load_error empty, want human-readable message on corrupt file")
	}
	if dto.Enabled {
		t.Error("corrupt file must degrade to defaults (enabled=false)")
	}
}

func TestNotificationConfig_GET_MaskedToken(t *testing.T) {
	dir := t.TempDir()
	cfg := notification.DefaultConfig()
	cfg.Enabled = true
	cfg.Channels.Bark.Enabled = true
	cfg.Channels.Bark.Token = "bark-token-1234567890"
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "notification.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/notification/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAllString(t, resp.Body)
	if strings.Contains(body, "bark-token-1234567890") {
		t.Error("response MUST NOT contain full token")
	}
	dto := mustParseNotificationConfigDTO(t, strings.NewReader(body))
	if dto.Channels.Bark.TokenMasked != "bark***" {
		t.Errorf("token_masked = %q, want bark***", dto.Channels.Bark.TokenMasked)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestNotificationConfig_GET_NilStore_Degrades(t *testing.T) {
	s := newNotificationConfigServer(t, nil, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/notification/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	dto := mustParseNotificationConfigDTO(t, resp.Body)
	if dto.Enabled || dto.LoadError != "" {
		t.Errorf("nil store must degrade to defaults, got %+v", dto)
	}
}

// --- PUT 状态码矩阵 ---

func TestNotificationConfig_PUT_InvalidJSON_400(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", "{not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	eb := mustParseErrorBody(t, resp.Body)
	if eb.Error.Code != CodeInvalidInput {
		t.Errorf("code = %v, want invalid_input", eb.Error.Code)
	}
}

func TestNotificationConfig_PUT_MissingRequiredKey_422(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 缺 base_url 与 channels.macos。
	body := `{"enabled":false,"categories":{"question":true,"permission":true,"idle":true,"retry":true,"error":true},"idle_timeout_seconds":60,"channels":{"web":{"enabled":false},"bark":{"enabled":false,"endpoint":"https://api.day.app","token":""}},"llm_summary":false}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	eb := mustParseErrorBody(t, resp.Body)
	if eb.Error.Code != CodeInvalidInput {
		t.Errorf("code = %v, want invalid_input", eb.Error.Code)
	}
	// 未落盘。
	if _, err := os.Stat(filepath.Join(dir, "notification.json")); err == nil {
		t.Error("notification.json written despite 422")
	}
}

func TestNotificationConfig_PUT_IdleTimeoutOutOfRange_422(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := strings.Replace(validNotificationConfigJSON(false), `"idle_timeout_seconds": 60`, `"idle_timeout_seconds": 5`, 1)
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestNotificationConfig_PUT_InvalidBaseURL_422(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := strings.Replace(validNotificationConfigJSON(false), `"base_url": ""`, `"base_url": "ftp://x"`, 1)
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	// 原配置不变。
	dto := mustParseNotificationConfigDTO(t, resp.Body)
	_ = dto
	getResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/notification/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	getDTO := mustParseNotificationConfigDTO(t, getResp.Body)
	if getDTO.BaseURL != "" {
		t.Errorf("config changed after rejected PUT, base_url = %q", getDTO.BaseURL)
	}
}

func TestNotificationConfig_PUT_Success_SameShape200_MaskedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", validNotificationConfigJSON(true)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readAllString(t, resp.Body)
	if strings.Contains(body, "bark-token-1234567890") {
		t.Error("PUT response MUST NOT contain full token")
	}
	dto := mustParseNotificationConfigDTO(t, strings.NewReader(body))
	if !dto.Enabled {
		t.Error("enabled = false, want true after PUT")
	}
	if dto.Channels.Bark.TokenMasked != "bark***" {
		t.Errorf("token_masked = %q, want bark***", dto.Channels.Bark.TokenMasked)
	}
	if dto.LoadError != "" {
		t.Errorf("load_error = %q, want empty after successful PUT", dto.LoadError)
	}
}

// TestNotificationConfig_PUT_MaskedTokenPreservesStored token 语义（spec：空串
// 或含 *** 的字符串保留已存储原 token；无已存储 token 时按空处理不拒绝）。
func TestNotificationConfig_PUT_MaskedTokenPreservesStored(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	// 先落一份含 token 的合法配置。
	if err := store.Put(mustConfigWithToken("bark-token-orig-123")); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 提交掩码值（token 含 ***）→ 保留原 token。
	body := strings.Replace(validNotificationConfigJSON(true), "bark-token-1234567890", "bar***", 1)
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	dto := mustParseNotificationConfigDTO(t, resp.Body)
	if dto.Channels.Bark.TokenMasked != "bark***" {
		t.Errorf("token_masked = %q, want bark*** (original preserved)", dto.Channels.Bark.TokenMasked)
	}
}

// TestNotificationConfig_PUT_MaskedTokenNoStoredNotRejected 无已存储 token 时
// 掩码提交按空处理、不拒绝保存（spec token 语义）。
func TestNotificationConfig_PUT_MaskedTokenNoStoredNotRejected(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := strings.Replace(validNotificationConfigJSON(true), "bark-token-1234567890", "***", 1)
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (masked token without stored must not reject)", resp.StatusCode)
	}
	dto := mustParseNotificationConfigDTO(t, resp.Body)
	if dto.Channels.Bark.TokenMasked != "" {
		t.Errorf("token_masked = %q, want empty (no stored token)", dto.Channels.Bark.TokenMasked)
	}
}

// TestNotificationConfig_PUT_LoadErrorReadOnly PUT 含 load_error 字段按未知字段
// 忽略（只读）。
func TestNotificationConfig_PUT_LoadErrorReadOnly(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := strings.Replace(validNotificationConfigJSON(false), `"base_url": ""`, `"base_url": "", "load_error": "injected"`, 1)
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (load_error ignored as unknown field)", resp.StatusCode)
	}
	dto := mustParseNotificationConfigDTO(t, resp.Body)
	if dto.LoadError != "" {
		t.Errorf("load_error = %q, must not be settable via PUT", dto.LoadError)
	}
}

// TestNotificationConfig_PUT_NilStore_500 wiring 缺失（非客户端可修复）→ 500。
func TestNotificationConfig_PUT_NilStore_500(t *testing.T) {
	s := newNotificationConfigServer(t, nil, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", validNotificationConfigJSON(false)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	eb := mustParseErrorBody(t, resp.Body)
	if eb.Error.Code != CodeInvalidState {
		t.Errorf("code = %v, want invalid_state", eb.Error.Code)
	}
}

// TestNotificationConfig_PUT_WriteFailure_500KeepsSnapshot 真实写盘失败：
// dataDir 只读 → 500，内存快照保持 PUT 前值。
func TestNotificationConfig_PUT_WriteFailure_500KeepsSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	seed := mustConfigWithToken("bark-token-1234567890")
	seed.IdleTimeoutSeconds = 60
	if err := store.Put(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := strings.Replace(validNotificationConfigJSON(true), `"idle_timeout_seconds": 60`, `"idle_timeout_seconds": 300`, 1)
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	getResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/notification/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	dto := mustParseNotificationConfigDTO(t, getResp.Body)
	if dto.IdleTimeoutSeconds != 60 {
		t.Errorf("snapshot idle = %d, want 60 (unchanged after write failure)", dto.IdleTimeoutSeconds)
	}
}

// TestNotificationConfig_PUT_HotUpdate_ReflectedByGET PUT 成功后内存快照原子
// 替换，GET 立即返回新值（spec「配置运行时生效」）。
func TestNotificationConfig_PUT_HotUpdate_ReflectedByGET(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	put := func(body string) {
		t.Helper()
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
		}
	}
	put(validNotificationConfigJSON(false))
	put(strings.Replace(validNotificationConfigJSON(true), `"idle_timeout_seconds": 60`, `"idle_timeout_seconds": 300`, 1))

	getResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/notification/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	dto := mustParseNotificationConfigDTO(t, getResp.Body)
	if !dto.Enabled || dto.IdleTimeoutSeconds != 300 {
		t.Errorf("after hot update dto = %+v, want enabled/300s", dto)
	}
}

// TestNotificationConfig_PUT_ConcurrentLastWriterWins 并发 PUT 由写锁串行化：
// 终值为某一次完整写入（last-writer-wins，无字段混配）。
func TestNotificationConfig_PUT_ConcurrentLastWriterWins(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	const writers = 4
	const iters = 20
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				enabled := (id+i)%2 == 0
				resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/notification/config", validNotificationConfigJSON(enabled)))
				if err != nil {
					t.Errorf("put: %v", err)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("put status = %d, want 200", resp.StatusCode)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// 终值为完整一致的一套（enabled 与 categories/timeout 无混配）。
	getResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/notification/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	dto := mustParseNotificationConfigDTO(t, getResp.Body)
	if dto.IdleTimeoutSeconds != 60 {
		t.Errorf("idle_timeout_seconds = %d, want 60 (every writer writes 60)", dto.IdleTimeoutSeconds)
	}
	// 磁盘与内存一致。
	diskSt := notify.LoadStore(dir).State()
	if diskSt.Config.Enabled != dto.Enabled || diskSt.Config.IdleTimeoutSeconds != dto.IdleTimeoutSeconds {
		t.Errorf("mem/disk mismatch: mem enabled=%v disk enabled=%v", dto.Enabled, diskSt.Config.Enabled)
	}
}

// --- 测试通知端点 ---

// fakeTestChannel 测试通知渠道 fake（记录意图，可脚本化失败）。
type fakeTestChannel struct {
	mu   sync.Mutex
	name string
	fail bool
	sent []notification.Intent
}

func (c *fakeTestChannel) Name() string { return c.name }
func (c *fakeTestChannel) Caps() notification.Capability {
	return notification.CapGroup
}
func (c *fakeTestChannel) Send(_ context.Context, in notification.Intent, _ notification.ChannelConfig) notification.Result {
	c.mu.Lock()
	c.sent = append(c.sent, in)
	c.mu.Unlock()
	if c.fail {
		return notification.Result{OK: false, Err: "scripted failure"}
	}
	return notification.Result{OK: true}
}

func (c *fakeTestChannel) intents() []notification.Intent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]notification.Intent, len(c.sent))
	copy(out, c.sent)
	return out
}

func TestNotificationTest_MasterOff_422(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	// 默认总开关关闭。
	s := newNotificationConfigServer(t, store, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/notification/test", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	eb := mustParseErrorBody(t, resp.Body)
	if eb.Error.Code != CodeInvalidState {
		t.Errorf("code = %v, want invalid_state", eb.Error.Code)
	}
}

func TestNotificationTest_ResultsShapeAndChannelStatuses(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	cfg := notification.DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = "http://127.0.0.1:18080"
	cfg.Channels.Web.Enabled = true
	cfg.Channels.Bark.Enabled = true
	cfg.Channels.Bark.Token = "bark-token-1234567890"
	if err := store.Put(cfg); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	hub := newWebHub()
	web := notify.NewWebChannel(hub)
	bark := &fakeTestChannel{name: "bark", fail: true}
	macos := &fakeTestChannel{name: "macos"}
	s := newNotificationConfigServer(t, store, newNotifierTester([]notification.Channel{web, bark, macos}))
	s.webHub = hub
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	postTest := func() map[string]ChannelTestResult {
		t.Helper()
		resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/notification/test", ""))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body struct {
			Results []ChannelTestResult `json:"results"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		byName := map[string]ChannelTestResult{}
		for _, r := range body.Results {
			byName[r.Name] = r
		}
		return byName
	}

	// 无 SSE 连接：web failed。
	got := postTest()
	if r := got["web"]; r.Status != notification.ChannelStatusFailed {
		t.Errorf("web (no conn) = %+v, want failed", r)
	}
	if r := got["bark"]; r.Status != notification.ChannelStatusFailed || r.Error != "scripted failure" {
		t.Errorf("bark = %+v, want failed", r)
	}
	if r := got["macos"]; r.Status != notification.ChannelStatusSkipped || r.Error != "" {
		t.Errorf("macos = %+v, want skipped", r)
	}
	barkSent := bark.intents()
	if len(barkSent) != 1 {
		t.Fatalf("bark sent %d, want 1", len(barkSent))
	}
	in := barkSent[0]
	if in.TaskID != "notification-test" || in.TaskName != "ocdeck" ||
		in.Category != notification.CategoryTest || in.Level != notification.LevelActive ||
		in.Title != "[ocdeck] [ocdeck] 测试通知" || in.Body != "ocdeck 通知链路测试" {
		t.Errorf("test intent = %+v", in)
	}
	if !strings.HasSuffix(in.URL, "/#/configs#notifications") {
		t.Errorf("URL = %q", in.URL)
	}
	if n := len(macos.intents()); n != 0 {
		t.Errorf("macos sent %d, want 0", n)
	}

	// 有 SSE 连接：web success。
	stream := openNotificationsStream(t, ts.URL)
	defer stream.Body.Close()
	frames := startSSEFrameReader(stream.Body)
	nextFrame(t, frames, "connect comment")
	got = postTest()
	if r := got["web"]; r.Status != notification.ChannelStatusSuccess {
		t.Errorf("web (connected) = %+v, want success", r)
	}
}

func TestNotificationTest_NilStore_500(t *testing.T) {
	s := newNotificationConfigServer(t, nil, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/notification/test", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// TestNotificationTest_URLFallback base_url 配置覆盖优先于监听地址（design D8）。
func TestNotificationTest_URLFallback(t *testing.T) {
	dir := t.TempDir()
	store := notify.LoadStore(dir)
	cfg := notification.DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = "https://example.com/"
	cfg.Channels.Bark.Enabled = true
	cfg.Channels.Bark.Token = "bark-token-1234567890"
	if err := store.Put(cfg); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	ch := &fakeTestChannel{name: "bark"}
	s := newNotificationConfigServer(t, store, newNotifierTester([]notification.Channel{ch}))
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/notification/test", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := ch.intents(); len(got) != 1 || got[0].URL != "https://example.com/#/configs#notifications" {
		t.Errorf("intent URL = %+v, want base_url override with trailing slash trimmed", got)
	}
}

// --- Listen/Serve 拆分（design D8） ---

// closeListenerForTest 测试辅助：关闭 listener（避免端口泄漏阻塞后续测试）。
func (s *Server) closeListenerForTest() {
	s.lnMu.Lock()
	ln := s.listener
	s.listener = nil
	s.lnMu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

func TestListenServe_BoundAddrNilBeforeListen(t *testing.T) {
	s := newNotificationTestServer()
	if addr := s.BoundAddr(); addr != nil {
		t.Fatalf("BoundAddr before Listen = %v, want nil", addr)
	}
}

func TestListenServe_ServeWithoutListenErrors(t *testing.T) {
	s := newNotificationTestServer()
	if err := s.Serve(context.Background()); err == nil {
		t.Fatal("Serve without Listen must return error")
	}
}

func TestListenServe_DoubleListenErrors(t *testing.T) {
	s := newNotificationTestServer()
	if err := s.Listen(); err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer s.closeListenerForTest()
	if err := s.Listen(); err == nil {
		t.Fatal("second Listen must return error")
	}
}

func TestListenServe_BoundAddrAfterListen(t *testing.T) {
	cfg := testConfig()
	cfg.ListenAddr = "127.0.0.1"
	cfg.ListenPort = 0 // 系统分配端口
	s := newNotificationTestServer()
	s.cfg = cfg
	if err := s.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer s.closeListenerForTest()
	addr := s.BoundAddr()
	if addr == nil {
		t.Fatal("BoundAddr after Listen = nil, want actual address")
	}
	if _, port, err := net.SplitHostPort(addr.String()); err != nil || port == "0" {
		t.Errorf("bound addr %v must carry system-assigned port", addr)
	}
}

// TestNotificationBaseURL base 推导（design D8）：base_url 非空优先（剔除尾部 /）；
// 未配置时从 BoundAddr 推导（wildcard → 127.0.0.1）；均不可用 → error。
func TestNotificationBaseURL(t *testing.T) {
	s := newNotificationTestServer()

	cfg := notification.DefaultConfig()
	cfg.BaseURL = "https://example.com/"
	if got, err := s.NotificationBaseURL(cfg.BaseURL); err != nil || got != "https://example.com" {
		t.Errorf("configured base = %q err=%v, want https://example.com", got, err)
	}

	// 未 Listen 且未配置 → error（URL 不可用）。
	cfg.BaseURL = ""
	if _, err := s.NotificationBaseURL(cfg.BaseURL); err == nil {
		t.Error("base url must be unavailable before Listen without base_url")
	}

	// Listen 后从实际监听地址推导。
	l := &Server{
		cfg:       testConfig(),
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator("testtoken"),
		wsClients: newWSClientRegistry(),
		webHub:    newWebHub(),
	}
	l.cfg.ListenAddr = "127.0.0.1"
	l.cfg.ListenPort = 0
	if err := l.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.closeListenerForTest()
	got, err := l.NotificationBaseURL(cfg.BaseURL)
	if err != nil {
		t.Fatalf("base url after listen: %v", err)
	}
	if !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Errorf("derived base = %q, want http://127.0.0.1:<port>", got)
	}

	// wildcard ListenAddr → host 127.0.0.1，port 取 BoundAddr。
	w := &Server{
		cfg:       testConfig(),
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator("testtoken"),
		wsClients: newWSClientRegistry(),
		webHub:    newWebHub(),
	}
	w.cfg.ListenAddr = "0.0.0.0"
	w.cfg.ListenPort = 0
	if err := w.Listen(); err != nil {
		t.Fatalf("listen wildcard: %v", err)
	}
	defer w.closeListenerForTest()
	got, err = w.NotificationBaseURL(cfg.BaseURL)
	if err != nil {
		t.Fatalf("wildcard base: %v", err)
	}
	if !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Errorf("wildcard base = %q, want http://127.0.0.1:<port>", got)
	}

	// ListenAddr=localhost 时 URL 保持 localhost（port 仍取 BoundAddr）。
	h := &Server{
		cfg:       testConfig(),
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator("testtoken"),
		wsClients: newWSClientRegistry(),
		webHub:    newWebHub(),
	}
	h.cfg.ListenAddr = "localhost"
	h.cfg.ListenPort = 0
	if err := h.Listen(); err != nil {
		t.Fatalf("listen localhost: %v", err)
	}
	defer h.closeListenerForTest()
	got, err = h.NotificationBaseURL(cfg.BaseURL)
	if err != nil {
		t.Fatalf("localhost base: %v", err)
	}
	if !strings.HasPrefix(got, "http://localhost:") {
		t.Errorf("localhost base = %q, want http://localhost:<port>", got)
	}
}

// --- helpers ---

func mustConfigWithToken(token string) notification.Config {
	cfg := notification.DefaultConfig()
	cfg.Enabled = true
	cfg.Channels.Bark.Enabled = true
	cfg.Channels.Bark.Token = token
	return cfg
}

func readAllString(t *testing.T, r interface{ Read(p []byte) (int, error) }) string {
	t.Helper()
	data, err := io.ReadAll(r.(io.Reader))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}
