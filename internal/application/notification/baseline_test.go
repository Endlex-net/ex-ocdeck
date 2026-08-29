// baseline_test.go 启动基线与 overflow 对账（spec「通知抑制、启动基线与对账」；
// design D3 启动基线/overflow reconciling）。engine 直测。
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

// baselineFixture 三任务装置：t1 idle+pending q1、t2 retry、t3 idle 干净。
func baselineFixture(t *testing.T) (*Notifier, *fakeTasks, *fakeLister, *fakeCfgStore, *fakeChannel, *fakeClock) {
	t.Helper()
	snapT1 := attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q1", "启动前已 pending")}, nil)
	snapT1.Task = TaskRef{ID: "t1", Name: "任务一", Status: "active"}
	snapT2 := activeSnap("t2", "任务二", "retry")
	snapT3 := activeSnap("t3", "任务三", "idle")
	ft := newFakeTasks(snapT1, snapT2, snapT3)
	fl := &fakeLister{ids: []string{"t1", "t2", "t3"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
	clk := newFakeClock()
	n := newTestNotifier(ft, fl, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	return n, ft, fl, fc, ch, clk
}

// TestBaseline_SeedsWithoutBackfill 启动基线：pending attention 只播种去重集合、
// 不补发；idle 不武装；retry 自启动时刻重新计时 60s（spec 启动基线三 scenario）。
func TestBaseline_SeedsWithoutBackfill(t *testing.T) {
	n, ft, _, _, ch, clk := baselineFixture(t)
	ctx := context.Background()
	n.initBaseline(ctx)

	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("baseline must not backfill notifications, sends = %d", got)
	}
	st1 := n.states["t1"]
	if st1 == nil {
		t.Fatal("t1 state missing after baseline")
	}
	if _, seeded := st1.notifiedQuestions["q1"]; !seeded {
		t.Fatal("pending question at startup must seed dedup set")
	}
	if st1.idleSince != nil {
		t.Fatal("idle must not arm from baseline")
	}
	st2 := n.states["t2"]
	if st2 == nil || st2.retryDeadline == nil {
		t.Fatal("retry task at startup must be re-timed")
	}
	if got := st2.retryDeadline.Sub(clk.now()); got != 60*time.Second {
		t.Fatalf("baseline retry deadline = %v from now, want 60s", got)
	}
	if !st2.episodeActive {
		t.Fatal("baseline retry task must be in episode")
	}

	// 基线后：q1 重复刷新不补发；新 q2 通知；t2 retry 计时届满通知（重启后重新
	// 计时语义）。
	snapT1 := attentionSnapWith("idle",
		[]application.PendingQuestion{pendingQuestion("q1", "启动前已 pending"), pendingQuestion("q2", "新问题")}, nil)
	snapT1.Task = TaskRef{ID: "t1", Name: "任务一", Status: "active"}
	ft.set(snapT1)
	n.handleEvent(ctx, attentionEvent("t1"))

	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	cats := map[notification.Category]int{}
	for _, in := range sent {
		cats[in.Category]++
	}
	if cats[notification.CategoryQuestion] != 1 || len(sent) != 2 ||
		!containsBody(sent, "新问题") || containsBody(sent, "启动前已 pending") {
		t.Fatalf("after baseline: only new q2 + retry t2 should notify, got %+v", sent)
	}
}

// TestBaseline_EnumFailureDisablesTriggers 枚举失败：记错误日志并整体禁用通知
// 触发（事件与 tick 全部无效）。
func TestBaseline_EnumFailureDisablesTriggers(t *testing.T) {
	n, ft, fl, _, ch, clk := baselineFixture(t)
	fl.set(nil, errNotFound)
	ctx := context.Background()
	n.initBaseline(ctx)

	if n.mode != modeDisabled {
		t.Fatalf("mode = %v, want disabled", n.mode)
	}
	// 事件与 tick 均不再产生任何投递。
	ft.set(attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("qn", "x")}, nil))
	n.handleEvent(ctx, attentionEvent("t1"))
	n.handleEvent(ctx, runStatusEvent("t2", "busy", "retry", true))
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("disabled notifier must never deliver, sends = %d", got)
	}
}

// TestBaseline_TaskSnapshotFailureSkipped 单任务基线快照失败：跳过该任务
// （fail-safe：不武装任何计时），其余任务正常。
func TestBaseline_TaskSnapshotFailureSkipped(t *testing.T) {
	n, ft, _, _, _, _ := baselineFixture(t)
	ft.setErr(func(taskID string) error {
		if taskID == "t1" {
			return errNotFound
		}
		return nil
	})
	n.initBaseline(context.Background())
	if n.states["t1"] != nil {
		t.Fatal("snapshot-failed task must be skipped at baseline")
	}
	if n.states["t2"] == nil || n.states["t2"].retryDeadline == nil {
		t.Fatal("other tasks must still be baselined")
	}
}

// TestOverflowReconcile_PreservesDedupAndQuota overflow 对账：保留去重集合与
// episodeConsumed、取消全部计时、未消费 pending 仅播种不补发、retry 重新计时。
func TestOverflowReconcile_PreservesDedupAndQuota(t *testing.T) {
	n, ft, _, _, ch, clk := baselineFixture(t)
	ctx := context.Background()
	n.initBaseline(ctx)

	// 制造已消费 episode（t2 error 已投递）与已通知去重条目（t1 q2 已通知）。
	clk.add(60 * time.Second)
	n.scan(ctx) // t2 retry 通知（episode 名额占用）
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 || ch.sent()[0].Category != notification.CategoryRetry {
		t.Fatalf("prereq: t2 retry notify, got %+v", ch.sent())
	}
	ft.set(attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q1", "p"), pendingQuestion("q2", "n")}, nil))
	n.handleEvent(ctx, attentionEvent("t1")) // q2 通知（q1 播种抑制）
	waitDispatch(n)
	if got := len(ch.sent()); got != 2 {
		t.Fatalf("prereq sends = %d", got)
	}

	// 溢出对账：取消 idle/error 计时（retry 按基线规则对仍 retry 的任务重新计时）、
	// 保留仍 pending 的去重条目与 t2 episodeConsumed、新 pending 仅播种不补发。
	snapT1 := attentionSnapWith("idle",
		[]application.PendingQuestion{pendingQuestion("q1", "p"), pendingQuestion("q2", "n"), pendingQuestion("q3", "reconcile-new")}, nil)
	snapT1.Task = TaskRef{ID: "t1", Name: "任务一", Status: "active"}
	ft.set(snapT1)
	ft.set(activeSnap("t2", "任务二", "retry"))
	n.onOverflow(ctx)

	if n.mode != modeRunning {
		t.Fatalf("reconcile success must restore running mode, got %v", n.mode)
	}
	st1 := n.states["t1"]
	if _, ok := st1.notifiedQuestions["q3"]; !ok {
		t.Fatal("unconsumed pending found by reconcile must be seeded (not backfilled)")
	}
	waitDispatch(n)
	if got := len(ch.sent()); got != 2 {
		t.Fatalf("reconcile must not backfill, sends = %d", got)
	}
	for id, st := range n.states {
		if st.idleSince != nil || st.errorDeadline != nil {
			t.Fatalf("task %s idle/error timers must be canceled by reconcile: %+v", id, st)
		}
	}
	if !n.states["t2"].episodeConsumed {
		t.Fatal("episodeConsumed must be preserved across reconcile")
	}
	// t2 仍 retry → 按基线规则重新计时；但名额已占用 → 届满不再投递。
	if n.states["t2"].retryDeadline == nil {
		t.Fatal("still-retry task must be re-timed by reconcile")
	}
	clk.add(70 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 2 {
		t.Fatalf("consumed episode must not re-deliver after reconcile, sends = %d", got)
	}
}

// TestOverflowReconcile_FailEnterReconciling 对账期间枚举/快照失败：进入
// reconciling（抑制全部发送，事件丢弃），每 tick 重试，成功后恢复。
func TestOverflowReconcile_FailEnterReconciling(t *testing.T) {
	n, ft, fl, _, ch, clk := baselineFixture(t)
	ctx := context.Background()
	n.initBaseline(ctx)

	// 溢出 + 枚举失败 → reconciling。
	fl.set(nil, errNotFound)
	n.onOverflow(ctx)
	if n.mode != modeReconciling {
		t.Fatalf("mode = %v, want reconciling", n.mode)
	}
	// reconciling 期间：事件丢弃（不投递不记录）、tick 只重试对账。
	ft.set(attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q9", "during-reconciling")}, nil))
	n.handleEvent(ctx, attentionEvent("t1"))
	clk.add(time.Hour)
	n.scan(ctx) // 重试对账仍失败
	waitDispatch(n)
	if n.mode != modeReconciling || len(ch.sent()) != 0 {
		t.Fatalf("reconciling must suppress all sends: mode=%v sends=%d", n.mode, len(ch.sent()))
	}

	// 恢复：枚举成功 → 对账完成恢复 running；后续事件正常处理。
	fl.set([]string{"t1"}, nil)
	n.scan(ctx)
	if n.mode != modeRunning {
		t.Fatalf("recovered reconcile must restore running, got %v", n.mode)
	}
	n.handleEvent(ctx, attentionEvent("t1")) // q9 在对账中已播种 → 不投递
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("seeded pending must not backfill after reconcile, sends = %d", got)
	}
	// 新事件正常通知。
	snapT1 := attentionSnapWith("idle",
		[]application.PendingQuestion{pendingQuestion("q9", "during-reconciling"), pendingQuestion("q10", "post")}, nil)
	snapT1.Task = TaskRef{ID: "t1", Name: "任务一", Status: "active"}
	ft.set(snapT1)
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 || !containsBody(ch.sent(), "post") {
		t.Fatalf("post-reconcile event must notify, got %+v", ch.sent())
	}
}

// TestOverflowReconcile_BusySnapshotClearsQuota B4：对账时快照已 busy（episode
// 已结束）→ 执行 episode 关闭语义、清除消费位与 errorSeen；仅仍存续的非 busy
// episode 保留名额（design D3 fencing 段）。
func TestOverflowReconcile_BusySnapshotClearsQuota(t *testing.T) {
	n, ft, _, _, ch, clk := baselineFixture(t)
	ctx := context.Background()
	n.initBaseline(ctx)

	// t2 retry 届满 → 通知（episode 名额占用）。
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 || ch.sent()[0].Category != notification.CategoryRetry {
		t.Fatalf("prereq: t2 retry notify, got %+v", ch.sent())
	}

	// 聚合回到 busy（episode 结束）后 overflow 对账 → 消费位被清除。
	ft.set(activeSnap("t2", "任务二", "busy"))
	n.onOverflow(ctx)
	st2 := n.states["t2"]
	if st2 == nil || st2.episodeConsumed || st2.episodeActive || st2.errorSeen {
		t.Fatalf("busy snapshot at reconcile must close episode & clear quota: %+v", st2)
	}

	// 新 episode：idle→retry 重新武装 → 届满可再次通知（名额已释放）。
	ft.set(activeSnap("t2", "任务二", "retry"))
	snapT2 := activeSnap("t2", "任务二", "retry")
	snapT2.HasRetryDetail = true
	snapT2.RetryDetail = RetryDetail{Attempt: 2, Message: "again"}
	ft.set(snapT2)
	n.handleEvent(ctx, runStatusEvent("t2", "idle", "retry", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	sent := ch.sent()
	if len(sent) != 2 || !strings.Contains(sent[1].Body, "第 2 次重试") {
		t.Fatalf("new episode must re-notify after busy reconcile, got %+v", sent)
	}
}

// TestTaskLeavingActiveCancelsAll 任务离开 active（status_changed from=active）：
// 取消全部待决计时并清空触发态；重新激活后重新武装可再通知（suspend→reactivate）。
func TestTaskLeavingActiveCancelsAll(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "idle")
	ctx := context.Background()

	// 挂起：idle 计时未届满时被挂起 → 计时器取消不再触发（leave 事件处理时
	// 任务行已非 active——B3 依据组合快照判定 leave 生效）。
	suspending := activeSnap("t1", "构建服务", "idle")
	suspending.Task.Status = "suspending"
	ft.set(suspending)
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	n.handleEvent(ctx, ocdeckevent.NewTaskStatusChanged("t1", "active", "suspending"))
	if n.states["t1"] != nil {
		t.Fatal("leaving active must drop task state")
	}
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("suspended task must not notify, sends = %d", got)
	}

	// 重新激活 + busy→idle → 重新武装并通知（挂起前状态不恢复；实例切换后事件
	// 须携带新 instVersion——B3 fencing）。
	n.handleEvent(ctx, ocdeckevent.NewTaskStatusChanged("t1", "suspended", "active"))
	reactivated := activeSnap("t1", "构建服务", "idle")
	reactivated.InstVersion = "iv2-t1"
	ft.set(reactivated)
	n.handleEvent(ctx, ocdeckevent.NewServeRuntimeRunStatusChanged("iv2-t1", "t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 || ch.sent()[0].Category != notification.CategoryIdle {
		t.Fatalf("reactivated task must re-arm idle, got %+v", ch.sent())
	}
}

// TestTaskDeletedFromActiveCancels task.deleted from=active 同样清空触发态。
func TestTaskDeletedFromActiveCancels(t *testing.T) {
	n, _, _, _, _ := triggerFixture(t, "idle")
	n.handleEvent(context.Background(), runStatusEvent("t1", "busy", "idle", true))
	n.handleEvent(context.Background(), ocdeckevent.NewTaskDeleted("t1", "active"))
	if n.states["t1"] != nil {
		t.Fatal("deleted task must drop state")
	}
}

// containsBody 判定发送记录中是否存在包含指定正文的意图。
func containsBody(sent []notification.Intent, sub string) bool {
	for _, in := range sent {
		if strings.Contains(in.Body, sub) {
			return true
		}
	}
	return false
}
