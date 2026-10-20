// resolver_test.go 落实 tasks 6.3/6.6：D4 决策表 R1–R12 全组合语义与错误分类
// （R2/R6/R9→CapabilityUnavailable、R3/R7→ProviderNotFound；binding 优先路由；
// 多条件同时满足按首个命中优先级）。
package task

import (
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
	ocdeckstore "ocdeck/internal/infrastructure/store"
)

func TestResolveExecutionPath_R1ToR12(t *testing.T) {
	v1 := ocdeckstore.ProviderIDV1
	v2 := ocdeckstore.ProviderIDV2
	type tc struct {
		name             string
		binding, pinned  string
		major            int
		enabled          bool
		wantExec         string
		wantAdopt        bool
		wantDeny         appagent.ErrorKind
		wantDenyNotEmpty bool
	}
	on := true
	cases := []tc{
		// R1–R3：已有 v2 binding 优先于钉扎判定。
		{"R1 v2-binding+machine2+on", v2, v1, 2, on, v2, false, "", false},
		{"R1b v2-binding+machine2+off→R3", v2, v1, 2, false, "", false, appagent.ErrKindProviderNotFound, true},
		{"R2a v2-binding+machine1", v2, v1, 1, on, "", false, appagent.ErrKindCapabilityUnavailable, true},
		{"R2b v2-binding+unknown+off（R2 优先 R3）", v2, v1, 0, false, "", false, appagent.ErrKindCapabilityUnavailable, true},
		// R5–R7：v2 钉扎。
		{"R5 pinned-v2+machine2+on", "", v2, 2, on, v2, false, "", false},
		{"R6a pinned-v2+machine1+on", "", v2, 1, on, "", false, appagent.ErrKindCapabilityUnavailable, true},
		{"R6b pinned-v2+unknown+off（R6 优先 R7）", "", v2, 0, false, "", false, appagent.ErrKindCapabilityUnavailable, true},
		{"R7 pinned-v2+machine2+off", "", v2, 2, false, "", false, appagent.ErrKindProviderNotFound, true},
		// R8–R11：v1 钉扎。
		{"R8 pinned-v1+machine2+on→承接", "", v1, 2, on, v2, true, "", false},
		{"R9 pinned-v1+machine2+off", "", v1, 2, false, "", false, appagent.ErrKindCapabilityUnavailable, true},
		{"R10 pinned-v1+machine1", "", v1, 1, on, v1, false, "", false},
		{"R10b pinned-v1+machine1+off（开关不影响纯 v1）", "", v1, 1, false, v1, false, "", false},
		{"R11 pinned-v1+unknown→纯 v1 保守不承接", "", v1, 0, on, v1, false, "", false},
		// 数据异常：钉扎值未知 → fail-closed 不猜测。
		{"unknown pinned provider", "", "codex", 2, on, "", false, appagent.ErrKindProviderNotFound, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveExecutionPath(c.binding, c.pinned, machineFacts{MachineMajor: c.major, V2Enabled: c.enabled})
			if c.wantDenyNotEmpty {
				if got.DenyKind == "" || got.DenyKind != c.wantDeny {
					t.Fatalf("deny: got %+v want %v", got, c.wantDeny)
				}
				if got.DenyReason == "" {
					t.Fatalf("deny reason must be user-perceivable")
				}
				if got.ExecProviderID != "" {
					t.Fatalf("deny must not carry exec provider: %+v", got)
				}
				return
			}
			if got.DenyKind != "" {
				t.Fatalf("unexpected deny: %+v", got)
			}
			if got.ExecProviderID != c.wantExec {
				t.Fatalf("exec: got %s want %s", got.ExecProviderID, c.wantExec)
			}
			if got.Adopt != c.wantAdopt {
				t.Fatalf("adopt: got %v want %v", got.Adopt, c.wantAdopt)
			}
		})
	}
}

func TestResolveExecutionPath_R9HintMentionsSwitch(t *testing.T) {
	got := resolveExecutionPath("", ocdeckstore.ProviderIDV1, machineFacts{MachineMajor: 2, V2Enabled: false})
	if got.DenyKind != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("kind: %v", got.DenyKind)
	}
	// R9 仅在开关关闭形态触发：提示须含开关名与恢复方式（不断言关闭原因——
	// 显式关闭与未知值保守关闭均进此分支）。
	for _, sub := range []string{"OCDECK_V2_ADOPTION", "移除该环境变量或设为 1"} {
		if !containsSub(got.DenyReason, sub) {
			t.Fatalf("R9 hint missing %q: %q", sub, got.DenyReason)
		}
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
