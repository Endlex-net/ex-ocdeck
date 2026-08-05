package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/ai"
)

// newAIConfigAPIServer 构造注入了 ai.Store 的 Server（路由注册前注入，沿用 main wiring 顺序）。
func newAIConfigAPIServer(t *testing.T, store *ai.Store) *Server {
	t.Helper()
	cfg := testConfig()
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
		aiConfig:  store,
	}
	s.registerRoutes()
	return s
}

// mustParseAIConfigDTO 解码响应体为 aiConfigDTO，失败 t.Fatal。
func mustParseAIConfigDTO(t *testing.T, body interface{ Read(p []byte) (int, error) }) aiConfigDTO {
	t.Helper()
	var dto aiConfigDTO
	if err := json.NewDecoder(body).Decode(&dto); err != nil {
		t.Fatalf("decode ai config dto: %v", err)
	}
	return dto
}

func mustParseErrorBody(t *testing.T, body interface{ Read(p []byte) (int, error) }) errorBody {
	t.Helper()
	var eb errorBody
	if err := json.NewDecoder(body).Decode(&eb); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return eb
}

// --- GET 掩码 / 未配置 / 损坏 ---

func TestAIConfig_GET_Masked_LongKey(t *testing.T) {
	dir := t.TempDir()
	mustWriteAIConfig(t, dir, ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "sk-1234567890abcdef", Model: "gpt-4o", BaseURL: "",
	})
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	dto := mustParseAIConfigDTO(t, resp.Body)
	if !dto.Configured {
		t.Error("configured = false, want true")
	}
	if dto.Provider != "openai" {
		t.Errorf("provider = %q, want openai", dto.Provider)
	}
	if dto.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", dto.Model)
	}
	if dto.APIKeyMasked != "sk-1***" {
		t.Errorf("api_key_masked = %q, want sk-1***", dto.APIKeyMasked)
	}
	if strings.Contains(dto.APIKeyMasked, "234567890") {
		t.Error("api_key_masked leaks key tail")
	}
	if dto.LoadError != "" {
		t.Errorf("load_error = %q, want empty", dto.LoadError)
	}
}

func TestAIConfig_GET_Masked_ShortKey(t *testing.T) {
	dir := t.TempDir()
	mustWriteAIConfig(t, dir, ai.ProviderConfig{
		Provider: ai.ProviderAnthropic, APIKey: "abcd", Model: "claude-3", BaseURL: "",
	})
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	dto := mustParseAIConfigDTO(t, resp.Body)
	if dto.APIKeyMasked != "***" {
		t.Errorf("api_key_masked = %q, want ***", dto.APIKeyMasked)
	}
	if !dto.Configured {
		t.Error("configured = false, want true (key 非空 model 非空)")
	}
}

func TestAIConfig_GET_NoKey(t *testing.T) {
	dir := t.TempDir()
	// 文件存在但 key 空：configured=false（key 缺失），api_key_masked 为空串。
	mustWriteAIConfig(t, dir, ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "", Model: "gpt-4o", BaseURL: "",
	})
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	dto := mustParseAIConfigDTO(t, resp.Body)
	if dto.Configured {
		t.Error("configured = true, want false (no key)")
	}
	if dto.APIKeyMasked != "" {
		t.Errorf("api_key_masked = %q, want empty for no key", dto.APIKeyMasked)
	}
}

func TestAIConfig_GET_NotConfigured_FileMissing(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	dto := mustParseAIConfigDTO(t, resp.Body)
	if dto.Configured {
		t.Error("configured = true, want false")
	}
	if dto.Provider != "" || dto.BaseURL != "" || dto.Model != "" || dto.APIKeyMasked != "" {
		t.Errorf("unconfigured dto = %+v, want all empty", dto)
	}
	if dto.LoadError != "" {
		t.Errorf("load_error = %q, want empty when file missing", dto.LoadError)
	}
}

func TestAIConfig_GET_CorruptFile_LoadError_No500(t *testing.T) {
	dir := t.TempDir()
	// 写入非法 JSON，模拟损坏。
	if err := os.WriteFile(filepath.Join(dir, "ai.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (MUST NOT 500)", resp.StatusCode)
	}
	dto := mustParseAIConfigDTO(t, resp.Body)
	if dto.Configured {
		t.Error("configured = true, want false on corrupt file")
	}
	if dto.LoadError == "" {
		t.Error("load_error empty, want human-readable message")
	}
}

func TestAIConfig_GET_NilStore_Degrades(t *testing.T) {
	s := newAIConfigAPIServer(t, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	dto := mustParseAIConfigDTO(t, resp.Body)
	if dto.Configured {
		t.Error("configured = true, want false (nil store)")
	}
}

// --- PUT 校验 422 分支 ---

func TestAIConfig_PUT_InvalidProvider_422(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"provider":"azure","api_key":"sk-1234567890abcdef","base_url":"","model":"gpt-4o"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
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
}

func TestAIConfig_PUT_EmptyModel_422(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"provider":"openai","api_key":"sk-1234567890abcdef","base_url":"","model":""}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
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
}

func TestAIConfig_PUT_InvalidBaseURL_422(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"provider":"openai","api_key":"sk-1234567890abcdef","base_url":"ftp://x","model":"gpt-4o"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
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
}

func TestAIConfig_PUT_NoPrevKey_EmptyKey_422(t *testing.T) {
	dir := t.TempDir() // 无 ai.json，无旧 key
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"provider":"openai","api_key":"","base_url":"","model":"gpt-4o"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
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
	// 未落盘
	if _, err := os.Stat(filepath.Join(dir, "ai.json")); err == nil {
		t.Error("ai.json written despite 422")
	}
}

func TestAIConfig_PUT_NoPrevKey_MaskedKey_422(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"provider":"openai","api_key":"sk-1***","base_url":"","model":"gpt-4o"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

// --- 保留原 key / 成功 / 热更新 ---

func TestAIConfig_PUT_MaskedKey_PreservesPrevKey(t *testing.T) {
	dir := t.TempDir()
	mustWriteAIConfig(t, dir, ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "sk-orig1234567890", Model: "gpt-4o", BaseURL: "",
	})
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 仅改 model，api_key 提交掩码值 → 保留原 key。
	body := `{"provider":"openai","api_key":"sk-o***","base_url":"","model":"gpt-4o-mini"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	dto := mustParseAIConfigDTO(t, resp.Body)
	if dto.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini", dto.Model)
	}
	if !dto.Configured {
		t.Error("configured = false, want true after PUT")
	}
	// 重新 GET：仍配置，掩码基于原 key 前 4 位。
	getResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	getDTO := mustParseAIConfigDTO(t, getResp.Body)
	if getDTO.APIKeyMasked != "sk-o***" {
		t.Errorf("api_key_masked = %q, want sk-o*** (prev key preserved)", getDTO.APIKeyMasked)
	}
}

func TestAIConfig_PUT_EmptyKey_PreservesPrevKey(t *testing.T) {
	dir := t.TempDir()
	mustWriteAIConfig(t, dir, ai.ProviderConfig{
		Provider: ai.ProviderAnthropic, APIKey: "sk-ant-abcdef123456", Model: "claude-3", BaseURL: "",
	})
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"provider":"anthropic","api_key":"","base_url":"https://gw.local","model":"claude-3.5"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	dto := mustParseAIConfigDTO(t, resp.Body)
	if dto.BaseURL != "https://gw.local" {
		t.Errorf("base_url = %q, want https://gw.local", dto.BaseURL)
	}
	if !dto.Configured {
		t.Error("configured = false, want true")
	}
}

func TestAIConfig_PUT_Success_SameShape200(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"provider":"openai","api_key":"sk-newkey12345678","base_url":"","model":"gpt-4o"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	dto := mustParseAIConfigDTO(t, resp.Body)
	if !dto.Configured {
		t.Error("configured = false, want true")
	}
	if dto.Provider != "openai" || dto.Model != "gpt-4o" || dto.APIKeyMasked != "sk-n***" {
		t.Errorf("dto = %+v, want configured openai/gpt-4o/sk-n***", dto)
	}
	if dto.LoadError != "" {
		t.Errorf("load_error = %q, want empty after success", dto.LoadError)
	}
}

func TestAIConfig_PUT_HotUpdate_ReflectedByGET(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	put := func(body string) {
		t.Helper()
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
		}
	}

	put(`{"provider":"openai","api_key":"sk-first12345678","base_url":"","model":"gpt-4o"}`)
	put(`{"provider":"anthropic","api_key":"sk-second1234567","base_url":"https://gw.local","model":"claude-3"}`)

	getResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	dto := mustParseAIConfigDTO(t, getResp.Body)
	if dto.Provider != "anthropic" || dto.Model != "claude-3" || dto.BaseURL != "https://gw.local" {
		t.Errorf("after hot update dto = %+v, want anthropic/claude-3/gw.local", dto)
	}
	if dto.APIKeyMasked != "sk-s***" {
		t.Errorf("api_key_masked = %q, want sk-s***", dto.APIKeyMasked)
	}
}

// --- nil store 防御 ---

func TestAIConfig_PUT_NilStore_500(t *testing.T) {
	s := newAIConfigAPIServer(t, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"provider":"openai","api_key":"sk-1234567890abcdef","base_url":"","model":"gpt-4o"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
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

// --- maskAPIKey 单元 ---

func TestMaskAPIKey(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"short", "***"},
		{"1234567", "***"},     // len 7
		{"12345678", "1234***"}, // len 8
		{"sk-abcdef123456", "sk-a***"},
	}
	for _, c := range cases {
		if got := maskAPIKey(c.in); got != c.want {
			t.Errorf("maskAPIKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- thinking 字段：PUT 非法值 422、GET 返回当前值 ---

func TestAIConfig_PUT_ThinkingInvalid_422(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	cases := []string{`"auto"`, `"  "`, `"HIGH"`, `"medium-low"`}
	for _, thinkingVal := range cases {
		body := `{"provider":"openai","api_key":"sk-1234567890abcdef","base_url":"","model":"gpt-4o","thinking":` + thinkingVal + `}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("thinking=%s: status=%d want 422", thinkingVal, resp.StatusCode)
		}
	}
}

func TestAIConfig_PUT_ThinkingValid_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	for _, thinking := range []string{"", "off", "low", "medium", "high"} {
		thinkingJSON := `""`
		if thinking != "" {
			thinkingJSON = `"` + thinking + `"`
		}
		body := `{"provider":"openai","api_key":"sk-1234567890abcdef","base_url":"","model":"gpt-4o","thinking":` + thinkingJSON + `}`
		resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("thinking=%q: PUT status=%d want 200", thinking, resp.StatusCode)
		}
		dto := mustParseAIConfigDTO(t, resp.Body)
		resp.Body.Close()
		if dto.Thinking != thinking {
			t.Errorf("thinking=%q: PUT response thinking=%q want %q", thinking, dto.Thinking, thinking)
		}
		// GET 返回相同值
		getResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
		if err != nil {
			t.Fatal(err)
		}
		getDTO := mustParseAIConfigDTO(t, getResp.Body)
		getResp.Body.Close()
		if getDTO.Thinking != thinking {
			t.Errorf("thinking=%q: GET thinking=%q want %q", thinking, getDTO.Thinking, thinking)
		}
	}
}

func TestAIConfig_GET_ThinkingDefaultEmpty(t *testing.T) {
	dir := t.TempDir()
	// 写入不含 thinking 字段的旧格式配置 → 默认空串。
	mustWriteAIConfig(t, dir, ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "sk-1234567890abcdef", Model: "gpt-4o", BaseURL: "",
	})
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	dto := mustParseAIConfigDTO(t, resp.Body)
	if dto.Thinking != "" {
		t.Errorf("default thinking=%q want empty", dto.Thinking)
	}
}

// mustWriteAIConfig 写入合法 ai.json（测试预置已配置态）。
func mustWriteAIConfig(t *testing.T, dir string, cfg ai.ProviderConfig) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- 空白 api_key（trim 后为空）应 422，而非 500 ---

func TestAIConfig_PUT_NoPrevKey_WhitespaceKey_422(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"provider":"openai","api_key":"   ","base_url":"","model":"gpt-4o"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
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
	// 未落盘
	if _, err := os.Stat(filepath.Join(dir, "ai.json")); err == nil {
		t.Error("ai.json written despite 422 on whitespace key")
	}
}

func TestAIConfig_PUT_PrevKey_WhitespaceKey_422(t *testing.T) {
	dir := t.TempDir()
	// 已有旧 key
	mustWriteAIConfig(t, dir, ai.ProviderConfig{
		Provider: ai.ProviderOpenAI, APIKey: "sk-orig1234567890", Model: "gpt-4o", BaseURL: "",
	})
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 即便有旧 key，提交纯空白的新 key 也应视为非法（不应误走「保留旧 key」分支）。
	body := `{"provider":"openai","api_key":"   ","base_url":"","model":"gpt-4o"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
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
}

// --- DTO 单次快照读一致性（并发 PUT + GET 不会字段混配） ---

func TestAIConfig_GET_ConcurrentPutNoMixedDTO(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	s := newAIConfigAPIServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 两套合法配置，合法的 (provider, model, base_url, masked) 组合。
	pairA := `{"provider":"openai","api_key":"sk-aaaaaaaaaa","base_url":"https://a.local","model":"gpt-4o"}`
	pairB := `{"provider":"anthropic","api_key":"sk-bbbbbbbbbb","base_url":"https://b.local","model":"claude-3"}`

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mixed atomic.Bool
	mixed.Store(false)

	// writer: 交替 PUT
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				body := pairA
				if id%2 == 0 {
					body = pairB
				}
				resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/ai/config", body))
				if err != nil {
					continue
				}
				resp.Body.Close()
			}
		}(w)
	}

	// reader: GET 并校验字段同属一套配置
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
				resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/ai/config", ""))
				if err != nil {
					continue
				}
				dto := mustParseAIConfigDTO(t, resp.Body)
				resp.Body.Close()
				// 合法组合：要么 A 套，要么 B 套，要么全空（未配置/损坏）。
				isA := dto.Provider == "openai" && dto.Model == "gpt-4o" && dto.BaseURL == "https://a.local"
				isB := dto.Provider == "anthropic" && dto.Model == "claude-3" && dto.BaseURL == "https://b.local"
				isEmpty := dto.Provider == "" && dto.Model == "" && dto.BaseURL == ""
				if !isA && !isB && !isEmpty {
					mixed.Store(true)
				}
			}
		}()
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	if mixed.Load() {
		t.Errorf("DTO field mixing detected under concurrent PUT + GET")
	}
}