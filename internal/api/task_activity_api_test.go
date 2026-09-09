// task_activity_api_test.go POST /api/v1/tasks/{id}/activity（idle-reminder-user-activity
// 任务 5.1；design D4 处理链：Get 校验失败零发布+错误映射、成功 204 且发布一次、
// 非 active 任务同样 204）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ocdeck/internal/application"
)

// activityRowBackend 覆盖 Get 返回指定任务行（active / 非 active 场景）。
type activityRowBackend struct {
	*fakeTaskBackend
	row application.TaskRow
}

func (b *activityRowBackend) Get(ctx context.Context, taskID string) (application.TaskRow, error) {
	return b.row, nil
}

// activityGetErrBackend 覆盖 Get 存在性校验失败路径。
type activityGetErrBackend struct {
	*fakeTaskBackend
	getErr error
}

func (b *activityGetErrBackend) Get(ctx context.Context, taskID string) (application.TaskRow, error) {
	return application.TaskRow{}, b.getErr
}

// TestTaskActivity_GetFailureNoPublish Get 失败：经 mapTaskErr 映射错误信封，
// MUST NOT 发布（design D4）。
func TestTaskActivity_GetFailureNoPublish(t *testing.T) {
	tb := &activityGetErrBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		getErr:          &application.OpError{Code: "not_found", Err: errors.New("task missing")},
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/t1/activity", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != CodeNotFound {
		t.Errorf("code=%v want not_found", body.Error.Code)
	}
	if calls := tb.recordedActivityCalls(); len(calls) != 0 {
		t.Errorf("Get failure must not publish, calls = %v", calls)
	}
}

// TestTaskActivity_ActiveSuccess204 成功：204 且发布一次（RID=路径 taskID）。
func TestTaskActivity_ActiveSuccess204(t *testing.T) {
	tb := &activityRowBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		row:             application.TaskRow{ID: "t1", Status: application.StatusActive},
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/t1/activity", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want 204", resp.StatusCode)
	}
	if calls := tb.recordedActivityCalls(); len(calls) != 1 || calls[0] != "t1" {
		t.Fatalf("activity calls = %v, want exactly [t1]", calls)
	}
}

// TestTaskActivity_NonActiveStill204 任务存在但非 active：不新增错误分支，同样 204。
func TestTaskActivity_NonActiveStill204(t *testing.T) {
	tb := &activityRowBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		row:             application.TaskRow{ID: "t1", Status: application.StatusSuspended},
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/tasks/t1/activity", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want 204 (non-active must not error)", resp.StatusCode)
	}
	if calls := tb.recordedActivityCalls(); len(calls) != 1 {
		t.Fatalf("activity calls = %v, want exactly 1", calls)
	}
}
