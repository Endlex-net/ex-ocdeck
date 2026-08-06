package git

// add-plain-dir-project: ResolveBaseRef 真实 git 仓库探测测试。
// 覆盖：本地分支、远端-only 分支、同名 heads 优先、不存在 ref、context 取消传播。

import (
	"context"
	"strings"
	"testing"
)

// TestResolveBaseRef_LocalBranch 验证本地分支解析为 refs/heads/<name>。
func TestResolveBaseRef_LocalBranch(t *testing.T) {
	repo := newTestRepo(t)
	runGit(t, repo, "branch", "feature-x")
	got, err := ResolveBaseRef(context.Background(), repo, "feature-x")
	if err != nil {
		t.Fatalf("ResolveBaseRef local: %v", err)
	}
	if got != "refs/heads/feature-x" {
		t.Errorf("got = %q, want refs/heads/feature-x", got)
	}
}

// TestResolveBaseRef_RemoteOnly 验证仅远端分支（无同名本地）解析为 refs/remotes/<name>。
func TestResolveBaseRef_RemoteOnly(t *testing.T) {
	repo := newTestRepo(t)
	// 构造远端：bare remote + push main 为 origin/main，本地不建同名分支。
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare", "-q")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "origin", "main")
	got, err := ResolveBaseRef(context.Background(), repo, "origin/main")
	if err != nil {
		t.Fatalf("ResolveBaseRef remote-only: %v", err)
	}
	if got != "refs/remotes/origin/main" {
		t.Errorf("got = %q, want refs/remotes/origin/main", got)
	}
}

// TestResolveBaseRef_HeadsPriorityOverRemotes 验证同名时 heads 优先于 remotes。
// 本地分支 feature-x 与远端 origin/feature-x 同名短名 `feature-x`（本地）vs `origin/feature-x`（远端）；
// 直接同名短名指本地 feature-x，应解析为 refs/heads/feature-x。
func TestResolveBaseRef_HeadsPriorityOverRemotes(t *testing.T) {
	repo := newTestRepo(t)
	runGit(t, repo, "branch", "feature-x")
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare", "-q")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "origin", "feature-x")
	got, err := ResolveBaseRef(context.Background(), repo, "feature-x")
	if err != nil {
		t.Fatalf("ResolveBaseRef heads优先: %v", err)
	}
	if got != "refs/heads/feature-x" {
		t.Errorf("got = %q, want refs/heads/feature-x (heads优先)", got)
	}
}

// TestResolveBaseRef_Nonexistent 验证不存在 ref 返回错误。
func TestResolveBaseRef_Nonexistent(t *testing.T) {
	repo := newTestRepo(t)
	_, err := ResolveBaseRef(context.Background(), repo, "nonexistent-branch")
	if err == nil {
		t.Fatal("ResolveBaseRef nonexistent: want error, got nil")
	}
}

// TestResolveBaseRef_ContextCancel 验证 context 取消时错误传播（不误判为 ref 不存在）。
func TestResolveBaseRef_ContextCancel(t *testing.T) {
	repo := newTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 先取消，确保 rev-parse 启动即被取消。
	_, err := ResolveBaseRef(ctx, repo, "any-name")
	if err == nil {
		t.Fatal("ResolveBaseRef with canceled ctx: want error, got nil")
	}
	// 应反映 context 取消（而非 "not found as refs/heads/* or refs/remotes/*"）。
	if strings.Contains(err.Error(), "not found as refs/heads") {
		t.Errorf("err = %q, should reflect context cancel not 'not found'", err.Error())
	}
}

// TestResolveBaseRef_EmptyShortName 验证空短名返回错误。
func TestResolveBaseRef_EmptyShortName(t *testing.T) {
	repo := newTestRepo(t)
	_, err := ResolveBaseRef(context.Background(), repo, "")
	if err == nil {
		t.Fatal("ResolveBaseRef empty: want error, got nil")
	}
}

// TestResolveBaseRef_DefaultBranchMain 验证缺省 main 解析为 refs/heads/main。
func TestResolveBaseRef_DefaultBranchMain(t *testing.T) {
	repo := newTestRepo(t)
	got, err := ResolveBaseRef(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("ResolveBaseRef main: %v", err)
	}
	if got != "refs/heads/main" {
		t.Errorf("got = %q, want refs/heads/main", got)
	}
}