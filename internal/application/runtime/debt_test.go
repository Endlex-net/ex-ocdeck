package runtime

import "testing"

// p147Token 构造便捷令牌。
func p147Token(v string) InstVersion {
	return InstVersion(v)
}

// TestP147_NewInstVersion_UpdatesFullTombstone 验证分配即把令牌写入 tombstone。
func TestP147_NewInstVersion_UpdatesFullTombstone(t *testing.T) {
	r := New()
	v := r.NewInstVersion("t1")
	tomb, ok := r.Tombstone("t1")
	if !ok || tomb != v {
		t.Fatalf("Tombstone = %q (ok=%v), want %q", tomb, ok, v)
	}
}

// TestP147_Tombstone_SurvivesClearSim 验证 clear-sim 后 tombstone 仍为最近分配令牌
//（B4：清理后保留）。
func TestP147_Tombstone_SurvivesClearSim(t *testing.T) {
	r := New()
	r.NewInstVersion("t1")
	// 模拟 clearRuntime：runtime 移除后直接再分配（P1.4.9 无 prevGen 参数）。
	v2 := r.NewInstVersion("t1")
	tomb, ok := r.Tombstone("t1")
	if !ok || tomb != v2 {
		t.Fatalf("Tombstone = %q (ok=%v), want %q (last allocated)", tomb, ok, v2)
	}
}

// TestP147_RegisterIfCurrent_MatchingCurrentRegistersPreCleanup 当前 runtime 令牌等于
// 触发令牌 → 允许登记（preCleanup 由调用方请求）。
func TestP147_RegisterIfCurrent_MatchingCurrentRegistersPreCleanup(t *testing.T) {
	r := New()
	v := r.NewInstVersion("t1")
	cur := v
	registered, phase := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur, false)
	if !registered || phase != DebtPhasePreCleanup {
		t.Fatalf("registered=%v phase=%d, want true/preCleanup", registered, phase)
	}
	entry, ok := r.Get("t1")
	if !ok || entry.Token != v || entry.Phase != DebtPhasePreCleanup {
		t.Fatalf("entry = %+v (ok=%v), want token %q preCleanup", entry, ok, v)
	}
}

// TestP147_RegisterIfCurrent_NilRuntimeTombstoneMatchAllowsPostCleanup runtime 为 nil
// 且 tombstone 等于触发令牌（cleanup 已发生）→ 允许登记 postCleanup。
func TestP147_RegisterIfCurrent_NilRuntimeTombstoneMatchAllowsPostCleanup(t *testing.T) {
	r := New()
	v := r.NewInstVersion("t1")
	registered, phase := r.RegisterIfCurrent("t1", v, DebtPhasePostCleanup, nil, false)
	if !registered || phase != DebtPhasePostCleanup {
		t.Fatalf("registered=%v phase=%d, want true/postCleanup", registered, phase)
	}
}

// TestP147_RegisterIfCurrent_StaleTokenNotRegistered 触发令牌非当前代（tombstone 已推进）
// → 不登记、不写表。
func TestP147_RegisterIfCurrent_StaleTokenNotRegistered(t *testing.T) {
	r := New()
	r.NewInstVersion("t1")
	v2 := r.NewInstVersion("t1") // tombstone → v2
	stale := p147Token("01724000000123-deadbe")
	cur := v2
	registered, _ := r.RegisterIfCurrent("t1", stale, DebtPhasePreCleanup, &cur, false)
	if registered {
		t.Fatal("stale trigger token MUST NOT register")
	}
	if _, ok := r.Get("t1"); ok {
		t.Fatal("no debt entry should exist for stale trigger")
	}
	// runtime nil 但 tombstone 非触发令牌 → 同样 stale。
	registered, _ = r.RegisterIfCurrent("t1", stale, DebtPhasePreCleanup, nil, false)
	if registered {
		t.Fatal("nil runtime with mismatched tombstone MUST NOT register")
	}
}

// TestP147_RegisterIfCurrent_DoesNotOverwriteNewerToken 既有登记令牌更新时，
// 旧令牌登记不得覆盖。
func TestP147_RegisterIfCurrent_DoesNotOverwriteNewerToken(t *testing.T) {
	r := New()
	oldTok := r.NewInstVersion("t1")
	newTok := r.NewInstVersion("t1")
	if _, ok := r.Get("t1"); ok {
		t.Fatal("precondition: no debt yet")
	}
	// 先登记新代（模拟新代锁超时触发）。
	cur := newTok
	if registered, _ := r.RegisterIfCurrent("t1", newTok, DebtPhasePreCleanup, &cur, false); !registered {
		t.Fatal("precondition: newer token register failed")
	}
	// 旧令牌再登记 → 不覆盖新代登记（tombstone 已推进到 newTok，oldTok 为 stale）。
	oldCur := oldTok
	registered, phase := r.RegisterIfCurrent("t1", oldTok, DebtPhasePreCleanup, &oldCur, false)
	if registered {
		t.Fatal("older token MUST NOT register over newer debt")
	}
	if phase != DebtPhasePreCleanup {
		t.Fatalf("actualPhase = %d, want existing preCleanup", phase)
	}
	entry, _ := r.Get("t1")
	if entry.Token != newTok {
		t.Fatalf("debt token = %q, want newer %q (not overwritten)", entry.Token, newTok)
	}
}

// TestP147_RegisterIfCurrent_NewerTokenReplacesOlderDebt 旧债存在时，新代（当前代）
// 触发令牌登记 MUST 原子替换旧注册项（design.md D2:341），不得被旧债挡住。
func TestP147_RegisterIfCurrent_NewerTokenReplacesOlderDebt(t *testing.T) {
	r := New()
	tok1 := r.NewInstVersion("t1")
	cur1 := tok1
	if registered, _ := r.RegisterIfCurrent("t1", tok1, DebtPhasePreCleanup, &cur1, false); !registered {
		t.Fatal("precondition: initial register failed")
	}
	// 换代：tombstone 推进到 tok2（模拟 Suspend→重新 Activate）。
	tok2 := r.NewInstVersion("t1")
	cur2 := tok2
	registered, phase := r.RegisterIfCurrent("t1", tok2, DebtPhasePreCleanup, &cur2, false)
	if !registered || phase != DebtPhasePreCleanup {
		t.Fatalf("registered=%v phase=%d, want true/preCleanup (newer token replaces older debt)", registered, phase)
	}
	entry, ok := r.Get("t1")
	if !ok || entry.Token != tok2 {
		t.Fatalf("entry = %+v (ok=%v), want token %q (atomic replace)", entry, ok, tok2)
	}
}

// TestP147_RegisterIfCurrent_TombstoneMovedRejectsEvenIfCurrentSnapshotMatches
// tombstone 已推进（换代）→ 触发令牌即使带着仍匹配的 runtime 快照也 MUST 被拒绝
//（tombstone 是代际权威，快照可能滞后）；新令牌触发后原子替换旧债。
func TestP147_RegisterIfCurrent_TombstoneMovedRejectsEvenIfCurrentSnapshotMatches(t *testing.T) {
	r := New()
	tok1 := r.NewInstVersion("t1")
	cur1 := tok1
	if registered, _ := r.RegisterIfCurrent("t1", tok1, DebtPhasePreCleanup, &cur1, false); !registered {
		t.Fatal("precondition: initial register failed")
	}
	// 换代：tombstone → tok2。
	tok2 := r.NewInstVersion("t1")

	// 旧令牌携带「仍匹配」的 runtime 快照登记 → tombstone 已越过，MUST 拒绝。
	staleSnap := tok1
	if registered, _ := r.RegisterIfCurrent("t1", tok1, DebtPhasePostCleanup, &staleSnap, false); registered {
		t.Fatal("trigger moved past tombstone MUST be rejected even if runtime snapshot matches")
	}
	if entry, _ := r.Get("t1"); entry.Token != tok1 {
		t.Fatalf("debt token = %q, want still tok1 (stale register must not touch entry)", entry.Token)
	}

	// 新令牌触发（当前代）→ 原子替换旧债。
	cur2 := tok2
	if registered, _ := r.RegisterIfCurrent("t1", tok2, DebtPhasePreCleanup, &cur2, false); !registered {
		t.Fatal("new token register must succeed")
	}
	if entry, _ := r.Get("t1"); entry.Token != tok2 {
		t.Fatalf("debt token = %q, want tok2 (replaced)", entry.Token)
	}
}

// TestP147_RegisterIfCurrent_PhaseMergeMonotonic 同令牌重复登记阶段单调合并：
// pre+pre=pre；pre+post=post；post+pre 仍 post（MUST NOT 回退）。
func TestP147_RegisterIfCurrent_PhaseMergeMonotonic(t *testing.T) {
	r := New()
	v := r.NewInstVersion("t1")
	cur := v

	// pre+pre → pre。
	if _, phase := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur, false); phase != DebtPhasePreCleanup {
		t.Fatalf("pre+pre = %d, want preCleanup", phase)
	}
	// pre+post → post。
	if _, phase := r.RegisterIfCurrent("t1", v, DebtPhasePostCleanup, &cur, false); phase != DebtPhasePostCleanup {
		t.Fatalf("pre+post = %d, want postCleanup", phase)
	}
	// post+pre → 仍 post（不回退）。
	if _, phase := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur, false); phase != DebtPhasePostCleanup {
		t.Fatalf("post+pre = %d, want postCleanup (no rewind)", phase)
	}
}

// TestP147_RegisterIfCurrent_AttentionInvalidatedFlag 失效发布标记语义：
//   - 登记时记录（超时路径已发布 → true）；
//   - 同令牌合并只单调（true 后不回退 false）；
//   - AdvanceToPostCleanup 阶段推进保留标记；
//   - 新代替换安装新令牌自身的发布状态（不继承旧令牌的 true）。
func TestP147_RegisterIfCurrent_AttentionInvalidatedFlag(t *testing.T) {
	r := New()
	v1 := r.NewInstVersion("t1")
	cur1 := v1

	if registered, _ := r.RegisterIfCurrent("t1", v1, DebtPhasePreCleanup, &cur1, true); !registered {
		t.Fatal("precondition register failed")
	}
	// 同令牌再登记（该调用方未发布，false）→ 标记保持 true。
	if registered, _ := r.RegisterIfCurrent("t1", v1, DebtPhasePreCleanup, &cur1, false); !registered {
		t.Fatal("re-register failed")
	}
	if entry, _ := r.Get("t1"); !entry.AttentionInvalidated {
		t.Fatal("AttentionInvalidated must stay true on same-token merge (monotonic)")
	}

	// 阶段推进保留标记。
	if ok, missing, moved := r.AdvanceToPostCleanup("t1", v1); !ok || missing || moved {
		t.Fatalf("advance = ok:%v missing:%v moved:%v, want ok only", ok, missing, moved)
	}
	if entry, _ := r.Get("t1"); entry.Phase != DebtPhasePostCleanup || !entry.AttentionInvalidated {
		t.Fatalf("entry = %+v, want postCleanup with flag kept", entry)
	}

	// 新代替换：新令牌自身的发布状态（false），不继承旧令牌的 true。
	v2 := r.NewInstVersion("t1")
	cur2 := v2
	if registered, _ := r.RegisterIfCurrent("t1", v2, DebtPhasePreCleanup, &cur2, false); !registered {
		t.Fatal("new-gen register failed")
	}
	if entry, _ := r.Get("t1"); entry.Token != v2 || entry.AttentionInvalidated {
		t.Fatalf("entry = %+v, want token=%q flag=false (new token's own state)", entry, v2)
	}
}

// TestP147_ClaimAttentionInvalidation 发布所有权原子认领语义（唯一权威 marker +
// tombstone fencing）：
//   - 首次认领 → true；
//   - 同令牌再次认领 → false（该事实已发布，双发防护）；
//   - 新代令牌 → true（不同事实）；
//   - 换代后旧令牌 → false（tombstone 已推进，旧代 MUST NOT 发布过期失效）；
//   - marker 按任务隔离。
func TestP147_ClaimAttentionInvalidation(t *testing.T) {
	r := New()
	v1 := r.NewInstVersion("t1")

	if !r.ClaimAttentionInvalidation("t1", v1) {
		t.Fatal("first claim for a token MUST be allowed")
	}
	if r.ClaimAttentionInvalidation("t1", v1) {
		t.Fatal("second claim for the same token MUST be rejected (already published)")
	}

	// 新代令牌：不同事实 → 获准。
	v2 := r.NewInstVersion("t1")
	if !r.ClaimAttentionInvalidation("t1", v2) {
		t.Fatal("claim for a different (newer) token MUST be allowed")
	}
	// 换代后旧令牌：tombstone 已推进到 v2 → fencing 拒绝（不得发布过期失效）。
	if r.ClaimAttentionInvalidation("t1", v1) {
		t.Fatal("claim for an older token after generation moved on MUST be rejected (tombstone fencing)")
	}

	// 任务隔离：另一任务的 marker 不影响本任务。
	vOther := r.NewInstVersion("t2")
	if !r.ClaimAttentionInvalidation("t2", vOther) {
		t.Fatal("first claim for another task MUST be allowed")
	}
}

// TestP147_ClaimAttentionInvalidation_StaleDelayedTokenNeverClaims A→B→延迟 A 序列
//（oracle 第六轮 BLOCKER）：A 超时登记并认领发布 → 换代 B（tombstone 推进）→ B 认领
// 发布 → 延迟的 A 恢复执行再次认领 → MUST false（stale 失效不得发布，单一发布语义）。
func TestP147_ClaimAttentionInvalidation_StaleDelayedTokenNeverClaims(t *testing.T) {
	r := New()
	a := r.NewInstVersion("t1")

	if !r.ClaimAttentionInvalidation("t1", a) {
		t.Fatal("token A first claim MUST be allowed")
	}
	// 换代：B 分配推进 tombstone（A 的清理/登记流程被挂起）。
	b := r.NewInstVersion("t1")
	if !r.ClaimAttentionInvalidation("t1", b) {
		t.Fatal("token B claim MUST be allowed")
	}
	// 延迟的 A 恢复：即使 marker 当前是 B（与 A 不等），tombstone fencing 仍拒绝。
	if r.ClaimAttentionInvalidation("t1", a) {
		t.Fatal("delayed stale token A MUST NEVER claim after generation moved to B")
	}
}


// 才置 postCleanup；同令牌已 postCleanup → ok；令牌已换 → tokenMoved 且不删除；
// 记录缺失 → missing。
func TestP147_AdvanceToPostCleanup_ExactCAS(t *testing.T) {
	r := New()
	v := r.NewInstVersion("t1")
	cur := v
	if registered, _ := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur, false); !registered {
		t.Fatal("precondition register failed")
	}

	// 精确命中：preCleanup → postCleanup。
	ok, missing, tokenMoved := r.AdvanceToPostCleanup("t1", v)
	if !ok || missing || tokenMoved {
		t.Fatalf("advance = ok:%v missing:%v moved:%v, want ok only", ok, missing, tokenMoved)
	}
	if entry, _ := r.Get("t1"); entry.Phase != DebtPhasePostCleanup {
		t.Fatalf("phase = %d, want postCleanup", entry.Phase)
	}

	// 同令牌已 postCleanup → 幂等 ok。
	ok, missing, tokenMoved = r.AdvanceToPostCleanup("t1", v)
	if !ok || missing || tokenMoved {
		t.Fatalf("idempotent advance = ok:%v missing:%v moved:%v, want ok only", ok, missing, tokenMoved)
	}

	// 令牌已换（新代登记）→ tokenMoved，MUST NOT 删除注册项。
	newTok := r.NewInstVersion("t1")
	ok, missing, tokenMoved = r.AdvanceToPostCleanup("t1", newTok)
	if ok || missing || !tokenMoved {
		t.Fatalf("moved advance = ok:%v missing:%v moved:%v, want tokenMoved only", ok, missing, tokenMoved)
	}
	if entry, exists := r.Get("t1"); !exists || entry.Token != v {
		t.Fatalf("entry = %+v (exists=%v), want original token kept (not deleted on tokenMoved)", entry, exists)
	}

	// 记录缺失 → missing。
	ok, missing, tokenMoved = r.AdvanceToPostCleanup("t-none", v)
	if ok || !missing || tokenMoved {
		t.Fatalf("missing advance = ok:%v missing:%v moved:%v, want missing only", ok, missing, tokenMoved)
	}
}

// TestP147_CompareAndDelete_OnlyMatchingToken 仅 taskID+token 均匹配才删除。
func TestP147_CompareAndDelete_OnlyMatchingToken(t *testing.T) {
	r := New()
	v := r.NewInstVersion("t1")
	cur := v
	if registered, _ := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur, false); !registered {
		t.Fatal("precondition register failed")
	}

	// 令牌不匹配 → 不删除。
	if r.CompareAndDelete("t1", p147Token("01724000000999-zzzzzz")) {
		t.Fatal("CompareAndDelete with mismatched token MUST NOT delete")
	}
	if _, ok := r.Get("t1"); !ok {
		t.Fatal("entry must survive mismatched compare-and-delete")
	}
	// taskID 不匹配 → 不删除。
	if r.CompareAndDelete("t2", v) {
		t.Fatal("CompareAndDelete with mismatched taskID MUST NOT delete")
	}
	// 精确匹配 → 删除。
	if !r.CompareAndDelete("t1", v) {
		t.Fatal("CompareAndDelete with matching token MUST delete")
	}
	if _, ok := r.Get("t1"); ok {
		t.Fatal("entry must be deleted after matching compare-and-delete")
	}
	// 再删（已不存在）→ false。
	if r.CompareAndDelete("t1", v) {
		t.Fatal("CompareAndDelete on missing entry MUST return false")
	}
}

// TestP147_Snapshot_CopiesEntries Snapshot 返回拷贝，修改返回值不影响表内数据。
func TestP147_Snapshot_CopiesEntries(t *testing.T) {
	r := New()
	v := r.NewInstVersion("t1")
	cur := v
	if registered, _ := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur, false); !registered {
		t.Fatal("precondition register failed")
	}
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].Token != v {
		t.Fatalf("snapshot = %+v, want one entry with token %q", snap, v)
	}
	snap[0].Phase = DebtPhasePostCleanup
	if entry, _ := r.Get("t1"); entry.Phase != DebtPhasePreCleanup {
		t.Fatal("mutating snapshot MUST NOT affect registry state")
	}
}
