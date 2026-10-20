// debug.go 承载 task 层 [debug] 诊断日志（OCDECK_DEBUG 生产装配注入；默认关，
// 判断即返回零开销）。脱敏硬约束：凭据（OPENCODE_SERVER_PASSWORD/派生凭据/
// Authorization）、env 快照内容永不进入 debug 日志——只输出分派形态、决策码、
// 端口等非敏感事实。
package task

import (
	"log"
	"sync/atomic"
)

var debugLogEnabled atomic.Bool

// SetDebugLogging 开启/关闭 task 层 debug 日志（生产装配在启动期按 config 注入；
// MUST 先于任何 Manager 消费点生效）。
func SetDebugLogging(on bool) { debugLogEnabled.Store(on) }

// debugEnabled 供装配点决定是否安装带开销的诊断形态（如传输层 wrapper）。
func debugEnabled() bool { return debugLogEnabled.Load() }

func debugf(format string, args ...any) {
	if debugLogEnabled.Load() {
		log.Printf("[debug] task: "+format, args...)
	}
}
