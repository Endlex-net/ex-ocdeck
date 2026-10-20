// attention_test.go 落实 tasks 4.2：AttentionIO 三方法与违约透出——session 级
// permission 列表、form 族 question 列表（pending 过滤）、reply 双端点错误
// 映射（404 NotFound / 409 Conflict / 4xx 透出不得当作无 pending）。
package opencode2

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

func TestAttention_ListPermissions_OK(t *testing.T) {
	var gotQuery string
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/ses_fixture01a/permission" {
			t.Errorf("path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		gotQuery = r.URL.Query().Get("directory")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "permission_pending.json"))
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	list, err := p.Attention().ListPermissions(context.Background(), testBinding(t), testSessionKey())
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	// pending 仅存于 session 级端点 + directory query（D7 冻结形态）。
	if gotQuery != "/wt" {
		t.Fatalf("directory query: %q", gotQuery)
	}
	if len(list) != 1 {
		t.Fatalf("list: %v", list)
	}
	pr := list[0]
	if pr.ID != "per_fixture01" || pr.Permission != "shell" || len(pr.Patterns) != 1 || pr.Patterns[0] != "touch /tmp/fixture-marker" {
		t.Fatalf("payload: %+v", pr)
	}
}

func TestAttention_ListPermissions_BreachSurfacesNot(t *testing.T) {
	// 端点 5xx → 如实报错透出能力健康，MUST NOT 折算为无 pending。
	srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a/permission", status: 500, body: "boom"})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	list, err := p.Attention().ListPermissions(context.Background(), testBinding(t), testSessionKey())
	if err == nil {
		t.Fatalf("5xx must surface as error, got %v", list)
	}
	if appagent.KindOf(err) != appagent.ErrKindRuntimeUnavailable {
		t.Fatalf("kind: %v", err)
	}
}

func TestAttention_ListQuestions_PendingFilter(t *testing.T) {
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_fixture01a/form":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "form_list.json"))
		case "/api/session/ses_fixture01a/form/frm_fixture01":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "form_detail_pending.json"))
		case "/api/session/ses_fixture01a/form/frm_fixture02":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "form_detail_answered.json"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	list, err := p.Attention().ListQuestions(context.Background(), testBinding(t), testSessionKey())
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	// pending = Form.Detail.state.status=="pending"（answered form 过滤掉）；
	// 结构化映射（design D10）：原生 key 映射 Fields + Title，不再拼接文本
	//（Questions 兼容视图不填充）。
	if len(list) != 1 || list[0].ID != "frm_fixture01" {
		t.Fatalf("list: %+v", list)
	}
	if list[0].Title != "Deploy options" {
		t.Fatalf("title: %q", list[0].Title)
	}
	if len(list[0].Fields) != 1 || len(list[0].Questions) != 0 {
		t.Fatalf("payload: %+v", list[0])
	}
	f := list[0].Fields[0]
	if f.Key != "env" || f.Label != "Environment" || f.Prompt != "target env" || f.Type != "text" {
		t.Fatalf("field: %+v", f)
	}
}

func TestAttention_Reply_Permission_OK(t *testing.T) {
	var body map[string]interface{}
	var gotQuery string
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/session/ses_fixture01a/permission/per_fixture01/reply" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		gotQuery = r.URL.Query().Get("directory")
		body = jsonBody(t, r)
		w.WriteHeader(http.StatusNoContent)
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	at := domain.Attention{ID: "per_fixture01", Kind: domain.AttentionPermission, Session: testSessionKey()}
	if err := p.Attention().Reply(context.Background(), testBinding(t), at, appagent.AttentionReply{Action: "once"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// body {decision}（204；v2 冻结枚举含 always）；query 形态 = D7 冻结的
	// ?directory=<d>（G2-F4）。
	if body["decision"] != "once" {
		t.Fatalf("body: %v", body)
	}
	if gotQuery != "/wt" {
		t.Fatalf("reply directory query: %q", gotQuery)
	}
}

func TestAttention_Reply_Permission_GhostIDNotFound(t *testing.T) {
	// 重复回复/幽灵 per id → 404 _tag=PermissionNotFoundError → ErrKindNotFound。
	srv := fixtureServer(t, route{
		path:   "/api/session/ses_fixture01a/permission/per_gone/reply",
		status: 404,
		body:   string(readFixture(t, "error_permission_gone.json"))})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	at := domain.Attention{ID: "per_gone", Kind: domain.AttentionPermission, Session: testSessionKey()}
	err := p.Attention().Reply(context.Background(), testBinding(t), at, appagent.AttentionReply{Action: "reject"})
	if appagent.KindOf(err) != appagent.ErrKindNotFound {
		t.Fatalf("kind: %v", err)
	}
}

func TestAttention_Reply_Permission_ActionOutOfRange(t *testing.T) {
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request must not be sent")
		w.WriteHeader(200)
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	at := domain.Attention{ID: "per_fixture01", Kind: domain.AttentionPermission, Session: testSessionKey()}
	if err := p.Attention().Reply(context.Background(), testBinding(t), at, appagent.AttentionReply{Action: "banana"}); err == nil {
		t.Fatalf("out-of-range decision must reject")
	}
}

func TestAttention_Reply_Form_Conflict(t *testing.T) {
	var gotRawQuery string
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_fixture01a/form/frm_fixture01":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "form_detail_pending.json"))
		case "/api/session/ses_fixture01a/form/frm_fixture01/reply":
			// form reply 冻结形态无 directory query（D7：仅路径参数）。
			gotRawQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"_tag":"FormAlreadySettledError","message":"settled"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	at := domain.Attention{ID: "frm_fixture01", Kind: domain.AttentionQuestion, Session: testSessionKey()}
	// 409 FormAlreadySettled → ErrKindConflict（D7 错误映射）。
	err := p.Attention().Reply(context.Background(), testBinding(t), at, appagent.AttentionReply{Answers: []string{"prod"}})
	if appagent.KindOf(err) != appagent.ErrKindConflict {
		t.Fatalf("kind: %v", err)
	}
	if gotRawQuery != "" {
		t.Fatalf("form reply must not carry directory query, got %q", gotRawQuery)
	}
}

func TestAttention_Reply_Form_AnswerKeysFromDetail(t *testing.T) {
	var replyBody map[string]interface{}
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_fixture01a/form/frm_fixture01":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "form_detail_pending.json"))
		case "/api/session/ses_fixture01a/form/frm_fixture01/reply":
			replyBody = jsonBody(t, r)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	at := domain.Attention{ID: "frm_fixture01", Kind: domain.AttentionQuestion, Session: testSessionKey()}
	if err := p.Attention().Reply(context.Background(), testBinding(t), at, appagent.AttentionReply{Answers: []string{"prod"}}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// answer 对象按 detail fields[i].key zip：{answer: {env: "prod"}}。
	ans, _ := replyBody["answer"].(map[string]interface{})
	if ans == nil || ans["env"] != "prod" {
		t.Fatalf("reply body: %v", replyBody)
	}
}

func TestAttention_Reply_Form_MissingAnswers(t *testing.T) {
	// fail-closed（design D10）：空回复在校验阶段拒绝；detail GET（key 映射
	// 依据）允许，reply POST 不得发出（零 wire 副作用）。
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses_fixture01a/form/frm_fixture01":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "form_detail_pending.json"))
		case strings.HasSuffix(r.URL.Path, "/reply"):
			t.Errorf("reply must not be sent on validation failure")
			w.WriteHeader(200)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "form_detail_pending.json"))
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	at := domain.Attention{ID: "frm_fixture01", Kind: domain.AttentionQuestion, Session: testSessionKey()}
	if err := p.Attention().Reply(context.Background(), testBinding(t), at, appagent.AttentionReply{}); err == nil {
		t.Fatalf("empty answers must reject")
	}
}

// TestAttention_ListQuestions_UnknownTypePreserved 落实 tasks 4.6（design D10）：
// normalizeFormType 有限映射（string→text、boolean/number/multiselect 直映、
// enum→select），未知类型（integer/external）保留原值，MUST NOT 静默降级 text；
// Options 按 value/label 映射。
func TestAttention_ListQuestions_UnknownTypePreserved(t *testing.T) {
	detail := `{"data":{"id":"frm_fixture09","sessionID":"ses_fixture01a","title":"Mixed",
		"fields":[
			{"key":"s","type":"string","title":"S","description":"d1"},
			{"key":"b","type":"boolean","title":"B"},
			{"key":"n","type":"number","title":"N"},
			{"key":"e","type":"enum","title":"E","options":[{"value":"a","label":"A"}]},
			{"key":"m","type":"multiselect","title":"M","options":[{"value":"x","label":"X"},{"value":"y","label":"Y"}]},
			{"key":"i","type":"integer","title":"I"},
			{"key":"x","type":"external","title":"X"}
		],
		"state":{"status":"pending"}}}`
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_fixture01a/form":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"frm_fixture09","sessionID":"ses_fixture01a","title":"Mixed","fields":[]}]}`))
		case "/api/session/ses_fixture01a/form/frm_fixture09":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(detail))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	list, err := p.Attention().ListQuestions(context.Background(), testBinding(t), testSessionKey())
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	wantTypes := map[string]string{
		"s": "text", "b": "boolean", "n": "number", "e": "select",
		"m": "multiselect", "i": "integer", "x": "external",
	}
	if len(list[0].Fields) != len(wantTypes) {
		t.Fatalf("fields: %+v", list[0].Fields)
	}
	for _, f := range list[0].Fields {
		if f.Type != wantTypes[f.Key] {
			t.Fatalf("field %s type = %q, want %q", f.Key, f.Type, wantTypes[f.Key])
		}
	}
	// Options 按 value/label 映射（multiselect 双选项）。
	for _, f := range list[0].Fields {
		if f.Key == "m" {
			if len(f.Options) != 2 || f.Options[0].Value != "x" || f.Options[1].Label != "Y" {
				t.Fatalf("options: %+v", f.Options)
			}
		}
	}
}

// TestAttention_Reply_Form_ValuesKeyBasedOrderIndependent 落实 tasks 4.6（design
// D10）：key-based 回复对字段顺序不敏感——同一 Values 在两种字段顺序下产生
// 相同 answer body；Answers 兼容回退按当时字段顺序转换。
func TestAttention_Reply_Form_ValuesKeyBasedOrderIndependent(t *testing.T) {
	detailFor := func(order [2]string) string {
		return `{"data":{"id":"frm_fixture01","sessionID":"ses_fixture01a","title":"F","fields":[
			{"key":"` + order[0] + `","type":"string","title":"A"},
			{"key":"` + order[1] + `","type":"string","title":"B"}],
			"state":{"status":"pending"}}}`
	}
	run := func(t *testing.T, order [2]string, reply appagent.AttentionReply) map[string]interface{} {
		var replyBody map[string]interface{}
		srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/session/ses_fixture01a/form/frm_fixture01":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(detailFor(order)))
			case "/api/session/ses_fixture01a/form/frm_fixture01/reply":
				replyBody = jsonBody(t, r)
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
				w.WriteHeader(404)
			}
		})
		p := newTestProvider(t, srv.URL, ProviderOptions{})
		at := domain.Attention{ID: "frm_fixture01", Kind: domain.AttentionQuestion, Session: testSessionKey()}
		if err := p.Attention().Reply(context.Background(), testBinding(t), at, reply); err != nil {
			t.Fatalf("Reply: %v", err)
		}
		ans, _ := replyBody["answer"].(map[string]interface{})
		return ans
	}

	values := appagent.AttentionReply{Values: []domain.QuestionAnswer{
		{Key: "env", Value: "prod"}, {Key: "note", Value: "hi"},
	}}
	// 字段顺序互换，同一 Values 的 answer 映射不变（key 关联，非位置关联）。
	got1 := run(t, [2]string{"env", "note"}, values)
	got2 := run(t, [2]string{"note", "env"}, values)
	want := map[string]interface{}{"env": "prod", "note": "hi"}
	if !reflect.DeepEqual(got1, want) || !reflect.DeepEqual(got2, want) {
		t.Fatalf("key-based reply order-dependent: %v vs %v", got1, got2)
	}

	// Answers 兼容回退：按当时字段顺序位置转换（顺序变化 → key 归属变化）。
	ansA := run(t, [2]string{"env", "note"}, appagent.AttentionReply{Answers: []string{"prod", "hi"}})
	ansB := run(t, [2]string{"note", "env"}, appagent.AttentionReply{Answers: []string{"prod", "hi"}})
	if ansA["env"] != "prod" || ansA["note"] != "hi" {
		t.Fatalf("answers fallback A: %v", ansA)
	}
	if ansB["note"] != "prod" || ansB["env"] != "hi" {
		t.Fatalf("answers fallback B (position follows field order): %v", ansB)
	}
}

// TestAttention_ListQuestions_UnmodeledFieldsMetadata 落实 G2-F7（design D10）：
// 字段已建模属性之外的属性（hidden/when/format/default/custom 等）原样保留进
// QuestionField.Metadata；已建模键不得泄漏进 Metadata；无未建模属性时
// Metadata 为 nil。消费方继续以结构化字段为主路径。
func TestAttention_ListQuestions_UnmodeledFieldsMetadata(t *testing.T) {
	detail := `{"data":{"id":"frm_fixture10","sessionID":"ses_fixture01a","title":"Rich",
		"fields":[
			{"key":"email","type":"string","title":"Email","description":"your email",
			 "hidden":false,"when":[{"field":"x"}],"format":"email","default":"a@b.c","custom":true},
			{"key":"plain","type":"boolean","title":"Plain"}
		],
		"state":{"status":"pending"}}}`
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_fixture01a/form":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"frm_fixture10","sessionID":"ses_fixture01a","title":"Rich","fields":[]}]}`))
		case "/api/session/ses_fixture01a/form/frm_fixture10":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(detail))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	list, err := p.Attention().ListQuestions(context.Background(), testBinding(t), testSessionKey())
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(list) != 1 || len(list[0].Fields) != 2 {
		t.Fatalf("list: %+v", list)
	}
	rich := list[0].Fields[0]
	// 已建模属性照常映射。
	if rich.Key != "email" || rich.Type != "text" || rich.Required {
		t.Fatalf("modeled: %+v", rich)
	}
	// 未建模属性全部保留（键排序确定性输出），已建模键不泄漏。
	want := `{"custom":true,"default":"a@b.c","format":"email","hidden":false,"when":[{"field":"x"}]}`
	if string(rich.Metadata) != want {
		t.Fatalf("metadata = %s, want %s", rich.Metadata, want)
	}
	// 无未建模属性 → Metadata nil。
	if list[0].Fields[1].Metadata != nil {
		t.Fatalf("plain field metadata = %s, want nil", list[0].Fields[1].Metadata)
	}
}

// TestAttention_Reply_UsesMetadataPreservedFields 落实 G2-F7 对偶：保留
// Metadata 不影响 key-based 回复（未建模属性不参与答案关联）。
func TestAttention_Reply_UsesMetadataPreservedFields(t *testing.T) {
	detail := `{"data":{"id":"frm_fixture01","sessionID":"ses_fixture01a","title":"F",
		"fields":[{"key":"env","type":"string","title":"E","hidden":false,"when":[]}],
		"state":{"status":"pending"}}}`
	var replyBody map[string]interface{}
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_fixture01a/form/frm_fixture01":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(detail))
		case "/api/session/ses_fixture01a/form/frm_fixture01/reply":
			replyBody = jsonBody(t, r)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	at := domain.Attention{ID: "frm_fixture01", Kind: domain.AttentionQuestion, Session: testSessionKey()}
	err := p.Attention().Reply(context.Background(), testBinding(t), at,
		appagent.AttentionReply{Values: []domain.QuestionAnswer{{Key: "env", Value: "prod"}}})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	ans, _ := replyBody["answer"].(map[string]interface{})
	if ans == nil || ans["env"] != "prod" {
		t.Fatalf("reply body: %v", replyBody)
	}
}
