// Package runtime 提供 ServeRuntimeRegistry：进程内 ServeRuntime 实体的 instVersion/
// tombstone 管理权威（OpenSpec change sse-active-sessions P1.3/P1.4.3/P1.4.9）。
//
// 设计依据（design.md D0 step 3 + instVersion 定义，design.md:70）：一次性迁移
// generation/instanceID/tombstone 责任到 ServeRuntimeRegistry，单一锁域，实体身份
// instVersion（单字符串令牌），不持有 *Task，无 Repository。MUST NOT 旧 Manager 与新
// Registry 双写。
//
// 本步范围（P1.4.9）：原 RuntimeToken{instanceID, generation} 双字段收敛为单一
// InstVersion 字符串。fencing 只需等值判定（tombstone 为权威，MUST NOT 数值比大小），
// 双字段冗余；唯一性由 ms 时间戳前缀 + 随机 hex 后缀 + per-task issued 集合复检承载
//（构造上同任务不撞，见 NewInstVersion），可读顺序由时间戳前缀承载。
//
// ServeRuntime 实体（含 runtime groups/SSE/watch 句柄/attention 组件）因与 internal/task 的
// attentionState（owner/epoch/buffer）与 SSE goroutine 生命周期紧耦合，且既有测试经
// taskRuntime 字段直接访问，保留在 internal/task 侧作为 *taskRuntime；Manager 经 Registry
// 取得 instVersion 后构造 taskRuntime。这是在「不破坏既有测试 + 不引入 application→task
// 反向依赖 + attention 业务逻辑不改」约束下的最小安全迁移。runtime groups/clear 的编排
// 责任随 taskRuntime 保留于 Manager 侧，由 instVersion 驱动身份校验（matchesRegistry 经
// 令牌比对）；groups 数据本体与 clear 操作仍在 taskRuntime，因其与 attention/SSE 生命周期
// 不可分（clearRuntime 先 clearAttention 再 stopAll，D6 不变量）。
//
// 迁移不变量保持（design.md D0:146-156）：
//   - instVersion 构造上唯一（同任务连续分配不相等，含同毫秒；issued 集合确定性
//     保证），进程生命周期内回调 fencing 不误判（B4 语义由唯一性取代原 generation
//     单调递增）。
//   - tombstone 在 runtime 创建时更新、清理后保留（沿用 manager.go:204-208
//     lastGen 持久代先例）。
//   - 单一锁域：genMu 保护 lastToken 与 issued，与 runtimes map 访问串行化（Registry 内部）。
package runtime

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// InstVersion 是 ServeRuntime 实体的实例身份令牌（design.md:70，2026-08-18 决策
// 替代原 RuntimeToken{instanceID, generation} 双字段）。
//
// 格式：`<13 位毫秒时间戳>-<随机 hex 后缀>`（如 `01724000000123-a3f9c2`）。
// fencing 只需等值判定（tombstone 为权威，MUST NOT 数值比大小）；跨进程重启不防撞
// 也无需防——旧进程回调随进程死亡，比对只发生在单进程生命周期内。
type InstVersion string

// runStatusFact 标识一次 run_status 失效事实：令牌 + 该次失效 apply 分配的写代号
//（task 侧 agentStatusState.writeGen，单调递增）。同一令牌上「失效→恢复→再失效」
// 是不同事实（写代号更高），各自恰好发布一次；claim 按 (token, factID) 判定同一事实。
type runStatusFact struct {
	Token  InstVersion
	FactID uint64
}

// Registry 是 ServeRuntime 实体的 instVersion/tombstone 管理权威（design.md D0 step 3）。
// 单一锁域：genMu 保护 tombstone 与收敛债务表。MUST NOT 与旧 Manager 字段双写。
//
// Registry 仅负责 instVersion 分配、tombstone 维护与收敛债务登记（P1.4.7，design.md
// D0:45）；ServeRuntime 实体（taskRuntime）本体、runtime groups、SSE/watch 句柄与
// attention 组件保留在 Manager 侧（见包注释关于耦合与测试约束的说明）。
type Registry struct {
	genMu sync.Mutex
	// lastToken 为 taskID 最近一次分配的 InstVersion（tombstone，B4）。
	// 债务过期判定以 tombstone 为代际权威（RegisterIfCurrent，debt.go）。
	lastToken map[string]InstVersion
	// issued 记录 taskID 在本进程生命周期内分配过的全部令牌（含非当前的历史令牌）。
	// NewInstVersion 的重生成循环按 issued 集合拒绝重复候选：3 字节（24 位）随机后缀
	// 在同毫秒快速分配下存在生日界碰撞概率（实测 100 次跑出 7 个重复），仅对照当前
	// tombstone 会让与历史令牌相等的候选通过——延迟回调持该历史令牌即可误过等值
	// fencing（oracle BLOCKED）。issued 集合把唯一性从概率保证升级为构造性保证
	//（fencing 等值判定不误判的唯一性来源）。单任务单进程激活次数有限，内存可忽略。
	issued map[string]map[InstVersion]struct{}
	// debts 为收敛债务表（taskID → entry，design.md D0:45/D2）。与 runtime 安装
	//（NewInstVersion）/tombstone 更新同一互斥锁域（genMu，design.md D0:342），
	// 保证登记的过期判定、阶段推进与新代分配串行化。
	debts map[string]DebtEntry
	// invalidationPublished 记录 taskID 最近一次已发布 attention 可见失效的令牌
	//（taskID → token）。ClaimAttentionInvalidation 的 marker：发布所有权的唯一
	// 原子权威（genMu 锁域内比较并占位），消除「捕获时查询、发布时使用」的 TOCTOU
	// 双发窗口。换代自然重新获准（不同令牌 = 不同事实）；无需在新令牌分配时清理
	//（旧 marker 与新令牌比较不等即获准）。
	invalidationPublished map[string]InstVersion
	// runStatusInvalidated 记录 taskID 最近一次已发布 run_status 失效事实的
	//（令牌, 事实号）对（ClaimRunStatusInvalidation 的 marker）。与 attention 的
	// token-only marker 不同：run_status 失效在同一令牌上可先后发生多次（失效→恢复
	//→再失效各为独立事实，事实号取自失效 apply 分配的写代号，单调递增），claim 按
	// (token, factID) 精确去重。
	runStatusInvalidated map[string]runStatusFact
	// genValueFn 为令牌值生成器（测试接缝：默认 newInstVersionValue，测试注入
	// 脚本化序列以确定性复现候选碰撞）。仅本包内可替换，生产路径恒为默认实现。
	genValueFn func() InstVersion
}

// New 构造空 Registry。
func New() *Registry {
	return &Registry{
		lastToken:             make(map[string]InstVersion),
		issued:                make(map[string]map[InstVersion]struct{}),
		debts:                 make(map[string]DebtEntry),
		invalidationPublished: make(map[string]InstVersion),
		runStatusInvalidated:  make(map[string]runStatusFact),
		genValueFn:            newInstVersionValue,
	}
}

// NewInstVersion 为 taskID 分配新实例令牌并写入 tombstone（design.md:70）。
//
// 令牌 = 13 位毫秒时间戳前缀 + 随机 hex 后缀（crypto/rand 手拼，无第三方依赖）：
//   - 分配在 genMu 锁域内对照该任务 issued 集合复检，候选命中任一历史令牌（含当前
//     tombstone）即重新生成——同任务在本进程生命周期内构造上不重复（确定性保证，
//     不依赖随机后缀的概率充分性）；
//   - 时间戳前缀保证日志可读顺序。
//
// 无 prevGen/instanceID 参数：不做代际递增算术（MUST NOT 数值比大小）。跨任务碰撞
// 无害（比对只按 taskID 域内发生）。
func (r *Registry) NewInstVersion(taskID string) InstVersion {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	issued := r.issued[taskID]
	if issued == nil {
		issued = make(map[InstVersion]struct{})
		r.issued[taskID] = issued
	}
	v := r.genValueFn()
	for {
		if _, dup := issued[v]; !dup {
			break
		}
		// 候选命中本任务历史令牌（同毫秒 + 相同随机后缀）：重新生成直至集合外
		//（构造上唯一）。
		v = r.genValueFn()
	}
	issued[v] = struct{}{}
	r.lastToken[taskID] = v
	return v
}

// newInstVersionValue 生成 `<13 位 ms>-<6 hex>` 令牌值（调用方自行串行化复检）。
func newInstVersionValue() InstVersion {
	var suffix [3]byte
	_, _ = rand.Read(suffix[:]) // Go 1.24 起 crypto/rand.Read 保证填充成功
	return InstVersion(fmt.Sprintf("%013d-%x", time.Now().UnixMilli(), suffix[:]))
}

// Tombstone 返回 taskID 当前 tombstone 令牌（最近一次分配的 InstVersion，
// design.md D0:204-208）。清理后保留（不随 clearRuntime 删除）；found=false 表示
// 该任务从未分配过令牌（无 tombstone）。供两阶段债务的令牌校验使用（tombstone 为
// 代际权威）。
func (r *Registry) Tombstone(taskID string) (InstVersion, bool) {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	tok, ok := r.lastToken[taskID]
	return tok, ok
}
