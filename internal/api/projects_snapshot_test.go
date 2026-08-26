package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"ocdeck/internal/application"
)

// newProjectsSnapshotFixture 构造组装测试的 backend/store：p1（repo，active t1 +
// suspended t2）、p2（dir，无任务）。t1 注入 agentStatus 内存快照 busy，t2 无快照。
func newProjectsSnapshotFixture() (*projectSummaryBackend, *fakeProjectStore) {
	tb := &projectSummaryBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		summaries: []application.ProjectTaskSummary{
			{TaskID: "t1", Name: "taskA", ProjectID: "p1", Status: application.StatusActive,
				InitStatus: application.InitStatusNone, Branch: "b1", WorktreePath: "/wt1",
				UpdatedAt: 100, AttentionCount: 2, Notice: `{"items":["n"]}`},
			{TaskID: "t2", Name: "taskB", ProjectID: "p1", Status: application.StatusSuspended,
				InitStatus: application.InitStatusSucceeded, Branch: "b2", WorktreePath: "/wt2",
				LastError: "boom", UpdatedAt: 200},
		},
		agentStatusSnapshot: map[string]string{"t1": "busy"},
	}
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 50}
	projs.projects["p2"] = storeProjectRow{ID: "p2", Name: "projB", Path: "/d", Kind: "dir", CreatedAt: 60}
	projs.counts["p1"] = storeTaskCounts{Total: 2, ByStatus: map[string]int{application.StatusActive: 1, application.StatusSuspended: 1}}
	return tb, projs
}

// TestBuildProjectsSnapshot_AssemblesProjectsCountsSummariesAgentStatus 验证共享
// 快照 helper 组装四路数据（projects 列表 + 全量任务摘要分组 + 逐项目 counts +
// agentStatus 内存快照）到与 REST 响应同构的 projectDTO 裸数组（projects-stream
// design D3）。
func TestBuildProjectsSnapshot_AssemblesProjectsCountsSummariesAgentStatus(t *testing.T) {
	tb, projs := newProjectsSnapshotFixture()
	s := newAPITestServer(t, tb)
	s.projs = projs

	out, err := s.buildProjectsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildProjectsSnapshot: %v", err)
	}
	want := []projectDTO{
		{
			ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo", CreatedAt: 50,
			TaskCount: 2, Tasks: map[string]int{application.StatusActive: 1, application.StatusSuspended: 1},
			TaskSummaries: []projectTaskSummaryDTO{
				{ID: "t1", Name: "taskA", Status: application.StatusActive, InitStatus: application.InitStatusNone,
					Branch: "b1", WorktreePath: "/wt1", Notice: json.RawMessage(`{"items":["n"]}`),
					UpdatedAt: 100, AgentStatus: "busy", AttentionCount: 2},
				{ID: "t2", Name: "taskB", Status: application.StatusSuspended, InitStatus: application.InitStatusSucceeded,
					Branch: "b2", WorktreePath: "/wt2", LastError: "boom", UpdatedAt: 200},
			},
		},
		{
			ID: "p2", Name: "projB", Path: "/d", Kind: "dir", CreatedAt: 60,
			TaskCount: 0, Tasks: map[string]int{},
			// 无任务项目摘要为 [] 非 null。
			TaskSummaries: []projectTaskSummaryDTO{},
		},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("snapshot = %+v, want %+v", out, want)
	}
	// 组装读内存快照，MUST NOT 实时探测。
	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus realtime probe called %d times, want 0", calls)
	}
}

// TestBuildProjectsSnapshot_EmptyNonNil 验证无项目时返回非 nil 空切片（JSON `[]` 非 null）。
func TestBuildProjectsSnapshot_EmptyNonNil(t *testing.T) {
	s := newAPITestServer(t, &fakeTaskBackend{})
	s.projs = newFakeProjectStore()

	out, err := s.buildProjectsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildProjectsSnapshot: %v", err)
	}
	if out == nil {
		t.Fatal("snapshot = nil, want non-nil empty slice")
	}
	b, _ := json.Marshal(out)
	if string(b) != "[]" {
		t.Errorf("empty snapshot JSON = %s, want []", b)
	}
}

// TestBuildProjectsSnapshot_AgentStatusDegradationOmitempty 验证 active 任务无内存
// 快照时 agentStatus 空串经 omitempty 省略（降级语义：快照不可用 → 不展示陈旧值）。
func TestBuildProjectsSnapshot_AgentStatusDegradationOmitempty(t *testing.T) {
	tb, projs := newProjectsSnapshotFixture()
	tb.agentStatusSnapshot = nil // 无任何快照
	s := newAPITestServer(t, tb)
	s.projs = projs

	out, err := s.buildProjectsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildProjectsSnapshot: %v", err)
	}
	if got := out[0].TaskSummaries[0].AgentStatus; got != "" {
		t.Errorf("t1 agentStatus = %q, want empty (snapshot unavailable, degraded)", got)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "agentStatus") {
		t.Errorf("degraded summaries must omit agentStatus field: %s", b)
	}
}

// TestBuildProjectsSnapshot_StoreErrorPassthrough 验证三路 store 查询失败均原样返回
// error（REST 500 / SSE 保留上次快照的决策留在调用方）。
func TestBuildProjectsSnapshot_StoreErrorPassthrough(t *testing.T) {
	t.Run("list projects", func(t *testing.T) {
		projs := &listErrStore{fakeProjectStore: newFakeProjectStore()}
		s := newServerWithStore(t, projs)
		out, err := s.buildProjectsSnapshot(context.Background())
		if err == nil {
			t.Fatalf("want error passthrough, got snapshot %+v", out)
		}
		if out != nil {
			t.Errorf("snapshot = %+v, want nil on error", out)
		}
	})
	t.Run("list summaries", func(t *testing.T) {
		projs := newFakeProjectStore()
		projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", Kind: "repo"}
		s := newAPITestServer(t, &summaryErrBackend{fakeTaskBackend: &fakeTaskBackend{}})
		s.projs = projs
		out, err := s.buildProjectsSnapshot(context.Background())
		if err == nil {
			t.Fatalf("want error passthrough, got snapshot %+v", out)
		}
		if out != nil {
			t.Errorf("snapshot = %+v, want nil on error", out)
		}
	})
	t.Run("count tasks", func(t *testing.T) {
		projs := &countErrStore{fakeProjectStore: newFakeProjectStore()}
		projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", Kind: "repo"}
		s := newServerWithStore(t, projs)
		out, err := s.buildProjectsSnapshot(context.Background())
		if err == nil {
			t.Fatalf("want error passthrough, got snapshot %+v", out)
		}
		if out != nil {
			t.Errorf("snapshot = %+v, want nil on error", out)
		}
	})
}

// TestBuildProjectsSnapshot_RESTParity 验证 REST /projects handler 经共享 helper 后
// 响应体与 helper 直 assemble 结果字节级一致（含尾部换行；projects-stream design D3
// 「帧与 REST 响应体完全同构」的 REST 侧锚点）。
func TestBuildProjectsSnapshot_RESTParity(t *testing.T) {
	tb, projs := newProjectsSnapshotFixture()
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.buildProjectsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildProjectsSnapshot: %v", err)
	}
	want, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	// json.NewEncoder(w).Encode 追加单个换行。
	if string(raw) != string(want)+"\n" {
		t.Errorf("REST body != snapshot marshal:\n got: %s\nwant: %s\\n", raw, want)
	}
}

// listErrStore 包装 fakeProjectStore，ListProjects 恒失败。
type listErrStore struct {
	*fakeProjectStore
}

func (l *listErrStore) ListProjects(ctx context.Context) ([]storeProjectRow, error) {
	return nil, errors.New("list boom")
}

// summaryErrBackend 包装 fakeTaskBackend，ListProjectTaskSummaries 恒失败。
type summaryErrBackend struct {
	*fakeTaskBackend
}

func (b *summaryErrBackend) ListProjectTaskSummaries(ctx context.Context) ([]application.ProjectTaskSummary, error) {
	return nil, errors.New("summaries boom")
}
