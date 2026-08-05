package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// openaiResponseBody 构造 OpenAI 响应体。
func openaiResponseBody(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"content": content}},
		},
	})
	return string(b)
}

func anthropicResponseBody(parts []string) string {
	blocks := make([]map[string]string, len(parts))
	for i, p := range parts {
		blocks[i] = map[string]string{"type": "text", "text": p}
	}
	b, _ := json.Marshal(map[string]any{"content": blocks})
	return string(b)
}

func TestOpenAICompleter_RequestShape(t *testing.T) {
	var gotURL string
	var gotAuth, gotCT string
	var gotBody openaiRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, openaiResponseBody("fix-bug"))
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "sk-test", Model: "gpt-4o-mini", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	resp, err := c.Complete(context.Background(), Request{System: "sys", User: "u", MaxTokens: 16})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Text != "fix-bug" {
		t.Errorf("text=%q want fix-bug", resp.Text)
	}
	if gotURL != "/v1/chat/completions" {
		t.Errorf("url=%q want /v1/chat/completions", gotURL)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth=%q want Bearer sk-test", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type=%q want application/json", gotCT)
	}
	if gotBody.Model != "gpt-4o-mini" {
		t.Errorf("model=%q", gotBody.Model)
	}
	if gotBody.MaxTokens != 16 {
		t.Errorf("max_tokens=%d want 16", gotBody.MaxTokens)
	}
	// messages: system + user
	if len(gotBody.Messages) != 2 || gotBody.Messages[0].Role != "system" || gotBody.Messages[1].Role != "user" {
		t.Errorf("messages=%+v", gotBody.Messages)
	}
}

func TestOpenAICompleter_DefaultBaseURL(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		io.WriteString(w, openaiResponseBody("x"))
	}))
	defer srv.Close()
	// 用 srv.URL 作为 base，验证去尾 /
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL + "/"}
	c := newOpenAICompleter(cfg)
	if _, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotHost == "" {
		t.Errorf("no host received")
	}
}

func TestOpenAICompleter_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"bad key"}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Errorf("err=%v want status 401", err)
	}
}

func TestOpenAICompleter_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not json")
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil {
		t.Errorf("bad json should error")
	}
}

func TestOpenAICompleter_MissingChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "missing choices") {
		t.Errorf("err=%v want missing choices", err)
	}
}

func TestOpenAICompleter_BodyOverLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// 写 maxBodyBytes + 大量字节
		io.WriteString(w, strings.Repeat("a", maxBodyBytes+10))
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "exceeds 1MB") {
		t.Errorf("err=%v want exceeds 1MB", err)
	}
}

func TestOpenAICompleter_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		io.WriteString(w, openaiResponseBody("x"))
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	// 用更短 client timeout 加速测试
	c.client = &http.Client{Timeout: 100 * time.Millisecond}
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil {
		t.Errorf("timeout should error")
	}
}

func TestAnthropicCompleter_RequestShape(t *testing.T) {
	var gotURL, gotAPIKey, gotVersion, gotCT string
	var gotBody anthropicRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, anthropicResponseBody([]string{"hello"}))
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "ant-key", Model: "claude-3", BaseURL: srv.URL}
	c := newAnthropicCompleter(cfg)
	resp, err := c.Complete(context.Background(), Request{System: "sys", User: "u", MaxTokens: 32})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Text != "hello" {
		t.Errorf("text=%q want hello", resp.Text)
	}
	if gotURL != "/v1/messages" {
		t.Errorf("url=%q", gotURL)
	}
	if gotAPIKey != "ant-key" {
		t.Errorf("x-api-key=%q", gotAPIKey)
	}
	if gotVersion != anthropicVersion {
		t.Errorf("anthropic-version=%q want %q", gotVersion, anthropicVersion)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type=%q", gotCT)
	}
	if gotBody.Model != "claude-3" {
		t.Errorf("model=%q", gotBody.Model)
	}
	if gotBody.MaxTokens != 32 {
		t.Errorf("max_tokens=%d want 32", gotBody.MaxTokens)
	}
	if gotBody.System != "sys" {
		t.Errorf("system=%q want sys", gotBody.System)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Role != "user" || gotBody.Messages[0].Content != "u" {
		t.Errorf("messages=%+v", gotBody.Messages)
	}
}

func TestAnthropicCompleter_MultiTextBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 含非 text block 应被忽略
		blocks := []map[string]string{
			{"type": "text", "text": "foo-"},
			{"type": "tool_use", "text": "ignored"},
			{"type": "text", "text": "bar"},
		}
		b, _ := json.Marshal(map[string]any{"content": blocks})
		w.Write(b)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newAnthropicCompleter(cfg)
	resp, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Text != "foo-bar" {
		t.Errorf("text=%q want foo-bar", resp.Text)
	}
}

func TestAnthropicCompleter_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"x"}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newAnthropicCompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Errorf("err=%v want status 500", err)
	}
}

func TestAnthropicCompleter_MissingContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newAnthropicCompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "missing content") {
		t.Errorf("err=%v want missing content", err)
	}
}

func TestNewCompleter_Dispatch(t *testing.T) {
	if _, err := NewCompleter(ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m"}); err != nil {
		t.Errorf("openai dispatch: %v", err)
	}
	if _, err := NewCompleter(ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m"}); err != nil {
		t.Errorf("anthropic dispatch: %v", err)
	}
	if _, err := NewCompleter(ProviderConfig{Provider: "x", APIKey: "k", Model: "m"}); err == nil {
		t.Errorf("unknown provider should error")
	}
}

// --- 请求 body 超限 ---

func TestOpenAICompleter_RequestBodyOverLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("should not reach server on oversized request")
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	// 构造超过 1MB 的 user 消息。
	big := strings.Repeat("a", maxBodyBytes+10)
	_, err := c.Complete(context.Background(), Request{User: big, MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "request body exceeds 1MB") {
		t.Errorf("err=%v want request body exceeds 1MB", err)
	}
}

func TestAnthropicCompleter_RequestBodyOverLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("should not reach server on oversized request")
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newAnthropicCompleter(cfg)
	big := strings.Repeat("a", maxBodyBytes+10)
	_, err := c.Complete(context.Background(), Request{User: big, MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "request body exceeds 1MB") {
		t.Errorf("err=%v want request body exceeds 1MB", err)
	}
}

// --- OpenAI 结构缺失分支 ---

func TestOpenAICompleter_MissingMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// choices 元素存在但 message 缺失。
		io.WriteString(w, `{"choices":[{}]}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "missing message") {
		t.Errorf("err=%v want missing message", err)
	}
}

func TestOpenAICompleter_EmptyContentAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":""}}]}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	resp, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err != nil {
		t.Fatalf("empty content should be allowed, got %v", err)
	}
	if resp.Text != "" {
		t.Errorf("text=%q want empty", resp.Text)
	}
}

func TestOpenAICompleter_NonStringContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// content 为数字 → JSON 反序列化为 string 字段失败。
		io.WriteString(w, `{"choices":[{"message":{"content":123}}]}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil {
		t.Errorf("non-string content should error")
	}
}

func TestOpenAICompleter_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "missing choices") {
		t.Errorf("err=%v want missing choices", err)
	}
}

// --- Anthropic 结构缺失分支 ---

func TestAnthropicCompleter_EmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":[]}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newAnthropicCompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "missing content") {
		t.Errorf("err=%v want missing content", err)
	}
}

func TestAnthropicCompleter_NoTextBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// content 数组存在但无 type=="text" block。
		b, _ := json.Marshal(map[string]any{"content": []map[string]string{
			{"type": "tool_use"},
		}})
		w.Write(b)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL}
	c := newAnthropicCompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "missing text block") {
		t.Errorf("err=%v want missing text block", err)
	}
}

// --- API key 不进错误信息（安全 P1） ---

func TestOpenAICompleter_APIKeyNotInError(t *testing.T) {
	const secretKey = "sk-super-secret-1234567890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 自定义 base_url 回显请求头（模拟恶意/调试代理）。
		auth := r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"echo":"`+auth+`"}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: secretKey, Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil {
		t.Fatalf("expected error")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("api_key leaked into error: %q", err.Error())
	}
	if strings.Contains(err.Error(), "Bearer") {
		t.Errorf("auth header fragment leaked into error: %q", err.Error())
	}
}

func TestAnthropicCompleter_APIKeyNotInError(t *testing.T) {
	const secretKey = "sk-ant-secret-1234567890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"echo":"`+key+`"}`)
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: secretKey, Model: "m", BaseURL: srv.URL}
	c := newAnthropicCompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil {
		t.Fatalf("expected error")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("api_key leaked into error: %q", err.Error())
	}
}

// --- OpenAI content 三态：缺失 / null / 显式空串 ---

func TestOpenAICompleter_ContentThreeStates(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantErr   bool
		wantErrSub string
		wantText  string
	}{
		// content 缺失（message 存在但无 content 字段）→ error 结构缺失
		{"content missing", `{"choices":[{"message":{}}]}`, true, "missing content", ""},
		// content 为 JSON null → error 结构缺失
		{"content null", `{"choices":[{"message":{"content":null}}]}`, true, "missing content", ""},
		// content 显式空串 → 成功空文本（SlugNamer 门禁兜底）
		{"content empty string", `{"choices":[{"message":{"content":""}}]}`, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, c.body)
			}))
			defer srv.Close()
			cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL}
			cc := newOpenAICompleter(cfg)
			resp, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), c.wantErrSub) {
					t.Errorf("err=%v want substring %q", err, c.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Text != c.wantText {
				t.Errorf("text=%q want %q", resp.Text, c.wantText)
			}
		})
	}
}

// --- Anthropic text 三态：缺失 / null / 显式空串 ---

func TestAnthropicCompleter_TextThreeStates(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantErr     bool
		wantErrSub  string
		wantText    string
	}{
		// text 缺失（type=text 但无 text 字段）→ error 结构缺失
		{"text missing", `{"content":[{"type":"text"}]}`, true, "missing text", ""},
		// text 为 JSON null → error 结构缺失
		{"text null", `{"content":[{"type":"text","text":null}]}`, true, "missing text", ""},
		// text 显式空串 → 成功空文本
		{"text empty string", `{"content":[{"type":"text","text":""}]}`, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, c.body)
			}))
			defer srv.Close()
			cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL}
			cc := newAnthropicCompleter(cfg)
			resp, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), c.wantErrSub) {
					t.Errorf("err=%v want substring %q", err, c.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Text != c.wantText {
				t.Errorf("text=%q want %q", resp.Text, c.wantText)
			}
		})
	}
}

// --- redirect 不跟随 + Location 泄露 key 防护 ---

func TestOpenAICompleter_RedirectNotFollowed_NoKeyLeak(t *testing.T) {
	const secretKey = "sk-redirect-secret-1234567890"
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		// 302 Location 含泄露 key。
		w.Header().Set("Location", "https://attacker.example/leak?token="+secretKey)
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: secretKey, Model: "m", BaseURL: srv.URL}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil {
		t.Fatalf("expected error on 302")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("api_key leaked via redirect Location into error: %q", err.Error())
	}
	if strings.Contains(err.Error(), "attacker.example") {
		t.Errorf("redirect Location leaked into error: %q", err.Error())
	}
	// 仅收到一次请求（未跟随 redirect 到 attacker）。
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (no redirect follow)", got)
	}
}

func TestAnthropicCompleter_RedirectNotFollowed_NoKeyLeak(t *testing.T) {
	const secretKey = "sk-ant-redirect-secret-12345678"
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Location", "https://attacker.example/leak?token="+secretKey)
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: secretKey, Model: "m", BaseURL: srv.URL}
	c := newAnthropicCompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil {
		t.Fatalf("expected error on 301")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("api_key leaked via redirect Location into error: %q", err.Error())
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (no redirect follow)", got)
	}
}

// --- transport error 去敏（纵深防御） ---

func TestOpenAICompleter_TransportErrorRedactsAPIKey(t *testing.T) {
	const secretKey = "sk-transport-secret-1234567890"
	// 指向一个不可达的 base_url：构造 transport error。为让 error 字符串包含 key，
	// 把 key 作为 host 嵌入 base_url（transport error 会含该 host）。
	// 这样 redactAPIKey 必须替换掉 key 子串。
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: secretKey, Model: "m", BaseURL: "http://" + secretKey + ".invalid"}
	c := newOpenAICompleter(cfg)
	_, err := c.Complete(context.Background(), Request{User: "u", MaxTokens: 1})
	if err == nil {
		t.Fatalf("expected transport error")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("api_key leaked into transport error: %q", err.Error())
	}
}

func TestRedactAPIKey(t *testing.T) {
	if got := redactAPIKey("some text", ""); got != "some text" {
		t.Errorf("empty key should return original, got %q", got)
	}
	if got := redactAPIKey("err: sk-secret-xyz tail", "sk-secret-xyz"); got != "err: *** tail" {
		t.Errorf("redact got %q", got)
	}
	if got := redactAPIKey("sk-secret-xyz sk-secret-xyz", "sk-secret-xyz"); got != "*** ***" {
		t.Errorf("redact multiple got %q", got)
	}
}

// --- 思考强度（thinking）映射：OpenAI reasoning_effort 全档位 ---

func TestOpenAICompleter_ThinkingMapping(t *testing.T) {
	cases := []struct {
		name            string
		thinking        string
		wantEffort      string // 空 = 期望不含 reasoning_effort 键
	}{
		{"empty no field", "", ""},
		{"off minimal", "off", "minimal"},
		{"low", "low", "low"},
		{"medium", "medium", "medium"},
		{"high", "high", "high"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotBody openaiRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				io.WriteString(w, openaiResponseBody("x"))
			}))
			defer srv.Close()
			cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: c.thinking}
			cc := newOpenAICompleter(cfg)
			if _, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16}); err != nil {
				t.Fatalf("complete: %v", err)
			}
			if c.wantEffort == "" {
				if gotBody.ReasoningEffort != nil {
					t.Errorf("thinking=%q: reasoning_effort should be absent, got %q", c.thinking, *gotBody.ReasoningEffort)
				}
			} else {
				if gotBody.ReasoningEffort == nil {
					t.Errorf("thinking=%q: reasoning_effort missing, want %q", c.thinking, c.wantEffort)
				} else if *gotBody.ReasoningEffort != c.wantEffort {
					t.Errorf("thinking=%q: reasoning_effort=%q want %q", c.thinking, *gotBody.ReasoningEffort, c.wantEffort)
				}
			}
		})
	}
}

func TestOpenAICompleter_ThinkingEmpty_NoFieldInRawJSON(t *testing.T) {
	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &raw)
		io.WriteString(w, openaiResponseBody("x"))
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: ""}
	cc := newOpenAICompleter(cfg)
	if _, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, has := raw["reasoning_effort"]; has {
		t.Errorf("reasoning_effort key present in request body when thinking empty")
	}
}

// --- 思考强度映射：Anthropic thinking 全档位 + max_tokens 自动提升 ---

func TestAnthropicCompleter_ThinkingMapping(t *testing.T) {
	cases := []struct {
		name           string
		thinking       string
		wantType       string // 空 = 期望不含 thinking 键
		wantBudget     int
		reqMaxTokens   int
		wantMaxTokens  int
	}{
		{"empty no field", "", "", 0, 100, 100},
		{"off disabled", "off", "disabled", 0, 100, 100},
		{"low budget 1024", "low", "enabled", 1024, 100, 1024 + 512},
		{"medium budget 4096", "medium", "enabled", 4096, 100, 4096 + 512},
		{"high budget 16384", "high", "enabled", 16384, 100, 16384 + 512},
		{"low max_tokens already large", "low", "enabled", 1024, 5000, 5000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotBody anthropicRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				io.WriteString(w, anthropicResponseBody([]string{"x"}))
			}))
			defer srv.Close()
			cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: c.thinking}
			cc := newAnthropicCompleter(cfg)
			if _, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: c.reqMaxTokens}); err != nil {
				t.Fatalf("complete: %v", err)
			}
			if c.wantType == "" {
				if gotBody.Thinking != nil {
					t.Errorf("thinking=%q: thinking field should be absent, got %+v", c.thinking, gotBody.Thinking)
				}
				if gotBody.MaxTokens != c.wantMaxTokens {
					t.Errorf("thinking=%q: max_tokens=%d want %d", c.thinking, gotBody.MaxTokens, c.wantMaxTokens)
				}
				return
			}
			if gotBody.Thinking == nil {
				t.Fatalf("thinking=%q: thinking field missing", c.thinking)
			}
			if gotBody.Thinking.Type != c.wantType {
				t.Errorf("thinking=%q: type=%q want %q", c.thinking, gotBody.Thinking.Type, c.wantType)
			}
			if c.wantBudget > 0 && gotBody.Thinking.BudgetTokens != c.wantBudget {
				t.Errorf("thinking=%q: budget_tokens=%d want %d", c.thinking, gotBody.Thinking.BudgetTokens, c.wantBudget)
			}
			if gotBody.MaxTokens != c.wantMaxTokens {
				t.Errorf("thinking=%q: max_tokens=%d want %d", c.thinking, gotBody.MaxTokens, c.wantMaxTokens)
			}
		})
	}
}

func TestAnthropicCompleter_ThinkingEmpty_NoFieldInRawJSON(t *testing.T) {
	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &raw)
		io.WriteString(w, anthropicResponseBody([]string{"x"}))
	}))
	defer srv.Close()
	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: ""}
	cc := newAnthropicCompleter(cfg)
	if _, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, has := raw["thinking"]; has {
		t.Errorf("thinking key present in request body when thinking empty")
	}
}

// --- 能力协商：4xx unsupported → 剥离思考参数重试一次 ---

func TestOpenAICompleter_NegotiationRetrySuccess(t *testing.T) {
	var requests atomic.Int32
	var firstHadEffort atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		var body openaiRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if idx == 1 {
			firstHadEffort.Store(body.ReasoningEffort != nil)
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"unknown parameter reasoning_effort"}}`)
			return
		}
		// 第二次请求体 MUST NOT 含 reasoning_effort。
		if body.ReasoningEffort != nil {
			t.Errorf("retry request still contains reasoning_effort")
		}
		io.WriteString(w, openaiResponseBody("fix-bug"))
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	resp, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err != nil {
		t.Fatalf("expected negotiation success, got %v", err)
	}
	if resp.Text != "fix-bug" {
		t.Errorf("text=%q want fix-bug", resp.Text)
	}
	if !firstHadEffort.Load() {
		t.Errorf("first request should contain reasoning_effort")
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2 (initial + retry)", got)
	}
}

func TestOpenAICompleter_NegotiationRetryStillFails(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unsupported parameter reasoning_effort"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error after retry failure")
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2 (initial + retry)", got)
	}
}

func TestAnthropicCompleter_NegotiationRetrySuccess(t *testing.T) {
	var requests atomic.Int32
	var firstHadThinking atomic.Bool
	var firstMaxTokens, secondMaxTokens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		var body anthropicRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if idx == 1 {
			firstHadThinking.Store(body.Thinking != nil)
			firstMaxTokens.Store(int32(body.MaxTokens))
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"thinking is an unknown parameter"}}`)
			return
		}
		// 第二次请求体 MUST NOT 含 thinking。
		if body.Thinking != nil {
			t.Errorf("retry request still contains thinking")
		}
		secondMaxTokens.Store(int32(body.MaxTokens))
		io.WriteString(w, anthropicResponseBody([]string{"fix-bug"}))
	}))
	defer srv.Close()

	const reqMaxTokens = 16
	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "medium"}
	cc := newAnthropicCompleter(cfg)
	resp, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: reqMaxTokens})
	if err != nil {
		t.Fatalf("expected negotiation success, got %v", err)
	}
	if resp.Text != "fix-bug" {
		t.Errorf("text=%q want fix-bug", resp.Text)
	}
	if !firstHadThinking.Load() {
		t.Errorf("first request should contain thinking")
	}
	// 首次：thinking=medium → budget=4096，max_tokens 自动提升为 4096+512=4608。
	if got := firstMaxTokens.Load(); got != 4096+512 {
		t.Errorf("first request max_tokens=%d want %d (auto-raised)", got, 4096+512)
	}
	// 第二次：剥离 thinking → max_tokens 恢复为调用方原值，不提升。
	if got := secondMaxTokens.Load(); got != reqMaxTokens {
		t.Errorf("retry request max_tokens=%d want %d (restored to original)", got, reqMaxTokens)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2 (initial + retry)", got)
	}
}

func TestAnthropicCompleter_NegotiationRetryStillFails(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unsupported thinking parameter"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newAnthropicCompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error after retry failure")
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2 (initial + retry)", got)
	}
}

// --- 能力协商不触发：thinking 为空时不重试 ---

func TestOpenAICompleter_NoNegotiationWhenThinkingEmpty(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"unsupported"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: ""}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	// thinking 为空 → 不触发协商重试，仅 1 次请求。
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (no negotiation when thinking empty)", got)
	}
}

// --- 能力协商不触发：5xx 不重试 ---

func TestOpenAICompleter_NoNegotiationOn5xx(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"server thinking failure"}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (no negotiation on 5xx)", got)
	}
}

// --- 能力协商错误不含响应原文/key ---

func TestOpenAICompleter_NegotiationErrorNoBodyLeak(t *testing.T) {
	const secretKey = "sk-negotiate-secret-1234567890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"unknown parameter reasoning_effort token=`+secretKey+`"}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: secretKey, Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("api_key leaked into negotiation error: %q", err.Error())
	}
	if strings.Contains(err.Error(), "reasoning_effort") || strings.Contains(err.Error(), "unknown parameter") {
		t.Errorf("response body leaked into negotiation error: %q", err.Error())
	}
}

// --- 能力协商正例：结构化 error.param + 文本「not supported」 ---

func TestOpenAICompleter_NegotiationStructuredParam(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		if idx == 1 {
			// OpenAI 风格结构化错误：error.param == reasoning_effort。
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unsupported parameter","param":"reasoning_effort"}}`)
			return
		}
		var body openaiRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ReasoningEffort != nil {
			t.Errorf("retry request still contains reasoning_effort")
		}
		io.WriteString(w, openaiResponseBody("fix-bug"))
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err != nil {
		t.Fatalf("expected negotiation success, got %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2", got)
	}
}

func TestAnthropicCompleter_NegotiationNotSupportedSemantic(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		if idx == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"thinking is not supported for this model"}}`)
			return
		}
		io.WriteString(w, anthropicResponseBody([]string{"fix-bug"}))
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "medium"}
	cc := newAnthropicCompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err != nil {
		t.Fatalf("expected negotiation success, got %v", err)
	}
	// thinking + "not supported" → 触发协商，2 次请求。
	if got := requests.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2", got)
	}
}

// --- 能力协商负例：4xx 但非思考参数不支持 → 不重试（count=1） ---

func TestOpenAICompleter_NoNegotiationOnUnsupportedContentType(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unsupported content type"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (no thinking param marker)", got)
	}
}

func TestOpenAICompleter_NoNegotiationOn401UnknownKey(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"unknown API key"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (unknown key, no thinking param)", got)
	}
}

func TestOpenAICompleter_NoNegotiationOn404UnknownModel(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"message":"unknown model"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (unknown model, no thinking param)", got)
	}
}

func TestOpenAICompleter_NoNegotiationOn429RateLimitThinkingModel(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		// body 含 thinking 字样但无不支持语义 → 不应触发。
		io.WriteString(w, `{"error":{"message":"rate limit exceeded for model thinking-v1"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (429 thinking-model, no unsupported semantic)", got)
	}
}

func TestOpenAICompleter_NoNegotiationOnModelNameWithThinking(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
		// model 名含 thinking，错误体含 thinking + not found，但无 parameter 标识、
		// 无 unsupported/unknown 思考参数语义 → 不触发协商。
		io.WriteString(w, `{"error":{"message":"model thinking-v1 not found"}}`)
	}))
	defer srv.Close()

	// thinking 配置为 high，但 model 名为 thinking-v1（错误体含 thinking 裸子串 + not found）。
	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "thinking-v1", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	// 裸 thinking 子串（无 parameter 上下文）+ not found（不在 unsupported 词表）→ 不触发协商。
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (model not found, not unsupported thinking)", got)
	}
}

func TestAnthropicCompleter_NoNegotiationOn429ThinkingModel(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limit for thinking-model"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newAnthropicCompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (429, thinking in body but no unsupported semantic)", got)
	}
}

// --- 负例：model 名含 thinking + 不支持语义词，但无参数上下文 → 不重试 ---

func TestOpenAICompleter_NoNegotiationOnInvalidModelThinkingV1(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		// 含 invalid（不支持语义）+ thinking（裸子串，非参数上下文，无 parameter）。
		io.WriteString(w, `{"error":{"message":"invalid model thinking-v1"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "thinking-v1", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	// 裸 thinking 子串（无 parameter 上下文）+ invalid → thinking 不命中参数标识 → 不触发协商。
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (invalid model thinking-v1, no param context)", got)
	}
}

func TestOpenAICompleter_NoNegotiationOnUnknownModelThinkingV1(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
		// 含 unknown（不支持语义）+ thinking（裸子串，非参数上下文）。
		io.WriteString(w, `{"error":{"message":"unknown model thinking-v1"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "thinking-v1", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (unknown model thinking-v1, no param context)", got)
	}
}

func TestOpenAICompleter_NoNegotiationOnUnrecognizedModelThinkingV1(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		// 含 unrecognized（不支持语义）+ thinking（裸子串，非参数上下文）。
		io.WriteString(w, `{"error":{"message":"unrecognized model thinking-v1"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "thinking-v1", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (unrecognized model thinking-v1, no param context)", got)
	}
}

// --- 状态码收窄：401/404/429 即使强匹配 error.param 也 MUST NOT 重试 ---

func TestOpenAICompleter_NoNegotiationOn401WithStrongParam(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		// 强匹配体：error.param == reasoning_effort，但状态 401 不应触发。
		io.WriteString(w, `{"error":{"message":"Unsupported parameter","param":"reasoning_effort"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (401 status MUST NOT trigger negotiation)", got)
	}
}

func TestOpenAICompleter_NoNegotiationOn404WithStrongParam(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"message":"Unsupported parameter","param":"reasoning_effort"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (404 status MUST NOT trigger negotiation)", got)
	}
}

func TestOpenAICompleter_NoNegotiationOn429WithStrongParam(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"Unsupported parameter","param":"reasoning_effort"}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (429 status MUST NOT trigger negotiation)", got)
	}
}

// --- 跨字段误判防护：thinking 在 request 字段、invalid 在 message → 不触发 ---

func TestOpenAICompleter_NoNegotiationOnCrossFieldMismatch(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		// thinking 在 request 对象、invalid 在 error.message，分处不同对象 → 不应触发。
		io.WriteString(w, `{"error":{"message":"invalid max_tokens"},"request":{"thinking":{"type":"enabled"}}}`)
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newAnthropicCompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1 (thinking in request field, invalid in message, cross-field mismatch)", got)
	}
}

// --- 自然表述 "not support thinking" / "not support reasoning_effort" 正例 ---

func TestOpenAICompleter_NegotiationOnNotSupportReasoningEffort(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		if idx == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"This model does not support reasoning_effort."}}`)
			return
		}
		var body openaiRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ReasoningEffort != nil {
			t.Errorf("retry request still contains reasoning_effort")
		}
		io.WriteString(w, openaiResponseBody("fix-bug"))
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderOpenAI, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newOpenAICompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err != nil {
		t.Fatalf("expected negotiation success, got %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2 (not support reasoning_effort → negotiate)", got)
	}
}

func TestAnthropicCompleter_NegotiationOnNotSupportThinking(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := requests.Add(1)
		if idx == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"This model does not support thinking."}}`)
			return
		}
		var body anthropicRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Thinking != nil {
			t.Errorf("retry request still contains thinking")
		}
		io.WriteString(w, anthropicResponseBody([]string{"fix-bug"}))
	}))
	defer srv.Close()

	cfg := ProviderConfig{Provider: ProviderAnthropic, APIKey: "k", Model: "m", BaseURL: srv.URL, Thinking: "high"}
	cc := newAnthropicCompleter(cfg)
	_, err := cc.Complete(context.Background(), Request{User: "u", MaxTokens: 16})
	if err != nil {
		t.Fatalf("expected negotiation success, got %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2 (not support thinking → negotiate)", got)
	}
}