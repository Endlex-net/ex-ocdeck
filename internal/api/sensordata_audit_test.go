package api

// sensordata_audit_test.go 敏感数据审查（opencode-v1-spi-provider tasks 5.2 /
// design D3「敏感数据保护」）：legacy auth.password 与 master secret MUST NOT
// 进入 API DTO、日志、事件、错误文本。审查面 = API 边界的输出投影：
//  1. 任务详情/列表 DTO 序列化形状（非空存储行注入）：TaskRow → taskRowDTO 无
//     binding/auth/password 字段（凭据事实来源只在 tasks.task_binding 列，DTO
//     投影面无对应成员）；详情/列表断言 HTTP 200、元素 ID 与列表长度；
//  2. 错误信封（errorBody）经实际 handler 序列化：错误由真实 provider 的
//     binding 解码/凭据解析入口（SPIProvider.BindingRuntimeEndpoint →
//     decodeV1Binding）生成——输入 binding 携带 legacy 密码，解码错误文本按
//     生产包装形态（OpError code=internal，task 层 provider binding endpoint
//     包装点同形）经 mapTaskErr 进入响应，扫描密码/master secret 零出现；
//  3. 能力健康透出通道（GET /api/v1/server/status 的 capabilities.reason）：
//     真实 provider 能力失败经 reportConversion 写入 HealthTracker——①解码
//     失败（BindingInvalid，输入携带 legacy 密码）；②上游 500 响应体透出链
//     （httpStatusError.Error() 保留响应体原文，上游为受信任本地 opencode
//     进程、错误响应不含 ocdeck 凭据——以响应体非秘密文本出现在 reason 中
//     证明该通道被审查覆盖）。reason 全文扫描凭据零出现。
//
// mutation 证据（红→还原，生产构造点夹带而非测试输入）：
//   - decodeV1Binding「未知 auth.kind」错误文本临时夹带 data.Auth.Password →
//     ②（错误信封）与 ③（capabilities.reason）同时红 → 还原。
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ocdeck/internal/application"
	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/opencode"
)

const (
	auditLegacyPW   = "legacy-audit-secret-pw"
	auditMasterPW   = "audit-master-secret"
	auditBadKind    = "weird"
	auditUpstreamBo = "upstream-boom-audit"
)

// newAuditServer 装配审查用 Server（真实路由表 + 注入 backend/项目桩）。
func newAuditServer(t *testing.T, tb *fakeTaskBackend) (*httptest.Server, *Server) {
	t.Helper()
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main", Kind: "repo"}
	s := &Server{
		cfg: testConfig(), mux: http.NewServeMux(),
		auth:      NewTokenAuthenticator(testConfig().Token),
		wsClients: newWSClientRegistry(), projs: projs, tasks: tb,
	}
	s.registerRoutes()
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	return ts, s
}

// auditDo 执行带认证的 GET/POST 并返回 (状态码, 响应体)；读取失败 fail。
func auditDo(t *testing.T, ts *httptest.Server, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer testtoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s read body: %v", method, path, err)
	}
	return resp.StatusCode, string(body)
}

// auditBindingData 构造携带 legacy 密码的 v1 binding data JSON（kind/taskId 可注入）。
func auditBindingData(kind, taskID string) []byte {
	return []byte(fmt.Sprintf(
		`{"port":50001,"directory":"/wt/audit","taskId":%q,"auth":{"kind":%q,"password":%q}}`,
		taskID, kind, auditLegacyPW))
}

func TestSensitiveDataAudit_DTOAndErrors(t *testing.T) {
	// master secret 注入真实解析环境（若错误/透出链意外触发凭据解析分支，
	// 夹带的解析值可被 secrets 扫描检出）。
	t.Setenv(appagent.MasterSecretEnv, auditMasterPW)
	// 合法生产形态行：last_error 为真实 provider 错误文本（不含凭据值）；
	// binding 材料只存在于 store 列（TaskRow 不携带）。
	row := application.TaskRow{
		ID: "t-audit", ProjectID: "p1", Name: "audit", Branch: "ocdeck/audit",
		BaseRef: "refs/heads/main", Status: application.StatusSuspended,
		WorktreePath: "/wt/audit", Mode: "worktree", PermissionMode: "ask",
		LastError: sql.NullString{String: "migration import: legacy runtime 健康检查失败: 401", Valid: true},
	}
	row2 := row
	row2.ID = "t-audit-2"
	row2.Name = "audit-2"

	tb := &fakeTaskBackend{}
	tb.getGetRow = &row
	tb.listRows = []application.TaskRow{row, row2}
	ts, _ := newAuditServer(t, tb)

	// ① 详情 + 列表（非空注入）：HTTP 200、元素 ID、列表长度；全响应体扫描。
	codeDetail, detail := auditDo(t, ts, http.MethodGet, "/api/v1/tasks/t-audit")
	if codeDetail != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", codeDetail)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(detail), &decoded); err != nil {
		t.Fatalf("decode detail body: %v\nbody=%s", err, detail)
	}
	var gotID string
	if err := json.Unmarshal(decoded["id"], &gotID); err != nil {
		t.Fatalf("decode detail id: %v", err)
	}
	if gotID != "t-audit" {
		t.Errorf("detail id = %q, want t-audit", gotID)
	}
	// DTO 无凭据形状成员：顶层字段名集合不得出现 password/auth/binding。
	for _, forbidden := range []string{"password", "auth", "binding"} {
		if _, ok := decoded[forbidden]; ok {
			t.Errorf("taskRowDTO 出现凭据形状字段 %q", forbidden)
		}
	}

	codeList, listBody := auditDo(t, ts, http.MethodGet, "/api/v1/projects/p1/tasks")
	if codeList != http.StatusOK {
		t.Fatalf("list status = %d, want 200", codeList)
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(listBody), &list); err != nil {
		t.Fatalf("decode list body: %v\nbody=%s", err, listBody)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	gotIDs := map[string]bool{list[0].ID: true, list[1].ID: true}
	for _, want := range []string{"t-audit", "t-audit-2"} {
		if !gotIDs[want] {
			t.Errorf("list 缺元素 id %q（got %v）", want, gotIDs)
		}
	}

	for _, body := range []string{detail, listBody} {
		for _, secret := range []string{auditLegacyPW, auditMasterPW} {
			if strings.Contains(body, secret) {
				t.Errorf("DTO 响应体泄露凭据材料（%.8s…）", secret)
			}
		}
	}

	// ② 错误信封经实际 handler：错误由真实 provider 解码入口生成（输入携带
	// legacy 密码），按生产包装形态进 mapTaskErr 序列化。
	prov := opencode.NewSPIProvider()
	badBinding := agentprovider.TaskBinding{
		Version: agentprovider.TaskBindingVersion, ProviderID: opencode.ProviderID,
		ProviderInstanceID: opencode.ProviderInstanceID,
		UpstreamSessionID:  "ses-audit", TerminalRef: "ocdeck-t-audit-runtime",
		Data: auditBindingData(auditBadKind, "t-audit"),
	}
	_, _, _, derr := prov.BindingRuntimeEndpoint(badBinding)
	if derr == nil {
		t.Fatal("真实 binding 解码入口对未知 auth.kind 必须失败（审查 fixture 前置）")
	}
	if strings.Contains(derr.Error(), auditLegacyPW) {
		t.Fatalf("真实解码入口错误文本本身泄露密码（生产缺陷，审查输入不成立）: %v", derr)
	}
	tb.reopenErr = application.NewOpErr(application.CodeInternal,
		fmt.Errorf("provider binding endpoint: %w", derr))
	code, errBody := auditDo(t, ts, http.MethodPost, "/api/v1/tasks/t-audit/attach/reopen")
	if code != http.StatusInternalServerError {
		t.Errorf("reopen error status = %d, want 500", code)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(errBody), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v\nbody=%s", err, errBody)
	}
	if envelope.Error.Code != string(CodeInternal) {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, CodeInternal)
	}
	for _, secret := range []string{auditLegacyPW, auditMasterPW} {
		if strings.Contains(errBody, secret) {
			t.Errorf("错误信封泄露凭据材料（%.8s…）", secret)
		}
	}
	// 审查面校验：kind 字面值（非秘密）确实透出——证明错误文本链在位。
	if !strings.Contains(errBody, auditBadKind) {
		t.Errorf("错误信封缺失 provider kind 文本（审查面错位）: %s", errBody)
	}
}

// TestSensitiveDataAudit_CapabilitiesChannel 能力健康透出通道审查（F3）：
// 真实 provider 能力失败 → reportConversion → HealthTracker →
// GET /api/v1/server/status 的 capabilities.reason 全文扫描。
func TestSensitiveDataAudit_CapabilitiesChannel(t *testing.T) {
	// master secret 注入真实解析环境（同 DTOAndErrors）。
	t.Setenv(appagent.MasterSecretEnv, auditMasterPW)
	tr := appagent.NewHealthTracker()
	prov := opencode.NewSPIProvider()
	prov.WithHealthReporter(func(h appagent.CapabilityHealth) {
		if h.ProviderID == "" {
			h.ProviderID = opencode.ProviderID
		}
		tr.Transition(h)
	}, tr.RuntimeEpoch)
	ctx := context.Background()

	// ① 解码失败（BindingInvalid）：输入 binding 携带 legacy 密码 + 未知 kind
	// → sessions.list 出口上报 degraded + reason 错误原文。（两场景 taskId 不同
	// → tracker identity 不同 → 各自独立落账。）
	badBinding := agentprovider.TaskBinding{
		Version: agentprovider.TaskBindingVersion, ProviderID: opencode.ProviderID,
		ProviderInstanceID: opencode.ProviderInstanceID,
		UpstreamSessionID:  "ses-audit", TerminalRef: "ocdeck-t-audit-runtime",
		Data: auditBindingData(auditBadKind, "t-audit"),
	}
	if _, err := prov.Sessions().List(ctx, badBinding); err == nil {
		t.Fatal("bad binding sessions.list must fail（审查 fixture 前置）")
	}

	// ② 上游 500 响应体透出链（httpStatusError 保留响应体）：合法 legacy
	// binding（带密码）指向 500 server → reason 含响应体非秘密文本（证明该
	// 通道在审查覆盖内）且不含凭据。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(auditUpstreamBo))
	}))
	defer upstream.Close()
	upPort, perr := portFromAuditURL(upstream.URL)
	if perr != nil {
		t.Fatalf("parse upstream port: %v", perr)
	}
	goodBinding := agentprovider.TaskBinding{
		Version: agentprovider.TaskBindingVersion, ProviderID: opencode.ProviderID,
		ProviderInstanceID: opencode.ProviderInstanceID,
		UpstreamSessionID:  "ses-audit", TerminalRef: "ocdeck-t-audit-runtime",
		Data: []byte(fmt.Sprintf(
			`{"port":%d,"directory":"/wt/audit","taskId":"t-audit-500","auth":{"kind":"legacy","password":%q}}`,
			upPort, auditLegacyPW)),
	}
	if _, err := prov.Sessions().List(ctx, goodBinding); err == nil {
		t.Fatal("upstream 500 sessions.list must fail（审查 fixture 前置）")
	}

	snap := tr.Snapshot()
	var sessionScope []appagent.CapabilityHealth
	for _, h := range snap {
		if h.Scope == appagent.ScopeRuntime {
			sessionScope = append(sessionScope, h)
		}
	}
	if len(sessionScope) < 2 {
		t.Fatalf("runtime 作用域健康记录 = %d, want ≥2（两个失败场景均须落账）", len(sessionScope))
	}
	var sawUpstreamBody bool
	for _, h := range sessionScope {
		if h.State != appagent.CapDegraded {
			t.Errorf("state = %s, want degraded（失败场景）", h.State)
		}
		if h.Reason == "" {
			t.Error("reason 空（失败场景 reason 必须透出错误原文）")
		}
		for _, secret := range []string{auditLegacyPW, auditMasterPW} {
			if strings.Contains(h.Reason, secret) {
				t.Errorf("capabilities.reason 泄露凭据材料（%.8s…）: %q", secret, h.Reason)
			}
		}
		if strings.Contains(h.Reason, auditUpstreamBo) {
			sawUpstreamBody = true
		}
	}
	if !sawUpstreamBody {
		t.Errorf("无 reason 含上游响应体文本 %q（500 透出通道未被审查覆盖）", auditUpstreamBo)
	}

	// API 投影：capabilities.reason 全文扫描（status handler 序列化面）。
	tb := &fakeTaskBackend{}
	statusTs, s := newAuditServer(t, tb)
	s.SetCapabilitiesProvider(tr.Snapshot)
	code, body := auditDo(t, statusTs, http.MethodGet, "/api/v1/server/status")
	if code != http.StatusOK {
		t.Fatalf("server status = %d", code)
	}
	var decoded struct {
		Capabilities []struct {
			Reason string `json:"reason"`
			State  string `json:"state"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode status body: %v\nbody=%s", err, body)
	}
	if len(decoded.Capabilities) == 0 {
		t.Fatal("status capabilities 为空（透出链断裂）")
	}
	for _, c := range decoded.Capabilities {
		for _, secret := range []string{auditLegacyPW, auditMasterPW} {
			if strings.Contains(c.Reason, secret) {
				t.Errorf("status capabilities.reason 泄露凭据材料（%.8s…）", secret)
			}
		}
	}
}

func portFromAuditURL(raw string) (int, error) {
	const prefix = "http://127.0.0.1:"
	if len(raw) <= len(prefix) || raw[:len(prefix)] != prefix {
		return 0, errors.New("unexpected httptest url: " + raw)
	}
	n := 0
	for _, c := range raw[len(prefix):] {
		if c < '0' || c > '9' {
			return 0, errors.New("unexpected httptest url: " + raw)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
