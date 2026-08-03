package task

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
)

// newTaskID 生成 16 字节随机 hex 任务 ID（与 project ID 同策略）。
func newTaskID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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

// serveSessionName 返回 serve 会话名。
func serveSessionName(taskID string) string { return sessionNameFor(taskID, "serve") }

// tuiSessionName 返回 tui 会话名。
func tuiSessionName(taskID string) string { return sessionNameFor(taskID, "tui") }

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

// roleFromSessionName 从会话名解析 role（serve/tui/shell-<n>）。
// 会话名格式 ocdeck-<taskID>-<role>（§2），role ∈ {serve, tui, shell-<n>}。
// 按已知 role 后缀匹配，避免用 lastIndex('-') 把 shell-<n> 误拆（B9）。
func roleFromSessionName(name string) string {
	rest, ok := stripOcdeckPrefix(name)
	if !ok {
		return ""
	}
	// 优先匹配多段 role：shell-<n>、serve、tui（按后缀匹配）。
	switch {
	case strings.HasSuffix(rest, "-serve"):
		return "serve"
	case strings.HasSuffix(rest, "-tui"):
		return "tui"
	case strings.Contains(rest, "-shell-"):
		return "shell-" + rest[strings.Index(rest, "-shell-")+len("-shell-"):]
	}
	return ""
}

// taskIDFromSessionName 从 ocdeck-<taskID>-<role> 解析 taskID。
// 按已知 role 后缀截取前缀部分，避免 lastIndex('-') 对 shell-1 解析错误（B9）。
func taskIDFromSessionName(name string) string {
	rest, ok := stripOcdeckPrefix(name)
	if !ok {
		return ""
	}
	switch {
	case strings.HasSuffix(rest, "-serve"):
		return rest[:len(rest)-len("-serve")]
	case strings.HasSuffix(rest, "-tui"):
		return rest[:len(rest)-len("-tui")]
	case strings.Contains(rest, "-shell-"):
		return rest[:strings.Index(rest, "-shell-")]
	}
	return ""
}

// stripOcdeckPrefix 去除 ocdeck- 前缀，返回 (rest, ok)。
func stripOcdeckPrefix(name string) (string, bool) {
	const prefix = "ocdeck-"
	if !strings.HasPrefix(name, prefix) || len(name) <= len(prefix) {
		return "", false
	}
	return name[len(prefix):], true
}