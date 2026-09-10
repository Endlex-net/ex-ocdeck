package task

import (
	"context"
	"sync"
	"testing"

	ocdeckevent "ocdeck/internal/domain/event"
)

// activityRecordingPublisher 记录型 Publisher double（idle-reminder-user-activity
// 任务 2.2/2.3 验证）：捕获发布的事件供信封断言。
type activityRecordingPublisher struct {
	mu     sync.Mutex
	events []ocdeckevent.Event
}

func (p *activityRecordingPublisher) Publish(e ocdeckevent.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

func (p *activityRecordingPublisher) recorded() []ocdeckevent.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ocdeckevent.Event(nil), p.events...)
}

// seedActivityTask 经 mockStore 预置一条任务行，返回落库后的行（含 mock 回填的
// CreatedAt/UpdatedAt，作为不变性断言基线）。
func seedActivityTask(t *testing.T, store *mockStore, id string) TaskRow {
	t.Helper()
	if err := store.CreateTask(context.Background(), TaskRow{ID: id, ProjectID: "p1", Name: "t", Branch: "b", Status: StatusActive}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	row, err := store.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return row
}

// TestRecordUserActivity_PublishesEnvelope 断言事件信封四字段与行不变性
//（任务 2.2：捕获的事件仅为 task.user_activity、任务行（至少 updated_at）不变）。
func TestRecordUserActivity_PublishesEnvelope(t *testing.T) {
	store := newMockStore()
	row := seedActivityTask(t, store, "taska1")
	pub := &activityRecordingPublisher{}
	m := New(Options{Store: store, Publish: pub})

	m.RecordUserActivity(context.Background(), "taska1")

	events := pub.recorded()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Topic != ocdeckevent.TopicTask || ev.Type != ocdeckevent.TypeTaskUserActivity ||
		ev.RID != "taska1" {
		t.Fatalf("envelope mismatch: %+v", ev)
	}
	if _, ok := ev.Payload.(struct{}); !ok {
		t.Fatalf("payload type = %T, want struct{}", ev.Payload)
	}
	got, err := store.GetTask(context.Background(), "taska1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.UpdatedAt != row.UpdatedAt || got.Status != row.Status {
		t.Fatalf("task row mutated: updated_at %d→%d, status %q→%q",
			row.UpdatedAt, got.UpdatedAt, row.Status, got.Status)
	}
}

// TestRecordUserActivity_NoPublishNoOp 断言 Publish 未注入时 no-op（零发布、行不变）。
func TestRecordUserActivity_NoPublishNoOp(t *testing.T) {
	store := newMockStore()
	row := seedActivityTask(t, store, "taska1")
	m := New(Options{Store: store})

	m.RecordUserActivity(context.Background(), "taska1")

	got, err := store.GetTask(context.Background(), "taska1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.UpdatedAt != row.UpdatedAt {
		t.Fatalf("updated_at mutated: %d→%d", row.UpdatedAt, got.UpdatedAt)
	}
}

// TestRecordShellUserActivity 断言合法 tid 归属解析正确、非法 tid 零发布（任务 2.3）。
func TestRecordShellUserActivity(t *testing.T) {
	t.Run("legal shell session name", func(t *testing.T) {
		pub := &activityRecordingPublisher{}
		m := New(Options{Publish: pub})

		m.RecordShellUserActivity(context.Background(), "ocdeck-taska1-shell-1")

		events := pub.recorded()
		if len(events) != 1 {
			t.Fatalf("published %d events, want 1", len(events))
		}
		if events[0].RID != "taska1" || events[0].Type != ocdeckevent.TypeTaskUserActivity {
			t.Fatalf("envelope mismatch: %+v", events[0])
		}
	})
	t.Run("illegal tid silent zero publish", func(t *testing.T) {
		pub := &activityRecordingPublisher{}
		m := New(Options{Publish: pub})

		m.RecordShellUserActivity(context.Background(), "not-an-ocdeck-session")
		m.RecordShellUserActivity(context.Background(), "")

		if n := len(pub.recorded()); n != 0 {
			t.Fatalf("published %d events, want 0", n)
		}
	})
	t.Run("no publish injected no-op", func(t *testing.T) {
		m := New(Options{})

		m.RecordShellUserActivity(context.Background(), "ocdeck-taska1-shell-1")
	})
}
