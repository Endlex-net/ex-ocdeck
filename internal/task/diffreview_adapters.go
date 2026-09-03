// diffreview_adapters.go 实现 diffreview service 的 task 层 adapter（design.md D9）。
//
// 三类 adapter（均经 Manager 协调，MUST NOT 反向依赖 application/diffreview 以外的 application 包）：
//   - PromptPortAdapter：实现 diffreview.PromptPort，opencode.PromptResult → diffreview.PromptOutcome 显式逐字段映射；
//     经 Manager.taskOcClient(taskID) 获取 client+directory（attention.go:854）；获取失败 ok=false →
//     PromptOutcome{Kind: pre_send_failure, Detail: "runtime client unavailable"}（design.md D1 adapter 获取失败唯一规则）。
//   - DiffSourcePortAdapter：实现 diffreview.DiffSourcePort，经 Manager.gitDiffLocked（已持锁核心 helper）读取。
//   - RuntimePortAdapter：实现 diffreview.RuntimePort，返回任务 runtime 快照（instVersion/锚定会话/能力缓存/SessionStatus）。
//
// 编译期断言保证接口实现完整（design.md D9 五口全覆盖）。
package task

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"ocdeck/internal/application/diffreview"
	"ocdeck/internal/infrastructure/git"
	"ocdeck/internal/infrastructure/opencode"
)

// PromptPortAdapter 实现 diffreview.PromptPort（design.md D1/D9，task 层）。
//
// opencode.PromptResult → diffreview.PromptOutcome 显式逐字段映射（MUST NOT 类型别名跨层共享）。
// 经 Manager.taskOcClient(taskID) 获取当前 client+directory；获取失败 → pre_send_failure 固定文案。
type PromptPortAdapter struct {
	m *Manager
}

// NewPromptPortAdapter 构造 PromptPort adapter。m 为持有 ocFactory/runtimeRegistry 的 Manager。
func NewPromptPortAdapter(m *Manager) *PromptPortAdapter {
	return &PromptPortAdapter{m: m}
}

// 编译期断言：*PromptPortAdapter 实现 diffreview.PromptPort（design.md D9 五口全覆盖）。
var _ diffreview.PromptPort = (*PromptPortAdapter)(nil)

// PromptAsync 投递 text prompt 到目标 session 的异步队列（design.md D1）。
// taskID 路由：经 Manager.taskOcClient(taskID) 获取当前 client+directory，保证多任务投递路由隔离。
// adapter 获取失败（ok=false：任务非 active、凭据/端口缺失等）→ PromptOutcome{Kind: pre_send_failure,
// Detail: "runtime client unavailable"}（design.md D1 adapter 获取失败唯一规则，MUST NOT 标 delivery_unknown）。
// 成功获取 client 后调用 OCClient.PromptAsync（返回 opencode.PromptResult transport DTO），
// 逐字段映射为 diffreview.PromptOutcome。
// files（批注 7）：批注涉及的 worktree 相对路径（已去重），逐一构造 file part——
// file:// 绝对路径 URI + mime "text/plain" + basename；发送前 os.Stat 校验，
// 不存在/非 regular 的文件跳过（避免 agent 收到 "Read tool failed to read" 错误文本）。
func (a *PromptPortAdapter) PromptAsync(ctx context.Context, taskID, sessionID, messageID, text string, files []string) diffreview.PromptOutcome {
	oc, dir, ok := a.m.taskOcClient(ctx, taskID)
	if !ok {
		return diffreview.PromptOutcome{
			Kind:   diffreview.PromptOutcomePreSendFailure,
			Detail: "runtime client unavailable",
		}
	}
	parts := make([]opencode.PromptFilePart, 0, len(files))
	for _, rel := range files {
		abs := filepath.Join(dir, rel)
		st, serr := os.Stat(abs)
		if serr != nil || !st.Mode().IsRegular() {
			continue // 跳过缺失/非 regular 文件（批注快照已在 payload 中，不影响提交语义）
		}
		parts = append(parts, opencode.PromptFilePart{
			URL:      fileURLForPath(abs),
			Mime:     "text/plain",
			Filename: filepath.Base(rel),
		})
	}
	res := oc.PromptAsync(ctx, dir, sessionID, messageID, text, parts)
	return mapPromptResult(res)
}

// fileURLForPath 构造 file:// 绝对路径 URI（anomalyco/opencode FilePartInput 契约：
// fileURLToPath 解码为本地绝对路径；路径段逐段 PathEscape，保持 '/' 分隔）。
func fileURLForPath(abs string) string {
	segs := strings.Split(abs, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "file://" + strings.Join(segs, "/")
}

// mapPromptResult 将 opencode.PromptResult（transport DTO）逐字段映射为 diffreview.PromptOutcome（domain 类型）。
// design.md D1 类型归属条：MUST 显式逐字段转换，MUST NOT 类型别名跨层共享。
// 四值 Kind 一一对应：ResultAccepted→PromptOutcomeAccepted，余类推。
func mapPromptResult(r opencode.PromptResult) diffreview.PromptOutcome {
	var kind diffreview.PromptOutcomeKind
	switch r.Kind {
	case opencode.ResultAccepted:
		kind = diffreview.PromptOutcomeAccepted
	case opencode.ResultHTTPResponse:
		kind = diffreview.PromptOutcomeHTTPResponse
	case opencode.ResultTransportUnknown:
		kind = diffreview.PromptOutcomeTransportUnknown
	case opencode.ResultPreSendFailure:
		kind = diffreview.PromptOutcomePreSendFailure
	default:
		// 未知 Kind（opencode 包契约保证仅四值，兜底 fail-closed 为 pre_send_failure）。
		kind = diffreview.PromptOutcomePreSendFailure
	}
	return diffreview.PromptOutcome{
		Kind:       kind,
		StatusCode: r.StatusCode,
		Body:       r.Body,
		Detail:     r.Detail,
	}
}

// DiffSourcePortAdapter 实现 diffreview.DiffSourcePort（design.md D9，task 层）。
// 经 Manager.gitDiffLocked（已持锁核心 helper）读取，调用方无需再加锁。
type DiffSourcePortAdapter struct {
	m *Manager
}

// NewDiffSourcePortAdapter 构造 DiffSourcePort adapter。
func NewDiffSourcePortAdapter(m *Manager) *DiffSourcePortAdapter {
	return &DiffSourcePortAdapter{m: m}
}

// 编译期断言：*DiffSourcePortAdapter 实现 diffreview.DiffSourcePort（design.md D9 五口全覆盖）。
var _ diffreview.DiffSourcePort = (*DiffSourcePortAdapter)(nil)

// Read 读取单个 diff 来源两侧版本内容（design.md D9 DiffSourcePort）。
// 实现为 GitDiff 公共入口的等价路径：阶段①词法校验 → tryLockTask → 阶段② task/worktree/repo 校验
// → gitDiffLocked（阶段③④⑤⑥ + UTF-8 规范化管线）。错误映射与 GitDiff 一致。
// 本阶段直接委托 Manager.GitDiff（公共入口已封装完整六阶段 + 加锁），后续 3.6 组装器
// 在已持锁上下文复用时可直接调 gitDiffLocked（见 design.md D7 末段）。
func (a *DiffSourcePortAdapter) Read(ctx context.Context, taskID string, src diffreview.DiffSource) (diffreview.DiffSourceResult, error) {
	dto, err := a.m.GitDiff(ctx, taskID, src.Ref, src.Path, src.Untracked)
	if err != nil {
		return diffreview.DiffSourceResult{}, err
	}
	return diffreview.DiffSourceResult{
		OldContent:   dto.OldContent,
		NewContent:   dto.NewContent,
		OldExists:    dto.OldExists,
		NewExists:    dto.NewExists,
		OldMode:      dto.OldMode,
		NewMode:      dto.NewMode,
		IsBinary:     dto.IsBinary,
		Truncated:    dto.Truncated,
		OldTruncated: dto.OldTruncated,
		NewTruncated: dto.NewTruncated,
	}, nil
}

// ReadLocked 在单个任务锁作用域内批量读取多个 diff 来源（F5/D7 组装全程持锁）。
// 阶段①词法校验（全部 srcs，锁前）→ tryLockTask → 阶段② task/worktree/repo 校验 →
// 逐来源调核心 helper gitDiffLocked（阶段③④⑤⑥ + UTF-8 规范化管线），回调内完成组装。
// 多源失败返回首个错误（D7：多来源同时失败返回排序最前错误，与批注展示顺序无关）。
// 禁止递归加锁：核心 helper gitDiffLocked 不加锁，仅本方法持锁一次。
func (a *DiffSourcePortAdapter) ReadLocked(ctx context.Context, taskID string, srcs []diffreview.DiffSource, fn diffreview.DiffReadCallback) error {
	// 阶段①：全部来源词法校验（锁前，与 GitDiff 公共入口同源）。
	for _, src := range srcs {
		if src.Untracked {
			if src.Path == "" {
				return newOpErr(codeInvalidInput, errors.New("untracked diff requires a path"))
			}
			if src.Ref != "" {
				return newOpErr(codeInvalidInput, errors.New("untracked diff does not accept a ref"))
			}
		}
		if err := git.ValidateDiffPath(src.Path); err != nil {
			return newOpErr(codeInvalidInput, err)
		}
	}

	unlock, err := a.m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	// 阶段②：task 存在性与 worktree/repo kind 校验。
	row, err := a.m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.WorktreePath == "" {
		return newOpErr(codeInvalidState, fmt.Errorf("task %s has no worktree", taskID))
	}
	if _, err := a.m.assertGitRepoTask(ctx, row); err != nil {
		return err
	}

	// 逐来源调核心 helper（已持锁，禁止递归加锁）。
	// F14/D7：来源读取失败不立即返回——错误交给回调汇总后继续遍历剩余来源
	//（D7：后续来源仍须读取以汇总 error/truncated）。仅回调自身返回错误才中止。
	for _, src := range srcs {
		dto, derr := a.m.gitDiffLocked(ctx, row, src.Ref, src.Path, src.Untracked)
		if derr != nil {
			// 读取失败：交给回调汇总（回调记录首个排序 error），继续下一来源。
			if cerr := fn(src, diffreview.DiffSourceResult{}, derr); cerr != nil {
				return cerr
			}
			continue
		}
		result := diffreview.DiffSourceResult{
			OldContent:   dto.OldContent,
			NewContent:   dto.NewContent,
			OldExists:    dto.OldExists,
			NewExists:    dto.NewExists,
			OldMode:      dto.OldMode,
			NewMode:      dto.NewMode,
			IsBinary:     dto.IsBinary,
			Truncated:    dto.Truncated,
			OldTruncated: dto.OldTruncated,
			NewTruncated: dto.NewTruncated,
		}
		if cerr := fn(src, result, nil); cerr != nil {
			return cerr
		}
	}
	return nil
}

// RuntimePortAdapter 实现 diffreview.RuntimePort（design.md D9，task 层）。
// 返回任务 runtime 快照：instVersion（runtimeRegistry tombstone）、锚定会话（TaskRow.AnchorSessionID）、
// 能力缓存（taskRuntime 内存态，本阶段 absent）、SessionStatus（OCClient.SessionStatus）。
type RuntimePortAdapter struct {
	m *Manager
}

// NewRuntimePortAdapter 构造 RuntimePort adapter。
func NewRuntimePortAdapter(m *Manager) *RuntimePortAdapter {
	return &RuntimePortAdapter{m: m}
}

// 编译期断言：*RuntimePortAdapter 实现 diffreview.RuntimePort（design.md D9 五口全覆盖）。
var _ diffreview.RuntimePort = (*RuntimePortAdapter)(nil)

// Snapshot 返回任务当前 runtime 快照（design.md D9 RuntimePort）。
// 无运行时（getRuntime nil）→ HasRuntime=false，能力为 absent；instVersion 取 registry tombstone
// （清理后保留，用于 fencing 比对）。锚定会话取 TaskRow.AnchorSessionID。
// 本阶段能力缓存为 absent（3.4 能力协调实现后由 taskRuntime 内存态填充）。
func (a *RuntimePortAdapter) Snapshot(ctx context.Context, taskID string) (diffreview.RuntimeSnapshot, error) {
	row, err := a.m.store.GetTask(ctx, taskID)
	if err != nil {
		return diffreview.RuntimeSnapshot{}, fmt.Errorf("diffreview runtime snapshot: get task %s: %w", taskID, err)
	}
	snap := diffreview.RuntimeSnapshot{
		HasRuntime:      false,
		CapabilityState: diffreview.CapabilityAbsent,
	}
	if rt := a.m.getRuntime(taskID); rt != nil {
		snap.HasRuntime = true
		snap.InstVersion = string(rt.instVersion)
	}
	if row.AnchorSessionID.Valid {
		snap.HasAnchorSession = true
		snap.AnchorSessionID = row.AnchorSessionID.String
	}
	return snap, nil
}

// SessionStatus 返回目标会话的 busy/idle/retry 状态（design.md D9 RuntimePort，调度器投递门禁用）。
// 经 taskOcClient 获取 client 后调 OCClient.SessionStatus；map 中无该 session → 视为 idle（可投递，
// design.md D2 调度器条："idle 或缺席可投递"）。adapter 获取失败 → error（调度器保持 queued 下轮重试）。
func (a *RuntimePortAdapter) SessionStatus(ctx context.Context, taskID, sessionID string) (diffreview.SessionStatus, error) {
	oc, dir, ok := a.m.taskOcClient(ctx, taskID)
	if !ok {
		return "", fmt.Errorf("diffreview session status: runtime client unavailable for task %s", taskID)
	}
	statuses, err := oc.SessionStatus(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("diffreview session status: %w", err)
	}
	st, present := statuses[sessionID]
	if !present {
		// 缺席视为 idle（可投递，design.md D2）。
		return diffreview.SessionStatusIdle, nil
	}
	return diffreview.SessionStatus(st.Type), nil
}