// terminal.go 实现 TerminalIO 端口（opencode-v2-provider tasks 4.4，design D3
// TerminalIO 契约）：EnsureTerminal = REST 预验证（GET /api/session/{id}：存在
// 且归属本任务——location.directory 必须等于 binding directory）+ 返回托管单元
// 引用；不存在 → NotFound、归属不符 → BindingInvalid，不创建替代 session。
// AttachCommand = 交互 TUI attach argv 且自动选中 binding 的原 session（机制
// 已冻结 D8-7：`opencode --server <url> --session <id>`；认证凭据经 task 层以
// OPENCODE_SERVER_PASSWORD env 注入，不入 argv）。机制失效时明确失败（构造
// 阶段显式失败），MUST NOT 退化为手动定位。
package opencode2

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

type spiTerminal struct{ p *SPIProvider }

// EnsureTerminal 预验证绑定 session 存在且归属本任务（session 的
// location.directory == binding directory——v2 Session.Info 显式携带目录绑定）。
func (t spiTerminal) EnsureTerminal(ctx context.Context, b domain.TaskBinding) (string, error) {
	h := t.p.healthBase(b, appagent.PrimitiveTerminalIO, appagent.ScopeRuntime, nil)
	c, data, err := t.p.resolveBinding(b)
	if err != nil {
		t.p.reportConversion(h, "terminal.ensure", err)
		return "", err
	}
	if b.TerminalRef == "" {
		err := appagent.NewError("terminal.ensure", appagent.ErrKindBindingInvalid,
			errors.New("binding TerminalRef 为空"))
		t.p.reportConversion(h, "terminal.ensure", err)
		return "", err
	}
	if b.UpstreamSessionID == "" {
		err := appagent.NewError("terminal.ensure", appagent.ErrKindBindingInvalid,
			errors.New("binding 缺少 upstream session id"))
		t.p.reportConversion(h, "terminal.ensure", err)
		return "", err
	}
	sess, gerr := c.GetSession(ctx, b.UpstreamSessionID)
	if gerr != nil {
		gerr = classifyHTTPError("terminal.ensure", gerr)
		t.p.reportConversion(h, "terminal.ensure", gerr)
		return "", gerr
	}
	if sess.Location.Directory != data.Directory {
		err := appagent.NewError("terminal.ensure", appagent.ErrKindBindingInvalid,
			fmt.Errorf("session %s 归属不符：location.directory=%q 与 binding directory=%q 不一致",
				b.UpstreamSessionID, sess.Location.Directory, data.Directory))
		t.p.reportConversion(h, "terminal.ensure", err)
		return "", err
	}
	t.p.reportConversion(h, "terminal.ensure", nil)
	return b.TerminalRef, nil
}

// AttachCommand 产出交互 TUI attach argv（自动选中机制已冻结：--server +
// --session；session 存在性由 EnsureTerminal 预验证承载，本方法不重复探测）。
func (t spiTerminal) AttachCommand(ctx context.Context, b domain.TaskBinding) ([]string, error) {
	h := t.p.healthBase(b, appagent.PrimitiveTerminalIO, appagent.ScopeRuntime, nil)
	_, data, err := t.p.resolveBinding(b)
	if err != nil {
		t.p.reportConversion(h, "terminal.attach", err)
		return nil, err
	}
	if b.UpstreamSessionID == "" {
		err := appagent.NewError("terminal.attach", appagent.ErrKindBindingInvalid,
			errors.New("binding 缺少 upstream session id（自动选中机制失效，MUST NOT 退化手动定位）"))
		t.p.reportConversion(h, "terminal.attach", err)
		return nil, err
	}
	argv := []string{
		t.p.opencodeBin,
		"--server", "http://127.0.0.1:" + strconv.Itoa(data.Port),
		"--session", b.UpstreamSessionID,
	}
	t.p.reportConversion(h, "terminal.attach", nil)
	return argv, nil
}
