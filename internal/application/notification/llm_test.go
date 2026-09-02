// llm_test.go LLM 停止原因总结（Lane E，task-notifications design D9 + spec
// 「LLM 停止原因总结（可选增强，仅 idle）」）：仅 idle 类别调用（其他类别
// completer 与 agent 输出拉取双零）、agent 最后一轮输出 prompt、输出不可得降级、
// 2s 拉取预算（总 5s 上界内）、失败/超时/未配置/空白/超长输出确定性降级、
// LLM 阻塞期间并发 PUT 不影响在途。
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

// fakeAgentOutput 记录型 agent 最后一轮输出 fake（D9）：记录 entry/deadline 供
// 拉取预算断言；block 非 nil 时阻塞（2s 预算 select 兜底的确定性构造）。
type fakeAgentOutput struct {
	mu        sync.Mutex
	calls     int
	deadlines []time.Time
	entryAt   []time.Time
	result    string
	ok        bool
	block     chan struct{}
}

func (f *fakeAgentOutput) LastAgentOutput(ctx context.Context, _ string) (string, bool) {
	f.mu.Lock()
	f.calls++
	f.entryAt = append(f.entryAt, time.Now())
	if dl, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, dl)
	}
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, f.ok
}

func (f *fakeAgentOutput) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAgentOutput) lastDeadline() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deadlines[len(f.deadlines)-1]
}

func (f *fakeAgentOutput) lastEntryTime() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entryAt[len(f.entryAt)-1]
}

// llmFixture 标准装置：bark 渠道 + fake completer + agent 输出 fake + 50ms 注入
// 预算（默认 5s 的可测等价，超时分支不睡真实时间）。
func llmFixture(t *testing.T) (*Notifier, *fakeTasks, *fakeCfgStore, *fakeChannel, *fakeCompleter, *fakeAgentOutput, *fakeClock) {
	t.Helper()
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "bark", caps: notification.CapGroup}
	comp := &fakeCompleter{result: "模型返回的总结"}
	fetch := &fakeAgentOutput{result: "agent 的最终输出", ok: true}
	clk := newFakeClock()
	n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	n.opts.Summarizer = comp
	n.opts.LastAgentOutput = fetch
	n.opts.LLMBudget = 50 * time.Millisecond
	return n, ft, fc, ch, comp, fetch, clk
}

// llmOn 开启 llm_summary 的配置副本。
func llmOn() notification.Config {
	c := testConfig()
	c.LLMSummary = true
	return c
}

// TestLLM_PromptVerbatim 固定 prompt 模板逐字（design D9）：占位符替换、
// max_tokens 200、类别用人类可读名、LastOutput 段原样嵌入。
func TestLLM_PromptVerbatim(t *testing.T) {
	in := idleIntent(activeSnap("t1", "构建服务", "idle"), 60, "u")
	got := buildSummaryPrompt(in, "完成了登录模块重构")
	want := "你是通知摘要助手。根据以下任务运行信息，用一两句中文概括该任务停止时的状态与最后完成的工作，只基于给定信息，不要臆测。\n" +
		"任务：构建服务\n" +
		"类别：任务已空闲\n" +
		"agent 最后一轮输出：\n" +
		"完成了登录模块重构"
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

// TestLLM_SummarySuccess idle 开关打开且调用成功：正文附带总结；prompt 携带
// agent 最后一轮输出；max_tokens 200。
func TestLLM_SummarySuccess(t *testing.T) {
	n, _, fc, ch, comp, fetch, clk := llmFixture(t)
	fc.set(llmOn())
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if got := comp.callCount(); got != 1 {
		t.Fatalf("completer calls = %d, want 1", got)
	}
	if got := fetch.callCount(); got != 1 {
		t.Fatalf("agent output fetch calls = %d, want 1", got)
	}
	if comp.lastMaxTokens() != 200 {
		t.Fatalf("max_tokens = %d, want 200", comp.lastMaxTokens())
	}
	sent := ch.sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if want := "已空闲超过 60 秒\nAI 总结：模型返回的总结"; sent[0].Body != want {
		t.Fatalf("body = %q, want %q", sent[0].Body, want)
	}
	if u := comp.lastUser(); !strings.Contains(u, "任务：构建服务") || !strings.Contains(u, "agent 最后一轮输出：\nagent 的最终输出") {
		t.Fatalf("prompt missing inputs: %q", u)
	}
}

// TestLLM_OnlyIdleCalls D9 范围门控：question/permission/retry/error/test 类别
// 零 LLM 调用、零 agent 输出拉取（通知本身照常投递）。
func TestLLM_OnlyIdleCalls(t *testing.T) {
	t.Run("question", func(t *testing.T) {
		n, ft, fc, ch, comp, fetch, _ := llmFixture(t)
		fc.set(llmOn())
		snap := attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q1", "用哪个分支？")}, nil)
		ft.set(snap)
		n.handleEvent(context.Background(), attentionEvent("t1"))
		waitDispatch(n)
		if comp.callCount() != 0 || fetch.callCount() != 0 {
			t.Fatalf("question must not call llm/fetch: comp=%d fetch=%d", comp.callCount(), fetch.callCount())
		}
		if got := ch.callCount(); got != 1 {
			t.Fatalf("question notification must still deliver, calls = %d", got)
		}
	})
	t.Run("retry", func(t *testing.T) {
		n, ft, fc, ch, comp, fetch, clk := llmFixture(t)
		fc.set(llmOn())
		snap := activeSnap("t1", "构建服务", "retry")
		snap.HasRetryDetail = true
		snap.RetryDetail = RetryDetail{Attempt: 2, Message: "超时"}
		ft.set(snap)
		ctx := context.Background()
		n.handleEvent(ctx, runStatusEvent("t1", "busy", "retry", true))
		clk.add(60 * time.Second)
		n.scan(ctx)
		waitDispatch(n)
		if comp.callCount() != 0 || fetch.callCount() != 0 {
			t.Fatalf("retry must not call llm/fetch: comp=%d fetch=%d", comp.callCount(), fetch.callCount())
		}
		if got := ch.callCount(); got != 1 {
			t.Fatalf("retry notification must still deliver, calls = %d", got)
		}
	})
	t.Run("error", func(t *testing.T) {
		n, _, fc, ch, comp, fetch, clk := llmFixture(t)
		fc.set(llmOn())
		ctx := context.Background()
		n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil))
		clk.add(60 * time.Second)
		n.scan(ctx)
		waitDispatch(n)
		if comp.callCount() != 0 || fetch.callCount() != 0 {
			t.Fatalf("error must not call llm/fetch: comp=%d fetch=%d", comp.callCount(), fetch.callCount())
		}
		if got := ch.callCount(); got != 1 {
			t.Fatalf("error notification must still deliver, calls = %d", got)
		}
	})
	t.Run("test", func(t *testing.T) {
		n, _, _, _, comp, fetch, _ := llmFixture(t)
		cfg := llmOn()
		n.SendTestNotification(context.Background(), cfg, "http://x")
		if comp.callCount() != 0 || fetch.callCount() != 0 {
			t.Fatalf("test notification must not call llm/fetch: comp=%d fetch=%d", comp.callCount(), fetch.callCount())
		}
	})
}

// TestLLM_LastOutputUnavailable agent 输出不可得（拉取失败/超时/未装配）：
// prompt 的 LastOutput 段为「（不可得）」，总结仍生成、通知照常投递（spec
// 「agent 输出不可得降级」）。
func TestLLM_LastOutputUnavailable(t *testing.T) {
	n, _, fc, ch, comp, fetch, clk := llmFixture(t)
	fetch.ok = false
	fc.set(llmOn())
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if got := comp.callCount(); got != 1 {
		t.Fatalf("completer must still be called with unavailable output, calls = %d", got)
	}
	if u := comp.lastUser(); !strings.Contains(u, "agent 最后一轮输出：\n"+lastOutputUnavailable) {
		t.Fatalf("prompt must carry %q segment, got %q", lastOutputUnavailable, u)
	}
	if sent := ch.sent(); len(sent) != 1 || !strings.Contains(sent[0].Body, "AI 总结：模型返回的总结") {
		t.Fatalf("summary must still append and deliver, got %+v", sent)
	}
}

// TestLLM_FetchNotWired 未装配 agent 输出端口：与不可得同路径（「（不可得）」）。
func TestLLM_FetchNotWired(t *testing.T) {
	n, _, fc, ch, comp, _, clk := llmFixture(t)
	n.opts.LastAgentOutput = nil
	fc.set(llmOn())
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if got := comp.callCount(); got != 1 {
		t.Fatalf("completer calls = %d, want 1", got)
	}
	if u := comp.lastUser(); !strings.Contains(u, lastOutputUnavailable) {
		t.Fatalf("prompt must carry unavailable marker, got %q", u)
	}
	if sent := ch.sent(); len(sent) != 1 || !strings.Contains(sent[0].Body, "AI 总结：") {
		t.Fatalf("delivery must carry summary, got %+v", sent)
	}
}

// TestLLM_FetchBudget 拉取预算 2s（design D9，含在总预算内）：阻塞不返回的
// 端口被 select 兜底截断，deadline-entry ≤ 2s；投递照常完成（「（不可得）」
// 路径 + completer 正常返回）。
func TestLLM_FetchBudget(t *testing.T) {
	n, _, fc, ch, comp, fetch, clk := llmFixture(t)
	n.opts.LLMBudget = 0 // 默认 5s 总预算，暴露拉取 2s 子预算
	fetch.block = make(chan struct{})
	fc.set(llmOn())
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	start := time.Now()
	n.scan(ctx)
	waitDispatch(n)

	if got := fetch.callCount(); got != 1 {
		t.Fatalf("fetch calls = %d, want 1", got)
	}
	if d := fetch.lastDeadline().Sub(fetch.lastEntryTime()); d > lastAgentOutputBudget {
		t.Fatalf("fetch budget = %v, want ≤ %v", d, lastAgentOutputBudget)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("delivery exceeded total budget, elapsed = %v", elapsed)
	}
	if u := comp.lastUser(); !strings.Contains(u, lastOutputUnavailable) {
		t.Fatalf("blocked fetch must degrade to unavailable marker, got %q", u)
	}
	if sent := ch.sent(); len(sent) != 1 {
		t.Fatalf("delivery must complete within budget, sends = %d", len(sent))
	}
}

// TestLLM_DisabledNoCall 开关关闭（默认）：不发起任何 LLM 调用与 agent 输出
// 拉取，正文仅确定性摘要。
func TestLLM_DisabledNoCall(t *testing.T) {
	n, _, _, ch, comp, fetch, clk := llmFixture(t) // testConfig 默认 llm_summary=false
	ctx := context.Background()
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)

	if got := comp.callCount(); got != 0 {
		t.Fatalf("llm_summary off must not call completer, calls = %d", got)
	}
	if got := fetch.callCount(); got != 0 {
		t.Fatalf("llm_summary off must not fetch agent output, calls = %d", got)
	}
	sent := ch.sent()
	if len(sent) != 1 || sent[0].Body != "已空闲超过 60 秒" {
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
			if !strings.HasPrefix(sent[0].Body, "已空闲超过 60 秒") {
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
			fetch := &fakeAgentOutput{result: "输出", ok: true}
			clk := newFakeClock()
			n := newTestNotifier(ft, &fakeLister{ids: []string{"t1"}}, fc, []notification.Channel{ch},
				resolver, clk)
			n.opts.Summarizer = comp
			n.opts.LastAgentOutput = fetch
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
			if got := fetch.callCount(); got != 0 {
				t.Fatalf("gate failure must not fetch agent output, calls = %d", got)
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
	n, _, fc, ch, _, _, clk := llmFixture(t)
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
	if len(sent) != 1 || sent[0].Body != "已空闲超过 60 秒" {
		t.Fatalf("fallback delivery = %+v", sent)
	}
}

// TestLLM_PlanFixatedUnderConcurrentPUT tasks 5.2 集成断言：LLM 阻塞期间并发
// PUT（llm 开关/bark token/base_url 全量变更）不影响在途投递——总结仍生成、
// 渠道仍用计划固化配置与 URL。entered 信号提供确定性时序（无 Sleep 轮询）。
func TestLLM_PlanFixatedUnderConcurrentPUT(t *testing.T) {
	n, ft, fc, ch, comp, _, clk := llmFixture(t)
	fc.set(llmOn())
	comp.block = make(chan struct{})
	comp.entered = make(chan struct{})
	ctx := context.Background()

	// 在途投递：idle 触发（唯一 LLM 类别），completer 阻塞中。
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
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
		t.Fatalf("sends = %d", len(sent))
	}
	// LLMSummary 在计划中固化：在途投递仍附带总结。
	if want := "已空闲超过 60 秒\nAI 总结：模型返回的总结"; sent[0].Body != want {
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
	ft.set(activeSnap("t1", "构建服务", "idle"))
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := comp.callCount(); got != 1 {
		t.Fatalf("next delivery with llm off must not call completer, calls = %d", got)
	}
	if sent := ch.sent(); len(sent) != 2 || sent[1].URL != "https://elsewhere.example.com/#/task/t1" ||
		ch.configs[1].Token != "new-token-999999" || strings.Contains(sent[1].Body, "AI 总结：") {
		t.Fatalf("next delivery must use new config, got %+v / %+v", sent[1], ch.configs[1])
	}
}
