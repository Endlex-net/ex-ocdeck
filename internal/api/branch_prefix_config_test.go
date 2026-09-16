// branch_prefix_config_test.go 覆盖 GET/PUT /api/v1/config/branch-prefix
//（task-info-editable 3.1：缺省读取/保存回读/非法拒绝/损坏降级/错误矩阵）。
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ocdeck/internal/infrastructure/branchprefix"
)

func newBranchPrefixServer(t *testing.T, dataDir string) (*Server, *httptest.Server) {
	t.Helper()
	s := newBranchPrefixOnlyServer(t, branchprefix.LoadStore(dataDir))
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	return s, ts
}

// newBranchPrefixOnlyServer 镜像 newPaletteConfigServer（仅注入 branch-prefix Store 的独立 Server）。
func newBranchPrefixOnlyServer(t *testing.T, store *branchprefix.Store) *Server {
	t.Helper()
	cfg := testConfig()
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
		webHub:    newWebHub(),
	}
	s.SetBranchPrefixStore(store)
	s.registerRoutes()
	return s
}

func TestBranchPrefix_Get_Default(t *testing.T) {
	_, ts := newBranchPrefixServer(t, t.TempDir())
	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/config/branch-prefix", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto branchPrefixDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.Prefix != "ocdeck" {
		t.Fatalf("prefix = %q, want ocdeck (default)", dto.Prefix)
	}
}

func TestBranchPrefix_PutThenGet_RoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	_, ts := newBranchPrefixServer(t, dataDir)

	req := authedReq("PUT", ts.URL+"/api/v1/config/branch-prefix", `{"prefix":"team-x"}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	var putDTO branchPrefixDTO
	if err := json.NewDecoder(resp.Body).Decode(&putDTO); err != nil {
		t.Fatal(err)
	}
	if putDTO.Prefix != "team-x" {
		t.Fatalf("PUT response prefix = %q, want team-x", putDTO.Prefix)
	}

	// 保存后立即可读 + 落盘持久（新 Store 实例读回同值）。
	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/config/branch-prefix", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var getDTO branchPrefixDTO
	if err := json.NewDecoder(resp2.Body).Decode(&getDTO); err != nil {
		t.Fatal(err)
	}
	if getDTO.Prefix != "team-x" {
		t.Fatalf("GET after PUT = %q, want team-x", getDTO.Prefix)
	}
	if got := branchprefix.LoadStore(dataDir).Prefix(); got != "team-x" {
		t.Fatalf("persisted prefix = %q, want team-x", got)
	}
}

func TestBranchPrefix_Put_Invalid_RejectedWithoutWrite(t *testing.T) {
	dataDir := t.TempDir()
	seed := branchprefix.LoadStore(dataDir)
	if err := seed.Put("team-x"); err != nil {
		t.Fatal(err)
	}
	s := newBranchPrefixOnlyServer(t, branchprefix.LoadStore(dataDir))
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	for _, bad := range []string{"Team", "team x", " team", "", "oc/deck"} {
		req := authedReq("PUT", ts.URL+"/api/v1/config/branch-prefix", string(mustJSON(map[string]string{"prefix": bad})))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("PUT %q status = %d, want 422 (body=%s)", bad, resp.StatusCode, body)
		}
		var eb errorBody
		_ = json.Unmarshal(body, &eb)
		if eb.Error.Code != CodeInvalidInput {
			t.Fatalf("PUT %q code = %q, want invalid_input", bad, eb.Error.Code)
		}
	}
	// 拒绝后内存值与文件保持原样（校验失败 MUST NOT 写盘）。
	if got := s.branchPrefix.Prefix(); got != "team-x" {
		t.Fatalf("memory prefix = %q, want team-x (unchanged)", got)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "branch-prefix.json"))
	if err != nil || !strings.Contains(string(b), "team-x") {
		t.Fatalf("file changed after rejects: %q, %v", string(b), err)
	}
}

func TestBranchPrefix_Put_DecodeErrorMatrix(t *testing.T) {
	_, ts := newBranchPrefixServer(t, t.TempDir())
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"empty body", "", http.StatusBadRequest},
		{"syntax error", "{", http.StatusBadRequest},
		{"trailing value", `{"prefix":"a"} {"prefix":"b"}`, http.StatusBadRequest},
		{"top-level array", `["a"]`, http.StatusUnprocessableEntity},
		{"missing key", `{}`, http.StatusUnprocessableEntity},
		{"null value", `{"prefix":null}`, http.StatusUnprocessableEntity},
		{"type error", `{"prefix":123}`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authedReq("PUT", ts.URL+"/api/v1/config/branch-prefix", tc.body)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			var eb errorBody
			_ = json.NewDecoder(resp.Body).Decode(&eb)
			if eb.Error.Code != CodeInvalidInput {
				t.Fatalf("code = %q, want invalid_input", eb.Error.Code)
			}
		})
	}
}

func TestBranchPrefix_ConfigCorrupted_ServedDefault(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "branch-prefix.json"), []byte("{corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ts := newBranchPrefixServer(t, dataDir)
	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/config/branch-prefix", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrade, not error)", resp.StatusCode)
	}
	var dto branchPrefixDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.Prefix != "ocdeck" {
		t.Fatalf("prefix = %q, want ocdeck (degraded default)", dto.Prefix)
	}
}
