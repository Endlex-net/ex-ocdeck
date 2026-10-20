package opencode2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// readFixture 读取 testdata fixture（v2 wire 形状的唯一测试事实来源）。
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

// newTestClient 构造指向 httptest server 的 Client（复用 NewClient 默认值，
// 短超时便于测试；覆盖 baseURL）。
func newTestClient(t *testing.T, srv *httptest.Server, password string) *Client {
	t.Helper()
	c := NewClient(1, password, Options{
		OpTimeout:         1 * time.Second,
		ReconnectBase:     5 * time.Millisecond,
		ReconnectMax:      20 * time.Millisecond,
		ReconnectMaxTries: 0,
		HeartbeatTimeout:  0,
	})
	c.baseURL = srv.URL
	return c
}

// basicAuthed 校验请求携带正确的 Basic Auth（user 固定 opencode）。
func basicAuthed(t *testing.T, r *http.Request, password string) bool {
	t.Helper()
	u, p, ok := r.BasicAuth()
	return ok && u == "opencode" && p == password
}

// testBindingData 构造合法的生命周期 binding data（origin created）。
func testBindingData() v2BindingData {
	return v2BindingData{
		Port:      1,
		Directory: "/wt",
		TaskID:    "task_fixture01",
		Origin:    originCreated,
		Auth:      v2BindingAuth{Kind: appagent.DerivedCredKind},
	}
}

// testBinding 构造合法的 v2 TaskBinding envelope。
func testBinding(t *testing.T) domain.TaskBinding {
	t.Helper()
	raw, err := encodeV2BindingData(testBindingData())
	if err != nil {
		t.Fatalf("encode binding data: %v", err)
	}
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ProviderID,
		ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID:  "ses_fixture01a",
		TerminalRef:        v2TerminalRef("task_fixture01"),
		Data:               raw,
	}
}

// testSessionKey 构造 v2 命名空间的 SessionKey。
func testSessionKey() domain.SessionKey {
	return domain.SessionKey{ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID, UpstreamID: "ses_fixture01a"}
}

// setMasterSecret 注入测试 master secret（凭据派生 fail-closed 入口）。
func setMasterSecret(t *testing.T) {
	t.Helper()
	t.Setenv(appagent.MasterSecretEnv, "test-master-secret")
}

// testDerivedPassword 计算测试 master secret 下的派生凭据（与 setMasterSecret
// 同源；DeriveRuntimePassword 为纯函数，不依赖环境变量）。
func testDerivedPassword() string {
	return appagent.DeriveRuntimePassword("test-master-secret", ProviderID, ProviderInstanceID)
}

// fixtureServer 构造按路径分派 fixture 响应的 httptest server。认证接受显式
// "pw"（newTestClient 直构）或派生凭据（newTestProvider 经 resolveBinding 派生）。
type route struct {
	path   string
	status int
	body   string
}

// httptestServerFunc 构造自定义 handler 的 httptest server。
func httptestServerFunc(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func fixtureServer(t *testing.T, routes ...route) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !basicAuthed(t, r, "pw") && !basicAuthed(t, r, testDerivedPassword()) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		for _, rt := range routes {
			if r.URL.Path == rt.path {
				if rt.status != http.StatusOK && rt.status != http.StatusNoContent {
					w.WriteHeader(rt.status)
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(rt.status)
				}
				_, _ = w.Write([]byte(rt.body))
				return
			}
		}
		t.Errorf("unexpected path: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jsonBody 解析请求体为通用对象（断言 wire 形状用）。
func jsonBody(t *testing.T, r *http.Request) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}
