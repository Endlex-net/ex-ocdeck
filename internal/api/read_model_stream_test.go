package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
)

func TestReadModelStream_AssembleGoneInitialJSON404(t *testing.T) {
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, newStreamBackend(), sub, 50*time.Millisecond, 5*time.Second)

	req := httptest.NewRequest("GET", "/api/v1/tasks/missing/stream", nil)
	rec := httptest.NewRecorder()
	s.runReadModelStream(rec, req, readModelStreamConfig{
		assemble: func(ctx context.Context) (any, error) {
			return nil, application.ErrTaskNotFound
		},
		eventDirty:   func(ocdeckevent.Event) bool { return true },
		assembleGone: func(err error) bool { return errors.Is(err, application.ErrTaskNotFound) },
		logPrefix:    "gone-initial",
		errCopy:      "get task failed",
	})

	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (no SSE headers)", ct)
	}
	if got := resp.Header.Get("Cache-Control"); got != "" {
		t.Errorf("cache-control = %q, want empty (no SSE headers)", got)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeNotFound || eb.Error.Message != "task not found" {
		t.Errorf("envelope = %+v, want not_found/task not found", eb.Error)
	}
	waitFor(t, 2*time.Second, "subscriptions closed after initial gone", func() bool {
		return sub.liveSubs() == 0
	})
}

func TestReadModelStream_AssembleGonePushClosesWithoutErrorLogPath(t *testing.T) {
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, newStreamBackend(), sub, 40*time.Millisecond, 10*time.Second)

	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.runReadModelStream(w, r, readModelStreamConfig{
			assemble: func(ctx context.Context) (any, error) {
				if n.Add(1) == 1 {
					return map[string]string{"id": "t1"}, nil
				}
				return nil, application.ErrTaskNotFound
			},
			eventDirty:   func(ocdeckevent.Event) bool { return true },
			assembleGone: func(err error) bool { return errors.Is(err, application.ErrTaskNotFound) },
			logPrefix:    "gone-push",
			errCopy:      "get task failed",
		})
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	snap := nextFrame(t, frames, "snapshot")
	if snap.event != "snapshot" {
		t.Fatalf("first frame = %+v, want snapshot", snap)
	}
	sub.publish(ocdeckevent.NewTaskDeleted("t1", application.StatusActive))
	select {
	case f, ok := <-frames:
		if ok && f.event == "error" {
			t.Errorf("gone path must not write error frames: %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after push gone")
	}
	waitFor(t, 2*time.Second, "subscriptions closed after gone", func() bool {
		return sub.liveSubs() == 0
	})
}

func TestReadModelStream_NilAssembleGoneKeepsExistingBehavior(t *testing.T) {
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, newStreamBackend(), sub, 50*time.Millisecond, 5*time.Second)

	req := httptest.NewRequest("GET", "/api/v1/tasks/active/stream", nil)
	rec := httptest.NewRecorder()
	s.runReadModelStream(rec, req, readModelStreamConfig{
		assemble: func(ctx context.Context) (any, error) {
			return nil, errStoreFailure
		},
		eventDirty: func(ocdeckevent.Event) bool { return true },
		logPrefix:  "nil-hook",
		errCopy:    "list active sessions failed",
	})

	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (nil assembleGone)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "list active sessions failed") {
		t.Errorf("body = %q, want existing 500 copy", body)
	}
}
