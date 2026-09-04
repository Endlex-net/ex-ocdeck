package notify

import (
	"testing"

	"ocdeck/internal/domain/notification"
)

type stubPublisher struct{}

func (stubPublisher) Publish(notification.Intent) bool { return false }

func TestBuildChannels_OrderAndNames(t *testing.T) {
	chs := BuildChannels(stubPublisher{}, "linux")
	if len(chs) != 4 {
		t.Fatalf("len = %d, want 4", len(chs))
	}
	want := []string{"web", "bark", "macos", "wecom"}
	for i, name := range want {
		if got := chs[i].Name(); got != name {
			t.Errorf("channel[%d].Name() = %q, want %q", i, got, name)
		}
	}
}
