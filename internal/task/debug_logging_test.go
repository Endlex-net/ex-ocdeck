// debug_logging_test.go 落实 OCDECK_DEBUG 的 task 层开关态（默认零输出；开启
// 后 [debug] 前缀诊断日志在位）。
package task

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/opencode2"
	ocdeckstore "ocdeck/internal/infrastructure/store"
)

// syncBuffer 是并发安全 log 捕获 buffer（包内既有测试的泄漏 goroutine 会在
// 捕获窗口内向全局 log 写入——读侧必须与这些写入互斥，race 才干净）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureDebugLog 捕获包级 log 输出执行 fn，返回原文。
func captureDebugLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf syncBuffer
	flags := log.Flags()
	out := log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	}()
	fn()
	return buf.String()
}

// TestDebugLogging_OffByDefaultZeroOutput 落实默认关：不注入 SetDebugLogging
// 时（进程级默认），debug 消费点零输出。
func TestDebugLogging_OffByDefaultZeroOutput(t *testing.T) {
	SetDebugLogging(false)
	ms := newMockStore()
	seedTaskRow(t, ms, "t1")
	out := captureDebugLog(t, func() {
		m := newTestManagerFull(t, ms, ms, newMockProc(), newMockWorktree(), newMockOC(true))
		if err := m.alignSessions(context.Background(), "t1", "/wt", newMockOC(true), AlignModeRepo); err != nil {
			t.Fatalf("alignSessions: %v", err)
		}
	})
	if strings.Contains(out, "[debug]") {
		t.Fatalf("debug disabled must produce zero [debug] output, got: %q", out)
	}
}

// TestDebugLogging_OnEmitsPrefixedDiagnostics 落实开启态：align/binding 分派
// 消费点输出 [debug] task: 前缀行（含对齐事实与分派形态）。
func TestDebugLogging_OnEmitsPrefixedDiagnostics(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)
	ms := newMockStore()
	seedTaskRow(t, ms, "t1")
	out := captureDebugLog(t, func() {
		m := newTestManagerFull(t, ms, ms, newMockProc(), newMockWorktree(), newMockOC(true))
		if err := m.alignSessions(context.Background(), "t1", "/wt", newMockOC(true), AlignModeRepo); err != nil {
			t.Fatalf("alignSessions: %v", err)
		}
	})
	if !strings.Contains(out, "[debug] task: align task=t1") {
		t.Fatalf("debug enabled must emit align diagnostics, got: %q", out)
	}
}

// TestDebugLogging_ResolverDecision 落实 resolver 决策日志：R 码路径输出输入
// 事实（binding/pinned/major/开关）与决策结果。
func TestDebugLogging_ResolverDecision(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)
	out := captureDebugLog(t, func() {
		resolveExecutionPath("", ocdeckstore.ProviderIDV2, machineFacts{MachineMajor: 2, V2Enabled: true})
	})
	if !strings.Contains(out, `[debug] task: resolver: binding="" pinned="opencode-v2" machineMajor=2 v2Enabled=true -> exec="opencode-v2" adopt=false`) {
		t.Fatalf("resolver decision diagnostics missing, got: %q", out)
	}
}

// debugSeamProvider 是 binding 分派 debug 测试的接缝形态 provider：归属 checker
// 在位（gate 可过）、Sessions() 窄接口 nil（桥接不生效）、BindingRuntimeEndpoint
// 走 CapabilityUnavailable 接缝形态（isTestSeamBindingErr 回退镜像——与
// minimalProvider 的 C2 过渡语义一致）。
type debugSeamProvider struct {
	v2Stub
}

func (debugSeamProvider) CheckBindingOwnership(domain.TaskBinding, string, string) error { return nil }

func (debugSeamProvider) BindingRuntimeEndpoint(domain.TaskBinding) (int, string, string, error) {
	return 0, "", "", appagent.NewError("test.endpoint", appagent.ErrKindCapabilityUnavailable,
		errors.New("test seam: endpoint decode unavailable"))
}

// TestDebugLogging_BridgeDispatchForm 落实 binding 分派日志：v2 binding +
// 真实窄接口 provider（opencode2 SPIProvider——Sessions() 非 nil）→ form=bridge；
// 测试接缝形态（窄接口 nil）→ form=fallback。
func TestDebugLogging_BridgeDispatchForm(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)
	ms := newMockStore()
	seedV2BindingTask(t, ms, "t-br")
	t.Setenv(appagent.MasterSecretEnv, "test-master-secret")
	// 真实形态 v2 provider（Sessions() 窄接口在位）→ 桥接优先。
	reg := appagent.NewRegistry()
	prov := opencode2.NewSPIProviderWithDeps(opencode2.ProviderOptions{}, &opencode2.SupervisorDeps{})
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m := newTestManagerFull(t, ms, ms, newMockProc(), newMockWorktree(), newMockOC(true))
	m.providerRegistry = reg
	out := captureDebugLog(t, func() {
		if _, err := m.buildTaskOCClient(context.Background(), "t-br", appagent.Options{HealthTimeout: time.Second}); err != nil {
			t.Fatalf("buildTaskOCClient: %v", err)
		}
	})
	if !strings.Contains(out, "[debug] task: oc client task=t-br provider=opencode-v2 form=bridge") {
		t.Fatalf("bridge dispatch diagnostics missing, got: %q", out)
	}

	// 测试接缝形态（窄接口 nil、接缝 endpoint）→ fallback 维持既有工厂路径。
	reg2 := appagent.NewRegistry()
	if err := reg2.Register(&debugSeamProvider{}); err != nil {
		t.Fatal(err)
	}
	m2 := newTestManagerFull(t, ms, ms, newMockProc(), newMockWorktree(), newMockOC(true))
	m2.providerRegistry = reg2
	out2 := captureDebugLog(t, func() {
		if _, err := m2.buildTaskOCClient(context.Background(), "t-br", appagent.Options{HealthTimeout: time.Second}); err != nil {
			t.Fatalf("buildTaskOCClient: %v", err)
		}
	})
	if !strings.Contains(out2, "form=fallback") {
		t.Fatalf("test-seam dispatch must log fallback form, got: %q", out2)
	}
}
