// capability.go 实现 3.4 能力协调用例（design.md D1 探测事件模型 + D2 能力门禁）。
//
// 本文件仅承载 application 层协调逻辑：何时触发探测/复探、准入能力检查。
// 探测底层（GET /doc、缓存、singleflight、instVersion fencing）在 task 层 RuntimePortAdapter
// （diffreview_capability.go）实现，经 RuntimePort.ProbeCapability/InvalidateCapability 暴露。
//
// 事件模型归属（design.md D1 事件模型逐项）：
//   - 首探①：runtime ready 时 eager 执行一次 —— 由 Manager 激活路径触发（task 层接线）。
//   - 复探②：GET/annotations 与提交准入遇 absent/unknown 时同步复探 —— 本文件 EnsureCapabilitySupported。
//   - 恢复门禁③：重启恢复 queued 队列发送前须 supported —— scheduler 复用 EnsureCapabilitySupported。
//   - 运行时复核④：PromptAsync 400/意外 2xx 后置 unknown 复探 —— scheduler 调 InvalidateCapability。
//   - 缓存绑定 instVersion⑤：Suspend/重启/实例替换失效 —— RuntimePortAdapter 内部 fencing。
package diffreview

import "context"

// EnsureCapabilitySupported 确保 task 的 prompt_async 能力为 supported（design.md D1 事件模型②复探、
// ③恢复门禁）。遇 absent/unknown 同步复探（singleflight 合并由 RuntimePortAdapter 实现）。
//
// 返回值语义（与 D1/D2 对齐）：
//   - supported → nil（可继续）。
//   - unsupported → ErrCapabilityNotReady（端点不支持，不可重试）。
//   - unknown → ErrCapabilityNotReady（探测模糊，可重试）。
//   - absent → ErrTaskNotRunning（无运行时，任务未运行）。
//
// 用于：GET/annotations（submitCapability）、提交准入、调度器能力门禁复探分支、重启恢复门禁。
func (s *Service) EnsureCapabilitySupported(ctx context.Context, taskID string) error {
	st, err := s.rt.ProbeCapability(ctx, taskID)
	if err != nil {
		return err
	}
	switch st {
	case CapabilitySupported:
		return nil
	case CapabilityAbsent:
		return ErrTaskNotRunning
	default:
		// unsupported / unknown 均不满足 supported 准入。
		return ErrCapabilityNotReady
	}
}

// InvalidateCapability 置 instVersion-bound 能力缓存为 unknown（design.md D1 事件模型④运行时复核）。
// 由 scheduler 在 PromptAsync 返回 400 或意外 2xx 后调用，触发下次准入/列表复探。
// instVersion 由调用方从 RuntimeSnapshot 取（保证 fencing 一致性）。
func (s *Service) InvalidateCapability(ctx context.Context, taskID, instVersion string) {
	s.rt.InvalidateCapability(ctx, taskID, instVersion)
}

// ProbeFirst 执行 runtime-ready 主动首探（design.md D1 事件模型①）。
// 由 Manager 激活路径在 runtime ready 时调用一次，eager 填充能力缓存。
// 失败不阻断 runtime 启动（错误仅记录于调用方）；后续准入/列表遇 absent/unknown 仍会复探。
// 与 EnsureCapabilitySupported 不同：本方法不强制 supported，仅填充缓存（结果可为 unsupported/unknown）。
func (s *Service) ProbeFirst(ctx context.Context, taskID string) error {
	_, err := s.rt.ProbeCapability(ctx, taskID)
	return err
}

// SubmitCapability 返回 GET /annotations 响应的 submitCapability 结构（design.md D8）。
// state 为能力状态（supported/unsupported/unknown）；absent 映射为 unknown（API 不暴露 absent）。
// reason 为 state != supported 时的提示文案。
func (s *Service) SubmitCapability(ctx context.Context, taskID string) (state CapabilityState, reason string, err error) {
	st, err := s.rt.ProbeCapability(ctx, taskID)
	if err != nil {
		return CapabilityUnknown, "", err
	}
	switch st {
	case CapabilitySupported:
		return CapabilitySupported, "", nil
	case CapabilityUnsupported:
		return CapabilityUnsupported, "prompt_async endpoint not supported by opencode version", nil
	case CapabilityAbsent:
		// 无运行时 → API 侧仍需暴露（任务可能未激活），映射 unknown + 提示。
		return CapabilityUnknown, "task runtime not active, capability unknown", nil
	default:
		return CapabilityUnknown, "capability probe inconclusive", nil
	}
}
