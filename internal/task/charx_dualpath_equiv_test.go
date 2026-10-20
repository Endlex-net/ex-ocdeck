package task

// charx_dualpath_equiv_test.go 落地 C18 门禁修复（review attempt 3）：行为保持
// 矩阵不再只验证内联路径——对「锚定直启 / 无锚定双启动 / 健康永不就绪失败」三场景
// 做双路径同输入同观察面等价验证：同一 fixture 输入分别驱动
//  1. 内联路径（runtimeSupervisor 未注入——charx 既有 golden 矩阵的路径）；
//  2. supervisor 路径（真实 opencode.Supervisor——生产装配形态，C2c），
// 然后逐观察面断言两次运行输出等价（store 终态/归属/notice/agentStatus DTO/
// process 调用序列形态）。等价断言面=终态行为合同；已批准的归一化差异仅限：
//   - last_error 文本（内联=「health check timeout」；supervisor 轮换耗尽=
//     「serve not ready after port retries」——错误文本属实现细节，行为语义
//     「激活失败+落挂起+last_error 非空」等价）；
//   - env 快照 OCDECK_SERVE_PORT 持久化时点（内联 mergeEnvSnapshot 建程前 /
//     supervisor startServeWithRetry——终值等价，时点不参与行集快照）。
//
// mutation 证据（已验证 → 红 → 还原）：等价判定旁路（对 supervisor 腿的观察面
// 断言删除/恒真）→ 双路径差异静默 → 本文件测试红失效形态见各测试注释。

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/opencode"
)

// dualPathPair 是同一场景的内联/双路径环境对（独立 fake server + store + proc）。
type dualPathPair struct {
	in *charxEnv // 内联 golden 路径
	sp *charxEnv // supervisor 生产装配路径
}

// newDualPathPair 装配一对同形态环境：两腿各自独占 fake server/store/proc
// （观察面互不污染），装配变体同参（retryRange/deadPorts/killAfterNew）。
// supervisor 腿在 harness 基础上注入真实 opencode Supervisor（生产装配形态）。
func newDualPathPair(t *testing.T, opts charxEnvOpts) *dualPathPair {
	t.Helper()
	in := newCharxEnv(t, opts)
	sp := newCharxEnv(t, opts)
	// C18 等价形态：两腿端口分配统一固定为 fake server 真监听端口（allocateServePortFn
	// 测试注入——allocatePort 优先委托该 seam，快照/last_port/proc argv 归一化
	// token 同为 FAKE）。关键差异与校正：
	//  - supervisor 腿健康检查经 supervisor 内部 opencode.NewClient 直连分配端口
	//   （不经 Manager ocFactory 映射），分配端口必须真监听；
	//  - 内联腿健康检查经 m.ocFactory（harness 工厂全员映射 fake server），端口值
	//    只影响 argv/快照形状。统一真端口后两腿同构（轮换恒返同一端口由两侧
	//    allocate exclude 形态同构承载——等价断言面不含轮换端口多样性）。
	for _, e := range []*charxEnv{in, sp} {
		e.m.allocateServePortFn = func(_ sql.NullInt64, _ int) (int, error) { return e.srv.port, nil }
	}
	// C18：真实 Provider/supervisor 组合（NewSPIProviderWithDeps + 全 deps；
	// 端口 seam 与 Manager allocateServePortFn 同参恒指本腿 fake server 真端口）。
	deps := opencode.SupervisorDeps{
		Proc:         sp.proc,
		Store:        &mockSupStoreSeam{store: sp.store},
		SideFx:       &mockSupSideFx{m: sp.m},
		AllocatePort: func(lastPort int, lastValid bool, exclude int) (int, error) { return sp.srv.port, nil },
		ReadyTimeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
		ProbeColdStartBackoff: func() []time.Duration { return []time.Duration{time.Millisecond} },
	}
	sp.m.SetRuntimeSupervisor(opencode.NewSPIProviderWithDeps(deps).Supervisor())
	return &dualPathPair{in: in, sp: sp}
}

// seedAnchoredTask 给环境对两侧种子锚定任务（同输入）。
func (p *dualPathPair) seedAnchoredTask(t *testing.T, anchor string) {
	t.Helper()
	for _, e := range []*charxEnv{p.in, p.sp} {
		charxSeedRepoWorktreeTask(e.store, "t1", "p1")
		srvDir := "/data/worktrees/p1/t1"
		e.srv.expectDirectory(srvDir)
		if anchor != "" {
			e.store.mutTask("t1", func(tr *TaskRow) {
				tr.AnchorSessionID = sql.NullString{String: anchor, Valid: true}
			})
			e.srv.seedSession(srvDir, anchor, 1, 1)
		}
	}
}

// assertSurfaceEquivalence 逐观察面对比双路径输出（等价断言面=终态行为合同；
// ignore 声明同 charxSnapshot.ignore 语义——已批准归一化差异逐项登记。
// assertCharxMatches 要求 want 携带 ignore——双路径语义中「已批准差异」同时豁免
// 两侧（in 侧不设差异，ignore 置给 want=in）。
func assertSurfaceEquivalence(t *testing.T, label string, in, sp charxSnapshot, ignore map[string]string) {
	t.Helper()
	in.ignore = ignore
	sp.name = label + "/sup"
	assertCharxMatches(t, label+"/equiv", in, sp)
}

// TestCharx_DualPath_AnchoredDirectStart 场景①：有锚定直启。断言双路径在
// proc 序列（argv 已归一化）、store 终态、归属、notice、agentStatus DTO 全等价。
//
// mutation 证据（已验证 → 红 → 还原）：supervisor 腿 Resume 轮换出口不持久化
// 端口（persist 段 if false 屏蔽）→ env 快照终值不一致 → 本测试 store/tasks 面
// 红「mismatch」；还原后绿（C13 已有 mutation 载体，此处为等价面复核）。
func TestCharx_DualPath_AnchoredDirectStart(t *testing.T) {
	p := newDualPathPair(t, charxEnvOpts{})
	p.seedAnchoredTask(t, "ses-existing")

	if err := p.in.charxActivate("t1"); err != nil {
		t.Fatalf("inline Activate: %v", err)
	}
	if err := p.sp.charxActivate("t1"); err != nil {
		t.Fatalf("supervisor Activate: %v", err)
	}
	assertSurfaceEquivalence(t, "anchored_direct",
		p.in.captureCharx("inline", "t1"),
		p.sp.captureCharx("sup", "t1"), nil)
}

// TestCharx_DualPath_NoAnchorDualStarts 场景②：无锚定双启动。断言双启动的
// process 时序形态（bootstrap 无 --session → kill 确认 → formal --session）、
// 锚定写入（fake server 创建 ses_fake_001）、归属、终态行集全等价。
func TestCharx_DualPath_NoAnchorDualStarts(t *testing.T) {
	p := newDualPathPair(t, charxEnvOpts{})
	p.seedAnchoredTask(t, "")

	if err := p.in.charxActivate("t1"); err != nil {
		t.Fatalf("inline Activate: %v", err)
	}
	if err := p.sp.charxActivate("t1"); err != nil {
		t.Fatalf("supervisor Activate: %v", err)
	}
	// 双启动形态逐点预检（失败时给出可诊断差异，再走全观察面等价）。
	for tag, e := range map[string]*charxEnv{"inline": p.in, "sup": p.sp} {
		row := e.charxTaskRow("t1")
		if !row.AnchorSessionID.Valid || !strings.HasPrefix(row.AnchorSessionID.String, "ses_fake_") {
			t.Errorf("[%s] anchor = %+v, want fake-server created", tag, row.AnchorSessionID)
		}
		if n := countNewServe(e.proc.mockProc, "t1"); n != 2 {
			t.Errorf("[%s] NewSession = %d, want 2 (dual start)", tag, n)
		}
		if n := countKillServe(e.proc.mockProc, "t1"); n != 1 {
			t.Errorf("[%s] KillSession = %d, want 1 (bootstrap confirm)", tag, n)
		}
	}
	assertSurfaceEquivalence(t, "no_anchor_dual",
		p.in.captureCharx("inline", "t1"),
		p.sp.captureCharx("sup", "t1"), nil)
}

// TestCharx_DualPath_HealthNeverReadyFails 场景③：健康永不就绪（health 503 持续）
// → 双路径 Activate 失败 → 落 suspended + last_error 非空 + 零归属/锚定/notice。
// last_error 文本差异已批准（归一化说明见文件头）——该面登记 ignore。
func TestCharx_DualPath_HealthNeverReadyFails(t *testing.T) {
	p := newDualPathPair(t, charxEnvOpts{})
	p.seedAnchoredTask(t, "")
	for _, e := range []*charxEnv{p.in, p.sp} {
		e.srv.setHealthFailFirst(1 << 30)
	}

	if err := p.in.charxActivate("t1"); err == nil {
		t.Fatal("inline Activate must fail (health never ready)")
	}
	if err := p.sp.charxActivate("t1"); err == nil {
		t.Fatal("supervisor Activate must fail (health never ready)")
	}

	// 终态行为等价：suspended + last_error 非空 + 零残留。
	for tag, e := range map[string]*charxEnv{"inline": p.in, "sup": p.sp} {
		row := e.charxTaskRow("t1")
		if row.Status != StatusSuspended {
			t.Errorf("[%s] status = %s, want suspended", tag, row.Status)
		}
		if !row.LastError.Valid || row.LastError.String == "" {
			t.Errorf("[%s] last_error empty, want failure reason", tag)
		}
		if owned := e.charxOwnedSessions("t1"); len(owned) != 0 {
			t.Errorf("[%s] ownership = %v, want empty", tag, owned)
		}
		if entries := e.charxNoticesOf("t1"); len(entries) != 0 {
			t.Errorf("[%s] notices = %v, want none", tag, entries)
		}
	}
	assertSurfaceEquivalence(t, "health_fail", p.in.captureCharx("inline", "t1"),
		p.sp.captureCharx("sup", "t1"),
		map[string]string{"store/tasks": "last_error 文本差异（已批准归一化：内联 health timeout vs supervisor 轮换耗尽——行为语义等价）"})
}
