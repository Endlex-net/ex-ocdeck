// macOS 本地通知渠道适配器（spec「macOS 本地通知渠道」/design D10）。
//
// 双实现选择：terminal-notifier 在场 → 用之（Caps=Group|Replace，执行失败 MUST NOT
// 降级 osascript）；仅未安装时才 osascript（Caps=0，固定 on run argv 脚本模板经
// argv 传 title/body，文案不进脚本字符串杜绝转义注入）。两实现均 argv 直传无
// shell、10s 硬超时、输出读取上限 4KB。启动构造时 LookPath 探测并缓存结果；
// 仅 darwin 且至少一个可用才算可用（skipped 语义由上层判定）。渠道只持静态
// 依赖，不读配置 Store。
package notify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"ocdeck/internal/domain/notification"
)

// macOS 渠道进程约束常量（spec「macOS 本地通知渠道」）。
const (
	macosCommandTimeout = 10 * time.Second
	macosMaxOutputBytes = 4 * 1024
)

// osascriptNotifyScript 固定 AppleScript 模板：on run argv 形式从 argv 读
// title（item 1）/ body（item 2）。
const osascriptNotifyScript = `on run argv
	display notification (item 2 of argv) with title (item 1 of argv)
end run
`

// lookPathFunc 二进制探测函数，默认 exec.LookPath（测试注入先例：
// internal/config Options.BinaryProbe）。
type lookPathFunc func(name string) (string, error)

// commandRunner 外部命令执行函数：argv 直传（无 shell）、对进程实施 timeout
// 硬超时、合并 stdout/stderr 且读取上限 macosMaxOutputBytes。
type commandRunner func(ctx context.Context, timeout time.Duration, name string, args ...string) (output string, err error)

// MacosChannel macOS 本地通知渠道。
type MacosChannel struct {
	goos          string
	run           commandRunner
	notifierPath  string // terminal-notifier 探测缓存；空为未安装
	osascriptPath string // osascript 探测缓存；空为未安装
}

// NewMacosChannel 构造并缓存探测结果（goos 由组合根注入 runtime.GOOS，
// design D11）。
func NewMacosChannel(goos string) *MacosChannel {
	return newMacosChannel(goos, exec.LookPath, runCommand)
}

func newMacosChannel(goos string, lookPath lookPathFunc, run commandRunner) *MacosChannel {
	c := &MacosChannel{goos: goos, run: run}
	if p, err := lookPath("terminal-notifier"); err == nil {
		c.notifierPath = p
	}
	if p, err := lookPath("osascript"); err == nil {
		c.osascriptPath = p
	}
	return c
}

func (c *MacosChannel) Name() string { return "macos" }

// Caps 能力位随探测到的实现变化：terminal-notifier=Group|Replace、osascript=0
// （spec「通知渠道投递与降级」矩阵；不可用时无投递，取 0）。
func (c *MacosChannel) Caps() notification.Capability {
	if c.notifierPath != "" {
		return notification.CapGroup | notification.CapReplace
	}
	return 0
}

// Available 渠道可用性：仅 darwin 且 terminal-notifier 或 osascript 至少一个
// 在场。上层（dispatch/测试通知）据此判定 skipped。
func (c *MacosChannel) Available() bool {
	return c.goos == "darwin" && (c.notifierPath != "" || c.osascriptPath != "")
}

// Send 按探测缓存选择实现投递。不可用时直接失败（正常流程上层已判 skipped）。
func (c *MacosChannel) Send(ctx context.Context, in notification.Intent, _ notification.ChannelConfig) notification.Result {
	if !c.Available() {
		return notification.Result{OK: false, Err: "macos: channel unavailable (non-darwin or no notifier installed)"}
	}
	if c.notifierPath != "" {
		out, err := c.run(ctx, macosCommandTimeout, c.notifierPath,
			"-group", in.TaskID,
			"-title", in.Title,
			"-message", in.Body,
			"-open", in.URL,
			"-sound", "default",
		)
		if err != nil {
			return notification.Result{OK: false, Err: commandErrMsg("terminal-notifier", out, err)}
		}
		return notification.Result{OK: true}
	}
	out, err := c.run(ctx, macosCommandTimeout, c.osascriptPath,
		"-e", osascriptNotifyScript,
		in.Title,
		in.Body,
	)
	if err != nil {
		return notification.Result{OK: false, Err: commandErrMsg("osascript", out, err)}
	}
	return notification.Result{OK: true}
}

// commandErrMsg 组装执行失败摘要：命令名 + 错误 + 截断输出片段（本地工具输出
// 非敏感，片段辅助诊断）。
func commandErrMsg(name, output string, err error) string {
	msg := fmt.Sprintf("macos: %s failed: %v", name, err)
	if snippet := strings.TrimSpace(output); snippet != "" {
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		msg += ": " + snippet
	}
	return msg
}

// runCommand 生产 commandRunner：exec.CommandContext argv 直传，timeout 到期
// 进程被杀；stdout/stderr 合并写入 cappedBuffer（保留前 4KB，超出丢弃但持续
// 排空，避免子进程写管道阻塞）。
func runCommand(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out := &cappedBuffer{limit: macosMaxOutputBytes}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	return out.String(), err
}

// cappedBuffer 保留前 limit 字节的 io.Writer：超出部分丢弃但报告已全部消费，
// 持续排空子进程管道。
type cappedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if remain := b.limit - b.buf.Len(); remain > 0 {
		b.buf.Write(p[:min(remain, len(p))])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
