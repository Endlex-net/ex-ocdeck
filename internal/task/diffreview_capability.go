// diffreview_capability.go 实现 task 层能力协调（design.md D1 探测事件模型 + D2 能力门禁）。
//
// 能力缓存归属 RuntimePortAdapter（内存态，instVersion-bound），以独立 registry 存储，
// 不侵入 taskRuntime / RuntimePortAdapter 结构体（避免修改 OUT 文件 diffreview_adapters.go 的
// 结构体定义，以及与 3c 并行子任务在 manager.go 上的结构体扩展冲突）。
//
// 探测事件模型（D1，唯一时序）：
//   - 首探：runtime ready 时 eager 执行一次（由 Manager 激活路径触发，见 manager 接线）。
//   - 复探：GET /annotations 与提交准入遇 absent/unknown 时同步复探，并发经 singleflight 合并。
//   - 恢复门禁：重启恢复 queued 队列发送前须先获得 supported。
//   - 运行时复核：PromptAsync 400 或意外 2xx 后置 unknown 触发复探（InvalidateCapability）。
//   - 缓存绑定 instVersion：Suspend/重启/实例替换失效（instVersion 不匹配 → 视为 absent）。
//
// 404→GetSession 分流在调度器结果处理（diffreview_scheduler.go），不在本文件——
// ProbeCapability 是 GET /doc 探测（404→unknown），与 PromptAsync POST 404 分流是两条独立路径。
package task

import (
	"context"
	"errors"
	"sync"

	"ocdeck/internal/application/diffreview"
	"ocdeck/internal/infrastructure/opencode"
)

// capabilityCache 为 per-taskID 的 instVersion-bound 能力缓存条目。
// instVersion 不匹配（Suspend/重启/实例替换）→ 视为 absent（缓存失效）。
type capabilityCache struct {
	instVersion string                     // 绑定的 runtime 实例版本（空=无运行时）
	state       diffreview.CapabilityState // 缓存状态（supported/unsupported/unknown）
}

// capRegistry 为 per-Manager 的能力缓存 + singleflight 注册表。
// 以 *Manager 指针为 key 隔离不同 Manager 实例（测试并发构造多个 Manager 时互不干扰）。
//
// F13：singleflight inflight 键含 instVersion（taskID+"@"+instVer），使 runtime 探测期间被替换时
// 新实例的 eager/复探不被旧 inflight 吞掉（旧 inflight 结果经 writeCapCache fence 丢弃）。
type capRegistry struct {
	cache    map[string]capabilityCache // taskID → 缓存条目
	inflight map[string]chan struct{}   // taskID+"@"+instVer → 探测进行中信号（singleflight）
	// flightJoined 在有调用者挂入既有 flight 时非阻塞通知（缓冲 1，F12③ 测试确定性栅栏）。
	// 生产路径无消费者：发送走 default 分支，绝不阻塞探测关键路径。
	flightJoined chan struct{}
	mu           sync.Mutex
}

// capRegistries 以 *Manager 为 key 存放 per-Manager 注册表（懒初始化）。
// sync.Map 避免包初始化顺序依赖；Registry 生命周期与 Manager 一致（Manager 不回收则保留）。
var capRegistries sync.Map

// getCapRegistry 取（或懒初始化）某 Manager 的能力缓存注册表。
func (a *RuntimePortAdapter) getCapRegistry() *capRegistry {
	if v, ok := capRegistries.Load(a.m); ok {
		return v.(*capRegistry)
	}
	reg := &capRegistry{
		cache:        map[string]capabilityCache{},
		inflight:     map[string]chan struct{}{},
		flightJoined: make(chan struct{}, 1),
	}
	v, _ := capRegistries.LoadOrStore(a.m, reg)
	return v.(*capRegistry)
}

// ProbeCapability 探测/复探 prompt_async 能力并更新 instVersion-bound 缓存（design.md D1 事件模型②）。
//
// 并发请求经 singleflight 合并为一次探测：首个调用者执行 GET /doc，后续调用者等待结果后读缓存。
// 任务无运行时（getRuntime nil）→ CapabilityAbsent（不探测）；instVersion 变化 → 缓存失效，重新探测。
// 探测底层失败 → CapabilityUnknown（GET /doc 自身 404/5xx/网络→unknown，opencode 包已封装）。
//
// 缓存稳定性（F2/D1）：仅 supported/unsupported 作稳定缓存直接返回；unknown 为非稳定态——
// 下一次独立请求 MUST 触发 singleflight 复探。400/意外 2xx 后 InvalidateCapability 置 unknown +
// 立即 ProbeCapability 触发本轮复探（scheduler 已实现）。
//
// F13：singleflight inflight 键含 instVersion。runtime 探测期间被替换时：
//   - 旧 inflight 的探测结果经 writeCapCache 的 instVersion fence 丢弃（不覆盖新实例缓存）。
//   - 新实例的 ProbeCapability 不被旧 inflight 吞掉（键不同，直接执行新探测）。
//   - 等待旧 inflight 完成后，若 instVersion 已变化，MUST NOT 返回旧 supported 缓存，
//     降级为重新探测或返回 unknown（避免调用方误用已失效实例的能力结论）。
//
// F15：稳定缓存检查与 inflight 注册在同一 reg.mu 临界区内完成，消除 TOCTOU 窗口
// （原分两临界区导致压力并发下稳定缓存未命中后多调用者绕过 inflight 各自发起 probe）。
//
// F13①③：leader 的 writeCapCache 返回 fence 成功与否，fence 失败不得返回 supported
// （runtime 已替换，旧 probe 结果对当前实例无意义）；waiter 不再无界递归，改有界迭代
// （探测前后都校验 instVersion，连续替换超上限返回 unknown）。
//
// 返回探测后的缓存状态（supported/unsupported/unknown；absent 仅无运行时）。
func (a *RuntimePortAdapter) ProbeCapability(ctx context.Context, taskID string) (diffreview.CapabilityState, error) {
	const maxInstanceRotations = 8 // F13③：有界迭代上限，连续 runtime 替换超过此值降级 unknown
	for rotation := 0; rotation < maxInstanceRotations; rotation++ {
		rt := a.m.getRuntime(taskID)
		if rt == nil {
			// 无运行时 → absent，不探测（design.md D1：能力缓存绑定 runtime 实例）。
			reg := a.getCapRegistry()
			reg.mu.Lock()
			delete(reg.cache, taskID)
			reg.mu.Unlock()
			return diffreview.CapabilityAbsent, nil
		}
		instVer := string(rt.instVersion)
		reg := a.getCapRegistry()
		key := taskID + "@" + instVer

		// F15：稳定缓存检查 + inflight 查找 + 新 flight 注册在同一 reg.mu 临界区内完成。
		reg.mu.Lock()
		// 稳定缓存命中（仅 supported/unsupported）→ 直接返回（unknown 非稳定，不命中）。
		if entry, ok := reg.cache[taskID]; ok && entry.instVersion == instVer {
			if entry.state != diffreview.CapabilityUnknown {
				st := entry.state
				reg.mu.Unlock()
				return st, nil
			}
		}
		if done, inflight := reg.inflight[key]; inflight {
			// 挂入既有 flight：非阻塞通知（F12③ 测试经此确认 waiter 已挂入，确定性栅栏）。
			select {
			case reg.flightJoined <- struct{}{}:
			default:
			}
			reg.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return diffreview.CapabilityUnknown, ctx.Err()
			}
			// 探测完成。F13：若 instVersion 仍一致，读缓存；不一致（runtime 已替换）→ 迭代重新探测，
			// MUST NOT 返回旧实例的 supported。
			curRt := a.m.getRuntime(taskID)
			if curRt == nil {
				return diffreview.CapabilityAbsent, nil
			}
			if string(curRt.instVersion) != instVer {
				// runtime 已替换：有界迭代重新探测（绑定新 instVersion），而非无界递归。
				continue
			}
			// F17：缓存读取经持锁 readCapCache（旧实现无锁直读 reg.cache 与并发写竞态，-race 可 panic）。
			if st, ok := readCapCache(reg, taskID, instVer); ok {
				return st, nil
			}
			return diffreview.CapabilityUnknown, nil
		}
		// 首个调用者：标记探测进行中，释放锁后执行 GET /doc。
		done := make(chan struct{})
		reg.inflight[key] = done
		reg.mu.Unlock()

		// F13②：探测绑定捕获的 runtime 实例（经 taskOcClient 获取的 client 对应当前实例）。
		// writeCapCache 内部 fence 校验确保仅当 instVersion 仍一致时写入。
		probed := a.probePromptAsync(ctx, taskID)
		fenced := writeCapCache(a.m, reg, taskID, instVer, probed)

		// 清理 singleflight 标记并广播。
		reg.mu.Lock()
		delete(reg.inflight, key)
		close(done)
		reg.mu.Unlock()

		// F13①：fence 失败（runtime 已替换）不得返回 supported（旧 probe 结果对当前实例无意义）。
		if !fenced {
			// runtime 已替换：有界迭代重新探测当前实例。
			continue
		}
		return probed, nil
	}
	// F13③：连续 runtime 替换超上限 → 降级 unknown。
	return diffreview.CapabilityUnknown, nil
}

// InvalidateCapability 将 instVersion-bound 能力缓存置 unknown（design.md D1 事件模型④）。
// PromptAsync 400 或意外 2xx 后调用：置 unknown 后，下一次独立请求（GET /annotations、提交准入、
// 调度门禁）经 ProbeCapability 即触发 singleflight 复探（F2：unknown 非稳定缓存，MUST 复探）。
// instVersion fencing：仅当当前 instVersion 与传入一致时失效（避免旧实例的失效覆盖新实例缓存）。
// 任务无运行时 → no-op（缓存已被 ProbeCapability 清理）。
func (a *RuntimePortAdapter) InvalidateCapability(ctx context.Context, taskID, instVersion string) {
	reg := a.getCapRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.cache[taskID]
	if !ok {
		return
	}
	if entry.instVersion != instVersion {
		// instVersion 已变化，旧缓存自然失效，不覆盖。
		return
	}
	entry.state = diffreview.CapabilityUnknown
	reg.cache[taskID] = entry
}

// SetCapabilityUnsupported 将 instVersion-bound 能力缓存直接置 unsupported
// （design.md D1 404 分流端点不支持分支：能力转 unsupported + 零重投，MUST NOT 复探）。
// 与 InvalidateCapability 不同：本方法写入稳定态 unsupported，下一次 ProbeCapability 命中稳定缓存
// 直接返回 unsupported，不触发复探（满足"零重投"——门禁三分支 unsupported 直接 failed）。
// instVersion fencing：仅当当前 instVersion 与传入一致时写入。
func (a *RuntimePortAdapter) SetCapabilityUnsupported(ctx context.Context, taskID, instVersion string) {
	reg := a.getCapRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	// 校验当前 instVersion 仍一致（探测期间可能 Suspend/重启）。
	rt := a.m.getRuntime(taskID)
	if rt == nil || string(rt.instVersion) != instVersion {
		return
	}
	reg.cache[taskID] = capabilityCache{instVersion: instVersion, state: diffreview.CapabilityUnsupported}
}

// GetSession 查询目标会话是否存在（design.md D1 404 分流穷尽）。
// 经 taskOcClient 获取 client 后调 OCClient.GetSession，映射结构化结果：
//   - 会话存在（GET 200）→ GetSessionFound。
//   - 会话明确不存在（opencode.ErrSessionNotFound，GET 404）→ GetSessionMissing。
//   - 其余（其他状态码/网络错误/解码失败）→ GetSessionUnknown（携带错误摘要）。
//
// adapter 获取失败（taskOcClient ok=false）→ GetSessionUnknown（runtime client unavailable）。
func (a *RuntimePortAdapter) GetSession(ctx context.Context, taskID, sessionID string) (diffreview.GetSessionResult, error) {
	oc, dir, ok := a.m.taskOcClient(ctx, taskID)
	if !ok {
		return diffreview.GetSessionResult{Status: diffreview.GetSessionUnknown, Detail: "runtime client unavailable"}, nil
	}
	_, err := oc.GetSession(ctx, dir, sessionID)
	if err == nil {
		return diffreview.GetSessionResult{Status: diffreview.GetSessionFound}, nil
	}
	if errors.Is(err, opencode.ErrSessionNotFound) {
		return diffreview.GetSessionResult{Status: diffreview.GetSessionMissing}, nil
	}
	return diffreview.GetSessionResult{Status: diffreview.GetSessionUnknown, Detail: err.Error()}, nil
}

// probePromptAsync 执行 GET /doc 探测（经 taskOcClient 获取 client）。
// adapter 获取失败 → CapabilityUnknown（探测底层失败，D1）。
func (a *RuntimePortAdapter) probePromptAsync(ctx context.Context, taskID string) diffreview.CapabilityState {
	oc, _, ok := a.m.taskOcClient(ctx, taskID)
	if !ok {
		return diffreview.CapabilityUnknown
	}
	ocState := oc.ProbePromptAsyncCapability(ctx)
	return mapOCCapabilityState(ocState)
}

// mapOCCapabilityState 将 opencode.CapabilityState（三值）映射为 diffreview.CapabilityState。
// opencode 三值 supported/unsupported/unknown → domain 四值中的前三个（absent 仅无运行时）。
func mapOCCapabilityState(s opencode.CapabilityState) diffreview.CapabilityState {
	switch s {
	case opencode.CapabilitySupported:
		return diffreview.CapabilitySupported
	case opencode.CapabilityUnsupported:
		return diffreview.CapabilityUnsupported
	default:
		return diffreview.CapabilityUnknown
	}
}

// readCapCache 读取缓存，instVersion 匹配时命中（含 unknown 非稳定态）。
// 返回 (state, true) 表示命中；(CapabilityAbsent, false) 表示缓存缺失/失效（需探测）。
// 调用方若仅需稳定缓存（supported/unsupported），用 readStableCapCache。
func readCapCache(reg *capRegistry, taskID, instVer string) (diffreview.CapabilityState, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.cache[taskID]
	if !ok || entry.instVersion != instVer {
		return diffreview.CapabilityAbsent, false
	}
	return entry.state, true
}

// readStableCapCache 读取稳定缓存（仅 supported/unsupported 命中；unknown/absent 不命中，需复探）。
// F2：unknown 为非稳定态，下一次独立请求 MUST 触发 singleflight 复探。
func readStableCapCache(reg *capRegistry, taskID, instVer string) (diffreview.CapabilityState, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.cache[taskID]
	if !ok || entry.instVersion != instVer {
		return diffreview.CapabilityAbsent, false
	}
	if entry.state == diffreview.CapabilityUnknown {
		// unknown 非稳定，不命中（触发复探）。
		return diffreview.CapabilityAbsent, false
	}
	return entry.state, true
}

// writeCapCache 写入缓存条目（instVersion-bound）。仅当 instVersion 仍一致时写入。
// F13①：返回 fenced bool——true 表示 instVersion 仍一致且已写入；false 表示 runtime 已替换
// （fence 失败），调用方不得返回旧 probe 结果（旧实例的 supported 对当前实例无意义）。
func writeCapCache(m *Manager, reg *capRegistry, taskID, instVer string, state diffreview.CapabilityState) bool {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	// instVersion fencing：写入前再次校验当前 instVersion 未变（探测期间可能 Suspend/重启）。
	rt := m.getRuntime(taskID)
	if rt == nil || string(rt.instVersion) != instVer {
		return false
	}
	reg.cache[taskID] = capabilityCache{instVersion: instVer, state: state}
	return true
}
