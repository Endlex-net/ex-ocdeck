// binding_test.go 落实 tasks 2.4：v2BindingData fail-closed 编解码（畸形/缺
// 字段/未知 origin 或 auth.kind 拒绝）、归属比对（BindingOwnershipChecker）、
// 凭据不落库（密码只经派生注入）。
package opencode2

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
)

func TestBindingData_Roundtrip(t *testing.T) {
	in := testBindingData()
	raw, err := encodeV2BindingData(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeV2BindingData(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("roundtrip mismatch: %+v != %+v", out, in)
	}
}

func TestBindingData_DecodeRejects(t *testing.T) {
	valid := `{"port":4096,"directory":"/abs/wt","taskId":"task_x","origin":"created","auth":{"kind":"derived"}}`
	cases := []struct {
		name string
		raw  string
	}{
		{"malformed json", `{bad`},
		{"empty", ``},
		{"missing port", `{"directory":"/wt","taskId":"t","origin":"created","auth":{"kind":"derived"}}`},
		{"port zero", strings.Replace(valid, `"port":4096`, `"port":0`, 1)},
		{"port over", strings.Replace(valid, `"port":4096`, `"port":65536`, 1)},
		{"relative directory", strings.Replace(valid, `"/abs/wt"`, `"rel/wt"`, 1)},
		{"empty directory", strings.Replace(valid, `"/abs/wt"`, `""`, 1)},
		{"missing taskId", strings.Replace(valid, `"taskId":"task_x",`, ``, 1)},
		{"unknown origin", strings.Replace(valid, `"created"`, `"forked"`, 1)},
		{"ephemeral in persisted", strings.Replace(valid, `"created"`, `"ephemeral"`, 1)},
		{"unknown auth kind", strings.Replace(valid, `"derived"`, `"legacy"`, 1)},
		{"missing auth kind", strings.Replace(valid, `"kind":"derived"`, `"kind":""`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeV2BindingData(json.RawMessage(tc.raw)); err == nil {
				t.Fatalf("expected reject for %s", tc.name)
			}
		})
	}
}

func TestBindingData_EphemeralValidation(t *testing.T) {
	valid := v2BindingData{Port: 4096, Directory: "/wt", TaskID: "t", Origin: originEphemeral,
		Auth: v2BindingAuth{Kind: appagent.DerivedCredKind}}
	if err := validateEphemeralBindingData(valid); err != nil {
		t.Fatalf("valid ephemeral rejected: %v", err)
	}
	wrongOrigin := valid
	wrongOrigin.Origin = originCreated
	if err := validateEphemeralBindingData(wrongOrigin); err == nil {
		t.Fatalf("ephemeral validation must reject non-ephemeral origin")
	}
	wrongKind := valid
	wrongKind.Auth.Kind = "legacy"
	if err := validateEphemeralBindingData(wrongKind); err == nil {
		t.Fatalf("ephemeral validation must reject unknown auth.kind")
	}
	// 生命周期解码拒绝 ephemeral（禁止持久化形态回流）。
	raw, _ := json.Marshal(valid)
	if _, err := decodeV2BindingData(raw); err == nil {
		t.Fatalf("lifecycle decode must reject origin=ephemeral")
	}
}

func TestBinding_TerminalRefDerivation(t *testing.T) {
	if got := v2TerminalRef("task_x"); got != "ocdeck-v2-tui-task_x" {
		t.Fatalf("terminal ref: %q", got)
	}
}

func TestBinding_OwnershipChecker(t *testing.T) {
	setMasterSecret(t) // 不应被消费——归属校验 MUST NOT 派生凭据
	p := NewSPIProvider(ProviderOptions{})
	b := testBinding(t)

	if err := p.CheckBindingOwnership(b, "task_fixture01", "/wt"); err != nil {
		t.Fatalf("ownership match: %v", err)
	}
	if err := p.CheckBindingOwnership(b, "task_other", "/wt"); err == nil {
		t.Fatalf("taskID mismatch must reject")
	}
	if err := p.CheckBindingOwnership(b, "task_fixture01", "/other"); err == nil {
		t.Fatalf("directory mismatch must reject")
	}
	if kind := appagent.KindOf(p.CheckBindingOwnership(b, "task_other", "/wt")); kind != appagent.ErrKindBindingInvalid {
		t.Fatalf("mismatch kind: %v", kind)
	}
	// 解码失败（错命名空间/畸形 data）→ BindingInvalid。
	wrongNS := b
	wrongNS.ProviderID = "opencode-v1"
	if err := p.CheckBindingOwnership(wrongNS, "task_fixture01", "/wt"); err == nil {
		t.Fatalf("wrong namespace must reject")
	}
	corrupt := b
	corrupt.Data = json.RawMessage(`{bad`)
	if err := p.CheckBindingOwnership(corrupt, "task_fixture01", "/wt"); err == nil {
		t.Fatalf("corrupt data must reject")
	}
}

func TestBinding_CredentialNotStored(t *testing.T) {
	setMasterSecret(t)
	raw, err := encodeV2BindingData(testBindingData())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// 凭据不落库：binding data JSON 无 password 字段。
	if strings.Contains(string(raw), "password") {
		t.Fatalf("binding data must not contain password: %s", raw)
	}
	p := NewSPIProvider(ProviderOptions{})
	port, password, dir, err := p.BindingRuntimeEndpoint(testBinding(t))
	if err != nil {
		t.Fatalf("BindingRuntimeEndpoint: %v", err)
	}
	if port != 1 || dir != "/wt" {
		t.Fatalf("endpoint: port=%d dir=%q", port, dir)
	}
	// 密码只经派生注入：派生标识 = runtime 实例标识 default（D2，非 taskId）。
	want := appagent.DeriveRuntimePassword("test-master-secret", ProviderID, ProviderInstanceID)
	if password != want {
		t.Fatalf("password: got %q want derived %q", password, want)
	}
	if password == appagent.DeriveRuntimePassword("test-master-secret", ProviderID, "task_fixture01") {
		t.Fatalf("password must derive from instance id, not taskId")
	}
}

func TestBinding_DecodeWrongNamespace(t *testing.T) {
	b := testBinding(t)
	b.ProviderID = "opencode-v1"
	_, err := decodeV2Binding(b)
	var ae *appagent.Error
	if !errors.As(err, &ae) || ae.Kind != appagent.ErrKindBindingInvalid {
		t.Fatalf("expect BindingInvalid, got %v", err)
	}
}
