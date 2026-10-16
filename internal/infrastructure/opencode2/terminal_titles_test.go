// terminal_titles_test.go 落实 tasks 4.4：TerminalIO 预验证拒绝路径（不存在 →
// NotFound、归属不符 → BindingInvalid）、AttachCommand argv（自动选中机制
// D8-7）；TitleUpdater = PATCH 204。
package opencode2

import (
	"context"
	"net/http"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
)

func TestTerminal_Ensure_OK(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a", status: 200, body: string(readFixture(t, "session_get.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	ref, err := p.Terminal().EnsureTerminal(context.Background(), testBinding(t))
	if err != nil {
		t.Fatalf("EnsureTerminal: %v", err)
	}
	if ref != "ocdeck-v2-tui-task_fixture01" {
		t.Fatalf("terminal ref: %q", ref)
	}
}

func TestTerminal_Ensure_SessionMissing(t *testing.T) {
	// REST 预验证：session 不存在 → ErrKindNotFound，不创建替代 session。
	srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a", status: 404, body: `{"_tag":"SessionNotFoundError"}`})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	_, err := p.Terminal().EnsureTerminal(context.Background(), testBinding(t))
	if appagent.KindOf(err) != appagent.ErrKindNotFound {
		t.Fatalf("kind: %v", err)
	}
}

func TestTerminal_Ensure_OwnershipMismatch(t *testing.T) {
	// 归属不符：session 的 location.directory 与 binding directory 不一致
	//（session_get fixture 的 directory=/wt，binding 声明 /other）。
	srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a", status: 200, body: string(readFixture(t, "session_get.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	b := testBinding(t)
	b.Data = mustEncodeBinding(t, v2BindingData{Port: 1, Directory: "/other", TaskID: "task_fixture01",
		Origin: originCreated, Auth: v2BindingAuth{Kind: "derived"}})
	_, err := p.Terminal().EnsureTerminal(context.Background(), b)
	if appagent.KindOf(err) != appagent.ErrKindBindingInvalid {
		t.Fatalf("kind: %v", err)
	}
}

func TestTerminal_AttachCommand_Argv(t *testing.T) {
	p := newTestProvider(t, "http://unused", ProviderOptions{OpencodeBin: "/usr/local/bin/opencode"})
	argv, err := p.Terminal().AttachCommand(context.Background(), testBinding(t))
	if err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
	// 冻结机制：opencode --server <url> --session <id>（自动选中 binding 的原
	// session；凭据经 env 注入，不入 argv）。
	want := []string{"/usr/local/bin/opencode", "--server", "http://127.0.0.1:1", "--session", "ses_fixture01a"}
	if len(argv) != len(want) {
		t.Fatalf("argv: %v", argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv: got %v want %v", argv, want)
		}
	}
}

func TestTerminal_AttachCommand_MissingUpstream(t *testing.T) {
	p := newTestProvider(t, "http://unused", ProviderOptions{})
	b := testBinding(t)
	b.UpstreamSessionID = "" // 机制失效形态：明确失败，MUST NOT 退化手动定位
	if _, err := p.Terminal().AttachCommand(context.Background(), b); err == nil {
		t.Fatalf("missing upstream session must fail explicitly")
	}
}

func TestTitles_Sync_PATCH204(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]interface{}
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = jsonBody(t, r)
		w.WriteHeader(http.StatusNoContent)
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	if err := p.Titles().Sync(context.Background(), testBinding(t), testSessionKey(), "new-title"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/api/session/ses_fixture01a" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	if gotBody["title"] != "new-title" {
		t.Fatalf("body: %v", gotBody)
	}
}

func TestTitles_Sync_NotFound(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a", status: 404, body: `{"_tag":"SessionNotFoundError"}`})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	err := p.Titles().Sync(context.Background(), testBinding(t), testSessionKey(), "t")
	if appagent.KindOf(err) != appagent.ErrKindNotFound {
		t.Fatalf("kind: %v", err)
	}
}

// mustEncodeBinding 测试辅助：编码 binding data（失败即 Fatal）。
func mustEncodeBinding(t *testing.T, data v2BindingData) []byte {
	t.Helper()
	raw, err := encodeV2BindingData(data)
	if err != nil {
		t.Fatalf("encode binding data: %v", err)
	}
	return raw
}
