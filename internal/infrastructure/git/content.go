// content.go 按版本读取单文件内容（codemirror-git-diff design D3/D4/D5）。
//
// 三种来源：
//   - ref 侧（ReadRefSideContent）：ls-tree -z 探测 + git show <blobOID> 读取；
//   - index 侧（ReadIndexSideContent）：ls-files -z --stage 探测 + git show <stage-0 blobOID>；
//   - 工作区新侧（ReadWorktreeSideContent）：受限文件系统读取（禁锢校验 + 有界读取）。
//
// 探测 MUST 用 literal pathspec 包裹并逐条核对返回记录（路径精确相等 + mode 分支：
// 100644/100755/120000 为 blob，160000 为 gitlink），禁止依赖 stderr 文案匹配判定存在性。
// 只读操作，不修改 index/工作区，不进 repo 写锁。
package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FileContentMaxBytes 单侧内容上限；超过返回有界前缀且 Truncated=true
//（design D5：512KB 值沿用旧 unified diff 的输出上限，重命名为内容上限语义）。
const FileContentMaxBytes = 512 * 1024

// binarySniffBytes 二进制嗅探窗口：内容前 8000 字节含 NUL 判定为二进制
//（对齐 git 启发式，status 行计数嗅探与 diff 两侧内容嗅探同口径）。
const binarySniffBytes = 8000

// git 条目 mode 文本（spec「文件 diff 查看」：mode 为 git 八进制 mode 文本）。
const (
	modeRegular    = "100644" // regular blob / 无 owner 执行位 regular file
	modeExecutable = "100755" // regular blob / owner 执行位置位 regular file
	modeSymlink    = "120000" // symlink：内容为链接目标文本
	modeGitlink    = "160000" // gitlink（submodule commit）：内容为 commit OID 文本
)

// SideContent 单侧版本内容查询结果（design D1/D5 八字段契约的单侧投影）。
type SideContent struct {
	// Content 为该侧内容；Exists=false 时为空串。
	Content string
	// Exists 表示该侧存在：旧侧为 ref/index 探测命中的条目（blob 100644/100755/120000 或
	// gitlink 160000），新侧为工作区 regular file/symlink/directory（gitlink）。
	Exists bool
	// Mode 为该侧 git 八进制 mode 文本（100644/100755/120000/160000），取自探测记录或
	// 工作区类型/权限位；Exists=false 时为空串。
	Mode string
	// IsBinary 为内容前 binarySniffBytes 字节含 NUL 的嗅探结果（单侧独立判定）；
	// mode 120000/160000 的侧不参与嗅探（内容为链接目标/commit OID 文本，天然为文本）。
	IsBinary bool
	// Truncated 仅表示内容大小超过 FileContentMaxBytes 的截断，MUST NOT 兼任二进制含义。
	Truncated bool
}

// contentIsBinary 判定内容前 binarySniffBytes 字节是否含 NUL。
func contentIsBinary(content string) bool {
	prefix := content
	if len(prefix) > binarySniffBytes {
		prefix = prefix[:binarySniffBytes]
	}
	return strings.IndexByte(prefix, 0) >= 0
}

// ValidateDiffPath 校验 diff path：拒绝空、绝对路径、`..` 逃逸、NUL，命中返回 ErrInvalidDiffPath
//（errors.Is 可判）。Manager 阶段① 词法校验与 ReadWorktreeSideContent 同源调用本函数。
func ValidateDiffPath(path string) error {
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

// ResolveRefOID 将 ref 解析为 OID（git rev-parse --verify --end-of-options，防 option 注入）。
// ref MUST 非空。失败返回包装错误（StderrOf 可提取 git stderr 供调用方透传）。
func ResolveRefOID(ctx context.Context, dir, ref string) (string, error) {
	out, _, err := run(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref)
	if err != nil {
		return "", fmt.Errorf("rev-parse %q: %w", ref, err)
	}
	oid := strings.TrimSpace(out)
	if oid == "" {
		return "", fmt.Errorf("rev-parse returned empty for ref %q", ref)
	}
	return oid, nil
}

// ReadRefSideContent 读取 ref 侧（ref 已解析为 oid）path 的条目内容（design D3）。
// 探测：git ls-tree -z <oid> -- ":(literal)<path>"，逐条核对记录路径与 mode/type——
// 仅记录路径与请求 path 精确相等的条目参与判定：regular blob（100644/100755）与 symlink
//（120000，blob 内容即链接目标文本）经 git show <blobOID> 读取；gitlink（160000）内容直接
// 取记录 commit OID 文本（无需 git show）；目录（tree）等按不存在处理；path 为目录时
// ls-tree 返回其子路径记录（与请求 path 不相等），同样按不存在。
// 存在时 MUST 以探测取得的对象 OID 为读取对象，禁止以 <ref>:<path> 二次拼路径
//（实测 `git show HEAD:<目录>` 输出 tree listing 而非报错，会把目录误当文件内容）。
// 旧侧 mode MUST 取自探测记录。
func ReadRefSideContent(ctx context.Context, dir, oid, path string) (SideContent, error) {
	out, _, err := run(ctx, dir, "ls-tree", "-z", oid, "--", literalPathspec(path))
	if err != nil {
		return SideContent{}, fmt.Errorf("ls-tree %s: %w", oid, err)
	}
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		mode, typ, objOID, recPath, ok := parseLsTreeRecord(record)
		if !ok || recPath != path {
			continue
		}
		if typ == "blob" && (isRegularBlobMode(mode) || mode == modeSymlink) {
			// symlink blob 与 regular blob 同经 git show <blobOID> 读取。
			return readBlobSide(ctx, dir, objOID, mode)
		}
		if typ == "commit" && mode == modeGitlink {
			return finalizeSideContent(objOID, modeGitlink), nil
		}
		// 目录（tree）等其他对象类型：按不存在处理（正常结果，非错误）。
		return SideContent{}, nil
	}
	return SideContent{}, nil
}

// ReadIndexSideContent 读取 index 侧（ref 为空）path 的 stage-0 条目内容（design D3）。
// 探测：git ls-files -z --stage -- ":(literal)<path>"，逐条核对记录——仅记录路径与请求 path
// 精确相等（:(literal) 不等于精确匹配，目录 path 会返回其下全部子路径记录）且 stage 为 0
// 的条目参与判定：mode 为 100644/100755/120000 时视为存在（blob 经 git show <blobOID> 读取），
// 160000 视为存在且内容直接取记录 OID 文本；其他 mode 按不存在处理。
// 无任何精确匹配记录 → 不存在（正常结果）；存在同 path 记录但无 stage-0（仅其他 stage）
// → ErrUnmergedPath（未解决冲突，MUST NOT 降级为不存在）。旧侧 mode MUST 取自探测记录。
func ReadIndexSideContent(ctx context.Context, dir, path string) (SideContent, error) {
	out, _, err := run(ctx, dir, "ls-files", "-z", "--stage", "--", literalPathspec(path))
	if err != nil {
		return SideContent{}, fmt.Errorf("ls-files: %w", err)
	}
	var hasMatch, hasStage0 bool
	var stage0OID, stage0Mode string
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		mode, oid, stage, recPath, ok := parseLsFilesStageRecord(record)
		if !ok || recPath != path {
			continue
		}
		hasMatch = true
		if stage == "0" {
			hasStage0 = true
			stage0OID, stage0Mode = oid, mode
		}
	}
	if !hasMatch {
		return SideContent{}, nil
	}
	if !hasStage0 {
		return SideContent{}, ErrUnmergedPath
	}
	switch {
	case isRegularBlobMode(stage0Mode) || stage0Mode == modeSymlink:
		return readBlobSide(ctx, dir, stage0OID, stage0Mode)
	case stage0Mode == modeGitlink:
		return finalizeSideContent(stage0OID, modeGitlink), nil
	default:
		// 其他 mode 的 stage-0 记录：按不存在处理。
		return SideContent{}, nil
	}
}

// ReadWorktreeSideContent 读取新侧（工作区）path 的文件内容（design D4）。
// worktree 为任务 worktree 根路径（由 Manager 传入）。执行顺序：
//  1. ValidateDiffPath 同源校验（空/绝对路径/`..` 逃逸/NUL → ErrInvalidDiffPath）。
//  2. os.Lstat 按类型分支（symlink 与 directory 分支的禁锢校验前置于 Readlink/git 执行）：
//     ENOENT（竞态）→ Exists=false，优先返回；symlink → 先校验 resolved parent（不含最终
//     链接段）位于 worktree 根内（越界 → ErrWorktreeEscape；链接目标文本本身不受禁锢），
//     再 Readlink 目标文本为内容，mode=120000；directory → gitlink 工作区侧：先校验
//     resolved target 位于 worktree 根内再执行任何 git 命令（toplevel 校验/OID/dirty 判定
//     见 readGitlinkSide），mode=160000；其他非 regular 类型（fifo/socket 等）→ Exists=false。
//  3. 候选 regular file 经同一禁锢校验（resolveInWorktree）。
//  4. 以校验后的真实路径打开有界读取（FileContentMaxBytes+1 判定截断）+ NUL 嗅探；
//     mode 依 owner 执行位（0100）为 100644/100755。
//
// 非 ENOENT 的 IO 错误返回明确错误（消息含相对 path 与操作名 stat/resolve/readlink/open/read），
// 供 Manager 映射 internal；子模块 dirty 探测失败返回 ErrSubmoduleDirtyProbe（供 Manager
// 映射 git_error 并透传 stderr）。已知残余风险：EvalSymlinks 与 open 之间存在 TOCTOU 窗口
//（威胁模型为单用户本机工具，见 design Risks）。
func ReadWorktreeSideContent(ctx context.Context, worktree, path string) (SideContent, error) {
	if err := ValidateDiffPath(path); err != nil {
		return SideContent{}, err
	}
	target := filepath.Join(worktree, path)

	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			// 查询期间文件被并发删除（ENOENT 竞态）→ 新侧不存在（正常结果）。
			return SideContent{}, nil
		}
		return SideContent{}, fmt.Errorf("stat %q: %w", path, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		// symlink：读链接文本而非跟随；先校验该链接的 resolved parent（不含最终链接段）
		// 位于 worktree 根内防中间级 symlink 逃逸，链接目标文本本身不受禁锢。
		if _, notExist, cerr := resolveInWorktree(worktree, filepath.Dir(target), path); cerr != nil {
			return SideContent{}, cerr
		} else if notExist {
			return SideContent{}, nil
		}
		link, lerr := os.Readlink(target)
		if lerr != nil {
			return SideContent{}, fmt.Errorf("readlink %q: %w", path, lerr)
		}
		return finalizeSideContent(link, modeSymlink), nil
	}
	if !info.Mode().IsRegular() {
		if info.IsDir() {
			// directory 按 gitlink 工作区侧处理：禁锢校验 MUST 先于任何 git 命令。
			dirReal, notExist, cerr := resolveInWorktree(worktree, target, path)
			if cerr != nil {
				return SideContent{}, cerr
			}
			if notExist {
				return SideContent{}, nil
			}
			sc, gerr := readGitlinkSide(ctx, dirReal)
			if gerr != nil {
				return SideContent{}, gerr
			}
			return sc, nil
		}
		// fifo/socket/设备等其他非 regular 类型：新侧不存在，优先于禁锢判定。
		return SideContent{}, nil
	}

	targetReal, notExist, cerr := resolveInWorktree(worktree, target, path)
	if cerr != nil {
		return SideContent{}, cerr
	}
	if notExist {
		return SideContent{}, nil
	}
	f, err := os.Open(targetReal)
	if err != nil {
		if os.IsNotExist(err) {
			// EvalSymlinks 与 open 之间被删除：同 ENOENT 竞态语义。
			return SideContent{}, nil
		}
		return SideContent{}, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	data, rerr := io.ReadAll(io.LimitReader(f, int64(FileContentMaxBytes)+1))
	if rerr != nil {
		return SideContent{}, fmt.Errorf("read %q: %w", path, rerr)
	}
	return finalizeSideContent(string(data), regularFileMode(info.Mode())), nil
}

// resolveInWorktree 解析 p 的 EvalSymlinks 真实路径并执行禁锢校验：真实路径 MUST 仍位于
// worktree 根内，防中间级 symlink 逃逸（MUST NOT 用 strings.HasPrefix 判定路径前缀，
// 会把 /worktree-other 误判为 /worktree 内部）。path 为用户相对路径，仅用于错误消息。
// p 消失（Lstat 与 resolve 之间被删除的竞态）→ notExist=true。
func resolveInWorktree(worktree, p, path string) (real string, notExist bool, err error) {
	rootReal, rerr := filepath.EvalSymlinks(worktree)
	if rerr != nil {
		return "", false, fmt.Errorf("resolve worktree root %q: %w", worktree, rerr)
	}
	real, rerr = filepath.EvalSymlinks(p)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return "", true, nil
		}
		return "", false, fmt.Errorf("resolve %q: %w", path, rerr)
	}
	rel, relErr := filepath.Rel(rootReal, real)
	if relErr != nil {
		return "", false, fmt.Errorf("resolve %q: %w", path, relErr)
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false, fmt.Errorf("%w: %q resolves to %q", ErrWorktreeEscape, path, real)
	}
	return real, false, nil
}

// readGitlinkSide 读取已通过禁锢校验的 submodule 工作目录（mode 160000）内容。
// 先以 rev-parse --show-toplevel 校验仓库 canonical 根与目标目录一致：真实未初始化子模块
// 无自身 .git，repo discovery 会向上发现父仓库——MUST 视为未初始化而非误返回
// superproject HEAD；不一致或失败 → 内容为空（Exists=true，正常结果，非错误）。
// toplevel 输出仅去除 git 追加的行尾换行、EvalSymlinks 归一后再比较——MUST NOT 整体
// TrimSpace（路径自身的首尾空白合法，尾空格目录会被误判未初始化）。
// 一致 → rev-parse HEAD 取 commit OID 文本；再以 status --porcelain 非空判定 dirty，
// dirty 时内容追加稳定 -dirty 后缀（对齐旧 unified diff 的 `Subproject commit <OID>-dirty`
// 显示语义）；dirty 探测执行失败 → ErrSubmoduleDirtyProbe（Manager 映射 git_error 并透传
// stderr），MUST NOT 静默按 clean 处理。
func readGitlinkSide(ctx context.Context, dir string) (SideContent, error) {
	uninitialized := SideContent{Exists: true, Mode: modeGitlink}
	topOut, _, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return uninitialized, nil
	}
	topReal, terr := filepath.EvalSymlinks(strings.TrimSuffix(topOut, "\n"))
	if terr != nil || topReal != dir {
		return uninitialized, nil
	}
	oidOut, _, err := run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return uninitialized, nil
	}
	content := strings.TrimSpace(oidOut)
	stOut, _, serr := run(ctx, dir, "status", "--porcelain")
	if serr != nil {
		return SideContent{}, fmt.Errorf("%w: %w", ErrSubmoduleDirtyProbe, serr)
	}
	if stOut != "" {
		content += "-dirty"
	}
	return finalizeSideContent(content, modeGitlink), nil
}

// readBlobSide 以 git show <blobOID> 读取 blob 内容（regular 与 symlink blob 同路径，
// mode 取自探测记录），并应用内容上限与二进制嗅探。
// ErrOutputTruncated 真值表：溢出且 stdout 非空、stderr 空 →
// 以部分输出继续 finalize（截断语义）；否则透传错误。
// 注意 exec.go 溢出路径返回 fmt.Errorf("%w (%v)", ErrOutputTruncated, ce)，
// errors.As(*exec.ExitError) 在此路径上断裂，MUST 先判 ErrOutputTruncated。
func readBlobSide(ctx context.Context, dir, blobOID, mode string) (SideContent, error) {
	out, stderr, err := run(ctx, dir, "show", blobOID)
	if err != nil {
		if errors.Is(err, ErrOutputTruncated) && out != "" && stderr == "" {
			return finalizeSideContent(out, mode), nil
		}
		return SideContent{}, fmt.Errorf("show %s: %w", blobOID, err)
	}
	return finalizeSideContent(out, mode), nil
}

// finalizeSideContent 应用二进制嗅探与截断标志，MUST NOT 预截断原始字节内容
//（specs/git-operations「文件 diff 查看」单侧内容处理管线唯一顺序：raw bytes → NUL 嗅探 →
// ToValidUTF8 → rune 边界 524288）。NUL 嗅探在完整 raw 上执行以定 IsBinary；
// Truncated 仅表示原始读取超 FileContentMaxBytes，内容裁短（ToValidUTF8 + rune 边界）
// 由调用方核心 helper 完成（internal/task.gitDiffLocked）。旧实现先按 524288 原始字节截断
// 再嗅探，会在多字节 rune 跨界处截成残片，经 ToValidUTF8 产生额外 U+FFFD——与唯一管线相反。
//
// 本函数返回的 Content 为完整 raw（可能超过 FileContentMaxBytes，上限为 execOutputLimit
// 或工作区有界读取 FileContentMaxBytes+1），由调用方做 rune-safe 裁短。mode 为
// 120000/160000 的侧（链接目标/commit OID 文本）MUST NOT 参与嗅探（其内容天然为文本）。
func finalizeSideContent(content, mode string) SideContent {
	sc := SideContent{Content: content, Exists: true, Mode: mode}
	if len(sc.Content) > FileContentMaxBytes {
		sc.Truncated = true
	}
	if mode != modeSymlink && mode != modeGitlink {
		// NUL 嗅探在完整 raw 上执行（对齐 git 启发式：前 binarySniffBytes 字节含 NUL）。
		sc.IsBinary = contentIsBinary(sc.Content)
	}
	return sc
}

// parseLsTreeRecord 解析单条 ls-tree -z 记录 "<mode> <type> <object>\t<path>"。
func parseLsTreeRecord(record string) (mode, typ, oid, path string, ok bool) {
	meta, path, found := strings.Cut(record, "\t")
	if !found || path == "" {
		return "", "", "", "", false
	}
	fields := strings.Fields(meta)
	if len(fields) != 3 {
		return "", "", "", "", false
	}
	return fields[0], fields[1], fields[2], path, true
}

// parseLsFilesStageRecord 解析单条 ls-files -z --stage 记录 "<mode> <object> <stage>\t<path>"。
func parseLsFilesStageRecord(record string) (mode, oid, stage, path string, ok bool) {
	meta, path, found := strings.Cut(record, "\t")
	if !found || path == "" {
		return "", "", "", "", false
	}
	fields := strings.Fields(meta)
	if len(fields) != 3 {
		return "", "", "", "", false
	}
	return fields[0], fields[1], fields[2], path, true
}

// isRegularBlobMode 判定 mode 是否 regular blob（100644/100755）。
func isRegularBlobMode(mode string) bool {
	return mode == modeRegular || mode == modeExecutable
}

// regularFileMode 依工作区权限位映射 regular file 的 git mode 文本：owner 执行位（0100）
// 置位 → 100755，否则 100644（对齐 git canonical mode 口径，group/other 执行位不参与判定）。
func regularFileMode(m os.FileMode) string {
	if m.Perm()&0o100 != 0 {
		return modeExecutable
	}
	return modeRegular
}
