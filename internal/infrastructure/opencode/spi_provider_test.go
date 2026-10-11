package opencode

// spi_provider_test.go 覆盖 v1 适配器窄接口的 D8 映射（tasks 3.3 验证）：
// binding 解析 fail-closed、HTTP 错误默认映射 + 操作级例外（Reply 按 _tag、
// Title 404→CapabilityUnavailable、Prompt 四分类通道规则）、REST 状态未知枚举
// → unknown、ListResult.Overflow。SSE typed events 映射见 spi_events_test.go；
// 连接管理（共享 loopback transport）见 spi_connection_test.go。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// spiTestServer 起一个最小 opencode 上游；handler 可编程各端点行为。
// 端口从监听地址解析——适配器 binding 携带该端口。
type spiTestServer struct {
	srv  *httptest.Server
	port int
}

func newSPITestServer(t *testing.T, h http.Handler) *spiTestServer {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var port int
	if _, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port); err != nil {
		t.Fatalf("parse port from %s: %v", srv.URL, err)
	}
	return &spiTestServer{srv: srv, port: port}
}

// testBinding 构造指向测试 server 的 v1 binding（auth.kind=derived，密码现场
// 计算不入 fixture——这里仅作为 transport 凭据占位，server 不校验值）。
func (s *spiTestServer) testBinding() domain.TaskBinding {
	data, _ := json.Marshal(v1BindingData{
		Port: s.port, Directory: "/wt/task",
		// legacy 形态（迁移导入的存量随机密码）——派生形态的 resolveBinding
		// 路径由 charx 契约组 DerivedCredentials fixture 覆盖（需 master secret）。
		// C6 残余（review attempt 2）：data 携带 taskId（迁移导入按任务行 ID 显式
		// 上下文写入；健康观测身份键= t1 ≠ UpstreamSessionID）。
		TaskID: "t1",
		Auth:   v1BindingAuth{Kind: appagent.LegacyCredKind, Password: "pw-test"},
	})
	return domain.TaskBinding{
		Version: 1, ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID: "ses_a", TerminalRef: "ocdeck-t1-runtime", Data: data,
	}
}

func v1Key(upstream string) domain.SessionKey {
	return domain.SessionKey{ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID, UpstreamID: upstream}
}

func TestSPI_ResolveBinding_FailClosed(t *testing.T) {
	p := NewSPIProvider()
	ctx := context.Background()

	// 非 v1 provider → BindingInvalid。
	foreign := domain.TaskBinding{Version: 1, ProviderID: "codex", ProviderInstanceID: "default",
		UpstreamSessionID: "s", TerminalRef: "t", Data: json.RawMessage(`{"port":1,"directory":"/d","auth":{"kind":"derived"}}`)}
	if _, err := p.Sessions().List(ctx, foreign); !errors.Is(err, appagent.ErrBindingInvalid) {
		t.Errorf("foreign provider = %v, want binding_invalid", err)
	}
	// 畸形 Data → BindingInvalid。
	bad := domain.TaskBinding{Version: 1, ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID: "s", TerminalRef: "t", Data: json.RawMessage(`{not-json`)}
	if _, err := p.Sessions().List(ctx, bad); !errors.Is(err, appagent.ErrBindingInvalid) {
		t.Errorf("malformed data = %v, want binding_invalid", err)
	}
	// 缺 port/directory/auth.kind → BindingInvalid。
	missing := domain.TaskBinding{Version: 1, ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID: "s", TerminalRef: "t", Data: json.RawMessage(`{"port":0,"directory":"","auth":{"kind":""}}`)}
	if _, err := p.Sessions().List(ctx, missing); !errors.Is(err, appagent.ErrBindingInvalid) {
		t.Errorf("missing fields = %v, want binding_invalid", err)
	}
	// 跨命名空间 SessionKey → BindingInvalid（specs 归属规则）。
	ts := newSPITestServer(t, http.NotFoundHandler())
	if _, err := p.Sessions().Get(ctx, ts.testBinding(), domain.SessionKey{ProviderID: "codex", ProviderInstanceID: "d", UpstreamID: "x"}); !errors.Is(err, appagent.ErrBindingInvalid) {
		t.Errorf("cross-namespace key = %v, want binding_invalid", err)
	}
	// supervisor/terminal 中间态占位：显式 fail-closed（CapabilityUnavailable）。
	if _, err := p.Supervisor().Acquire(ctx, appagent.TaskRef{TaskID: "t1"}); !errors.Is(err, appagent.ErrCapabilityUnavailable) {
		t.Errorf("supervisor stub = %v, want capability_unavailable", err)
	}
	if _, err := p.Terminal().AttachCommand(ctx, ts.testBinding()); !errors.Is(err, appagent.ErrCapabilityUnavailable) {
		t.Errorf("terminal stub = %v, want capability_unavailable", err)
	}
}

func TestSPI_Sessions_List_OverflowSemantics(t *testing.T) {
	var gotLimit string
	ts := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotLimit = r.URL.Query().Get("limit")
		limit, _ := strconv.Atoi(gotLimit)
		out := make([]map[string]interface{}, 0, limit)
		for i := 0; i < limit; i++ { // count == limit → 客户端判定 overflow
			out = append(out, map[string]interface{}{
				"id":   fmt.Sprintf("ses_%03d", i),
				"time": map[string]interface{}{"created": 1.5, "updated": 2.5},
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	p := NewSPIProvider()
	res, err := p.Sessions().List(context.Background(), ts.testBinding())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotLimit != "1000" {
		t.Errorf("limit param = %q, want 1000", gotLimit)
	}
	if !res.Overflow {
		t.Error("count==limit 必须映射 Overflow=true（不完整快照信号，非错误）")
	}
	if len(res.Sessions) != 1000 {
		t.Fatalf("sessions = %d, want 1000（overflow 仍携带列表）", len(res.Sessions))
	}
	first := res.Sessions[0]
	if first.Key != v1Key("ses_000") || first.CreatedAt != 1 || first.UpdatedAt != 2 {
		t.Errorf("session 形状漂移: %+v", first)
	}

	// 少于 limit → Overflow=false。
	ts2 := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": "ses_parent", "parentID": "", "time": map[string]interface{}{"created": 1, "updated": 2}},
			{"id": "ses_child", "parentID": "ses_parent", "time": map[string]interface{}{"created": 1, "updated": 2}},
		})
	}))
	res2, err := p.Sessions().List(context.Background(), ts2.testBinding())
	if err != nil {
		t.Fatalf("List2: %v", err)
	}
	if res2.Overflow || len(res2.Sessions) != 2 {
		t.Fatalf("res2 = %+v", res2)
	}
	if res2.Sessions[1].Parent == nil || res2.Sessions[1].Parent.UpstreamID != "ses_parent" {
		t.Errorf("parentID 映射漂移: %+v", res2.Sessions[1])
	}
}

func TestSPI_Sessions_GetDelete_Classification(t *testing.T) {
	ts := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_a", "time": map[string]interface{}{"created": 1, "updated": 2}})
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_a":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_a", "time": map[string]interface{}{"created": 1, "updated": 2}})
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodDelete && r.URL.Path == "/session/ses_a":
			_, _ = w.Write([]byte("true"))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	p := NewSPIProvider()
	ctx := context.Background()

	if s, err := p.Sessions().Get(ctx, ts.testBinding(), v1Key("ses_a")); err != nil || s.Key != v1Key("ses_a") {
		t.Errorf("Get ok = (%+v, %v)", s, err)
	}
	if _, err := p.Sessions().Get(ctx, ts.testBinding(), v1Key("missing")); !errors.Is(err, appagent.ErrNotFound) {
		t.Errorf("Get 404 = %v, want not_found", err)
	}
	if err := p.Sessions().Delete(ctx, ts.testBinding(), v1Key("ses_a")); err != nil {
		t.Errorf("Delete ok = %v", err)
	}
	if err := p.Sessions().Delete(ctx, ts.testBinding(), v1Key("boom")); !errors.Is(err, appagent.ErrRuntimeUnavailable) {
		t.Errorf("Delete 500 = %v, want runtime_unavailable", err)
	}
	// Create：D7 形状无 title；响应 ID 供原子 claim。
	if s, err := p.Sessions().Create(ctx, ts.testBinding()); err != nil || s.Key != v1Key("ses_a") {
		t.Errorf("Create = (%+v, %v)", s, err)
	}
}

func TestSPI_TitleSync_404CapabilityException(t *testing.T) {
	titleStatus := 200
	var gotTitle string
	ts := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotTitle = body.Title
		w.WriteHeader(titleStatus)
		if titleStatus == 200 {
			// 成功响应是 Session JSON（client 校验顶层 id 与请求一致）。
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_a", "time": map[string]interface{}{"created": 1, "updated": 2}})
		}
	}))
	p := NewSPIProvider()
	ctx := context.Background()
	b := ts.testBinding()

	if err := p.Titles().Sync(ctx, b, v1Key("ses_a"), "新标题"); err != nil {
		t.Fatalf("Sync ok: %v", err)
	}
	if gotTitle != "新标题" {
		t.Errorf("title body = %q", gotTitle)
	}
	// 操作级例外（D8）：一切 404 → CapabilityUnavailable（能力降级），
	// MUST NOT 套用「对象不存在」默认分类。
	titleStatus = http.StatusNotFound
	if err := p.Titles().Sync(ctx, b, v1Key("ses_a"), "x"); !errors.Is(err, appagent.ErrCapabilityUnavailable) {
		t.Errorf("Sync 404 = %v, want capability_unavailable", err)
	}
	// 非 404 失败走默认映射。
	titleStatus = http.StatusInternalServerError
	if err := p.Titles().Sync(ctx, b, v1Key("ses_a"), "x"); !errors.Is(err, appagent.ErrRuntimeUnavailable) {
		t.Errorf("Sync 500 = %v, want runtime_unavailable", err)
	}
}

func TestSPI_Status_Current_UnknownEnum(t *testing.T) {
	var gotDir string
	ts := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDir = r.URL.Query().Get("directory")
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{
			"ses_a": {"type": "busy"},
			"ses_b": {"type": "totally-new-state"}, // 未知枚举
			"ses_c": {},                            // 缺失枚举
		})
	}))
	p := NewSPIProvider()
	cur, err := p.Status().Current(context.Background(), ts.testBinding())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if gotDir != "/wt/task" {
		t.Errorf("directory param = %q", gotDir)
	}
	if cur[v1Key("ses_a")] != domain.StatusBusy {
		t.Errorf("ses_a = %v, want busy", cur[v1Key("ses_a")])
	}
	// D8 REST 路径：未知/缺失枚举 → StatusUnknown（不得报错、不得猜测）。
	if cur[v1Key("ses_b")] != domain.StatusUnknown || cur[v1Key("ses_c")] != domain.StatusUnknown {
		t.Errorf("unknown 枚举映射 = %v / %v, want unknown", cur[v1Key("ses_b")], cur[v1Key("ses_c")])
	}
	if len(cur) != 3 {
		t.Errorf("len = %d, want 3", len(cur))
	}
}

func TestSPI_Prompt_FourChannelRule(t *testing.T) {
	status := 204
	body := ""
	ts := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/prompt_async") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	p := NewSPIProvider()
	ctx := context.Background()
	b := ts.testBinding()

	// accepted：204，StatusCode=204，Body/Detail 空。
	res, err := p.Prompts().Send(ctx, b, v1Key("ses_a"), appagent.PromptRequest{Text: "hi", MessageID: "msg-1"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome != appagent.PromptAccepted || res.StatusCode != 204 || res.Body != "" || res.Detail != "" {
		t.Errorf("accepted 形状 = %+v", res)
	}
	// http_response：实际状态码 + 有界截断体。
	status, body = 500, `{"error":"boom"}`
	res, _ = p.Prompts().Send(ctx, b, v1Key("ses_a"), appagent.PromptRequest{Text: "hi", MessageID: "msg-2"})
	if res.Outcome != appagent.PromptHTTPResponse || res.StatusCode != 500 || !strings.Contains(res.Body, "boom") || res.Detail != "" {
		t.Errorf("http_response 形状 = %+v", res)
	}
	// transport_unknown：连接拒绝（无监听端口）——MUST NOT 经 error 通道返回。
	dead := domain.TaskBinding{
		Version: 1, ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID: "ses_a", TerminalRef: "t",
		Data: json.RawMessage(`{"port":1,"directory":"/wt","auth":{"kind":"legacy","password":"x"}}`),
	}
	res, err = p.Prompts().Send(ctx, dead, v1Key("ses_a"), appagent.PromptRequest{Text: "hi", MessageID: "msg-3"})
	if err != nil {
		t.Fatalf("transport failure MUST NOT return error: %v", err)
	}
	if res.Outcome != appagent.PromptTransportUnknown || res.StatusCode != 0 || res.Body != "" || res.Detail == "" {
		t.Errorf("transport_unknown 形状 = %+v", res)
	}
	// pre_send_failure 分支为 client 本地构造失败（prompt_async.go 现状），
	// 由既有 prompt_async_test.go 覆盖；通道规则（仅准入失败走 error）已由
	// binding_invalid 用例验证。
}

func TestSPI_Attention_ReplyClassification(t *testing.T) {
	replyStatus := 200
	replyBody := "true"
	ts := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/permission/") || !strings.HasSuffix(r.URL.Path, "/reply") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(replyStatus)
		_, _ = w.Write([]byte(replyBody))
	}))
	p := NewSPIProvider()
	ctx := context.Background()
	b := ts.testBinding()

	perm := domain.Attention{ID: "r1", Kind: domain.AttentionPermission, Session: v1Key("ses_a"), Permission: &domain.PermissionPayload{ID: "r1"}}

	// 200 true → 成功。
	if err := p.Attention().Reply(ctx, b, perm, appagent.AttentionReply{Action: "once"}); err != nil {
		t.Fatalf("Reply ok: %v", err)
	}
	// Action 越界（含 always）：进入 provider 前拒绝（零请求副作用）。
	if err := p.Attention().Reply(ctx, b, perm, appagent.AttentionReply{Action: "always"}); err == nil {
		t.Error("Action=always MUST 被拒绝（D6 契约）")
	}
	// 404 + _tag=PermissionNotFoundError → NotFound（已了结竞态）。
	replyStatus, replyBody = 404, `{"_tag":"PermissionNotFoundError"}`
	if err := p.Attention().Reply(ctx, b, perm, appagent.AttentionReply{Action: "once"}); !errors.Is(err, appagent.ErrNotFound) {
		t.Errorf("gone 404 = %v, want not_found", err)
	}
	// 404 其他 → CapabilityUnavailable（路由缺失=能力端点不存在）。
	replyBody = `{}`
	if err := p.Attention().Reply(ctx, b, perm, appagent.AttentionReply{Action: "once"}); !errors.Is(err, appagent.ErrCapabilityUnavailable) {
		t.Errorf("route 404 = %v, want capability_unavailable", err)
	}
	// 401 → BindingInvalid（凭据失效）。
	replyStatus = 401
	if err := p.Attention().Reply(ctx, b, perm, appagent.AttentionReply{Action: "once"}); !errors.Is(err, appagent.ErrBindingInvalid) {
		t.Errorf("401 = %v, want binding_invalid", err)
	}
	// 500 → 默认映射 RuntimeUnavailable（结果未知类）。
	replyStatus = 500
	if err := p.Attention().Reply(ctx, b, perm, appagent.AttentionReply{Action: "once"}); !errors.Is(err, appagent.ErrRuntimeUnavailable) {
		t.Errorf("500 = %v, want runtime_unavailable", err)
	}
	// question reply：v1 client 无实现方 → 显式 CapabilityUnavailable（不猜测）。
	quest := domain.Attention{ID: "q1", Kind: domain.AttentionQuestion, Session: v1Key("ses_a"), Question: &domain.QuestionPayload{ID: "q1"}}
	if err := p.Attention().Reply(ctx, b, quest, appagent.AttentionReply{Answers: []string{"y"}}); !errors.Is(err, appagent.ErrCapabilityUnavailable) {
		t.Errorf("question reply = %v, want capability_unavailable", err)
	}
}

func TestSPI_Attention_ListPayloads(t *testing.T) {
	ts := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/permission":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "r1", "sessionID": "ses_a", "permission": "bash", "patterns": []string{"x"}, "metadata": map[string]interface{}{"path": "a.go"}},
			})
		case "/question":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "q1", "sessionID": "ses_a", "questions": []map[string]string{{"header": "h", "question": "继续？"}}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	p := NewSPIProvider()
	ctx := context.Background()
	b := ts.testBinding()

	perms, err := p.Attention().ListPermissions(ctx, b, v1Key("ses_a"))
	if err != nil || len(perms) != 1 {
		t.Fatalf("ListPermissions = (%+v, %v)", perms, err)
	}
	if perms[0].Permission != "bash" || len(perms[0].Patterns) != 1 || string(perms[0].Metadata) == "" || !strings.Contains(string(perms[0].Metadata), "a.go") {
		t.Errorf("permission payload = %+v", perms[0])
	}
	quests, err := p.Attention().ListQuestions(ctx, b, v1Key("ses_a"))
	if err != nil || len(quests) != 1 || quests[0].Questions[0].Question != "继续？" {
		t.Fatalf("ListQuestions = (%+v, %v)", quests, err)
	}
}
