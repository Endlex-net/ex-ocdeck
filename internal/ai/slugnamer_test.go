package ai

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCleanSlug(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		// 合法
		{"fix-bug", "fix-bug", true},
		{"fix-bug-123", "fix-bug-123", true},
		{"a", "a", true},
		{"ab", "ab", true},
		// 去首行 + 空白
		{"  fix-bug\nsecond line", "fix-bug", true},
		// 去包裹引号
		{`"fix-bug"`, "fix-bug", true},
		{"`fix-bug`", "fix-bug", true},
		{"'fix-bug'", "fix-bug", true},
		// 去尾部标点
		{"fix-bug.", "fix-bug", true},
		{"fix-bug,", "fix-bug", true},
		{"fix-bug;", "fix-bug", true},
		{"fix-bug:", "fix-bug", true},
		// lowercase
		{"Fix-Bug", "fix-bug", true},
		{"FIXBUG", "fixbug", true},
		// 失败：空
		{"", "", false},
		{"   ", "", false},
		// 失败：含非法字符
		{"fix_bug", "", false}, // 下划线不匹配
		{"fix bug", "", false}, // 空格
		{"fix.bug", "", false}, // 内部点号
		// 失败：超长（>50）
		{strings.Repeat("a", 51), "", false},
		// 合法边界 50 字符
		{strings.Repeat("a", 50), strings.Repeat("a", 50), true},
		// 失败：首尾为 -
		{"-fix-bug", "", false},
		{"fix-bug-", "", false},
		// 失败：词表
		{"task", "", false},
		{"new", "", false},
		{"untitled", "", false},
		// 失败：以数字开头但词表命中（task 不含数字变体，仅命中精确）
		{"task1", "task1", true},
	}
	for _, c := range cases {
		got, ok := cleanSlug(c.in)
		if ok != c.ok || (c.ok && got != c.want) {
			t.Errorf("cleanSlug(%q)=%q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// fakeCompleter 用于 SlugNamer 测试，记录调用并返回预设结果。
type fakeCompleter struct {
	calls  atomic.Int32
	text   string
	err    error
	last   Request
	mu     sync.Mutex
}

func (f *fakeCompleter) Complete(ctx context.Context, req Request) (Response, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.last = req
	f.mu.Unlock()
	if f.err != nil {
		return Response{}, f.err
	}
	return Response{Text: f.text}, nil
}

func newConfiguredStoreWithDir(dataDir string) *Store {
	s := LoadStore(dataDir)
	_ = s.Put(validCfg())
	return s
}

func TestSlugNamer_NotConfigured_ZeroNetwork(t *testing.T) {
	store := LoadStore(t.TempDir()) // 未配置
	var fallbackCalled atomic.Int32
	namer := NewSlugNamer(store, func(s string) string {
		fallbackCalled.Add(1)
		return "fb-" + s
	})
	var fc fakeCompleter
	namer.completerFactory = func(ProviderConfig) (Completer, error) { return &fc, nil }

	got := namer.Slug(context.Background(), "任务名")
	if got != "fb-任务名" {
		t.Errorf("got %q want fb-任务名", got)
	}
	if fallbackCalled.Load() != 1 {
		t.Errorf("fallback should be called once")
	}
	if fc.calls.Load() != 0 {
		t.Errorf("no network call expected when unconfigured")
	}
}

func TestSlugNamer_LLMSuccess(t *testing.T) {
	store := newConfiguredStoreWithDir(t.TempDir())
	var fallbackCalled atomic.Int32
	namer := NewSlugNamer(store, func(s string) string {
		fallbackCalled.Add(1)
		return "fb"
	})
	fc := &fakeCompleter{text: "fix-login-bug"}
	namer.completerFactory = func(ProviderConfig) (Completer, error) { return fc, nil }

	got := namer.Slug(context.Background(), "修复登录bug")
	if got != "fix-login-bug" {
		t.Errorf("got %q want fix-login-bug", got)
	}
	if fallbackCalled.Load() != 0 {
		t.Errorf("fallback should NOT be called on success")
	}
	if fc.calls.Load() != 1 {
		t.Errorf("completer should be called once")
	}
	// prompt 应为 slugPrompt
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.last.System != slugPrompt {
		t.Errorf("system prompt=%q want slugPrompt", fc.last.System)
	}
	if fc.last.MaxTokens != slugNamerMaxTokens {
		t.Errorf("max_tokens=%d want %d", fc.last.MaxTokens, slugNamerMaxTokens)
	}
}

func TestSlugNamer_LLMError_Fallback(t *testing.T) {
	store := newConfiguredStoreWithDir(t.TempDir())
	namer := NewSlugNamer(store, func(s string) string { return "fb-" + s })
	fc := &fakeCompleter{err: errors.New("boom")}
	namer.completerFactory = func(ProviderConfig) (Completer, error) { return fc, nil }

	got := namer.Slug(context.Background(), "任务")
	if got != "fb-任务" {
		t.Errorf("got %q want fb-任务", got)
	}
}

func TestSlugNamer_LLMReturnsInvalid_Fallback(t *testing.T) {
	store := newConfiguredStoreWithDir(t.TempDir())
	namer := NewSlugNamer(store, func(s string) string { return "fb" })
	cases := []string{
		"",               // 空
		"task",           // 词表
		"Fix Bug",        // 含空格
		strings.Repeat("a", 51), // 超长
		"-bad",           // 首字符非法
	}
	for _, out := range cases {
		fc := &fakeCompleter{text: out}
		namer.completerFactory = func(ProviderConfig) (Completer, error) { return fc, nil }
		got := namer.Slug(context.Background(), "任务")
		if got != "fb" {
			t.Errorf("output %q: got %q want fb (fallback)", out, got)
		}
	}
}

func TestSlugNamer_CleansQuotedOutput(t *testing.T) {
	store := newConfiguredStoreWithDir(t.TempDir())
	namer := NewSlugNamer(store, func(s string) string { return "fb" })
	cases := []string{
		`"fix-bug"`,   // 包裹引号
		`  fix-bug  `, // 首尾空白
		`fix-bug.`,    // 尾部标点
		`Fix-Bug,`,    // 尾部标点 + 大写
	}
	for _, out := range cases {
		fc := &fakeCompleter{text: out}
		namer.completerFactory = func(ProviderConfig) (Completer, error) { return fc, nil }
		got := namer.Slug(context.Background(), "任务")
		if got != "fix-bug" {
			t.Errorf("output %q: got %q want fix-bug", out, got)
		}
	}
}

func TestSlugNamer_FactoryError_Fallback(t *testing.T) {
	store := newConfiguredStoreWithDir(t.TempDir())
	namer := NewSlugNamer(store, func(s string) string { return "fb" })
	namer.completerFactory = func(ProviderConfig) (Completer, error) { return nil, errors.New("bad cfg") }
	got := namer.Slug(context.Background(), "任务")
	if got != "fb" {
		t.Errorf("got %q want fb", got)
	}
}

func TestSlugNamer_NilNamer(t *testing.T) {
	var namer *SlugNamer
	got := namer.Slug(context.Background(), "Fix Bug!!")
	// 防御路径走 fallbackSlugify：fix-bug
	if got != "fix-bug" {
		t.Errorf("nil namer got %q want fix-bug", got)
	}
}

func TestSlugNamer_TaskNameTruncation(t *testing.T) {
	store := newConfiguredStoreWithDir(t.TempDir())
	namer := NewSlugNamer(store, func(s string) string { return "fb" })
	fc := &fakeCompleter{}
	namer.completerFactory = func(ProviderConfig) (Completer, error) { return fc, nil }
	long := strings.Repeat("字", 600)
	namer.Slug(context.Background(), long)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	userRunes := []rune(fc.last.User)
	if len(userRunes) != maxTaskNameChars {
		t.Errorf("user truncated to %d runes want %d", len(userRunes), maxTaskNameChars)
	}
}

// TestSlugNamer_ConcurrentPutSlug_NoMixedSnapshot 并发 PUT + Slug：单次 Slug 全程使用同一快照，
// 不会出现 provider/model 混配（design.md D7）。-race 不报 data race。
func TestSlugNamer_ConcurrentPutSlug_NoMixedSnapshot(t *testing.T) {
	store := LoadStore(t.TempDir())
	// 两套合法配置：openai+gpt-4o，anthropic+claude-3。合法的 provider/model 配对。
	pairA := ProviderConfig{Provider: ProviderOpenAI, APIKey: "sk-a-key-12345", Model: "gpt-4o"}
	pairB := ProviderConfig{Provider: ProviderAnthropic, APIKey: "sk-b-key-12345", Model: "claude-3"}
	if err := store.Put(pairA); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	// consistent 记录每个 Slug 调用收到的 (provider, model) 是否为合法配对。
	var consistent atomic.Bool
	consistent.Store(true)
	var seen atomic.Int32

	// allowedPairs: 合法的 (provider,model) 组合。
	allowed := map[Provider]string{
		ProviderOpenAI:    "gpt-4o",
		ProviderAnthropic: "claude-3",
	}
	namer := NewSlugNamer(store, func(s string) string { return "fb" })
	// factory 校验传入的 cfg 的 provider 与 model 为同一快照内的合法配对。
	namer.completerFactory = func(cfg ProviderConfig) (Completer, error) {
		seen.Add(1)
		if cfg.Model != allowed[cfg.Provider] {
			consistent.Store(false)
		}
		return &fakeCompleter{text: "fix-bug"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// writer: 并发交替 PUT pairA / pairB
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				c := pairA
				if (id+i)%2 == 0 {
					c = pairB
				}
				_ = store.Put(c)
			}
		}(w)
	}

	// reader: 持续 Slug
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				namer.Slug(context.Background(), "任务")
			}
		}()
	}

	// 跑一段时间后停止
	time.Sleep(100 * time.Millisecond)
	cancel()
	wg.Wait()

	if !consistent.Load() {
		t.Errorf("mixed snapshot detected: some Slug calls received mismatched provider/model pair")
	}
	if seen.Load() == 0 {
		t.Errorf("no Slug calls observed")
	}
}