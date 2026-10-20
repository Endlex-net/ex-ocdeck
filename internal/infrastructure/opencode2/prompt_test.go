// prompt_test.go 落实 tasks 4.1：AsyncPrompt 四分类逐字段填充（D7）——提交
// 成功（200 accepted）/4xx/5xx/传输失败，files 映射（mime 丢弃）、MessageID
// pattern 校验与契约不可用保守拒绝。
package opencode2

import (
	"context"
	"net/http"
	"strings"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
)

func TestPrompt_Accepted200(t *testing.T) {
	srv := fixtureServer(t,
		route{path: "/api/session/ses_fixture01a/prompt", status: 200, body: string(readFixture(t, "prompt_accepted.json"))},
		route{path: "/openapi.json", status: 200, body: string(readFixture(t, "openapi_doc.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
		appagent.PromptRequest{Text: "hi", MessageID: "msg_fixture01"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// v2 成功 = 200 {data} 且 data.id 回显（G2-F2：envelope 解包 + 回显校验
	// 通过路径）：归一化 accepted，保留实际码供诊断，Body/Detail 空。
	if res.Outcome != appagent.PromptAccepted || res.StatusCode != 200 || res.Body != "" || res.Detail != "" {
		t.Fatalf("result: %+v", res)
	}
}

// TestPrompt_ResponseViolations 落实 G2-F2：2xx 响应必须解包 {data} envelope
// 且 data.id 回显请求 ID；空体/畸形 envelope/缺 data/ID 不一致属协议违约——
// 不得报 accepted，按 http_response 透出（StatusCode=实际码、Body=原文）。
func TestPrompt_ResponseViolations(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"malformed envelope", `{"data": oops`},
		{"missing data", `{"other": 1}`},
		{"null data", `{"data": null}`},
		{"id mismatch", `{"data":{"id":"msg_other01","sessionID":"ses_fixture01a"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixtureServer(t,
				route{path: "/api/session/ses_fixture01a/prompt", status: 200, body: tc.body},
				route{path: "/openapi.json", status: 200, body: string(readFixture(t, "openapi_doc.json"))})
			p := newTestProvider(t, srv.URL, ProviderOptions{})
			res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
				appagent.PromptRequest{Text: "hi", MessageID: "msg_fixture01"})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if res.Outcome == appagent.PromptAccepted {
				t.Fatalf("protocol violation must not be accepted: %+v", res)
			}
			if res.Outcome != appagent.PromptHTTPResponse || res.StatusCode != 200 {
				t.Fatalf("result: %+v", res)
			}
		})
	}
}

func TestPrompt_HTTPResponseClassification(t *testing.T) {
	for _, status := range []int{400, 404, 500} {
		srv := fixtureServer(t,
			route{path: "/api/session/ses_fixture01a/prompt", status: status, body: "err-body"},
			route{path: "/openapi.json", status: 200, body: string(readFixture(t, "openapi_doc.json"))})
		p := newTestProvider(t, srv.URL, ProviderOptions{})
		res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
			appagent.PromptRequest{Text: "hi", MessageID: "msg_fixture01"})
		if err != nil {
			t.Fatalf("Send %d: %v", status, err)
		}
		if res.Outcome != appagent.PromptHTTPResponse || res.StatusCode != status || res.Body != "err-body" || res.Detail != "" {
			t.Fatalf("status %d result: %+v", status, res)
		}
	}
}

func TestPrompt_TransportUnknown(t *testing.T) {
	// 契约发现可达，但 prompt POST 连接被服务端中断 → transport_unknown
	//（结果未知：MUST NOT 推断是否已发出；适配层内部零重试——未知交付禁止重投）。
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "openapi_doc.json"))
			return
		}
		panic(http.ErrAbortHandler) // 中断连接：Do 返回错误
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
		appagent.PromptRequest{Text: "hi", MessageID: "msg_fixture01"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// transport_unknown：StatusCode=0、Body 空、Detail=底层错误文本；MUST NOT
	// 推断是否已发出（适配层内部零重试——未知交付禁止重投）。
	if res.Outcome != appagent.PromptTransportUnknown || res.StatusCode != 0 || res.Body != "" || res.Detail == "" {
		t.Fatalf("result: %+v", res)
	}
}

func TestPrompt_PreSendFailure_PatternMismatch(t *testing.T) {
	srv := fixtureServer(t,
		route{path: "/openapi.json", status: 200, body: string(readFixture(t, "openapi_doc.json"))},
		route{path: "/api/session/ses_fixture01a/prompt", status: 200, body: "{}"})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
		appagent.PromptRequest{Text: "hi", MessageID: "no-pattern"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome != appagent.PromptPreSendFailure || res.StatusCode != 0 || res.Detail == "" {
		t.Fatalf("result: %+v", res)
	}
}

func TestPrompt_PreSendFailure_ContractUnavailable(t *testing.T) {
	// 契约发现失败 → pre_send_failure（能力待验证，MUST NOT 猜默认 pattern）；
	// prompt 端点不得被触达。
	promptHits := 0
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session/ses_fixture01a/prompt" {
			promptHits++
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(500) // /openapi.json
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
		appagent.PromptRequest{Text: "hi", MessageID: "msg_fixture01"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome != appagent.PromptPreSendFailure || !strings.Contains(res.Detail, "contract discovery unavailable") {
		t.Fatalf("result: %+v", res)
	}
	if promptHits != 0 {
		t.Fatalf("prompt endpoint must not be hit, hits=%d", promptHits)
	}
}

func TestPrompt_BodyShapeAndFilesMapping(t *testing.T) {
	var body map[string]interface{}
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "openapi_doc.json"))
			return
		}
		body = jsonBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "prompt_accepted.json"))
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	_, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
		appagent.PromptRequest{
			Text:      "hello",
			MessageID: "msg_fixture01",
			Files:     []appagent.PromptFile{{URL: "file:///wt/x.txt", Mime: "text/plain", Filename: "x.txt"}},
		})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// body {text, id}：id 透传不重写。
	if body["id"] != "msg_fixture01" || body["text"] != "hello" {
		t.Fatalf("body: %v", body)
	}
	files, _ := body["files"].([]interface{})
	if len(files) != 1 {
		t.Fatalf("files: %v", body)
	}
	f := files[0].(map[string]interface{})
	// FileAttachment 冻结映射：uri=URL、name=Filename；mime 丢弃（上游无字段）。
	if f["uri"] != "file:///wt/x.txt" || f["name"] != "x.txt" {
		t.Fatalf("file attachment: %v", f)
	}
	if _, has := f["mime"]; has {
		t.Fatalf("mime must be dropped (upstream has no field): %v", f)
	}
}

// TestPrompt_Accepted_LargeSuccessBody 落实 G2-F6：合法成功响应 >4KB 仍判
// accepted（校验基于上限内完整正文，4KB 截断仅用于诊断填充——截断先于校验会
// 把被上游接受的 prompt 误报协议违约）。
func TestPrompt_Accepted_LargeSuccessBody(t *testing.T) {
	big := `{"data":{"id":"msg_fixture01","sessionID":"ses_fixture01a","padding":"` +
		strings.Repeat("x", 8*1024) + `"}}`
	srv := fixtureServer(t,
		route{path: "/api/session/ses_fixture01a/prompt", status: 200, body: big},
		route{path: "/openapi.json", status: 200, body: string(readFixture(t, "openapi_doc.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
		appagent.PromptRequest{Text: "hi", MessageID: "msg_fixture01"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome != appagent.PromptAccepted || res.StatusCode != 200 {
		t.Fatalf("large legal success must be accepted, got %+v", res)
	}
}

// TestPrompt_ViolationBodyTruncatedForDiagnostics 落实 G2-F6：协议违约时诊断
// Body 截断到 4KB（>4KB 畸形体不整体透传）。
func TestPrompt_ViolationBodyTruncatedForDiagnostics(t *testing.T) {
	big := `{"data":{"id":"msg_other01","padding":"` + strings.Repeat("x", 8*1024) + `"}}`
	srv := fixtureServer(t,
		route{path: "/api/session/ses_fixture01a/prompt", status: 200, body: big},
		route{path: "/openapi.json", status: 200, body: string(readFixture(t, "openapi_doc.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
		appagent.PromptRequest{Text: "hi", MessageID: "msg_fixture01"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome != appagent.PromptHTTPResponse {
		t.Fatalf("outcome: %v", res.Outcome)
	}
	if len(res.Body) != 4*1024 {
		t.Fatalf("diagnostic body must be 4KB truncated, got %d", len(res.Body))
	}
}

// TestPrompt_OversizeBody 落实 G2-F6：超上限响应无法校验回显——不得 accepted，
// 按 http_response 透出（Body=截断诊断）。
func TestPrompt_OversizeBody(t *testing.T) {
	// 合法 JSON 但 > maxReadBodyBytes：padding 撑到 1MB+。
	big := `{"data":{"id":"msg_fixture01","padding":"` + strings.Repeat("x", 1<<20) + `"}}`
	srv := fixtureServer(t,
		route{path: "/api/session/ses_fixture01a/prompt", status: 200, body: big},
		route{path: "/openapi.json", status: 200, body: string(readFixture(t, "openapi_doc.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
		appagent.PromptRequest{Text: "hi", MessageID: "msg_fixture01"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome == appagent.PromptAccepted {
		t.Fatalf("oversize body must not be accepted (echo unverifiable)")
	}
	if res.Outcome != appagent.PromptHTTPResponse || res.StatusCode != 200 {
		t.Fatalf("result: %+v", res)
	}
}

// TestPrompt_ReadFailureBody 落实 G2-F6：2xx 后正文读取失败（Content-Length
// 与实际字节不符 → unexpected EOF）——无法核验回显，不得 accepted，按
// http_response 透出。
func TestPrompt_ReadFailureBody(t *testing.T) {
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "openapi_doc.json"))
		default:
			// 声明 100 字节只写 10 字节：客户端完整读取返回 unexpected EOF。
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"data":{`))
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	res, err := p.Prompts().Send(context.Background(), testBinding(t), testSessionKey(),
		appagent.PromptRequest{Text: "hi", MessageID: "msg_fixture01"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome == appagent.PromptAccepted {
		t.Fatalf("unreadable body must not be accepted")
	}
	if res.Outcome != appagent.PromptHTTPResponse || res.StatusCode != 200 {
		t.Fatalf("result: %+v", res)
	}
}
