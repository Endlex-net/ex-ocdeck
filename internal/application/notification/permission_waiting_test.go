// permission_waiting_test.go ai-auto 延迟触发 waiting 状态机（task-permission-mode
// tasks 3.3-3.5，design D1/D7）：首次观察登记 waiting（不写去重）、判定结论唤醒
// 分支定论、deadline 边界（fake clock 15s 与近 25s）、pending 消失清理、mode-changed
// 立即投递、发送前复核否决、overflow 对账矩阵全行、重启基线不建 waiting。
// engine 直测：fake clock 推进、handleEvent/scan 串行调用，不睡真实时间。
package notification

import (
	"context"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/domain/notification"
)

// aiAutoPermSnap 构造有效模式 ai-auto 的 active 快照（pending permissions + 判定状态可选）。
func aiAutoPermSnap(perms []application.PendingPermission, verdicts map[string]PermissionVerdict) TaskSnapshot {
	snap := attentionSnapWith("idle", nil, perms)
	snap.Task.EffectivePermissionMode = aiAutoMode
	snap.Verdicts = verdicts
	return snap
}

func verdictEvent(taskID string) ocdeckevent.Event {
	return ocdeckevent.NewServeRuntimePermissionVerdict("iv-"+taskID, taskID)
}

func modeChangedEvent(taskID string) ocdeckevent.Event {
	return ocdeckevent.NewServeRuntimePermissionModeChanged("iv-"+taskID, taskID)
}

// waitingFixture 单 ai-auto 任务装置（快照由用例覆盖）。
func waitingFixture(t *testing.T) (*Notifier, *fakeTasks, *fakeChannel, *fakeClock) {
	t.Helper()
	ft := newFakeTasks(aiAutoPermSnap(nil, nil))
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	return n, ft, ch, clk
}

// TestWaiting_AutoApprovedNoNotify 自动放行不通知（spec「ai-auto 自动放行不通知」）：
// 首次观察仅登记 waiting 不写去重；settled 唤醒清除 waiting 且零发送。
func TestWaiting_AutoApprovedNoNotify(t *testing.T) {
	n, ft, ch, _ := waitingFixture(t)
	ctx := context.Background()
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if sent := len(ch.sent()); sent != 0 {
		t.Fatalf("first observation must not notify, sends = %d", sent)
	}
	st := n.states["t1"]
	if _, waiting := st.waitingPerms["p1"]; !waiting {
		t.Fatal("first observation must register waiting entry")
	}
	if _, deduped := st.notifiedPermissions["p1"]; deduped {
		t.Fatal("ai-auto first observation MUST NOT write dedup")
	}

	// 判定放行且回复受理（settled_no_notify）→ 清 waiting，不通知。
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")},
		map[string]PermissionVerdict{"p1": {Epoch: 0, State: VerdictSettledNoNotify}}))
	n.handleEvent(ctx, verdictEvent("t1"))
	waitDispatch(n)
	if sent := len(ch.sent()); sent != 0 {
		t.Fatalf("auto-approved request must not notify, sends = %d", sent)
	}
	if _, waiting := st.waitingPerms["p1"]; waiting {
		t.Fatal("settled verdict must clear waiting entry")
	}
}

// TestWaiting_ManualRequiredImmediateNotify 转人工立即通知（不等 deadline）；
// 同 runtime 同 request 最多一次。
func TestWaiting_ManualRequiredImmediateNotify(t *testing.T) {
	n, ft, ch, _ := waitingFixture(t)
	ctx := context.Background()
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
	n.handleEvent(ctx, attentionEvent("t1")) // 登记 waiting（t0）

	// 未推进时钟：manual_required 唤醒即通知（deadline 未到期也立即投递）。
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")},
		map[string]PermissionVerdict{"p1": {Epoch: 0, State: VerdictManualRequired}}))
	n.handleEvent(ctx, verdictEvent("t1"))
	waitDispatch(n)
	if sent := len(ch.sent()); sent != 1 {
		t.Fatalf("manual_required must notify immediately, sends = %d", sent)
	}
	st := n.states["t1"]
	if _, waiting := st.waitingPerms["p1"]; waiting {
		t.Fatal("notified entry must leave waiting")
	}
	if _, deduped := st.notifiedPermissions["p1"]; !deduped {
		t.Fatal("notified entry must be recorded in dedup")
	}
	// 重复唤醒/重复观察不再通知（去重作用域（runtime 实例, request ID））。
	n.handleEvent(ctx, verdictEvent("t1"))
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 {
		t.Fatalf("same request must not re-notify, sends = %d", got)
	}
}

// TestWaiting_DeadlineBoundary deadline 边界（fake clock，D1 [15s, 25s) 窗口）：
// 未定论条目 14s 不触发、15s 整点届满触发；另一用例首个 tick 落在 24s（近 25s 边界内）。
func TestWaiting_DeadlineBoundary(t *testing.T) {
	t.Run("15s 整点触发", func(t *testing.T) {
		n, ft, ch, clk := waitingFixture(t)
		ctx := context.Background()
		ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
		n.handleEvent(ctx, attentionEvent("t1"))
		clk.add(14 * time.Second)
		n.scan(ctx)
		waitDispatch(n)
		if got := len(ch.sent()); got != 0 {
			t.Fatalf("must not fire before deadline, sends = %d", got)
		}
		clk.add(1 * time.Second) // t=15s：deadline 届满
		n.scan(ctx)
		waitDispatch(n)
		if got := len(ch.sent()); got != 1 {
			t.Fatalf("deadline expiry must fire fallback notify, sends = %d", got)
		}
	})
	t.Run("首个 tick 近 25s 边界", func(t *testing.T) {
		n, ft, ch, clk := waitingFixture(t)
		ctx := context.Background()
		ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p2", "edit")}, nil))
		n.handleEvent(ctx, attentionEvent("t1"))
		// 生产 tick 周期 10s：deadline 15s 后首个 tick 可迟至 24s+，仍在 25s 内触发。
		clk.add(24 * time.Second)
		n.scan(ctx)
		waitDispatch(n)
		if got := len(ch.sent()); got != 1 {
			t.Fatalf("first tick after deadline must fire, sends = %d", got)
		}
	})
}

// TestWaiting_RepeatedObservationKeepsDeadline 重复观察 MUST NOT 延长 deadline。
func TestWaiting_RepeatedObservationKeepsDeadline(t *testing.T) {
	n, ft, ch, clk := waitingFixture(t)
	ctx := context.Background()
	snap := aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil)
	ft.set(snap)
	n.handleEvent(ctx, attentionEvent("t1"))
	clk.add(10 * time.Second)
	n.handleEvent(ctx, attentionEvent("t1")) // 重复观察（仍 pending）
	clk.add(4 * time.Second)                 // t=14s：若 deadline 被延长到 25s 此时不触发
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("deadline must not be extended by re-observation, sends = %d", got)
	}
	clk.add(1 * time.Second) // t=15s：原 deadline 届满
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 {
		t.Fatalf("original deadline must stand, sends = %d", got)
	}
}

// TestWaiting_PendingDisappearedClears pending 消失清理 waiting 不通知。
func TestWaiting_PendingDisappearedClears(t *testing.T) {
	n, ft, ch, _ := waitingFixture(t)
	ctx := context.Background()
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
	n.handleEvent(ctx, attentionEvent("t1"))
	if _, waiting := n.states["t1"].waitingPerms["p1"]; !waiting {
		t.Fatal("prereq: waiting entry registered")
	}
	ft.set(aiAutoPermSnap(nil, nil)) // p1 消失（人工已了结）
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("resolved request must not notify, sends = %d", got)
	}
	if _, waiting := n.states["t1"].waitingPerms["p1"]; waiting {
		t.Fatal("disappeared pending must prune waiting entry")
	}
}

// TestWaiting_ModeChangedToNonAIAutoImmediateNotify 模式切出唤醒（D7：waiting
// 重评估只由 mode-changed 驱动）：有效模式已非 ai-auto → 立即按正常门禁投递，
// 不等原 deadline（spec「模式变更事件溢出丢失后既有等待条目仍通知」）。
func TestWaiting_ModeChangedToNonAIAutoImmediateNotify(t *testing.T) {
	n, ft, ch, _ := waitingFixture(t)
	ctx := context.Background()
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
	n.handleEvent(ctx, attentionEvent("t1"))

	// 切出 ai-auto（converge 推 epoch 后旧判定状态不出现在快照）：仍 pending、无状态。
	switched := aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil)
	switched.Task.EffectivePermissionMode = "ask"
	ft.set(switched)
	n.handleEvent(ctx, modeChangedEvent("t1"))
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 {
		t.Fatalf("switched-out waiting entry must notify immediately, sends = %d", got)
	}
	if _, waiting := n.states["t1"].waitingPerms["p1"]; waiting {
		t.Fatal("dispatched entry must leave waiting")
	}
}

// TestWaiting_SendGateVeto 发送前复核否决不发送：唤醒时快照显示该请求已被自动
// 放行（settled）→ 不投递且清 waiting（spec 触发前复核「仍 pending 且未被自动放行」）。
func TestWaiting_SendGateVeto(t *testing.T) {
	n, ft, ch, _ := waitingFixture(t)
	ctx := context.Background()
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
	n.handleEvent(ctx, attentionEvent("t1"))
	// 唤醒快照与 waiting 登记时的判定预期矛盾：请求实际已被自动放行。
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")},
		map[string]PermissionVerdict{"p1": {Epoch: 0, State: VerdictSettledNoNotify}}))
	n.handleEvent(ctx, verdictEvent("t1"))
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("auto-approved request must not notify, sends = %d", got)
	}
	if _, waiting := n.states["t1"].waitingPerms["p1"]; waiting {
		t.Fatal("settled entry must be cleared on wake")
	}
}

// TestWaiting_WakeFencing 旧 runtime 迟到唤醒丢弃（B3）；读取失败保守保留 waiting
// （不误发、待后续事件/tick 重评）。
func TestWaiting_WakeFencing(t *testing.T) {
	n, ft, ch, _ := waitingFixture(t)
	ctx := context.Background()
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")},
		map[string]PermissionVerdict{"p1": {Epoch: 0, State: VerdictManualRequired}}))
	n.handleEvent(ctx, attentionEvent("t1"))

	// RID 与快照实例不一致：唤醒丢弃（manual_required 也不投递）。
	n.handleEvent(ctx, ocdeckevent.NewServeRuntimePermissionVerdict("iv-stale", "t1"))
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("stale wake must be dropped, sends = %d", got)
	}
	// 读取失败：条目保留，无发送。
	ft.setErr(func(string) error { return errNotFound })
	n.handleEvent(ctx, verdictEvent("t1"))
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("read failure must not notify, sends = %d", got)
	}
	if _, waiting := n.states["t1"].waitingPerms["p1"]; !waiting {
		t.Fatal("waiting entry must be kept on read failure (re-evaluated later)")
	}
}

// TestOverflowReconcile_WaitingMatrix overflow 对账矩阵（tasks 3.4，D1 不变量逐行）。
func TestOverflowReconcile_WaitingMatrix(t *testing.T) {
	t.Run("ai-auto 既未通知也无 waiting 新建 waiting 不播种去重", func(t *testing.T) {
		n, ft, ch, clk := waitingFixture(t)
		ctx := context.Background()
		n.initBaseline(ctx) // 无 pending：空基线
		// 对账发现 ai-auto pending p1（attention 事件在 waiting 建立前丢失）。
		ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
		n.onOverflow(ctx)
		if n.mode != modeRunning {
			t.Fatalf("reconcile must restore running, got %v", n.mode)
		}
		st := n.states["t1"]
		if _, waiting := st.waitingPerms["p1"]; !waiting {
			t.Fatal("reconcile must create waiting entry for un-notified un-waiting ai-auto pending")
		}
		if _, deduped := st.notifiedPermissions["p1"]; deduped {
			t.Fatal("MUST NOT seed dedup for the new waiting entry")
		}
		// deadline 从对账观察时刻起算：15s 后兜底触发。
		clk.add(15 * time.Second)
		n.scan(ctx)
		waitDispatch(n)
		if got := len(ch.sent()); got != 1 {
			t.Fatalf("new waiting entry must fire from reconcile observation time, sends = %d", got)
		}
	})
	t.Run("ai-auto waiting 保留原 deadline 按判定状态立即重评", func(t *testing.T) {
		n, ft, ch, clk := waitingFixture(t)
		ctx := context.Background()
		ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
		n.handleEvent(ctx, attentionEvent("t1")) // waiting 登记（t0，deadline t0+15）
		clk.add(5 * time.Second)
		// 对账快照：仍 ai-auto、仍 pending、判定 manual_required → 对账成功后立即通知。
		ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")},
			map[string]PermissionVerdict{"p1": {Epoch: 0, State: VerdictManualRequired}}))
		n.onOverflow(ctx)
		waitDispatch(n)
		if got := len(ch.sent()); got != 1 {
			t.Fatalf("manual_required waiting must notify right after reconcile, sends = %d", got)
		}
	})
	t.Run("ai-auto waiting 未定论保留原 deadline 不重置", func(t *testing.T) {
		n, ft, ch, clk := waitingFixture(t)
		ctx := context.Background()
		ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
		n.handleEvent(ctx, attentionEvent("t1")) // deadline t0+15
		clk.add(5 * time.Second)                 // 对账发生在 t0+5：若重置为 t0+20 则 t0+15 不触发
		n.onOverflow(ctx)
		clk.add(10 * time.Second) // t0+15：原 deadline 届满
		n.scan(ctx)
		waitDispatch(n)
		if got := len(ch.sent()); got != 1 {
			t.Fatalf("original deadline must be preserved across reconcile, sends = %d", got)
		}
	})
	t.Run("切出事件丢失 waiting 对账成功后立即投递", func(t *testing.T) {
		n, ft, ch, _ := waitingFixture(t)
		ctx := context.Background()
		ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
		n.handleEvent(ctx, attentionEvent("t1")) // waiting 登记
		// 切出 ai-auto（mode-changed 事件溢出丢失）：对账快照有效模式 ask、仍 pending。
		switched := aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil)
		switched.Task.EffectivePermissionMode = "ask"
		ft.set(switched)
		n.onOverflow(ctx)
		waitDispatch(n)
		if got := len(ch.sent()); got != 1 {
			t.Fatalf("switched-out waiting entry must deliver right after reconcile, sends = %d", got)
		}
		if _, waiting := n.states["t1"].waitingPerms["p1"]; waiting {
			t.Fatal("delivered entry must leave waiting")
		}
	})
	t.Run("仍 pending 已通知条目保留去重", func(t *testing.T) {
		n, ft, ch, _ := waitingFixture(t)
		ctx := context.Background()
		// 通知先于切入 ai-auto（dedup 由此建立）。
		plain := aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil)
		plain.Task.EffectivePermissionMode = "ask"
		ft.set(plain)
		n.handleEvent(ctx, attentionEvent("t1")) // 非 ai-auto：直接通知并写去重
		waitDispatch(n)
		if got := len(ch.sent()); got != 1 {
			t.Fatalf("prereq direct notify, sends = %d", got)
		}
		// 切入 ai-auto 后溢出对账：p1 仍 pending 且已通知 → 保留去重、不建 waiting、不补发。
		ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
		n.onOverflow(ctx)
		st := n.states["t1"]
		if _, deduped := st.notifiedPermissions["p1"]; !deduped {
			t.Fatal("still-pending notified entry must keep dedup across reconcile")
		}
		if _, waiting := st.waitingPerms["p1"]; waiting {
			t.Fatal("notified entry must not enter waiting")
		}
		if got := len(ch.sent()); got != 1 {
			t.Fatalf("notified entry must not re-notify, sends = %d", got)
		}
	})
}

// TestBaseline_NoWaitingForAIAutoPending 进程重启 waiting 不恢复：启动基线的
// ai-auto pending 按既有语义播种去重、不补发、不建 waiting（D1）。
func TestBaseline_NoWaitingForAIAutoPending(t *testing.T) {
	n, ft, ch, clk := waitingFixture(t)
	ctx := context.Background()
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")}, nil))
	n.initBaseline(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("baseline must not backfill, sends = %d", got)
	}
	st := n.states["t1"]
	if len(st.waitingPerms) != 0 {
		t.Fatalf("waiting must not be restored on restart, got %v", st.waitingPerms)
	}
	if _, deduped := st.notifiedPermissions["p1"]; !deduped {
		t.Fatal("baseline must seed dedup for pending permission")
	}
	// 后续唤醒/到期均被播种的去重压住（不补发）。
	ft.set(aiAutoPermSnap([]application.PendingPermission{pendingPermission("p1", "bash", "rm")},
		map[string]PermissionVerdict{"p1": {Epoch: 0, State: VerdictManualRequired}}))
	n.handleEvent(ctx, verdictEvent("t1"))
	clk.add(1 * time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("baseline-seeded pending must not backfill, sends = %d", got)
	}
}
