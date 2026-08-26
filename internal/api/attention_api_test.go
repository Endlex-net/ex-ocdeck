package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/opencode"
)

// attentionTaskBackend 注入 Attention 返回值，供 API 透出字段测试。
type attentionTaskBackend struct {
	*fakeTaskBackend
	attention   application.Attention
	attentionOk bool
}

func (b *attentionTaskBackend) Attention(taskID string) (application.Attention, bool) {
	return b.attention, b.attentionOk
}

func (b *attentionTaskBackend) Get(ctx context.Context, taskID string) (application.TaskRow, error) {
	return application.TaskRow{ID: taskID, ProjectID: "p1", Status: application.StatusActive}, nil
}

func TestGetTask_AttentionFields(t *testing.T) {
	perm := application.PendingPermission{
		PermissionRequest: opencode.PermissionRequest{
			ID: "perm1", SessionID: "s1", Permission: "bash", Patterns: []string{"rm", "ls"},
		},
		Since: 1700000000,
	}
	quest := application.PendingQuestion{
		QuestionRequest: opencode.QuestionRequest{
			ID: "quest1", SessionID: "s1",
			Questions: []opencode.QuestionItem{{Header: "h1", Question: "what?"}},
		},
		Since: 1700000001,
	}
	tb := &attentionTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		attention:       application.Attention{Permissions: []application.PendingPermission{perm}, Questions: []application.PendingQuestion{quest}},
		attentionOk:     true,
	}
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 1}

	s := newAPITestServer(t, tb)
	s.projs = projs
	s.RebuildRoutes()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto taskRowDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantAtt := attentionDTO{
		Permissions: []permissionDTO{
			{ID: "perm1", Permission: "bash", Patterns: []string{"rm", "ls"}, Since: 1700000000},
		},
		Questions: []questionDTO{
			{ID: "quest1", Questions: []questionItemDTO{{Header: "h1", Question: "what?"}}, Since: 1700000001},
		},
	}
	if !reflect.DeepEqual(dto.Attention, wantAtt) {
		t.Errorf("attention = %+v, want %+v", dto.Attention, wantAtt)
	}
	// since 本地首次观察时间字段验证
	if dto.Attention.Permissions[0].Since != 1700000000 {
		t.Errorf("perm since = %d, want 1700000000", dto.Attention.Permissions[0].Since)
	}
}

func TestGetTask_AttentionEmptyArrayNotNull(t *testing.T) {
	tb := &attentionTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		attention:       application.Attention{Permissions: []application.PendingPermission{}, Questions: []application.PendingQuestion{}},
		attentionOk:     false,
	}
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 1}

	s := newAPITestServer(t, tb)
	s.projs = projs
	s.RebuildRoutes()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto taskRowDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 空数组非 null
	if dto.Attention.Permissions == nil {
		t.Error("Permissions should be [] not null")
	}
	if dto.Attention.Questions == nil {
		t.Error("Questions should be [] not null")
	}
	if len(dto.Attention.Permissions) != 0 || len(dto.Attention.Questions) != 0 {
		t.Errorf("expected empty arrays, got %+v", dto.Attention)
	}
}

// TestListActiveSessions_AttentionEmptyArrayNotNull 验证 tasks/active 元素 attention 空数组非 null。
func TestListActiveSessions_AttentionEmptyArrayNotNull(t *testing.T) {
	rows := []application.ActiveTaskOverviewRow{
		activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 300),
	}
	tb := newActiveSessionsBackend(rows...)
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, got := readAndDecode(t, resp.Body)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Attention.Permissions == nil || got[0].Attention.Questions == nil {
		t.Error("attention arrays should be [] not null")
	}
}

// TestListProjects_TaskSummaries 验证 projects 摘要 tasks 数组字段。
func TestListProjects_TaskSummaries(t *testing.T) {
	tb := &projectSummaryBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		summaries: []application.ProjectTaskSummary{
			{TaskID: "t1", Name: "taskA", ProjectID: "p1", Status: application.StatusActive,
				InitStatus: application.InitStatusNone, Branch: "b1", WorktreePath: "/wt1",
				UpdatedAt: 100, AttentionCount: 2},
			{TaskID: "t2", Name: "taskB", ProjectID: "p1", Status: application.StatusSuspended,
				InitStatus: application.InitStatusSucceeded, Branch: "b2", WorktreePath: "/wt2",
				UpdatedAt: 200, AttentionCount: 0},
		},
		agentStatusSnapshot: map[string]string{"t1": "busy"},
	}
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 50}
	projs.counts["p1"] = storeTaskCounts{Total: 2, ByStatus: map[string]int{application.StatusActive: 1, application.StatusSuspended: 1}}

	s := newAPITestServer(t, tb)
	s.projs = projs
	s.RebuildRoutes()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []projectDTO
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("projects len = %d, want 1", len(got))
	}
	if got[0].TaskSummaries == nil {
		t.Fatal("tasks summaries should be [] not null")
	}
	if len(got[0].TaskSummaries) != 2 {
		t.Fatalf("tasks summaries len = %d, want 2", len(got[0].TaskSummaries))
	}
	// updated_at / attention_count / agentStatus 快照字段透传验证。updated_at 为 P1.9a
	// 迁移回归锚点：seed 值（100/200）必须原样到达响应，映射丢失或零值即失败。
	// agentStatus（projects-stream）：t1（active）读内存快照 busy；t2（suspended）不读、省略。
	for _, ts := range got[0].TaskSummaries {
		switch ts.ID {
		case "t1":
			if ts.UpdatedAt != 100 {
				t.Errorf("t1 updated_at = %d, want 100", ts.UpdatedAt)
			}
			if ts.AttentionCount != 2 {
				t.Errorf("t1 attention_count = %d, want 2", ts.AttentionCount)
			}
			if ts.AgentStatus != "busy" {
				t.Errorf("t1 agentStatus = %q, want busy (memory snapshot)", ts.AgentStatus)
			}
		case "t2":
			if ts.UpdatedAt != 200 {
				t.Errorf("t2 updated_at = %d, want 200", ts.UpdatedAt)
			}
			if ts.AgentStatus != "" {
				t.Errorf("t2 agentStatus = %q, want empty (suspended, not hydrated)", ts.AgentStatus)
			}
		}
	}
	// projects-stream：/projects MUST NOT 实时探测 agentStatus。
	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus realtime probe called %d times, want 0", calls)
	}
}

// projectSummaryBackend 注入 ListProjectTaskSummaries 返回值；projects-stream 起 agentStatus
// 经 agentStatusSnapshot 内存快照注入（与 activeSessionsBackend 同模式），
// 实时探测 AgentStatus 仅记录调用供断言「不再实时探测」。
type projectSummaryBackend struct {
	*fakeTaskBackend
	summaries []application.ProjectTaskSummary
	// agentStatusSnapshot 注入每个 taskID 的内存快照值；缺省返回空串（降级）。
	agentStatusSnapshot map[string]string
	// agentStatusCalls 记录实时探测 AgentStatus 被调用的 taskID。
	agentStatusMu    sync.Mutex
	agentStatusCalls []string
}

func (b *projectSummaryBackend) ListProjectTaskSummaries(ctx context.Context) ([]application.ProjectTaskSummary, error) {
	out := make([]application.ProjectTaskSummary, len(b.summaries))
	copy(out, b.summaries)
	return out, nil
}

// AgentStatus 实时探测：projects-stream 后 /projects 改读内存快照，本方法仅记录调用供断言。
func (b *projectSummaryBackend) AgentStatus(ctx context.Context, taskID string) string {
	b.agentStatusMu.Lock()
	b.agentStatusCalls = append(b.agentStatusCalls, taskID)
	b.agentStatusMu.Unlock()
	return ""
}

func (b *projectSummaryBackend) AgentStatusSnapshot(taskID string) string {
	return b.agentStatusSnapshot[taskID]
}

func (b *projectSummaryBackend) agentStatusCallCount() int {
	b.agentStatusMu.Lock()
	defer b.agentStatusMu.Unlock()
	return len(b.agentStatusCalls)
}

// TestListProjects_AgentStatusSnapshotDegradation 验证 active 任务无内存快照时
// agentStatus 省略（projects-stream 降级语义：快照不可用 → omitempty，不实时探测、不阻塞响应）。
func TestListProjects_AgentStatusSnapshotDegradation(t *testing.T) {
	tb := &projectSummaryBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		summaries: []application.ProjectTaskSummary{
			{TaskID: "t1", Name: "taskA", ProjectID: "p1", Status: application.StatusActive,
				InitStatus: application.InitStatusNone, Branch: "b1", WorktreePath: "/wt1", UpdatedAt: 100},
		},
	}
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 50}
	projs.counts["p1"] = storeTaskCounts{Total: 1, ByStatus: map[string]int{application.StatusActive: 1}}

	s := newAPITestServer(t, tb)
	s.projs = projs
	s.RebuildRoutes()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []projectDTO
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || len(got[0].TaskSummaries) != 1 {
		t.Fatalf("unexpected response: %+v", got)
	}
	// 无快照 → 空串省略（降级）。
	if got[0].TaskSummaries[0].AgentStatus != "" {
		t.Errorf("agentStatus = %q, want empty (snapshot unavailable, degraded)", got[0].TaskSummaries[0].AgentStatus)
	}
	// MUST NOT 实时探测。
	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus realtime probe called %d times, want 0", calls)
	}
}
