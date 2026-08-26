// Package worktree 实现任务 worktree 的创建与删除（design.md §6、§18）。
//
// 位置约定：<dataDir>/worktrees/...（具体路径由调用方计算并传入 dest，ai-worktree-naming）。
// 创建：每 repo 写锁（git.RepoLock）串行，check-ref-format 校验分支名，add 前 prune；
// 任何文件/git 副作用前 MUST 先 checkContainment(dest)，拒绝 dest 逃逸 worktrees 根。
// 删除：canonical path + filepath.Rel 包含性校验，dirty 检查，分支占用检查，prune。
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ocdeck/internal/infrastructure/git"
)

// Manager 管理 worktree 的创建与删除，基于 config.DataDir。
type Manager struct {
	dataDir string
}

// New 构造 Manager。dataDir MUST 已绝对路径化（由 config 层保证）。
func New(dataDir string) *Manager {
	return &Manager{dataDir: filepath.Clean(dataDir)}
}

// Add 在 dest 创建 worktree，基于 baseRef 新建 branch。dest 由调用方计算（绝对路径），
// MUST 位于 <dataDir>/worktrees/ 之下（副作用前先 checkContainment 校验）。
// 每 repo 写锁串行化仓库级写操作（design.md §6/§17）。
//
// 幂等性/补偿（design.md §19）：目标存在检查在 repo 写锁内完成，避免并发 Add
// 在锁外各自通过存在性检查后锁内争抢；WorktreeAdd 失败时回收"本次创建"的产物——
// worktree 目录、新建分支、worktree metadata（prune）——MUST 限定为本操作产物，
// 不得删除并发另一次成功创建的 worktree（锁内串行保证本 Add 独占，故回收安全）。
func (m *Manager) Add(ctx context.Context, repoPath, dest, branch, baseRef string) error {
	if branch == "" {
		return errors.New("worktree: empty branch")
	}
	if baseRef == "" {
		return errors.New("worktree: empty base ref")
	}

	// 副作用前 MUST 先做包含性校验：dest 逃逸 worktrees 根时拒绝，且无任何文件/git 副作用。
	if err := m.checkContainment(dest); err != nil {
		return err
	}

	// 仓库级写锁（add-plain-dir-project D10：升级为 context-aware AcquireRepoLock，
	// 与 refresh fetch、GitPush(-u) 串行；ctx 取消及时返回不进锁）。
	unlock, err := git.AcquireRepoLock(ctx, repoPath)
	if err != nil {
		return err
	}
	defer unlock()

	// 目标已存在检查移入锁内（B8）：锁外检查通过后锁内可能被并发 Add 抢先创建。
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("worktree: target already exists: %s", dest)
	}

	if err := git.WorktreePrune(ctx, repoPath); err != nil {
		return fmt.Errorf("worktree: prune before add: %w", err)
	}
	if err := git.ValidateBranchName(ctx, repoPath, branch); err != nil {
		return err
	}
	// 记录分支创建前是否存在（B1）：补偿时仅当分支确为本次创建才删，
	// 不得 branch -D 既有分支（design.md §19 Create 补偿范围限定）。
	branchExistedBefore := gitBranchExists(ctx, repoPath, branch)
	if err := git.WorktreeAdd(ctx, repoPath, dest, branch, baseRef); err != nil {
		// 失败全量补偿（B8）：回收本次创建的 worktree 目录、分支、worktree metadata。
		// 锁内串行保证此时无并发 Add 干扰，回收的是本操作产物。
		// B1：仅当分支在本次 Add 前不存在（即本次创建）才删除，避免误删既有分支。
		// branchCreatedHere = !branchExistedBefore：补偿方向 MUST 与"是否本次创建"一致，
		// 反向传入会误删既有分支、残留新建分支。
		cleanupFailedAdd(ctx, repoPath, dest, branch, !branchExistedBefore)
		return fmt.Errorf("worktree: add: %w", err)
	}
	return nil
}

// gitBranchExists 判断分支是否已存在（用于补偿范围判定，B1）。
// 保守语义：not-found（分支确不存在）→ false；其他 git 错误（仓库损坏/权限等无法判定）
// 视为"可能存在"（true），避免补偿误删可能存在的分支。
func gitBranchExists(ctx context.Context, repoPath, branch string) bool {
	_, err := git.ResolveRef(ctx, repoPath, "refs/heads/"+branch)
	if err == nil {
		return true
	}
	if isRefNotFound(err) {
		return false
	}
	// 非 not-found 的 git 错误：拿不准时保守视为存在，避免误删。
	return true
}

// isRefNotFound 判断 ResolveRef 错误是否为"ref 不存在"。
// git rev-parse --verify 对不存在 ref 返回非零且 stderr 含 "Needed a single revision"
// 或 "unknown revision"（取决于 git 版本）；此处按常见关键字判定。
func isRefNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Needed a single revision") ||
		strings.Contains(msg, "unknown revision") ||
		strings.Contains(msg, "bad revision")
}

// cleanupFailedAdd 回收 Add 失败后可能残留的产物：worktree 目录、分支、worktree metadata。
// 每步独立 best-effort，收集错误但不中断后续回收。
// B1：仅当 branchCreatedHere=true（分支确为本次 Add 创建）才删除分支，
// 不得无条件 branch -D 既有分支（design.md §19 补偿范围限定）。
func cleanupFailedAdd(ctx context.Context, repoPath, dest, branch string, branchCreatedHere bool) {
	_ = os.RemoveAll(dest)
	if branchCreatedHere {
		_ = git.DeleteBranch(ctx, repoPath, branch) // 分支不存在视为成功（幂等）
	}
	_ = git.WorktreePrune(ctx, repoPath)
}

// RemoveOpts 控制删除行为。
type RemoveOpts struct {
	// RepoPath 主仓库路径（用于 worktree remove/branch -D/prune）。
	RepoPath string
	// Branch 任务关联分支名（为空则跳过分支删除）。
	Branch string
	// ForceDirty 是否强制删除含 dirty/untracked 的 worktree。
	ForceDirty bool
}

// ErrPruneIncomplete 表示 worktree 与分支已成功删除，但 prune 清理失败。
// 非致命：资源已移除，下次 Add 会再次 prune；调用方可视为 Retry 已完成但留清理债务。
var ErrPruneIncomplete = errors.New("worktree: prune incomplete (worktree and branch removed)")

// Remove 删除 worktree 及其分支，并 prune。
// 包含性校验采用双端 EvalSymlinks + filepath.Rel（design.md §6/§17）。
// 存在 dirty/untracked 且 ForceDirty=false 时返回明确错误，由上层交互确认。
// 分支被其他 worktree 占用时拒绝并说明。
//
// 幂等性（B8/design.md §19，资源不存在视为已成功）：
//   - worktree 已不存在视为成功（git worktree remove 与目录清理均幂等）
//   - branch 已不存在视为成功（git branch -D 幂等）
//   - prune 失败不阻塞：返回 ErrPruneIncomplete 而非致命错误，Retry 可继续完成
//
// S3：在真正 destructive call（worktree remove / RemoveAll）前再做一次包含性校验，
// 缩小锁外初校验与锁内 destructive 之间的 TOCTOU 窗口。
func (m *Manager) Remove(ctx context.Context, wtPath string, opts RemoveOpts) error {
	if opts.RepoPath == "" {
		return errors.New("worktree: empty repo path")
	}

	// 初次包含性校验（锁外，提前拒绝明显逃逸）。
	if err := m.checkContainment(wtPath); err != nil {
		return err
	}

	// 仓库级写锁（add-plain-dir-project D10：升级为 context-aware AcquireRepoLock，
	// 与 refresh fetch、GitPush(-u) 串行；ctx 取消及时返回不进锁）。
	unlock, err := git.AcquireRepoLock(ctx, opts.RepoPath)
	if err != nil {
		return err
	}
	defer unlock()

	// S3：destructive 前二次包含性校验，缩小 TOCTOU 窗口。
	if err := m.checkContainment(wtPath); err != nil {
		return err
	}

	// dirty 检查（worktree 不存在则跳过）。
	if _, err := os.Stat(wtPath); err == nil {
		dirty, derr := git.IsWorktreeDirty(ctx, wtPath)
		if derr != nil {
			return fmt.Errorf("worktree: check dirty: %w", derr)
		}
		if dirty && !opts.ForceDirty {
			return errors.New("worktree: contains modified or untracked files; confirm with ForceDirty to remove")
		}
	}

	// 分支被其他 worktree 占用检查（删除前；排除自身 worktree，design.md §6）。
	if opts.Branch != "" {
		checked, err := git.BranchCheckedOutByOther(ctx, opts.RepoPath, opts.Branch, wtPath)
		if err != nil {
			return fmt.Errorf("worktree: check branch occupied: %w", err)
		}
		if checked {
			return fmt.Errorf("worktree: branch %q is checked out by another worktree", opts.Branch)
		}
	}

	// worktree remove（幂等：worktree 已不存在视为成功）。
	if err := git.WorktreeRemove(ctx, opts.RepoPath, wtPath, opts.ForceDirty); err != nil {
		return fmt.Errorf("worktree: remove: %w", err)
	}
	// 兜底清理目录：git worktree remove 成功后目录应已不存在，防御性清理残留。
	if err := os.RemoveAll(wtPath); err != nil {
		return fmt.Errorf("worktree: cleanup dir: %w", err)
	}

	// 分支删除（幂等：分支已不存在视为成功）。
	if opts.Branch != "" {
		if err := git.DeleteBranch(ctx, opts.RepoPath, opts.Branch); err != nil {
			return fmt.Errorf("worktree: delete branch: %w", err)
		}
	}

	// prune（design.md §17：remove 后）。失败不阻塞 Retry 完成（B8）：
	// worktree 与分支已删除，prune 仅清理元数据，返回 ErrPruneIncomplete 提示留清理债务。
	if err := git.WorktreePrune(ctx, opts.RepoPath); err != nil {
		return fmt.Errorf("%w: %v", ErrPruneIncomplete, err)
	}
	return nil
}

// BranchExists 判断分支是否已存在（B1：Create 落库前检查分支冲突）。
// not-found → (false, nil)；其他 git 错误 → (false, err)（不得把所有 git 错误当不存在，
// 调用方据 err 决策，避免误判分支可创建而走到 worktree add）。
func (m *Manager) BranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	_, err := git.ResolveRef(ctx, repoPath, "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	if isRefNotFound(err) {
		return false, nil
	}
	return false, err
}

// VerifyWorktreeProduct 严格校验 worktree 产物（B1：RetryCreate 幂等跳过 add 的判定依据）。
func (m *Manager) VerifyWorktreeProduct(ctx context.Context, repoPath, wtPath, branch string) error {
	return git.VerifyWorktreeProduct(ctx, repoPath, wtPath, branch)
}

// PreflightDeleteOpts 删除前置检查选项（B8）。
type PreflightDeleteOpts struct {
	RepoPath     string
	Branch       string
	ConfirmDirty bool // 用户已确认 dirty（API 层 confirmDirty=true）
}

// PreflightDelete 在删除副作用前做静态安全检查（B8：包含性/dirty/分支占用先于 oc session 清理）。
// dirty 且未确认 → 拒绝；分支被其他 worktree 占用 → 拒绝；路径逃逸 → 拒绝。
func (m *Manager) PreflightDelete(ctx context.Context, wtPath string, opts PreflightDeleteOpts) error {
	if opts.RepoPath == "" {
		return errors.New("worktree: empty repo path")
	}
	if err := m.checkContainment(wtPath); err != nil {
		return err
	}
	// dirty 检查（worktree 不存在则跳过）。
	if _, err := os.Stat(wtPath); err == nil {
		dirty, derr := git.IsWorktreeDirty(ctx, wtPath)
		if derr != nil {
			return fmt.Errorf("worktree: preflight check dirty: %w", derr)
		}
		if dirty && !opts.ConfirmDirty {
			return errors.New("worktree: contains modified or untracked files; confirm deletion with confirmDirty=true or force mode")
		}
	}
	// 分支被其他 worktree 占用检查。
	if opts.Branch != "" {
		checked, err := git.BranchCheckedOutByOther(ctx, opts.RepoPath, opts.Branch, wtPath)
		if err != nil {
			return fmt.Errorf("worktree: preflight check branch occupied: %w", err)
		}
		if checked {
			return fmt.Errorf("worktree: branch %q is checked out by another worktree", opts.Branch)
		}
	}
	return nil
}

// DirtyFiles 返回 worktree 中 dirty/untracked 文件路径集合（porcelain v2）。
// 用于删除二次门禁：preflight 快照与删除前快照对比，preflight 后新产生的 dirty
// 未经确认不得删（design.md §19 B7c）。worktree 不存在返回空集 + nil。
func (m *Manager) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	if _, err := os.Stat(wtPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worktree: stat for dirty snapshot: %w", err)
	}
	entries, err := git.Status(ctx, wtPath)
	if err != nil {
		return nil, fmt.Errorf("worktree: status for dirty snapshot: %w", err)
	}
	out := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		key := e.Path
		if e.Rename != "" {
			// rename 以 "old\x00new" 复合键记录，避免歧义。
			key = e.Rename + "\x00" + e.Path
		}
		out[key] = struct{}{}
	}
	return out, nil
}

// checkContainment 校验 wtPath 解析后位于 <dataDir>/worktrees 之下。
// 双端 EvalSymlinks：dataDir/worktrees 端与 wtPath 端均解析为 canonical path，
// 再用 filepath.Rel 判断包含性（禁止字符串前缀判断，design.md §6/§17）。
func (m *Manager) checkContainment(wtPath string) error {
	root := filepath.Join(m.dataDir, "worktrees")
	rootCanon, err := evalSymlinks(root)
	if err != nil {
		return fmt.Errorf("worktree: resolve root %s: %w", root, err)
	}
	wtCanon, err := evalSymlinks(wtPath)
	if err != nil {
		return fmt.Errorf("worktree: resolve worktree %s: %w", wtPath, err)
	}
	rel, err := filepath.Rel(rootCanon, wtCanon)
	if err != nil {
		return fmt.Errorf("worktree: path escapes worktrees root: %w", err)
	}
	if rel == "." || rel == "" || startsWithDotDot(rel) {
		return fmt.Errorf("worktree: path escapes worktrees root: %s", wtPath)
	}
	return nil
}

// evalSymlinks 解析为 canonical path；路径不存在时向上查找最近存在的祖先解析，
// 再拼接不存在的尾部（emdash realpath-containment 模式，design.md §17）。
func evalSymlinks(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	// 向上找到最近存在的祖先。
	dir := path
	tail := ""
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			if tail != "" {
				return filepath.Join(r, tail), nil
			}
			return r, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// 到达根仍未找到存在路径。
			return "", os.ErrNotExist
		}
		if tail == "" {
			tail = filepath.Base(dir)
		} else {
			tail = filepath.Join(filepath.Base(dir), tail)
		}
		dir = parent
	}
}

// startsWithDotDot 判断 rel 是否以 ".." 开头（含 ".."、"../x"），即逃逸。
func startsWithDotDot(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, "../")
}