// triggers_test.go 五类触发状态机与 episode 仲裁（spec「通知触发」×5、「发送前
// 门禁与投递原子性」；design D3 状态机）。engine 直测：fake clock 推进、
// handleEvent/scan 串行调用，不睡真实时间。
package notification

import (
	"context"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/domain/notification"
)

// triggerFixture 标准直测装置：t1 active 任务 + bark 渠道 + 常驻 resolver。
func triggerFixture(t *testing.T, runStatus string) (*Notifier, *fakeTasks, *fakeCfgStore, *fakeChannel, *fakeClock) {
	t.Helper()
	ft := newFakeTasks(activeSnap("t1", "构建服务", runStatus))
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	return n, ft, fc, ch, clk
}

// attentionSnapWith 构造含 pending 集合的快照。
func attentionSnapWith(runStatus string, quests []application.PendingQuestion, perms []application.PendingPermission) TaskSnapshot {
	snap := activeSnap("t1", "构建服务", runStatus)
	snap.Attention.Questions = quests
	snap.Attention.Permissions = perms
	return snap
}

// --- question / permission ---

// TestTrigger_QuestionDedupAndResolve question 触发：新增通知、同 ID 不重复、
// 了结移除去重、之后新 ID 重新通知；与 permission 去重键独立（spec question
// requirement 全部 scenario）。
func TestTrigger_QuestionDedupAndResolve(t *testing.T) {
	n, ft, _, ch, _ := triggerFixture(t, "idle")
	ctx := context.Background()

	ft.set(attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q1", "用哪个分支？")}, nil))
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if sent := ch.sent(); len(sent) != 1 || sent[0].Category != notification.CategoryQuestion ||
		!strings.Contains(sent[0].Body, "用哪个分支？") {
		t.Fatalf("question notify = %+v", sent)
	}

	// 同 ID 仍 pending：重复刷新不重复通知。
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if len(ch.sent()) != 1 {
		t.Fatalf("same request id must not re-notify, sends = %d", len(ch.sent()))
	}

	// 了结（q1 消失）→ 从去重集合移除；新 q2 重新通知。
	ft.set(attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q2", "第二个问题")}, nil))
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if sent := ch.sent(); len(sent) != 2 || !strings.Contains(sent[1].Body, "第二个问题") {
		t.Fatalf("re-notify after resolve = %+v", sent)
	}
	if _, still := n.states["t1"].notifiedQuestions["q1"]; still {
		t.Fatal("resolved request id must be pruned from dedup set")
	}
}

// TestTrigger_QuestionPermissionIndependentKeys 同值 request ID 不跨类型抑制。
func TestTrigger_QuestionPermissionIndependentKeys(t *testing.T) {
	n, ft, _, ch, _ := triggerFixture(t, "idle")
	ctx := context.Background()

	ft.set(attentionSnapWith("idle",
		[]application.PendingQuestion{pendingQuestion("same-id", "问题内容")},
		[]application.PendingPermission{pendingPermission("same-id", "bash", "rm")}))
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 2 {
		t.Fatalf("question & permission with same request id must both notify, sends = %d", len(sent))
	}
	cats := map[notification.Category]int{}
	for _, in := range sent {
		cats[in.Category]++
	}
	if cats[notification.CategoryQuestion] != 1 || cats[notification.CategoryPermission] != 1 {
		t.Fatalf("categories = %v", cats)
	}
	for _, in := range sent {
		if in.Category == notification.CategoryPermission {
			if !strings.Contains(in.Body, "bash") || !strings.Contains(in.Body, "rm") {
				t.Fatalf("permission body must contain permission name and patterns, got %q", in.Body)
			}
		}
	}
}

// TestTrigger_AttentionReadFailureNoChange 快照读取失败按无变化处理，MUST NOT 误发。
func TestTrigger_AttentionReadFailureNoChange(t *testing.T) {
	n, ft, _, ch, _ := triggerFixture(t, "idle")
	ctx := context.Background()
	ft.setErr(func(string) error { return errNotFound })
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("read failure must not notify, sends = %d", len(ch.sent()))
	}
	if st := n.states["t1"]; st != nil && (len(st.notifiedQuestions) > 0 || len(st.notifiedPermissions) > 0) {
		t.Fatalf("dedup maps must stay unchanged on read failure: %+v", st)
	}
}

// --- idle ---

func TestTrigger_IdleTimeoutNotifyAndRearm(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	// busy→idle（available）武装；59s 未到、60s 届满通知（spec idle scenario）。
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(59 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("idle must not fire before threshold, sends = %d", len(ch.sent()))
	}
	clk.add(1 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Category != notification.CategoryIdle || sent[0].Body != "已空闲超过 60 秒" {
		t.Fatalf("idle notify = %+v", sent)
	}
	// 通知后抑制：持续 idle 不再触发。
	clk.add(10 * time.Minute)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 1 {
		t.Fatalf("idle must be suppressed after notify, sends = %d", len(ch.sent()))
	}
	// 新的 busy→idle 迁移重新武装（spec「通知后重新武装」）。
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 2 {
		t.Fatalf("re-arm must re-notify, sends = %d", len(ch.sent()))
	}
}

// TestTrigger_IdleNoArmFromUnavailableOrRetry from 为不可用态或 retry 的 idle 迁移
// MUST NOT 武装（spec idle requirement）。
func TestTrigger_IdleNoArmFromUnavailableOrRetry(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()
	for _, from := range []string{"", "retry"} {
		n.handleEvent(ctx, runStatusEvent("t1", from, "idle", true))
	}
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("idle must not arm from %q/\"\"/retry, sends = %d", "unavailable-or-retry", len(ch.sent()))
	}
}

// TestTrigger_IdleCancelConditions 武装后的取消条件：回到非 idle、不可用、pending
// 注意力、进入异常周期（spec idle 取消条件全集）。
func TestTrigger_IdleCancelConditions(t *testing.T) {
	cases := []struct {
		name   string
		cancel func(ctx context.Context, n *Notifier, ft *fakeTasks)
	}{
		{"back to busy", func(ctx context.Context, n *Notifier, _ *fakeTasks) {
			n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
		}},
		{"unavailable", func(ctx context.Context, n *Notifier, _ *fakeTasks) {
			n.handleEvent(ctx, runStatusEvent("t1", "idle", "", false))
		}},
		{"pending attention", func(ctx context.Context, n *Notifier, ft *fakeTasks) {
			ft.set(attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q9", "x")}, nil))
			n.handleEvent(ctx, attentionEvent("t1"))
		}},
		{"episode (retry)", func(ctx context.Context, n *Notifier, _ *fakeTasks) {
			n.handleEvent(ctx, runStatusEvent("t1", "idle", "retry", true))
		}},
		{"episode (error)", func(ctx context.Context, n *Notifier, ft *fakeTasks) {
			ft.set(activeSnap("t1", "构建服务", "idle"))
			n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, ft, _, ch, clk := triggerFixture(t, "idle")
			ctx := context.Background()
			n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
			tc.cancel(ctx, n, ft)
			clk.add(time.Hour)
			n.scan(ctx)
			waitDispatch(n)
			// pending attention 取消场景会先投递 question 通知，但绝无 idle 通知。
			for _, in := range ch.sent() {
				if in.Category == notification.CategoryIdle {
					t.Fatalf("idle must be canceled by %s", tc.name)
				}
			}
		})
	}
}

// TestTrigger_IdleThresholdHotUpdate 阈值热更新：缩短下一周期即可到期、延长顺延
// （spec idle「缩短阈值立即到期」「延长阈值顺延」）。
func TestTrigger_IdleThresholdHotUpdate(t *testing.T) {
	ctx := context.Background()

	// 缩短：300 → 60，armed 3 分钟 → 立即到期。
	n, _, fc, ch, clk := triggerFixture(t, "idle")
	cfg := testConfig()
	cfg.IdleTimeoutSeconds = 300
	fc.set(cfg)
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(3 * time.Minute)
	cfg.IdleTimeoutSeconds = 60
	fc.set(cfg)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 1 {
		t.Fatalf("shrunken threshold must fire immediately, sends = %d", len(ch.sent()))
	}

	// 延长：60 → 300，armed 50s → 顺延至 300s。
	n2, _, fc2, ch2, clk2 := triggerFixture(t, "idle")
	cfg2 := testConfig()
	cfg2.IdleTimeoutSeconds = 60
	fc2.set(cfg2)
	n2.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk2.add(50 * time.Second)
	cfg2.IdleTimeoutSeconds = 300
	fc2.set(cfg2)
	n2.scan(ctx)
	waitDispatch(n2)
	if len(ch2.sent()) != 0 {
		t.Fatalf("extended threshold must defer, sends = %d", len(ch2.sent()))
	}
	clk2.add(250 * time.Second) // 共 300s
	n2.scan(ctx)
	waitDispatch(n2)
	if len(ch2.sent()) != 1 {
		t.Fatalf("deferred idle must fire at idleSince+300s, sends = %d", len(ch2.sent()))
	}
}

// --- retry ---

func TestTrigger_RetryTimeoutNotify(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "retry")
	snap := activeSnap("t1", "构建服务", "retry")
	snap.HasRetryDetail = true
	snap.RetryDetail = RetryDetail{Attempt: 2, Message: "stub rate limit", Next: 1787710728671}
	ft.set(snap)
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "idle", "retry", true))
	clk.add(59 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("retry must not fire before 60s, sends = %d", len(ch.sent()))
	}
	clk.add(1 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Category != notification.CategoryRetry || sent[0].Body != "第 2 次重试：stub rate limit" {
		t.Fatalf("retry notify = %+v", sent)
	}
}

// TestTrigger_RetryRecoverBeforeTimeout 1 分钟内聚合回到 busy 不通知。
func TestTrigger_RetryRecoverBeforeTimeout(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "retry")
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "retry", true))
	clk.add(30 * time.Second)
	n.handleEvent(ctx, runStatusEvent("t1", "retry", "busy", true))
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("recovered retry must not notify, sends = %d", len(ch.sent()))
	}
}

// TestTrigger_RetryFallbackDetail retry 详情不可得 → 固定降级文案仍投递。
func TestTrigger_RetryFallbackDetail(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "retry")
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "retry", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Body, "任务持续处于重试状态") {
		t.Fatalf("fallback retry = %+v", sent)
	}
}

// TestTrigger_RetryLeavingRetryCancelsTimer retry→idle：触发条件失效计时取消，
// episode 仍存续（spec 仲裁表「触发条件失效」）。
func TestTrigger_RetryLeavingRetryCancelsTimer(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "retry")
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "retry", true))
	n.handleEvent(ctx, runStatusEvent("t1", "retry", "idle", true))
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("retry timer must be canceled on leaving retry, sends = %d", len(ch.sent()))
	}
	if st := n.states["t1"]; st == nil || st.retryDeadline != nil {
		t.Fatalf("retry deadline must be nil, state = %+v", st)
	}
}

// --- error ---

func TestTrigger_ErrorTimeoutNotify(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()
	code := 429
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "p17 stub rate limit", &code, nil))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Category != notification.CategoryError || sent[0].Level != notification.LevelTimeSensitive {
		t.Fatalf("error notify = %+v", sent)
	}
	if want := "p17 stub rate limit (HTTP 429)"; sent[0].Body != want {
		t.Fatalf("body = %q, want %q", sent[0].Body, want)
	}
}

// TestTrigger_ErrorBusyTransient 聚合已 busy 的瞬时错误不打开计时。
func TestTrigger_ErrorBusyTransient(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "busy")
	ctx := context.Background()
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "transient", nil, nil))
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("busy-time transient error must not notify, sends = %d", len(ch.sent()))
	}
	if st := n.states["t1"]; st != nil && st.errorDeadline != nil {
		t.Fatal("busy-time transient error must not open error timer")
	}
}

// TestTrigger_ErrorFastRecover 1 分钟内回到 busy 视为已恢复不通知。
func TestTrigger_ErrorFastRecover(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil))
	clk.add(30 * time.Second)
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("recovered error must not notify, sends = %d", len(ch.sent()))
	}
}

// TestTrigger_ErrorRepeatDoesNotExtend 重复 session.error 不延长首个计时起点，
// 仅更新最新详情（spec「重复错误不延长计时」）。
func TestTrigger_ErrorRepeatDoesNotExtend(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "first failure", nil, nil))
	clk.add(50 * time.Second)
	n.handleEvent(ctx, sessionErrorEvent("t1", "s2", "second failure", nil, nil))
	st := n.states["t1"]
	if st == nil || st.errorDeadline == nil {
		t.Fatal("error deadline must be armed")
	}
	if got := st.errorDeadline.Sub(clk.now()); got != 10*time.Second {
		t.Fatalf("deadline extends to %v after repeat, want original start (10s remaining)", got)
	}
	clk.add(10 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Body, "second failure") {
		t.Fatalf("latest detail must be used, got %+v", sent)
	}
}

// TestTrigger_ErrorAfterIdleStillFires 不可重试错误终止（isRetryable=false）、随后
// 聚合转 idle：error 仍触发且 idle 被抑制（spec「不可重试错误终止后仍触发」）。
func TestTrigger_ErrorAfterIdleStillFires(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "retry")
	ctx := context.Background()
	noRetry := false
	// 先武装 idle（busy→idle？—— 本场景从 retry 转入 idle：from=retry 不武装；
	// 改为直接验证 error 计时跨 idle 迁移存活）。
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "fatal", nil, &noRetry))
	n.handleEvent(ctx, runStatusEvent("t1", "retry", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Category != notification.CategoryError || !strings.Contains(sent[0].Body, "fatal") {
		t.Fatalf("error must fire after task went idle, got %+v", sent)
	}
}

// TestTrigger_ErrorSuppressesIdle episode 存续期间 idle 不武装（error 开启 episode）。
func TestTrigger_ErrorSuppressesIdle(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true)) // 武装 idle
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil))
	clk.add(2 * time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	for _, in := range ch.sent() {
		if in.Category == notification.CategoryIdle {
			t.Fatal("idle must not fire while episode active")
		}
	}
	if len(ch.sent()) != 1 || ch.sent()[0].Category != notification.CategoryError {
		t.Fatalf("only error should fire, got %+v", ch.sent())
	}
}

// --- episode 仲裁 ---

// TestEpisode_ErrorPriorityOverRetrySameTick 同 tick 两计时届满 error 优先
// （spec retry requirement），且同 episode 合计至多一次投递。
func TestEpisode_ErrorPriorityOverRetrySameTick(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "retry")
	snap := activeSnap("t1", "构建服务", "retry")
	snap.HasRetryDetail = true
	snap.RetryDetail = RetryDetail{Attempt: 1, Message: "m", Next: 0}
	ft.set(snap)
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "busy", "retry", true)) // retry deadline t0+60
	clk.add(5 * time.Second)
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil)) // error deadline t0+65
	clk.add(60 * time.Second)                                           // 两计时同 tick 届满
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Category != notification.CategoryError {
		t.Fatalf("error must take priority over retry in same tick, got %+v", sent)
	}
	if st := n.states["t1"]; !st.episodeConsumed {
		t.Fatal("episode quota must be consumed by the delivered error")
	}
}

// TestEpisode_ErrorNotifiedSuppressesRetry error 已通知 → retry 计时届满被抑制。
func TestEpisode_ErrorNotifiedSuppressesRetry(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "retry")
	ft.set(activeSnap("t1", "构建服务", "retry"))
	ctx := context.Background()

	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil)) // error t0+60
	clk.add(10 * time.Second)
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "retry", true)) // retry t0+70（同 episode）
	clk.add(70 * time.Second)                                       // 两计时都已过
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Category != notification.CategoryError {
		t.Fatalf("retry must be suppressed in consumed episode, got %+v", sent)
	}
}

// TestTrigger_ErrorSeenOncePerEpisode B1：episode 内首个 error 只武装一次——
// 即使门禁消费但未占名额（总开关关闭），重复 session.error 也不得重新武装；
// episode 结束（回 busy）复位后可再武装。
func TestTrigger_ErrorSeenOncePerEpisode(t *testing.T) {
	n, ft, fc, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	// 首个 error 武装 t0+60。
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "first", nil, nil))
	st := n.states["t1"]
	if st == nil || st.errorDeadline == nil {
		t.Fatal("first error must arm deadline")
	}

	// 总开关关闭：届满门禁失败、不占名额、计时消费。
	off := testConfig()
	off.Enabled = false
	fc.set(off)
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("master off must not deliver, sends = %d", got)
	}
	if st := n.states["t1"]; st.episodeConsumed {
		t.Fatal("master-off gate must NOT consume quota")
	}

	// B1 回归：重复 error 只更新 lastError，不重新武装。
	n.handleEvent(ctx, sessionErrorEvent("t1", "s2", "repeat", nil, nil))
	if st := n.states["t1"]; st.errorDeadline != nil {
		t.Fatal("repeat error must not re-arm deadline within episode")
	}
	// 恢复总开关后也不会再触发（无计时）。
	fc.set(testConfig())
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("no deadline must remain after consumed error, sends = %d", got)
	}

	// episode 结束复位：busy → 新 error 重新武装并可通知。
	ft.set(activeSnap("t1", "构建服务", "busy"))
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
	ft.set(activeSnap("t1", "构建服务", "idle"))
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "second episode", nil, nil))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Body, "second episode") {
		t.Fatalf("error must re-arm after episode end, got %+v", sent)
	}
}

// TestFencing_StaleInstVersionDropped B3：suspend→reactivate（实例切换）后，
// 旧 instVersion 的迟到事件 MUST NOT 武装新实例；当前实例事件正常生效。
func TestFencing_StaleInstVersionDropped(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "busy")
	ctx := context.Background()

	// reactivate：快照实例切换为 iv2-t1。
	staleSnap := activeSnap("t1", "构建服务", "idle")
	staleSnap.InstVersion = "iv2-t1"
	ft.set(staleSnap)

	// 旧实例事件：busy→idle（RID=iv-t1）→ 丢弃，不武装（无状态或未武装皆可）。
	n.handleEvent(ctx, ocdeckevent.NewServeRuntimeRunStatusChanged("iv-t1", "t1", "busy", "idle", true))
	if st := n.states["t1"]; st != nil && st.idleSince != nil {
		t.Fatal("stale instVersion event must not arm idle")
	}
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("stale event must not lead to delivery, sends = %d", got)
	}

	// 当前实例事件（RID=iv2-t1，与快照一致）→ 正常武装并通知。
	n.handleEvent(ctx, ocdeckevent.NewServeRuntimeRunStatusChanged("iv2-t1", "t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 || ch.sent()[0].Category != notification.CategoryIdle {
		t.Fatalf("current instVersion event must arm and notify, got %+v", ch.sent())
	}
}

// TestReconcile_PreservesErrorSeen B1 补齐：同实例非 busy 的存续 episode 对账
// 保留 errorSeen——开关关闭消费后发生 overflow，重复 error 不得再次武装；
// 换代或 busy 时清除。
func TestReconcile_PreservesErrorSeen(t *testing.T) {
	n, ft, fc, ch, clk := triggerFixture(t, "idle")
	fl := &fakeLister{ids: []string{"t1"}}
	n.opts.ListActive = fl
	ctx := context.Background()

	// 首个 error 武装；总开关关闭 → 届满消费不占名额。
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "first", nil, nil))
	off := testConfig()
	off.Enabled = false
	fc.set(off)
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("master off must not deliver, sends = %d", got)
	}

	// overflow 对账（快照仍 idle、同实例）→ errorSeen 保留：重复 error 不武装。
	n.onOverflow(ctx)
	n.handleEvent(ctx, sessionErrorEvent("t1", "s2", "repeat-after-reconcile", nil, nil))
	if st := n.states["t1"]; st.errorDeadline != nil || !st.errorSeen {
		t.Fatalf("same-instance reconcile must preserve errorSeen (no re-arm): %+v", st)
	}

	// busy 对账清除：回 busy → 再 overflow → errorSeen 复位，新 episode 可武装。
	ft.set(activeSnap("t1", "构建服务", "busy"))
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
	n.onOverflow(ctx)
	if st := n.states["t1"]; st.errorSeen {
		t.Fatal("busy snapshot at reconcile must reset errorSeen")
	}
	ft.set(activeSnap("t1", "构建服务", "idle"))
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "next episode", nil, nil))
	if st := n.states["t1"]; st.errorDeadline == nil {
		t.Fatal("error must re-arm after episode closed by busy reconcile")
	}
}

// TestFire_GateRechecksInstance B3 补齐：到期门禁复验实例一致才允许投递——旧
// 实例武装的 deadline 在换代（无 serve 事件触达、如对账遗漏）后届满 MUST NOT
// 对新实例投递。
func TestFire_GateRechecksInstance(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	// iv-t1 实例武装 idle。
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	// 换代：快照实例变为 iv2-t1（状态绑定仍属旧实例——模拟对账/事件窗口遗漏）。
	swapped := activeSnap("t1", "构建服务", "idle")
	swapped.InstVersion = "iv2-t1"
	ft.set(swapped)
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("stale-instance deadline must not deliver on new instance, sends = %d", got)
	}
	// 旧实例条件按已消费处理（不占名额、不再重试）。
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("no re-delivery for consumed stale-instance condition, sends = %d", got)
	}
}

// TestTaskLeave_StaleEventKeepsNewInstanceState B3 补齐：迟到的旧 active→非
// active 事件不得删除新实例已建立的状态（当前快照仍 active 时仅清旧实例状态）。
func TestTaskLeave_StaleEventKeepsNewInstanceState(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	// 新实例 iv2-t1 武装 idle。
	swapped := activeSnap("t1", "构建服务", "idle")
	swapped.InstVersion = "iv2-t1"
	ft.set(swapped)
	n.handleEvent(ctx, ocdeckevent.NewServeRuntimeRunStatusChanged("iv2-t1", "t1", "busy", "idle", true))
	if st := n.states["t1"]; st == nil || st.idleSince == nil {
		t.Fatal("prereq: new-instance state armed")
	}

	// 迟到的旧 leave 事件（任务当前仍 active）→ 新实例状态保留，计时存活。
	n.handleEvent(ctx, ocdeckevent.NewTaskStatusChanged("t1", "active", "suspending"))
	if st := n.states["t1"]; st == nil || st.idleSince == nil || st.instVersion != "iv2-t1" {
		t.Fatalf("stale leave event must keep new-instance state: %+v", st)
	}
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 || ch.sent()[0].Category != notification.CategoryIdle {
		t.Fatalf("new-instance idle must still fire after stale leave, got %+v", ch.sent())
	}
}

// TestEpisode_ReopenAfterBusy busy→retry→busy→retry：episode 重开，第二次 retry
// 重新获得名额（spec「episode 结束后重新武装」）。
func TestEpisode_ReopenAfterBusy(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "retry")
	ctx := context.Background()
	setRetry := func(attempt int) {
		snap := activeSnap("t1", "构建服务", "retry")
		snap.HasRetryDetail = true
		snap.RetryDetail = RetryDetail{Attempt: attempt, Message: "m"}
		ft.set(snap)
	}
	setRetry(1)
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "retry", true))
	clk.add(60 * time.Second)
	n.scan(ctx) // 第一次 retry 通知
	waitDispatch(n)

	// busy 结束 episode → 再次 retry 开新 episode。
	setRetry(2)
	n.handleEvent(ctx, runStatusEvent("t1", "retry", "busy", true))
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "retry", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 2 {
		t.Fatalf("reopened episode must re-notify, sends = %d", len(sent))
	}
	if !strings.Contains(sent[1].Body, "第 2 次重试") {
		t.Fatalf("second episode uses latest detail, got %q", sent[1].Body)
	}
}
