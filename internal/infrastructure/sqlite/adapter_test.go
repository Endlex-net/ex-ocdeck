// adapter_test.go 验证 Adapter 实现 application ports 全部方法（编译期接口闭合，P1.2）。
package sqlite

import (
	"testing"

	"ocdeck/internal/application"
)

// TestAdapter_ImplementsApplicationPorts 确保 Adapter 满足所有 application ports 接口。
// 编译期断言：方法缺失或签名不匹配即编译失败。
func TestAdapter_ImplementsApplicationPorts(t *testing.T) {
	var _ application.TaskRepository = (*Adapter)(nil)
	var _ application.SessionRepository = (*Adapter)(nil)
	var _ application.ProjectReader = (*Adapter)(nil)
	var _ application.EnvReader = (*Adapter)(nil)
	var _ application.CleanupDebtRepository = (*Adapter)(nil)
	var _ application.ProcessPort = (*Adapter)(nil)
	var _ application.OpenCodePort = (*Adapter)(nil)
	var _ application.WorktreePort = (*Adapter)(nil)
}