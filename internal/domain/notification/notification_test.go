package notification

import (
	"context"
	"testing"
)

// fakeChannel 编译期钉住 Channel 接口形状（design D4：Name/Caps/Send 签名）。
type fakeChannel struct{}

func (fakeChannel) Name() string     { return "fake" }
func (fakeChannel) Caps() Capability { return CapGroup | CapReplace }
func (fakeChannel) Send(context.Context, Intent, ChannelConfig) Result {
	return Result{OK: true}
}

var _ Channel = fakeChannel{}

// TestEnumValues 钉住 Category/Level 枚举字面量（SSE 契约取值，spec「网页通知渠道」
// JSON 形状的唯一表述）。
func TestEnumValues(t *testing.T) {
	cats := map[Category]string{
		CategoryQuestion:   "question",
		CategoryPermission: "permission",
		CategoryIdle:       "idle",
		CategoryRetry:      "retry",
		CategoryError:      "error",
		CategoryTest:       "test",
	}
	for got, want := range cats {
		if string(got) != want {
			t.Errorf("category literal = %q, want %q", got, want)
		}
	}
	levels := map[Level]string{
		LevelPassive:       "passive",
		LevelActive:        "active",
		LevelTimeSensitive: "timeSensitive",
		LevelCritical:      "critical",
	}
	for got, want := range levels {
		if string(got) != want {
			t.Errorf("level literal = %q, want %q", got, want)
		}
	}
}

// TestCapabilityBitmask 验证能力位为独立位掩码（可组合判定）；零值表达无能力
// （spec 能力矩阵：osascript Caps=0，标题加任务名前缀降级）。
func TestCapabilityBitmask(t *testing.T) {
	if CapGroup <= 0 || CapReplace <= 0 || CapWithdraw <= 0 {
		t.Fatalf("capability bits must be positive: %d %d %d", CapGroup, CapReplace, CapWithdraw)
	}
	if (CapGroup&CapReplace) != 0 || (CapGroup&CapWithdraw) != 0 || (CapReplace&CapWithdraw) != 0 {
		t.Fatalf("capability bits must be independent: %d %d %d", CapGroup, CapReplace, CapWithdraw)
	}
	var none Capability
	if none&CapGroup != 0 || none&CapReplace != 0 || none&CapWithdraw != 0 {
		t.Fatal("zero Capability must express no capability")
	}
	// 组合与位判定（能力矩阵用法：terminal-notifier Caps=Group|Replace）。
	combined := CapGroup | CapReplace
	if combined&CapGroup == 0 || combined&CapReplace == 0 || combined&CapWithdraw != 0 {
		t.Fatalf("combined caps bit test mismatch: %v", combined)
	}
}
