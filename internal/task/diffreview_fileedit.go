// diffreview_fileedit.go 实现 diffreview.FileEditPort 的 task 层 adapter（design.md D5/D9）。
//
// FileEditPortAdapter 包装 Manager：tryLockTask + task/worktree/repo 校验 + 文件系统操作
//（禁锢/Lstat/regular 判定/原始字节读取/临时文件写入/chmod/终检/rename）。领域逻辑
//（reasonCode 判定、BOM 推导、换行重建、SHA-256）由 diffreview 包纯函数承载，adapter 调用之。
//
// 分层（design.md D9）：adapter 仅协调文件系统操作与任务锁，MUST NOT 包含领域判定逻辑。
package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"ocdeck/internal/application/diffreview"
	"ocdeck/internal/infrastructure/git"
)

// FileEditPortAdapter 实现 diffreview.FileEditPort（design.md D5/D9，task 层）。
type FileEditPortAdapter struct {
	m *Manager
}

// NewFileEditPortAdapter 构造 FileEditPort adapter。
func NewFileEditPortAdapter(m *Manager) *FileEditPortAdapter {
	return &FileEditPortAdapter{m: m}
}

// 编译期断言：*FileEditPortAdapter 实现 diffreview.FileEditPort。
var _ diffreview.FileEditPort = (*FileEditPortAdapter)(nil)

// ReadRaw 实现 diffreview.FileEditPort.ReadRaw（design.md D5 判别联合读取的文件系统部分）。
// 执行：词法校验 → tryLockTask → task/worktree/repo 校验 → 禁锢+Lstat+regular 判定 → 有界读取。
// 文件缺失 → FileEditRawFile{Exists:false}（非 error）；非 regular → *FileEditReadRawError；
// IO 错误 → internal；禁锢逃逸 → invalid_input；任务锁冲突 → conflict。
func (a *FileEditPortAdapter) ReadRaw(ctx context.Context, taskID, path string) (diffreview.FileEditRawFile, error) {
	// 阶段①词法校验（先于任务锁与任何文件操作）。
	if err := git.ValidateDiffPath(path); err != nil {
		return diffreview.FileEditRawFile{}, newOpErr(codeInvalidInput, err)
	}

	unlock, err := a.m.tryLockTask(taskID)
	if err != nil {
		return diffreview.FileEditRawFile{}, err
	}
	defer unlock()

	row, err := a.m.store.GetTask(ctx, taskID)
	if err != nil {
		return diffreview.FileEditRawFile{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.WorktreePath == "" {
		return diffreview.FileEditRawFile{}, newOpErr(codeInvalidState, fmt.Errorf("task %s has no worktree", taskID))
	}
	if _, err := a.m.assertGitRepoTask(ctx, row); err != nil {
		return diffreview.FileEditRawFile{}, err
	}

	return readRawFile(row.WorktreePath, path)
}

// readRawFile 执行禁锢+Lstat+regular 判定+有界读取（design.md D5，content.go 同规则）。
// 返回值语义：缺失 → Exists=false（非 error）；非 regular → *FileEditReadRawError；
// IO 错误 → internal；禁锢逃逸 → invalid_input。
// G2：与写路径同一 FD 锚定体系——父目录经 safeOpenWorktreeRoot + walkOpenatDir 建立 dirFd，
// 叶级经 openat(O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC) 打开并在同一 FD 上 fstat 取 mode、有界读取；
// Lstat/EvalSymlinks 之后的 leaf symlink swap 与中间目录 swap 均被拒绝（不得跟随读到外部内容），
// bytes 与 mode 保证来自同一对象。
func readRawFile(worktree, path string) (diffreview.FileEditRawFile, error) {
	target := filepath.Join(worktree, path)
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return diffreview.FileEditRawFile{}, nil
		}
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, fmt.Errorf("stat %q: %w", path, err))
	}
	// symlink/dir/fifo/socket 等非 regular 类型 → not_regular（不跟随 symlink）。
	if !info.Mode().IsRegular() {
		return diffreview.FileEditRawFile{}, &diffreview.FileEditReadRawError{NotRegular: true}
	}
	// 禁锢校验：resolved target MUST 位于 worktree 根内（防中间级 symlink 逃逸）。
	if _, notExist, cerr := resolveConfined(worktree, target, path); cerr != nil {
		if errors.Is(cerr, git.ErrWorktreeEscape) {
			return diffreview.FileEditRawFile{}, newOpErr(codeInvalidInput, cerr)
		}
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, cerr)
	} else if notExist {
		return diffreview.FileEditRawFile{}, nil
	}
	// readRawPostConfineHook 供 G2 测试注入：在 target 禁锢校验后、父目录解析前调用
	// （生产为 nil）。测试在此替换 leaf/中间目录/内容+mode，验证 resolve 后替换被
	// O_NOFOLLOW/FD-fstat 拒绝或按新对象读取（旧 Lstat→os.Open 实现会读到替换后内容/旧 mode）。
	if readRawPostConfineHook != nil {
		readRawPostConfineHook(target)
	}
	// G2：父目录经安全 root FD 逐组件 walk 建立 dirFd（root/中间目录在 resolve 后被替换为
	// symlink 即拒绝）。
	dirReal, dirNotExist, derr := resolveConfined(worktree, filepath.Dir(target), filepath.Dir(path))
	if derr != nil {
		if errors.Is(derr, git.ErrWorktreeEscape) {
			return diffreview.FileEditRawFile{}, newOpErr(codeInvalidInput, derr)
		}
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, derr)
	}
	if dirNotExist {
		return diffreview.FileEditRawFile{}, nil
	}
	rootFd, rootReal, _, err := safeOpenWorktreeRoot(worktree)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return diffreview.FileEditRawFile{}, &diffreview.FileEditReadRawError{NotRegular: true}
		}
		if errors.Is(err, syscall.ENOENT) {
			return diffreview.FileEditRawFile{}, nil
		}
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, fmt.Errorf("open worktree root: %w", err))
	}
	defer unix.Close(rootFd)
	relDir, relErr := filepath.Rel(rootReal, dirReal)
	if relErr != nil || filepath.IsAbs(relDir) || relDir == ".." || strings.HasPrefix(relDir, "../") {
		return diffreview.FileEditRawFile{}, newOpErr(codeInvalidInput, fmt.Errorf("%w: parent of %q escapes worktree", git.ErrWorktreeEscape, path))
	}
	dirFd, werr := walkOpenatDir(rootFd, relDir)
	if werr != nil {
		if errors.Is(werr, syscall.ELOOP) || errors.Is(werr, syscall.ENOTDIR) {
			return diffreview.FileEditRawFile{}, &diffreview.FileEditReadRawError{NotRegular: true}
		}
		if errors.Is(werr, syscall.ENOENT) {
			return diffreview.FileEditRawFile{}, nil
		}
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, fmt.Errorf("open parent dir of %q: %w", path, werr))
	}
	defer unix.Close(dirFd)
	// 叶级 openat：O_NOFOLLOW 拒绝 Lstat 后被替换的 symlink；O_NONBLOCK 防 FIFO 阻塞。
	fd, oerr := unix.Openat(dirFd, filepath.Base(target), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if oerr != nil {
		if errors.Is(oerr, syscall.ENOENT) {
			return diffreview.FileEditRawFile{}, nil
		}
		if errors.Is(oerr, syscall.ELOOP) || errors.Is(oerr, syscall.ENXIO) || errors.Is(oerr, syscall.EOPNOTSUPP) {
			// 被替换为 symlink/socket 等设备文件 → not_regular（与 Lstat 判定同语义）。
			return diffreview.FileEditRawFile{}, &diffreview.FileEditReadRawError{NotRegular: true}
		}
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, fmt.Errorf("open %q: %w", path, oerr))
	}
	f := os.NewFile(uintptr(fd), filepath.Base(target))
	defer f.Close()
	// 同一 FD 上 fstat：mode 与 bytes 来自同一对象（G2 基线一致性）。
	st, serr := f.Stat()
	if serr != nil {
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, fmt.Errorf("stat %q: %w", path, serr))
	}
	if !st.Mode().IsRegular() {
		return diffreview.FileEditRawFile{}, &diffreview.FileEditReadRawError{NotRegular: true}
	}
	// 读取 FileEditMaxBytes+1 字节，+1 供 service 判定 too_large。
	data, rerr := io.ReadAll(io.LimitReader(f, int64(diffreview.FileEditMaxBytes)+1))
	if rerr != nil {
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, fmt.Errorf("read %q: %w", path, rerr))
	}
	return diffreview.FileEditRawFile{
		Exists: true,
		Mode:   fileModeToOctal(st.Mode()),
		Bytes:  data,
	}, nil
}

// resolveConfined 解析 p 的 EvalSymlinks 真实路径并执行禁锢校验（与 git.resolveInWorktree 同规则）。
// 真实路径 MUST 位于 worktree 根内。p 消失（竞态）→ notExist=true。
func resolveConfined(worktree, p, path string) (real string, notExist bool, err error) {
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
		return "", false, fmt.Errorf("%w: %q resolves to %q", git.ErrWorktreeEscape, path, real)
	}
	return real, false, nil
}

// Write 实现 diffreview.FileEditPort.Write（design.md D5 步骤 2-9，adapter 持锁内完成）。
// 步骤 1（领域格式校验）由 FileEditService.WriteFile 在调用前完成。
// 本方法执行：词法校验 → tryLockTask → task/worktree/repo 校验 → 禁锢复检（步骤 3）→
// 重读当前字节+hash/换行/mode 比对（步骤 4）→ BOM 推导+换行重建（步骤 5，调用 diffreview 纯函数）→
// 临时文件写入+chmod（步骤 6）→ 终检（步骤 7）→ rename（步骤 8）→ 返回新 hash（步骤 9）。
// 前置失败零副作用（不创建临时文件/不改目标）；rename 前失败清理临时文件。
func (a *FileEditPortAdapter) Write(ctx context.Context, taskID string, req diffreview.FileEditWriteRequest) (diffreview.FileEditWriteResult, error) {
	// 词法校验（先于任务锁）。
	if err := git.ValidateDiffPath(req.Path); err != nil {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidInput, err)
	}

	unlock, err := a.m.tryLockTask(taskID)
	if err != nil {
		return diffreview.FileEditWriteResult{}, err
	}
	defer unlock()

	// 步骤 2：task 存在→worktree 非空→repo kind 校验。
	row, err := a.m.store.GetTask(ctx, taskID)
	if err != nil {
		return diffreview.FileEditWriteResult{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.WorktreePath == "" {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidState, fmt.Errorf("task %s has no worktree", taskID))
	}
	if _, err := a.m.assertGitRepoTask(ctx, row); err != nil {
		return diffreview.FileEditWriteResult{}, err
	}

	worktree := row.WorktreePath
	target := filepath.Join(worktree, req.Path)
	base := filepath.Base(req.Path)

	// 步骤 3：禁锢复检（Lstat + resolved parent/target 禁锢 + regular，MUST 早于临时文件创建）。
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidState, fmt.Errorf("file %q disappeared: %w", req.Path, err))
		}
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, fmt.Errorf("stat %q: %w", req.Path, err))
	}
	if !info.Mode().IsRegular() {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidState, fmt.Errorf("file %q is no longer regular", req.Path))
	}
	if _, notExist, cerr := resolveConfined(worktree, target, req.Path); cerr != nil {
		if errors.Is(cerr, git.ErrWorktreeEscape) {
			return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidInput, cerr)
		}
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, cerr)
	} else if notExist {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidState, fmt.Errorf("file %q disappeared during confinement check", req.Path))
	}

	// preTempWriteHook 供 F6 测试注入：在父目录解析前调用（生产为 nil）。
	if preTempWriteHook != nil {
		preTempWriteHook(target)
	}

	// 步骤 4 前置：目标父目录必须存在且禁锢在 worktree 内。notExist 或空 dirReal → invalid_state
	// （F6：禁止把空目录传给临时文件创建——空 dir 会落系统临时目录，违反同目录契约）。
	dirReal, dirNotExist, derr := resolveConfined(worktree, filepath.Join(worktree, filepath.Dir(req.Path)), filepath.Dir(req.Path))
	if derr != nil {
		if errors.Is(derr, git.ErrWorktreeEscape) {
			return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidInput, derr)
		}
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, derr)
	}
	if dirNotExist || dirReal == "" {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidState, fmt.Errorf("parent dir of %q disappeared", req.Path))
	}
	// F17：安全打开 canonical worktree root（EvalSymlinks 后从文件系统根逐组件 O_NOFOLLOW
	// 走到 root——Eval→Open 间隙组件被替换为 symlink 即拒绝），并记录 root dev:ino；
	// 终检与 parent 身份一并比对（root 被移动/替换时不得继续写旧目录还报告成功）。
	rootFd, rootReal, rootIdentity, err := safeOpenWorktreeRoot(worktree)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidState, fmt.Errorf("worktree root replaced during confinement: %v", err))
		}
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, fmt.Errorf("open worktree root: %w", err))
	}
	defer unix.Close(rootFd)
	// F15：从 worktree root FD 逐组件 openat(O_NOFOLLOW|O_DIRECTORY) 建立父目录 FD。
	// resolveConfined 与打开目录之间组件被替换为 symlink 也会 ELOOP 拒绝（dirReal 各组件在
	// resolve 时点均非 symlink，此后被替换即检出）。记录父目录 dev:ino 供终检身份比对。
	relDir, relErr := filepath.Rel(rootReal, dirReal)
	if relErr != nil || filepath.IsAbs(relDir) || relDir == ".." || strings.HasPrefix(relDir, "../") {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidInput, fmt.Errorf("%w: parent of %q escapes worktree", git.ErrWorktreeEscape, req.Path))
	}
	dirFd, err := walkOpenatDir(rootFd, relDir)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			// ELOOP(linux)/ENOTDIR(darwin)：组件为 symlink/非目录；ENOENT：组件消失。
			return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidState, fmt.Errorf("parent dir of %q replaced during confinement", req.Path))
		}
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, fmt.Errorf("open parent dir of %q: %w", req.Path, err))
	}
	defer unix.Close(dirFd)
	dirIdentity, err := statDirIdentity(dirFd)
	if err != nil {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, fmt.Errorf("stat parent dir of %q: %w", req.Path, err))
	}

	// 步骤 4：同一 FD 有界重读当前精确字节（openat O_NOFOLLOW|O_NONBLOCK + f.Stat 复检打开对象，
	// ≤512KiB 有界读防外部增长撑爆内存）。初检阶段：消失/类型变化 → invalid_state；
	// 外部增长超上限 → conflict（内容必不同于基线）。
	currentBytes, curFileMode, err := readBaselineBytesAt(dirFd, base, req.Path, checkPhaseInitial)
	if err != nil {
		return diffreview.FileEditWriteResult{}, err
	}
	currentHash := diffreview.SHA256Hex(currentBytes)
	if currentHash != req.BaseHash {
		return diffreview.FileEditWriteResult{}, newOpErr(codeConflict, fmt.Errorf("base hash mismatch (expected %s, got %s)", req.BaseHash, currentHash))
	}
	// 当前文件含换行且其风格与请求 lineEnding 不一致 → conflict。
	if hasNewline(currentBytes) {
		curLE, leOK := diffreview.DetectLineEnding(currentBytes)
		if !leOK || curLE != req.LineEnding {
			return diffreview.FileEditWriteResult{}, newOpErr(codeConflict, fmt.Errorf("line ending changed (frozen %s, current mismatch)", req.LineEnding))
		}
	}
	// 当前 mode（完整 chmod 含特殊位）与请求 baseMode 不一致 → conflict。
	baseMode, _ := diffreview.ParseBaseMode(req.BaseMode) // service 已校验合法性，此处 ok
	currentMode := fileModeToOctal(curFileMode)
	if currentMode != baseMode {
		return diffreview.FileEditWriteResult{}, newOpErr(codeConflict, fmt.Errorf("mode changed (expected %s, got %s)", req.BaseMode, diffreview.ModeToOctalString(currentMode)))
	}

	// 步骤 5：BOM 推导 + 换行重建（调用 diffreview 纯函数）。
	hasBOM := diffreview.DeriveBOM(currentBytes)
	rebuilt := diffreview.RebuildWriteBytes(req.Content, req.LineEnding, hasBOM)
	if len(rebuilt) > diffreview.FileEditMaxBytes {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidInput, fmt.Errorf("rebuilt content exceeds 512KiB (%d bytes)", len(rebuilt)))
	}

	// 步骤 6：同目录临时文件写入 + chmod(baseMode 含特殊位) + flush（openat 相对 dirFd 创建）。
	tempName, err := writeTempFileAt(dirFd, rebuilt, baseMode)
	if err != nil {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, fmt.Errorf("write temp file: %w", err))
	}
	// 临时文件清理统一经 unlinkat（dirFd 锚定，父目录被替换也不误删外部路径）。
	cleanupTemp := func() { _ = unix.Unlinkat(dirFd, tempName, 0) }

	// preFinalCheckHook 供终检测试注入：在临时文件写入后、终检前修改目标文件，
	// 验证终检（非初检）能检测到外部改动。生产路径为 nil（无副作用）。
	if preFinalCheckHook != nil {
		preFinalCheckHook(target)
	}

	// 步骤 7 前置（F15/F17）：重新安全打开当前 worktree root 并比对 root 与 parent 的
	// dev:ino 身份——root/parent 逃逸/消失/移动/替换均 → conflict（终检阶段语义）。
	// 通过后才在原 dirFd 上终检和 rename。
	if cerr := verifyDirIdentity(worktree, req.Path, rootIdentity, dirIdentity); cerr != nil {
		cleanupTemp()
		return diffreview.FileEditWriteResult{}, cerr
	}

	// 步骤 7：rename 前终检（乐观并发，dirFd 锚定）。任一不匹配 → 删临时文件 + conflict。
	if cerr := finalCheckAt(dirFd, base, req.Path, baseMode, currentHash); cerr != nil {
		cleanupTemp()
		return diffreview.FileEditWriteResult{}, cerr
	}

	// 步骤 8：原子 rename（renameat 相对 dirFd，F13：不经可被替换的路径组件）。
	if err := unix.Renameat(dirFd, tempName, dirFd, base); err != nil {
		cleanupTemp()
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, fmt.Errorf("rename temp to %q: %w", req.Path, err))
	}

	// 步骤 9：响应新 baseHash。直接取已知 rebuilt bytes 的 SHA-256（rename 已将临时文件
	// 原子替换为目标，内容即为 rebuilt bytes；MUST NOT 重读目标——可能读到并发外部写入或
	// 因 IO 失败把已成功的写报告为 internal）。
	return diffreview.FileEditWriteResult{BaseHash: diffreview.SHA256Hex(rebuilt)}, nil
}

// preFinalCheckHook 仅供终检测试注入：在临时文件写入后、终检前调用，参数为目标文件路径。
// 测试在此修改目标文件（mode/hash/类型），验证终检（非初检）检测到外部改动。生产路径为 nil。
var preFinalCheckHook func(target string)

// preTempWriteHook 仅供 F6 测试注入：在步骤 6 父目录解析前调用，参数为目标文件路径。
// 测试在此删除父目录，验证临时文件不得落系统临时目录。生产路径为 nil。
var preTempWriteHook func(target string)

// readRawPostConfineHook 仅供 G2 测试注入：readRawFile 在 target 禁锢校验后、父目录
// 解析前调用，参数为目标文件路径。测试在此替换 leaf/中间目录/内容+mode，验证 resolve
// 后替换被 O_NOFOLLOW/FD-fstat 拒绝或按新对象读取。生产路径为 nil。
var readRawPostConfineHook func(target string)

// checkPhase 标记读取发生的阶段，决定消失/类型变化的错误映射（design.md D5 错误矩阵：
// 初检缺失/类型变化 → invalid_state；终检相对初检的任何变化 → conflict）。
type checkPhase int

const (
	checkPhaseInitial checkPhase = iota // 写回初检（步骤 4）
	checkPhaseFinal                     // rename 前终检（步骤 7）
)

// phaseChangeErr 按阶段映射「目标消失/类型变化/外部增长」：初检 → invalid_state；终检 → conflict。
func phaseChangeErr(phase checkPhase, err error) error {
	if phase == checkPhaseInitial {
		return newOpErr(codeInvalidState, err)
	}
	return newOpErr(codeConflict, err)
}

// readBaselineBytesAt 经 openat 相对 dirFd（已禁锢父目录 FD）有界读取基线文件并返回
// 内容与打开对象的 mode（F5/F13）。
// O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC：base 为 symlink 时 ELOOP 拒绝（不跟随），FIFO 等不阻塞；
// f.Stat 在已打开句柄上复检类型/mode。读取上限 FileEditMaxBytes+1，外部增长超上限按
// conflict 处理（内容必不同于基线，且避免无界内存）。ELOOP/设备文件/类型竞态按阶段映射：
// 初检 → invalid_state；终检 → conflict。dirFd 锚定已验证目录，中间目录 symlink 替换无法逃逸。
func readBaselineBytesAt(dirFd int, base, path string, phase checkPhase) ([]byte, os.FileMode, error) {
	fd, err := unix.Openat(dirFd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, 0, phaseChangeErr(phase, fmt.Errorf("file %q disappeared: %w", path, err))
		}
		if errors.Is(err, syscall.ELOOP) {
			// Lstat 后被替换为 symlink → 类型竞态（F13：不得跟随）。
			return nil, 0, phaseChangeErr(phase, fmt.Errorf("file %q replaced by symlink during check", path))
		}
		if errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.EOPNOTSUPP) {
			// 被替换为 socket 等设备文件 → open 阶段即拒绝（F13：类型竞态，非 internal；
			// darwin 报 EOPNOTSUPP "operation not supported on socket"，linux 报 ENXIO）。
			return nil, 0, phaseChangeErr(phase, fmt.Errorf("file %q replaced by non-regular device during check", path))
		}
		return nil, 0, newOpErr(codeInternal, fmt.Errorf("open %q: %w", path, err))
	}
	f := os.NewFile(uintptr(fd), base)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, newOpErr(codeInternal, fmt.Errorf("stat %q: %w", path, err))
	}
	if !st.Mode().IsRegular() {
		return nil, 0, phaseChangeErr(phase, fmt.Errorf("file %q is no longer regular", path))
	}
	data, rerr := io.ReadAll(io.LimitReader(f, int64(diffreview.FileEditMaxBytes)+1))
	if rerr != nil {
		return nil, 0, newOpErr(codeInternal, fmt.Errorf("read %q: %w", path, rerr))
	}
	if len(data) > diffreview.FileEditMaxBytes {
		return nil, 0, newOpErr(codeConflict, fmt.Errorf("file %q grew beyond 512KiB", path))
	}
	return data, st.Mode(), nil
}

// hasNewline 判定字节是否含换行字符（\n 或 \r）。
func hasNewline(b []byte) bool {
	return strings.ContainsRune(string(b), '\n') || strings.ContainsRune(string(b), '\r')
}

// writeTempFileAt 在 dirFd（已禁锢的父目录 FD）锚定的目录内创建临时文件，
// chmod 为 baseMode（含特殊位映射），flush。
// 按 design.md D5 固定顺序：Write → Chmod（在打开的句柄上）→ Sync → Close。
// mode 变更经句柄 Fsync 落盘（Sync 刷新文件数据与元数据），避免 close 后按路径 Chmod
// 导致 mode 变更未 flush。
// 返回临时文件名（相对 dirFd 的 basename，供后续 renameat/unlinkat 使用）。
// F13：经 openat(O_CREAT|O_EXCL) 创建——父目录在 resolve 后被替换也不影响（FD 锚定），
// 且不会跟随/创建到目录外。
func writeTempFileAt(dirFd int, data []byte, baseMode uint32) (string, error) {
	var f *os.File
	var name string
	// 随机后缀重试（O_EXCL 撞名时换名，至多 5 次）。
	for attempt := 0; attempt < 5; attempt++ {
		var rb [6]byte
		if _, err := rand.Read(rb[:]); err != nil {
			return "", fmt.Errorf("temp name rand: %w", err)
		}
		name = ".ocdeck-fileedit-" + hex.EncodeToString(rb[:])
		fd, err := unix.Openat(dirFd, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o600)
		if err != nil {
			if errors.Is(err, syscall.EEXIST) {
				continue
			}
			return "", err
		}
		f = os.NewFile(uintptr(fd), name)
		break
	}
	if f == nil {
		return "", fmt.Errorf("create temp file: name collision after retries")
	}
	// 任一失败清理临时文件（unlinkat 相对 dirFd，不经可替换路径）。
	cleanup := func() {
		f.Close()
		_ = unix.Unlinkat(dirFd, name, 0)
	}
	// 步骤 6a：Write 数据。
	if _, werr := f.Write(data); werr != nil {
		cleanup()
		return "", werr
	}
	// 步骤 6b：Chmod 在打开的句柄上设置 baseMode（含特殊位映射）。
	if cerr := f.Chmod(octalToFileMode(baseMode)); cerr != nil {
		cleanup()
		return "", cerr
	}
	// 步骤 6c：Sync flush 数据与 mode 元数据变更到磁盘。
	if ferr := f.Sync(); ferr != nil {
		cleanup()
		return "", ferr
	}
	// 步骤 6d：Close 关闭句柄。
	if cerr := f.Close(); cerr != nil {
		_ = unix.Unlinkat(dirFd, name, 0)
		return "", cerr
	}
	return name, nil
}

// walkOpenatDir 从 rootFd（已打开的 worktree root FD）逐组件 openat 建立 rel 目录的 FD
//（F15）。每组件 O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC——resolve 后组件被替换为
// symlink 即 ELOOP 拒绝；组件消失即 ENOENT。rel 为 "." 或空时返回 rootFd 的 Dup。
// 返回的 FD 由调用方负责关闭；中途失败已打开的 FD 一律关闭。
// F19：Dup 使用 F_DUPFD_CLOEXEC（dup(2) 语义新 FD 不带 close-on-exec，写事务期间不得可继承）。
func walkOpenatDir(rootFd int, rel string) (int, error) {
	curFd, err := unix.FcntlInt(uintptr(rootFd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if rel == "" || rel == "." {
		return curFd, nil
	}
	for _, comp := range strings.Split(rel, string(filepath.Separator)) {
		if comp == "" || comp == "." {
			continue
		}
		next, oerr := unix.Openat(curFd, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(curFd)
		if oerr != nil {
			return -1, oerr
		}
		curFd = next
	}
	return curFd, nil
}

// safeOpenWorktreeRoot 安全打开 canonical worktree root 并返回 (fd, rootReal, identity)。
// F17：EvalSymlinks 后从文件系统根逐组件 openat(O_NOFOLLOW|O_DIRECTORY) 走到 root——
// Eval→Open 间隙任何路径组件被替换为 symlink 即 ELOOP 拒绝（darwin 报 ENOTDIR 同义覆盖）。
// identity 为 root 的 dev:ino 串（statDirIdentity），供终检比对 root 是否被移动/替换。
func safeOpenWorktreeRoot(worktree string) (int, string, string, error) {
	rootReal, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return -1, "", "", err
	}
	fsRoot, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", "", err
	}
	defer unix.Close(fsRoot)
	rel := strings.TrimPrefix(filepath.Clean(rootReal), string(filepath.Separator))
	fd, werr := walkOpenatDir(fsRoot, rel)
	if werr != nil {
		return -1, "", "", werr
	}
	identity, serr := statDirIdentity(fd)
	if serr != nil {
		unix.Close(fd)
		return -1, "", "", serr
	}
	return fd, rootReal, identity, nil
}

// statDirIdentity 返回目录 FD 的 dev:ino 身份串（F15 终检身份比对用；
// 字符串形式避免 st.Dev 跨平台类型差异）。
func statDirIdentity(fd int) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

// verifyDirIdentity 终检前置（F15/F17）：重新安全打开当前 worktree root，先比对 root
// dev:ino（root 被移动/替换即 conflict），再安全解析当前请求父路径并比对其 dev:ino。
// root/parent 逃逸/消失/移动/替换均 → conflict（终检阶段语义）；检查自身 IO 失败 → internal。
func verifyDirIdentity(worktree, reqPath, wantRootIdentity, wantIdentity string) error {
	rootFd, rootReal, rootIdentity, err := safeOpenWorktreeRoot(worktree)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			return newOpErr(codeConflict, fmt.Errorf("final check: worktree root replaced: %v", err))
		}
		return newOpErr(codeInternal, fmt.Errorf("final check: open worktree root: %w", err))
	}
	defer unix.Close(rootFd)
	if rootIdentity != wantRootIdentity {
		return newOpErr(codeConflict, fmt.Errorf("final check: worktree root replaced or moved"))
	}
	dirReal, notExist, err := resolveConfined(worktree, filepath.Join(worktree, filepath.Dir(reqPath)), filepath.Dir(reqPath))
	if err != nil {
		if errors.Is(err, git.ErrWorktreeEscape) {
			return newOpErr(codeConflict, err)
		}
		return newOpErr(codeInternal, err)
	}
	if notExist {
		return newOpErr(codeConflict, fmt.Errorf("final check: parent dir of %q disappeared", reqPath))
	}
	relDir, relErr := filepath.Rel(rootReal, dirReal)
	if relErr != nil || filepath.IsAbs(relDir) || relDir == ".." || strings.HasPrefix(relDir, "../") {
		return newOpErr(codeConflict, fmt.Errorf("%w: parent of %q escapes worktree", git.ErrWorktreeEscape, reqPath))
	}
	fd, werr := walkOpenatDir(rootFd, relDir)
	if werr != nil {
		if errors.Is(werr, syscall.ELOOP) || errors.Is(werr, syscall.ENOENT) || errors.Is(werr, syscall.ENOTDIR) {
			return newOpErr(codeConflict, fmt.Errorf("final check: parent dir of %q replaced", reqPath))
		}
		return newOpErr(codeInternal, fmt.Errorf("final check: open parent dir of %q: %w", reqPath, werr))
	}
	defer unix.Close(fd)
	identity, serr := statDirIdentity(fd)
	if serr != nil {
		return newOpErr(codeInternal, fmt.Errorf("final check: stat parent dir of %q: %w", reqPath, serr))
	}
	if identity != wantIdentity {
		return newOpErr(codeConflict, fmt.Errorf("final check: parent dir of %q replaced or moved", reqPath))
	}
	return nil
}
// octalToFileMode 将完整 chmod 值（uint32，含特殊位）映射为 os.FileMode。
// design.md D5：Perm() | ModeSetuid(0o4000) | ModeSetgid(0o2000) | ModeSticky(0o1000)。
func octalToFileMode(mode uint32) os.FileMode {
	fm := os.FileMode(mode & 0o777)      // permission bits
	if mode&0o4000 != 0 {                // setuid
		fm |= os.ModeSetuid
	}
	if mode&0o2000 != 0 { // setgid
		fm |= os.ModeSetgid
	}
	if mode&0o1000 != 0 { // sticky
		fm |= os.ModeSticky
	}
	return fm
}

// fileModeToOctal 将 os.FileMode 映射回完整 chmod 值（uint32，含特殊位）。
func fileModeToOctal(fm os.FileMode) uint32 {
	mode := uint32(fm.Perm())
	if fm&os.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if fm&os.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if fm&os.ModeSticky != 0 {
		mode |= 0o1000
	}
	return mode
}

// finalCheckAt 执行 rename 前终检（design.md D5 步骤 7，dirFd 锚定）。
// fstatat(AT_SYMLINK_NOFOLLOW) 检查类型/symlink + openat 重读比对精确字节 SHA-256 +
// 当前 mode 与基线（baseMode）比对。任一不匹配 → 删除临时文件由调用方负责，返回 conflict 错误。
// F13：全部经 dirFd 相对调用——FD 锚定已禁锢目录，中间目录 symlink 在 resolve 后替换无法逃逸；
// 禁锢语义由步骤 3/4 前置的 resolveConfined + dirFd 锚定共同保证。
func finalCheckAt(dirFd int, base, path string, baseMode uint32, baselineHash string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return newOpErr(codeConflict, fmt.Errorf("final check: %q disappeared", path))
		}
		return newOpErr(codeInternal, fmt.Errorf("final check: stat %q: %w", path, err))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		// symlink/目录/设备替换 → 类型变化（终检阶段 → conflict）。
		return newOpErr(codeConflict, fmt.Errorf("final check: %q is no longer regular", path))
	}
	currentBytes, curFileMode, err := readBaselineBytesAt(dirFd, base, path, checkPhaseFinal)
	if err != nil {
		return err
	}
	if diffreview.SHA256Hex(currentBytes) != baselineHash {
		return newOpErr(codeConflict, fmt.Errorf("final check: hash changed for %q", path))
	}
	if fileModeToOctal(curFileMode) != baseMode {
		return newOpErr(codeConflict, fmt.Errorf("final check: mode changed for %q", path))
	}
	return nil
}