// runtime_cmd.go 承载 runtime 进程命令与 kill 结果分类的 provider 单点原语
// （opencode-v1-spi-provider C2a / design D5/D9）：task 层与 v1 适配器
// （opencode v1 适配器与 task 层）共用同一实现，消除平行重复。
//
// 分层说明：本包（application）不得依赖 infrastructure——kill 分类的输入
// 以原语值（disposition 字符串 + SessionKilled bool）传入，infrastructure
// 类型（process.KillResult）由各适配层拆取后传入（disposition 底层类型为
// string，逐字透传零转换损失）。
package agentprovider

import "strconv"

// RuntimeCmdArgv 构造单进程启动 argv：`opencode --port <p> --hostname 127.0.0.1`，
// 有锚定时追加 `--session <id>`（D1/D5）。密码经 env 注入，禁止进 argv。
// permissionMode 为已解析的有效权限模式：all-approve 追加 `--auto`（未被显式
// deny 的权限请求自动批准）；ask/ai-auto 与现状逐字一致。调用方 MUST 先经
// 权限解析（未知值 fail-closed 拒绝启动）。
func RuntimeCmdArgv(port int, sessionID, permissionMode string) []string {
	argv := []string{"opencode", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"}
	if sessionID != "" {
		argv = append(argv, "--session", sessionID)
	}
	if permissionMode == PermissionModeAllApprove {
		argv = append(argv, "--auto")
	}
	return argv
}

// PermissionModeAllApprove 是全部权限模式的 --auto 判定输入（与 task 层常量同值）。
const PermissionModeAllApprove = "all-approve"

// PermissionModeAsk 是 ask 权限模式值（与 task 层常量同值；临时 runtime
// AcquireEphemeral 的硬编码清理语义经本常量引用，不写字面量）。
const PermissionModeAsk = "ask"

// Kill notice reason 常量（task 层 notice.go 现状值；PendingCleanupError.Reason）。
const (
	KillReasonKillFailed      = "kill_failed"
	KillReasonReapFailed      = "reap_failed"
	KillReasonSnapshotFailed  = "snapshot_failed"
	KillReasonSnapshotMissing = "snapshot_missing_degraded"
)

// 清理 disposition 的稳定字符串值（与 infrastructure/process.CleanupDisposition
// 逐字同值；此处独立声明，避免 application → infrastructure 依赖）。
const (
	DispositionClean                   = "clean"
	DispositionSnapshotFailed          = "retryable_snapshot_failed"
	DispositionKillFailed              = "retryable_kill_failed"
	DispositionReapFailed              = "retryable_reap_failed"
	DispositionSnapshotMissingDegraded = "snapshot_missing_degraded"
)

// KillNoticeClass 是 kill 结果的五值映射分类（dispositionToNotice 表的应用形态）。
type KillNoticeClass struct {
	Action    string // none（clean，无 notice）| continue | terminal
	Reason    string
	Retryable bool
}

// ClassifyKill 按（disposition, sessionKilled）分类。
// 未知 disposition 或 disposition 与 SessionKilled 矛盾 → kill_failed /
// retryable / terminal（fail-closed，MUST NOT 静默忽略未知值）。
func ClassifyKill(disposition string, sessionKilled bool) KillNoticeClass {
	switch disposition {
	case DispositionClean:
		if !sessionKilled {
			return KillNoticeClass{Action: "terminal", Reason: KillReasonKillFailed, Retryable: true}
		}
		return KillNoticeClass{Action: "none"}
	case DispositionReapFailed:
		if !sessionKilled {
			return KillNoticeClass{Action: "terminal", Reason: KillReasonKillFailed, Retryable: true}
		}
		return KillNoticeClass{Action: "terminal", Reason: KillReasonReapFailed, Retryable: true}
	case DispositionSnapshotFailed:
		if sessionKilled {
			return KillNoticeClass{Action: "terminal", Reason: KillReasonKillFailed, Retryable: true}
		}
		return KillNoticeClass{Action: "terminal", Reason: KillReasonSnapshotFailed, Retryable: true}
	case DispositionKillFailed:
		if sessionKilled {
			return KillNoticeClass{Action: "terminal", Reason: KillReasonKillFailed, Retryable: true}
		}
		return KillNoticeClass{Action: "terminal", Reason: KillReasonKillFailed, Retryable: true}
	case DispositionSnapshotMissingDegraded:
		if sessionKilled {
			return KillNoticeClass{Action: "terminal", Reason: KillReasonKillFailed, Retryable: true}
		}
		return KillNoticeClass{Action: "continue", Reason: KillReasonSnapshotMissing, Retryable: false}
	default:
		return KillNoticeClass{Action: "terminal", Reason: KillReasonKillFailed, Retryable: true}
	}
}
