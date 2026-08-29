// llm_test.go LLM 停止原因总结（Lane E，task-notifications design D9 + spec
// 「LLM 停止原因总结（可选增强）」）：DispatchPlan 固化开关、5s 预算上界、
// 失败/超时/未配置/空白/超长输出确定性降级、LLM 阻塞期间并发 PUT 不影响在途。
package notification

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/domain/notification"
)

// fakeCompleter 记录型 LLM 补全 fake（可脚本化结果/错误/阻塞到 ctx 完成；
// entered 为 Complete 入口一次性信号——确定性同步点，消除 Sleep 轮询）。
type fakeCompleter struct {
	mu        sync.Mutex
	calls     int
	systems   []string
	users     []string
	maxTokns  []int
	deadlines []time.Time
	entryAt   []time.Time
	result    string
	err       error
	block     chan struct{} // 非 nil 时阻塞直到关闭或 ctx.Done
	entered   chan struct{} // 非 nil 时首次进入 Complete 即关闭（测试同步点）
	enteredOn bool
}

func (f *fakeCompleter) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	f.mu.Lock()
	f.calls++
	f.systems = append(f.systems, system)
	f.users = append(f.users, user)
	f.maxTokns = append(f.maxTokns, maxTokens)
	f.entryAt = append(f.entryAt, time.Now())
	if dl, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, dl)
	}
	if f.entered != nil && !f.enteredOn {
		f.enteredOn = true
		close(f.entered)
	}
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, f.err
}

func (f *fakeCompleter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCompleter) lastDeadline() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deadlines[len(f.deadlines)-1]
}

func (f *fakeCompleter) lastEntryTime() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entryAt[len(f.entryAt)-1]
}

func (f *fakeCompleter) lastUser() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[len(f.users)-1]
}

func (f *fakeCompleter) lastMaxTokens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxTokns[len(f.maxTokns)-1]
}

// llmFixture 标准装置：bark 渠道 + fake completer + 50ms 注入预算（默认 5s 的
// 可测等价，超时分支不睡真实时间）。
func llmFixture(t *testing.T) (*Notifier, *fakeTasks, *fakeCfgStore, *fakeChannel, *fakeCompleter, *fakeClock) {
	t.Helper()
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
	comp := &fakeCompleter{result: "模型返回的总结"}
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	n.opts.Summarizer = comp
	n.opts.LLMBudget = 50 * time.Millisecond
	return n, ft, fc, ch, comp, clk
}

// llmOn 开启 llm_summary 的配置副本。
func llmOn() notification.Config {
	c := testConfig()
	c.LLMSummary = true
	return c
}

// TestLLM_PromptVerbatim 固定 prompt 模板逐字（design D9）：占位符替换、
// max_tokens 200、类别用人类可读名（复用 D4 各类别 Title）。
func TestLLM_PromptVerbatim(t *testing.T) {
	in := idleIntent(activeSnap("t1", "构建服务", "idle"), 60, "u")
	got := buildSummaryPrompt(in, summaryDetail(in))
	want := "你是通知摘要助手。根据以下任务运行信息，用一两句中文概括该任务停止或等待人工处理的原因，只基于给定信息，不要臆测。\n" +
		"任务：构建服务\n" +
		"类别：任务已空闲\n" +
		"详情：已空闲超过 60 秒"
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

// TestLLM_SummarySuccess 开关打开且调用成功：正文附带总结；prompt/max_tokens
// 传参正确；预算 ctx 生效范围内。
func TestLLM_SummarySuccess(t *testing.T) {
	n, _, fc, ch, comp, clk := llmFixture(t)
	fc.set(llmOn())
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if got := comp.callCount(); got != 1 {
		t.Fatalf("completer calls = %d, want 1", got)
	}
	if comp.lastMaxTokens() != 200 {
		t.Fatalf("max_tokens = %d, want 200", comp.lastMaxTokens())
	}
	sent := ch.sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if want := "构建服务\n已空闲超过 60 秒\nAI 总结：模型返回的总结"; sent[0].Body != want {
		t.Fatalf("body = %q, want %q", sent[0].Body, want)
	}
	if u := comp.lastUser(); !strings.Contains(u, "任务：构建服务") || !strings.Contains(u, "详情：已空闲超过 60 秒") {
		t.Fatalf("prompt missing inputs: %q", u)
	}
}

// TestLLM_DisabledNoCall 开关关闭（默认）：不发起任何 LLM 调用，正文仅确定性摘要。
func TestLLM_DisabledNoCall(t *testing.T) {
	n, _, _, ch, comp, clk := llmFixture(t) // testConfig 默认 llm_summary=false
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if got := comp.callCount(); got != 0 {
		t.Fatalf("llm_summary off must not call completer, calls = %d", got)
	}
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Body != "构建服务\n已空闲超过 60 秒" {
		t.Fatalf("body = %+v", sent)
	}
}

// TestLLM_FallbackBranches 失败/超时/空白/超长/未装配：确定性摘要照常投递
// （spec：MUST NOT 因此失败或丢失通知）；超长边界 300 字符内接受。
func TestLLM_FallbackBranches(t *testing.T) {
	exact300 := strings.Repeat("摘", 300)
	over300 := strings.Repeat("摘", 301)
	cases := []struct {
		name    string
		comp    *fakeCompleter
		noInst  bool
		wantApp bool
	}{
		{name: "call error", comp: &fakeCompleter{err: errNotFound}},
		// timeout：block 永不关闭 → 只能被预算 ctx 解除（50ms 注入预算）。
		{name: "timeout", comp: &fakeCompleter{block: make(chan struct{})}},
		{name: "blank output", comp: &fakeCompleter{result: "   \n\t"}},
		{name: "over 300 chars", comp: &fakeCompleter{result: over300}},
		{name: "not wired", comp: nil, noInst: true},
		{name: "exactly 300 chars ok", comp: &fakeCompleter{result: exact300}, wantApp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
			fc := &fakeCfgStore{cfg: llmOn()}
			ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
			clk := newFakeClock()
			n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
				func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
			n.opts.LLMBudget = 50 * time.Millisecond
			if !tc.noInst {
				n.opts.Summarizer = tc.comp
			}
			ctx := context.Background()
			n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
			clk.add(60 * time.Second)
			n.scan(ctx)
			waitDispatch(n)

			sent := ch.sent()
			if len(sent) != 1 {
				t.Fatalf("notification must deliver despite llm degradation, sends = %d", len(sent))
			}
			appended := strings.Contains(sent[0].Body, "AI 总结：")
			if appended != tc.wantApp {
				t.Fatalf("summary appended = %v, want %v, body = %q", appended, tc.wantApp, sent[0].Body)
			}
			if !strings.HasPrefix(sent[0].Body, "构建服务\n已空闲超过 60 秒") {
				t.Fatalf("deterministic summary must be preserved, body = %q", sent[0].Body)
			}
		})
	}
}

// TestLLM_EffectiveBudget E3：预算选择逻辑表驱动锁死——5s 硬上界，注入只允许
// 缩短（零值/负值/超限/恰好 5s → 5s；短预算原样生效）。
func TestLLM_EffectiveBudget(t *testing.T) {
	cases := []struct {
		injected time.Duration
		want     time.Duration
	}{
		{0, 5 * time.Second},
		{-3 * time.Second, 5 * time.Second},
		{30 * time.Second, 5 * time.Second}, // 6s/30s 等任何超限注入一律钳到 5s
		{5 * time.Second, 5 * time.Second},  // 恰好上界不变
		{80 * time.Millisecond, 80 * time.Millisecond},
		{4999 * time.Millisecond, 4999 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := effectiveBudget(tc.injected); got != tc.want {
			t.Errorf("effectiveBudget(%v) = %v, want %v", tc.injected, got, tc.want)
		}
	}
}

// TestLLM_BudgetDeadlinePropagation E3 集成断言：ctx.Deadline 与 Complete 进入
// 时间差 ≤ 有效预算（以 fake 记录的 entryTime 为基准，调度延迟不影响断言——
// 6s 回归必然使 deadline-entry > 5s 而失败）。
func TestLLM_BudgetDeadlinePropagation(t *testing.T) {
	cases := []struct {
		name     string
		injected time.Duration
		want     time.Duration
	}{
		{name: "default 5s", injected: 0, want: 5 * time.Second},
		{name: "over 5s clamped", injected: 30 * time.Second, want: 5 * time.Second},
		{name: "short budget effective", injected: 80 * time.Millisecond, want: 80 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
			fc := &fakeCfgStore{cfg: llmOn()}
			ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
			comp := &fakeCompleter{result: "总结"}
			clk := newFakeClock()
			n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
				func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
			n.opts.Summarizer = comp
			n.opts.LLMBudget = tc.injected
			ctx := context.Background()
			n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
			clk.add(60 * time.Second)
			n.scan(ctx)
			waitDispatch(n)
			if got := comp.callCount(); got != 1 {
				t.Fatalf("completer calls = %d, want 1", got)
			}
			d := comp.lastDeadline().Sub(comp.lastEntryTime())
			if d > tc.want {
				t.Fatalf("deadline-entry = %v, budget bound = %v — 5s 为硬上界，注入不得放大", d, tc.want)
			}
			if d < tc.want-500*time.Millisecond {
				t.Fatalf("deadline-entry = %v, too far below budget %v (deadline not propagated)", d, tc.want)
			}
		})
	}
}

// TestLLM_ZeroCallsOnGateFailure E4：五个门禁失败 stage（条件失效/总开关关/
// 类别关/URL 不可用/无渠道）各装配 fake completer 且 llm_summary=true ——
// 零 LLM 调用、零渠道调用（LLM 副作用只能在门禁全部通过后发生）。
func TestLLM_ZeroCallsOnGateFailure(t *testing.T) {
	mkResolver := func(err error) BaseURLResolver {
		return func(string) (string, error) {
			if err != nil {
				return "", err
			}
			return "http://127.0.0.1:7777", nil
		}
	}
	cases := []struct {
		name     string
		mutate   func(cfg *notification.Config, resolver *BaseURLResolver, ft *fakeTasks)
		category notification.Category
	}{
		{
			name:     "condition failed (recovered to busy)",
			category: notification.CategoryIdle,
			mutate: func(_ *notification.Config, _ *BaseURLResolver, ft *fakeTasks) {
				ft.set(activeSnap("t1", "构建服务", "busy"))
			},
		},
		{
			name:     "master off",
			category: notification.CategoryIdle,
			mutate:   func(cfg *notification.Config, _ *BaseURLResolver, _ *fakeTasks) { cfg.Enabled = false },
		},
		{
			name:     "category off",
			category: notification.CategoryIdle,
			mutate:   func(cfg *notification.Config, _ *BaseURLResolver, _ *fakeTasks) { cfg.Categories.Idle = false },
		},
		{
			name:     "url unavailable",
			category: notification.CategoryIdle,
			mutate:   func(_ *notification.Config, r *BaseURLResolver, _ *fakeTasks) { *r = mkResolver(errNotFound) },
		},
		{
			name:     "no channel",
			category: notification.CategoryIdle,
			mutate: func(cfg *notification.Config, _ *BaseURLResolver, _ *fakeTasks) {
				cfg.Channels.Web.Enabled = false
				cfg.Channels.Bark.Token = ""
				cfg.Channels.Bark.Endpoint = ""
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
			cfg := llmOn()
			resolver := mkResolver(nil)
			tc.mutate(&cfg, &resolver, ft)
			fc := &fakeCfgStore{cfg: cfg}
			ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
			comp := &fakeCompleter{result: "总结"}
			clk := newFakeClock()
			n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
				resolver, clk)
			n.opts.Summarizer = comp
			n.opts.LLMBudget = 50 * time.Millisecond
			ctx := context.Background()
			// 武装 idle（快照 idle 时条件成立；条件失效用例已改为 busy 快照）。
			n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
			clk.add(60 * time.Second)
			n.scan(ctx)
			waitDispatch(n)
			if got := comp.callCount(); got != 0 {
				t.Fatalf("gate failure must not trigger llm call, calls = %d", got)
			}
			if got := ch.callCount(); got != 0 {
				t.Fatalf("gate failure must not deliver, channel calls = %d", got)
			}
		})
	}
}

// TestLLM_BudgetBoundsDelivery 超时上界内完成降级投递（注入 50ms 预算 + 永不
// 返回的 completer）：墙钟断言仅作防挂死保护（预算语义见 TestLLM_BudgetClamp）。
func TestLLM_BudgetBoundsDelivery(t *testing.T) {
	n, _, fc, ch, _, clk := llmFixture(t)
	fc.set(llmOn())
	n.opts.Summarizer = &fakeCompleter{block: make(chan struct{})} // 永不主动返回
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)

	start := time.Now()
	n.scan(ctx)
	waitDispatch(n)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("delivery must complete within budget bound, elapsed = %v", elapsed)
	}
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Body != "构建服务\n已空闲超过 60 秒" {
		t.Fatalf("fallback delivery = %+v", sent)
	}
}

// TestLLM_PlanFixatedUnderConcurrentPUT tasks 5.2 集成断言：LLM 阻塞期间并发
// PUT（llm 开关/bark token/base_url 全量变更）不影响在途投递——总结仍生成、
// 渠道仍用计划固化配置与 URL。entered 信号提供确定性时序（无 Sleep 轮询）。
func TestLLM_PlanFixatedUnderConcurrentPUT(t *testing.T) {
	n, ft, fc, ch, comp, _ := llmFixture(t)
	fc.set(llmOn())
	comp.block = make(chan struct{})
	comp.entered = make(chan struct{})
	ctx := context.Background()

	// 在途投递：question 触发（事件驱动即时 dispatch），completer 阻塞中。
	snap := attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q1", "用哪个分支？")}, nil)
	ft.set(snap)
	n.handleEvent(ctx, attentionEvent("t1"))
	<-comp.entered // 确定性同步：completer 已进入（dispatch 在途）

	// 并发 PUT：llm 开关关、bark token/base_url 全量变更。
	mutated := testConfig()
	mutated.LLMSummary = false
	mutated.Channels.Bark.Token = "new-token-999999"
	mutated.BaseURL = "https://elsewhere.example.com"
	fc.set(mutated)

	close(comp.block) // 放行：completer 返回成功总结
	waitDispatch(n)

	sent := ch.sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	// LLMSummary 在计划中固化：在途投递仍附带总结。
	if want := "构建服务\n用哪个分支？\nAI 总结：模型返回的总结"; sent[0].Body != want {
		t.Fatalf("in-flight body must carry plan-fixated summary, got %q", sent[0].Body)
	}
	// 渠道配置与 URL 同样来自计划固化（不受并发 PUT 影响）。
	if got := ch.configs[0].Token; got != "bark-token-123456" {
		t.Fatalf("in-flight channel token = %q, want plan-fixated", got)
	}
	if got := sent[0].URL; got != "http://127.0.0.1:7777/#/task/t1" {
		t.Fatalf("in-flight url = %q, want plan-fixated", got)
	}

	// 下一次投递使用新配置：llm 关 → 不再调用 completer、新 token/base。
	snap2 := attentionSnapWith("idle", []application.PendingQuestion{
		pendingQuestion("q1", "用哪个分支？"), pendingQuestion("q2", "第二个问题"),
	}, nil)
	ft.set(snap2)
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if got := comp.callCount(); got != 1 {
		t.Fatalf("next delivery with llm off must not call completer, calls = %d", got)
	}
	if sent := ch.sent(); len(sent) != 2 || sent[1].URL != "https://elsewhere.example.com/#/task/t1" ||
		ch.configs[1].Token != "new-token-999999" || strings.Contains(sent[1].Body, "AI 总结：") {
		t.Fatalf("next delivery must use new config, got %+v / %+v", sent[1], ch.configs[1])
	}
}
