package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Status 返回 dir 工作区的文件状态列表（porcelain v2 -z 解析 + numstat -z 统计）。
// 只读操作，不进 repo 写锁。变更文件数超过 MaxStatusFiles 返回 ErrTooManyFilesChanged。
// numstat 调用失败返回明确错误（MUST NOT 静默降级为零统计）。
func Status(ctx context.Context, dir string) ([]FileStatus, error) {
	out, _, err := run(ctx, dir, "status", "--porcelain=v2", "-z", "-uall")
	if err != nil {
		return nil, err
	}

	entries, perr := parseStatusPorcelainV2Z(strings.NewReader(out), MaxStatusFiles, false, nil)
	if perr != nil {
		return nil, perr
	}
	if len(entries) > MaxStatusFiles {
		return nil, ErrTooManyFilesChanged
	}

	// 关联 numstat -z：staged 用 --cached，unstaged 用工作区。失败 MUST 返回明确错误。
	stagedStat, _, serr := run(ctx, dir, "diff", "--numstat", "-z", "--cached")
	if serr != nil {
		return nil, fmt.Errorf("git status: staged numstat failed: %w", serr)
	}
	unstagedStat, _, uerr := run(ctx, dir, "diff", "--numstat", "-z")
	if uerr != nil {
		return nil, fmt.Errorf("git status: unstaged numstat failed: %w", uerr)
	}
	stagedByPath, stagedByRename := parseNumstatZ([]byte(stagedStat))
	unstagedByPath, unstagedByRename := parseNumstatZ([]byte(unstagedStat))

	for i := range entries {
		e := &entries[i]
		if e.Kind == "2" {
			// rename：用 "old\x00new" 复合键在 rename map 查找。
			applyRenameNumstat(e, stagedByPath, stagedByRename, unstagedByPath, unstagedByRename)
			continue
		}
		key := e.Path
		if e.Staged {
			if s, ok := stagedByPath[key]; ok {
				mergeEntry(e, s)
			}
		}
		if e.Unstaged || e.Untracked {
			if s, ok := unstagedByPath[key]; ok {
				mergeEntry(e, s)
			}
		}
	}
	return entries, nil
}

func mergeEntry(e *FileStatus, s *numstatEntry) {
	if s.isBinary {
		e.IsBinary = true
	}
	e.Additions += s.additions
	e.Deletions += s.deletions
}

// ListIgnoredUntracked 枚举 repoPath 工作区的 untracked 与 ignored 文件（design.md §7.2）。
// 命令 MUST 为 `git status --porcelain=v2 -z --ignored=traditional --untracked-files=all`：
// --ignored=traditional + -uall 才返回 ignored 目录内文件级记录（matching 仅返回目录级）。
// 返回仅含 untracked(`?`) 与 ignored(`!`) 两类；`.git` 条目 MUST 排除。
// 复用 boundedBuffer（有界输出）+ 参数白名单（无选项注入面）。变更文件数超过 MaxStatusFiles
// 返回 ErrTooManyFilesChanged。只读操作，不进 repo 写锁。
//
// 有界计数只针对 `?`/`!` 目标条目：parser 经 kindFilter 在解析阶段即跳过 tracked
// ordinary/rename/unmerged 条目（不分配 FileStatus、不计数），避免大量 modified tracked
// 文件触发上限或造成无谓分配。达到 MaxStatusFiles+1 个目标条目即返回 ErrTooManyFilesChanged。
func ListIgnoredUntracked(ctx context.Context, repoPath string) ([]FileStatus, error) {
	out, _, err := run(ctx, repoPath, "status", "--porcelain=v2", "-z",
		"--ignored=traditional", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	// kindFilter 仅保留 '?'(untracked) 与 '!'(ignored)；其余 kind（tracked）在解析阶段跳过。
	entries, perr := parseStatusPorcelainV2Z(strings.NewReader(out), MaxStatusFiles, true,
		func(kind byte) bool { return kind == '?' || kind == '!' })
	if perr != nil {
		return nil, perr
	}
	// 排除 `.git` 条目（design.md §7.2）。
	var filtered []FileStatus
	for _, e := range entries {
		if isGitPath(e.Path) {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered, nil
}

// isGitPath 判断相对路径是否位于 .git 下（根 .git 或任意层级子目录）。
// Path 为相对路径（porcelain v2 输出），含反斜杠需先归一。
func isGitPath(p string) bool {
	if p == ".git" {
		return true
	}
	rest := p
	for {
		if rest == ".git" {
			return true
		}
		i := strings.IndexByte(rest, '/')
		if i < 0 {
			return false
		}
		if rest[:i] == ".git" {
			return true
		}
		rest = rest[i+1:]
	}
}

func applyRenameNumstat(e *FileStatus, stagedByPath, stagedByRename, unstagedByPath, unstagedByRename map[string]*numstatEntry) {
	key := e.Rename + "\x00" + e.Path
	if e.Staged {
		if s, ok := stagedByRename[key]; ok {
			mergeEntry(e, s)
		}
		if s, ok := stagedByPath[e.Path]; ok {
			mergeEntry(e, s)
		}
	}
	if e.Unstaged {
		if s, ok := unstagedByRename[key]; ok {
			mergeEntry(e, s)
		}
		if s, ok := unstagedByPath[e.Path]; ok {
			mergeEntry(e, s)
		}
	}
}

// DiffMaxBytes 限制单次 unified diff 输出字节数；超限返回 truncated=true。
const DiffMaxBytes = 512 * 1024

// DiffMaxFiles 限制单次 diff 涉及文件数（path 为空时全仓 diff）；超限返回 truncated=true。
const DiffMaxFiles = 1000

// Diff 返回 dir 中 path 相对 ref 的 unified diff 文本。
// ref 为空表示工作区 vs 索引/HEAD 的默认 diff。path 为空表示全部（受 DiffMaxFiles 限制）。
// 输出超过 DiffMaxBytes、文件数超过 DiffMaxFiles 或文件为二进制时返回 truncated=true。
// ref 先经 git rev-parse --verify --end-of-options 解析为 OID，防止 option 注入。
// path 经 literal pathspec 包裹（":(literal)<path>"），防止 pathspec magic 扩大范围。
func Diff(ctx context.Context, dir, ref, path string) (string, bool, error) {
	oid := ""
	if ref != "" {
		resolved, _, rerr := run(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref)
		if rerr != nil {
			return "", false, fmt.Errorf("git diff: invalid ref %q: %w", ref, rerr)
		}
		oid = strings.TrimSpace(resolved)
		if oid == "" {
			return "", false, fmt.Errorf("git diff: rev-parse returned empty for ref %q", ref)
		}
	}

	args := []string{"diff"}
	if oid != "" {
		args = append(args, oid)
	}
	args = append(args, "--")
	if path != "" {
		args = append(args, literalPathspec(path))
	}

	// path 为空（全仓 diff）时先校验文件数上限。
	// 预统计 MUST 带与实际 diff 同一 resolved ref（oid），否则大 ref diff 可绕过 DiffMaxFiles
	//（预统计默认工作区 diff，而实际带 ref 时为 ref→工作区/索引 diff，文件数不同）。
	if path == "" {
		nameArgs := []string{"diff", "--name-only", "-z"}
		if oid != "" {
			nameArgs = append(nameArgs, oid)
		}
		nameArgs = append(nameArgs, "--")
		nameOut, _, nerr := run(ctx, dir, nameArgs...)
		if nerr != nil {
			return "", false, fmt.Errorf("git diff: name-only count failed: %w", nerr)
		}
		if countNulRecords(nameOut) > DiffMaxFiles {
			return "", true, nil
		}
	}

	out, _, err := run(ctx, dir, args...)
	if err != nil {
		return "", false, err
	}

	// 二进制文件：git 输出 "Binary files a/x and b/x differ"。
	if isBinaryDiffOutput(out) {
		return "", true, nil
	}
	if len(out) > DiffMaxBytes {
		return out[:DiffMaxBytes], true, nil
	}
	return out, false, nil
}

func isBinaryDiffOutput(s string) bool {
	return strings.Contains(s, "Binary files ") && strings.Contains(s, "differ")
}

// countNulRecords 统计 -z 输出中的非空记录数（忽略末尾 trailing NUL 产生的空段）。
func countNulRecords(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	for _, r := range strings.Split(s, "\x00") {
		if r != "" {
			n++
		}
	}
	return n
}

// literalPathspec 将用户路径包裹为 literal pathspec，禁止 magic（glob/exclude 等）。
// 路径以 ':' 开头时仍需保证 ": (literal)" 前缀正确，git 形式为 ":(literal)<path>"。
func literalPathspec(path string) string {
	return ":(literal)" + path
}

// Commit 在 dir 中暂存 paths（非空时）并以 msg 提交。保留 hooks/签名行为，错误原样透传。
// paths 经 literal pathspec 包裹，防止 pathspec magic 扩大 stage/commit 范围。
func Commit(ctx context.Context, dir, msg string, paths []string) error {
	if msg == "" {
		return errors.New("git: empty commit message")
	}
	if len(paths) > 0 {
		addArgs := []string{"add", "--"}
		for _, p := range paths {
			addArgs = append(addArgs, literalPathspec(p))
		}
		if _, _, err := run(ctx, dir, addArgs...); err != nil {
			return err
		}
	} else {
		// 空 paths = 提交全部改动（design.md §9）：先 stage 全部（含 untracked），
		// 否则 `git commit` 在无暂存内容时失败（冒烟实证）。
		if _, _, err := run(ctx, dir, "add", "-A"); err != nil {
			return err
		}
	}
	commitArgs := []string{"commit", "-m", msg}
	if len(paths) > 0 {
		commitArgs = append(commitArgs, "--")
		for _, p := range paths {
			commitArgs = append(commitArgs, literalPathspec(p))
		}
	}
	if _, _, err := run(ctx, dir, commitArgs...); err != nil {
		return err
	}
	return nil
}

// Push 将 dir 所在分支推送到 origin。首次推送含 -u 设置 upstream。MUST NOT force-push。
// Push 推送分支到 origin 并设置上游（-u 写共享 .git/config）。
// add-plain-dir-project D10：-u 写共享 .git/config 纳入 canonical repo 写锁，与 worktree.Add/Remove、
// refresh fetch 串行。dir 可为 worktree 路径，经 WorktreeDir 解析为 canonical repo 根再上锁。
func Push(ctx context.Context, dir, branch string) error {
	if branch == "" {
		return errors.New("git: empty branch")
	}
	// 解析 canonical repo 根（dir 可为 worktree；--git-common-dir 的父目录即 repo 根）。
	repoRoot, rerr := WorktreeDir(ctx, dir)
	if rerr != nil {
		// 解析失败回退用 dir 上锁（best-effort，保持既有串行；不阻塞 push）。
		repoRoot = dir
	}
	unlock, lerr := AcquireRepoLock(ctx, repoRoot)
	if lerr != nil {
		return lerr
	}
	defer unlock()

	if err := ValidateBranchName(ctx, dir, branch); err != nil {
		return err
	}
	if _, _, err := run(ctx, dir, "push", "-u", "origin", branch); err != nil {
		return err
	}
	return nil
}

// ValidateBranchName 用 git check-ref-format 校验分支名合法性。
func ValidateBranchName(ctx context.Context, dir, branch string) error {
	if branch == "" {
		return errors.New("git: empty branch name")
	}
	if _, _, err := run(ctx, dir, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("invalid branch name %q: %w", branch, err)
	}
	return nil
}
