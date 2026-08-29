package main

import (
	"context"
	"errors"
	"testing"

	appnotification "ocdeck/internal/application/notification"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/infrastructure/ai"
	"ocdeck/internal/infrastructure/eventbus"
)

func TestNotifyEventSubscriberAdapter_Subscribe(t *testing.T) {
	bus := eventbus.New()
	sub := notifyEventSubscriberAdapter{bus}.Subscribe(ocdeckevent.TopicServeRuntime)
	defer sub.Close()
	if sub.C() == nil || sub.Overflow() == nil {
		t.Fatal("subscription channels must be non-nil")
	}
}

func TestSummaryCompleterAdapter_Unconfigured(t *testing.T) {
	dir := t.TempDir()
	store := ai.LoadStore(dir)
	a := summaryCompleterAdapter{store: store}
	_, err := a.Complete(context.Background(), "", "user", 200)
	if err == nil {
		t.Fatal("unconfigured ai must error so notifier degrades")
	}
}

func TestNotifierSatisfiesTesterPort(t *testing.T) {
	n := appnotification.New(appnotification.Options{})
	if n == nil {
		t.Fatal("notifier")
	}
}

type recordingStopper struct{ stopped int }

func (r *recordingStopper) Stop() { r.stopped++ }

type recordingShutdowner struct {
	calls int
	err   error
}

func (r *recordingShutdowner) Shutdown(context.Context) error {
	r.calls++
	return r.err
}

// TestShutdownRuntime_ListenFailureStillStops G1：Listen 失败（serveErr 非 nil、
// notifier 未 Start）仍走统一关停——Stop / Shutdown / bgStop 都必须执行。
func TestShutdownRuntime_ListenFailureStillStops(t *testing.T) {
	stopper := &recordingStopper{}
	tm := &recordingShutdowner{}
	bg := 0
	listenErr := errors.New("listen: address already in use")
	got := shutdownRuntime(shutdownRuntimeArgs{
		notifier: stopper,
		tm:       tm,
		bgStop:   func() { bg++ },
		serveErr: listenErr,
	})
	if got != listenErr {
		t.Fatalf("return = %v, want listen error", got)
	}
	if stopper.stopped != 1 {
		t.Fatalf("Stop calls = %d, want 1 (Stop-before-Start is safe)", stopper.stopped)
	}
	if tm.calls != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", tm.calls)
	}
	if bg != 1 {
		t.Fatalf("bgStop calls = %d, want 1", bg)
	}
}
