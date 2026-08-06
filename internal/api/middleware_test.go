package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ocdeck/internal/config"
	"ocdeck/internal/opencode"
)

func TestAuthMiddleware_MissingToken_401(t *testing.T) {
	auth := NewTokenAuthenticator("secret")
	srv := httptest.NewServer(auth.AuthMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called without token")
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/server/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthMiddleware_InvalidToken_401(t *testing.T) {
	auth := NewTokenAuthenticator("secret")
	srv := httptest.NewServer(auth.AuthMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with invalid token")
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/server/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthMiddleware_ValidToken_PassThrough(t *testing.T) {
	called := false
	auth := NewTokenAuthenticator("secret")
	srv := httptest.NewServer(auth.AuthMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/server/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Error("handler not called with valid token")
	}
}

func TestAuthMiddleware_BearerPrefixCaseInsensitive(t *testing.T) {
	auth := NewTokenAuthenticator("secret")
	srv := httptest.NewServer(auth.AuthMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set("Authorization", "bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (bearer prefix case-insensitive)", resp.StatusCode)
	}
}

func TestAuthMiddleware_StaticExempt(t *testing.T) {
	called := false
	auth := NewTokenAuthenticator("secret")
	exempt := func(path string) bool { return path == "/" }
	srv := httptest.NewServer(auth.AuthMiddleware(exempt, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()

	// 根路径豁免，无需 token。
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for exempt path", resp.StatusCode)
	}
	if !called {
		t.Error("exempt path handler not called")
	}
}

func TestValidateToken_ConstantTime(t *testing.T) {
	auth := NewTokenAuthenticator("secret")
	if !auth.ValidateToken("secret") {
		t.Error("matching token rejected")
	}
	if auth.ValidateToken("secre") {
		t.Error("shorter token accepted")
	}
	if auth.ValidateToken("secretx") {
		t.Error("wrong token accepted")
	}
	if auth.ValidateToken("") {
		t.Error("empty token accepted")
	}
}

func TestWriteError_Structure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, CodeInvalidInput, "bad name")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("code = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	want := `{"error":{"code":"invalid_input","message":"bad name"}}`
	if body != want+"\n" && body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

func TestServerStatus_RequiresAuth(t *testing.T) {
	cfg := testConfig()
	srv := New(cfg, nil)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/server/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestServerStatus_WithAuth(t *testing.T) {
	cfg := testConfig()
	srv := New(cfg, nil)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/server/status", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestValidateToken_DifferentLengthConstantTime(t *testing.T) {
	auth := NewTokenAuthenticator("secret")
	// 不同长度的错误 token 都应拒绝，且不因长度提前返回泄露长度信息。
	// 这里验证功能正确性（拒绝）；时序保证依赖 SHA-256 固定摘要 + ConstantTimeCompare。
	for _, wrong := range []string{"s", "se", "sec", "secre", "secretx", "xecret", "secrets-extra-long"} {
		got := auth.ValidateToken(wrong)
		if got {
			t.Errorf("ValidateToken(%q) = true, want false", wrong)
		}
	}
	// 正确长度但内容不同也应拒绝。
	if auth.ValidateToken("SECRET") {
		t.Error("ValidateToken(uppercase) = true, want false")
	}
}

func TestAuthMiddleware_401HasWWWAuthenticate(t *testing.T) {
	auth := NewTokenAuthenticator("secret")
	srv := httptest.NewServer(auth.AuthMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
	}
	// 401 响应体应为统一 JSON 错误结构。
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"unauthorized"`) {
		t.Errorf("body = %q, want JSON error body with unauthorized code", string(body))
	}
}

func TestAuthenticatedUnknownRoute_JSON404(t *testing.T) {
	cfg := testConfig()
	srv := New(cfg, nil)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"not_found"`) {
		t.Errorf("body = %q, want JSON error body with not_found code", string(body))
	}
}

func TestAuthenticatedWrongMethod_JSON404(t *testing.T) {
	cfg := testConfig()
	srv := New(cfg, nil)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	// /api/v1/server/status 只注册了 GET，POST 应返回 JSON 404/405 形态。
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/server/status", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"not_found"`) {
		t.Errorf("body = %q, want JSON error body with not_found code", string(body))
	}
}

func TestServerStatus_VersionVerifiedReal(t *testing.T) {
	cfg := testConfig()
	cfg.OpenCodeVersion = opencode.ContractBaseline
	cfg.VersionVerified = config.VersionMatches(cfg.OpenCodeVersion, config.ContractBaseline)
	srv := New(cfg, nil)
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/server/status", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if vv, _ := status["versionVerified"].(bool); !vv {
		t.Errorf("versionVerified = %v, want true for matching baseline", status["versionVerified"])
	}
	if cb, _ := status["contractBaseline"].(string); cb != config.ContractBaseline {
		t.Errorf("contractBaseline = %q, want %s", cb, config.ContractBaseline)
	}
}
