// health.go 实现能力健康的迁移与代际机制（opencode-v1-spi-provider tasks 3.8
// / design D7）：状态迁移记 warn 日志、runtime 代际（Acquire/Resume 重建递增、
// 回收/重启归零）、provider 作用域 primitives 预填 unknown（server 重启后全部
// 能力回到 unknown 的透出语义）。
package agentprovider

import (
	"log"
)

// allCapabilityPrimitives 是能力原语全集（provider 作用域预填用）。
var allCapabilityPrimitives = []CapabilityPrimitive{
	PrimitiveSessionControl,
	PrimitiveEventSource,
	PrimitiveAttentionIO,
	PrimitiveAsyncPrompt,
	PrimitiveStatusReader,
	PrimitiveTerminalIO,
	PrimitiveTitleUpdater,
}

// NewHealthTracker 构造 tracker：provider 作用域全部原语预填 unknown
// （epoch=0；server 重启后内存归零，透出语义即「重启归零 unknown」——
// runtime/session 作用域记录随消费出现，不预填）。
func NewHealthTracker() *HealthTracker {
	t := &HealthTracker{
		entries:    map[string]CapabilityHealth{},
		epochs:     map[string]int64{},
		providerID: "",
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range allCapabilityPrimitives {
		h := CapabilityHealth{Primitive: p, Scope: ScopeProvider, State: CapUnknown}
		t.entries[h.identity()] = h
	}
	return t
}

// SetProviderID 登记 provider 标识（预填记录与后续上报的默认 ProviderID；
// 装配期调用一次）。
func (t *HealthTracker) SetProviderID(providerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.providerID = providerID
}

// Transition 记录一次状态观测：State 相对既有记录变化时记 warn 日志
// （design D7「状态迁移记 warn」——同值重复观测不告警）；随后 upsert。
// 返回是否发生状态迁移。
func (t *HealthTracker) Transition(h CapabilityHealth) bool {
	t.mu.Lock()
	prev, existed := t.entries[h.identity()]
	t.entries[h.identity()] = h
	t.mu.Unlock()
	if !existed || prev.State != h.State {
		log.Printf("warn: capability health transition: primitive=%s scope=%s task=%s state %s→%s reason=%q epoch=%d",
			h.Primitive, h.Scope, h.TaskID, stateOrUnknown(prev.State), h.State, h.Reason, h.Epoch)
		return true
	}
	return false
}

// stateOrUnknown 空状态显示为 unknown（新记录无前态）。
func stateOrUnknown(s CapabilityState) CapabilityState {
	if s == "" {
		return CapUnknown
	}
	return s
}

// BumpRuntimeEpoch 递增任务 runtime 代际（Acquire 成功 / Resume 重建成功时，
// D7 Epoch 语义）并返回新值。调用方据此组装后续能力观测的 Epoch。
func (t *HealthTracker) BumpRuntimeEpoch(taskID string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.epochs[taskID]++
	return t.epochs[taskID]
}

// RuntimeEpoch 返回任务当前 runtime 代际（无记录为 0）。
func (t *HealthTracker) RuntimeEpoch(taskID string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.epochs[taskID]
}

// ResetRuntime 清除任务全部 runtime/session 作用域记录并归零其代际
// （runtime 回收/删除；provider 作用域记录保留——见 agentprovider.go 注释）。
func (t *HealthTracker) ResetRuntime(taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, h := range t.entries {
		if h.TaskID == taskID && h.Scope != ScopeProvider {
			delete(t.entries, k)
		}
	}
	delete(t.epochs, taskID)
}
