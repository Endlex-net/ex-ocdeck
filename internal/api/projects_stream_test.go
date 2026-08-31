// projects_stream_test.go GET /api/v1/projects/stream 端点测试（projects-stream
// design D3/D4；tasks §4.1/§4.2 的 projects 场景绑定面）。
//
// SSE 循环纪律（合并窗口、溢出重推、心跳、写失败退出、进程 ctx 取消）已由共享核心
// read_model_stream.go 承载并被 sessions_stream_test.go 全套锁定，本文件不重复验证
// 循环行为，只断言 projects 场景绑定：字面路由不被 {id} 通配吞掉、首帧快照与 REST
// /projects 同构、绑定的消费过滤表为 eventDirtiesProjectsTaskTree（全任务树，含
// active-only 过滤拒绝的差异行）、401/初始组装 500/断连退订归零。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
)

// newProjectsStreamTestServer 构造注入 fake 订阅器、project store 与短间隔的测试
// Server（newStreamTestServer 的 projects 场景变体：SetEventSubscriber 须先于
// RebuildRoutes，与生产 wiring 顺序一致）。
func newProjectsStreamTestServer(t *testing.T, tb TaskBackend, projs ProjectStore, sub *fakeStreamSubscriber, coalesce, heartbeat time.Duration) *Server {
	t.Helper()
	s := newAPITestServer(t, tb)
	s.projs = projs
	s.SetEventSubscriber(sub)
	s.sseCoalesce = coalesce
	s.sseHeartbeat = heartbeat
	s.RebuildRoutes()
	return s
}

// openProjectsStream 发起已认证的 projects SSE 请求并返回响应。
func openProjectsStream(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(authedReq("GET", url+"/api/v1/projects/stream", ""))
	if err != nil {
		t.Fatalf("open projects stream: %v", err)
	}
	return resp
}

// TestProjectsStream_FirstFrameBareArraySnapshot 首帧为裸数组 snapshot，SSE headers
// 正确；帧 data 与 REST /projects 响应逐字段同构（agentStatus omitempty、
// attention_count 透出、无任务项目摘要为 [] 非 null）。
func TestProjectsStream_FirstFrameBareArraySnapshot(t *testing.T) {
	tb, projs := newProjectsSnapshotFixture()
	sub := &fakeStreamSubscriber{}
	// 心跳 5s：断言首帧到达不依赖任何心跳/窗口触发。
	s := newProjectsStreamTestServer(t, tb, projs, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openProjectsStream(t, ts.URL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache, no-transform" {
		t.Errorf("cache-control = %q, want no-cache, no-transform", cc)
	}
	if xa := resp.Header.Get("X-Accel-Buffering"); xa != "no" {
		t.Errorf("x-accel-buffering = %q, want no", xa)
	}
	if conn := resp.Header.Get("Connection"); conn != "keep-alive" {
		t.Errorf("connection = %q, want keep-alive", conn)
	}

	frames := startSSEFrameReader(resp.Body)
	snap := nextFrame(t, frames, "snapshot")
	if snap.event != "snapshot" {
		t.Fatalf("first frame event = %q, want snapshot", snap.event)
	}
	if !strings.HasPrefix(snap.data, "[") {
		t.Fatalf("snapshot data not bare array: %s", snap.data)
	}
	// 摘要字段锚点：t1（active）agentStatus 读快照出现且仅一次（t2 suspended 省略）、
	// attention_count 透出；p2 无任务项目摘要为 [] 非 null。
	if !strings.Contains(snap.data, `"agentStatus":"busy"`) || strings.Count(snap.data, "agentStatus") != 1 {
		t.Errorf("snapshot agentStatus omitempty wrong: %s", snap.data)
	}
	if !strings.Contains(snap.data, `"attention_count":2`) {
		t.Errorf("snapshot attention_count missing: %s", snap.data)
	}
	if !strings.Contains(snap.data, `"tasks":[]`) {
		t.Errorf("task-less project summaries should be [] not null: %s", snap.data)
	}

	// 与 REST /projects 响应逐字段同构。
	restResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer restResp.Body.Close()
	var rest, sse []projectDTO
	if err := json.NewDecoder(restResp.Body).Decode(&rest); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(snap.data), &sse); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rest, sse) {
		t.Errorf("SSE snapshot %+v != REST %+v", sse, rest)
	}
	if n := sub.liveSubs(); n != 4 {
		t.Errorf("live subs = %d, want 4 while streaming", n)
	}
}

// TestProjectsStream_LiteralRouteNotSwallowedByIDWildcard 路由锁定（design D3 正反
// 两向）：Go 1.22+ ServeMux 字面段优先，`/projects/stream` 走 SSE 流、真实项目 ID
// `/projects/p1` 仍走详情 handler（JSON），互不串扰。
func TestProjectsStream_LiteralRouteNotSwallowedByIDWildcard(t *testing.T) {
	tb, projs := newProjectsSnapshotFixture()
	sub := &fakeStreamSubscriber{}
	s := newProjectsStreamTestServer(t, tb, projs, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 正向：字面路径走流（若被 {id} 通配吞掉将以 JSON 详情响应）。
	resp := openProjectsStream(t, ts.URL)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream route content-type = %q, want text/event-stream (not swallowed by {id})", ct)
	}
	frames := startSSEFrameReader(resp.Body)
	if snap := nextFrame(t, frames, "snapshot"); snap.event != "snapshot" {
		t.Fatalf("first frame event = %q, want snapshot", snap.event)
	}

	// 反向：真实项目 ID 仍走详情（application/json，projectDTO 单对象）。
	detailResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/projects/p1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer detailResp.Body.Close()
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", detailResp.StatusCode)
	}
	if ct := detailResp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("detail route content-type = %q, want application/json", ct)
	}
	var dto projectDTO
	if err := json.NewDecoder(detailResp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.ID != "p1" {
		t.Errorf("detail project ID = %q, want p1", dto.ID)
	}
}

// TestProjectsStream_TaskCreatedProducesUpdate 事件驱动 update 的差异代表行：
// task.created 只影响 projects 树（active-only 过滤拒绝、全任务树表命中），发布后
// 下一合并窗口内送达 update（design D4 第一行）。
func TestProjectsStream_TaskCreatedProducesUpdate(t *testing.T) {
	tb, projs := newProjectsSnapshotFixture()
	sub := &fakeStreamSubscriber{}
	s := newProjectsStreamTestServer(t, tb, projs, sub, 50*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openProjectsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	sub.publish(ocdeckevent.NewTaskCreated("t3"))
	upd := nextFrame(t, frames, "update after task.created")
	if upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	if !strings.HasPrefix(upd.data, "[") {
		t.Errorf("update data not bare array: %s", upd.data)
	}
}

// TestProjectsStream_BoundFilterIsProjectsTaskTree 过滤表绑定断言（D4 全任务树
// 场景无非命中行可测——一切事件标脏——故断言绑定的是 projects 过滤表而非 active
// 表）：D4 差异行（active-only 过滤全部拒绝、全任务树表全部命中）逐个发布均产生
// update 帧；若误绑 eventDirtiesActiveSessions，这些事件不会产生任何帧。
func TestProjectsStream_BoundFilterIsProjectsTaskTree(t *testing.T) {
	tb, projs := newProjectsSnapshotFixture()
	sub := &fakeStreamSubscriber{}
	s := newProjectsStreamTestServer(t, tb, projs, sub, 30*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openProjectsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	// D4 差异行（task.created 已由上一测试覆盖）：非 active 迁移（含往返）与非 active
	// 删除——全部被 active-only 过滤拒绝。
	differential := []ocdeckevent.Event{
		ocdeckevent.NewTaskStatusChanged("t3", application.StatusSuspended, application.StatusArchived),
		ocdeckevent.NewTaskStatusChanged("t3", application.StatusArchived, application.StatusSuspended),
		ocdeckevent.NewTaskDeleted("t4", application.StatusSuspended),
	}
	for _, ev := range differential {
		if eventDirtiesActiveSessions(ev) {
			t.Fatalf("precondition: %s should be rejected by the active-only filter", ev.Type)
		}
		if !eventDirtiesProjectsTaskTree(ev) {
			t.Fatalf("precondition: %s should be accepted by the projects task-tree filter", ev.Type)
		}
		sub.publish(ev)
		upd := nextFrame(t, frames, "update after "+string(ev.Type))
		if upd.event != "update" {
			t.Fatalf("event after %s = %q, want update", ev.Type, upd.event)
		}
	}
}

// TestProjectsStream_Unauthorized401 无 token：中间件返回 JSON 401，不进入 SSE
// （无 text/event-stream）。
func TestProjectsStream_Unauthorized401(t *testing.T) {
	tb, projs := newProjectsSnapshotFixture()
	sub := &fakeStreamSubscriber{}
	s := newProjectsStreamTestServer(t, tb, projs, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/projects/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeUnauthorized {
		t.Errorf("error code = %q, want %q", eb.Error.Code, CodeUnauthorized)
	}
}

// TestProjectsStream_InitialAssemblyFailureReturns500 初始组装失败（store 错误）：
// 写 SSE headers 前退订全部订阅并返回 500 JSON 错误信封（无 text/event-stream、
// 无悬挂连接）。
func TestProjectsStream_InitialAssemblyFailureReturns500(t *testing.T) {
	tb, _ := newProjectsSnapshotFixture()
	projs := &listErrStore{fakeProjectStore: newFakeProjectStore()}
	sub := &fakeStreamSubscriber{}
	s := newProjectsStreamTestServer(t, tb, projs, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openProjectsStream(t, ts.URL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (no SSE headers)", ct)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeInternal {
		t.Errorf("error code = %q, want %q", eb.Error.Code, CodeInternal)
	}
	// 四路订阅全部关闭。
	waitFor(t, 2*time.Second, "subscriptions closed after 500", func() bool {
		return sub.liveSubs() == 0
	})
}

// TestProjectsStream_ClientDisconnectClosesSubscriptions 客户端断开（请求 ctx
// 取消）：handler 退出、四路订阅归零。
func TestProjectsStream_ClientDisconnectClosesSubscriptions(t *testing.T) {
	tb, projs := newProjectsSnapshotFixture()
	sub := &fakeStreamSubscriber{}
	s := newProjectsStreamTestServer(t, tb, projs, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/projects/stream", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			respCh <- resp
		}
	}()
	var resp *http.Response
	select {
	case resp = <-respCh:
	case <-time.After(2 * time.Second):
		t.Fatal("stream request did not start")
	}
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	cancel()
	waitFor(t, 2*time.Second, "subscriptions closed after client disconnect", func() bool {
		return sub.liveSubs() == 0
	})
}
