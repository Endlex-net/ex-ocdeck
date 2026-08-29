package api

import (
	"os/exec"
	"strings"
	"testing"
)

// goListOutput 执行 go list 并按行返回输出（架构断言共用 helper）。
func goListOutput(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", strings.Join(args, " "), err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// depMatches 判断 go list -deps 输出行是否为 target 包。-test 会被测包追加
// " [pkg.test]" 装饰后缀，按精确相等或路径前缀 + 空格识别。
func depMatches(pkg, target string) bool {
	return pkg == target || strings.HasPrefix(pkg, target+" ")
}

// stripTestDecoration 解析 go list -f '{{if not .Standard}}{{.ImportPath}}{{end}}'
// 的单行输出：跳过空行（stdlib 包模板输出为空），剥离 -test 装饰
// （"pkg [pkg.test]"，取首个空格前路径）与合成测试主二进制后缀（"pkg.test"），
// 返回底层包路径。第三方路径（github.com/... 等）原样保留，由调用方判违规。
func stripTestDecoration(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if i := strings.IndexByte(line, ' '); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSuffix(line, ".test")
}

// TestStripTestDecoration 用伪造行列表单测装饰剥离逻辑（TestDomainStdlibOnly
// 敏感性的一部分：第三方依赖行不得被误归为 domain 自身）。
func TestStripTestDecoration(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"", ""},
		{"ocdeck/internal/domain/task", "ocdeck/internal/domain/task"},
		{"ocdeck/internal/domain/task [ocdeck/internal/domain/task.test]", "ocdeck/internal/domain/task"},
		{"ocdeck/internal/domain/task.test", "ocdeck/internal/domain/task"},
		// 第三方依赖：必须原样存活（非 domain → 违规）。
		{"github.com/samber/lo", "github.com/samber/lo"},
		// 非 domain 的 ocdeck 包同样必须存活。
		{"ocdeck/internal/application", "ocdeck/internal/application"},
	}
	for _, c := range cases {
		if got := stripTestDecoration(c.line); got != c.want {
			t.Errorf("stripTestDecoration(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

// TestNoLegacyTaskImport 断言 internal/api（含测试二进制）不再依赖 legacy internal/task
// 包（sse-active-sessions P1.9a；design.md D0:55 import 方向 api → application → domain）。
// 注意 ocdeck/internal/application/task 是不同的包，不受本断言限制。
// 用 `go list -deps -test` 而非源码 grep：覆盖间接依赖（经别名/接口泄漏的类型引用
// 不会重新拉入包，但任何真实 import 都会出现在依赖闭包里），且对未来文件新增免疫。
func TestNoLegacyTaskImport(t *testing.T) {
	for _, pkg := range goListOutput(t, "-deps", "-test", "ocdeck/internal/api") {
		if depMatches(pkg, "ocdeck/internal/task") {
			t.Fatalf("internal/api must not depend on ocdeck/internal/task (design.md D0:55, sse-active-sessions P1.9a)")
		}
	}
}

// TestDomainStdlibOnly 断言 internal/domain/... 依赖闭包（含测试二进制）除 domain
// 自身包外不含任何非 stdlib 包（design.md D0:55：domain 位于依赖方向最底层，
// stdlib-only，不得引入 application/infrastructure/store/api 或第三方库）。
// 用 .Standard 模板过滤而非 ocdeck/ 前缀判断：第三方 import（github.com/...）
// 同样是违规，闭包中每个非 stdlib 行都必须可归类为 domain 包自身。
func TestDomainStdlibOnly(t *testing.T) {
	domains := goListOutput(t, "ocdeck/internal/domain/...")
	domainSet := make(map[string]bool, len(domains))
	for _, d := range domains {
		domainSet[d] = true
	}
	for _, line := range goListOutput(t, "-deps", "-test",
		"-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "ocdeck/internal/domain/...") {
		pkg := stripTestDecoration(line)
		if pkg != "" && !domainSet[pkg] {
			t.Errorf("internal/domain must stay stdlib-only: non-stdlib dependency %q (design.md D0:55)", pkg)
		}
	}
}

// TestInfrastructureNoAPIDependency 断言 internal/infrastructure/...（含 store，
// 含测试二进制）不依赖 ocdeck/internal/api：api 是边界适配层，只能位于依赖方向
// 末端（design.md D0:55 api → application → domain），下层包反向 import api 会
// 制造隐式依赖环。
func TestInfrastructureNoAPIDependency(t *testing.T) {
	for _, pkg := range goListOutput(t, "-deps", "-test", "ocdeck/internal/infrastructure/...") {
		if depMatches(pkg, "ocdeck/internal/api") {
			t.Fatalf("internal/infrastructure must not depend on ocdeck/internal/api (design.md D0:55)")
		}
	}
}

// TestAppNotificationNoInfrastructure 断言 internal/application/notification
// 的直接 import 不含 ocdeck/internal/infrastructure 包（task-notifications
// design D1：任务侧与 bus 依赖全部经窄端口注入，组合根适配——application/
// notification 不 import infrastructure 具体类型，Lane D 4.4）。断言用直接
// import 而非传递闭包：Attention 等共享 DTO 在 ocdeck/internal/application
// （合法上游）中内嵌 opencode 请求类型，为既有架构接受的间接依赖。
func TestAppNotificationNoInfrastructure(t *testing.T) {
	for _, line := range goListOutput(t, "-f",
		`{{range .Imports}}{{.}}{{"\n"}}{{end}}`,
		"ocdeck/internal/application/notification") {
		pkg := stripTestDecoration(line)
		if pkg != "" && strings.HasPrefix(pkg, "ocdeck/internal/infrastructure") {
			t.Fatalf("internal/application/notification must not import %q (task-notifications D1: narrow ports only)", pkg)
		}
	}
}
