package task

import (
	"context"
	"testing"

	"ocdeck/internal/infrastructure/opencode"
)

// TestAgentStatus_NonActiveReturnsEmpty 验证非 active 任务 agentStatus 为空串。
func TestAgentStatus_NonActiveReturnsEmpty(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if got := m.AgentStatus(context.Background(), "t1"); got != "" {
		t.Errorf("non-active agentStatus = %q, want empty", got)
	}
}

// TestAgentStatus_QueryFailedDegradesEmpty 验证 active 但 SessionStatus 查询失败时降级为空串。
func TestAgentStatus_QueryFailedDegradesEmpty(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
	}
	// 预置一个 session 行（ListTaskSessions 首项为最近 session）。
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "S1", LastSeenAt: 100}}

	oc := newMockOC(true)
	oc.sessionStatusErr = errAgentStatusProbe
	factory := func(port int, password string, opts opencode.Options) OCClient { return oc }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)

	if got := m.AgentStatus(context.Background(), "t1"); got != "" {
		t.Errorf("degraded agentStatus = %q, want empty", got)
	}
}

// TestAgentStatus_HappyPath 验证 active 且 SessionStatus 命中最近 session 返回其 type。
func TestAgentStatus_HappyPath(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
	}
	// 预置 sessions：S1 最近（last_seen 最大），S2 较早。期望取 S1 的状态。
	store.sessions["t1"] = []SessionRow{
		{TaskID: "t1", SessionID: "S1", LastSeenAt: 300},
		{TaskID: "t1", SessionID: "S2", LastSeenAt: 100},
	}

	oc := newMockOC(true)
	oc.sessionStatuses = map[string]opencode.SessionStatus{
		"S1": {Type: opencode.StatusBusy},
		"S2": {Type: opencode.StatusIdle},
	}
	factory := func(port int, password string, opts opencode.Options) OCClient { return oc }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)

	if got := m.AgentStatus(context.Background(), "t1"); got != "busy" {
		t.Errorf("agentStatus = %q, want busy (from S1)", got)
	}
}

// TestAgentStatus_AggregatesSubagentBusy 防回归（m0625 实测缺陷）：background
// subagent 是独立子 session——最近 session（主 session）idle 而较早的子 session busy
// 时，聚合结果 MUST 为 busy；只看"最近一个 session"会漏报运行状态。
func TestAgentStatus_AggregatesSubagentBusy(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
	}
	store.sessions["t1"] = []SessionRow{
		{TaskID: "t1", SessionID: "S-main", LastSeenAt: 300}, // 主 session 最近但 idle
		{TaskID: "t1", SessionID: "S-sub", LastSeenAt: 100},  // 子 session（subagent）busy
	}
	oc := newMockOC(true)
	oc.sessionStatuses = map[string]opencode.SessionStatus{
		"S-main": {Type: opencode.StatusIdle},
		"S-sub":  {Type: opencode.StatusBusy},
	}
	factory := func(port int, password string, opts opencode.Options) OCClient { return oc }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)

	if got := m.AgentStatus(context.Background(), "t1"); got != "busy" {
		t.Errorf("agentStatus = %q, want busy（任一 session busy 即 busy）", got)
	}
}

// TestAgentStatus_AggregatesRetryOverIdle 验证 retry 优先于 idle；未记录在
// status map 的 session 按 opencode 契约默认 idle，不影响聚合。
func TestAgentStatus_AggregatesRetryOverIdle(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
	}
	store.sessions["t1"] = []SessionRow{
		{TaskID: "t1", SessionID: "S1", LastSeenAt: 300},
		{TaskID: "t1", SessionID: "S2", LastSeenAt: 200},
		{TaskID: "t1", SessionID: "S3-unknown", LastSeenAt: 100}, // 不在 status map
	}
	oc := newMockOC(true)
	oc.sessionStatuses = map[string]opencode.SessionStatus{
		"S1": {Type: opencode.StatusIdle},
		"S2": {Type: opencode.StatusRetry},
	}
	factory := func(port int, password string, opts opencode.Options) OCClient { return oc }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)

	if got := m.AgentStatus(context.Background(), "t1"); got != "retry" {
		t.Errorf("agentStatus = %q, want retry（busy 缺席时 retry 优先）", got)
	}
}

// TestListAllActiveTaskIDs 验证仅返回 active 任务 ID。
func TestListAllActiveTaskIDs(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t-active", "p1")
	seedSuspendedTask(store, "t-suspended", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	ids, err := m.ListAllActiveTaskIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "t-active" {
		t.Errorf("active ids = %+v, want [t-active]", ids)
	}
}

// errAgentStatusProbe 测试用 sentinel error（模拟 /session/status 查询失败）。
var errAgentStatusProbe = &agentStatusProbeErr{}

type agentStatusProbeErr struct{}

func (e *agentStatusProbeErr) Error() string { return "agent status probe failed" }