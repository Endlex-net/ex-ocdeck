package application

// dto_shape_test.go 验证 Attention DTO 的 JSON 序列化字段形状（opencode-v1-spi-provider
// tasks 3.6 验证项）：PendingPermission/PendingQuestion 去 opencode.* 嵌入改嵌
// provider-neutral payload 后，对外序列化字段逐字段不变（前端契约不漂移）。

import (
	"encoding/json"
	"strings"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
)

func TestDTOPendingShapes_JSONFieldShape(t *testing.T) {
	att := Attention{
		Permissions: []PendingPermission{{
			PermissionRequest: appagent.PermissionRequest{
				ID: "r1", SessionID: "ses_a", Permission: "bash",
				Patterns: []string{"rm"}, Metadata: []byte(`{"command":"ls"}`),
			},
			Since: 7,
		}},
		Questions: []PendingQuestion{{
			QuestionRequest: appagent.QuestionRequest{
				ID: "q1", SessionID: "ses_a",
				Questions: []appagent.QuestionItem{{Header: "方案", Question: "选哪个？"}},
			},
			Since: 9,
		}},
	}
	raw, err := json.Marshal(att)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	// 嵌入字段平铺（embedded 字段名不出现在 JSON）+ tag 与既有 opencode 契约逐字一致。
	for _, field := range []string{
		`{"id":"r1"`, `"sessionID":"ses_a"`, `"permission":"bash"`,
		`"patterns":["rm"]`, `"metadata":{"command":"ls"}`,
		`"Since":7`,
		`"id":"q1"`, `"questions":[{"header":"方案","question":"选哪个？"}]`,
		`"Since":9`,
	} {
		if !strings.Contains(got, field) {
			t.Errorf("DTO JSON 缺少字段 %s: %s", field, got)
		}
	}
	if strings.Contains(got, "PermissionRequest") || strings.Contains(got, "QuestionRequest") {
		t.Errorf("嵌入结构名不得出现在 JSON 中: %s", got)
	}
	// 空集合形状：空数组非 null（Attention spec）。
	empty, _ := json.Marshal(Attention{Permissions: []PendingPermission{}, Questions: []PendingQuestion{}})
	if string(empty) != `{"Permissions":[],"Questions":[]}` {
		t.Errorf("空集合 JSON = %s, want 非null空数组", empty)
	}
	// metadata omitempty：nil 时不出现字段。
	noMeta, _ := json.Marshal(Attention{Permissions: []PendingPermission{{
		PermissionRequest: appagent.PermissionRequest{ID: "r2", SessionID: "s"}, Since: 1,
	}}, Questions: []PendingQuestion{}})
	if strings.Contains(string(noMeta), `"metadata"`) {
		t.Errorf("nil metadata 必须省略: %s", noMeta)
	}
}
