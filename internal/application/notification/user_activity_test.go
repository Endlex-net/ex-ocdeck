// user_activity_test.go task.user_activity 消费（idle-reminder-user-activity D2；
// spec「通知触发——空闲超时（idle）」新增 scenario 与「用户主动操作识别」）。
// engine 直测：fake clock 推进、handleEvent/scan 串行调用，不睡真实时间。
package notification

import (
	"context"
	"testing"
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/domain/notification"
)

// TestTrigger_UserActivityCancelsArmedIdle 等待窗口内主动操作取消 idle 计时，
// 取消后持续 idle 无论多久 MUST NOT 触发（spec「等待窗口内主动操作取消 idle 计时」
// 「主动操作取消后持续空闲不再触发」）。
func TestTrigger_UserActivityCancelsArmedIdle(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	if n.states["t1"].idleSince == nil {
		t.Fatal("idle must be armed before activity")
	}

	n.handleEvent(ctx, userActivityEvent("t1"))
	if n.states["t1"].idleSince != nil {
		t.Fatal("user activity must cancel armed idle timer")
	}

	clk.add(10 * time.Minute)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("canceled idle must never notify, sends = %d", len(ch.sent()))
	}
}

// TestTrigger_UserActivityCancelThenRearm 主动操作取消后，新的满足武装条件的
// busy→idle 迁移照常重新武装并触发（spec「通知后重新武装」同语义）。
func TestTrigger_UserActivityCancelThenRearm(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	n.handleEvent(ctx, userActivityEvent("t1"))

	n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Category != notification.CategoryIdle {
		t.Fatalf("re-arm after activity must notify, sends = %+v", sent)
	}
}

// TestTrigger_UserActivityBeforeArmNotCached 主动操作先于武装被处理：无状态任务
// no-op（不创建状态）、不缓存；之后武装照常起算（spec「主动操作先于武装被处理
// 不缓存」「无已武装 idle 计时任务的主动操作无效果」）。
func TestTrigger_UserActivityBeforeArmNotCached(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	n.handleEvent(ctx, userActivityEvent("t1")) // 尚无任何状态
	n.handleEvent(ctx, userActivityEvent("ghost"))
	if st, ok := n.states["t1"]; ok && st.idleSince != nil {
		t.Fatal("activity before arm must not arm or cache anything")
	}
	if _, ok := n.states["ghost"]; ok {
		t.Fatal("activity for stateless task must not create state")
	}

	// 之后的武装迁移照常起算并触发。
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 1 {
		t.Fatalf("arm after early activity must notify normally, sends = %d", len(ch.sent()))
	}
}

// TestTrigger_UserActivityCrossTaskIsolation 其他任务的主动操作不影响本任务计时，
// 届满照常触发（spec「其他任务的主动操作不影响本任务计时」）。
func TestTrigger_UserActivityCrossTaskIsolation(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	n.handleEvent(ctx, userActivityEvent("t2"))
	if n.states["t1"].idleSince == nil {
		t.Fatal("other task's activity must not cancel t1's timer")
	}
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 1 {
		t.Fatalf("t1 must still fire at deadline, sends = %d", len(ch.sent()))
	}
}

// TestTrigger_UserActivityLateHitsCurrentlyArmed 迟到活动命中同任务当前已武装
// 计时（已确认的串行消费顺序，DR1）：活动信号晚于一次 re-arm 到达时，取消的
// 是处理时刻仍武装的计时（spec「等待窗口内主动操作取消 idle 计时」+ design D2）。
func TestTrigger_UserActivityLateHitsCurrentlyArmed(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	// 第一轮武装，busy→idle→busy→idle 重新武装后活动才被处理。
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	if n.states["t1"].idleSince == nil {
		t.Fatal("idle must be armed (latest cycle)")
	}

	n.handleEvent(ctx, userActivityEvent("t1"))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 0 {
		t.Fatalf("late activity must cancel the currently armed timer, sends = %d", len(ch.sent()))
	}
}

// TestTrigger_UserActivityKeepsOtherTriggerState 活动”仅置空 idleSince“：retry/
// error 计时、episode、去重集合与抑制态一律不动（任务 3.1 契约；spec「通知已触发
// 后主动操作不改变抑制态」）。
func TestTrigger_UserActivityKeepsOtherTriggerState(t *testing.T) {
	n, _, _, _, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	st := n.states["t1"]
	retryAt := clk.now().Add(retryErrorWindow)
	errorAt := clk.now().Add(retryErrorWindow)
	st.retryDeadline = &retryAt
	st.errorDeadline = &errorAt
	st.episodeActive = true
	st.errorSeen = true
	st.idleSuppressed = true
	st.notifiedQuestions["q1"] = struct{}{}
	st.notifiedPermissions["p1"] = struct{}{}

	n.handleEvent(ctx, userActivityEvent("t1"))

	if st.idleSince != nil {
		t.Fatal("idleSince must be cleared")
	}
	if st.retryDeadline == nil || st.errorDeadline == nil {
		t.Fatal("retry/error deadlines must be untouched")
	}
	if !st.episodeActive || !st.errorSeen {
		t.Fatal("episode state must be untouched")
	}
	if !st.idleSuppressed {
		t.Fatal("suppression must be untouched")
	}
	if len(st.notifiedQuestions) != 1 || len(st.notifiedPermissions) != 1 {
		t.Fatal("dedup sets must be untouched")
	}
}

// TestTrigger_UserActivityAfterConsumedNoEffect 计时先被消费（本周期以通知结束、
// 进入抑制态）后的主动操作不改变抑制态、不产生额外通知（spec「计时先被消费则主动
// 操作不改变本周期结果」「通知已触发后主动操作不改变抑制态」）。
func TestTrigger_UserActivityAfterConsumedNoEffect(t *testing.T) {
	n, _, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 1 || !n.states["t1"].idleSuppressed {
		t.Fatalf("idle notify must fire and suppress, sends = %d", len(ch.sent()))
	}

	n.handleEvent(ctx, userActivityEvent("t1"))
	if !n.states["t1"].idleSuppressed {
		t.Fatal("activity must not change suppression state")
	}
	clk.add(10 * time.Minute)
	n.scan(ctx)
	waitDispatch(n)
	if len(ch.sent()) != 1 {
		t.Fatalf("suppressed idle must not re-notify, sends = %d", len(ch.sent()))
	}
}

func userActivityEvent(taskID string) ocdeckevent.Event {
	return ocdeckevent.NewTaskUserActivity(taskID)
}
