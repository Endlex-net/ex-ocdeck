package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	domainpalette "ocdeck/internal/domain/palette"
	"ocdeck/internal/infrastructure/palette"
)

func newPaletteConfigServer(t *testing.T, store *palette.Store) *Server {
	t.Helper()
	cfg := testConfig()
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
		webHub:    newWebHub(),
	}
	s.SetPaletteConfigStore(store)
	s.registerRoutes()
	return s
}

func validPaletteJSON() string {
	return `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`
}

// paletteConfigEqual Config/DTO 含 map 不可 ==，测试专用相等断言。
func paletteConfigEqual(a, b paletteConfigDTO) bool {
	return a.Hotkey == b.Hotkey && a.TriggerWord == b.TriggerWord && a.MatchMode == b.MatchMode &&
		maps.Equal(a.CommandTriggers, b.CommandTriggers)
}

func storeConfigEqual(a, b domainpalette.Config) bool {
	return a.Hotkey == b.Hotkey && a.TriggerWord == b.TriggerWord && a.MatchMode == b.MatchMode &&
		maps.Equal(a.CommandTriggers, b.CommandTriggers)
}

func TestPaletteConfig_GET_DefaultsWhenFileMissing(t *testing.T) {
	store := palette.LoadStore(t.TempDir())
	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/palette/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto paletteConfigDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.Hotkey != "mod+k" || dto.TriggerWord != "new" || dto.MatchMode != "exact-then-substring" {
		t.Fatalf("default GET mismatch: %+v", dto)
	}
	// GET 返回全部 8 键（cc/pro/reg + 5 空键），空串 = 未启用。
	if !maps.Equal(dto.CommandTriggers, domainpalette.DefaultCommandTriggers()) {
		t.Fatalf("default GET commandTriggers mismatch: %v", dto.CommandTriggers)
	}
}

func TestPaletteConfig_PUT_ThenGET(t *testing.T) {
	dir := t.TempDir()
	store := palette.LoadStore(dir)
	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	putResp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/palette/config", validPaletteJSON()))
	if err != nil {
		t.Fatal(err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", putResp.StatusCode)
	}
	var putDTO paletteConfigDTO
	if err := json.NewDecoder(putResp.Body).Decode(&putDTO); err != nil {
		t.Fatal(err)
	}
	if putDTO.Hotkey != "alt+k" || putDTO.TriggerWord != "create" || putDTO.MatchMode != "exact" {
		t.Fatalf("PUT 200 body mismatch: %+v", putDTO)
	}
	if putDTO.CommandTriggers[domainpalette.CommandIDCenter] != "cc" ||
		putDTO.CommandTriggers[domainpalette.CommandIDProjects] != "pr" ||
		putDTO.CommandTriggers[domainpalette.CommandIDRegisterProj] != "reg" {
		t.Fatalf("PUT 200 commandTriggers mismatch: %v", putDTO.CommandTriggers)
	}

	getResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/palette/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	var getDTO paletteConfigDTO
	if err := json.NewDecoder(getResp.Body).Decode(&getDTO); err != nil {
		t.Fatal(err)
	}
	if !paletteConfigEqual(getDTO, putDTO) {
		t.Fatalf("GET after PUT = %+v want %+v", getDTO, putDTO)
	}
}

func TestPaletteConfig_PUT_ErrorMatrix(t *testing.T) {
	dir := t.TempDir()
	store := palette.LoadStore(dir)
	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"empty body", "", http.StatusBadRequest},
		{"whitespace body", "   \n\t", http.StatusBadRequest},
		{"syntax error", "{not json", http.StatusBadRequest},
		{"trailing second JSON", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact"}{"x":1}`, http.StatusBadRequest},
		{"top-level array", `["alt+k"]`, http.StatusUnprocessableEntity},
		{"top-level string", `"mod+k"`, http.StatusUnprocessableEntity},
		{"top-level number", `1`, http.StatusUnprocessableEntity},
		{"top-level bool", `true`, http.StatusUnprocessableEntity},
		{"top-level null", `null`, http.StatusUnprocessableEntity},
		{"missing hotkey", `{"triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"missing triggerWord", `{"hotkey":"alt+k","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"missing matchMode", `{"hotkey":"alt+k","triggerWord":"create","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"missing commandTriggers", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"null hotkey", `{"hotkey":null,"triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"null triggerWord", `{"hotkey":"alt+k","triggerWord":null,"matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"null matchMode", `{"hotkey":"alt+k","triggerWord":"create","matchMode":null,"commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"null commandTriggers", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":null}`, http.StatusUnprocessableEntity},
		{"type error hotkey", `{"hotkey":1,"triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"type error triggerWord", `{"hotkey":"alt+k","triggerWord":true,"matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"type error matchMode", `{"hotkey":"alt+k","triggerWord":"create","matchMode":[],"commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers not object", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":["cc"]}`, http.StatusUnprocessableEntity},
		{"commandTriggers value type error", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":5,"projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers unknown ID", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","unknown-cmd":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers incomplete keys", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers value whitespace", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"p r","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers value 33 code points", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"` + strings.Repeat("你", 33) + `","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers dup exact", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"cc","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers dup fold Ä/ä", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"Ä","projects":"ä","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers dup fold İ/i+U+0307", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"İ","projects":"i̇","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers dup fold ΟΣ/ος", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"ΟΣ","projects":"ος","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"commandTriggers equals global triggerWord", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"CREATE","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"wrong-case Hotkey only", `{"Hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"wrong-case CommandTriggers only", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","CommandTriggers":{"command-center":"cc"}}`, http.StatusUnprocessableEntity},
		{"illegal hotkey", `{"hotkey":"k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"illegal triggerWord", `{"hotkey":"alt+k","triggerWord":"new ","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
		{"illegal matchMode", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"fuzzy","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"}}`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/palette/config", tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			eb := mustParseErrorBody(t, resp.Body)
			if eb.Error.Code != CodeInvalidInput {
				t.Fatalf("code = %v, want invalid_input", eb.Error.Code)
			}
			if !storeConfigEqual(store.Config(), domainpalette.DefaultConfig()) {
				t.Fatalf("snapshot changed after %s: %+v", tc.name, store.Config())
			}
			if _, err := os.Stat(palette.ConfigPath(dir)); err == nil {
				t.Fatalf("file written after %s", tc.name)
			}
		})
	}
}

func TestPaletteConfig_PUT_UnknownKeysIgnored(t *testing.T) {
	store := palette.LoadStore(t.TempDir())
	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"pr","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"","settings-palette":"","register-project":"reg"},"extra":true,"Hotkey":"k"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/palette/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto paletteConfigDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.Hotkey != "alt+k" || dto.TriggerWord != "create" {
		t.Fatalf("unknown keys must be ignored: %+v", dto)
	}
}

// TestPaletteConfig_PUT_TriggerOverlapAllowed 前缀重叠允许（解析按最长前缀
// 优先）与 fold 不等对（İ/i、ΟΣ/οσ）不判重复 → 200。
func TestPaletteConfig_PUT_TriggerOverlapAllowed(t *testing.T) {
	store := palette.LoadStore(t.TempDir())
	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","commandTriggers":{"command-center":"cc","projects":"ccx","settings-appearance":"","settings-env":"","settings-opencode":"","settings-ai":"İ","settings-palette":"ΟΣ","register-project":"οσ"}}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/palette/config", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto paletteConfigDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"command-center": "cc", "projects": "ccx", "settings-appearance": "",
		"settings-env": "", "settings-opencode": "", "settings-ai": "İ",
		"settings-palette": "ΟΣ", "register-project": "οσ",
	}
	if !maps.Equal(dto.CommandTriggers, want) {
		t.Fatalf("commandTriggers = %v, want %v", dto.CommandTriggers, want)
	}
}

func TestPaletteConfig_PUT_Exactly4096_200(t *testing.T) {
	store := palette.LoadStore(t.TempDir())
	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	base := validPaletteJSON()
	if len(base) >= 4096 {
		t.Fatalf("fixture too large: %d", len(base))
	}
	padded := base + strings.Repeat(" ", 4096-len(base))
	if len(padded) != 4096 {
		t.Fatalf("pad = %d", len(padded))
	}
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/palette/config", padded))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestPaletteConfig_PUT_4097_NoValidateNoWrite(t *testing.T) {
	dir := t.TempDir()
	store := palette.LoadStore(dir)
	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	base := validPaletteJSON()
	padded := base + strings.Repeat(" ", 4097-len(base))
	if len(padded) != 4097 {
		t.Fatalf("pad = %d", len(padded))
	}
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/palette/config", padded))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	eb := mustParseErrorBody(t, resp.Body)
	if eb.Error.Code != CodeInvalidInput {
		t.Fatalf("code = %v, want invalid_input", eb.Error.Code)
	}
	if !storeConfigEqual(store.Config(), domainpalette.DefaultConfig()) {
		t.Fatalf("snapshot must stay default after oversized PUT: %+v", store.Config())
	}
	if _, err := os.Stat(filepath.Join(dir, "palette.json")); err == nil {
		t.Fatal("oversized PUT must not write palette.json")
	}
}

func TestPaletteConfig_PUT_WriteFailure_500KeepsSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := palette.LoadStore(dir)
	if err := store.Put(domainpalette.DefaultConfig()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/palette/config", validPaletteJSON()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	eb := mustParseErrorBody(t, resp.Body)
	if eb.Error.Code != CodeInternal {
		t.Fatalf("code = %v, want internal", eb.Error.Code)
	}
	if !storeConfigEqual(store.Config(), domainpalette.DefaultConfig()) {
		t.Fatalf("snapshot changed after write failure: %+v", store.Config())
	}
}

func TestSetPaletteConfigStore_RebuildRoutesSmoke(t *testing.T) {
	// nil-store 的 GET 也返回默认值，仅断言 200 无法鉴别接线：预写非默认配置，
	// GET 必须读到注入 store 的快照（setter 改 no-op 时此测试失败）
	store := palette.LoadStore(t.TempDir())
	seed := domainpalette.Config{
		Hotkey:          "alt+k",
		TriggerWord:     "create",
		MatchMode:       "exact",
		CommandTriggers: domainpalette.DefaultCommandTriggers(),
	}
	if err := store.Put(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv := New(testConfig(), nil)
	srv.SetPaletteConfigStore(store)
	srv.RebuildRoutes()

	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/palette/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto paletteConfigDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if !paletteConfigEqual(dto, paletteConfigDTO{
		Hotkey:          seed.Hotkey,
		TriggerWord:     seed.TriggerWord,
		MatchMode:       seed.MatchMode,
		CommandTriggers: seed.CommandTriggers,
	}) {
		t.Fatalf("GET must serve injected store snapshot, got %+v", dto)
	}
}
