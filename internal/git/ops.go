package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

	// 未跟踪文件无 numstat：按行读取估算 Additions（deletions=0）。
	// 有界读取语义见 countUntrackedLines（单文件 16MB / 累计 64MB 双预算 + 8000B NUL 嗅探）。
	if uerr := countUntrackedLines(ctx, dir, entries); uerr != nil {
		return nil, uerr
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

// untracked 单文件文本读取预算上限：单文件 16MB；全部 untracked 累计 64MB。
// 仅约束行数读取——二进制嗅探（前 8000 字节 NUL 检测）对所有 regular file 始终执行。
const (
	untrackedSniffBytes  = 8000
	untrackedFileBudget  = 16 * 1024 * 1024
	untrackedTotalBudget = 64 * 1024 * 1024
)

// countUntrackedLines 对 entries 中 Untracked 条目按行读取估算 Additions（deletions=0）。
// 执行顺序（design.md D2）：
//  1. os.Lstat：非 regular file（symlink/fifo/设备）→ 跳过（additions=0，不报错）。
//     Lstat not-exist 静默跳过（status 快照后文件被并发删除属正常竞态，非 IO 错误，不阻塞 status）；
//     其他 Lstat 错误（权限/IO 失败）返回明确错误，不静默置零。
//  2. 单次 Open + 读前 untrackedSniffBytes 嗅探 NUL：含 NUL → IsBinary=true、不计行；
//     嗅探 MUST 始终执行（即使预算耗尽），仅为标记 IsBinary。二进制 prefix 不消耗文本预算
//     （预算仅约束文本行数读取；否则 8192 个小二进制文件即可耗尽预算使后续文本全部 additions=0）。
//  3. 文本行数读取：prefix 计入单文件与累计双预算；续读用 io.LimitReader(min(剩余单文件预算, 剩余累计预算)+1) 有界。
//     累计预算需先扣除 prefix（prefix <= totalRemaining 才续读，续读上限 = totalRemaining - len(prefix)）。
//  4. 单文件超 16MB 或累计 64MB 耗尽 → 该文件跳过行计数（additions=0，IsBinary 仍标记）。
//  5. count('\n') + 末行无换行 +1 → additions。
//     IO 错误返回含文件路径的明确错误（MUST NOT 静默降级，延续 ops.go:12 numstat 原则）。
//     ctx 取消时返回 ctx.Err()（属明确错误，不静默）。
func countUntrackedLines(ctx context.Context, dir string, entries []FileStatus) error {
	totalRemaining := untrackedTotalBudget
	for i := range entries {
		e := &entries[i]
		if !e.Untracked {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("git status: untracked count %q cancelled: %w", e.Path, cerr)
		}
		path := filepath.Join(dir, e.Path)

		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				// status 快照后文件被并发删除（TOCTOU）属正常竞态，静默跳过；
				// 与其他 IO 错误（权限/读失败）区分——后者 MUST 返回明确错误。
				continue
			}
			return fmt.Errorf("git status: untracked stat %q failed: %w", e.Path, err)
		}
		if !info.Mode().IsRegular() {
			// symlink/fifo/设备：跳过行计数（additions=0），不报错。
			continue
		}

		// 单次 Open：嗅探 + 续读同一 *os.File，避免并发修改导致快照非一致
		// （两次 open 可能拼接两个版本或漏掉新版本中的 NUL）。
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				// open 与 Lstat 之间被删除：同 Lstat 竞态语义，静默跳过。
				continue
			}
			return fmt.Errorf("git status: untracked open %q failed: %w", e.Path, err)
		}
		additions, used, rerr := countUntrackedFileLines(ctx, f, e, &totalRemaining)
		f.Close()
		if rerr != nil {
			return fmt.Errorf("git status: untracked read %q failed: %w", e.Path, rerr)
		}
		e.Additions = additions
		// used 仅文本路径返回非 0；二进制与跳过路径不消耗文本预算。
		// used 按实际读取字节计（prefix + 续读），可能超过 totalRemaining（超大文件超限分支），
		// clamp 防止负数——后续文件 totalRemaining 已 <= 0，全部跳过。
		if used > 0 {
			if used >= totalRemaining {
				totalRemaining = 0
			} else {
				totalRemaining -= used
			}
		}
	}
	return nil
}

// countUntrackedFileLines 对已打开的 f 完成嗅探 + 续读统计，返回 additions 与消耗的累计预算字节。
// used 按实际读取字节计（prefix + 续读，含超限分支本次读取的额外字节），供调用方扣减累计预算。
// 二进制路径返回 used=0（不消耗文本预算）；文本超预算路径返回 used=实际读取量（行计数无效但预算按实际扣除）。
// f 的偏移由本函数管理（嗅探后 seek 到 prefix 长度续读）。
func countUntrackedFileLines(ctx context.Context, f *os.File, e *FileStatus, totalRemaining *int) (additions, used int, err error) {
	sniff := make([]byte, untrackedSniffBytes)
	n, serr := io.ReadFull(f, sniff)
	if serr != nil && serr != io.EOF && serr != io.ErrUnexpectedEOF {
		return 0, 0, serr
	}
	prefix := sniff[:n]

	// 嗅探始终执行（含 NUL → IsBinary）。二进制不计行且 prefix 不消耗文本预算。
	if bytes.IndexByte(prefix, 0) >= 0 {
		e.IsBinary = true
		return 0, 0, nil
	}

	// 文本预算扣 prefix；prefix 超出累计预算 → 跳过行计数（IsBinary 已标记非二进制）。
	if n > *totalRemaining {
		return 0, 0, nil
	}
	fileRemaining := untrackedFileBudget - n
	if fileRemaining <= 0 {
		// 单文件 prefix 已超 16MB → 跳过行计数；累计预算仅扣 prefix。
		return 0, n, nil
	}
	// 续读预算 = min(单文件剩余, 累计剩余 - prefix)。
	contBudget := *totalRemaining - n
	if fileRemaining < contBudget {
		contBudget = fileRemaining
	}

	if _, serr := f.Seek(int64(n), io.SeekStart); serr != nil {
		return 0, 0, serr
	}
	additions = bytes.Count(prefix, []byte{'\n'})
	last := byte(0)
	if len(prefix) > 0 {
		last = prefix[len(prefix)-1]
	}

	var contRead int
	r := io.LimitReader(f, int64(contBudget)+1)
	buf := make([]byte, 64*1024)
	for {
		if cerr := ctx.Err(); cerr != nil {
			return 0, 0, cerr
		}
		m, rerr := r.Read(buf)
		if m > 0 {
			if contRead+m > contBudget {
				// 超预算：该文件行计数无效。报告实际已读字节（prefix + 已成功续读 + 本次读取），
				// 使调用方按实际读取字节扣减累计预算——避免超大文件只耗 prefix 额度导致多个 >16MB 文件重复获得近 16MB 读取额度。
				used := n + contRead + m
				return 0, used, nil
			}
			contRead += m
			additions += bytes.Count(buf[:m], []byte{'\n'})
			last = buf[m-1]
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, 0, rerr
		}
	}
	if n+contRead > 0 && last != '\n' {
		additions++
	}
	return additions, n + contRead, nil
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

// DiffUntracked 返回 dir 中 untracked 新文件 path 的合成 unified diff。
// 实现：`git diff --no-index -- <os.DevNull> <path>`（git 对 --no-index 两侧按文件系统路径比较，
// 不依赖 index，只读无副作用）。diff 子命令已在 exec.go 白名单内，MUST NOT 套 ":(literal)" pathspec。
//
// 防御性路径校验：拒绝绝对路径、".." 逃逸、NUL → 返回 sentinel ErrInvalidDiffPath（errors.Is 可判），
// Manager 据此映射 invalid_input。
//
// 输出判定真值表（design.md D1，依据 git --no-index 实测契约：exit=1 歧义，MUST 联合 stdout/stderr）：
//  1. err==nil（含 stdout 空）→ 正常返回
//  2. exit==1 && stdout 非空 && stderr 空 → 正常 diff 输出
//  3. errors.Is(ErrOutputTruncated) && stdout 非空 && stderr 空 → 512KB 前缀 + truncated=true
//  4. 其他非 nil 错误（exit>1 / stdout 空 / stderr 非空）→ 透传
//
// 注意 exec.go:108 溢出路径返回 fmt.Errorf("%w (%v)", ErrOutputTruncated, ce)，
// errors.As(*exec.ExitError) 在此路径上断裂，MUST 先判 ErrOutputTruncated。
// stderr 从 run() 第二返回值获取，MUST 校验 stderr 为空以排除 "stdout 部分 + stderr 溢出" 的失败。
func DiffUntracked(ctx context.Context, dir, path string) (string, bool, error) {
	if err := validateDiffPath(path); err != nil {
		return "", false, err
	}

	out, stderr, err := run(ctx, dir, "diff", "--no-index", "--", os.DevNull, path)
	if err == nil {
		// err==nil（含 stdout 空）：正常返回。空新文件经实测 exit=1，不会走到这里；
		// 但若未来 git 版本对空文件返回 exit=0，此处仍正确兜底。
		d, trunc := finalizeDiffTruncatable(out)
		return d, trunc, nil
	}

	// 真值表顺序：先判 ErrOutputTruncated（溢出路径 errors.As 断裂），再判 exit==1。
	if errors.Is(err, ErrOutputTruncated) {
		if out != "" && stderr == "" {
			d, trunc := finalizeDiffTruncatable(out)
			return d, trunc, nil
		}
		// 溢出但 stdout 空 或 stderr 非空（含 stderr 溢出）→ 失败透传。
		return "", false, err
	}

	var ce *commandError
	if errors.As(err, &ce) {
		if exitCode(ce) == 1 && out != "" && stderr == "" {
			// exit=1 + stdout 非空 + stderr 空 → 正常 diff 输出（二进制→truncated=true）。
			d, trunc := finalizeDiffTruncatable(out)
			return d, trunc, nil
		}
	}
	// 其他非 nil 错误透传（含 exit>1、stdout 空、stderr 非空）。
	return "", false, err
}

// validateDiffPath 校验 DiffUntracked 的路径：拒绝绝对路径、".." 逃逸、NUL。
func validateDiffPath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidDiffPath)
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("%w: absolute path %q", ErrInvalidDiffPath, path)
	}
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("%w: path contains NUL", ErrInvalidDiffPath)
	}
	// filepath.Clean 归一后检查是否逃逸到父目录（".." 或以 "../" 开头）。
	cleaned := filepath.Clean(path)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("%w: path escapes via .. %q", ErrInvalidDiffPath, path)
	}
	return nil
}

// exitCode 从 commandError 提取 git 进程退出码（非 *exec.ExitError 返回 -1）。
func exitCode(ce *commandError) int {
	var ee *exec.ExitError
	if errors.As(ce.err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// finalizeDiffTruncatable 处理 diff 文本并返回 (diff, truncated)：
// 二进制 → ("", true)；超 512KB → (前缀, true)；否则原样。
func finalizeDiffTruncatable(out string) (string, bool) {
	if isBinaryDiffOutput(out) {
		return "", true
	}
	if len(out) > DiffMaxBytes {
		return out[:DiffMaxBytes], true
	}
	return out, false
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
