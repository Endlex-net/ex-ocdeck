// discover_test.go 落实 tasks 2.5：运行时契约发现——获取/解析失败的保守行为
// （MUST NOT 猜默认 pattern，能力保持待验证）与成功路径的 pattern 提取。
package opencode2

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscover_OK(t *testing.T) {
	srv := fixtureServer(t, route{path: "/openapi.json", status: 200, body: string(readFixture(t, "openapi_doc.json"))})
	c := newTestClient(t, srv, "pw")
	w, err := c.DiscoverContract(context.Background())
	if err != nil {
		t.Fatalf("DiscoverContract: %v", err)
	}
	if w.SessionIDPatternRaw != "^ses" || w.MessageIDPatternRaw != "^msg_" {
		t.Fatalf("patterns: %q %q", w.SessionIDPatternRaw, w.MessageIDPatternRaw)
	}
	if !w.ValidSessionID("ses_fixture01a") || w.ValidSessionID("zzz") {
		t.Fatalf("ValidSessionID mismatch")
	}
	if !w.ValidMessageID("msg_fixture01") || w.ValidMessageID("nope") {
		t.Fatalf("ValidMessageID mismatch")
	}
}

func TestDiscover_FailuresConservative(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"http 500", 500, "boom"},
		{"http 404", 404, "nope"},
		{"malformed json", 200, "not-json"},
		{"trailing garbage", 200, `{"components":{"schemas":{}}} trailing`},
		{"missing pattern schema", 200, `{"components":{"schemas":{"Other":{"type":"string"}}}}`},
		{"empty pattern", 200, `{"components":{"schemas":{"Session.ID":{"type":"string"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixtureServer(t, route{path: "/openapi.json", status: tc.status, body: tc.body})
			c := newTestClient(t, srv, "pw")
			_, err := c.DiscoverContract(context.Background())
			if !errors.Is(err, ErrContractUnavailable) {
				t.Fatalf("expect ErrContractUnavailable, got %v", err)
			}
		})
	}
}

func TestDiscover_NetworkFailureConservative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // 立即关闭：连接拒绝
	c := newTestClient(t, srv, "pw")
	if _, err := c.DiscoverContract(context.Background()); !errors.Is(err, ErrContractUnavailable) {
		t.Fatalf("expect ErrContractUnavailable, got %v", err)
	}
}

func TestProvider_ContractForCacheAndFailure(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "openapi_doc.json"))
	}))
	defer srv.Close()

	setMasterSecret(t)
	p := NewSPIProvider(ProviderOptions{})
	b := testBinding(t)
	c, _, err := p.resolveBinding(b)
	if err != nil {
		t.Fatalf("resolveBinding: %v", err)
	}
	c.baseURL = srv.URL

	w, err := p.contractFor(context.Background(), c)
	if err != nil {
		t.Fatalf("contractFor: %v", err)
	}
	if !w.ValidSessionID("ses_fixture01a") {
		t.Fatalf("contract invalid")
	}
	// 第二次命中缓存（单一来源；不再请求上游）。
	if _, err := p.contractFor(context.Background(), c); err != nil {
		t.Fatalf("contractFor cached: %v", err)
	}
	if hits != 1 {
		t.Fatalf("upstream hits: %d", hits)
	}
}
