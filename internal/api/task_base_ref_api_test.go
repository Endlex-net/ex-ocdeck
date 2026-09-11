// task_base_ref_api_test.go 任务详情 base_ref 透出（workbench-base-ref-and-overflow
// D1 / task-detail-stream delta）：
//   - base_ref 为必有字段（无 omitempty），worktree 任务落库全限定 ref（refs/heads/...、
//     refs/remotes/... 两形态）原样透出；
//   - 非 worktree（local-path/dir）与历史空值 worktree 任务为空串，字段仍存在，
//     详情不报错、不回填；
//   - REST 详情与 SSE 快照复用同一组装逻辑，同任务 base_ref 一致。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/application"
)

// TestTaskDetail_BaseRef_OutputMatrix REST 详情 base_ref 输出矩阵：两形态 ref 原样透传、
// 空 Key 恒存在（无 omitempty）、历史空值与非 worktree 任务空串合法（200 不报错）。
func TestTaskDetail_BaseRef_OutputMatrix(t *testing.T) {
	cases := []struct {
		desc       string
		projectRow storeProjectRow
		row        application.TaskRow
		wantRef    string
	}{
		{
			desc:       "worktree refs/heads verbatim",
			projectRow: storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo"},
			row:        func() application.TaskRow { r := fixtureTaskRow("t1"); r.BaseRef = "refs/heads/main"; return r }(),
			wantRef:    "refs/heads/main",
		},
		{
			desc:       "worktree refs/remotes verbatim",
			projectRow: storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo"},
			row: func() application.TaskRow {
				r := fixtureTaskRow("t1")
				r.BaseRef = "refs/remotes/origin/main"
				return r
			}(),
			wantRef: "refs/remotes/origin/main",
		},
		{
			desc:       "worktree historical empty stays empty",
			projectRow: storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo"},
			row:        fixtureTaskRow("t1"),
			wantRef:    "",
		},
		{
			desc:       "local-path task empty",
			projectRow: storeProjectRow{ID: "p1", Name: "projA", Path: "/p", DefaultBranch: "main", Kind: "repo"},
			row:        func() application.TaskRow { r := fixtureTaskRow("t1"); r.Mode = "local-path"; return r }(),
			wantRef:    "",
		},
		{
			desc:       "dir project local-path task empty",
			projectRow: storeProjectRow{ID: "p1", Name: "projA", Path: "/p", Kind: "dir"},
			row:        func() application.TaskRow { r := fixtureTaskRow("t1"); r.Mode = "local-path"; return r }(),
			wantRef:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			tb := newTaskStreamBackend(tc.row)
			s := newTaskStreamTestServer(t, tb, &fakeStreamSubscriber{}, 50*time.Millisecond, 5*time.Second)
			projs := newFakeProjectStore()
			projs.projects["p1"] = tc.projectRow
			s.projs = projs
			ts := httptest.NewServer(s.mux)
			defer ts.Close()

			resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (base_ref 空串为合法响应)", resp.StatusCode)
			}
			raw := readAllBody(t, resp.Body)
			// 子串同时证明键存在（无 omitempty）与值逐字同源（含空串形态 `"base_ref":""`）。
			if !strings.Contains(raw, `"base_ref":"`+tc.wantRef+`"`) {
				t.Errorf("detail body = %s, want base_ref %q (与持久化行同源)", raw, tc.wantRef)
			}
			var dto taskRowDTO
			if err := json.Unmarshal([]byte(raw), &dto); err != nil {
				t.Fatalf("decode: %v (body=%s)", err, raw)
			}
			if dto.BaseRef != tc.wantRef {
				t.Errorf("dto.BaseRef = %q, want %q", dto.BaseRef, tc.wantRef)
			}
		})
	}
}

// TestTaskStream_BaseRef_MatchesREST SSE 详情流与 REST 详情复用同一组装逻辑：
// 同任务 base_ref 一致且为落库值原样透传（SSE 推送路径无需单独改流代码）。
func TestTaskStream_BaseRef_MatchesREST(t *testing.T) {
	row := fixtureTaskRow("t1")
	row.BaseRef = "refs/remotes/origin/main"
	tb := newTaskStreamBackend(row)
	tb.agentStatusSnapshot = "idle"
	sub := &fakeStreamSubscriber{}
	s := newTaskStreamTestServer(t, tb, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	restResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer restResp.Body.Close()
	var rest taskRowDTO
	if err := json.NewDecoder(restResp.Body).Decode(&rest); err != nil {
		t.Fatal(err)
	}

	streamResp := openTaskStream(t, ts.URL, "t1")
	defer streamResp.Body.Close()
	frames := startSSEFrameReader(streamResp.Body)
	snap := nextFrame(t, frames, "snapshot")
	stream := decodeTaskDTO(t, snap.data)

	if rest.BaseRef == "" {
		t.Fatalf("REST base_ref = %q, want refs/remotes/origin/main", rest.BaseRef)
	}
	if stream.BaseRef != rest.BaseRef {
		t.Errorf("SSE base_ref = %q, want same as REST %q (共享组装)", stream.BaseRef, rest.BaseRef)
	}
	if stream.BaseRef != "refs/remotes/origin/main" {
		t.Errorf("SSE base_ref = %q, want verbatim refs/remotes/origin/main", stream.BaseRef)
	}
}
