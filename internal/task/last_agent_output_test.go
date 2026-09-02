// last_agent_output_test.go LastAgentOutput 端口实现（task-notifications
// design D9）：锚会话优先、无锚取最近更新 owned session、最后一条 assistant
// 消息文本 part 拼接、截 2000 字符、拉取失败/无 assistant → 不可得。
package task

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/infrastructure/opencode"
)

// lastOutputOC ListMessages 记录型 fake（OCClient 可选能力注入，接口本身不变）。
type lastOutputOC struct {
	OCClient
	mu     sync.Mutex
	calls  int
	ids    []string
	limits []int
	msgs   []opencode.Message
	err    error
}

func (c *lastOutputOC) ListMessages(_ context.Context, _, sessionID string, limit int) ([]opencode.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.ids = append(c.ids, sessionID)
	c.limits = append(c.limits, limit)
	if c.err != nil {
		return nil, c.err
	}
	return c.msgs, nil
}

func (c *lastOutputOC) firstCall() (int, string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == 0 {
		return 0, "", 0
	}
	return c.calls, c.ids[0], c.limits[0]
}

// lastOutputFixture active 任务 + serve env + ListMessages fake 的 Manager
// （sessions 按 last_seen 降序语义预置：ListTaskSessions 返回最近更新在前）。
func lastOutputFixture(t *testing.T, oc *lastOutputOC, anchor string, sessions ...SessionRow) (*Manager, *lastOutputOC) {
	t.Helper()
	store := newMockStore()
	row := TaskRow{ID: "t1", Name: "构建服务", Status: StatusActive, WorktreePath: "/wt"}
	if anchor != "" {
		row.AnchorSessionID = sql.NullString{String: anchor, Valid: true}
	}
	store.tasks["t1"] = row
	for _, s := range sessions {
		_ = store.UpsertTaskSession(context.Background(), s)
	}
	proc := newMockProc()
	p18SetServeEnv(proc, "t1")
	if oc == nil {
		oc = &lastOutputOC{}
	}
	oc.OCClient = newMockOC(true)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	return m, oc
}

func TestLastAgentOutput_AnchorPreferred(t *testing.T) {
	m, oc := lastOutputFixture(t, nil, "ses-anchor",
		SessionRow{TaskID: "t1", SessionID: "ses-newer", LastSeenAt: 200},
		SessionRow{TaskID: "t1", SessionID: "ses-older", LastSeenAt: 100},
	)
	oc.msgs = []opencode.Message{
		{ID: "m1", Role: "user", Parts: []opencode.MessagePart{{Type: "text", Text: "继续"}}},
		{ID: "m2", Role: "assistant", Parts: []opencode.MessagePart{
			{Type: "text", Text: "已完成登录"},
			{Type: "tool"},
		}},
	}

	got, ok := m.LastAgentOutput(context.Background(), "t1")
	if !ok || got != "已完成登录" {
		t.Fatalf("LastAgentOutput = (%q, %v), want (已完成登录, true)", got, ok)
	}
	calls, id, limit := oc.firstCall()
	if calls != 1 {
		t.Fatalf("ListMessages calls = %d, want 1", calls)
	}
	if id != "ses-anchor" {
		t.Fatalf("anchor session must be preferred, got %q", id)
	}
	if limit != 10 {
		t.Fatalf("limit = %d, want 10", limit)
	}
}

func TestLastAgentOutput_FallsBackToMostRecentOwned(t *testing.T) {
	// mockStore 不排序：按生产语义（last_seen DESC）预置顺序，最近更新在前。
	m, oc := lastOutputFixture(t, nil, "",
		SessionRow{TaskID: "t1", SessionID: "ses-newer", LastSeenAt: 200},
		SessionRow{TaskID: "t1", SessionID: "ses-older", LastSeenAt: 100},
	)
	oc.msgs = []opencode.Message{
		{ID: "m1", Role: "assistant", Parts: []opencode.MessagePart{{Type: "text", Text: "输出"}}},
	}

	got, ok := m.LastAgentOutput(context.Background(), "t1")
	if !ok || got != "输出" {
		t.Fatalf("LastAgentOutput = (%q, %v)", got, ok)
	}
	_, id, _ := oc.firstCall()
	if id != "ses-newer" {
		t.Fatalf("most recently seen session must be used, got %q", id)
	}
}

func TestLastAgentOutput_LastAssistantJoinedText(t *testing.T) {
	m, oc := lastOutputFixture(t, nil, "ses-anchor")
	oc.msgs = []opencode.Message{
		{ID: "m1", Role: "assistant", Parts: []opencode.MessagePart{{Type: "text", Text: "早期"}}},
		{ID: "m2", Role: "user", Parts: []opencode.MessagePart{{Type: "text", Text: "问"}}},
		{ID: "m3", Role: "assistant", Parts: []opencode.MessagePart{
			{Type: "text", Text: "第一段"},
			{Type: "text", Text: "第二段"},
		}},
	}

	got, ok := m.LastAgentOutput(context.Background(), "t1")
	if !ok || got != "第一段\n第二段" {
		t.Fatalf("last assistant text parts must join with \\n, got (%q, %v)", got, ok)
	}
}

func TestLastAgentOutput_TruncatedTo2000Runes(t *testing.T) {
	m, oc := lastOutputFixture(t, nil, "ses-anchor")
	oc.msgs = []opencode.Message{
		{ID: "m1", Role: "assistant", Parts: []opencode.MessagePart{{Type: "text", Text: strings.Repeat("出", 2500)}}},
	}

	got, ok := m.LastAgentOutput(context.Background(), "t1")
	if !ok || len([]rune(got)) != 2000 {
		t.Fatalf("output runes = %d ok=%v, want 2000/true", len([]rune(got)), ok)
	}
}

func TestLastAgentOutput_UnavailableBranches(t *testing.T) {
	cases := []struct {
		name     string
		msgs     []opencode.Message
		err      error
		anchor   string
		sessions []SessionRow
	}{
		{name: "no sessions and no anchor"},
		{name: "no assistant messages", anchor: "s", msgs: []opencode.Message{
			{ID: "m1", Role: "user", Parts: []opencode.MessagePart{{Type: "text", Text: "问"}}},
		}},
		{name: "last assistant has no text parts", anchor: "s", msgs: []opencode.Message{
			{ID: "m1", Role: "assistant", Parts: []opencode.MessagePart{{Type: "tool"}}},
		}},
		{name: "list error fail-closed", anchor: "s", err: errors.New("boom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oc := &lastOutputOC{msgs: tc.msgs, err: tc.err}
			m, _ := lastOutputFixture(t, oc, tc.anchor, tc.sessions...)
			got, ok := m.LastAgentOutput(context.Background(), "t1")
			if ok || got != "" {
				t.Fatalf("LastAgentOutput = (%q, %v), want zero/false", got, ok)
			}
		})
	}
}
