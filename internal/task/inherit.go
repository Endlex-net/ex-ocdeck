package task

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"ocdeck/internal/infrastructure/git"
)

// inheritLogCap 为 task 层 inherit.log 单次写入上限（design.md §7.4：1MB）。
const inheritLogCap = 1 << 20

// inheritLogTruncMarker 超限截断时追加的标记。
const inheritLogTruncMarker = "[log truncated at 1MB]"

// runInherit 执行 Create/retryCreate 的 inherit 编排（design.md §4，tasks 3.2-3.3）。
//
// 同步副作用序列（唯一阻断点为读配置 → creation_failed；其余失败降级为警告）：
//  1. 读配置（唯一阻断点）：失败 → 返回 error（调用方落 creation_failed）。
//     返回配置快照供调用方决定 init_status（避免二次读取把读错误静默当"无 init 脚本"，
//     违反"配置读取失败=唯一阻断点"，design.md §4/不变量 5）。
//  2. ListIgnoredUntracked：失败 → 警告。
//  3. CopyInherited：失败 → 逐条警告（返回 warnings）。
//  4. task 层重写 inherit.log：每次重写；无警告删除既有文件；1MB 截断；写失败仅服务端日志；
//     0600+0700。
//
// 返回 (cfg 快照, warnings, error)：cfg 为读到的配置快照（供调用方决策 init_status）；
// error 仅读配置失败时非 nil。
// repoPath 为主仓库路径（枚举 inherit 来源）；wtPath 为 worktree 路径（inherit 目标）。
func (m *Manager) runInherit(ctx context.Context, repoPath, wtPath string, projectID string) (cfg LifecycleConfigRow, warnings []string, err error) {
	cfg, err = m.store.GetLifecycleConfig(ctx, projectID)
	if err != nil {
		return LifecycleConfigRow{}, nil, fmt.Errorf("read lifecycle config: %w", err)
	}
	// 无 inherit_patterns → 跳过枚举与复制（仍返回 cfg 供调用方决策 init）。
	if cfg.InheritPatterns == "" {
		return cfg, nil, nil
	}
	patterns := splitInheritPatterns(cfg.InheritPatterns)
	if len(patterns) == 0 {
		return cfg, nil, nil
	}

	// ListIgnoredUntracked 失败 → 警告（非阻断）。
	entries, enumErr := git.ListIgnoredUntracked(ctx, repoPath)
	if enumErr != nil {
		warnings = append(warnings, fmt.Sprintf("inherit: enumerate ignored/untracked: %v", enumErr))
		// 无枚举结果仍写日志（记录警告）。
		return cfg, warnings, nil
	}

	// CopyInherited 仅返回警告（design.md §7.1：匹配/复制失败一律降级为逐条警告）。
	if m.lifecycleRunner != nil {
		copyWarnings := m.lifecycleRunner.CopyInherited(ctx, repoPath, wtPath, entries, patterns)
		warnings = append(warnings, copyWarnings...)
	}
	return cfg, warnings, nil
}

// writeInheritLog 写 task 层 inherit.log（design.md §4，tasks 3.3）。
// 行为：每次重写（truncate）；无警告且文件不存在 → no-op；无警告且文件存在 → 删除；
// 有警告 → 写入（1MB 截断加标记）；写失败仅服务端日志（不阻断 Create）；
// 日志目录 0700、文件 0600。
// logPath 为 inherit.log 的完整路径。
func (m *Manager) writeInheritLog(logPath string, warnings []string) {
	dir := filepath.Dir(logPath)
	if len(warnings) == 0 {
		// 无警告：删除既有文件（幂等，design.md §4：无警告删除既有文件）。
		if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
			log.Printf("inherit: remove stale inherit.log %s: %v", logPath, err)
		}
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("inherit: mkdir log dir %s: %v", dir, err)
		return
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		log.Printf("inherit: chmod log dir %s: %v", dir, err)
		return
	}
	var b []byte
	for _, w := range warnings {
		b = appendBytes(b, w)
		b = append(b, '\n')
		if len(b) >= inheritLogCap {
			b = b[:inheritLogCap]
			b = append(b, inheritLogTruncMarker...)
			break
		}
	}
	if err := os.WriteFile(logPath, b, 0o600); err != nil {
		log.Printf("inherit: write inherit.log %s: %v", logPath, err)
		return
	}
	_ = os.Chmod(logPath, 0o600)
}

// appendBytes 追加字符串字节到 slice（避免 strings.Builder 引入额外分配）。
func appendBytes(b []byte, s string) []byte { return append(b, s...) }

// splitInheritPatterns 逐行解析 inherit_patterns 字段（design.md §7.1/§8，与 API 校验侧
// lifecycle_config.go validate 语义一致）：每行 TrimSpace、忽略空行与 # 开头注释行，
// 保留行内空格（一行 = 一个 pattern）。同一份配置两侧解析出相同的 pattern 集合。
func splitInheritPatterns(s string) []string {
	var out []string
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// inheritLogPath 返回 <logDir>/<taskID>/inherit.log（design.md §7.4）。
func (m *Manager) inheritLogPath(taskID string) string {
	return filepath.Join(m.logDir, taskID, "inherit.log")
}

// initLogPath 返回 <logDir>/<taskID>/init.log（design.md §7.4）。
func (m *Manager) initLogPath(taskID string) string {
	return filepath.Join(m.logDir, taskID, "init.log")
}

// preDeleteLogPath 返回 <logDir>/<taskID>/pre-delete.log（design.md §7.4）。
func (m *Manager) preDeleteLogPath(taskID string) string {
	return filepath.Join(m.logDir, taskID, "pre-delete.log")
}

// lifecycleLogDir 返回 <logDir>/<taskID>/（删除成功后 best-effort 删除）。
func (m *Manager) lifecycleLogDir(taskID string) string {
	return filepath.Join(m.logDir, taskID)
}

// removeLifecycleLogDir 删除任务的日志目录（best-effort，忽略错误）。
func (m *Manager) removeLifecycleLogDir(taskID string) {
	dir := m.lifecycleLogDir(taskID)
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("lifecycle: remove log dir %s: %v", dir, err)
	}
}

// lifecycleLogTailLimit 为日志读取 API 的响应上限（design.md §7.4/§8：tail ≤64KB）。
const lifecycleLogTailLimit = 64 << 10

// ReadInitLog 读取任务 init 日志（design.md §7.4/§8，api TaskBackend 接口）。
// 任务不存在 → not_found；响应 = inherit 警告节（inherit.log 存在时，冠以 `[inherit warnings]`
// 节标题）+ init.log 拼接；无日志文件返回空串（非错误）；整体 tail ≤64KB
// （保留末尾，init.log 尾部优先）。
func (m *Manager) ReadInitLog(ctx context.Context, taskID string) (string, error) {
	if _, err := m.store.GetTask(ctx, taskID); err != nil {
		return "", newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	inherit, err := readLifecycleLog(m.inheritLogPath(taskID))
	if err != nil {
		return "", newOpErr(codeInternal, fmt.Errorf("read inherit log: %w", err))
	}
	init, err := readLifecycleLog(m.initLogPath(taskID))
	if err != nil {
		return "", newOpErr(codeInternal, fmt.Errorf("read init log: %w", err))
	}
	var sb strings.Builder
	if inherit != "" {
		sb.WriteString("[inherit warnings]\n")
		sb.WriteString(inherit)
		if !strings.HasSuffix(inherit, "\n") {
			sb.WriteByte('\n')
		}
	}
	sb.WriteString(init)
	return tailString(sb.String(), lifecycleLogTailLimit), nil
}

// ReadPreDeleteLog 读取任务 pre-delete 日志（design.md §7.4/§8，api TaskBackend 接口）。
// 任务不存在 → not_found；无日志文件返回空串（非错误）；tail ≤64KB。
func (m *Manager) ReadPreDeleteLog(ctx context.Context, taskID string) (string, error) {
	if _, err := m.store.GetTask(ctx, taskID); err != nil {
		return "", newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	content, err := readLifecycleLog(m.preDeleteLogPath(taskID))
	if err != nil {
		return "", newOpErr(codeInternal, fmt.Errorf("read pre-delete log: %w", err))
	}
	return tailString(content, lifecycleLogTailLimit), nil
}

// readLifecycleLog 读取日志文件全文；文件不存在返回空串（非错误）。
// 写入侧已按 1MB 上限截断，读取内存有界。
func readLifecycleLog(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// tailString 保留 s 的末尾 limit 字节（日志 tail 语义：最新内容优先）。
// 截断点落在 UTF-8 字符中间时从下一个有效边界开始（丢弃残缺的 continuation bytes），
// 避免返回非法 UTF-8 串导致客户端解码异常。
func tailString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	tail := s[len(s)-limit:]
	for len(tail) > 0 && (tail[0]&0xC0) == 0x80 {
		tail = tail[1:]
	}
	return tail
}
