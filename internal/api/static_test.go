// Package api 静态资源同源服务测试（design.md §14：embed.FS 内嵌前端 + SPA fallback）。
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newStaticTestServer 构造仅挂静态路由的测试服务（复用 spaHandler）。
func newStaticTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	h := newSPAHandler()
	return httptest.NewServer(h)
}

func TestSPAHandler_RootServesIndexHTML(t *testing.T) {
	srv := newStaticTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<!doctype html>") {
		t.Errorf("root body missing doctype; got %q", string(body)[:min(80, len(body))])
	}
	if !strings.Contains(string(body), "id=\"root\"") {
		t.Errorf("root body missing #root mount point")
	}
}

func TestSPAHandler_AssetsServedWithoutToken(t *testing.T) {
	srv := newStaticTestServer(t)
	defer srv.Close()

	// bundle 文件名带内容 hash，随前端重建变化——从 index.html 动态提取 script 路径。
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	rootBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assetPath := extractScriptSrc(t, string(rootBody))

	resp, err = http.Get(srv.URL + assetPath)
	if err != nil {
		t.Fatalf("get asset: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("asset content-type = %q, want text/javascript*", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("asset body empty; expected embedded JS bundle")
	}
}

// extractScriptSrc 从 index.html 提取 `<script ... src="...">` 的资源路径。
func extractScriptSrc(t *testing.T, html string) string {
	t.Helper()
	const marker = `src="`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatal("index.html missing script src")
	}
	rest := html[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j <= 0 {
		t.Fatal("index.html malformed script src")
	}
	return rest[:j]
}

func TestSPAHandler_UnknownPathFallsBackToIndex(t *testing.T) {
	srv := newStaticTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/some/unknown/spa/route")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<!doctype html>") {
		t.Errorf("fallback body missing doctype; got %q", string(body)[:min(80, len(body))])
	}
}

func TestStaticExemptPaths(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/favicon.ico", true},
		{"/assets/index-abc.js", true},
		{"/assets/style.css", true},
		{"/api/v1/server/status", false},
		{"/api/v1/projects", false},
		{"/ws/terminal/abc", false},
		{"/tasks", false},
	}
	for _, c := range cases {
		if got := staticExemptPaths(c.path); got != c.want {
			t.Errorf("staticExemptPaths(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}