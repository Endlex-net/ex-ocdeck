package opencode

import (
	"encoding/json"
	"testing"
)

// TestNormalizePermissionMetadata 归一化矩阵（D12）：缺失/null/非法 JSON/合法非
// object 一律归 nil；有效 object 原样保留。
func TestNormalizePermissionMetadata(t *testing.T) {
	cases := []struct {
		name string
		in   string // "" = nil（缺失）
		want string // "" = 期望 nil
	}{
		{name: "缺失", in: "", want: ""},
		{name: "null", in: `null`, want: ""},
		{name: "非法 JSON", in: `{"command":`, want: ""},
		{name: "合法非 object（string）", in: `"ls"`, want: ""},
		{name: "合法非 object（number）", in: `42`, want: ""},
		{name: "合法非 object（array）", in: `["a"]`, want: ""},
		{name: "空 object 保留", in: `{}`, want: `{}`},
		{name: "有效 object 保留", in: `{"command":"ls -la"}`, want: `{"command":"ls -la"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.in != "" {
				raw = json.RawMessage(tc.in)
			}
			got := normalizePermissionMetadata(raw)
			if tc.want == "" {
				if got != nil {
					t.Errorf("normalize(%s) = %s, want nil", tc.in, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("normalize(%s) = nil, want %s", tc.in, tc.want)
			}
			if string(got) != tc.want {
				t.Errorf("normalize(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestParsePermissionAsked_Metadata SSE 入口（tasks 8.1）：metadata 捕获与归一化，
// 缺失/非法不丢请求。
func TestParsePermissionAsked_Metadata(t *testing.T) {
	cases := []struct {
		name       string
		props      map[string]interface{}
		wantNil    bool
		wantAbsent bool // 期待 metadata 字段缺失（nil 且未捕获）
	}{
		{
			name: "携带 object",
			props: map[string]interface{}{
				"id": "r1", "sessionID": "s1", "permission": "bash",
				"patterns": []interface{}{"rm"},
				"metadata": map[string]interface{}{"command": "ls"},
			},
		},
		{
			name: "缺失 metadata（合法，归 nil）",
			props: map[string]interface{}{
				"id": "r1", "sessionID": "s1", "permission": "bash",
				"patterns": []interface{}{"rm"},
			},
			wantNil: true,
		},
		{
			name: "metadata 非 object（归 nil，不丢请求）",
			props: map[string]interface{}{
				"id": "r1", "sessionID": "s1", "permission": "bash",
				"patterns": []interface{}{"rm"},
				"metadata": "corrupt",
			},
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := parsePermissionAsked(tc.props)
			if !ok {
				t.Fatal("parsePermissionAsked must not drop the request")
			}
			if tc.wantNil {
				if len(ev.Metadata) != 0 {
					t.Errorf("metadata = %s, want nil", ev.Metadata)
				}
				return
			}
			if len(ev.Metadata) == 0 {
				t.Fatal("metadata MUST be captured")
			}
			var obj map[string]string
			if err := json.Unmarshal(ev.Metadata, &obj); err != nil || obj["command"] != "ls" {
				t.Errorf("metadata = %s, want {command: ls}", ev.Metadata)
			}
		})
	}
}

// TestParsePermissionRequest_Metadata REST 入口（tasks 8.1）：object 捕获、
// 缺失归 nil、非法 JSON 字段值归 nil、不丢请求。
func TestParsePermissionRequest_Metadata(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantCmd string // "" = 期待 nil
	}{
		{name: "携带 object", raw: `{"id":"r1","sessionID":"s1","permission":"bash","patterns":["rm"],"metadata":{"command":"ls"}}`, wantCmd: "ls"},
		{name: "缺失 metadata → nil", raw: `{"id":"r1","sessionID":"s1","permission":"bash","patterns":["rm"]}`},
		{name: "metadata null → nil", raw: `{"id":"r1","sessionID":"s1","permission":"bash","metadata":null}`},
		{name: "metadata 非 object → nil", raw: `{"id":"r1","sessionID":"s1","permission":"bash","metadata":"oops"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parsePermissionRequest(jsonRawObject(tc.raw))
			if err != nil {
				t.Fatalf("parsePermissionRequest: %v", err)
			}
			if tc.wantCmd == "" {
				if len(r.Metadata) != 0 {
					t.Errorf("metadata = %s, want nil", r.Metadata)
				}
				return
			}
			var obj map[string]string
			if err := json.Unmarshal(r.Metadata, &obj); err != nil || obj["command"] != tc.wantCmd {
				t.Errorf("metadata = %s, want command=%s", r.Metadata, tc.wantCmd)
			}
		})
	}
}
