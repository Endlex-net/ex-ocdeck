package agentprovider

// health_test.go 覆盖 tasks 3.8 能力健康机制验证项：状态迁移矩阵
//（unknown→available、协议违约→degraded、明确不支持→unsupported）、
// 迁移记 warn 日志（同值重复观测不告警）、epoch 递增（Acquire/Resume 换代）
// 与重启归零 unknown、ResetRuntime 代际归零。

import (
	"bytes"
	"log"
	"strings"
	"testing"

	domain "ocdeck/internal/domain/agentprovider"
)

// captureLog 重定向全局 log 输出（返回恢复函数）；本包测试不并行，无交叉污染。
func captureLog() (*bytes.Buffer, func()) {
	var buf bytes.Buffer
	old := log.Writer()
	flag := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	return &buf, func() {
		log.SetOutput(old)
		log.SetFlags(flag)
	}
}

func TestHealthTracker_TransitionMatrixAndWarn(t *testing.T) {
	buf, restore := captureLog()
	defer restore()
	tr := NewHealthTracker()

	// 初始（预填）unknown → available（探测成功）。
	if !tr.Transition(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveTitleUpdater,
		Scope: ScopeRuntime, TaskID: "t1", Epoch: 1, State: CapAvailable}) {
		t.Error("unknown→available 必须判定为迁移")
	}
	if !strings.Contains(buf.String(), "unknown→available") {
		t.Errorf("迁移 warn 缺失: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "title_updater") || !strings.Contains(buf.String(), "warn") {
		t.Errorf("warn 日志缺 primitive/warn 前缀: %q", buf.String())
	}

	// 协议违约 → degraded（带 reason）。
	buf.Reset()
	if !tr.Transition(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveTitleUpdater,
		Scope: ScopeRuntime, TaskID: "t1", Epoch: 1, State: CapDegraded, Reason: "contract violation: bad shape"}) {
		t.Error("available→degraded 必须判定为迁移")
	}
	if !strings.Contains(buf.String(), "available→degraded") || !strings.Contains(buf.String(), "contract violation") {
		t.Errorf("degraded 迁移 warn 缺失/reason 丢失: %q", buf.String())
	}

	// 明确不支持 → unsupported。
	buf.Reset()
	if !tr.Transition(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveAsyncPrompt,
		Scope: ScopeRuntime, TaskID: "t1", Epoch: 2, State: CapUnsupported, Reason: "endpoint missing"}) {
		t.Error("→unsupported 必须判定为迁移")
	}
	if !strings.Contains(buf.String(), "→unsupported") {
		t.Errorf("unsupported 迁移 warn 缺失: %q", buf.String())
	}

	// 同值重复观测：不迁移、不告警。
	buf.Reset()
	if tr.Transition(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveAsyncPrompt,
		Scope: ScopeRuntime, TaskID: "t1", Epoch: 2, State: CapUnsupported, Reason: "endpoint missing"}) {
		t.Error("同值重复观测不得判定为迁移")
	}
	if buf.Len() != 0 {
		t.Errorf("同值观测不得记 warn: %q", buf.String())
	}

	// 记录已被 upsert（终态可读）。
	found := false
	for _, h := range tr.Snapshot() {
		if h.Primitive == PrimitiveTitleUpdater && h.Scope == ScopeRuntime {
			found = h.State == CapDegraded && h.Reason == "contract violation: bad shape"
		}
	}
	if !found {
		t.Error("Transition 后终态未被 upsert")
	}
}

func TestHealthTracker_EpochLifecycle(t *testing.T) {
	tr := NewHealthTracker()
	// Acquire/Resume 重建换代：递增。
	if e := tr.BumpRuntimeEpoch("t1"); e != 1 {
		t.Errorf("首次 epoch = %d, want 1", e)
	}
	if e := tr.BumpRuntimeEpoch("t1"); e != 2 {
		t.Errorf("重建后 epoch = %d, want 2", e)
	}
	if e := tr.RuntimeEpoch("t1"); e != 2 {
		t.Errorf("RuntimeEpoch = %d, want 2", e)
	}
	// 未观测任务为 0。
	if e := tr.RuntimeEpoch("t2"); e != 0 {
		t.Errorf("未观测任务 epoch = %d, want 0", e)
	}
	// ResetRuntime：代际归零（runtime 回收）。
	tr.ResetRuntime("t1")
	if e := tr.RuntimeEpoch("t1"); e != 0 {
		t.Errorf("回收后 epoch = %d, want 0", e)
	}
	if e := tr.BumpRuntimeEpoch("t1"); e != 1 {
		t.Errorf("回收后重建 epoch = %d, want 1（重新从 1 起）", e)
	}
}

func TestHealthTracker_RestartZeroUnknown(t *testing.T) {
	// server 重启语义：新 tracker（进程内内存归零）全部 primitive 回 unknown epoch 0。
	tr := NewHealthTracker()
	tr.BumpRuntimeEpoch("t1")
	tr.Transition(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveEventSource,
		Scope: ScopeRuntime, TaskID: "t1", State: CapAvailable})

	restarted := NewHealthTracker()
	snap := restarted.Snapshot()
	if len(snap) != 7 {
		t.Fatalf("重启后 Snapshot len = %d, want 7", len(snap))
	}
	for _, h := range snap {
		if h.State != CapUnknown || h.Epoch != 0 {
			t.Errorf("重启后记录 = %+v, want unknown/epoch0", h)
		}
	}
	if e := restarted.RuntimeEpoch("t1"); e != 0 {
		t.Errorf("重启后代际 = %d, want 0", e)
	}
}

// TestHealthTracker_SessionScopeIdentity 覆盖 session 作用域键身份
// （同任务不同 session 独立记录）。
func TestHealthTracker_SessionScopeIdentity(t *testing.T) {
	tr := NewHealthTracker()
	k1 := domain.SessionKey{ProviderID: "opencode-v1", ProviderInstanceID: "default", UpstreamID: "ses_1"}
	k2 := domain.SessionKey{ProviderID: "opencode-v1", ProviderInstanceID: "default", UpstreamID: "ses_2"}
	tr.Report(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveAsyncPrompt,
		Scope: ScopeSession, TaskID: "t1", Session: &k1, State: CapAvailable})
	tr.Report(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveAsyncPrompt,
		Scope: ScopeSession, TaskID: "t1", Session: &k2, State: CapDegraded, Reason: "x"})
	n := 0
	for _, h := range tr.Snapshot() {
		if h.Scope == ScopeSession {
			n++
		}
	}
	if n != 2 {
		t.Errorf("session 作用域记录 = %d, want 2（不同 upstream 独立）", n)
	}
}
