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
	registered, phase := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur)
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
	registered, phase := r.RegisterIfCurrent("t1", v, DebtPhasePostCleanup, nil)
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
	registered, _ := r.RegisterIfCurrent("t1", stale, DebtPhasePreCleanup, &cur)
	if registered {
		t.Fatal("stale trigger token MUST NOT register")
	}
	if _, ok := r.Get("t1"); ok {
		t.Fatal("no debt entry should exist for stale trigger")
	}
	// runtime nil 但 tombstone 非触发令牌 → 同样 stale。
	registered, _ = r.RegisterIfCurrent("t1", stale, DebtPhasePreCleanup, nil)
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
	if registered, _ := r.RegisterIfCurrent("t1", newTok, DebtPhasePreCleanup, &cur); !registered {
		t.Fatal("precondition: newer token register failed")
	}
	// 旧令牌再登记 → 不覆盖新代登记（tombstone 已推进到 newTok，oldTok 为 stale）。
	oldCur := oldTok
	registered, phase := r.RegisterIfCurrent("t1", oldTok, DebtPhasePreCleanup, &oldCur)
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
	if registered, _ := r.RegisterIfCurrent("t1", tok1, DebtPhasePreCleanup, &cur1); !registered {
		t.Fatal("precondition: initial register failed")
	}
	// 换代：tombstone 推进到 tok2（模拟 Suspend→重新 Activate）。
	tok2 := r.NewInstVersion("t1")
	cur2 := tok2
	registered, phase := r.RegisterIfCurrent("t1", tok2, DebtPhasePreCleanup, &cur2)
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
	if registered, _ := r.RegisterIfCurrent("t1", tok1, DebtPhasePreCleanup, &cur1); !registered {
		t.Fatal("precondition: initial register failed")
	}
	// 换代：tombstone → tok2。
	tok2 := r.NewInstVersion("t1")

	// 旧令牌携带「仍匹配」的 runtime 快照登记 → tombstone 已越过，MUST 拒绝。
	staleSnap := tok1
	if registered, _ := r.RegisterIfCurrent("t1", tok1, DebtPhasePostCleanup, &staleSnap); registered {
		t.Fatal("trigger moved past tombstone MUST be rejected even if runtime snapshot matches")
	}
	if entry, _ := r.Get("t1"); entry.Token != tok1 {
		t.Fatalf("debt token = %q, want still tok1 (stale register must not touch entry)", entry.Token)
	}

	// 新令牌触发（当前代）→ 原子替换旧债。
	cur2 := tok2
	if registered, _ := r.RegisterIfCurrent("t1", tok2, DebtPhasePreCleanup, &cur2); !registered {
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
	if _, phase := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur); phase != DebtPhasePreCleanup {
		t.Fatalf("pre+pre = %d, want preCleanup", phase)
	}
	// pre+post → post。
	if _, phase := r.RegisterIfCurrent("t1", v, DebtPhasePostCleanup, &cur); phase != DebtPhasePostCleanup {
		t.Fatalf("pre+post = %d, want postCleanup", phase)
	}
	// post+pre → 仍 post（不回退）。
	if _, phase := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur); phase != DebtPhasePostCleanup {
		t.Fatalf("post+pre = %d, want postCleanup (no rewind)", phase)
	}
}

// TestP147_AdvanceToPostCleanup_ExactCAS 精确 CAS 推进：匹配 taskID+token+preCleanup
// 才置 postCleanup；同令牌已 postCleanup → ok；令牌已换 → tokenMoved 且不删除；
// 记录缺失 → missing。
func TestP147_AdvanceToPostCleanup_ExactCAS(t *testing.T) {
	r := New()
	v := r.NewInstVersion("t1")
	cur := v
	if registered, _ := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur); !registered {
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
	if registered, _ := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur); !registered {
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
	if registered, _ := r.RegisterIfCurrent("t1", v, DebtPhasePreCleanup, &cur); !registered {
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
