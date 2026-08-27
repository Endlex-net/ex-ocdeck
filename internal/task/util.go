package task

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"

	"ocdeck/internal/infrastructure/process"
)

// newTaskID 生成 16 字节随机 hex 任务 ID（与 project ID 同策略）。
func newTaskID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// normalizeSlug 将名称规范化为 slug：小写、非 [a-z0-9] 折叠为 -、去首尾 -。
// 允许返回空（调用方决定空兜底策略）。与 Slugify 共享同一规范化逻辑。
func normalizeSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := true // 首字符也禁止 -，等价于首字符前视为 -
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// Slugify 将名称转为 slug：normalizeSlug + 空兜底 "task"。
// 行为与历史 slugify 完全一致（分支名派生用）。
func Slugify(name string) string {
	out := normalizeSlug(name)
	if out == "" {
		return "task"
	}
	return out
}

// parsePort 解析端口字符串为 int 并校验 1..65535 范围。替代 fmt.Sscanf（后者不报错地
// 接受 "123abc"→123、空串→0 等非法输入）。返回 (port, ok)。
func parsePort(s string) (int, bool) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, false
	}
	return p, true
}

// newRandomPassword 生成 32 字节随机 hex 服务密码（每 serve 独立，design.md §2）。
func newRandomPassword() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sessionNameFor 构造会话名 ocdeck-<taskID>-<role>（design.md §2）。
func sessionNameFor(taskID, role string) string {
	return "ocdeck-" + taskID + "-" + role
}

// serveSessionName 返回 serve 会话名（legacy 双进程布局，Phase 2 前 Activate 仍使用）。
func serveSessionName(taskID string) string { return sessionNameFor(taskID, "serve") }

// tuiSessionName 返回 tui 会话名（legacy 双进程布局，Phase 2 前 Activate 仍使用）。
func tuiSessionName(taskID string) string { return sessionNameFor(taskID, "tui") }

// runtimeSessionName 返回单进程 runtime 会话名（design D2）。本阶段仅提供命名，Activate 不切换。
func runtimeSessionName(taskID string) string { return sessionNameFor(taskID, "runtime") }

// shellSessionName 返回第 n 个 shell 会话名。
func shellSessionName(taskID string, n int) string {
	return sessionNameFor(taskID, "shell-"+itoa(n))
}

// itoa 简单整数转字符串（避免引入 strconv 仅为此）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// roleFromSessionName 从会话名解析 role 后缀（canonical parser：process.ParseSessionName）。
// 运行期 ∈ {runtime, shell-<n>}（n>0），同时识别 legacy serve/tui。非法名返回空。
func roleFromSessionName(name string) string {
	_, suffix, err := process.ParseSessionName(name)
	if err != nil {
		return ""
	}
	return suffix
}

// taskIDFromSessionName 从 ocdeck-<taskID>-<role> 解析 taskID（canonical parser）。
func taskIDFromSessionName(name string) string {
	taskID, _, err := process.ParseSessionName(name)
	if err != nil {
		return ""
	}
	return taskID
}