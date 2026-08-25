// debt.go 收敛债务（两阶段 preCleanup/postCleanup）登记表（design.md D0:45/D2）。
//
// 债务表达「异常收敛未能当场完成」的事实：锁等待超时路径 MUST NOT 无锁清理/无锁 CAS
//（design.md D0:151 替换行为），仅按触发令牌登记债务，由 backgroundLoop worker 持锁消化。
// 表挂在 Registry 上，与 runtime 安装（NewInstVersion）/tombstone 更新同一互斥锁域
//（genMu，design.md D0:342），保证登记的过期判定、阶段推进与新代分配串行化。
//
// P1.4.9：令牌为单一 InstVersion 字符串（等值判定，MUST NOT 数值比大小），原
// RuntimeToken{instanceID, generation} 双字段已收敛。

package runtime

// DebtPhase 表达收敛债务的两阶段（design.md D2 债务两阶段）。
// 阶段只单调推进（preCleanup→postCleanup），MUST NOT 回退。
type DebtPhase int

const (
	// DebtPhasePreCleanup：清理尚未执行（runtime 可能仍存活），worker 持锁执行清理后推进。
	DebtPhasePreCleanup DebtPhase = iota + 1
	// DebtPhasePostCleanup：清理已完成（runtime 已清、tombstone 保留令牌），仅剩 CAS 重试。
	DebtPhasePostCleanup
)

// DebtEntry 为单条收敛债务（design.md D2：注册项携带触发时的 runtime 令牌 + 阶段）。
type DebtEntry struct {
	TaskID string
	Token  InstVersion
	Phase  DebtPhase
}

// RegisterIfCurrent 以触发令牌做登记前过期判定并登记/替换/合并债务（design.md D2
// registerDebtIfCurrent 语义 + D2:341-342 登记去重规则）。
//
// 过期判定只看 TRIGGER 令牌（不得用「等锁结束时刻」的当前令牌顶替），且以 tombstone
// 为代际权威（NewInstVersion 在同一 genMu 锁域内推进 tombstone）：
//   - tombstone 存在且 != trigger → 旧代 stale，一律拒绝（即使调用方 runtime 快照仍
//     匹配——快照可能滞后于换代）；
//   - 否则 currentRuntime 非 nil → 当前代当且仅当 *currentRuntime == trigger；
//   - 否则（currentRuntime 为 nil）→ cleanup 已发生语义，当前代当且仅当 tombstone
//     存在且 == trigger（调用方应请求 postCleanup）。
//
// 过期判定通过后按 taskID 去重登记（design.md D2:341-342）：
//   - 无既有登记 → 插入；
//   - 既有登记令牌 != trigger → 新代登记原子替换旧注册项（旧债不得挡住新代超时登记，
//     否则任务可能留在 active 且无托管 SSE）；
//   - 令牌相同 → 阶段单调合并取较高阶段（pre+pre=pre；pre+post=post），
//     MUST NOT postCleanup→preCleanup 回退。
//
// 返回 (registered, actualPhase)：registered=false 时 actualPhase 为既有登记的阶段
//（无登记时为零值），供调用方诊断。
func (r *Registry) RegisterIfCurrent(taskID string, trigger InstVersion, phase DebtPhase, currentRuntime *InstVersion) (registered bool, actualPhase DebtPhase) {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	existing, hasExisting := r.debts[taskID]
	if !r.triggerCurrentLocked(taskID, trigger, currentRuntime) {
		if hasExisting {
			return false, existing.Phase
		}
		return false, 0
	}
	if !hasExisting {
		r.debts[taskID] = DebtEntry{TaskID: taskID, Token: trigger, Phase: phase}
		return true, phase
	}
	if existing.Token != trigger {
		// 触发令牌仍是当前代（过期判定已通过）而既有登记为旧代 → 原子替换
		//（design.md D2:341「新代登记原子替换旧注册项」）。
		r.debts[taskID] = DebtEntry{TaskID: taskID, Token: trigger, Phase: phase}
		return true, phase
	}
	if phase > existing.Phase {
		existing.Phase = phase
		r.debts[taskID] = existing
	}
	return true, existing.Phase
}

// triggerCurrentLocked 判定触发令牌是否仍为当前代（调用方持 genMu）。
// tombstone 是代际权威（NewInstVersion 在本锁域内推进）：tombstone 已推进到其他令牌
// → 触发令牌为旧代，一律拒绝。runtime 快照（Manager.rtMu 域）仅作补充判定，不凌驾
// tombstone——快照可能滞后于换代。
func (r *Registry) triggerCurrentLocked(taskID string, trigger InstVersion, currentRuntime *InstVersion) bool {
	tomb, hasTomb := r.lastToken[taskID]
	if hasTomb && tomb != trigger {
		return false
	}
	if currentRuntime != nil {
		return *currentRuntime == trigger
	}
	return hasTomb && tomb == trigger
}

// AdvanceToPostCleanup 精确 CAS 推进阶段：匹配 taskID+token 且当前阶段为 preCleanup
// 才置 postCleanup（design.md D2：推进 MUST 为精确 CAS）。失败重读三态：
//   - 同令牌已是 postCleanup → ok=true（推进幂等）；
//   - 令牌已换（新代登记）→ tokenMoved=true，MUST NOT 删除注册项（防旧 worker 误删新代）；
//   - 记录缺失 → missing=true。
func (r *Registry) AdvanceToPostCleanup(taskID string, token InstVersion) (ok bool, missing bool, tokenMoved bool) {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	entry, exists := r.debts[taskID]
	if !exists {
		return false, true, false
	}
	if entry.Token != token {
		return false, false, true
	}
	if entry.Phase == DebtPhasePostCleanup {
		return true, false, false
	}
	entry.Phase = DebtPhasePostCleanup
	r.debts[taskID] = entry
	return true, false, false
}

// CompareAndDelete 仅当 taskID+token 均匹配时删除登记（design.md D2：移除 MUST 为
// compare-and-delete，防旧 worker 误删新代登记）。返回是否实际删除。
func (r *Registry) CompareAndDelete(taskID string, token InstVersion) bool {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	entry, exists := r.debts[taskID]
	if !exists || entry.Token != token {
		return false
	}
	delete(r.debts, taskID)
	return true
}

// Get 返回 taskID 当前债务登记。
func (r *Registry) Get(taskID string) (DebtEntry, bool) {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	entry, ok := r.debts[taskID]
	return entry, ok
}

// Snapshot 返回全部债务的拷贝（backgroundLoop tick 用）。
func (r *Registry) Snapshot() []DebtEntry {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	out := make([]DebtEntry, 0, len(r.debts))
	for _, e := range r.debts {
		out = append(out, e)
	}
	return out
}
