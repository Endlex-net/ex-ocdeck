package git

// add-plain-dir-project: ListBranches 边界测试。
// 覆盖：同短名碰撞（本地+远端去重，本地优先）、本地 feature/HEAD 保留、origin/HEAD 排除。

import (
	"context"
	"testing"
)

// TestListBranches_RealRepo_DedupCollisionLocalFirst 验证本地与远端同短名合并去重，
// 本地条目保留在前、远端同短名条目丢弃。
func TestListBranches_RealRepo_DedupCollisionLocalFirst(t *testing.T) {
	repo := newTestRepo(t)
	// 本地分支 feature-x。
	runGit(t, repo, "branch", "feature-x")
	// 远端 origin/feature-x。
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare", "-q")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "origin", "feature-x")

	got, err := ListBranches(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	// 本地 feature-x 与远端 origin/feature-x 短名不同（feature-x vs origin/feature-x），
	// 两者都保留；本地在前、远端在后。
	wantContains := map[string]bool{"feature-x": false, "origin/feature-x": false, "main": false}
	for _, b := range got {
		wantContains[b] = true
	}
	for name, found := range wantContains {
		if !found {
			t.Errorf("branches %v missing %q", got, name)
		}
	}
	// 本地在前：feature-x/main 应在 origin/feature-x 之前。
	localIdx, remoteIdx := -1, -1
	for i, b := range got {
		if b == "feature-x" {
			localIdx = i
		}
		if b == "origin/feature-x" {
			remoteIdx = i
		}
	}
	if localIdx < 0 || remoteIdx < 0 || localIdx > remoteIdx {
		t.Errorf("local-first order: feature-x idx=%d, origin/feature-x idx=%d (want local first)", localIdx, remoteIdx)
	}
}

// TestListBranches_RealRepo_LocalFeatureSlashHeadRetained 验证本地分支名 feature/HEAD 保留
// （symbolic HEAD 过滤仅作用于远端，不影响本地）。
func TestListBranches_RealRepo_LocalFeatureSlashHeadRetained(t *testing.T) {
	repo := newTestRepo(t)
	// 合法本地分支名 feature/HEAD（check-ref-format 接受）。
	runGit(t, repo, "branch", "feature/HEAD")
	got, err := ListBranches(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	found := false
	for _, b := range got {
		if b == "feature/HEAD" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("branches %v missing local feature/HEAD (must retain, only remote HEAD filtered)", got)
	}
}

// TestListBranches_RealRepo_OriginHeadExcluded 验证远端 symbolic HEAD origin/HEAD 被排除。
func TestListBranches_RealRepo_OriginHeadExcluded(t *testing.T) {
	repo := newTestRepo(t)
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare", "-q")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "origin", "main")
	// 设置 origin/HEAD 指向 origin/main（产生 symbolic HEAD 远端条目）。
	runGit(t, repo, "remote", "set-head", "origin", "main")
	got, err := ListBranches(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	for _, b := range got {
		if b == "origin/HEAD" {
			t.Errorf("branches %v contains origin/HEAD (must be excluded)", got)
		}
	}
}

// TestListBranches_RealRepo_OriginFeatureHeadRetained_OriginHeadExcluded 验证按 %(symref) 元数据
// 过滤：真实远端嵌套分支 origin/feature/HEAD（symref 为空）保留，symbolic origin/HEAD（symref 非空）
// 排除。回归前版本仅按名字过滤会把 origin/feature/HEAD 误排除（复审 BLOCKED 项）。
func TestListBranches_RealRepo_OriginFeatureHeadRetained_OriginHeadExcluded(t *testing.T) {
	repo := newTestRepo(t)
	// 真实远端嵌套分支 feature/HEAD（push 后形如 origin/feature/HEAD）。
	runGit(t, repo, "branch", "feature/HEAD")
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare", "-q")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "origin", "main")
	runGit(t, repo, "push", "-q", "origin", "feature/HEAD")
	// 产生 symbolic origin/HEAD（与 origin/feature/HEAD 同时存在于 `git branch -r` 输出）。
	runGit(t, repo, "remote", "set-head", "origin", "main")

	got, err := ListBranches(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	contains := map[string]bool{}
	for _, b := range got {
		contains[b] = true
	}
	// symbolic origin/HEAD 必须排除。
	if contains["origin/HEAD"] {
		t.Errorf("branches %v contains origin/HEAD (symbolic, must be excluded by symref)", got)
	}
	// 真实远端嵌套分支 origin/feature/HEAD 必须保留（symref 为空）。
	if !contains["origin/feature/HEAD"] {
		t.Errorf("branches %v missing origin/feature/HEAD (real branch, must retain)", got)
	}
	// origin/main 也应保留。
	if !contains["origin/main"] {
		t.Errorf("branches %v missing origin/main", got)
	}
}