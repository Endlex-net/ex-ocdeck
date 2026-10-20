// v2_adopt_test.go 落实 v1-legacy-anchor-adopt tasks 1.2：D5 判定矩阵 legacy-A
// 变体（无 binding 老任务形态）命中与拒绝边界，及既有 A/B/stop 形态不回归。
package task

import (
	"testing"

	domain "ocdeck/internal/domain/agentprovider"
	ocdeckstore "ocdeck/internal/infrastructure/store"
)

func TestClassifyAdoption_LegacyAnchorVariants(t *testing.T) {
	v1Key := func(upstream string) domain.SessionKey {
		return domain.SessionKey{ProviderID: ocdeckstore.ProviderIDV1, ProviderInstanceID: ocdeckstore.ProviderInstanceV1Default, UpstreamID: upstream}
	}
	v2Key := func(upstream string) domain.SessionKey {
		return domain.SessionKey{ProviderID: ocdeckstore.ProviderIDV2, ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default, UpstreamID: upstream}
	}
	noBinding := domain.TaskBinding{}
	cases := []struct {
		name       string
		hasBinding bool
		binding    domain.TaskBinding
		anchorID   string
		anchorPID  string
		anchorIID  string
		keys       []domain.SessionKey
		wantBranch adoptionBranch
		wantTarget string
	}{
		{
			name:       "legacy-A 命中：binding 空 + anchor 三元组 v1 + anchor ∈ 归属",
			hasBinding: false, binding: noBinding,
			anchorID: "sess-a", anchorPID: ocdeckstore.ProviderIDV1, anchorIID: ocdeckstore.ProviderInstanceV1Default,
			keys:       []domain.SessionKey{v1Key("sess-a"), v1Key("sess-a-child")},
			wantBranch: branchA, wantTarget: "sess-a",
		},
		{
			name:       "legacy-A 拒绝：anchor 不在归属集合",
			hasBinding: false, binding: noBinding,
			anchorID: "sess-a", anchorPID: ocdeckstore.ProviderIDV1, anchorIID: ocdeckstore.ProviderInstanceV1Default,
			keys:       []domain.SessionKey{v1Key("sess-other")},
			wantBranch: branchStop,
		},
		{
			name:       "legacy-A 拒绝：anchor 为空（部分三元组不误归 B）",
			hasBinding: false, binding: noBinding,
			anchorID: "", anchorPID: ocdeckstore.ProviderIDV1, anchorIID: ocdeckstore.ProviderInstanceV1Default,
			keys:       []domain.SessionKey{v1Key("sess-a")},
			wantBranch: branchStop,
		},
		{
			name:       "legacy-A 拒绝：归属含 v2 行（混杂）",
			hasBinding: false, binding: noBinding,
			anchorID: "sess-a", anchorPID: ocdeckstore.ProviderIDV1, anchorIID: ocdeckstore.ProviderInstanceV1Default,
			keys:       []domain.SessionKey{v1Key("sess-a"), v2Key("sess-a")},
			wantBranch: branchStop,
		},
		{
			name:       "既有 A 不回归：v1 binding 三者一致",
			hasBinding: true,
			binding:    domain.TaskBinding{ProviderID: ocdeckstore.ProviderIDV1, ProviderInstanceID: ocdeckstore.ProviderInstanceV1Default, UpstreamSessionID: "sess-a"},
			anchorID:   "sess-a", anchorPID: ocdeckstore.ProviderIDV1, anchorIID: ocdeckstore.ProviderInstanceV1Default,
			keys:       []domain.SessionKey{v1Key("sess-a")},
			wantBranch: branchA, wantTarget: "sess-a",
		},
		{
			name:       "B 不回归：三者一致为空",
			hasBinding: false, binding: noBinding,
			anchorID:   "",
			keys:       nil,
			wantBranch: branchB,
		},
		{
			// 负例钉住（回归保护）：既有 v1 binding 但 upstream 与 anchor 不一致
			//（anchor 在册也不放行——id 不一致属判定不一致，MUST NOT 走 legacy-A）。
			name:       "stop：v1 binding upstream ≠ anchor",
			hasBinding: true,
			binding:    domain.TaskBinding{ProviderID: ocdeckstore.ProviderIDV1, ProviderInstanceID: ocdeckstore.ProviderInstanceV1Default, UpstreamSessionID: "sess-b"},
			anchorID:   "sess-a", anchorPID: ocdeckstore.ProviderIDV1, anchorIID: ocdeckstore.ProviderInstanceV1Default,
			keys:       []domain.SessionKey{v1Key("sess-a")},
			wantBranch: branchStop,
		},
		{
			// 负例钉住：anchor 已在 v2 命名空间（已承接过/损坏）→ stop。
			name:       "stop：anchor 命名空间非 v1/default",
			hasBinding: false, binding: noBinding,
			anchorID: "sess-a", anchorPID: ocdeckstore.ProviderIDV2, anchorIID: ocdeckstore.ProviderInstanceV2Default,
			keys:       []domain.SessionKey{v1Key("sess-a")},
			wantBranch: branchStop,
		},
		{
			// 负例钉住：归属行含未知命名空间 → stop（不猜测）。
			name:       "stop：归属行含未知命名空间",
			hasBinding: false, binding: noBinding,
			anchorID: "sess-a", anchorPID: ocdeckstore.ProviderIDV1, anchorIID: ocdeckstore.ProviderInstanceV1Default,
			keys:       []domain.SessionKey{v1Key("sess-a"), {ProviderID: "other", ProviderInstanceID: "default", UpstreamID: "sess-x"}},
			wantBranch: branchStop,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			branch, target := classifyAdoption(tc.hasBinding, tc.binding, tc.anchorID, tc.anchorPID, tc.anchorIID, tc.keys)
			if branch != tc.wantBranch || target != tc.wantTarget {
				t.Fatalf("classifyAdoption = (%v, %q), want (%v, %q)", branch, target, tc.wantBranch, tc.wantTarget)
			}
		})
	}
}
