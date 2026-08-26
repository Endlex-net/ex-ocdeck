package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/application"
)

// activeSessionsBackend 可注入 ListActiveTaskOverview 返回值与 agentStatus 内存
// 快照行为，供 GET /api/v1/sessions/active handler 测试（cross-project-active-sessions
// D3/D4；P2.2 起改读 AgentStatusSnapshot，实时探测 AgentStatus 不再被调用）。
type activeSessionsBackend struct {
	*fakeTaskBackend
	overviewRows []application.ActiveTaskOverviewRow
	overviewErr  error
	// agentStatusSnapshot 注入每个 taskID 的内存快照值；缺省 taskID 返回空串（降级）。
	agentStatusSnapshot map[string]string
	// attentions 注入每个 taskID 的注意力快照（缺省走 fakeTaskBackend 空快照）。
	attentions map[string]application.Attention
	// agentStatusCalls 记录实时探测 AgentStatus 被调用的 taskID（断言 REST 不再实时探测）。
	agentStatusMu    sync.Mutex
	agentStatusCalls []string
}

func newActiveSessionsBackend(rows ...application.ActiveTaskOverviewRow) *activeSessionsBackend {
	return &activeSessionsBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		overviewRows:    rows,
	}
}

func (b *activeSessionsBackend) ListActiveTaskOverview(ctx context.Context) ([]application.ActiveTaskOverviewRow, error) {
	if b.overviewErr != nil {
		return nil, b.overviewErr
	}
	return b.overviewRows, nil
}

// AgentStatus 实时探测：P2.2 后 REST/SSE 均改读内存快照，本方法仅记录调用供断言
// 「不再实时探测」。
func (b *activeSessionsBackend) AgentStatus(ctx context.Context, taskID string) string {
	b.agentStatusMu.Lock()
	b.agentStatusCalls = append(b.agentStatusCalls, taskID)
	b.agentStatusMu.Unlock()
	return ""
}

func (b *activeSessionsBackend) AgentStatusSnapshot(taskID string) string {
	return b.agentStatusSnapshot[taskID]
}

func (b *activeSessionsBackend) Attention(taskID string) (application.Attention, bool) {
	if b.attentions != nil {
		if att, ok := b.attentions[taskID]; ok {
			return att, true
		}
		return application.Attention{}, false
	}
	return b.fakeTaskBackend.Attention(taskID)
}

func (b *activeSessionsBackend) agentStatusCallCount() int {
	b.agentStatusMu.Lock()
	defer b.agentStatusMu.Unlock()
	return len(b.agentStatusCalls)
}

// activeRow 构造 application.ActiveTaskOverviewRow 测试行辅助。
func activeRow(id, projectID, projectName, name, branch, wt string, lastActive int64) application.ActiveTaskOverviewRow {
	return application.ActiveTaskOverviewRow{
		ID: id, ProjectID: projectID, ProjectName: projectName, Name: name,
		Branch: branch, WorktreePath: wt, LastActiveAt: lastActive,
	}
}

// decodeActiveSessions 解码响应体为 activeSessionDTO 切片，并校验为非 null 的 JSON 数组。
func decodeActiveSessions(t *testing.T, body string) []activeSessionDTO {
	t.Helper()
	body = strings.TrimSpace(body)
	if body == "null" {
		t.Fatal("response body is null, want []")
	}
	var got []activeSessionDTO
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, body)
	}
	return got
}

// readAndDecode 读取响应体一次，返回原始字符串与解码结果（避免 double-read 空体 bug）。
func readAndDecode(t *testing.T, body io.Reader) (string, []activeSessionDTO) {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw), decodeActiveSessions(t, string(raw))
}

func TestListActiveSessions_HappyPath(t *testing.T) {
	rows := []application.ActiveTaskOverviewRow{
		activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 300),
		activeRow("t2", "p2", "projB", "taskB", "bB", "/wtB", 200),
	}
	tb := newActiveSessionsBackend(rows...)
	tb.agentStatusSnapshot = map[string]string{"t1": "busy", "t2": "idle"}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, got := readAndDecode(t, resp.Body)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// 字段断言 + sort（last_active_at DESC）。
	want := []activeSessionDTO{
		{TaskID: "t1", ProjectID: "p1", ProjectName: "projA", Name: "taskA", Branch: "bA", WorktreePath: "/wtA", LastActiveAt: 300, AgentStatus: "busy", Attention: attentionDTO{Permissions: []permissionDTO{}, Questions: []questionDTO{}}},
		{TaskID: "t2", ProjectID: "p2", ProjectName: "projB", Name: "taskB", Branch: "bB", WorktreePath: "/wtB", LastActiveAt: 200, AgentStatus: "idle", Attention: attentionDTO{Permissions: []permissionDTO{}, Questions: []questionDTO{}}},
	}
	for i, w := range want {
		if !reflect.DeepEqual(got[i], w) {
			t.Errorf("dto[%d] = %+v, want %+v", i, got[i], w)
		}
	}
	// P2.2：agentStatus 来自内存快照，MUST NOT 实时探测。
	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus realtime probe called %d times, want 0", calls)
	}
}

// TestListActiveSessions_PartialDegradationKeepsSuccessfulAgentStatus 验证多任务场景下
// 部分任务 agentStatus 内存快照不可用时，可用任务仍携带 agentStatus，仅不可用任务
// 省略字段（cross-project-active-sessions D4：单点降级不阻塞其他）。
func TestListActiveSessions_PartialDegradationKeepsSuccessfulAgentStatus(t *testing.T) {
	rows := []application.ActiveTaskOverviewRow{
		activeRow("t-ok", "p1", "projA", "taskA", "bA", "/wtA", 200),
		activeRow("t-fail", "p2", "projB", "taskB", "bB", "/wtB", 100),
	}
	tb := newActiveSessionsBackend(rows...)
	// t-ok 快照可用 busy；t-fail 无快照（空串降级）→ agentStatus 省略。
	tb.agentStatusSnapshot = map[string]string{"t-ok": "busy"}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 读取一次 body，复用于 raw 与 decoded 断言（避免 double-read 空体）。
	raw, got := readAndDecode(t, resp.Body)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// sort by last_active_at DESC: t-ok(200) first, t-fail(100) second.
	if got[0].TaskID != "t-ok" || got[1].TaskID != "t-fail" {
		t.Fatalf("sort order = [%s,%s], want [t-ok,t-fail]", got[0].TaskID, got[1].TaskID)
	}
	if got[0].AgentStatus != "busy" {
		t.Errorf("t-ok agentStatus = %q, want busy (available snapshot retained)", got[0].AgentStatus)
	}
	if got[1].AgentStatus != "" {
		t.Errorf("t-fail agentStatus = %q, want empty (degraded)", got[1].AgentStatus)
	}
	// 原始 JSON：t-ok 含 agentStatus，t-fail 省略。
	if !strings.Contains(raw, `"agentStatus":"busy"`) {
		t.Errorf("raw response missing t-ok agentStatus:busy: %s", raw)
	}
	// t-fail 行不应出现 agentStatus 字段（验证 omit 生效：仅 1 处 agentStatus 出现）。
	if strings.Count(raw, "agentStatus") != 1 {
		t.Errorf("raw response has %d agentStatus occurrences, want 1 (only t-ok): %s", strings.Count(raw, "agentStatus"), raw)
	}
}

func TestListActiveSessions_EmptyReturnsArrayNotNull(t *testing.T) {
	tb := newActiveSessionsBackend() // 无行
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := strings.TrimSpace(string(raw))
	if body == "null" {
		t.Fatal("empty response is null, want []")
	}
	if body != "[]" {
		t.Errorf("empty body = %q, want []", body)
	}
	// 空结果不应触发任何 agentStatus 读取。
	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus realtime probe called %d times on empty result, want 0", calls)
	}
}

func TestListActiveSessions_StoreFailureReturns500NoHydration(t *testing.T) {
	tb := newActiveSessionsBackend()
	tb.overviewErr = errStoreFailure
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeInternal {
		t.Errorf("error code = %q, want %q", eb.Error.Code, CodeInternal)
	}
	// store 失败 MUST NOT 进入组装（无 agentStatus 读取）。
	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus realtime probe called %d times on store failure, want 0 (no assembly)", calls)
	}
}

func TestListActiveSessions_UnauthorizedWithoutToken(t *testing.T) {
	tb := newActiveSessionsBackend()
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/sessions/active", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestListActiveSessions_ReadModelImmutability 验证组装仅读读模型字段
// （task_id/last_active_at 等来自 store，MUST NOT 被改写）。
func TestListActiveSessions_ReadModelImmutability(t *testing.T) {
	rows := []application.ActiveTaskOverviewRow{
		activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100),
	}
	tb := newActiveSessionsBackend(rows...)
	tb.agentStatusSnapshot = map[string]string{"t1": "busy"}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, got := readAndDecode(t, resp.Body)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	r := got[0]
	// 读模型字段保持 store 原值，仅 AgentStatus 被填充。
	if r.TaskID != "t1" || r.ProjectID != "p1" || r.ProjectName != "projA" || r.Name != "taskA" || r.Branch != "bA" || r.WorktreePath != "/wtA" || r.LastActiveAt != 100 {
		t.Errorf("read-model fields mutated: %+v", r)
	}
	if r.AgentStatus != "busy" {
		t.Errorf("agentStatus = %q, want busy", r.AgentStatus)
	}
}

var errStoreFailure = errString("store failure")

type errString string

func (e errString) Error() string { return string(e) }
