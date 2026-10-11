package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// spi_title_pipeline_test.go 落定 C2a 疑点① 的处置方案与证据（tasks 3.6 形状
// 对齐）：D7 SessionControl.Create 无 title 参数（创建发生在 SSE 订阅建立前，
// 无观察者可见中间态），锚定 session 标题由 TitleUpdater.Sync 补齐——与现状
// 「POST /session 即携带任务名标题」终态等价（上游 title 相同）。本测试证明
// 管线闭环：Create → 空 title → Sync → 上游 title=任务名。
func TestSPI_CreateThenTitleSync_PipelineEquivalence(t *testing.T) {
	var titles sync.Map // sessionID → title
	ts := newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			var body struct {
				Title string `json:"title"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Title != "" {
				t.Errorf("D7 形状：Create 请求不得携带 title（收到 %q）", body.Title)
			}
			titles.Store("ses_new", "")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_new", "time": map[string]any{"created": 1, "updated": 2}})
		case strings.HasPrefix(r.URL.Path, "/session/") && r.Method == http.MethodPatch:
			var body struct {
				Title string `json:"title"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			titles.Store(strings.TrimPrefix(r.URL.Path, "/session/"), body.Title)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_new", "time": map[string]any{"created": 1, "updated": 2}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	p := NewSPIProvider()
	ctx := context.Background()
	b := ts.testBinding()

	// ① Create（D7 形状，无 title）：上游 title 为空。
	s, err := p.Sessions().Create(ctx, b)
	if err != nil || s.Key.UpstreamID != "ses_new" {
		t.Fatalf("Create = (%+v, %v)", s, err)
	}
	if v, _ := titles.Load("ses_new"); v != "" {
		t.Errorf("创建后上游 title = %v, want 空（标题经 TitleUpdater 承载）", v)
	}
	// ② TitleUpdater 补齐：终态 title=任务名（与现状 POST 即带标题等价）。
	if err := p.Titles().Sync(ctx, b, s.Key, "task t1"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if v, _ := titles.Load("ses_new"); v != "task t1" {
		t.Errorf("补齐后上游 title = %v, want task t1", v)
	}
}
