package hostenv

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFakeCapture 替换捕获注入点为 fake 并重置缓存状态（隔离测试间状态），测试结束恢复。
// 返回捕获计数（atomic，并发测试安全）供断言捕获次数。
func withFakeCapture(t *testing.T, fake func() map[string]string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	origCapture, origCache, origLoaded := Capture, shellEnvCache, shellEnvLoaded
	Capture = func() map[string]string {
		calls.Add(1)
		return fake()
	}
	mu.Lock()
	shellEnvCache, shellEnvLoaded = nil, false
	mu.Unlock()
	t.Cleanup(func() {
		Capture = origCapture
		mu.Lock()
		shellEnvCache, shellEnvLoaded = origCache, origLoaded
		mu.Unlock()
	})
	return &calls
}

// TestLookupProcessEnvWins 进程环境命中且非空时优先返回，且不触发 login shell 捕获。
func TestLookupProcessEnvWins(t *testing.T) {
	calls := withFakeCapture(t, func() map[string]string {
		return map[string]string{"OCDECK_TEST_FOLLOW": "from-shell"}
	})
	t.Setenv("OCDECK_TEST_FOLLOW", "from-process")

	v, ok := Lookup("OCDECK_TEST_FOLLOW")
	if !ok || v != "from-process" {
		t.Fatalf("Lookup = (%q, %v), want (from-process, true)", v, ok)
	}
	if calls.Load() != 0 {
		t.Fatalf("capture calls = %d, want 0 (process env hit must not capture)", calls.Load())
	}
}

// TestLookupProcessEnvEmptyValueMisses 进程环境存在但为空串时视为未命中，走兜底。
func TestLookupProcessEnvEmptyValueMisses(t *testing.T) {
	calls := withFakeCapture(t, func() map[string]string {
		return map[string]string{"OCDECK_TEST_EMPTY": "from-shell"}
	})
	t.Setenv("OCDECK_TEST_EMPTY", "")

	v, ok := Lookup("OCDECK_TEST_EMPTY")
	if !ok || v != "from-shell" {
		t.Fatalf("Lookup = (%q, %v), want (from-shell, true)", v, ok)
	}
	if calls.Load() != 1 {
		t.Fatalf("capture calls = %d, want 1", calls.Load())
	}
}

// TestLookupFallsBackToShellEnv 进程环境未命中时兜底返回 login shell 捕获值。
func TestLookupFallsBackToShellEnv(t *testing.T) {
	calls := withFakeCapture(t, func() map[string]string {
		return map[string]string{"OCDECK_TEST_FOLLOW": "from-shell"}
	})

	v, ok := Lookup("OCDECK_TEST_FOLLOW")
	if !ok || v != "from-shell" {
		t.Fatalf("Lookup = (%q, %v), want (from-shell, true)", v, ok)
	}
	if calls.Load() != 1 {
		t.Fatalf("capture calls = %d, want 1", calls.Load())
	}
}

// TestLookupCapturesOnce 多次 Lookup 不同 miss key 时整包只捕获一次。
func TestLookupCapturesOnce(t *testing.T) {
	calls := withFakeCapture(t, func() map[string]string {
		return map[string]string{"OCDECK_TEST_K1": "v1", "OCDECK_TEST_K2": "v2"}
	})

	if v, ok := Lookup("OCDECK_TEST_K1"); !ok || v != "v1" {
		t.Fatalf("Lookup K1 = (%q, %v), want (v1, true)", v, ok)
	}
	if v, ok := Lookup("OCDECK_TEST_K2"); !ok || v != "v2" {
		t.Fatalf("Lookup K2 = (%q, %v), want (v2, true)", v, ok)
	}
	if _, ok := Lookup("OCDECK_TEST_MISSING"); ok {
		t.Fatal("Lookup MISSING = ok, want miss")
	}
	if calls.Load() != 1 {
		t.Fatalf("capture calls = %d, want 1 (capture once per process)", calls.Load())
	}
}

// TestLookupCaptureFailureNotRetried 捕获失败（nil）缓存失败结果：后续 Lookup 直接
// miss 且不再触发捕获。
func TestLookupCaptureFailureNotRetried(t *testing.T) {
	calls := withFakeCapture(t, func() map[string]string { return nil })

	if _, ok := Lookup("OCDECK_TEST_MISSING1"); ok {
		t.Fatal("Lookup MISSING1 = ok, want miss after capture failure")
	}
	if _, ok := Lookup("OCDECK_TEST_MISSING2"); ok {
		t.Fatal("Lookup MISSING2 = ok, want miss after capture failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("capture calls = %d, want 1 (failure cached, no retry)", calls.Load())
	}
}

// TestLookupShellCacheEmptyValueMisses 捕获缓存中的空值键保留（键存在性供列举 source），
// 但 Lookup 的有效值判断将空串视为未命中。
func TestLookupShellCacheEmptyValueMisses(t *testing.T) {
	withFakeCapture(t, func() map[string]string {
		return map[string]string{"OCDECK_TEST_EMPTYKEY": ""}
	})

	if _, ok := Lookup("OCDECK_TEST_EMPTYKEY"); ok {
		t.Fatal("Lookup empty-value key = ok, want miss (空串视为未命中)")
	}
}

// TestRefreshFailureFromUnloadedEntersFailedState 未加载态首次 Refresh 失败 MUST 迁移为
// 失败缓存：后续普通读取按失败缓存服务、不再次捕获（仅下一次显式 Refresh 可重试，D3）。
func TestRefreshFailureFromUnloadedEntersFailedState(t *testing.T) {
	calls := withFakeCapture(t, func() map[string]string { return nil })

	if err := Refresh(); err == nil {
		t.Fatal("Refresh = nil error, want error on capture failure")
	}
	if _, ok := Lookup("OCDECK_TEST_ANY"); ok {
		t.Fatal("Lookup after failed refresh = ok, want miss")
	}
	if calls.Load() != 1 {
		t.Fatalf("capture calls = %d, want 1 (failed state must not auto-retry)", calls.Load())
	}
}

// TestRefreshUpdatesCache Refresh 后以新捕获结果替换缓存，后续 Lookup 取新值。
func TestRefreshUpdatesCache(t *testing.T) {
	value := "v1"
	withFakeCapture(t, func() map[string]string {
		return map[string]string{"OCDECK_TEST_REFRESH": value}
	})

	if v, ok := Lookup("OCDECK_TEST_REFRESH"); !ok || v != "v1" {
		t.Fatalf("Lookup before refresh = (%q, %v), want (v1, true)", v, ok)
	}
	value = "v2"
	if err := Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if v, ok := Lookup("OCDECK_TEST_REFRESH"); !ok || v != "v2" {
		t.Fatalf("Lookup after refresh = (%q, %v), want (v2, true)", v, ok)
	}
}

// TestRefreshFailureKeepsOldCache Refresh 失败时保留旧缓存并返回 error。
func TestRefreshFailureKeepsOldCache(t *testing.T) {
	fail := false
	withFakeCapture(t, func() map[string]string {
		if fail {
			return nil
		}
		return map[string]string{"OCDECK_TEST_REFRESH": "old"}
	})

	if _, ok := Lookup("OCDECK_TEST_REFRESH"); !ok {
		t.Fatal("Lookup before refresh = miss, want hit")
	}
	fail = true
	if err := Refresh(); err == nil {
		t.Fatal("Refresh = nil error, want error on capture failure")
	}
	if v, ok := Lookup("OCDECK_TEST_REFRESH"); !ok || v != "old" {
		t.Fatalf("Lookup after failed refresh = (%q, %v), want (old, true)", v, ok)
	}
}

// TestRefreshConcurrentSerializesPublish 并发 Refresh 的捕获+发布串行化（D3/J2）：
// 发布顺序 = 捕获顺序，最终缓存 = 最后一次捕获的结果——后到者不得用旧结果覆盖新缓存。
func TestRefreshConcurrentSerializesPublish(t *testing.T) {
	var seq atomic.Int64
	withFakeCapture(t, func() map[string]string {
		return map[string]string{"SEQ": strconv.FormatInt(seq.Add(1), 10)}
	})
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Refresh()
		}()
	}
	wg.Wait()

	mu.RLock()
	got := shellEnvCache["SEQ"]
	mu.RUnlock()
	want := strconv.FormatInt(n, 10)
	if got != want {
		t.Fatalf("cache SEQ = %q, want %q (publishes must follow capture order)", got, want)
	}
}

// TestLookupRefreshConcurrent 并发 Lookup 与 Refresh 不产生数据竞争（go test -race 下验证）。
func TestLookupRefreshConcurrent(t *testing.T) {
	withFakeCapture(t, func() map[string]string {
		return map[string]string{"OCDECK_TEST_RACE": "v"}
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = Lookup("OCDECK_TEST_RACE")
				_, _ = Lookup("OCDECK_TEST_MISS")
				_ = Refresh()
			}
		}()
	}
	wg.Wait()
}

// TestParseLoginShellEnv 解析：标记过滤 motd/prompt 垃圾、NUL 分隔、值含 = 与换行、
// 无 = 条目忽略、KEY= 空值条目保留（键存在性供列举 source）、重复键取首条。
func TestParseLoginShellEnv(t *testing.T) {
	out := []byte(
		"motd line\nlast login: ...\nprompt$ " +
			"\n__OCDECK_ENV_BEGIN__\n" +
			"PLAIN=1\x00" +
			"WITH_EQ=a=b=c\x00" +
			"MULTI=line1\nline2\x00" +
			"EMPTY=\x00" +
			"NOSEP\x00" +
			"DUP=first\x00DUP=second\x00" +
			"\n__OCDECK_ENV_END__\n\x00" +
			"prompt$ ")

	env, ok := parseLoginShellEnv(out)
	if !ok {
		t.Fatal("parseLoginShellEnv = !ok, want ok (markers present)")
	}
	want := map[string]string{
		"PLAIN":   "1",
		"WITH_EQ": "a=b=c",
		"MULTI":   "line1\nline2",
		"EMPTY":   "",
		"DUP":     "first",
	}
	if !maps.Equal(env, want) {
		t.Fatalf("parseLoginShellEnv = %v, want %v", env, want)
	}
}

// TestParseLoginShellEnvMarkerTextInValue 记录内的换行与标记文本（含完整标记行）一律
// 视为值内容，不构成帧边界；帧结束仅由 NUL 终止的 END 控制记录标识。
func TestParseLoginShellEnvMarkerTextInValue(t *testing.T) {
	out := []byte(
		"\n__OCDECK_ENV_BEGIN__\n" +
			"TRICKY=prefix __OCDECK_ENV_END__ suffix\x00" +
			"FULL=line1\n__OCDECK_ENV_END__\nline3\x00" +
			"PLAIN=1\x00" +
			"\n__OCDECK_ENV_END__\n\x00" +
			"prompt$ ")

	env, ok := parseLoginShellEnv(out)
	if !ok {
		t.Fatal("parseLoginShellEnv = !ok, want ok (marker text inside records is value content)")
	}
	want := map[string]string{
		"TRICKY": "prefix __OCDECK_ENV_END__ suffix",
		"FULL":   "line1\n__OCDECK_ENV_END__\nline3",
		"PLAIN":  "1",
	}
	if !maps.Equal(env, want) {
		t.Fatalf("parseLoginShellEnv = %v, want %v", env, want)
	}
}

// TestParseLoginShellEnvEmptyKeyIgnored 空键记录（=value）忽略且不导致捕获失败
// （存在其它合法条目时帧合法）；G1：忽略后零个合法条目才判失败。
func TestParseLoginShellEnvEmptyKeyIgnored(t *testing.T) {
	out := []byte(
		"\n__OCDECK_ENV_BEGIN__\n" +
			"=orphan\x00" +
			"REAL=1\x00" +
			"EMPTY=\x00" +
			"\n__OCDECK_ENV_END__\n\x00")

	env, ok := parseLoginShellEnv(out)
	if !ok {
		t.Fatal("parseLoginShellEnv = !ok, want ok (empty-key record ignored, valid entries present)")
	}
	want := map[string]string{"REAL": "1", "EMPTY": ""}
	if !maps.Equal(env, want) {
		t.Fatalf("parseLoginShellEnv = %v, want %v", env, want)
	}
}

// TestParseLoginShellEnvEndKeyRecordNotBoundary H1 回归：键包含完整 END 标记文本的
// 记录必须完整保留、帧不在其中间结束。行识别协议会在这条记录的边界处（前导换行 +
// 紧随换行形态）误判帧结束，导致部分结果被当成功缓存发布。
func TestParseLoginShellEnvEndKeyRecordNotBoundary(t *testing.T) {
	out := []byte(
		"\n__OCDECK_ENV_BEGIN__\n" +
			"A=1\x00" +
			// 该记录以完整 END 标记文本开头（键含换行的极端形态）且紧随换行：
			// 记录实际内容 = 键 "\n__OCDECK_ENV_END__\n\nB"、值 "2"，必含 =，非控制记录。
			"\n__OCDECK_ENV_END__\n\nB=2\x00" +
			"\n__OCDECK_ENV_END__\n\x00")

	env, ok := parseLoginShellEnv(out)
	if !ok {
		t.Fatal("parseLoginShellEnv = !ok, want ok (record with END marker text in key is not a frame boundary)")
	}
	want := map[string]string{
		"A":                         "1",
		"\n__OCDECK_ENV_END__\n\nB": "2",
	}
	if !maps.Equal(env, want) {
		t.Fatalf("parseLoginShellEnv = %v, want %v (record must be preserved verbatim)", env, want)
	}
}

// TestParseLoginShellEnvInvalidFrames 无效帧一律返回 false：无标记、END 在 BEGIN 前、
// BEGIN 无 END、最后一条记录缺 NUL 终止（截断）、END 控制记录缺终止 NUL、
// 空帧、忽略后零个合法条目（G1：不得发布成功空缓存）。
func TestParseLoginShellEnvInvalidFrames(t *testing.T) {
	cases := map[string][]byte{
		"empty output":                {},
		"garbage only":                []byte("motd\nprompt$ "),
		"end before begin":            []byte("\n__OCDECK_ENV_END__\n mid \n__OCDECK_ENV_BEGIN__\n"),
		"begin without end":           []byte("\n__OCDECK_ENV_BEGIN__\nA=1\x00"),
		"truncated last record":       []byte("\n__OCDECK_ENV_BEGIN__\nA=1\x00B=2\n__OCDECK_ENV_END__\n"),
		"end token missing NUL":       []byte("\n__OCDECK_ENV_BEGIN__\nA=1\x00\n__OCDECK_ENV_END__\n"),
		"end not at record boundary":  []byte("\n__OCDECK_ENV_BEGIN__\nA=1\x00__END__\n"),
		"empty frame":                 []byte("\n__OCDECK_ENV_BEGIN__\n\n__OCDECK_ENV_END__\n"),
		"empty frame with terminator": []byte("\n__OCDECK_ENV_BEGIN__\n\n__OCDECK_ENV_END__\n\x00"),
		"only non-parseable entries":  []byte("\n__OCDECK_ENV_BEGIN__\nNOSEP\x00=orphan\x00\x00\n__OCDECK_ENV_END__\n\x00"),
	}
	for name, out := range cases {
		if _, ok := parseLoginShellEnv(out); ok {
			t.Errorf("%s: parseLoginShellEnv = ok, want !ok", name)
		}
	}
}

// TestParsePasswdShell /etc/passwd 解析纯函数表单测（linux 回退分支逻辑）：
// 正常行取 shell、用户不存在、shell 字段为空/非绝对路径、格式损坏行跳过。
func TestParsePasswdShell(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/bash\n" +
		"zshuser:x:1000:1000::/home/zshuser:/usr/bin/zsh\n" +
		"emptyshell:x:1001:1001::/home/empty:\n" +
		"relshell:x:1002:1002::/home/rel:fish\n" +
		"broken-line-no-colon\n" +
		"short:x:1:2\n" +
		"trailing:x:1003:1003::/home/t:/bin/dash" // 末行无换行
	cases := []struct {
		name     string
		username string
		want     string
	}{
		{"normal user zsh shell", "zshuser", "/usr/bin/zsh"},
		{"root bash", "root", "/bin/bash"},
		{"last line without newline", "trailing", "/bin/dash"},
		{"user missing", "nobody-here", ""},
		{"empty shell field", "emptyshell", ""},
		{"relative shell field", "relshell", ""},
		{"broken line skipped (no colon)", "broken-line-no-colon", ""},
		{"short line skipped (missing fields)", "short", ""},
	}
	for _, tc := range cases {
		if got := parsePasswdShell(passwd, tc.username); got != tc.want {
			t.Errorf("%s: parsePasswdShell(%q) = %q, want %q", tc.name, tc.username, got, tc.want)
		}
	}
}

// TestParseLoginShellEnvBeginAtEOF L1 回归：BEGIN 恰好结束于 EOF（无末尾换行）属
// 截断帧，判捕获失败而非 panic（旧实现在 EOF 伪行上匹配后起始索引越界）。
func TestParseLoginShellEnvBeginAtEOF(t *testing.T) {
	env, ok := parseLoginShellEnv([]byte("\n__OCDECK_ENV_BEGIN__"))
	if ok || env != nil {
		t.Fatalf("parseLoginShellEnv = (%v, %v), want (nil, false) at BEGIN terminated by EOF", env, ok)
	}
}

// TestParseLoginShellEnvTruncatedPrefixes L1 回归：遍历有效帧的全部截断前缀——
// 完整帧最短长度之前一律判失败不发布（不 panic）；达到最短长度后尾部垃圾截断不
// 影响解析结果。
func TestParseLoginShellEnvTruncatedPrefixes(t *testing.T) {
	valid := []byte("motd\n" +
		"\n__OCDECK_ENV_BEGIN__\n" +
		"A=1\x00B=2\x00" +
		"\n__OCDECK_ENV_END__\n\x00" +
		"prompt$ ")
	endToken := []byte("\n__OCDECK_ENV_END__\n\x00")
	endNul := bytes.Index(valid, endToken)
	if endNul < 0 {
		t.Fatal("fixture broken: END control record not found")
	}
	minLen := endNul + len(endToken) // 完整帧最短长度（END 控制记录 NUL 终止处）
	want := map[string]string{"A": "1", "B": "2"}

	for i := 0; i < len(valid); i++ {
		env, ok := parseLoginShellEnv(valid[:i])
		if i < minLen {
			if ok {
				t.Fatalf("truncated prefix len=%d parsed ok (%v), want !ok (截断不发布)", i, env)
			}
			continue
		}
		if !ok {
			t.Fatalf("prefix len=%d (complete frame + partial trailer) = !ok, want ok", i)
		}
		if !maps.Equal(env, want) {
			t.Fatalf("prefix len=%d = %v, want %v (尾部垃圾截断不影响帧解析)", i, env, want)
		}
	}
}

// writeFakeShell 写一个忽略参数的临时可执行脚本作为 SHELL（captureLoginShellEnv 经
// os.Getenv("SHELL") 取用），返回脚本路径。
func writeFakeShell(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakeshell.sh")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}
	return path
}

// TestCaptureShellValidFrame fake shell 输出完整有效帧时捕获成功并解析出条目。
func TestCaptureShellValidFrame(t *testing.T) {
	t.Setenv("SHELL", writeFakeShell(t,
		"#!/bin/bash\nprintf '\\n__OCDECK_ENV_BEGIN__\\n' && printf 'A=1\\0B=2\\0' && printf '\\n__OCDECK_ENV_END__\\n\\0'\n"))

	env := captureLoginShellEnv()
	if len(env) != 2 || env["A"] != "1" || env["B"] != "2" {
		t.Fatalf("captureLoginShellEnv = %v, want {A:1 B:2}", env)
	}
}

// TestCaptureShellOnlyGarbageRecordsFails 帧内全部为无 = / 空键记录（忽略后零个合法
// 条目）时捕获判失败，不得发布成功空缓存（G1）。
func TestCaptureShellOnlyGarbageRecordsFails(t *testing.T) {
	t.Setenv("SHELL", writeFakeShell(t,
		"#!/bin/bash\nprintf '\\n__OCDECK_ENV_BEGIN__\\n' && printf 'NOSEP\\0=orphan\\0' && printf '\\n__OCDECK_ENV_END__\\n\\0'\n"))

	if env := captureLoginShellEnv(); env != nil {
		t.Fatalf("captureLoginShellEnv = %v, want nil (no parseable entries)", env)
	}
}

// TestCaptureShellNonZeroExitFails 模拟 env -0 失败（&& 链中断，退出码非零、无 END）：
// 捕获判定失败返回 nil，不得发布部分结果或成功空结果。
func TestCaptureShellNonZeroExitFails(t *testing.T) {
	t.Setenv("SHELL", writeFakeShell(t, "#!/bin/bash\nprintf '\\n__OCDECK_ENV_BEGIN__\\n'\nexit 3\n"))

	if env := captureLoginShellEnv(); env != nil {
		t.Fatalf("captureLoginShellEnv = %v, want nil (non-zero exit, no END marker)", env)
	}
}

// TestCaptureShellTimeoutKillsProcessGroup 超时后 kill 整个进程组：shell 后代持有
// stdout 管道时 Wait 不悬挂，捕获在超时窗口附近返回 nil。
func TestCaptureShellTimeoutKillsProcessGroup(t *testing.T) {
	t.Setenv("SHELL", writeFakeShell(t, "#!/bin/bash\nsleep 30 &\nwait $!\n"))
	orig := captureTimeout
	captureTimeout = 300 * time.Millisecond
	t.Cleanup(func() { captureTimeout = orig })

	start := time.Now()
	if env := captureLoginShellEnv(); env != nil {
		t.Fatal("captureLoginShellEnv = env, want nil (timeout)")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("capture took %v, process group kill hung", elapsed)
	}
}

// TestCaptureShellDetachedGrandchildHoldsStdout K3 回归：输出完整帧后启动 setsid
// 脱离进程组的后代（perl POSIX::setsid 后 exec sleep，darwin 无 setsid 命令）持有
// stdout 管道——杀进程组不影响它，Wait 必须受 WaitDelay 上限约束：捕获在限定时间
// 内返回失败（不发布缓存），captureMu 随后可被 Refresh 重新获取。
// 旧实现（无 WaitDelay）下本用例悬挂至后代 sleep 结束，超时断言失败（已验证变红）。
func TestCaptureShellDetachedGrandchildHoldsStdout(t *testing.T) {
	t.Setenv("SHELL", writeFakeShell(t,
		"#!/bin/bash\n"+
			"printf '\\n__OCDECK_ENV_BEGIN__\\n'\n"+
			"perl -MPOSIX -e 'POSIX::setsid(); exec \"sleep\", \"30\"' &\n"+
			"printf 'A=1\\0'\n"+
			"printf '\\n__OCDECK_ENV_END__\\n\\0'\n"))

	start := time.Now()
	if env := captureLoginShellEnv(); env != nil {
		t.Fatalf("captureLoginShellEnv = %v, want nil (detached grandchild holds stdout, frame incomplete)", env)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("capture took %v, WaitDelay did not bound pipe drain", elapsed)
	}

	// captureMu 已释放：Refresh 可再次执行（该脚本每次都触发管道收尾失败 → 返回 error
	// 即证明调用返回未阻塞）。
	if err := Refresh(); err == nil {
		t.Fatal("Refresh after detached-grandchild capture = nil error, want error (capture fails again but must not block)")
	}
}
