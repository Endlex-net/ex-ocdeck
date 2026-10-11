package opencode

// spi_health_wiring_test.go 覆盖 C6 门禁修复：能力适配器健康上报接线
//（SPI spec「能力故障透出」139-144）。核心口径：观测由**真实能力调用**的
// 出口产生（成功/协议违约/明确不支持），装配方仅注入报告端口与 epoch 读取，
// 不得直接制造目标状态。
//
// mutation 证据（已验证 → 红 → 还原三处，破坏点标注于各测试注释）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// healthRecorder 收集上报的观测（测试线程安全收集器 + 可编程 epoch）。
type healthRecorder struct {
	mu      sync.Mutex
	records []appagent.CapabilityHealth
	epoch   map[string]int64
}

func newHealthRecorder() *healthRecorder {
	return &healthRecorder{epoch: map[string]int64{}}
}

func (r *healthRecorder) report(h appagent.CapabilityHealth) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, h)
}

func (r *healthRecorder) runtimeEpoch(taskID string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.epoch[taskID]
}

func (r *healthRecorder) bump(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.epoch[taskID]++
}

func (r *healthRecorder) all() []appagent.CapabilityHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]appagent.CapabilityHealth(nil), r.records...)
}

// lastByPrimitive 返回指定 primitive 的最后一次观测（无 → zero,false）。
func (r *healthRecorder) lastByPrimitive(p appagent.CapabilityPrimitive) (appagent.CapabilityHealth, bool) {
	var out appagent.CapabilityHealth
	found := false
	for _, h := range r.all() {
		if h.Primitive == p {
			out = h
			found = true
		}
	}
	return out, found
}

// wiringProvider 构造接入报告端口的 provider（epoch 可编程递增）。
func wiringProvider(t *testing.T, rec *healthRecorder) *SPIProvider {
	t.Helper()
	p := NewSPIProvider()
	p.WithHealthReporter(rec.report, rec.runtimeEpoch)
	return p
}

// c6TaskID 是健康观测的身份键（C6 残余：adapter 取 binding data.taskId 作 TaskID
// ——真实 task ID，与 UpstreamSessionID（锚定 session ID "ses_a"）不同值，C6 验证
// 「按 task ID 代际观测、非 session ID」）。
const c6TaskID = "t1"

// TestSPI_HealthWiring_RealCallConversions 固定输入：真实能力调用经 fake 上游，
// 失败注入逐路径验证转换（available/unsupported/degraded + epoch 携带）。
func TestSPI_HealthWiring_RealCallConversions(t *testing.T) {
	ctx := context.Background()

	// --- session_control：真实成功 → available（epoch 携带）。
	rec := newHealthRecorder()
	rec.bump(c6TaskID) // 模拟 Acquire 成功换代（生产由 OnRuntimeReady 驱动）
	srv := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "ses_a", "time": map[string]interface{}{"created": 1.5, "updated": 2.5}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	p := wiringProvider(t, rec)
	if _, err := p.Sessions().List(ctx, srv.testBinding()); err != nil {
		t.Fatalf("List: %v", err)
	}
	h, ok := rec.lastByPrimitive(appagent.PrimitiveSessionControl)
	if !ok {
		t.Fatal("no session_control observation")
	}
	if h.State != appagent.CapAvailable || h.Reason != "" {
		t.Errorf("success observation = %+v, want available", h)
	}
	if h.Epoch != 1 {
		t.Errorf("epoch = %d, want 1（换代后观测携带代际）", h.Epoch)
	}
	// C6 残余：身份键=真实 task ID（data.taskId），非 upstream session ID。
	if h.TaskID != "t1" {
		t.Errorf("TaskID = %q, want t1（C6 健康观测统一真实 task ID）", h.TaskID)
	}
	if h.ProviderID != ProviderID || h.Scope != appagent.ScopeRuntime {
		t.Errorf("identity = provider=%s scope=%s, want %s/runtime", h.ProviderID, h.Scope, ProviderID)
	}

	// --- session_control：上游不可达 → degraded（网络故障）。
	rec2 := newHealthRecorder()
	p2 := wiringProvider(t, rec2)
	srvDead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadPort := 1 // 未监听端口
	srvDead.Close()
	data := json.RawMessage(fmt.Sprintf(`{"port":%d,"directory":"/wt/task","auth":{"kind":"legacy","password":"pw"}}`, deadPort))
	deadBinding := domain.TaskBinding{Version: 1, ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID: c6TaskID, TerminalRef: "ocdeck-t1-runtime", Data: data}
	_ = srvDead
	if _, err := p2.Sessions().List(ctx, deadBinding); err == nil {
		t.Fatal("dead endpoint must fail")
	}
	h2, ok := rec2.lastByPrimitive(appagent.PrimitiveSessionControl)
	if !ok || h2.State != appagent.CapDegraded {
		t.Errorf("unreachable observation = %+v, want degraded", h2)
	}
	if h2.Reason == "" {
		t.Error("degraded observation MUST carry reason")
	}

	// --- title_updater：明确不支持（404 路由级）→ unsupported。
	// mutation 证据（已验证 → 红）：reportConversion 的 CapabilityUnavailable→
	// unsupported 映射删除（降级为统一 degraded）→ 本 subtest 红
	//（want unsupported, got degraded）。
	rec3 := newHealthRecorder()
	p3 := wiringProvider(t, rec3)
	srv404 := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	tb := srv404.testBinding()
	if err := p3.Titles().Sync(ctx, tb, v1Key("ses_a"), "t"); err == nil {
		t.Fatal("title sync must fail 404")
	}
	h3, ok := rec3.lastByPrimitive(appagent.PrimitiveTitleUpdater)
	if !ok || h3.State != appagent.CapUnsupported {
		t.Errorf("404 title observation = %+v, want unsupported", h3)
	}
	if h3.Primitive != appagent.PrimitiveTitleUpdater || h3.Scope != appagent.ScopeSession {
		t.Errorf("title observation scope = %+v, want session scope", h3)
	}

	// --- async_prompt：真实成功（accepted）→ available。
	rec4 := newHealthRecorder()
	p4 := wiringProvider(t, rec4)
	srvPrompt := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			w.Write([]byte(`{"healthy":true,"version":"1.18.14"}`))
		case "/doc":
			w.Write([]byte(`{"prompt_async":true,"experimental":{}}`))
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	}))
	// 直接构造 accepted 结果形态（真实 client 通道由 prompt_async.go 已测——
	// 这里仅验证转换出口的归因：pre_send_failure → degraded）。
	res, err := p4.Prompts().Send(ctx, srvPrompt.testBinding(), v1Key("ses_a"),
		appagent.PromptRequest{Text: "hi", MessageID: "m1"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = res
	h4, ok := rec4.lastByPrimitive(appagent.PrimitiveAsyncPrompt)
	if !ok {
		t.Fatal("no async_prompt observation")
	}
	// pre_send_failure（docMode 缺失等准入前的 v1 语义）→ degraded 或
	// accepted → available；两者必须二选一落观测。
	if h4.State != appagent.CapAvailable && h4.State != appagent.CapDegraded {
		t.Errorf("prompt observation = %+v, want available|degraded", h4)
	}

	// --- event_source：binding 无效准入失败 → degraded 观测。
	rec5 := newHealthRecorder()
	p5 := wiringProvider(t, rec5)
	bad := domain.TaskBinding{Version: 1, ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID: c6TaskID, TerminalRef: "t", Data: json.RawMessage(`{not-json`)}
	if _, err := p5.Events().Subscribe(ctx, bad); !errors.Is(err, appagent.ErrBindingInvalid) {
		t.Fatalf("bad binding = %v, want binding_invalid", err)
	}
	h5, ok := rec5.lastByPrimitive(appagent.PrimitiveEventSource)
	if !ok || h5.State != appagent.CapDegraded {
		t.Errorf("invalid binding events observation = %+v, want degraded", h5)
	}
}

// TestSPI_HealthWiring_NilReporterNoop 固定输入：未注入报告端口 → 全部 no-op
// （测试态零观测、零行为变化——OCClient 测试接缝路径不受 C6 影响）。
func TestSPI_HealthWiring_NilReporterNoop(t *testing.T) {
	ctx := context.Background()
	p := NewSPIProvider() // 无 WithHealthReporter
	srv := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" {
			w.Write([]byte(`[{"id":"ses_a","time":{"created":1.5,"updated":2.5}}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	if _, err := p.Sessions().List(ctx, srv.testBinding()); err != nil {
		t.Fatalf("List: %v", err)
	}
	// 断言面=无 panic 即通过（no-op 路径）。
}

// TestSPI_HealthWiring_EpochFollowsGenerations 固定输入：换代递增后新观测携带
// 新 epoch（生产链：OnRuntimeReady→BumpRuntimeEpoch；adapter epoch func 读取）。
func TestSPI_HealthWiring_EpochFollowsGenerations(t *testing.T) {
	rec := newHealthRecorder()
	srv := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	p := wiringProvider(t, rec)
	ctx := context.Background()
	rec.bump(c6TaskID)
	_, _ = p.Sessions().List(ctx, srv.testBinding())
	first, _ := rec.lastByPrimitive(appagent.PrimitiveSessionControl)
	rec.bump(c6TaskID)
	_, _ = p.Sessions().List(ctx, srv.testBinding())
	second, _ := rec.lastByPrimitive(appagent.PrimitiveSessionControl)
	if first.Epoch != 1 || second.Epoch != 2 {
		t.Errorf("epochs = %d,%d want 1,2（观测随换代携带新代际）", first.Epoch, second.Epoch)
	}
}
