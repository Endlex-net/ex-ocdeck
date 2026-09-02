package api

import (
	"encoding/json"
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
	return `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact"}`
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

	getResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/palette/config", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	var getDTO paletteConfigDTO
	if err := json.NewDecoder(getResp.Body).Decode(&getDTO); err != nil {
		t.Fatal(err)
	}
	if getDTO != putDTO {
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
		{"missing hotkey", `{"triggerWord":"create","matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"missing triggerWord", `{"hotkey":"alt+k","matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"missing matchMode", `{"hotkey":"alt+k","triggerWord":"create"}`, http.StatusUnprocessableEntity},
		{"null hotkey", `{"hotkey":null,"triggerWord":"create","matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"null triggerWord", `{"hotkey":"alt+k","triggerWord":null,"matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"null matchMode", `{"hotkey":"alt+k","triggerWord":"create","matchMode":null}`, http.StatusUnprocessableEntity},
		{"type error hotkey", `{"hotkey":1,"triggerWord":"create","matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"type error triggerWord", `{"hotkey":"alt+k","triggerWord":true,"matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"type error matchMode", `{"hotkey":"alt+k","triggerWord":"create","matchMode":[]}`, http.StatusUnprocessableEntity},
		{"wrong-case Hotkey only", `{"Hotkey":"alt+k","triggerWord":"create","matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"illegal hotkey", `{"hotkey":"k","triggerWord":"create","matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"illegal triggerWord", `{"hotkey":"alt+k","triggerWord":"new ","matchMode":"exact"}`, http.StatusUnprocessableEntity},
		{"illegal matchMode", `{"hotkey":"alt+k","triggerWord":"create","matchMode":"fuzzy"}`, http.StatusUnprocessableEntity},
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
			if store.Config() != domainpalette.DefaultConfig() {
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

	body := `{"hotkey":"alt+k","triggerWord":"create","matchMode":"exact","extra":true,"Hotkey":"k"}`
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

func TestPaletteConfig_PUT_Exactly1024_200(t *testing.T) {
	store := palette.LoadStore(t.TempDir())
	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	base := validPaletteJSON()
	if len(base) >= 1024 {
		t.Fatalf("fixture too large: %d", len(base))
	}
	padded := base + strings.Repeat(" ", 1024-len(base))
	if len(padded) != 1024 {
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

func TestPaletteConfig_PUT_1025_NoValidateNoWrite(t *testing.T) {
	dir := t.TempDir()
	store := palette.LoadStore(dir)
	s := newPaletteConfigServer(t, store)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	base := validPaletteJSON()
	padded := base + strings.Repeat(" ", 1025-len(base))
	if len(padded) != 1025 {
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
	if store.Config() != domainpalette.DefaultConfig() {
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
	if store.Config() != domainpalette.DefaultConfig() {
		t.Fatalf("snapshot changed after write failure: %+v", store.Config())
	}
}

func TestSetPaletteConfigStore_RebuildRoutesSmoke(t *testing.T) {
	// nil-store 的 GET 也返回默认值，仅断言 200 无法鉴别接线：预写非默认配置，
	// GET 必须读到注入 store 的快照（setter 改 no-op 时此测试失败）
	store := palette.LoadStore(t.TempDir())
	if err := store.Put(domainpalette.Config{Hotkey: "alt+k", TriggerWord: "create", MatchMode: "exact"}); err != nil {
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
	if dto.Hotkey != "alt+k" || dto.TriggerWord != "create" || dto.MatchMode != "exact" {
		t.Fatalf("GET must serve injected store snapshot, got %+v", dto)
	}
}
