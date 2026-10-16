// status_test.go 落实 tasks 4.3：StatusReader 两态——未验证期（空 map + nil
// error + CapUnknown 上报）与实证切换后（冻结映射 + CapAvailable）。
package opencode2

import (
	"context"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

func TestStatus_Conservative_EmptyAndCapUnknown(t *testing.T) {
	setMasterSecret(t)
	p := NewSPIProvider(ProviderOptions{})
	var reports []appagent.CapabilityHealth
	p.WithHealthReporter(func(h appagent.CapabilityHealth) { reports = append(reports, h) },
		func(taskID string) int64 { return 1 })

	out, err := p.Status().Current(context.Background(), testBinding(t))
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	// D8-0 精确规则：空 map + nil error；不得推导 idle/busy。
	if len(out) != 0 {
		t.Fatalf("conservative mode must return empty map, got %v", out)
	}
	if len(reports) != 1 {
		t.Fatalf("reports: %v", reports)
	}
	h := reports[0]
	if h.Primitive != appagent.PrimitiveStatusReader || h.State != appagent.CapUnknown {
		t.Fatalf("health: %+v", h)
	}
}

func TestStatus_Verified_FrozenMappingAndCapAvailable(t *testing.T) {
	setMasterSecret(t)
	p := NewSPIProvider(ProviderOptions{StatusVerified: true})
	var reports []appagent.CapabilityHealth
	p.WithHealthReporter(func(h appagent.CapabilityHealth) { reports = append(reports, h) },
		func(taskID string) int64 { return 1 })

	// 事件侧状态注入（execution 边界事件的冻结映射由事件路径完成）。
	p.recordStatus(testSessionKey(), domain.StatusBusy)
	other := domain.SessionKey{ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID, UpstreamID: "ses_fixture02b"}
	p.recordStatus(other, domain.StatusIdle)

	out, err := p.Status().Current(context.Background(), testBinding(t))
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if out[testSessionKey()] != domain.StatusBusy || out[other] != domain.StatusIdle {
		t.Fatalf("snapshot: %v", out)
	}
	// 未知枚举跳过（冻结映射仅 busy/idle）。
	p.recordStatus(testSessionKey(), domain.StatusRetry)
	out, _ = p.Status().Current(context.Background(), testBinding(t))
	if _, has := out[testSessionKey()]; has {
		t.Fatalf("unknown enum must be skipped: %v", out)
	}
	if len(reports) != 2 {
		t.Fatalf("reports: %v", reports)
	}
	for _, h := range reports {
		if h.State != appagent.CapAvailable {
			t.Fatalf("verified mode must report CapAvailable, got %+v", h)
		}
	}
}
