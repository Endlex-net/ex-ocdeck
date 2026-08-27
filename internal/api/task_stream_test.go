package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/infrastructure/opencode"
)

type taskStreamBackend struct {
	*fakeTaskBackend
	mu sync.Mutex

	row          application.TaskRow
	sessions     []application.SessionRow
	getErr       error
	sessionsErr  error
	failGetNext  int
	failSessNext int
	gets         int
	sessLists    int

	agentStatusSnapshot string
	agentStatusCalls    []string
	attention           application.Attention
}

func newTaskStreamBackend(row application.TaskRow) *taskStreamBackend {
	return &taskStreamBackend{fakeTaskBackend: &fakeTaskBackend{}, row: row}
}

func (b *taskStreamBackend) Get(ctx context.Context, taskID string) (application.TaskRow, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gets++
	if b.failGetNext > 0 {
		b.failGetNext--
		return application.TaskRow{}, errStoreFailure
	}
	if b.getErr != nil {
		return application.TaskRow{}, b.getErr
	}
	if b.row.ID == "" || b.row.ID != taskID {
		return application.TaskRow{}, &application.OpError{
			Code: application.CodeNotFound,
			Err:  application.ErrTaskNotFound,
		}
	}
	return b.row, nil
}

func (b *taskStreamBackend) ListTaskSessions(ctx context.Context, taskID string) ([]application.SessionRow, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessLists++
	if b.failSessNext > 0 {
		b.failSessNext--
		return nil, errStoreFailure
	}
	if b.sessionsErr != nil {
		return nil, b.sessionsErr
	}
	return b.sessions, nil
}

func (b *taskStreamBackend) AgentStatus(ctx context.Context, taskID string) string {
	b.mu.Lock()
	b.agentStatusCalls = append(b.agentStatusCalls, taskID)
	b.mu.Unlock()
	return "busy"
}

func (b *taskStreamBackend) AgentStatusSnapshot(taskID string) string {
	return b.agentStatusSnapshot
}

func (b *taskStreamBackend) Attention(taskID string) (application.Attention, bool) {
	return b.attention, true
}

func (b *taskStreamBackend) agentStatusCallCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.agentStatusCalls)
}

func (b *taskStreamBackend) setRow(row application.TaskRow) {
	b.mu.Lock()
	b.row = row
	b.mu.Unlock()
}

func (b *taskStreamBackend) setGetErr(err error) {
	b.mu.Lock()
	b.getErr = err
	b.mu.Unlock()
}

func (b *taskStreamBackend) setFailSessNext(n int) {
	b.mu.Lock()
	b.failSessNext = n
	b.mu.Unlock()
}

func (b *taskStreamBackend) setFailGetNext(n int) {
	b.mu.Lock()
	b.failGetNext = n
	b.mu.Unlock()
}

func fixtureTaskRow(id string) application.TaskRow {
	return application.TaskRow{
		ID:           id,
		ProjectID:    "p1",
		Name:         "taskA",
		Branch:       "main",
		Status:       application.StatusActive,
		WorktreePath: "/wtA",
		CreatedAt:    100,
		UpdatedAt:    200,
		InitStatus:   "none",
		LastPort:     sql.NullInt64{Int64: 50001, Valid: true},
	}
}

func newTaskStreamTestServer(t *testing.T, tb TaskBackend, sub *fakeStreamSubscriber, coalesce, heartbeat time.Duration) *Server {
	t.Helper()
	projs := newFakeProjectStore()
	projs.projects["p1"] = storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo"}
	s := newAPITestServer(t, tb)
	s.projs = projs
	if sub != nil {
		s.SetEventSubscriber(sub)
	}
	s.sseCoalesce = coalesce
	s.sseHeartbeat = heartbeat
	s.RebuildRoutes()
	return s
}

func openTaskStream(t *testing.T, url, taskID string) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(authedReq("GET", url+"/api/v1/tasks/"+taskID+"/stream", ""))
	if err != nil {
		t.Fatalf("open task stream: %v", err)
	}
	return resp
}

func decodeTaskDTO(t *testing.T, data string) taskRowDTO {
	t.Helper()
	var dto taskRowDTO
	if err := json.Unmarshal([]byte(data), &dto); err != nil {
		t.Fatalf("decode task dto: %v body=%s", err, data)
	}
	return dto
}

func TestTaskStream_FirstFrameSnapshot(t *testing.T) {
	row := fixtureTaskRow("t1")
	tb := newTaskStreamBackend(row)
	tb.sessions = []application.SessionRow{{TaskID: "t1", SessionID: "s1", LastSeenAt: 50}}
	tb.agentStatusSnapshot = "idle"
	tb.attention = application.Attention{
		Permissions: []application.PendingPermission{{
			PermissionRequest: opencode.PermissionRequest{ID: "perm1", Permission: "bash", Patterns: []string{"git *"}},
			Since:             100,
		}},
		Questions: []application.PendingQuestion{},
	}
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	frames := startSSEFrameReader(resp.Body)
	snap := nextFrame(t, frames, "snapshot")
	if snap.event != "snapshot" {
		t.Fatalf("first frame event = %q, want snapshot", snap.event)
	}
	if strings.HasPrefix(snap.data, "[") {
		t.Fatalf("snapshot must be a single object, got array: %s", snap.data)
	}
	dto := decodeTaskDTO(t, snap.data)
	if dto.ID != "t1" || dto.Name != "taskA" || dto.ProjectKind != "repo" {
		t.Errorf("snapshot dto = %+v", dto)
	}
	if dto.AgentStatus != "idle" {
		t.Errorf("agentStatus = %q, want idle (snapshot)", dto.AgentStatus)
	}
	if len(dto.Sessions) != 1 || dto.Sessions[0].SessionID != "s1" {
		t.Errorf("sessions = %+v", dto.Sessions)
	}
	if n := sub.liveSubs(); n != 4 {
		t.Errorf("live subs = %d, want 4", n)
	}
}

func TestTaskStream_RelatedEventProducesUpdateIncludingInit(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	tb.agentStatusSnapshot = "idle"
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	updated := fixtureTaskRow("t1")
	updated.Name = "taskA-v2"
	tb.setRow(updated)
	sub.publish(ocdeckevent.NewTaskActivityChanged("t1"))
	upd := nextFrame(t, frames, "update after activity_changed")
	if upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	if dto := decodeTaskDTO(t, upd.data); dto.Name != "taskA-v2" {
		t.Errorf("update dto = %+v, want name taskA-v2", dto)
	}
}

func TestTaskStream_OtherTaskEventDoesNotUpdate(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 30*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	sub.publish(ocdeckevent.NewTaskActivityChanged("t2"))
	sub.publish(ocdeckevent.NewSessionTouched("s9", "t2"))
	sub.publish(ocdeckevent.NewTaskStatusChanged("t9", application.StatusActive, application.StatusSuspended))
	assertNoFrame(t, frames, 200*time.Millisecond, "update after other-task events")
}

func TestTaskStream_CoalesceWindowSingleUpdate(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 120*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	sub.publish(ocdeckevent.NewTaskActivityChanged("t1"))
	sub.publish(ocdeckevent.NewSessionTouched("s1", "t1"))
	sub.publish(ocdeckevent.NewServeRuntimeAttentionChanged("iv1", "t1"))
	upd := nextFrame(t, frames, "coalesced update")
	if upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	assertNoFrame(t, frames, 350*time.Millisecond, "second update after coalesce window")
}

func TestTaskStream_InitialMissingJSON404(t *testing.T) {
	tb := newTaskStreamBackend(application.TaskRow{})
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "missing")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if got := resp.Header.Get("Cache-Control"); got == "no-cache" {
		t.Error("must not write SSE headers on initial gone")
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeNotFound || eb.Error.Message != "task not found" {
		t.Errorf("envelope = %+v, want not_found/task not found", eb.Error)
	}
	waitFor(t, 2*time.Second, "subscriptions closed", func() bool { return sub.liveSubs() == 0 })
}

func TestTaskStream_DeletedDuringStreamCloses(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	tb.setGetErr(&application.OpError{Code: application.CodeNotFound, Err: application.ErrTaskNotFound})
	sub.publish(ocdeckevent.NewTaskDeleted("t1", application.StatusActive))
	select {
	case _, ok := <-frames:
		if ok {
			// may observe close after last frame; extra update is not required
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after task deleted")
	}
	waitFor(t, 2*time.Second, "subscriptions closed after delete", func() bool {
		return sub.liveSubs() == 0
	})
}

func TestTaskStream_GetFailureKeepsDirtyRetries(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 50*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	tb.setFailGetNext(1)
	sub.publish(ocdeckevent.NewTaskActivityChanged("t1"))
	upd := nextFrame(t, frames, "update after non-not-found Get failure retry")
	if upd.event != "update" {
		t.Fatalf("event = %q, want update (connection kept, retry succeeded)", upd.event)
	}
	if n := sub.liveSubs(); n != 4 {
		t.Errorf("live subs = %d, want 4 (non-not-found Get failure must keep connection)", n)
	}
}

func TestTaskStream_ListSessionsFailureKeepsDirty(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 50*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	tb.setFailSessNext(1)
	sub.publish(ocdeckevent.NewTaskActivityChanged("t1"))
	upd := nextFrame(t, frames, "update after sessions failure retry")
	if upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
}

func TestTaskStream_RESTListSessionsFailureStillDegrades(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	tb.sessionsErr = errStoreFailure
	s := newTaskStreamTestServer(t, tb, &fakeStreamSubscriber{}, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("REST status = %d, want 200 (sessions failure ignored)", resp.StatusCode)
	}
}

func TestTaskStream_PushPathNoRealtimeProbe(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	tb.agentStatusSnapshot = "idle"
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")
	sub.publish(ocdeckevent.NewTaskActivityChanged("t1"))
	if upd := nextFrame(t, frames, "update"); upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	if n := tb.agentStatusCallCount(); n != 0 {
		t.Errorf("AgentStatus realtime probe called %d times, want 0", n)
	}
}

func TestTaskStream_Unauthorized401(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	s := newTaskStreamTestServer(t, tb, &fakeStreamSubscriber{}, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/tasks/t1/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestTaskStream_SubscriberNilReturns500(t *testing.T) {
	tb := newTaskStreamBackend(fixtureTaskRow("t1"))
	s := newTaskStreamTestServer(t, tb, nil, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openTaskStream(t, ts.URL, "t1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeInternal || eb.Error.Message != "event stream not configured" {
		t.Errorf("envelope = %+v", eb.Error)
	}
}

func TestTaskStream_RESTAndStreamIsomorphicExceptAgentStatus(t *testing.T) {
	row := fixtureTaskRow("t1")
	tb := newTaskStreamBackend(row)
	tb.sessions = []application.SessionRow{{TaskID: "t1", SessionID: "s1", LastSeenAt: 50}}
	tb.agentStatusSnapshot = "idle"
	tb.attention = application.Attention{
		Permissions: []application.PendingPermission{},
		Questions:   []application.PendingQuestion{},
	}
	s := newTaskStreamTestServer(t, tb, &fakeStreamSubscriber{}, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	restResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer restResp.Body.Close()
	var rest taskRowDTO
	if err := json.NewDecoder(restResp.Body).Decode(&rest); err != nil {
		t.Fatal(err)
	}

	streamResp := openTaskStream(t, ts.URL, "t1")
	defer streamResp.Body.Close()
	frames := startSSEFrameReader(streamResp.Body)
	snap := nextFrame(t, frames, "snapshot")
	stream := decodeTaskDTO(t, snap.data)

	if rest.AgentStatus == stream.AgentStatus {
		t.Fatalf("expected agentStatus to differ: rest=%q stream=%q", rest.AgentStatus, stream.AgentStatus)
	}
	rest.AgentStatus = ""
	stream.AgentStatus = ""
	if !reflect.DeepEqual(rest, stream) {
		t.Errorf("REST/stream DTO differ beyond agentStatus:\nREST=%+v\nSSE=%+v", rest, stream)
	}
}

func TestGetTask_RESTNotFoundForwardsHandlerMessage(t *testing.T) {
	tb := newTaskStreamBackend(application.TaskRow{})
	s := newTaskStreamTestServer(t, tb, &fakeStreamSubscriber{}, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/missing", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var eb errorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeNotFound {
		t.Errorf("code = %s, want not_found", eb.Error.Code)
	}
	if eb.Error.Message != "task not found" {
		t.Errorf("message = %q, want handler original (task not found)", eb.Error.Message)
	}
	if strings.Contains(string(body), "no route for") {
		t.Errorf("handler envelope rewritten to mux copy: %s", body)
	}
}
