package task

// migration_legacy_serve_test.go 阶段 4 残余修复（Section 7 评审 F2）：迁移导入
// runner 兼容旧 v1 双进程布局——来源标记/密码/端口探测优先 -runtime 会话，不存在
// 则探测 legacy -serve 会话（delete.go legacy 分支证明该布局升级时可达）；实际
// 会话名用于 marker/port/password 读取及导入 binding 的 TerminalRef。
//
// mutation 证据（红→还原，布局探测删除）：探测块 `if false &&` 旁路（恒用
// -runtime 名）→ legacy 布局 fixture 红（-runtime 无会话 → 标记读取失败 → 导入
// 失败）→ 还原。
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"ocdeck/internal/infrastructure/process"
)

// runtimeProbeErrProc 按会话名注入 HasSession 基础设施错误（F6 fixture：模拟
// tmux 部分故障——runtime 探测失败而 serve 探测正常）。
type runtimeProbeErrProc struct {
	ProcessBackend
	errFor map[string]error
}

func (p *runtimeProbeErrProc) HasSession(name string) (bool, error) {
	if e, ok := p.errFor[name]; ok {
		return false, e
	}
	return p.ProcessBackend.HasSession(name)
}

// seedLegacyLayoutRuntime 构造 legacy 布局 runtime 会话（旧 v1 双进程形态：
// 密码/端口在 -serve 会话 env，无 OCDECK_CRED_KIND 标记；-runtime 会话可选择性
// 缺席/共存——runtimeAlive 控制新布局会话是否在场）。
func seedLegacyLayoutRuntime(e *charxEnv, taskID string, runtimeAlive bool) {
	e.store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
	e.store.tasks[taskID] = TaskRow{ID: taskID, ProjectID: "p1", Name: "t", Status: StatusActive,
		WorktreePath:    "/wt-" + taskID,
		AnchorSessionID: sql.NullString{String: "ses-" + taskID, Valid: true}}
	e.srv.seedSession("/wt-"+taskID, "ses-"+taskID, 1, 1)
	legacyEnv := map[string]string{
		"OPENCODE_SERVER_PASSWORD": "legacy-serve-layout-pw",
		"OCDECK_SERVE_PORT":        fmt.Sprintf("%d", e.portHealthy()),
	}
	e.proc.mockProc.mu.Lock()
	if runtimeAlive {
		rn := runtimeSessionName(taskID)
		e.proc.mockProc.sessions[rn] = true
		e.proc.mockProc.envValues[rn] = legacyEnv
	}
	sn := serveSessionName(taskID)
	e.proc.mockProc.sessions[sn] = true
	e.proc.mockProc.envValues[sn] = legacyEnv
	e.proc.mockProc.mu.Unlock()
}

func TestMigrationImport_LegacyServeLayout(t *testing.T) {
	ctx := context.Background()

	// ① legacy -serve 布局（-runtime 缺席）：导入成功且 TerminalRef 保留实际
	// legacy 会话名（binding 列唯一存储位置）。
	e1 := newCharxEnv(t, charxEnvOpts{})
	seedLegacyLayoutRuntime(e1, "mi-serve-layout", false)
	if err := e1.m.RunMigrationImport(ctx); err != nil {
		t.Fatalf("RunMigrationImport: %v", err)
	}
	raw, ok := e1.store.bindingRawOf("mi-serve-layout")
	if !ok {
		t.Fatal("legacy -serve 布局导入未写入 binding（探测失效）")
	}
	var envP struct {
		TerminalRef string          `json:"terminal_ref"`
		Data        json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &envP); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envP.TerminalRef != serveSessionName("mi-serve-layout") {
		t.Fatalf("TerminalRef = %q, want legacy -serve 会话名（实际探测布局）", envP.TerminalRef)
	}
	if !strings.Contains(string(envP.Data), "legacy-serve-layout-pw") {
		t.Fatalf("legacy 密码未导入: %s", envP.Data)
	}

	// ② 新布局优先级：-runtime 与 -serve 同在 → 用 -runtime。
	e2 := newCharxEnv(t, charxEnvOpts{})
	seedLegacyLayoutRuntime(e2, "mi-both-layouts", true)
	if err := e2.m.RunMigrationImport(ctx); err != nil {
		t.Fatalf("RunMigrationImport (both): %v", err)
	}
	raw2, ok2 := e2.store.bindingRawOf("mi-both-layouts")
	if !ok2 {
		t.Fatal("-runtime/-serve 共存导入未写入 binding")
	}
	var envP2 struct {
		TerminalRef string `json:"terminal_ref"`
	}
	if err := json.Unmarshal([]byte(raw2), &envP2); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envP2.TerminalRef != runtimeSessionName("mi-both-layouts") {
		t.Fatalf("TerminalRef = %q, want -runtime（新布局优先级）", envP2.TerminalRef)
	}
}

// TestMigrationImport_RuntimeProbeInfraFailClosed F6：runtime HasSession 基础设施
// 错误（非 ErrNoTmuxServer）MUST fail-closed——不得当作「不存在」滑入 -serve 回退
// （tmux 部分故障下会错误导入仍存活 legacy -serve 的凭据/TerminalRef）。断言：
// 不写 binding、单任务 last_error 落账（runner 整体继续）、serve env 密码零泄漏。
//
// mutation 证据（红→还原，fail-closed 旁路）：错误分支删除（错误与「不存在」合并
// 处理——pre-F6 形态）→ serve 存活被探测回退 → binding 写入（TerminalRef=-serve、
// 密码落库）→ 本测试红 → 还原。
func TestMigrationImport_RuntimeProbeInfraFailClosed(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	seedLegacyLayoutRuntime(e, "mi-probe-infra", false)
	// tmux 部分故障：仅 -runtime 探测失败，-serve 探测正常（真实部分故障形态）。
	probeName := runtimeSessionName("mi-probe-infra")
	e.m.proc = &runtimeProbeErrProc{
		ProcessBackend: e.proc,
		errFor:         map[string]error{probeName: errors.New("tmux show-sessions partial failure")},
	}

	if err := e.m.RunMigrationImport(context.Background()); err != nil {
		t.Fatalf("RunMigrationImport: %v（单任务失败不中断 runner 整体）", err)
	}
	if _, ok := e.store.bindingRawOf("mi-probe-infra"); ok {
		t.Fatal("探测基础设施错误 MUST NOT 滑入 -serve 回退写入 binding（fail-closed 失守）")
	}
	le := e.store.tasks["mi-probe-infra"].LastError
	if !le.Valid || !strings.Contains(le.String, "runtime 会话探测失败") {
		t.Fatalf("last_error = %+v, want 探测失败结论落账", le)
	}
	// serve env 密码零泄漏（binding 是唯一可能的导入位置——不存在即零泄漏）。
	for taskID, raw := range e.store.bindings {
		if strings.Contains(raw, "legacy-serve-layout-pw") {
			t.Errorf("bindings(%s) 出现 serve env 密码", taskID)
		}
	}
}

// TestMigrationImport_NoTmuxServerFallsBackToServe F6 ErrNoTmuxServer 语义：按
// process 层既有处理视为「无会话」→ 允许探测并回退 legacy -serve（与 supervisor
// releaseBinding 的 errors.Is(herr, process.ErrNoTmuxServer) 处理一致）。
func TestMigrationImport_NoTmuxServerFallsBackToServe(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	seedLegacyLayoutRuntime(e, "mi-notmux-fallback", false)
	probeName := runtimeSessionName("mi-notmux-fallback")
	e.m.proc = &runtimeProbeErrProc{
		ProcessBackend: e.proc,
		errFor:         map[string]error{probeName: process.ErrNoTmuxServer},
	}

	if err := e.m.RunMigrationImport(context.Background()); err != nil {
		t.Fatalf("RunMigrationImport: %v", err)
	}
	raw, ok := e.store.bindingRawOf("mi-notmux-fallback")
	if !ok {
		t.Fatal("ErrNoTmuxServer 视为无会话 → 应回退 -serve 导入成功")
	}
	var envP struct {
		TerminalRef string `json:"terminal_ref"`
	}
	if err := json.Unmarshal([]byte(raw), &envP); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envP.TerminalRef != serveSessionName("mi-notmux-fallback") {
		t.Fatalf("TerminalRef = %q, want legacy -serve 会话名", envP.TerminalRef)
	}
}
