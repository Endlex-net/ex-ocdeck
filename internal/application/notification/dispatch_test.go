// dispatch_test.go 发送前门禁序列、episode 名额仲裁、DispatchPlan 固化与多渠道
// 并行投递（spec「发送前门禁与投递原子性」「通知渠道投递与降级」；design D4）。
package notification

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/domain/notification"
)

// dispatchFixture 两渠道装置：bark（CapGroup）+ web（CapReplace，无 Group）。
func dispatchFixture(t *testing.T, runStatus string) (*Notifier, *fakeTasks, *fakeCfgStore, *fakeChannel, *fakeChannel, *fakeClock) {
	t.Helper()
	ft := newFakeTasks(activeSnap("t1", "构建服务", runStatus))
	fc := &fakeCfgStore{cfg: testConfig()}
	bark := &fakeChannel{name: "bark", caps: notification.CapGroup}
	web := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc,
		[]notification.Channel{bark, web},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	return n, ft, fc, bark, web, clk
}

// TestDispatch_ParallelChannelsUniformTitle 门禁通过：全部已启用已配置渠道
// 并行收到投递；Title 由内容组装统一携带任务名，所有渠道（含无 CapGroup 的
// web）原样收到相同 Title——dispatch 层无标题降级（spec「通知渠道投递与降级」：
// 分组缺失仅表现为通知中心不折叠）。
func TestDispatch_ParallelChannelsUniformTitle(t *testing.T) {
	n, _, _, bark, web, clk := dispatchFixture(t, "idle")
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	for _, ch := range []*fakeChannel{bark, web} {
		if got := ch.callCount(); got != 1 {
			t.Fatalf("channel %s calls = %d, want 1", ch.name, got)
		}
	}
	if got := bark.sent()[0].Title; got != "OC [idle] 构建服务" {
		t.Fatalf("CapGroup channel title = %q", got)
	}
	if got := web.sent()[0].Title; got != bark.sent()[0].Title {
		t.Fatalf("no-Group channel must receive identical title, bark=%q web=%q", bark.sent()[0].Title, got)
	}
	// bark 固化 endpoint/token（来自配置快照）。
	if got := bark.configs[0]; got.Endpoint != "https://api.day.app" || got.Token != "bark-token-123456" {
		t.Fatalf("bark fixed config = %+v", got)
	}
	// web 意图 Body 不受前缀影响，URL 为任务页深链。
	if in := web.sent()[0]; in.Body != "已空闲超过 60 秒" || in.URL != "http://127.0.0.1:7777/#/task/t1" {
		t.Fatalf("web intent = %+v", in)
	}
}

// TestDispatch_SingleChannelFailureIsolated 单渠道失败不影响其他渠道投递，
// 全渠道失败不自动重试（仍计已消费）。
func TestDispatch_SingleChannelFailureIsolated(t *testing.T) {
	n, _, _, bark, web, clk := dispatchFixture(t, "idle")
	bark.fail = true
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if bark.callCount() != 1 || web.callCount() != 1 {
		t.Fatalf("both channels must be attempted once: bark=%d web=%d", bark.callCount(), web.callCount())
	}
	// 不重试：再扫描若干周期无新增调用。
	clk.add(5 * time.Minute)
	n.scan(ctx)
	n.scan(ctx)
	waitDispatch(n)
	if bark.callCount() != 1 || web.callCount() != 1 {
		t.Fatalf("no auto retry allowed: bark=%d web=%d", bark.callCount(), web.callCount())
	}
}

// TestArbitration_NoQuotaOnSwitchOff 总开关/类别开关关闭：不投递且不占 episode
// 名额（另一类别本 episode 仍可投递）。
func TestArbitration_NoQuotaOnSwitchOff(t *testing.T) {
	ctx := context.Background()

	// 类别开关关闭（error off）：error 届满不投递不占名额 → retry 同 tick 仍投递。
	n, ft, fc, ch, _, clk := dispatchFixture(t, "retry")
	snap := activeSnap("t1", "构建服务", "retry")
	snap.HasRetryDetail = true
	snap.RetryDetail = RetryDetail{Attempt: 1, Message: "m"}
	ft.set(snap)
	cfg := testConfig()
	cfg.Categories.Error = false
	fc.set(cfg)
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "retry", true)) // retry t0+60
	clk.add(5 * time.Second)
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil)) // error t0+65
	clk.add(65 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Category != notification.CategoryRetry {
		t.Fatalf("retry must still deliver when error category is off, got %+v", sent)
	}

	// 总开关关闭：error 届满不投递不占名额；恢复总开关后同 episode 的 retry
	//（重新武装计时）仍可投递。
	n2, ft2, fc2, ch2, _, clk2 := dispatchFixture(t, "retry")
	ft2.set(activeSnap("t1", "构建服务", "retry"))
	cfg2 := testConfig()
	cfg2.Enabled = false
	fc2.set(cfg2)
	n2.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil))
	clk2.add(60 * time.Second)
	n2.scan(ctx)
	waitDispatch(n2)
	if got := len(ch2.sent()); got != 0 {
		t.Fatalf("master switch off must not deliver, sends = %d", got)
	}
	if st := n2.states["t1"]; st == nil || st.episodeConsumed {
		t.Fatal("master-off gate failure must NOT consume episode quota")
	}
	// 总开关恢复 + 同 episode 新 retry 计时 → 投递（名额未被占用）。
	cfg2.Enabled = true
	fc2.set(cfg2)
	n2.handleEvent(ctx, runStatusEvent("t1", "idle", "retry", true))
	clk2.add(60 * time.Second)
	n2.scan(ctx)
	waitDispatch(n2)
	if got := len(ch2.sent()); got != 1 {
		t.Fatalf("retry must deliver after master re-enabled (quota not taken), sends = %d", got)
	}
}

// TestArbitration_QuotaOnURLUnavailable URL 不可用复验失败：占 episode 名额
// （另一类别本 episode 不再投递）。
func TestArbitration_QuotaOnURLUnavailable(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "构建服务", "retry"))
	snap := activeSnap("t1", "构建服务", "retry")
	snap.HasRetryDetail = true
	snap.RetryDetail = RetryDetail{Attempt: 1, Message: "m"}
	ft.set(snap)
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
		func(string) (string, error) { return "", errNotFound }, clk)
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "busy", "retry", true)) // retry t0+60
	clk.add(5 * time.Second)
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil)) // error t0+65（error 优先）
	clk.add(65 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("url unavailable must not deliver, sends = %d", got)
	}
	if st := n.states["t1"]; st == nil || !st.episodeConsumed {
		t.Fatal("URL-unavailable gate failure MUST consume episode quota")
	}
	// 名额已占：同 episode 内 retry 计时已消费/抑制，后续扫描不再投递。
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("quota consumed by URL failure must suppress retry in same episode, sends = %d", got)
	}
}

// TestArbitration_QuotaOnNoChannel 无启用且已配置渠道：占 episode 名额。
func TestArbitration_QuotaOnNoChannel(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fc := &fakeCfgStore{cfg: testConfig()}
	// bark 启用但 token/endpoint 为空（未配置）；web/macos 关闭 → 无已配置渠道。
	cfg := testConfig()
	cfg.Channels.Web.Enabled = false
	cfg.Channels.Bark.Token = ""
	cfg.Channels.Bark.Endpoint = ""
	fc.set(cfg)
	ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://x", nil }, clk)
	ctx := context.Background()

	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("no configured channel must not deliver, sends = %d", got)
	}
	if st := n.states["t1"]; st == nil || !st.episodeConsumed {
		t.Fatal("no-channel gate failure MUST consume episode quota")
	}
}

// TestArbitration_ConditionFailNoQuota 触发条件失效（复验时聚合已回 busy）：
// 不投递不占名额、不调用渠道（spec「投递前状态已恢复」）。
func TestArbitration_ConditionFailNoQuota(t *testing.T) {
	n, ft, _, ch, _, clk := dispatchFixture(t, "idle")
	ctx := context.Background()
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil))
	// 事件滞后：error 计时届满前聚合已回 busy（快照视角），run_status 事件未达。
	ft.set(activeSnap("t1", "构建服务", "busy"))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("recovered condition must not deliver, sends = %d", got)
	}
	if st := n.states["t1"]; st == nil || st.episodeConsumed {
		t.Fatal("condition-failed gate must NOT consume episode quota")
	}
}

// TestConfig_ReadPerCandidateAttentionBatch B6：同一 attention 批次的每个
// pending request 各读一次配置快照——第一候选判定后保存的关闭配置不影响其
// 已开始的投递，但即时约束第二候选。
func TestConfig_ReadPerCandidateAttentionBatch(t *testing.T) {
	n, ft, fc, bark, _, _ := dispatchFixture(t, "idle")
	snap := attentionSnapWith("idle",
		[]application.PendingQuestion{pendingQuestion("q1", "第一个"), pendingQuestion("q2", "第二个")}, nil)
	ft.set(snap)
	// 配置序列：第一候选 enabled，第二候选 disabled（按调用次序弹出）。
	on, off := testConfig(), testConfig()
	off.Enabled = false
	fc.seq = []notification.Config{on, off}
	ctx := context.Background()

	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	sent := bark.sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Body, "第一个") {
		t.Fatalf("first candidate must deliver under its own snapshot, got %+v", sent)
	}
	if bark.callCount() != 1 {
		t.Fatalf("second candidate must be gated by fresh config read, calls = %d", bark.callCount())
	}
}

// TestConfig_ReadPerCandidateTick B6：同一 tick 内多个到期候选各读一次配置。
func TestConfig_ReadPerCandidateTick(t *testing.T) {
	s1 := activeSnap("t1", "任务一", "idle")
	s2 := activeSnap("t2", "任务二", "idle")
	ft := newFakeTasks(s1, s2)
	fl := &fakeLister{ids: []string{"t1", "t2"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	n := newTestNotifier(ft, fl, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	n.handleEvent(ctx, runStatusEvent("t2", "busy", "idle", true))
	clk.add(60 * time.Second)
	on, off := testConfig(), testConfig()
	off.Enabled = false
	fc.seq = []notification.Config{on, off} // t1（字典序先判定）投递、t2 被关
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || sent[0].TaskID != "t1" {
		t.Fatalf("only first due candidate must deliver under per-candidate config, got %+v", sent)
	}
}

// TestDispatch_PlanImmutable B9 补齐：断言明确期望值（URL/LLM 开关/渠道身份
// 集合/bark endpoint+token），非自证比较；并断言下一候选的 plan 使用新全量配置。
// LLM 阻塞期间并发 PUT 的集成断言随 Lane E 5.2 交付。
func TestDispatch_PlanImmutable(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fc := &fakeCfgStore{cfg: testConfig()}
	bark := &fakeChannel{name: "bark", caps: notification.CapGroup}
	web := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc,
		[]notification.Channel{bark, web},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)

	// 候选判定的配置快照（LLM 开=true 使断言面有效）。
	cfgOn := testConfig()
	cfgOn.LLMSummary = true
	e := n.evaluate("t1", notification.CategoryIdle, cfgOn, activeSnap("t1", "构建服务", "idle"), func(TaskSnapshot) bool { return true })
	if e.stage != gatePass || e.plan == nil {
		t.Fatalf("prereq gate pass: %+v", e)
	}
	plan := e.plan
	// 期望值逐项断言。
	if plan.URL != "http://127.0.0.1:7777/#/task/t1" {
		t.Fatalf("plan.URL = %q", plan.URL)
	}
	if !plan.LLMSummary {
		t.Fatal("plan.LLMSummary must mirror candidate config snapshot")
	}
	if len(plan.Channels) != 2 || plan.Channels[0].Channel != bark || plan.Channels[1].Channel != web {
		t.Fatalf("plan channels identity = %+v", plan.Channels)
	}
	if got := plan.Channels[0].Config; got != (notification.ChannelConfig{Endpoint: "https://api.day.app", Token: "bark-token-123456"}) {
		t.Fatalf("bark fixed config = %+v", got)
	}
	if got := plan.Channels[1].Config; got != (notification.ChannelConfig{}) {
		t.Fatalf("web must have no config fields, got %+v", got)
	}

	// 下一候选：全量变更后的新配置生成新 plan（base_url 覆盖、LLM 关、bark 关
	// → 仅 web、渠道集合即时收缩）。
	mutated := testConfig()
	mutated.BaseURL = "https://elsewhere.example.com"
	mutated.LLMSummary = false
	mutated.Channels.Bark.Enabled = false
	fc.set(mutated)
	e2 := n.evaluate("t1", notification.CategoryIdle, fc.Config(), activeSnap("t1", "构建服务", "idle"), func(TaskSnapshot) bool { return true })
	if e2.stage != gatePass || e2.plan == nil {
		t.Fatalf("next candidate gate pass: %+v", e2.stage)
	}
	if e2.plan.URL != "https://elsewhere.example.com/#/task/t1" {
		t.Fatalf("next plan.URL = %q", e2.plan.URL)
	}
	if e2.plan.LLMSummary {
		t.Fatal("next plan.LLMSummary must follow new config")
	}
	if len(e2.plan.Channels) != 1 || e2.plan.Channels[0].Channel != web {
		t.Fatalf("next plan channels = %+v", e2.plan.Channels)
	}
	// 原计划不受影响（不可变）。
	if plan.URL != "http://127.0.0.1:7777/#/task/t1" || len(plan.Channels) != 2 || !plan.LLMSummary {
		t.Fatalf("original plan must stay immutable: %+v", plan)
	}
}

// TestDispatch_PlanFixatedDuringInFlight 单次投递全过程使用同一份配置快照：
// 投递阻塞期间配置变更不影响本次（spec「投递期间配置变更不影响本次」）；
// 新配置从下一次投递起生效。
func TestDispatch_PlanFixatedDuringInFlight(t *testing.T) {
	n, ft, fc, bark, _, clk := dispatchFixture(t, "idle")
	block := make(chan struct{})
	bark.block = block
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx) // dispatch 已启动并阻塞在 bark.Send（计划内 token 已固化）

	// 在途投递期间保存新配置（token 更新 + base_url 覆盖）。
	newCfg := testConfig()
	newCfg.Channels.Bark.Token = "new-token-999999"
	newCfg.BaseURL = "https://example.com"
	fc.set(newCfg)
	close(block) // 放行在途投递
	waitDispatch(n)

	if got := bark.configs[0].Token; got != "bark-token-123456" {
		t.Fatalf("in-flight delivery must use plan-fixated token, got %q", got)
	}
	if got := bark.sent()[0].URL; got != "http://127.0.0.1:7777/#/task/t1" {
		t.Fatalf("in-flight delivery must use plan-fixated url, got %q", got)
	}

	// 下一次投递（重新武装 idle）使用新配置快照。
	ft.set(activeSnap("t1", "构建服务", "idle"))
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := bark.configs[1].Token; got != "new-token-999999" {
		t.Fatalf("next delivery must use new token, got %q", got)
	}
	if got := bark.sent()[1].URL; got != "https://example.com/#/task/t1" {
		t.Fatalf("next delivery must use new base url, got %q", got)
	}
}

// --- 测试通知投递（spec「测试通知」；与真实 dispatch 共享解析/降级/并行投递） ---

func resultByName(results []notification.ChannelResult, name string) notification.ChannelResult {
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	return notification.ChannelResult{}
}

// TestSendTestNotification_SkippedMatrix skipped 矩阵：web 未启用、bark 缺 token、
// macos 不可用 → status=skipped、Error 空、零次 Send。
func TestSendTestNotification_SkippedMatrix(t *testing.T) {
	web := &fakeChannel{name: "web", caps: notification.CapReplace}
	bark := &fakeChannel{name: "bark", caps: notification.CapGroup}
	macos := &fakeChannel{name: "macos", caps: 0, unavail: true}
	n := New(Options{Channels: []notification.Channel{web, bark, macos}})

	cfg := testConfig()
	cfg.Channels.Web.Enabled = false
	cfg.Channels.Bark.Token = ""
	cfg.Channels.Macos.Enabled = true

	got := n.SendTestNotification(context.Background(), cfg, "http://127.0.0.1:9")
	if len(got) != 3 {
		t.Fatalf("results len = %d, want 3 (all injected channels reported)", len(got))
	}
	for _, name := range []string{"web", "bark", "macos"} {
		r := resultByName(got, name)
		if r.Status != notification.ChannelStatusSkipped || r.Error != "" {
			t.Fatalf("%s = %+v, want skipped with empty error", name, r)
		}
	}
	if web.callCount() != 0 || bark.callCount() != 0 || macos.callCount() != 0 {
		t.Fatalf("skipped channels must not Send: web=%d bark=%d macos=%d",
			web.callCount(), bark.callCount(), macos.callCount())
	}
}

// TestSendTestNotification_SuccessFailedIntent 已配置渠道并行投递：success/failed
// 判定与真实 Send 一致；Title 由 TestIntent 统一组装，各渠道原样收到相同值。
func TestSendTestNotification_SuccessFailedPrefix(t *testing.T) {
	web := &fakeChannel{name: "web", caps: notification.CapReplace}
	bark := &fakeChannel{name: "bark", caps: notification.CapGroup, fail: true}
	macos := &fakeChannel{name: "macos", caps: notification.CapGroup | notification.CapReplace}
	n := New(Options{Channels: []notification.Channel{web, bark, macos}})

	cfg := testConfig()
	cfg.Channels.Macos.Enabled = true

	got := n.SendTestNotification(context.Background(), cfg, "http://127.0.0.1:9")
	if r := resultByName(got, "web"); r.Status != notification.ChannelStatusSuccess || r.Error != "" {
		t.Fatalf("web = %+v", r)
	}
	if r := resultByName(got, "bark"); r.Status != notification.ChannelStatusFailed || r.Error != "scripted failure" {
		t.Fatalf("bark = %+v", r)
	}
	if r := resultByName(got, "macos"); r.Status != notification.ChannelStatusSuccess || r.Error != "" {
		t.Fatalf("macos = %+v", r)
	}

	if web.callCount() != 1 || bark.callCount() != 1 || macos.callCount() != 1 {
		t.Fatalf("enabled channels must Send once: web=%d bark=%d macos=%d",
			web.callCount(), bark.callCount(), macos.callCount())
	}
	if got := web.sent()[0].Title; got != "OC [test] ocdeck" {
		t.Fatalf("no-Group title = %q, want OC [test] ocdeck", got)
	}
	if got := bark.sent()[0].Title; got != web.sent()[0].Title {
		t.Fatalf("CapGroup channel must receive identical title, bark=%q web=%q", got, web.sent()[0].Title)
	}
	if in := web.sent()[0]; in.TaskID != "notification-test" || in.Category != notification.CategoryTest ||
		in.Level != notification.LevelActive || in.URL != "http://127.0.0.1:9/#/configs#notifications" ||
		in.Body != "ocdeck 通知链路测试" {
		t.Fatalf("test intent = %+v", in)
	}
	if got := bark.configs[0]; got != (notification.ChannelConfig{Endpoint: "https://api.day.app", Token: "bark-token-123456"}) {
		t.Fatalf("bark config = %+v", got)
	}
}

// TestSendTestNotification_NoLLM test 路径 MUST NOT 调用 LLM（spec 豁免），
// 即使 cfg.LLMSummary=true 且 Completer 已装配。
func TestSendTestNotification_NoLLM(t *testing.T) {
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	fc := &fakeCompleter{result: "不应出现"}
	n := New(Options{Channels: []notification.Channel{ch}, Summarizer: fc})
	cfg := testConfig()
	cfg.LLMSummary = true

	n.SendTestNotification(context.Background(), cfg, "http://x")
	if fc.callCount() != 0 {
		t.Fatalf("test notification must not call LLM, calls = %d", fc.callCount())
	}
	if got := ch.sent()[0].Body; strings.Contains(got, "AI 总结") {
		t.Fatalf("body must stay deterministic, got %q", got)
	}
}

// TestSendTestNotification_MasterOffStillDelivers 总开关由调用方（api）拦截；
// 本方法只负责投递，Enabled=false 仍走渠道解析。
func TestSendTestNotification_MasterOffStillDelivers(t *testing.T) {
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	n := New(Options{Channels: []notification.Channel{ch}})
	cfg := testConfig()
	cfg.Enabled = false

	got := n.SendTestNotification(context.Background(), cfg, "http://x")
	if r := resultByName(got, "web"); r.Status != notification.ChannelStatusSuccess {
		t.Fatalf("master-off is caller's 422, method still delivers: %+v", r)
	}
}

// TestDispatch_MacosUnavailableSkipped 真实 dispatch 与测试路径统一：macos
// 不可用按 skipped 不纳入发送（不再由适配器 failed）。
func TestDispatch_MacosUnavailableSkipped(t *testing.T) {
	macos := &fakeChannel{name: "macos", caps: 0, unavail: true}
	web := &fakeChannel{name: "web", caps: notification.CapReplace}
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fc := &fakeCfgStore{cfg: testConfig()}
	cfg := testConfig()
	cfg.Channels.Bark.Enabled = false
	cfg.Channels.Macos.Enabled = true
	fc.set(cfg)
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc,
		[]notification.Channel{macos, web},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)

	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if macos.callCount() != 0 {
		t.Fatalf("unavailable macos must be skipped, not sent: calls=%d", macos.callCount())
	}
	if web.callCount() != 1 {
		t.Fatalf("web must still deliver, calls=%d", web.callCount())
	}

	// 仅不可用 macos：真实路径 gateNoChannel（占名额、零投递）。
	n2 := newTestNotifier(newFakeTasks(activeSnap("t1", "构建服务", "idle")),
		&fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{macos},
		func(string) (string, error) { return "http://x", nil }, newFakeClock())
	e := n2.evaluate("t1", notification.CategoryIdle, cfg, activeSnap("t1", "构建服务", "idle"),
		func(TaskSnapshot) bool { return true })
	if e.stage != gateNoChannel {
		t.Fatalf("unavailable macos only → gateNoChannel, got %v", e.stage)
	}

	// 同一配置下测试路径报告 skipped，与真实解析共享（不 Send）。
	testGot := n2.SendTestNotification(context.Background(), cfg, "http://x")
	if r := resultByName(testGot, "macos"); r.Status != notification.ChannelStatusSkipped {
		t.Fatalf("test path macos = %+v", r)
	}
	if macos.callCount() != 0 {
		t.Fatalf("shared skip must not Send macos, calls=%d", macos.callCount())
	}
}

// TestDispatch_WecomEnabledButURLEmptySkipped wecom 开关开但 url 空 → 未配置，
// skipped 且零 Send（spec「企业微信渠道」未配置判定）。
func TestDispatch_WecomEnabledButURLEmptySkipped(t *testing.T) {
	wecom := &fakeChannel{name: "wecom", caps: 0}
	web := &fakeChannel{name: "web", caps: notification.CapReplace}
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fc := &fakeCfgStore{cfg: testConfig()}
	cfg := testConfig()
	cfg.Channels.Wecom.Enabled = true
	cfg.Channels.Wecom.URL = ""
	fc.set(cfg)
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc,
		[]notification.Channel{wecom, web},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)

	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if wecom.callCount() != 0 {
		t.Fatalf("wecom enabled but url empty must be skipped, calls=%d", wecom.callCount())
	}
	if web.callCount() != 1 {
		t.Fatalf("web must still deliver, calls=%d", web.callCount())
	}
}

// TestDispatch_WecomAndWebBothDeliver wecom 与 web 同时启用且已配置 → 各自投递。
func TestDispatch_WecomAndWebBothDeliver(t *testing.T) {
	wecom := &fakeChannel{name: "wecom", caps: 0}
	web := &fakeChannel{name: "web", caps: notification.CapReplace}
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fc := &fakeCfgStore{cfg: testConfig()}
	cfg := testConfig()
	cfg.Channels.Bark.Enabled = false
	cfg.Channels.Wecom.Enabled = true
	cfg.Channels.Wecom.URL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test"
	fc.set(cfg)
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc,
		[]notification.Channel{wecom, web},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)

	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if wecom.callCount() != 1 || web.callCount() != 1 {
		t.Fatalf("both wecom and web must deliver once: wecom=%d web=%d", wecom.callCount(), web.callCount())
	}
	if got := wecom.configs[0].Endpoint; got != "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test" {
		t.Fatalf("wecom endpoint must be the full webhook URL, got %q", got)
	}
	if got := wecom.configs[0].Token; got != "" {
		t.Fatalf("wecom token must be empty, got %q", got)
	}
}

// TestDispatch_WecomFailureLogOmitsSecrets 失败投递走 dispatch.go:168 的
// log.Printf；日志 MUST 含渠道失败前缀，MUST NOT 含 webhook URL、Intent.Body
// 或响应原文。fakeChannel 失败时 Err 为固定 "scripted failure"，Config.Endpoint
// 与 Intent.Body 含泄露标记——若生产日志误打印 plan/cfg/Intent 正文，本测试 MUST 失败。
func TestDispatch_WecomFailureLogOmitsSecrets(t *testing.T) {
	const (
		urlMarker      = "wecom-log-leak-key-xyz"
		bodyMarker     = "leak-check-body-marker"
		responseMarker = "raw-response-marker-xyz"
	)
	wecom := &fakeChannel{name: "wecom", caps: 0, fail: true}
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fc := &fakeCfgStore{cfg: testConfig()}
	cfg := testConfig()
	cfg.Channels.Bark.Enabled = false
	cfg.Channels.Web.Enabled = false
	cfg.Channels.Wecom.Enabled = true
	cfg.Channels.Wecom.URL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + urlMarker
	fc.set(cfg)
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc,
		[]notification.Channel{wecom},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if wecom.callCount() != 1 {
		t.Fatalf("wecom must be attempted once, calls=%d", wecom.callCount())
	}
	sent := wecom.sent()[0]
	endpoint := wecom.configs[0].Endpoint
	if !strings.Contains(endpoint, urlMarker) {
		t.Fatalf("prereq: Send must receive webhook URL containing marker, got %q", endpoint)
	}
	if sent.Body == "" {
		t.Fatal("prereq: sent Intent.Body must be non-empty")
	}
	logged := buf.String()
	if !strings.Contains(logged, "notify: channel wecom failed") {
		t.Fatalf("expected dispatch failure log, got %q", logged)
	}
	for _, secret := range []string{urlMarker, bodyMarker, responseMarker, endpoint, sent.Body, sent.URL} {
		if secret != "" && strings.Contains(logged, secret) {
			t.Fatalf("dispatch log MUST NOT contain %q: %q", secret, logged)
		}
	}
}
