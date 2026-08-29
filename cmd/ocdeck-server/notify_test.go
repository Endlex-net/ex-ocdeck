package main

import (
	"context"
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
