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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	targetReal, notExist, cerr := resolveConfined(worktree, target, path)
	if cerr != nil {
		if errors.Is(cerr, git.ErrWorktreeEscape) {
			return diffreview.FileEditRawFile{}, newOpErr(codeInvalidInput, cerr)
		}
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, cerr)
	}
	if notExist {
		return diffreview.FileEditRawFile{}, nil
	}
	f, err := os.Open(targetReal)
	if err != nil {
		if os.IsNotExist(err) {
			return diffreview.FileEditRawFile{}, nil
		}
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, fmt.Errorf("open %q: %w", path, err))
	}
	defer f.Close()
	// 读取 FileEditMaxBytes+1 字节，+1 供 service 判定 too_large。
	data, rerr := io.ReadAll(io.LimitReader(f, int64(diffreview.FileEditMaxBytes)+1))
	if rerr != nil {
		return diffreview.FileEditRawFile{}, newOpErr(codeInternal, fmt.Errorf("read %q: %w", path, rerr))
	}
	return diffreview.FileEditRawFile{
		Exists: true,
		Mode:   fileModeToOctal(info.Mode()),
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

	// 步骤 4：重读当前精确字节。
	currentBytes, err := readExactBytes(target)
	if err != nil {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, fmt.Errorf("re-read %q: %w", req.Path, err))
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
	currentMode := fileModeToOctal(info.Mode())
	if currentMode != baseMode {
		return diffreview.FileEditWriteResult{}, newOpErr(codeConflict, fmt.Errorf("mode changed (expected %s, got %s)", req.BaseMode, diffreview.ModeToOctalString(currentMode)))
	}

	// 步骤 5：BOM 推导 + 换行重建（调用 diffreview 纯函数）。
	hasBOM := diffreview.DeriveBOM(currentBytes)
	rebuilt := diffreview.RebuildWriteBytes(req.Content, req.LineEnding, hasBOM)
	if len(rebuilt) > diffreview.FileEditMaxBytes {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInvalidInput, fmt.Errorf("rebuilt content exceeds 512KiB (%d bytes)", len(rebuilt)))
	}

	// 步骤 6：同目录临时文件写入 + chmod(baseMode 含特殊位) + flush。
	tempPath, err := writeTempFile(worktree, req.Path, rebuilt, baseMode)
	if err != nil {
		return diffreview.FileEditWriteResult{}, newOpErr(codeInternal, fmt.Errorf("write temp file: %w", err))
	}

	// preFinalCheckHook 供终检测试注入：在临时文件写入后、终检前修改目标文件，
	// 验证终检（非初检）能检测到外部改动。生产路径为 nil（无副作用）。
	if preFinalCheckHook != nil {
		preFinalCheckHook(target)
	}

	// 步骤 7：rename 前终检（乐观并发）。任一不匹配 → 删临时文件 + conflict。
	if cerr := finalCheck(target, worktree, req.Path, baseMode, currentHash); cerr != nil {
		_ = os.Remove(tempPath)
		return diffreview.FileEditWriteResult{}, cerr
	}

	// 步骤 8：原子 rename。
	if err := os.Rename(tempPath, target); err != nil {
		_ = os.Remove(tempPath)
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

// readExactBytes 读取文件的全部精确字节（无上限，用于 hash 计算与终检比对）。
// 调用方已通过禁锢校验与 regular 判定。文件消失 → invalid_state 由调用方处理。
func readExactBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// hasNewline 判定字节是否含换行字符（\n 或 \r）。
func hasNewline(b []byte) bool {
	return strings.ContainsRune(string(b), '\n') || strings.ContainsRune(string(b), '\r')
}

// writeTempFile 在目标文件同目录写入临时文件，chmod 为 baseMode（含特殊位映射），flush。
// 按 design.md D5 固定顺序：Write → Chmod（在打开的句柄上）→ Sync → Close。
// mode 变更经句柄 Fsync 落盘（Sync 刷新文件数据与元数据），避免 close 后按路径 Chmod
// 导致 mode 变更未 flush。
// 返回临时文件路径。临时文件名使用固定前缀 + 随机后缀，避免冲突。
func writeTempFile(worktree, reqPath string, data []byte, baseMode uint32) (string, error) {
	dir := filepath.Join(worktree, filepath.Dir(reqPath))
	// 禁锢校验目录在 worktree 内（复用 resolveConfined）。
	dirReal, _, cerr := resolveConfined(worktree, dir, filepath.Dir(reqPath))
	if cerr != nil {
		return "", cerr
	}
	f, err := os.CreateTemp(dirReal, ".ocdeck-fileedit-*")
	if err != nil {
		return "", err
	}
	tempPath := f.Name()
	// 任一失败清理临时文件。
	cleanup := func() { _ = os.Remove(tempPath) }
	// 步骤 6a：Write 数据。
	if _, werr := f.Write(data); werr != nil {
		f.Close()
		cleanup()
		return "", werr
	}
	// 步骤 6b：Chmod 在打开的句柄上设置 baseMode（含特殊位映射）。
	if cerr := f.Chmod(octalToFileMode(baseMode)); cerr != nil {
		f.Close()
		cleanup()
		return "", cerr
	}
	// 步骤 6c：Sync flush 数据与 mode 元数据变更到磁盘。
	if ferr := f.Sync(); ferr != nil {
		f.Close()
		cleanup()
		return "", ferr
	}
	// 步骤 6d：Close 关闭句柄。
	if cerr := f.Close(); cerr != nil {
		cleanup()
		return "", cerr
	}
	return tempPath, nil
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

// finalCheck 执行 rename 前终检（design.md D5 步骤 7）。
// 禁锢 + regular + 目标精确字节 SHA-256 + 当前 mode 与基线（baseMode）比对。
// 任一不匹配 → 删除临时文件由调用方负责，返回 conflict 错误。
func finalCheck(target, worktree, path string, baseMode uint32, baselineHash string) error {
	info, err := os.Lstat(target)
	if err != nil {
		return newOpErr(codeConflict, fmt.Errorf("final check: stat %q: %w", path, err))
	}
	if !info.Mode().IsRegular() {
		return newOpErr(codeConflict, fmt.Errorf("final check: %q is no longer regular", path))
	}
	if _, notExist, cerr := resolveConfined(worktree, target, path); cerr != nil {
		if errors.Is(cerr, git.ErrWorktreeEscape) {
			return newOpErr(codeConflict, cerr)
		}
		return newOpErr(codeInternal, cerr)
	} else if notExist {
		return newOpErr(codeConflict, fmt.Errorf("final check: %q disappeared", path))
	}
	currentBytes, err := readExactBytes(target)
	if err != nil {
		return newOpErr(codeConflict, fmt.Errorf("final check: read %q: %w", path, err))
	}
	if diffreview.SHA256Hex(currentBytes) != baselineHash {
		return newOpErr(codeConflict, fmt.Errorf("final check: hash changed for %q", path))
	}
	if fileModeToOctal(info.Mode()) != baseMode {
		return newOpErr(codeConflict, fmt.Errorf("final check: mode changed for %q", path))
	}
	return nil
}