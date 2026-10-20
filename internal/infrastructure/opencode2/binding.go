// binding.go 实现 v2 私有 binding data（v2BindingData）的 fail-closed 编解码
// （opencode-v2-provider design D2）：全部字段 required；origin 值域 created/
// adopted（生命周期 binding）/ephemeral（仅内存临时 binding，独立结构校验、
// 禁止持久化）；auth.kind 唯一合法值 derived。校验失败 MUST 先于凭据供给/
// 网络/进程任何副作用。
package opencode2

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	appagent "ocdeck/internal/application/agentprovider"
)

// bindingOrigin 是 v2BindingData.origin 值域（D2：恢复配置策略的识别事实，
// MUST NOT 仅凭 binding.ProviderID 判断）。
type bindingOrigin string

const (
	// originCreated：session 由 ocdeck 首建（B 首建 / v2 钉扎新任务）。
	originCreated bindingOrigin = "created"
	// originAdopted：承接 A 延续的原 session（后续恢复持续禁止覆盖其配置）。
	originAdopted bindingOrigin = "adopted"
	// originEphemeral：AcquireEphemeral 仅内存临时 binding（独立结构校验、
	// MUST NOT Encode/持久化）。
	originEphemeral bindingOrigin = "ephemeral"
)

// v2BindingData 是 TaskBinding.Data 的 v2 私有形状（design D2 契约）：
//
//	{ "port": 4096, "directory": "/abs/worktree", "taskId": "task_xxx",
//	  "origin": "created", "auth": { "kind": "derived" } }
//
// 密码不入 data（凭据经派生原语现场计算；同一全局 server 全部任务共用同一
// 凭据，派生标识 = runtime 实例标识 default，见 provider.go derivePassword）。
type v2BindingData struct {
	Port      int           `json:"port"`
	Directory string        `json:"directory"`
	TaskID    string        `json:"taskId"`
	Origin    bindingOrigin `json:"origin"`
	Auth      v2BindingAuth `json:"auth"`
}

type v2BindingAuth struct {
	Kind string `json:"kind"`
}

// decodeV2BindingData 解码生命周期 binding 的私有 data（fail-closed）：
// 畸形 JSON / 缺字段 / 端口越界 / 相对路径 directory / 未知 origin /
// origin=ephemeral（临时形态禁止经 envelope 解码——它 MUST NOT 持久化）/
// 未知 auth.kind 一律拒绝。
func decodeV2BindingData(raw json.RawMessage) (v2BindingData, error) {
	var data v2BindingData
	if len(raw) == 0 {
		return v2BindingData{}, fmt.Errorf("opencode2: binding data 为空")
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return v2BindingData{}, fmt.Errorf("opencode2: decode v2 binding data: %w", err)
	}
	switch {
	case data.Port < 1 || data.Port > 65535:
		return v2BindingData{}, fmt.Errorf("opencode2: binding port %d 越界", data.Port)
	case data.Directory == "" || !filepath.IsAbs(data.Directory):
		return v2BindingData{}, fmt.Errorf("opencode2: binding directory %q 非绝对路径", data.Directory)
	case data.TaskID == "":
		return v2BindingData{}, fmt.Errorf("opencode2: binding 缺少 taskId")
	case data.Origin == originEphemeral:
		return v2BindingData{}, fmt.Errorf("opencode2: origin=ephemeral 禁止出现在持久化 binding（仅内存临时形态）")
	case data.Origin != originCreated && data.Origin != originAdopted:
		return v2BindingData{}, fmt.Errorf("opencode2: 未知 binding origin %q", data.Origin)
	case data.Auth.Kind != appagent.DerivedCredKind:
		return v2BindingData{}, fmt.Errorf("opencode2: 未知 auth.kind %q（唯一合法值 derived）", data.Auth.Kind)
	}
	return data, nil
}

// validateEphemeralBindingData 校验仅内存临时 binding 的私有 data（D3
// AcquireEphemeral 专用结构校验，与生命周期解码分立；调用方 MUST NOT 将该
// 形态 Encode/持久化）。
func validateEphemeralBindingData(data v2BindingData) error {
	switch {
	case data.Port < 1 || data.Port > 65535:
		return fmt.Errorf("opencode2: ephemeral binding port %d 越界", data.Port)
	case data.Directory == "" || !filepath.IsAbs(data.Directory):
		return fmt.Errorf("opencode2: ephemeral binding directory %q 非绝对路径", data.Directory)
	case data.TaskID == "":
		return fmt.Errorf("opencode2: ephemeral binding 缺少 taskId")
	case data.Origin != originEphemeral:
		return fmt.Errorf("opencode2: ephemeral binding origin 必须为 ephemeral，实际 %q", data.Origin)
	case data.Auth.Kind != appagent.DerivedCredKind:
		return fmt.Errorf("opencode2: ephemeral binding 未知 auth.kind %q", data.Auth.Kind)
	}
	return nil
}

// encodeV2BindingData 序列化生命周期 binding 的私有 data（写侧对偶；编码前
// 复走同一校验，不完整 data 不得进入 envelope）。
func encodeV2BindingData(data v2BindingData) (json.RawMessage, error) {
	if _, err := decodeV2BindingData(mustMarshal(data)); err != nil {
		return nil, err
	}
	return mustMarshal(data), nil
}

func mustMarshal(v interface{}) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("opencode2: marshal binding data: %v", err))
	}
	return raw
}

// v2TerminalRef 是 v2 binding envelope 的 TerminalRef 取值（D2）：交互 TUI
// 托管单元标识，从任务身份确定性派生；TUI 延迟创建不影响引用稳定。
func v2TerminalRef(taskID string) string {
	return "ocdeck-v2-tui-" + taskID
}
