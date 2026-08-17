// Package runtime 提供 ServeRuntimeRegistry：进程内 ServeRuntime 实体的代际/tombstone
// 管理权威（OpenSpec change sse-active-sessions P1.3/P1.4.3）。
//
// 设计依据（design.md D0 step 3）：一次性迁移 generation/instanceID/tombstone 责任到
// ServeRuntimeRegistry，单一锁域，实体身份 RuntimeToken{instanceID, generation}，
// 不持有 *Task，无 Repository。MUST NOT 旧 Manager 与新 Registry 双写。
//
// 本步范围（P1.4.3）：Registry 承载 generation/instanceID 分配与 tombstone（lastGen）语义。
// ServeRuntime 实体（含 runtime groups/SSE/watch 句柄/attention 组件）因与 internal/task 的
// attentionState（owner/epoch/buffer）与 SSE goroutine 生命周期紧耦合，且既有测试经
// taskRuntime 字段直接访问（断言 groups/sseCancel/watchCancels），保留在 internal/task 侧
// 作为 *taskRuntime；Manager 经 Registry 取得 RuntimeToken 后构造 taskRuntime。这是在
// 「不破坏既有测试 + 不引入 application→task 反向依赖 + attention 业务逻辑不改」约束下的
// 最小安全迁移。runtime groups/clear 的编排责任随 taskRuntime 保留于 Manager 侧，由
// RuntimeToken 驱动身份校验（matchesRegistry 经 token 比对）；groups 数据本体与 clear 操作
// 仍在 taskRuntime，因其与 attention/SSE 生命周期不可分（clearRuntime 先 clearAttention
// 再 stopAll，D6 不变量）。
//
// 迁移不变量保持（design.md D0:146-156）：
//   - generation 单调递增、进程生命周期内不回卷（B4：即便 runtime 被 clearRuntime 移除，
//     下次 newRuntime 从 lastGen+1 续递增）。
//   - tombstone（lastGen）在 runtime 创建时更新、清理后保留（沿用 manager.go:204-208
//     lastGen 持久代先例）。
//   - 单一锁域：genMu 保护 lastGen，与 runtimes map 访问串行化（Registry 内部）。
package runtime

import (
	"sync"
)

// RuntimeToken 是 ServeRuntime 实体的身份标识（design.md D0:63,67）。
// instanceID 为本代实例标识（回调三元组校验用，B4）；generation 为激活代（单调递增）。
// 不含 taskID（Registry 按 taskID 索引实体，token 仅承载身份比对信息）。
type RuntimeToken struct {
	InstanceID string
	Generation  int
}

// Registry 是 ServeRuntime 实体的代际/tombstone 管理权威（design.md D0 step 3）。
// 单一锁域：genMu 保护 tombstone 与收敛债务表。MUST NOT 与旧 Manager 字段双写。
//
// Registry 仅负责 generation/instanceID 分配、tombstone 维护与收敛债务登记
//（P1.4.7，design.md D0:45）；ServeRuntime 实体（taskRuntime）本体、runtime groups、
// SSE/watch 句柄与 attention 组件保留在 Manager 侧（见包注释关于耦合与测试约束的说明）。
type Registry struct {
	genMu sync.Mutex
	// lastToken 为 taskID 最近一次分配的完整 RuntimeToken（tombstone，B4）。
	// P1.4.7 起从 lastGen-only 扩展为完整令牌（generation+instanceID）：
	// 锁超时/worker 的债务过期判定需匹配 instanceID，仅代际不足以隔离同代并发实例。
	lastToken map[string]RuntimeToken
	// debts 为收敛债务表（taskID → entry，design.md D0:45/D2）。与 runtime 安装
	// （NewRuntimeToken）/tombstone 更新同一互斥锁域（genMu，design.md D0:342），
	// 保证登记的过期判定、阶段推进与新代分配串行化。
	debts map[string]DebtEntry
}

// New 构造空 Registry。
func New() *Registry {
	return &Registry{
		lastToken: make(map[string]RuntimeToken),
		debts:     make(map[string]DebtEntry),
	}
}

// NewRuntimeToken 为 taskID 分配新的 RuntimeToken（design.md D0:67, B4）。
// generation 单调递增、进程生命周期内不回卷：prevGen 为当前激活代（0 表示无激活 runtime），
// Registry 在 prevGen+1 与 tombstone+1 中取较大值，保证清理后重建不复用旧代。
// 分配后把完整返回令牌写入 tombstone（generation+instanceID）。
//
// 语义对齐 notice.go:62-84 newRuntime 的 generation 分配逻辑：
//
//	gen = prevGen + 1（无 prev 则 0）
//	if tombstone.Generation >= gen { gen = tombstone.Generation + 1 }
//	tombstone = 新令牌
//
// instanceID 由调用方提供（Manager 侧用 newTaskID() 生成），Registry 不依赖其生成算法，
// 避免引入 task 包依赖。调用方在构造 taskRuntime 时将 token 的 generation/instanceID
// 写入实体字段。
func (r *Registry) NewRuntimeToken(taskID string, prevGen int, instanceID string) RuntimeToken {
	gen := prevGen + 1
	r.genMu.Lock()
	if last, ok := r.lastToken[taskID]; ok && last.Generation >= gen {
		gen = last.Generation + 1
	}
	tok := RuntimeToken{InstanceID: instanceID, Generation: gen}
	r.lastToken[taskID] = tok
	r.genMu.Unlock()
	return tok
}

// Tombstone 返回 taskID 当前 tombstone 令牌（最近一次分配的完整 RuntimeToken，
// design.md D0:204-208）。清理后保留（不随 clearRuntime 删除）；found=false 表示
// 该任务从未分配过令牌（无 tombstone）。供 P1.4.7 两阶段债务的令牌校验使用。
func (r *Registry) Tombstone(taskID string) (RuntimeToken, bool) {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	tok, ok := r.lastToken[taskID]
	return tok, ok
}