package api

import (
	"encoding/json"
	"strings"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/opencode"
)

// TestToAttentionDTO_NoMetadataLeak tasks 8.1：metadata 仅供内部判定消费，
// MUST NOT 泄入对外 DTO（id/permission/patterns/since 投影零改动）。
func TestToAttentionDTO_NoMetadataLeak(t *testing.T) {
	att := application.Attention{
		Permissions: []application.PendingPermission{
			{
				PermissionRequest: opencode.PermissionRequest{
					ID: "r1", SessionID: "s1", Permission: "bash",
					Patterns: []string{"rm"},
					Metadata: json.RawMessage(`{"command":"rm -rf /secret"}`),
				},
				Since: 1234,
			},
		},
		Questions: []application.PendingQuestion{
			{QuestionRequest: opencode.QuestionRequest{
				ID: "q1", SessionID: "s1",
				Questions: []opencode.QuestionItem{{Header: "h", Question: "q"}},
			}, Since: 5678},
		},
	}
	dto := toAttentionDTO(att)
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal DTO: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "metadata") || strings.Contains(s, "rm -rf") || strings.Contains(s, "secret") {
		t.Errorf("DTO MUST NOT leak metadata, got %s", s)
	}
	// 投影字段不变（id/permission/patterns/since）。
	if len(dto.Permissions) != 1 {
		t.Fatalf("permissions = %d, want 1", len(dto.Permissions))
	}
	p := dto.Permissions[0]
	if p.ID != "r1" || p.Permission != "bash" || p.Since != 1234 || len(p.Patterns) != 1 {
		t.Errorf("permission projection changed: %+v", p)
	}
}
