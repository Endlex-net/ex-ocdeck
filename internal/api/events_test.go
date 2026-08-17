// events_test.go 验证 P1.6.5 SetEventSubscriber：存储注入 + RebuildRoutes 不 panic +
// 存储的订阅者可 Subscribe 并收到一条事件。api 不 import eventbus（真实 bus 经 cmd
// 适配层的集成由 cmd/ocdeck-server 测试覆盖），此处用纯内存 fake 验证注入语义。
package api

import (
	"testing"
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
)

// fakeEventSubscriber 实现 EventSubscriber：Subscribe 返回共享 channel 的订阅句柄。
type fakeEventSubscriber struct {
	ch chan ocdeckevent.Event
}

func (f *fakeEventSubscriber) Subscribe(ocdeckevent.Topic) EventSubscription {
	return &fakeEventSubscription{ch: f.ch}
}

// fakeEventSubscription 实现 EventSubscription（Overflow 无信号源，本测试不触达）。
type fakeEventSubscription struct {
	ch chan ocdeckevent.Event
}

func (f *fakeEventSubscription) C() <-chan ocdeckevent.Event { return f.ch }
func (f *fakeEventSubscription) Overflow() <-chan struct{}   { return nil }
func (f *fakeEventSubscription) Close()                      {}

func TestP165_SetEventSubscriber_RebuildRoutesSmoke(t *testing.T) {
	srv := New(testConfig(), nil)
	sub := &fakeEventSubscriber{ch: make(chan ocdeckevent.Event, 1)}
	srv.SetEventSubscriber(sub)
	srv.RebuildRoutes() // MUST NOT panic

	// 存储的订阅者可 Subscribe + Publish + receive 一条事件。
	es := srv.eventSubscriber.Subscribe(ocdeckevent.TopicTask)
	defer es.Close()

	sub.ch <- ocdeckevent.NewTaskCreated("t1")
	select {
	case ev := <-es.C():
		if ev.Type != ocdeckevent.TypeTaskCreated || ev.RID != "t1" {
			t.Fatalf("event = %+v, want task.created rid=t1", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out receiving published event")
	}
}
