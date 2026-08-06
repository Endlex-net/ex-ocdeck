package git

import (
	"context"
	"fmt"
	"strings"
)

// ResolveBaseRef 将 base_ref 短名（如 `feature-x` 或 `origin/feature-x`）解析为全限定 ref
// （add-plain-dir-project D10：repo 任务创建可选基线分支）。
//
// 解析顺序：`refs/heads/<name>` → `refs/remotes/<name>`（heads 优先：本地与远端同名时解析为
// 本地分支）。仅接受这两个命名空间，拒绝 tag/SHA/任意表达式。调用方 MUST 在此前已用
// `ValidateBranchName`（git check-ref-format --branch）做规范校验；本函数不再重复 check-ref-format。
//
// 探测用 `git rev-parse --verify --quiet --end-of-options <ref>`（无副作用只读）。
// --end-of-options 防止 ref 以 `-` 开头被当作选项注入（与 ResolveRef 防护一致，ops.go/worktree.go）。
// 命中即返回全限定 ref；两者都不存在返回错误（调用方映射 invalid_input）。
//
// 只读操作，不进 repo 写锁。argv 数组、context 取消、有界读取复用 run()（exec.go）。
func ResolveBaseRef(ctx context.Context, repoPath, shortName string) (string, error) {
	if shortName == "" {
		return "", fmt.Errorf("git: empty base_ref short name")
	}
	candidates := []string{
		"refs/heads/" + shortName,
		"refs/remotes/" + shortName,
	}
	for _, ref := range candidates {
		out, _, err := run(ctx, repoPath, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
		if err != nil {
			// rev-parse --verify --quiet 对不存在的 ref 退出码非零、stderr 为空。
			// run() 对一切非零退出都把底层 err（*exec.ExitError）挂到 commandError.err，
			// 故不能靠 ce.err==nil 判定；用 ctx.Err() 区分 context 取消/超时（真实错误需传播），
			// 其余非零退出视为"该 ref 不存在"，继续尝试下一个命名空间。
			if isRevParseNoSuchRef(ctx, err) {
				continue
			}
			return "", fmt.Errorf("git rev-parse %q: %w", ref, err)
		}
		if strings.TrimSpace(out) == "" {
			continue
		}
		return ref, nil
	}
	return "", fmt.Errorf("git: base_ref %q not found as refs/heads/* or refs/remotes/*", shortName)
}

// isRevParseNoSuchRef 判断 rev-parse --verify --quiet 错误是否为"ref 不存在"（非系统错误）。
// run() 对非零退出统一设置 commandError.err = *exec.ExitError，故用 ctx.Err() 排除 context
// 取消/超时（真实错误需向上传播）；ctx 未取消时的非零退出视为 ref 不存在。
func isRevParseNoSuchRef(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	// context 取消/超时（含 run() 透传的 exec.ExitError 信号终止场景）需传播，不视为"ref 不存在"。
	if ctx.Err() != nil {
		return false
	}
	if _, ok := err.(*commandError); ok {
		return true
	}
	return false
}