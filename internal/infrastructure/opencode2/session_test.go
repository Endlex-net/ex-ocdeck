// session_test.go 落实 tasks 3.1：SessionControl 四方法（testdata fixture
// v2 形状驱动）——create body 含 location.directory、返回 ID 按运行时发现
// pattern 校验、cursor 分页 Overflow、get/delete 404 映射。
package opencode2

import (
	"context"
	"net/http"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// newTestProvider 构造绑定 httptest server 的 Provider（newClient seam 注入）。
func newTestProvider(t *testing.T, srvURL string, opts ProviderOptions) *SPIProvider {
	t.Helper()
	setMasterSecret(t)
	p := NewSPIProvider(opts)
	p.newClient = func(port int, password string, opts Options) *Client {
		c := NewClient(port, password, opts)
		c.baseURL = srvURL
		return c
	}
	return p
}

func TestSessions_Create_SendsLocationDirectory(t *testing.T) {
	var gotBody map[string]interface{}
	var gotQuery string
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "openapi_doc.json"))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/session" {
			t.Errorf("create: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		gotQuery = r.URL.RawQuery
		gotBody = jsonBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "session_create.json"))
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	got, err := p.Sessions().Create(context.Background(), testBinding(t))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// v2 契约：无 directory query 参数，目录定位在 body location.directory
	//（CONTRACT §2——v1→v2 create-directory 语义变化）。
	if gotQuery != "" {
		t.Fatalf("create must not send query params, got %q", gotQuery)
	}
	loc, _ := gotBody["location"].(map[string]interface{})
	if loc == nil || loc["directory"] != "/wt" {
		t.Fatalf("create body location.directory: %v", gotBody)
	}
	if got.Key != (domain.SessionKey{ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID, UpstreamID: "ses_fixture01a"}) {
		t.Fatalf("key: %v", got.Key)
	}
	if got.Title != "fixture-task" || got.CreatedAt != 1792083316435 {
		t.Fatalf("session: %+v", got)
	}
}

func TestSessions_Create_RejectsIDPatternMismatch(t *testing.T) {
	// 返回 ID 不匹配运行时发现的 ^ses pattern = 协议违约。
	srv := fixtureServer(t,
		route{path: "/api/session", status: 200, body: `{"data":{"id":"not-ses","time":{"created":1,"updated":2},"location":{"directory":"/wt"}}}`},
		route{path: "/openapi.json", status: 200, body: string(readFixture(t, "openapi_doc.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	if _, err := p.Sessions().Create(context.Background(), testBinding(t)); err == nil {
		t.Fatalf("expected pattern mismatch error")
	}
}

func TestSessions_Create_ContractUnavailable_NoSideEffect(t *testing.T) {
	// 契约发现失败 → 先于 POST 拒绝（零副作用——POST handler 计数必须为 0）。
	posts := 0
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session" {
			posts++
			w.WriteHeader(200)
			return
		}
		// /openapi.json 500 → 契约不可用。
		w.WriteHeader(500)
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	_, err := p.Sessions().Create(context.Background(), testBinding(t))
	if err == nil {
		t.Fatalf("expected contract unavailable error")
	}
	if kind := appagent.KindOf(err); kind != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("kind: %v", kind)
	}
	if posts != 0 {
		t.Fatalf("create must not be attempted when contract discovery fails, posts=%d", posts)
	}
}

func TestSessions_List_OverflowOnCursor(t *testing.T) {
	srv := fixtureServer(t,
		route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page1.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Sessions().List(context.Background(), testBinding(t))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// cursor.next 非 null → Overflow=true（不完整快照信号，非错误——D7）。
	if !res.Overflow || len(res.Sessions) != 2 {
		t.Fatalf("list: overflow=%v n=%d", res.Overflow, len(res.Sessions))
	}
}

func TestSessions_List_LastPageNoOverflow(t *testing.T) {
	srv := fixtureServer(t,
		route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Sessions().List(context.Background(), testBinding(t))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Overflow || len(res.Sessions) != 1 {
		t.Fatalf("list: overflow=%v n=%d", res.Overflow, len(res.Sessions))
	}
}

func TestSessions_Get_NotFound(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a", status: 404, body: `{"_tag":"SessionNotFoundError","message":"x"}`})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	_, err := p.Sessions().Get(context.Background(), testBinding(t), testSessionKey())
	if appagent.KindOf(err) != appagent.ErrKindNotFound {
		t.Fatalf("kind: %v", err)
	}
	// 跨命名空间键拒绝（specs 归属规则）。
	err = func() error {
		_, err := p.Sessions().Get(context.Background(), testBinding(t), domain.SessionKey{ProviderID: "opencode-v1", ProviderInstanceID: "default", UpstreamID: "x"})
		return err
	}()
	if appagent.KindOf(err) != appagent.ErrKindBindingInvalid {
		t.Fatalf("cross namespace kind: %v", err)
	}
}

func TestSessions_Delete_NotFoundMapping(t *testing.T) {
	srv := fixtureServer(t,
		route{path: "/api/session/ses_fixture01a", status: 404, body: `{"_tag":"SessionNotFoundError"}`},
		route{path: "/openapi.json", status: 500, body: ""})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	// Delete 不依赖契约发现：404 → ErrKindNotFound（D7；非 v1 幂等成功）。
	if err := p.Sessions().Delete(context.Background(), testBinding(t), testSessionKey()); appagent.KindOf(err) != appagent.ErrKindNotFound {
		t.Fatalf("kind: %v", err)
	}
}

func TestSessions_Delete_NoContent(t *testing.T) {
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/session/ses_fixture01a" {
			t.Errorf("delete: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	if err := p.Sessions().Delete(context.Background(), testBinding(t), testSessionKey()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
