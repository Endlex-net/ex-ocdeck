package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---- PromptAsync 分类（design.md D1 唯一四 Kind 规则） ----

// promptAsyncHandler 构造一个按 statusCode 响应 prompt_async 的 handler，并捕获请求体。
func promptAsyncHandler(t *testing.T, statusCode int, capture *promptAsyncBody) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/sess-1/prompt_async" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("directory"); got != "/wt" {
			t.Fatalf("directory: %q", got)
		}
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method: %s", r.Method)
		}
		var body promptAsyncBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		*capture = body
		if statusCode == http.StatusNoContent {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(statusCode)
		_, _ = io.WriteString(w, "err-body")
	}
}

func TestPromptAsync_Accepted_204(t *testing.T) {
	var body promptAsyncBody
	srv := httptest.NewServer(promptAsyncHandler(t, http.StatusNoContent, &body))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")

	res := c.PromptAsync(context.Background(), "/wt", "sess-1", "msg_abc123", "hello", nil)
	if res.Kind != ResultAccepted {
		t.Fatalf("kind: %v", res.Kind)
	}
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("statusCode: %d", res.StatusCode)
	}
	if res.Body != "" || res.Detail != "" {
		t.Fatalf("body/detail should be empty: body=%q detail=%q", res.Body, res.Detail)
	}
	// messageID 透传不重写。
	if body.MessageID != "msg_abc123" {
		t.Fatalf("messageID: %q", body.MessageID)
	}
	if len(body.Parts) != 1 || body.Parts[0].Type != "text" || body.Parts[0].Text != "hello" {
		t.Fatalf("parts: %+v", body.Parts)
	}
}

func TestPromptAsync_HTTPResponse_Unexpected2xx(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted} {
		var body promptAsyncBody
		srv := httptest.NewServer(promptAsyncHandler(t, code, &body))
		c := newTestClient(t, srv, "pw")
		res := c.PromptAsync(context.Background(), "/wt", "sess-1", "msg_x", "t", nil)
		srv.Close()
		if res.Kind != ResultHTTPResponse {
			t.Fatalf("code=%d kind: %v", code, res.Kind)
		}
		if res.StatusCode != code {
			t.Fatalf("code=%d statusCode: %d", code, res.StatusCode)
		}
		if res.Body != "err-body" {
			t.Fatalf("code=%d body: %q", code, res.Body)
		}
		if res.Detail != "" {
			t.Fatalf("code=%d detail should be empty: %q", code, res.Detail)
		}
	}
}

func TestPromptAsync_HTTPResponse_4xx(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		var body promptAsyncBody
		srv := httptest.NewServer(promptAsyncHandler(t, code, &body))
		c := newTestClient(t, srv, "pw")
		res := c.PromptAsync(context.Background(), "/wt", "sess-1", "msg_x", "t", nil)
		srv.Close()
		if res.Kind != ResultHTTPResponse {
			t.Fatalf("code=%d kind: %v", code, res.Kind)
		}
		if res.StatusCode != code {
			t.Fatalf("code=%d statusCode: %d", code, res.StatusCode)
		}
	}
}

func TestPromptAsync_TransportUnknown_DoError(t *testing.T) {
	// 立即关闭的 server：Do 返回连接错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	c := newTestClient(t, srv, "pw")

	res := c.PromptAsync(context.Background(), "/wt", "sess-1", "msg_x", "t", nil)
	if res.Kind != ResultTransportUnknown {
		t.Fatalf("kind: %v", res.Kind)
	}
	if res.StatusCode != 0 {
		t.Fatalf("statusCode should be 0: %d", res.StatusCode)
	}
	if res.Body != "" {
		t.Fatalf("body should be empty: %q", res.Body)
	}
	if res.Detail == "" {
		t.Fatal("detail should be non-empty")
	}
}

func TestPromptAsync_TransportUnknown_CtxCancelled(t *testing.T) {
	// 阻塞 handler：ctx 取消前 Do 不会返回，ctx 取消后 Do 返回错误 → transport_unknown。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	ctx, cancel := context.WithCancel(context.Background())
	// 在调用后取消以触发 Do 错误。
	go func() {
		cancel()
	}()
	res := c.PromptAsync(ctx, "/wt", "sess-1", "msg_x", "t", nil)
	if res.Kind != ResultTransportUnknown {
		t.Fatalf("kind: %v", res.Kind)
	}
}

// TestPromptAsync_PreSendFailure_NewRequest 构造非法 baseURL 稳定触发 NewRequest 失败
//（design.md D1：pre_send_failure 覆盖 marshal/NewRequest 两路；marshal 路径输入全 string
// 永不失败，不可达，无需注入点——NewRequest 路径足以覆盖 pre_send_failure 分类）。
func TestPromptAsync_PreSendFailure_NewRequest(t *testing.T) {
	srv := httptest.NewServer(promptAsyncHandler(t, http.StatusNoContent, &promptAsyncBody{}))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	// 控制字符使 URL 解析失败（http.NewRequest 拒绝），稳定触发 pre_send_failure。
	c.baseURL = "http://127.0.0.1:0\x00"

	res := c.PromptAsync(context.Background(), "/wt", "sess-1", "msg_x", "t", nil)
	if res.Kind != ResultPreSendFailure {
		t.Fatalf("kind: %v want pre_send_failure", res.Kind)
	}
	if res.StatusCode != 0 {
		t.Fatalf("statusCode should be 0: %d", res.StatusCode)
	}
	if res.Body != "" {
		t.Fatalf("body should be empty: %q", res.Body)
	}
	if res.Detail == "" {
		t.Fatal("detail should be non-empty")
	}
}

// ---- messageID 请求体编码（透传不重写） ----

func TestPromptAsync_MessageID_Passthrough(t *testing.T) {
	// msg_ 前缀 + 去连字符小写 UUID hex 形态（调用方负责，本层只透传）。
	const mid = "msg_aabbccddeeff00112233445566778899"
	var body promptAsyncBody
	srv := httptest.NewServer(promptAsyncHandler(t, http.StatusNoContent, &body))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")

	_ = c.PromptAsync(context.Background(), "/wt", "sess-1", mid, "text", nil)
	if body.MessageID != mid {
		t.Fatalf("messageID not passed through: got %q want %q", body.MessageID, mid)
	}
	// body 仅含 messageID + parts，无其他字段。
	raw := mustMarshalPromptBody(t, body)
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for k := range m {
		if k != "messageID" && k != "parts" {
			t.Fatalf("unexpected body field: %q", k)
		}
	}
	// parts 仅含 type+text。
	pm, ok := m["parts"].([]interface{})
	if !ok || len(pm) != 1 {
		t.Fatalf("parts: %+v", m["parts"])
	}
	part := pm[0].(map[string]interface{})
	for k := range part {
		if k != "type" && k != "text" {
			t.Fatalf("unexpected part field: %q", k)
		}
	}
}

func mustMarshalPromptBody(t *testing.T, b promptAsyncBody) []byte {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ---- file parts（批注 7：随 prompt 引用 worktree 文件） ----

// TestPromptAsync_FileParts 验证携带 files 时 parts = text part + 逐 file part
//（file part 字段形态契约：type/mime/filename/url，url 为 file:// 绝对路径 URI）。
func TestPromptAsync_FileParts(t *testing.T) {
	var body promptAsyncBody
	srv := httptest.NewServer(promptAsyncHandler(t, http.StatusNoContent, &body))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")

	files := []PromptFilePart{
		{URL: "file:///wt/a%20b.txt", Mime: "text/plain", Filename: "a b.txt"},
		{URL: "file:///wt/c.go", Mime: "text/plain", Filename: "c.go"},
	}
	res := c.PromptAsync(context.Background(), "/wt", "sess-1", "msg_x", "review text", files)
	if res.Kind != ResultAccepted {
		t.Fatalf("kind: %v", res.Kind)
	}
	if len(body.Parts) != 3 {
		t.Fatalf("parts should be text + 2 file parts, got %d", len(body.Parts))
	}
	if body.Parts[0].Type != "text" || body.Parts[0].Text != "review text" {
		t.Fatalf("first part should be the text part: %+v", body.Parts[0])
	}
	want := []promptAsyncPart{
		{Type: "file", Mime: "text/plain", Filename: "a b.txt", URL: "file:///wt/a%20b.txt"},
		{Type: "file", Mime: "text/plain", Filename: "c.go", URL: "file:///wt/c.go"},
	}
	for i, w := range want {
		if got := body.Parts[i+1]; got != w {
			t.Fatalf("file part %d: got %+v want %+v", i, got, w)
		}
	}
}

// TestPromptAsync_NoFiles_NoFilePart 验证 files 为 nil 或空 slice 时 body 仅含 text part（无 file part）。
func TestPromptAsync_NoFiles_NoFilePart(t *testing.T) {
	for name, files := range map[string][]PromptFilePart{"nil": nil, "empty": {}} {
		var body promptAsyncBody
		srv := httptest.NewServer(promptAsyncHandler(t, http.StatusNoContent, &body))
		c := newTestClient(t, srv, "pw")
		_ = c.PromptAsync(context.Background(), "/wt", "sess-1", "msg_x", "t", files)
		srv.Close()
		if len(body.Parts) != 1 || body.Parts[0].Type != "text" {
			t.Fatalf("%s: parts should be exactly one text part: %+v", name, body.Parts)
		}
	}
}

// ---- /doc 能力探测解析（design.md D1 三值矩阵） ----

func docHandler(t *testing.T, body string, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/doc" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}
}

func docWithPromptAsync(operationID string) string {
	return `{"paths":{"` + promptAsyncPathKey + `":{"post":{"operationId":"` + operationID + `"}}}}`
}

func TestProbePromptAsyncCapability_Supported(t *testing.T) {
	srv := httptest.NewServer(docHandler(t, docWithPromptAsync(promptAsyncOperationID), http.StatusOK))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilitySupported {
		t.Fatalf("got %v want supported", got)
	}
}

func TestProbePromptAsyncCapability_Unsupported_PathMissing(t *testing.T) {
	srv := httptest.NewServer(docHandler(t, `{"paths":{}}`, http.StatusOK))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnsupported {
		t.Fatalf("got %v want unsupported", got)
	}
}

func TestProbePromptAsyncCapability_Unsupported_OperationIDMismatch(t *testing.T) {
	srv := httptest.NewServer(docHandler(t, docWithPromptAsync("session.prompt"), http.StatusOK))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnsupported {
		t.Fatalf("got %v want unsupported", got)
	}
}

func TestProbePromptAsyncCapability_Unsupported_PostMissing(t *testing.T) {
	srv := httptest.NewServer(docHandler(t, `{"paths":{"`+promptAsyncPathKey+`":{}}}`, http.StatusOK))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnsupported {
		t.Fatalf("got %v want unsupported", got)
	}
}

func TestProbePromptAsyncCapability_Unknown_401(t *testing.T) {
	srv := httptest.NewServer(docHandler(t, `{}`, http.StatusUnauthorized))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnknown {
		t.Fatalf("got %v want unknown", got)
	}
}

func TestProbePromptAsyncCapability_Unknown_404(t *testing.T) {
	srv := httptest.NewServer(docHandler(t, `{}`, http.StatusNotFound))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnknown {
		t.Fatalf("got %v want unknown", got)
	}
}

func TestProbePromptAsyncCapability_Unknown_5xx(t *testing.T) {
	srv := httptest.NewServer(docHandler(t, `internal`, http.StatusInternalServerError))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnknown {
		t.Fatalf("got %v want unknown", got)
	}
}

func TestProbePromptAsyncCapability_Unknown_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(docHandler(t, `{not json`, http.StatusOK))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnknown {
		t.Fatalf("got %v want unknown", got)
	}
}

// F-02：合法文档 + 尾随非空白垃圾必须判 unknown（防误判 supported）。
func TestProbePromptAsyncCapability_Unknown_TrailingGarbage(t *testing.T) {
	body := docWithPromptAsync(promptAsyncOperationID) + " GARBAGE"
	srv := httptest.NewServer(docHandler(t, body, http.StatusOK))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnknown {
		t.Fatalf("got %v want unknown", got)
	}
}

// F-02：合法文档 + 第二 JSON 值必须判 unknown。
func TestProbePromptAsyncCapability_Unknown_SecondJSONValue(t *testing.T) {
	body := docWithPromptAsync(promptAsyncOperationID) + `{"extra":1}`
	srv := httptest.NewServer(docHandler(t, body, http.StatusOK))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnknown {
		t.Fatalf("got %v want unknown", got)
	}
}

// F-02：合法尾随空白（空格/换行/制表）放行，仍判 supported。
func TestProbePromptAsyncCapability_Supported_TrailingWhitespace(t *testing.T) {
	body := docWithPromptAsync(promptAsyncOperationID) + " \n\t "
	srv := httptest.NewServer(docHandler(t, body, http.StatusOK))
	defer srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilitySupported {
		t.Fatalf("got %v want supported", got)
	}
}

func TestProbePromptAsyncCapability_Unknown_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	c := newTestClient(t, srv, "pw")
	if got := c.ProbePromptAsyncCapability(context.Background()); got != CapabilityUnknown {
		t.Fatalf("got %v want unknown", got)
	}
}