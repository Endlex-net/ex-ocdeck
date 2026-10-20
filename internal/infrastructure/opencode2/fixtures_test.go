// fixtures_test.go 落实 tasks 2.1：v2 testdata fixture 集（v2 wire 形状）的
// 可解析加载与形状自检——fixture 是本包全部适配器单测的 wire 事实来源，
// 形状漂移必须在本测试先行暴露。
package opencode2

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// decodeFixture 解析 JSON fixture 并断言 data envelope（v2 成功响应统一包裹）。
func decodeFixture(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	raw := readFixture(t, name)
	if !json.Valid(raw) {
		t.Fatalf("fixture %s: invalid json", name)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("fixture %s: decode: %v", name, err)
	}
	if _, ok := m["data"]; !ok {
		t.Fatalf("fixture %s: missing data envelope", name)
	}
	return m
}

// fixtureSession 断言 Session.Info 最小形状（id ^ses、time.created/updated
// 数字、location.directory 非空）并返回 data。
func fixtureSession(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	data := decodeFixture(t, name)["data"].(map[string]interface{})
	id, _ := data["id"].(string)
	if !strings.HasPrefix(id, "ses") {
		t.Fatalf("fixture %s: session id %q not ^ses", name, id)
	}
	tm, ok := data["time"].(map[string]interface{})
	if !ok {
		t.Fatalf("fixture %s: missing time object", name)
	}
	if _, ok := tm["created"].(float64); !ok {
		t.Fatalf("fixture %s: time.created not number", name)
	}
	if _, ok := tm["updated"].(float64); !ok {
		t.Fatalf("fixture %s: time.updated not number", name)
	}
	loc, ok := data["location"].(map[string]interface{})
	if !ok {
		t.Fatalf("fixture %s: missing location object", name)
	}
	if dir, _ := loc["directory"].(string); dir == "" {
		t.Fatalf("fixture %s: location.directory empty", name)
	}
	return data
}

func TestFixtures_SessionShapes(t *testing.T) {
	fixtureSession(t, "session_create.json")
	fixtureSession(t, "session_get.json")

	page1 := decodeFixture(t, "session_list_page1.json")
	items, ok := page1["data"].([]interface{})
	if !ok || len(items) != 2 {
		t.Fatalf("session_list_page1: data must be array of 2")
	}
	cursor, ok := page1["cursor"].(map[string]interface{})
	if !ok {
		t.Fatalf("session_list_page1: missing cursor object")
	}
	if cursor["next"] == nil {
		t.Fatalf("session_list_page1: cursor.next must be non-null (overflow signal fixture)")
	}
	for i, item := range items {
		id, _ := item.(map[string]interface{})["id"].(string)
		if !strings.HasPrefix(id, "ses") {
			t.Fatalf("session_list_page1: item %d id %q not ^ses", i, id)
		}
	}
	page2 := decodeFixture(t, "session_list_page2.json")
	cursor2 := page2["cursor"].(map[string]interface{})
	if cursor2["next"] != nil {
		t.Fatalf("session_list_page2: cursor.next must be null (last page fixture)")
	}
}

func TestFixtures_PermissionPending(t *testing.T) {
	data := decodeFixture(t, "permission_pending.json")["data"].([]interface{})
	req := data[0].(map[string]interface{})
	if id, _ := req["id"].(string); !strings.HasPrefix(id, "per") {
		t.Fatalf("permission id %q not ^per", id)
	}
	if req["action"] == "" || req["sessionID"] == "" {
		t.Fatalf("permission request missing action/sessionID")
	}
	if _, ok := req["resources"].([]interface{}); !ok {
		t.Fatalf("permission request missing resources array")
	}
}

func TestFixtures_FormFamily(t *testing.T) {
	list := decodeFixture(t, "form_list.json")["data"].([]interface{})
	if len(list) != 2 {
		t.Fatalf("form_list: expect 2 forms")
	}
	for i, item := range list {
		id, _ := item.(map[string]interface{})["id"].(string)
		if !strings.HasPrefix(id, "frm_") {
			t.Fatalf("form_list: item %d id %q not ^frm_", i, id)
		}
	}
	pending := decodeFixture(t, "form_detail_pending.json")["data"].(map[string]interface{})
	if st := pending["state"].(map[string]interface{})["status"]; st != "pending" {
		t.Fatalf("form_detail_pending: state.status=%v", st)
	}
	answered := decodeFixture(t, "form_detail_answered.json")["data"].(map[string]interface{})
	if st := answered["state"].(map[string]interface{})["status"]; st != "answered" {
		t.Fatalf("form_detail_answered: state.status=%v", st)
	}
}

func TestFixtures_PromptAccepted(t *testing.T) {
	data := decodeFixture(t, "prompt_accepted.json")["data"].(map[string]interface{})
	if id, _ := data["id"].(string); !strings.HasPrefix(id, "msg_") {
		t.Fatalf("prompt data.id %q not ^msg_", id)
	}
	if data["sessionID"] == "" || data["type"] != "user" {
		t.Fatalf("prompt inbox item missing sessionID/type")
	}
}

func TestFixtures_OpenAPIDoc(t *testing.T) {
	raw := readFixture(t, "openapi_doc.json")
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Pattern string `json:"pattern"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("openapi_doc: %v", err)
	}
	if doc.Components.Schemas["Session.ID"].Pattern != "^ses" {
		t.Fatalf("Session.ID pattern: %q", doc.Components.Schemas["Session.ID"].Pattern)
	}
	if doc.Components.Schemas["Session.Message.ID"].Pattern != "^msg_" {
		t.Fatalf("Session.Message.ID pattern: %q", doc.Components.Schemas["Session.Message.ID"].Pattern)
	}
}

func TestFixtures_SSEFrames(t *testing.T) {
	raw := readFixture(t, "sse_frames.txt")
	var types []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var ev struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("sse frame payload: %v (%s)", err, payload)
		}
		if ev.Type == "" {
			t.Fatalf("sse frame missing type: %s", payload)
		}
		if ev.Data == nil {
			t.Fatalf("sse frame missing data object: %s", ev.Type)
		}
		types = append(types, ev.Type)
	}
	if len(types) == 0 || types[0] != "server.connected" {
		t.Fatalf("first frame must be server.connected, got %v", types)
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"session.execution.started", "session.renamed", "session.execution.succeeded", "permission.asked", "session.created", streamFailureType, "session.unknown.probe"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("sse_frames missing event type %s", want)
		}
	}
	if !bytes.Contains(raw, []byte(`"data":{"title":"no-owner"}`)) {
		t.Fatalf("sse_frames missing attribution-reject fixture (renamed without sessionID)")
	}
}
