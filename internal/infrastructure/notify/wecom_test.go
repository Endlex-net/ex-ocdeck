package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ocdeck/internal/domain/notification"
)

var _ notification.Channel = (*WecomChannel)(nil)

// wecomTestIntent 测试用意图（字段值含泄露检查标记，供 Err 禁泄露断言）。
func wecomTestIntent() notification.Intent {
	return notification.Intent{
		TaskID:   "task-42",
		TaskName: "demo-task",
		Category: notification.CategoryQuestion,
		Level:    notification.LevelTimeSensitive,
		Title:    "等待你的回答",
		Body:     "demo-task\nleak-check-body-marker",
		URL:      "http://127.0.0.1:18080/#/task/task-42",
	}
}

// wecomTestURLMarker 仅出现在完整 webhook URL 中的泄露标记。
const wecomTestURLMarker = "wecom-leak-check-key-xyz"

func wecomTestConfig(endpoint string) notification.ChannelConfig {
	return notification.ChannelConfig{Endpoint: endpoint + "?key=" + wecomTestURLMarker}
}

// capturedWecomRequest handler 侧捕获的单次请求。
type capturedWecomRequest struct {
	method      string
	path        string
	rawQuery    string
	contentType string
	rawBody     []byte
}

// wecomCaptureServer 起 fake WeCom server，respond 决定响应内容，返回捕获器。
func wecomCaptureServer(t *testing.T, respond func(w http.ResponseWriter)) (*httptest.Server, *capturedWecomRequest) {
	t.Helper()
	captured := &capturedWecomRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.rawQuery = r.URL.RawQuery
		captured.contentType = r.Header.Get("Content-Type")
		captured.rawBody = body
		respond(w)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// wecomSizedJSON 构造总长恰为 total 字节、errcode=0 的合法 JSON（用于 64KiB 边界测试）。
func wecomSizedJSON(t *testing.T, total int) string {
	t.Helper()
	const prefix = `{"errcode":0,"pad":"`
	const suffix = `"}`
	pad := total - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatalf("total %d too small", total)
	}
	return prefix + strings.Repeat("a", pad) + suffix
}

// TestWecomChannel_SendWireContract 覆盖 wire 契约全项：POST cfg.Endpoint 原样
// （含 query，MUST NOT 拼接 path / 剥离 query）、Content-Type: application/json、
// msgtype=markdown、content 逐字模板（加粗标题 + 一个换行 + 正文 + 空行 + 链接）。
func TestWecomChannel_SendWireContract(t *testing.T) {
	srv, captured := wecomCaptureServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"errcode":0}`))
	})
	res := NewWecomChannel().Send(context.Background(), wecomTestIntent(), wecomTestConfig(srv.URL))
	if !res.OK || res.Err != "" {
		t.Fatalf("expect success, got OK=%v Err=%q", res.OK, res.Err)
	}
	if captured.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", captured.method)
	}
	if captured.path != "/" || captured.rawQuery != "key="+wecomTestURLMarker {
		t.Fatalf("endpoint used as-is: path=%q rawQuery=%q", captured.path, captured.rawQuery)
	}
	if captured.contentType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", captured.contentType)
	}

	var got wecomRequest
	if err := json.Unmarshal(captured.rawBody, &got); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if got.MsgType != "markdown" {
		t.Fatalf("msgtype = %q, want markdown", got.MsgType)
	}
	in := wecomTestIntent()
	wantContent := "**" + in.Title + "**\n" + in.Body + "\n\n[打开任务](" + in.URL + ")"
	if got.Markdown.Content != wantContent {
		t.Fatalf("content = %q, want %q", got.Markdown.Content, wantContent)
	}
	// 仅 msgtype 与 markdown 两键。
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(captured.rawBody, &keys); err != nil {
		t.Fatalf("request body not a JSON object: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("request body keys = %v, want exactly {msgtype, markdown}", keys)
	}
}

// TestWecomChannel_EmptyURLOmitsLink Intent.URL 为空时省略链接行（含前导空行），
// MUST NOT 渲染空 []()。
func TestWecomChannel_EmptyURLOmitsLink(t *testing.T) {
	srv, captured := wecomCaptureServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"errcode":0}`))
	})
	in := wecomTestIntent()
	in.URL = ""
	res := NewWecomChannel().Send(context.Background(), in, wecomTestConfig(srv.URL))
	if !res.OK {
		t.Fatalf("expect success, Err=%q", res.Err)
	}
	var got wecomRequest
	if err := json.Unmarshal(captured.rawBody, &got); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	want := "**" + in.Title + "**\n" + in.Body
	if got.Markdown.Content != want {
		t.Fatalf("content = %q, want %q (no link row or leading blank line)", got.Markdown.Content, want)
	}
	if strings.Contains(got.Markdown.Content, "[]()") || strings.Contains(got.Markdown.Content, "打开任务") {
		t.Fatalf("content must not contain link placeholder: %q", got.Markdown.Content)
	}
}

// TestWecomChannel_ContentExactly4096Bytes content 恰好 4096 字节不截断且 POST。
func TestWecomChannel_ContentExactly4096Bytes(t *testing.T) {
	srv, captured := wecomCaptureServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"errcode":0}`))
	})
	in := wecomTestIntent()
	in.URL = ""
	// 构造 content 恰好 4096 字节：标题前缀 "**" + title + "\n" + body。
	prefix := "**" + in.Title + "**\n"
	need := wecomMaxContentLen - len(prefix)
	in.Body = strings.Repeat("x", need)
	res := NewWecomChannel().Send(context.Background(), in, wecomTestConfig(srv.URL))
	if !res.OK {
		t.Fatalf("exactly 4096 bytes must succeed, Err=%q", res.Err)
	}
	var got wecomRequest
	if err := json.Unmarshal(captured.rawBody, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := len(got.Markdown.Content); n != wecomMaxContentLen {
		t.Fatalf("content len = %d, want exactly %d (no truncate)", n, wecomMaxContentLen)
	}
}

// TestWecomChannel_Content4097BytesTruncated 4097 字节截断至 ≤4096 有效 UTF-8 后仍 POST。
func TestWecomChannel_Content4097BytesTruncated(t *testing.T) {
	srv, captured := wecomCaptureServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"errcode":0}`))
	})
	in := wecomTestIntent()
	in.URL = ""
	prefix := "**" + in.Title + "**\n"
	need := wecomMaxContentLen + 1 - len(prefix)
	in.Body = strings.Repeat("x", need)
	res := NewWecomChannel().Send(context.Background(), in, wecomTestConfig(srv.URL))
	if !res.OK {
		t.Fatalf("over-limit must still POST and succeed, Err=%q", res.Err)
	}
	var got wecomRequest
	if err := json.Unmarshal(captured.rawBody, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := len(got.Markdown.Content); n > wecomMaxContentLen {
		t.Fatalf("content len = %d, must be ≤ %d", n, wecomMaxContentLen)
	}
	if !utf8.ValidString(got.Markdown.Content) {
		t.Fatalf("truncated content must be valid UTF-8, got %q", got.Markdown.Content)
	}
}

// TestWecomChannel_TruncateUTF8Boundary 截断不得切到半个 UTF-8 序列：多字节
// rune 临界构造。
func TestWecomChannel_TruncateUTF8Boundary(t *testing.T) {
	srv, captured := wecomCaptureServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"errcode":0}`))
	})
	in := wecomTestIntent()
	in.URL = ""
	// 前缀后填 3 字节中文 rune，使截断点落在 rune 中间。
	prefix := "**" + in.Title + "**\n"
	chinese := "中" // 3 字节 UTF-8
	// 重复到 content 刚好 4095 字节，再加一个 3 字节 rune 触发截断。
	remain := wecomMaxContentLen - len(prefix)
	count := remain / 3
	pad := strings.Repeat(chinese, count) // len = count*3 ≤ 4095
	// 调整到 4095 或 4094，再加一个 3 字节 rune 使超限。
	for len(prefix)+len(pad) < wecomMaxContentLen-2 {
		pad += chinese
	}
	in.Body = pad + chinese + "x" // 加一个字符确保超 4096
	res := NewWecomChannel().Send(context.Background(), in, wecomTestConfig(srv.URL))
	if !res.OK {
		t.Fatalf("over-limit must succeed, Err=%q", res.Err)
	}
	var got wecomRequest
	if err := json.Unmarshal(captured.rawBody, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !utf8.ValidString(got.Markdown.Content) {
		t.Fatalf("truncated content must be valid UTF-8: %q", got.Markdown.Content)
	}
	if len(got.Markdown.Content) > wecomMaxContentLen {
		t.Fatalf("content len = %d, must be ≤ %d", len(got.Markdown.Content), wecomMaxContentLen)
	}
}

func TestWecomChannel_Non2xxFails(t *testing.T) {
	srv, _ := wecomCaptureServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	res := NewWecomChannel().Send(context.Background(), wecomTestIntent(), wecomTestConfig(srv.URL))
	if res.OK {
		t.Fatal("non-2xx must fail")
	}
}

// TestWecomChannel_RedirectNotFollowed 禁跟随重定向：302 不发起第二次请求。
func TestWecomChannel_RedirectNotFollowed(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/leak-target", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	res := NewWecomChannel().Send(context.Background(), wecomTestIntent(), notification.ChannelConfig{Endpoint: srv.URL})
	if res.OK {
		t.Fatal("redirect must be treated as failure")
	}
	if hits != 1 {
		t.Fatalf("redirect target hit %d times, want 1 (must not follow)", hits)
	}
}

// TestWecomChannel_TimeoutFails 请求超时判定失败（缩短 client 超时避免慢测试；
// 生产 10s 由 TestWecomChannel_DefaultClientContract 固定）。
func TestWecomChannel_TimeoutFails(t *testing.T) {
	srv, _ := wecomCaptureServer(t, func(w http.ResponseWriter) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	})
	start := time.Now()
	res := newWecomChannel(150*time.Millisecond).Send(context.Background(), wecomTestIntent(), wecomTestConfig(srv.URL))
	if res.OK {
		t.Fatal("timeout must fail")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("timeout enforced too late: %v", el)
	}
}

// TestWecomChannel_ResponseSizeLimit 64KiB 上界：恰 64KiB 合法响应成功，
// 超过 1 字节判定失败。
func TestWecomChannel_ResponseSizeLimit(t *testing.T) {
	t.Run("exactly 64KiB succeeds", func(t *testing.T) {
		srv, _ := wecomCaptureServer(t, func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(wecomSizedJSON(t, 64*1024)))
		})
		res := NewWecomChannel().Send(context.Background(), wecomTestIntent(), wecomTestConfig(srv.URL))
		if !res.OK {
			t.Fatalf("64KiB exactly must succeed, got Err=%q", res.Err)
		}
	})
	t.Run("over 64KiB fails", func(t *testing.T) {
		srv, _ := wecomCaptureServer(t, func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(wecomSizedJSON(t, 64*1024+1)))
		})
		res := NewWecomChannel().Send(context.Background(), wecomTestIntent(), wecomTestConfig(srv.URL))
		if res.OK {
			t.Fatal("response over 64KiB must fail")
		}
	})
}

// TestWecomChannel_ResponseJudgement 成功判定 = HTTP 2xx 且 JSON errcode==0；
// errcode 非 0、非法 JSON、缺 errcode 字段均失败。
func TestWecomChannel_ResponseJudgement(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantOK bool
	}{
		{"errcode 0", `{"errcode":0}`, true},
		{"errcode 400", `{"errcode":400}`, false},
		{"malformed json", `not-json`, false},
		{"missing errcode", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := wecomCaptureServer(t, func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(tc.body))
			})
			res := NewWecomChannel().Send(context.Background(), wecomTestIntent(), wecomTestConfig(srv.URL))
			if res.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (Err=%q)", res.OK, tc.wantOK, res.Err)
			}
		})
	}
}

// TestWecomChannel_ErrNeverLeaksSecrets 失败路径的 Err（将进日志）MUST NOT 携带
// webhook URL、请求体内容与企微响应原文。
func TestWecomChannel_ErrNeverLeaksSecrets(t *testing.T) {
	scenarios := []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"errcode":400,"errmsg":"raw-response-marker-xyz"}`))
		},
		func(w http.ResponseWriter) { _, _ = w.Write([]byte(`raw-response-marker-xyz not json`)) },
	}
	for i, respond := range scenarios {
		srv, _ := wecomCaptureServer(t, respond)
		res := NewWecomChannel().Send(context.Background(), wecomTestIntent(), wecomTestConfig(srv.URL))
		if res.OK {
			t.Fatalf("scenario %d must fail", i)
		}
		for _, secret := range []string{wecomTestURLMarker, "leak-check-body-marker", "raw-response-marker-xyz", "等待你的回答"} {
			if strings.Contains(res.Err, secret) {
				t.Fatalf("scenario %d Err leaks %q: %q", i, secret, res.Err)
			}
		}
	}
}

// TestWecomChannel_BuildRequestFailureNeverLeaksURL NewRequestWithContext 失败路径
// （design D5：net/http 构造错误常含 URL，Result.Err MUST 为固定文案 `wecom: build
// request failed`，MUST NOT %v 包裹、MUST NOT 含 webhook URL）。用非法 endpoint
// 触发 NewRequest 失败且 endpoint 中嵌入泄露标记，断言 Err 恰为固定文案且不含标记。
func TestWecomChannel_BuildRequestFailureNeverLeaksURL(t *testing.T) {
	// 非法 endpoint：未闭合的方括号 host 使 url.Parse 在 NewRequestWithContext
	// 内失败，同时保留泄露标记以验证不泄漏。
	invalidEndpoint := "http://[" + wecomTestURLMarker
	ch := NewWecomChannel()
	res := ch.Send(context.Background(), wecomTestIntent(), notification.ChannelConfig{Endpoint: invalidEndpoint})
	if res.OK {
		t.Fatal("build request failure must not succeed")
	}
	if res.Err != "wecom: build request failed" {
		t.Fatalf("Err = %q, want exact %q", res.Err, "wecom: build request failed")
	}
	if strings.Contains(res.Err, wecomTestURLMarker) {
		t.Fatalf("Err leaks webhook URL marker: %q", res.Err)
	}
}

// wecomFailRT RoundTripper：返回的 error 的 Error() 含 webhook URL 标记，模拟
// net/http 在 Do 失败时把 URL 嵌入错误信息的常见行为（用于验证 Send 不 %v 包裹）。
type wecomFailRT struct{}

func (wecomFailRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial failed: Get " + wecomTestURLMarker + " connection refused")
}

// TestWecomChannel_HTTPDoFailureNeverLeaksURL client.Do 失败路径（design D5：Do
// 错误常含 URL，Result.Err MUST 为固定文案 `wecom: http request failed`，MUST NOT
// %v 包裹、MUST NOT 含 webhook URL）。注入返回含标记错误的 Transport，用语法合法
// 的 endpoint 确保 NewRequest 成功、仅 Do 失败，断言 Err 恰为固定文案且不含标记。
func TestWecomChannel_HTTPDoFailureNeverLeaksURL(t *testing.T) {
	ch := &WecomChannel{client: &http.Client{Transport: wecomFailRT{}}}
	endpoint := "https://example.invalid/cgi-bin/webhook/send?key=" + wecomTestURLMarker
	res := ch.Send(context.Background(), wecomTestIntent(), notification.ChannelConfig{Endpoint: endpoint})
	if res.OK {
		t.Fatal("Do failure must not succeed")
	}
	if res.Err != "wecom: http request failed" {
		t.Fatalf("Err = %q, want exact %q", res.Err, "wecom: http request failed")
	}
	if strings.Contains(res.Err, wecomTestURLMarker) {
		t.Fatalf("Err leaks webhook URL marker: %q", res.Err)
	}
}

// TestWecomChannel_DefaultClientContract 生产构造的 wire 参数：10s 超时、
// CheckRedirect 返回 ErrUseLastResponse（禁跟随）。
func TestWecomChannel_DefaultClientContract(t *testing.T) {
	ch := NewWecomChannel()
	if ch.client.Timeout != 10*time.Second {
		t.Fatalf("client timeout = %v, want 10s", ch.client.Timeout)
	}
	if ch.client.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := ch.client.CheckRedirect(req, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect error = %v, want http.ErrUseLastResponse", err)
	}
}

func TestWecomChannel_NameAndCaps(t *testing.T) {
	ch := NewWecomChannel()
	if ch.Name() != "wecom" {
		t.Fatalf("name = %q, want wecom", ch.Name())
	}
	if ch.Caps() != 0 {
		t.Fatalf("caps = %v, want 0", ch.Caps())
	}
}

// TestWecomChannel_SingleDo 单次 Send 仅一次 Do（不重试）。
func TestWecomChannel_SingleDo(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	t.Cleanup(srv.Close)
	res := NewWecomChannel().Send(context.Background(), wecomTestIntent(), notification.ChannelConfig{Endpoint: srv.URL})
	if !res.OK {
		t.Fatalf("expect success, Err=%q", res.Err)
	}
	if hits != 1 {
		t.Fatalf("Send must issue exactly one Do, got %d", hits)
	}
}

// TestTruncateUTF8Bytes unit 覆盖截断规则。
func TestTruncateUTF8Bytes(t *testing.T) {
	if got := truncateUTF8Bytes("abc", 10); got != "abc" {
		t.Fatalf("short content unchanged: got %q", got)
	}
	if got := truncateUTF8Bytes("abcdef", 3); got != "abc" {
		t.Fatalf("truncate ascii: got %q", got)
	}
	// 多字节 rune 不得被切到一半。
	got := truncateUTF8Bytes("中文字", 4) // "中"=3 字节，再加"中"会到 6>4
	if !utf8.ValidString(got) {
		t.Fatalf("must be valid UTF-8: %q", got)
	}
	if len(got) > 4 {
		t.Fatalf("len = %d, must be ≤ 4", len(got))
	}
	// 4 字节 rune (emoji) 边界。
	got = truncateUTF8Bytes("😀x", 4) // 😀 = 4 字节
	if got != "😀" {
		t.Fatalf("4-byte rune kept if fits: got %q", got)
	}
	got = truncateUTF8Bytes("x😀", 4) // x=1 + 😀=4 =5 >4 → 只留 x
	if got != "x" {
		t.Fatalf("4-byte rune dropped if overflow: got %q", got)
	}
}

// TestTruncateUTF8Bytes_InvalidShortExpandsPastLimit 非法 UTF-8 字节长度 ≤ maxBytes
// 时不能走 fast-path 原样返回（design D5/F1：json.Marshal 会把每个非法字节替换为
// 3 字节 U+FFFD，使 ≤4096 的输入 marshal 后膨胀超限）。必须经 rune 扫描，输出仍是
// 有效 UTF-8 且字节长度 ≤ maxBytes。本用例构造 10 个非法字节，maxBytes=9：fast-path
// 因 len=10>9 不命中本就触发扫描，故额外构造 len≤maxBytes 的非法串验证。
func TestTruncateUTF8Bytes_InvalidShortExpandsPastLimit(t *testing.T) {
	// 4 个非法字节（0xFF），len=4 ≤ maxBytes=4，但 marshal 后每个替换为 3 字节
	// U+FFFD 共 12 字节 > 4。fast-path 若仅按字节长度判断会原样返回非法串。
	const invalid = "\xff\xff\xff\xff"
	if utf8.ValidString(invalid) {
		t.Fatal("prereq: input must be invalid UTF-8")
	}
	got := truncateUTF8Bytes(invalid, 4)
	if !utf8.ValidString(got) {
		t.Fatalf("output must be valid UTF-8, got %q", got)
	}
	if len(got) > 4 {
		t.Fatalf("output len = %d, must be ≤ 4", len(got))
	}
	// 4 个非法字节 → range 产生 4 个 U+FFFD（每个 3 字节）= 12 字节 > 4，故只能
	// 保留第一个 U+FFFD（3 字节），第二个会超限。
	if len(got) != 3 {
		t.Fatalf("expected 1 U+FFFD (3 bytes), got len %d: %q", len(got), got)
	}
}

// TestWecomChannel_InvalidUTF8BodyStaysValidUnderLimit 请求级 F1：渲染后 content
// 字节长度 ≤4096 但含非法 UTF-8，fast-path 会原样放行，json.Marshal 替换非法字节为
// 3 字节 U+FFFD 后膨胀超 4096。修复后 truncateUTF8Bytes 必须经 rune 扫描使
// markdown.content 仍为有效 UTF-8 且 ≤4096，且仍 POST（不因超限失败）。
func TestWecomChannel_InvalidUTF8BodyStaysValidUnderLimit(t *testing.T) {
	srv, captured := wecomCaptureServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"errcode":0}`))
	})
	in := wecomTestIntent()
	in.URL = "" // 避免 URL 行参与前缀计算
	// 渲染前缀 = "**" + Title + "**\n"。Title="等待你的回答"=6 个 rune=18 字节，
	// 前缀字节长度 = 2 + 18 + 2 + 1 = 23。用 4091 个非法字节使 content 总长
	// 23 + 4091 = 4114 > 4096（确保非 fast-path 长度分支），但本用例目标是验证
	// 「len ≤ maxBytes 但非法」路径：把 Body 设为 4073 个非法字节 → content
	// 长度 = 23 + 4073 = 4096 == maxBytes（命中旧 fast-path），marshal 后膨胀。
	const prefixLen = 2 + 3*6 + 2 + 1 // "**" + Title(18B) + "**" + "\n"
	if prefixLen != 23 {
		t.Fatalf("prefix arithmetic changed: %d", prefixLen)
	}
	bodyLen := wecomMaxContentLen - prefixLen // 4073 → content 恰好 4096 字节
	in.Body = strings.Repeat("\xff", bodyLen)
	if utf8.ValidString(in.Body) {
		t.Fatal("prereq: Body must be invalid UTF-8")
	}

	res := NewWecomChannel().Send(context.Background(), in, wecomTestConfig(srv.URL))
	if !res.OK {
		t.Fatalf("over-limit/invalid must still POST and succeed, Err=%q", res.Err)
	}
	var got wecomRequest
	if err := json.Unmarshal(captured.rawBody, &got); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if !utf8.ValidString(got.Markdown.Content) {
		t.Fatalf("posted markdown.content must be valid UTF-8, got %q", got.Markdown.Content)
	}
	if len(got.Markdown.Content) > wecomMaxContentLen {
		t.Fatalf("posted markdown.content len = %d, must be ≤ %d", len(got.Markdown.Content), wecomMaxContentLen)
	}
}

// TestWecomChannel_FailedSendErrSafeForDispatchLog F2.4：dispatch.go:168 日志只
// 打印 res.Err（`log.Printf("notify: channel %s failed for task %s (%s): %s", ...)`）。
// Send 失败时 res.Err MUST NOT 含 webhook URL、请求体或响应原文——此处对每个失败
// 场景断言 res.Err 干净，且按 dispatch 日志格式拼出的日志行也不含这些秘密。若 Send
// 误把 URL/响应原文塞进 Err，本测试与 ErrNeverLeaksSecrets 共同捕获。
func TestWecomChannel_FailedSendErrSafeForDispatchLog(t *testing.T) {
	in := wecomTestIntent()
	scenarios := []struct {
		name    string
		respond func(w http.ResponseWriter)
	}{
		{"non-2xx", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }},
		{"errcode non-zero", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"errcode":400,"errmsg":"raw-response-marker-xyz"}`))
		}},
		{"malformed json", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`raw-response-marker-xyz not json`))
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			srv, _ := wecomCaptureServer(t, sc.respond)
			res := NewWecomChannel().Send(context.Background(), in, wecomTestConfig(srv.URL))
			if res.OK {
				t.Fatal("must fail")
			}
			// 按 dispatch.go:168 同款格式拼日志行。
			logLine := fmt.Sprintf("notify: channel %s failed for task %s (%s): %s",
				"wecom", in.TaskID, in.Category, res.Err)
			for _, secret := range []string{wecomTestURLMarker, "leak-check-body-marker", "raw-response-marker-xyz", "等待你的回答"} {
				if strings.Contains(res.Err, secret) {
					t.Fatalf("Err leaks %q: %q", secret, res.Err)
				}
				if strings.Contains(logLine, secret) {
					t.Fatalf("dispatch log line leaks %q: %q", secret, logLine)
				}
			}
		})
	}
}
