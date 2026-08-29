// content_test.go 内容组装模板与级别映射（design D4 逐字模板；spec「通知内容与
// 跳转链接」「通知渠道投递与降级」级别映射表）。
package notification

import (
	"encoding/json"
	"strings"
	"testing"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/domain/notification"
)

// pendingQuestion / pendingPermission 经 JSON 反序列化构造 application.Attention
// 内嵌的请求类型（测试二进制不直接 import infrastructure/opencode，Lane D 4.4
// import 断言覆盖 -test 闭包）。
func pendingQuestion(id string, questionTexts ...string) application.PendingQuestion {
	type item struct {
		Header   string `json:"header"`
		Question string `json:"question"`
	}
	items := make([]item, 0, len(questionTexts))
	for _, q := range questionTexts {
		items = append(items, item{Header: "h", Question: q})
	}
	raw, err := json.Marshal(map[string]any{"id": id, "sessionID": "s1", "questions": items})
	if err != nil {
		panic(err)
	}
	var pq application.PendingQuestion
	if err := json.Unmarshal(raw, &pq); err != nil {
		panic(err)
	}
	return pq
}

func pendingPermission(id, permission string, patterns ...string) application.PendingPermission {
	if patterns == nil {
		patterns = []string{}
	}
	raw, err := json.Marshal(map[string]any{"id": id, "sessionID": "s1", "permission": permission, "patterns": patterns})
	if err != nil {
		panic(err)
	}
	var pp application.PendingPermission
	if err := json.Unmarshal(raw, &pp); err != nil {
		panic(err)
	}
	return pp
}

// TestLevelMapping 级别映射逐字按 spec 映射表。
func TestLevelMapping(t *testing.T) {
	cases := map[notification.Category]notification.Level{
		notification.CategoryQuestion:   notification.LevelTimeSensitive,
		notification.CategoryPermission: notification.LevelTimeSensitive,
		notification.CategoryError:      notification.LevelCritical,
		notification.CategoryRetry:      notification.LevelTimeSensitive,
		notification.CategoryIdle:       notification.LevelActive,
		notification.CategoryTest:       notification.LevelActive,
	}
	for cat, want := range cases {
		if got := levelFor(cat); got != want {
			t.Errorf("levelFor(%s) = %s, want %s", cat, got, want)
		}
	}
}

func contentSnap() TaskSnapshot {
	return TaskSnapshot{Task: TaskRef{ID: "t1", Name: "构建服务", Status: "active"}, RunStatus: "idle"}
}

func TestQuestionIntent(t *testing.T) {
	snap := contentSnap()
	snap.Attention.Questions = []application.PendingQuestion{
		pendingQuestion("q1", "用哪个分支？", "要跑测试吗？"),
	}
	got := questionIntent(snap, snap.Attention.Questions[0], "http://x/#/task/t1")
	if got.Title != "等待你的回答" {
		t.Fatalf("title = %q", got.Title)
	}
	if want := "构建服务\n用哪个分支？\n要跑测试吗？"; got.Body != want {
		t.Fatalf("body = %q, want %q", got.Body, want)
	}
	if got.Category != notification.CategoryQuestion || got.Level != notification.LevelTimeSensitive || got.URL != "http://x/#/task/t1" {
		t.Fatalf("intent = %+v", got)
	}
}

// TestQuestionIntent_Truncation 单字段截 200（rune）、多条拼接整体截 500。
func TestQuestionIntent_Truncation(t *testing.T) {
	snap := contentSnap()
	long := strings.Repeat("问", 250)
	snap.Attention.Questions = []application.PendingQuestion{
		pendingQuestion("q1", long, long, long),
	}
	got := questionIntent(snap, snap.Attention.Questions[0], "u")
	detail := strings.TrimPrefix(got.Body, "构建服务\n")
	joined := strings.Repeat("问", 200) + "\n" + strings.Repeat("问", 200) + "\n" + strings.Repeat("问", 200)
	want := string([]rune(joined)[:500])
	if detail != want {
		t.Fatalf("detail = %d runes, want 500-rune prefix of joined fields (got %q…)", len([]rune(detail)), detail[:50])
	}
	// 单字段截断：仅一条超长提问 → 200。
	snap.Attention.Questions = []application.PendingQuestion{pendingQuestion("q1", long)}
	got = questionIntent(snap, snap.Attention.Questions[0], "u")
	if detail := strings.TrimPrefix(got.Body, "构建服务\n"); detail != strings.Repeat("问", 200) {
		t.Fatalf("single field detail = %d runes, want 200", len([]rune(detail)))
	}
}

// TestQuestionIntent_BlankDetailFallback B14：whitespace-only 提问 → 固定降级
// 文案，MUST NOT 出现空白详情（spec「通知内容与跳转链接」）；混合场景空白
// 字段被省略（仅保留有效提问）。
func TestQuestionIntent_BlankDetailFallback(t *testing.T) {
	snap := contentSnap()
	// 全部提问为空白：整体降级。
	snap.Attention.Questions = []application.PendingQuestion{
		pendingQuestion("q1", "   "),
		pendingQuestion("q2", "\n\t "),
	}
	got := questionIntent(snap, snap.Attention.Questions[0], "u")
	if want := composeBody("构建服务", questionDetailFallback); got.Body != want {
		t.Fatalf("all-blank questions must use fallback, body = %q, want %q", got.Body, want)
	}
	// 混合：同一请求内空白提问省略，仅保留有效内容。
	snap.Attention.Questions = []application.PendingQuestion{
		pendingQuestion("q1", "   ", "有效提问"),
	}
	got = questionIntent(snap, snap.Attention.Questions[0], "u")
	if want := composeBody("构建服务", "有效提问"); got.Body != want {
		t.Fatalf("blank field must be omitted, body = %q, want %q", got.Body, want)
	}
}

// TestPermissionIntent_BlankDetailFallback B14：whitespace-only 权限名且无
// patterns → 固定降级文案；空白权限名有 patterns → 省略权限名仅保留 patterns；
// 空白 patterns 条目省略。
func TestPermissionIntent_BlankDetailFallback(t *testing.T) {
	snap := contentSnap()
	// 空白权限名 + 无 patterns：整体降级。
	snap.Attention.Permissions = []application.PendingPermission{pendingPermission("p1", "  ")}
	if got := permissionIntent(snap, snap.Attention.Permissions[0], "u"); got.Body != composeBody("构建服务", permissionDetailFallback) {
		t.Fatalf("blank permission without patterns must use fallback, body = %q", got.Body)
	}
	// 空白权限名 + 有效 patterns：省略权限名（不含悬置冒号）。
	snap.Attention.Permissions = []application.PendingPermission{pendingPermission("p1", " ", "rm -rf /tmp/x")}
	if got := permissionIntent(snap, snap.Attention.Permissions[0], "u"); got.Body != composeBody("构建服务", "rm -rf /tmp/x") {
		t.Fatalf("blank name must be omitted with patterns kept, body = %q", got.Body)
	}
	// 空白 patterns 条目省略。
	snap.Attention.Permissions = []application.PendingPermission{pendingPermission("p1", "bash", "  ", "git status")}
	if got := permissionIntent(snap, snap.Attention.Permissions[0], "u"); got.Body != composeBody("构建服务", "bash: git status") {
		t.Fatalf("blank pattern must be omitted, body = %q", got.Body)
	}
}

func TestPermissionIntent(t *testing.T) {
	snap := contentSnap()
	snap.Attention.Permissions = []application.PendingPermission{
		pendingPermission("p1", "bash", "rm -rf /tmp/x", "git status"),
	}
	got := permissionIntent(snap, snap.Attention.Permissions[0], "u")
	if got.Title != "等待权限确认" {
		t.Fatalf("title = %q", got.Title)
	}
	if want := "构建服务\nbash: rm -rf /tmp/x, git status"; got.Body != want {
		t.Fatalf("body = %q, want %q", got.Body, want)
	}
	if got.Category != notification.CategoryPermission || got.Level != notification.LevelTimeSensitive {
		t.Fatalf("intent = %+v", got)
	}
}

// TestPermissionIntent_EmptyPatterns patterns 为空数组时详情仍非空白（权限名兜底）。
func TestPermissionIntent_EmptyPatterns(t *testing.T) {
	snap := contentSnap()
	snap.Attention.Permissions = []application.PendingPermission{pendingPermission("p1", "edit")}
	if got := permissionIntent(snap, snap.Attention.Permissions[0], "u"); got.Body != "构建服务\nedit" {
		t.Fatalf("body = %q", got.Body)
	}
}

func TestErrorIntent(t *testing.T) {
	snap := contentSnap()
	code := 429
	last := ocdeckevent.ServeRuntimeSessionErrorPayload{Message: "rate limit", StatusCode: &code}
	got := errorIntent(snap, last, "u")
	if got.Title != "运行出错" || got.Level != notification.LevelCritical {
		t.Fatalf("intent = %+v", got)
	}
	if want := "构建服务\nrate limit (HTTP 429)"; got.Body != want {
		t.Fatalf("body = %q, want %q", got.Body, want)
	}
	// statusCode 缺失：省略该字段（不出现空括号）。
	got = errorIntent(snap, ocdeckevent.ServeRuntimeSessionErrorPayload{Message: "boom"}, "u")
	if got.Body != "构建服务\nboom" {
		t.Fatalf("body without code = %q", got.Body)
	}
}

func TestRetryIntent(t *testing.T) {
	snap := contentSnap()
	snap.HasRetryDetail = true
	snap.RetryDetail = RetryDetail{Attempt: 3, Message: "API 超时", Next: 123}
	got := retryIntent(snap, "u")
	if got.Title != "重试未恢复" || got.Body != "构建服务\n第 3 次重试：API 超时" {
		t.Fatalf("intent = %+v", got)
	}
	// 详情不可得：固定降级文案，不放弃投递（spec「通知触发——重试持续未恢复」）。
	snap.HasRetryDetail = false
	got = retryIntent(snap, "u")
	if got.Body != "构建服务\n任务持续处于重试状态" {
		t.Fatalf("fallback body = %q", got.Body)
	}
}

func TestIdleIntent(t *testing.T) {
	got := idleIntent(contentSnap(), 60, "u")
	if got.Title != "任务已空闲" || got.Body != "构建服务\n已空闲超过 60 秒" {
		t.Fatalf("intent = %+v", got)
	}
}

// TestTestIntent 测试通知专用变体（spec「测试通知」：TaskID=notification-test、
// 任务名 ocdeck、固定文案、设置页深链由调用方传入）。
func TestTestIntent(t *testing.T) {
	got := TestIntent("http://x/#/configs#notifications")
	if got.TaskID != "notification-test" || got.TaskName != "ocdeck" ||
		got.Category != notification.CategoryTest || got.Level != notification.LevelActive ||
		got.Title != "测试通知" || got.Body != "ocdeck 通知链路测试" ||
		got.URL != "http://x/#/configs#notifications" {
		t.Fatalf("intent = %+v", got)
	}
}

// TestURLBuilders URL 拼接（design D8）：base 为已剔除尾部 '/' 的值（剔除逻辑
// 见 resolveBase），真实类别任务页深链、test 设置页深链。
func TestURLBuilders(t *testing.T) {
	if got := taskURL("http://h:1", "t9"); got != "http://h:1/#/task/t9" {
		t.Fatalf("taskURL = %q", got)
	}
	if got := testURL("http://h:1"); got != "http://h:1/#/configs#notifications" {
		t.Fatalf("testURL = %q", got)
	}
}

// TestResolveBase base 推导顺序（design D8）：配置 base_url 非空 → 剔除尾部 '/'
// 直接使用；否则 resolver 从监听地址推导；两者皆不可用 → error（门禁 URL 复验失败）。
func TestResolveBase(t *testing.T) {
	cfg := testConfig()
	n := newTestNotifier(nil, nil, nil, nil, func(configured string) (string, error) {
		if configured != "" {
			t.Fatalf("resolver must not be consulted when base_url configured, got %q", configured)
		}
		return "http://127.0.0.1:9000", nil
	}, newFakeClock())

	cfg.BaseURL = "https://example.com/"
	if got, err := n.resolveBase(cfg); err != nil || got != "https://example.com" {
		t.Fatalf("configured base = %q err=%v", got, err)
	}
	cfg.BaseURL = ""
	if got, err := n.resolveBase(cfg); err != nil || got != "http://127.0.0.1:9000" {
		t.Fatalf("resolver base = %q err=%v", got, err)
	}
	// resolver 失败 → URL 不可用（error）。
	n2 := newTestNotifier(nil, nil, nil, nil, func(string) (string, error) {
		return "", errNotFound
	}, newFakeClock())
	if _, err := n2.resolveBase(cfg); err == nil {
		t.Fatal("resolver failure must surface as error")
	}
}
