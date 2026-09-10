package task

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"ocdeck/internal/infrastructure/opencode"
)

// metaRaw 构造 metadata RawMessage。
func metaRaw(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

// TestUpsertPerm_MetadataPipeline 管道携带与等值判断修正（tasks 8.1，D12/F8②）：
// 第一段（可控 observedAt，独立 state + 新 requestID r9）——登记 command=A
//（Since=1000）→ 仅 command 改 B（changed=false、Metadata=B、Since 仍 1000）→
// 同值 B 再提交（no-op）。第二段——核心字段变化为可见变化、同值 no-op、快照
// 深拷贝字节隔离。
func TestUpsertPerm_MetadataPipeline(t *testing.T) {
	// —— 第一段：metadata-only 更新的 Since 保留（可控 observedAt）——
	st := newAttentionState()
	up := func(meta string, at int64) bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.upsertPermLocked(opencode.AttentionEvent{
			Kind: opencode.AttentionAsked, Type: opencode.AttentionPermission,
			RequestID: "r9", SessionID: "s9", Permission: "bash",
			Patterns: []string{"rm"}, Metadata: metaRaw(t, meta),
		}, at)
	}
	// 1. observedAt=1000 登记 command=A → 首次 Since=1000。
	if !up(`{"command":"A"}`, 1000) {
		t.Fatal("first registration must be a visible change")
	}
	snap := st.attentionSnapshot()
	if snap.Permissions[0].Since != 1000 {
		t.Fatalf("first Since = %d, want 1000", snap.Permissions[0].Since)
	}
	// 2. observedAt=5000 仅 command A→B：changed=false、Metadata=B、Since 仍 1000。
	if changed := up(`{"command":"B"}`, 5000); changed {
		t.Error("metadata-only change MUST NOT be reported as external-visible change")
	}
	snap = st.attentionSnapshot()
	if string(snap.Permissions[0].Metadata) != `{"command":"B"}` {
		t.Errorf("metadata-only change MUST update internal storage, got %s", snap.Permissions[0].Metadata)
	}
	if snap.Permissions[0].Since != 1000 {
		t.Errorf("Since = %d, want 1000（metadata 更新不重置 since）", snap.Permissions[0].Since)
	}
	// 3. observedAt=9000 再提交同值 B → 同值 no-op。
	if up(`{"command":"B"}`, 9000) {
		t.Error("identical re-submit MUST be no-op")
	}

	// —— 第二段：核心字段变化 / 同值 no-op / 深拷贝（r1，真实时钟）——
	a := newAttentionState()
	asked := func(meta string) opencode.AttentionEvent {
		return opencode.AttentionEvent{
			Kind: opencode.AttentionAsked, Type: opencode.AttentionPermission,
			RequestID: "r1", SessionID: "s1", Permission: "bash",
			Patterns: []string{"rm"}, Metadata: metaRaw(t, meta),
		}
	}
	if changed := a.applyAttentionEvent(asked(`{"command":"ls"}`)); !changed {
		t.Fatal("first asked must be visible change")
	}
	snap = a.attentionSnapshot()
	if string(snap.Permissions[0].Metadata) != `{"command":"ls"}` {
		t.Fatalf("metadata must be carried through pipeline, got %s", snap.Permissions[0].Metadata)
	}

	// 核心字段变化 → 外部可见变化（原条件不变）。
	if changed := a.applyAttentionEvent(func() opencode.AttentionEvent {
		ev := asked(`{"command":"ls -la /etc"}`)
		ev.Permission = "edit"
		return ev
	}()); !changed {
		t.Error("core-field change MUST be visible change")
	}

	// 同值 no-op（含 metadata 同值）。
	if changed := a.applyAttentionEvent(func() opencode.AttentionEvent {
		ev := asked(`{"command":"ls -la /etc"}`)
		ev.Permission = "edit"
		return ev
	}()); changed {
		t.Error("identical re-asked MUST be no-op")
	}

	// 快照深拷贝：字节级修改快照 Metadata 不影响内部（F8：可失败断言）。
	snap = a.attentionSnapshot()
	if len(snap.Permissions[0].Metadata) == 0 {
		t.Fatal("precondition: metadata present")
	}
	snap.Permissions[0].Metadata[0] = '{' + 1 // 字节级篡改（'{'→'|'）
	snap2 := a.attentionSnapshot()
	if string(snap2.Permissions[0].Metadata) != `{"command":"ls -la /etc"}` {
		t.Errorf("snapshot Metadata MUST be deep-copied (byte isolation), got %s", snap2.Permissions[0].Metadata)
	}
}

// TestBufferedReplay_MetadataCarriedAndIsolated F8：后台 GET 在途期间送入带
// metadata 的 asked（真进缓冲）→ 失败重放写入 pending——metadata 携带且与调用方
// 返回值字节隔离；重放期间二次失败不重复（judged 无关，仅管道）。
func TestBufferedReplay_MetadataCarriedAndIsolated(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	oc.listPermissionsErr = errors.New("flux")
	a := rt.ensureAttentionState()

	// 预置 degraded（失败 bg 对账）。
	a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileBackground, nil)

	// GET 在途期间 asked 携带 metadata 入缓冲。
	gated := &gatedPermOC{mockOC: oc, entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.ocFactory = func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: gated, onReady: opts.OnReady}
	}
	done := make(chan struct{})
	go func() { m.retryAttentionDegraded(context.Background()); close(done) }()
	<-gated.entered
	asked := permAsked("r1", "s1", "bash", "rm")
	asked.Metadata = metaRaw(t, `{"command":"rm -rf build"}`)
	a.applyAttentionEvent(asked) // 入缓冲（owner 在途）
	// F8：重放完成后字节级篡改输入事件——pending 已持有 upsert 时的独立拷贝，
	// 原值不变（证明重放写回不受后续输入篡改影响）。
	close(gated.release)
	<-done
	asked.Metadata[0] = '|' // 字节级篡改输入事件

	if len(oc.listPermissionsResult) > 0 {
		oc.listPermissionsResult[0].Metadata = json.RawMessage(`{"mutated":1}`)
	}
	snap := a.attentionSnapshot()
	if len(snap.Permissions) != 1 {
		t.Fatalf("pending = %d, want 1（重放写入）", len(snap.Permissions))
	}
	if string(snap.Permissions[0].Metadata) != `{"command":"rm -rf build"}` {
		t.Errorf("replayed metadata = %s, want carried", snap.Permissions[0].Metadata)
	}

	// 二次失败重放：metadata 仍稳定（无重放丢失）。
	oc.listPermissionsErr = errors.New("flux2")
	a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
	snap = a.attentionSnapshot()
	if string(snap.Permissions[0].Metadata) != `{"command":"rm -rf build"}` {
		t.Errorf("metadata must survive repeated replay, got %s", snap.Permissions[0].Metadata)
	}
}
func TestReplacePerm_MetadataCarried(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	oc := newMockOC(true)
	oc.listPermissionsResult = []opencode.PermissionRequest{
		{ID: "r1", SessionID: "s1", Permission: "bash", Patterns: []string{"rm"},
			Metadata: metaRaw(t, `{"command":"ls"}`)},
		{ID: "r2", SessionID: "s2", Permission: "edit", Patterns: []string{"a.go"},
			Metadata: metaRaw(t, `{"filepath":"a.go","diff":"d"}`)},
	}
	a.reconcileAttention(t.Context(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)

	snap := a.attentionSnapshot()
	if len(snap.Permissions) != 2 {
		t.Fatalf("permissions = %d, want 2", len(snap.Permissions))
	}
	if string(snap.Permissions[0].Metadata) != `{"command":"ls"}` {
		t.Errorf("REST replace must carry metadata (r1), got %s", snap.Permissions[0].Metadata)
	}
	if string(snap.Permissions[1].Metadata) != `{"filepath":"a.go","diff":"d"}` {
		t.Errorf("REST replace must carry metadata (r2), got %s", snap.Permissions[1].Metadata)
	}
}

// TestReplacePerm_MetadataByteIsolation F7：REST 替换写入内部状态的 Metadata 与
// 调用方返回值字节隔离——修改 oc.listPermissionsResult 底层字节后内部不变。
// 缓冲重放路径（bg 失败重放经 upsert）同样核对字节隔离。
func TestReplacePerm_MetadataByteIsolation(t *testing.T) {
	res := []opencode.PermissionRequest{
		{ID: "r1", SessionID: "s1", Permission: "bash", Patterns: []string{"rm"},
			Metadata: json.RawMessage(`{"command":"ls"}`)},
	}
	oc := newMockOC(true)
	oc.listPermissionsResult = res
	a := newAttentionState()
	a.reconcileAttention(t.Context(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)

	// 篡改调用方持有的底层字节（'{'→'|'）。
	res[0].Metadata[0] = '|'

	snap := a.attentionSnapshot()
	if string(snap.Permissions[0].Metadata) != `{"command":"ls"}` {
		t.Fatalf("REST replace MUST copy metadata bytes, got %s", snap.Permissions[0].Metadata)
	}

	// 缓冲重放路径：使 cap 进入 degraded（失败 bg 对账）→ SSE asked 入缓冲 →
	// 失败重放写入 pending（经 upsert，已拷贝）——核对字节隔离。
	badOC := newMockOC(true)
	badOC.listPermissionsErr = errors.New("replay failure")
	a.reconcileAttention(t.Context(), badOC, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
	a.applyAttentionEvent(permAsked("r2", "s2", "edit", "a.go"))
	a.reconcileAttention(t.Context(), badOC, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
	snap = a.attentionSnapshot()
	if len(snap.Permissions) != 2 {
		t.Fatalf("replayed permissions = %d, want 2", len(snap.Permissions))
	}
}
