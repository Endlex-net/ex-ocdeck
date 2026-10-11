package task

// charx_contract_test.go 落地 opencode-v1-spi-provider 的契约组 fixture
//（tasks.md 1.3 骨架 + 4.x/3.6-3.8 填实）：迁移导入 runner（4.2）、派生凭据
// 接线（4.1）、事实来源切换/删除清理临时绑定（4.3）、registry 分派与未注册
// fail-closed（3.6/specs「Provider registry 分派」）、provider 切换不可达
//（3.7/D9a）、回滚再升级命名空间补齐 task 层变体（2.2/D6）、能力健康透出
// 比较例外口径（3.8/D7）均为真实行为断言。
//
// 契约组与基线组共用 charx harness（fake opencode server + 六观察面快照），
// 保证各段落地时 fixture 无需重建观察基建。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/store"
)

// charxMasterSecret 是 harness 注入的固定测试 master secret（newCharxEnvOnTaskStore
// 经 t.Setenv 注入，D3 env 优先序）。
const charxMasterSecret = "charx-test-master-secret"

// charxV1Data 是迁移导入 runner 写入的 v1 binding data 形状（与 runner 内
// marshal 的 {"port","directory","auth"} 一致；断言用最小结构）。
type charxV1Data struct {
	Port      int    `json:"port"`
	Directory string `json:"directory"`
	Auth      struct {
		Kind     string `json:"kind"`
		Password string `json:"password"`
	} `json:"auth"`
}

// v1AuthSeedShape 是 binding data.auth 的 seed 构造形态。
type v1AuthSeedShape struct {
	Kind     string `json:"kind"`
	Password string `json:"password"`
}

// charxDerivedOf 按测试常量计算任务的派生密码（独立于生产代码路径的期望值）。
func charxDerivedOf(taskID string) string {
	return appagent.DeriveRuntimePassword(charxMasterSecret, store.ProviderIDV1, taskID)
}

// TestCharxContract_DerivedCredentials 派生凭据 spawn 接线（tasks 4.1，design D3）。
//
// 断言面：
//  1. 激活 spawn env 注入 OPENCODE_SERVER_PASSWORD=派生值（32 字符 base64url，
//     与独立计算的 D3 向量比对——非自证）+ OCDECK_CRED_KIND=derived；
//  2. env_snapshot 列原文不含密码/标记（快照为普通环境快照，D3 角色收敛）；
//  3. 同任务跨重启密码恒定（suspend→reactivate 同值——派生切换后不再轮换）。
func TestCharxContract_DerivedCredentials(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	name := runtimeSessionName("t1")
	e.proc.mockProc.mu.Lock()
	env := e.proc.mockProc.envValues[name]
	e.proc.mockProc.mu.Unlock()
	if env == nil {
		t.Fatal("spawn env missing for runtime session")
	}
	pw := env["OPENCODE_SERVER_PASSWORD"]

	// ① 派生值 + 形状（32 字符 base64url 词法）。
	want := charxDerivedOf("t1")
	if pw != want {
		t.Errorf("spawn password = %.8s..., want derived %.8s...", pw, want)
	}
	if len(pw) != 32 {
		t.Errorf("派生长度 = %d, want 32", len(pw))
	}
	if _, err := base64.RawURLEncoding.DecodeString(pw); err != nil {
		t.Errorf("派生值必须为 base64url 无填充词法: %v (%q)", err, pw)
	}
	if env[appagent.CredKindEnvKey] != appagent.DerivedCredKind {
		t.Errorf("OCDECK_CRED_KIND = %q, want %q", env[appagent.CredKindEnvKey], appagent.DerivedCredKind)
	}
	// ② env_snapshot 列原文不含凭据材料。
	row := e.charxTaskRow("t1")
	if row.EnvSnapshot.Valid && row.EnvSnapshot.String != "" {
		if strings.Contains(row.EnvSnapshot.String, pw) || strings.Contains(row.EnvSnapshot.String, appagent.CredKindEnvKey) {
			t.Errorf("env_snapshot 泄露凭据材料：%.60s", row.EnvSnapshot.String)
		}
	} else {
		t.Log("env_snapshot 为空（测试 env 未含宿主变量）——无泄露即满足")
	}
	// ③ 同任务恒定。
	if err := e.m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	e.proc.mockProc.mu.Lock()
	pw2 := e.proc.mockProc.envValues[name]["OPENCODE_SERVER_PASSWORD"]
	e.proc.mockProc.mu.Unlock()
	if pw2 != pw {
		t.Errorf("重启后密码漂移: %.8s... → %.8s...（派生同任务恒定）", pw, pw2)
	}
}

// TestCharxContract_MigrationImportRunner 迁移导入 runner（design D6 阶段 3，
// tasks 4.2 已实现——本 fixture 为行为断言）。
//
// 副作用边界固定：runner 全程不建进程、不删 session、不改锚定与归属
// （proc 调用计数断言）；结论经 last_error / SetTaskBinding 落账。
func TestCharxContract_MigrationImportRunner(t *testing.T) {
	ctx := context.Background()

	// seedImportTask 种子「持久化 active + anchor + runtime 会话 env」候选
	//（不经 Activate：runner 只读 store/proc env，fixture 直接构造现状形态；
	// 无锚定任务单独覆盖为 fail-closed 结论）。
	seedImportTask := func(e *charxEnv, taskID string, anchor string, env map[string]string) {
		wt := "/data/worktrees/p1/" + taskID
		e.store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
		e.store.tasks[taskID] = TaskRow{ID: taskID, ProjectID: "p1", Name: "t", Status: StatusActive,
			WorktreePath: wt, AnchorSessionID: sql.NullString{String: anchor, Valid: anchor != ""}}
		if anchor != "" {
			e.srv.seedSession(wt, anchor, 1, 1)
		}
		name := runtimeSessionName(taskID)
		e.proc.mockProc.mu.Lock()
		e.proc.mockProc.sessions[name] = true
		e.proc.mockProc.envValues[name] = env
		e.proc.mockProc.mu.Unlock()
	}
	legacyEnv := func(pw string, port int) map[string]string {
		// 旧二进制随机凭据形态：密码/端口在会话 env，无 OCDECK_CRED_KIND 标记。
		return map[string]string{"OPENCODE_SERVER_PASSWORD": pw, "OCDECK_SERVE_PORT": fmt.Sprintf("%d", port)}
	}

	t.Run("legacy_import_then_idempotent_rerun", func(t *testing.T) {
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		seedImportTask(e, "mi-legacy", "ses-anchor-mi-legacy", legacyEnv("legacy-random-pw", e.portHealthy()))
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		raw, ok := e.store.bindingRawOf("mi-legacy")
		if !ok {
			t.Fatal("binding must be imported")
		}
		env, err := domain.DecodeTaskBinding(raw)
		if err != nil {
			t.Fatalf("binding envelope: %v", err)
		}
		var d charxV1Data
		if err := json.Unmarshal(env.Data, &d); err != nil {
			t.Fatalf("binding data: %v (%s)", err, env.Data)
		}
		if d.Auth.Kind != appagent.LegacyCredKind || d.Auth.Password != "legacy-random-pw" ||
			d.Port != e.portHealthy() || d.Directory != "/data/worktrees/p1/mi-legacy" {
			t.Errorf("binding data = %+v (raw %s)", d, raw)
		}
		b, _ := e.store.rawBindingOf("mi-legacy")
		if b.UpstreamSessionID != "ses-anchor-mi-legacy" || b.TerminalRef != runtimeSessionName("mi-legacy") {
			t.Errorf("binding envelope = %+v", b)
		}
		// 幂等重跑：健康验证通过 → 保留（binding 原文不变、last_error 空）。
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("rerun: %v", err)
		}
		raw2, _ := e.store.bindingRawOf("mi-legacy")
		if raw2 != raw {
			t.Errorf("幂等重跑改写 binding：%q → %q", raw, raw2)
		}
		if r := e.charxTaskRow("mi-legacy"); r.LastError.Valid && r.LastError.String != "" {
			t.Errorf("last_error = %q, want empty", r.LastError.String)
		}
		// 副作用边界：零进程调用（不建进程/不删 session）。
		if calls := e.proc.calls(); len(calls) != 0 {
			t.Errorf("proc calls = %q, want none", calls)
		}
	})

	t.Run("derived_runtime_key_change_no_read_no_write", func(t *testing.T) {
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		// 密钥改变场景：derived 标记 runtime + binding 指向死端口（健康必败）——
		// 禁读取/导入/落库；不修改既有 binding。
		seedImportTask(e, "mi-derived", "ses-anchor-mi-derived", map[string]string{
			"OPENCODE_SERVER_PASSWORD": "some-actual-password",
			appagent.CredKindEnvKey:    appagent.DerivedCredKind,
			"OCDECK_SERVE_PORT":        "1",
		})
		// 既有 derived binding（陈旧派生值指向死端口）——健康失败触发标记分支。
		data, _ := json.Marshal(struct {
			Port      int    `json:"port"`
			Directory string `json:"directory"`
			TaskID    string `json:"taskId"`
			Auth      struct {
				Kind string `json:"kind"`
			} `json:"auth"`
		}{Port: 1, Directory: "/data/worktrees/p1/mi-derived", TaskID: "mi-derived",
			Auth: struct {
				Kind string `json:"kind"`
			}{Kind: appagent.DerivedCredKind}})
		stale := domain.TaskBinding{Version: domain.TaskBindingVersion, ProviderID: store.ProviderIDV1,
			ProviderInstanceID: store.ProviderInstanceV1Default, UpstreamSessionID: "ses-anchor-mi-derived",
			TerminalRef: runtimeSessionName("mi-derived"), Data: data}
		staleRaw := func() string { r, _ := stale.Encode(); return r }()
		if err := e.store.SetTaskBinding(ctx, "mi-derived", stale); err != nil {
			t.Fatalf("seed stale binding: %v", err)
		}
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		raw, _ := e.store.bindingRawOf("mi-derived")
		if raw != staleRaw {
			t.Errorf("binding MUST NOT be modified:\n old: %q\n new: %q", staleRaw, raw)
		}
		if !strings.Contains(e.charxTaskRow("mi-derived").LastError.String, "疑似主密钥或凭据问题") {
			t.Errorf("last_error = %q, want 疑似主密钥或凭据问题", e.charxTaskRow("mi-derived").LastError.String)
		}
	})

	t.Run("defense_derived_value_never_stored", func(t *testing.T) {
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		// 旧二进制形态（无标记）但 env 密码恰等于当前派生值 → MUST NOT 落库
		//（比对仅作防御断言，不作来源判据）；env 端口=fake 映射端口（健康通过，
		// 走到防御比对断言）。
		seedImportTask(e, "mi-defense", "ses-anchor-mi-defense", legacyEnv(charxDerivedOf("mi-defense"), e.portHealthy()))
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		if _, ok := e.store.bindingRawOf("mi-defense"); ok {
			t.Error("derived-value password MUST NOT be stored")
		}
		if !strings.Contains(e.charxTaskRow("mi-defense").LastError.String, "防御断言") {
			t.Errorf("last_error = %q, want 防御断言记录", e.charxTaskRow("mi-defense").LastError.String)
		}
	})

	t.Run("existing_healthy_binding_preserved", func(t *testing.T) {
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		seedImportTask(e, "mi-keep", "ses-anchor-mi-keep", legacyEnv("stale-pw", 1))
		data, _ := json.Marshal(struct {
			Port      int             `json:"port"`
			Directory string          `json:"directory"`
			Auth      v1AuthSeedShape `json:"auth"`
		}{Port: e.portHealthy(), Directory: "/data/worktrees/p1/mi-keep",
			Auth: v1AuthSeedShape{Kind: appagent.LegacyCredKind, Password: "good-pw"}})
		good := domain.TaskBinding{Version: domain.TaskBindingVersion, ProviderID: store.ProviderIDV1,
			ProviderInstanceID: store.ProviderInstanceV1Default, UpstreamSessionID: "ses-anchor-mi-keep",
			TerminalRef: runtimeSessionName("mi-keep"), Data: data}
		if err := e.store.SetTaskBinding(ctx, "mi-keep", good); err != nil {
			t.Fatalf("seed binding: %v", err)
		}
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		raw, _ := e.store.bindingRawOf("mi-keep")
		envB, derr := domain.DecodeTaskBinding(raw)
		if derr != nil {
			t.Fatalf("binding envelope: %v", derr)
		}
		var d charxV1Data
		if err := json.Unmarshal(envB.Data, &d); err != nil {
			t.Fatalf("binding data: %v (%s)", err, envB.Data)
		}
		if d.Port != e.portHealthy() || d.Directory != "/data/worktrees/p1/mi-keep" ||
			d.Auth.Kind != appagent.LegacyCredKind || d.Auth.Password != "good-pw" {
			t.Errorf("healthy binding must be preserved, got %+v (%s)", d, envB.Data)
		}
		if r := e.charxTaskRow("mi-keep"); r.LastError.Valid && r.LastError.String != "" {
			t.Errorf("last_error = %q, want empty", r.LastError.String)
		}
	})

	t.Run("legacy_health_fail_recorded", func(t *testing.T) {
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		seedImportTask(e, "mi-fail", "ses-anchor-mi-fail", legacyEnv("legacy-random-pw", 1))
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		if _, ok := e.store.bindingRawOf("mi-fail"); ok {
			t.Error("failed health check MUST NOT import binding")
		}
		if !strings.Contains(e.charxTaskRow("mi-fail").LastError.String, "健康检查失败") {
			t.Errorf("last_error = %q", e.charxTaskRow("mi-fail").LastError.String)
		}
	})

	t.Run("master_gate_fails_server_readiness", func(t *testing.T) {
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		seedImportTask(e, "mi-gate", "ses-anchor-mi-gate", legacyEnv("pw", e.portHealthy()))
		// 主密钥不可用：runner 整体失败（server 拒绝就绪）；任务零修改、零网络、
		// 候选屏障不释放（结论先于 gate 的任何动作）。
		os.Unsetenv(appagent.MasterSecretEnv)
		t.Cleanup(func() { _ = os.Setenv(appagent.MasterSecretEnv, charxMasterSecret) })
		err := e.m.RunMigrationImport(ctx)
		_ = os.Setenv(appagent.MasterSecretEnv, charxMasterSecret)
		if err == nil || !strings.Contains(err.Error(), "master secret gate") {
			t.Fatalf("gate error = %v, want master secret gate failure", err)
		}
		row := e.charxTaskRow("mi-gate")
		if row.LastError.Valid && row.LastError.String != "" {
			t.Errorf("候选零修改（gate 先于结论），last_error = %q", row.LastError.String)
		}
		if _, ok := e.store.rawBindingOf("mi-gate"); ok {
			t.Error("gate 失败 MUST NOT 写 binding")
		}
	})

	// --- C5 门禁修复 fixtures（design D6 阶段 3 判定链 1→2 分派 + Healthy 字段 +
	// 来源标记显式区分）---

	t.Run("legacy_health_protocol_violation", func(t *testing.T) {
		// C5 残余：legacy 导入路径 herr==nil 但 Healthy=false（协议违约）→ 非验证
		// 通过 → 失败落账不写 binding（与 derived 分支对称）。
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		seedImportTask(e, "mi-legacy-unhealthy", "ses-anchor-mi-legacy-unhealthy", legacyEnv("legacy-random-pw", e.portHealthy()))
		e.srv.setHealthHealthy(false)
		t.Cleanup(func() { e.srv.setHealthHealthy(true) })
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		if _, ok := e.store.bindingRawOf("mi-legacy-unhealthy"); ok {
			t.Error("healthy=false legacy runtime MUST NOT be imported")
		}
		if !strings.Contains(e.charxTaskRow("mi-legacy-unhealthy").LastError.String, "legacy runtime 健康检查失败") {
			t.Errorf("last_error = %q, want legacy 健康失败落账", e.charxTaskRow("mi-legacy-unhealthy").LastError.String)
		}
	})

	t.Run("corrupt_binding_enters_source_marker_branch", func(t *testing.T) {
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		// envelope 解析失败（畸形 task_binding 列值）→ 落 ② 来源判定：legacy runtime
		// 存活且健康 → legacy 导入成功（specs「导入成功建立或替换绑定」）。
		seedImportTask(e, "mi-corrupt", "ses-anchor-mi-corrupt", legacyEnv("legacy-random-pw", e.portHealthy()))
		corruptRaw := `{"version":1,"provider_id":"opencode-v1","data":{"port"`
		e.store.mu.Lock()
		e.store.bindings["mi-corrupt"] = corruptRaw // 直写列值：Encode 不放行畸形 JSON
		e.store.mu.Unlock()
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		raw, _ := e.store.bindingRawOf("mi-corrupt")
		if raw == "" || raw == corruptRaw {
			t.Fatalf("corrupt binding MUST be replaced by legacy import, got %q", raw)
		}
		envB, derr := domain.DecodeTaskBinding(raw)
		if derr != nil {
			t.Fatalf("new binding envelope: %v", derr)
		}
		var d charxV1Data
		if err := json.Unmarshal(envB.Data, &d); err != nil {
			t.Fatalf("new binding data: %v", err)
		}
		if d.Auth.Kind != appagent.LegacyCredKind || d.Port != e.portHealthy() {
			t.Errorf("re-imported binding = %+v, want legacy @ healthy port", d)
		}
	})

	t.Run("binding_read_store_failure_stays_failclosed", func(t *testing.T) {
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		// store 读取失败（行缺失/DB 故障）→ fail-closed 结论，不进入来源判定
		//（C5：数据层不可达时 marker/env 读取无从谈起任务 runtime 事实）。
		seedImportTask(e, "mi-readfail", "ses-anchor-mi-readfail", legacyEnv("legacy-random-pw", e.portHealthy()))
		e.store.mu.Lock()
		e.store.getTaskBindingErr = errors.New("db transient down")
		e.store.mu.Unlock()
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		e.store.mu.Lock()
		e.store.getTaskBindingErr = nil
		e.store.mu.Unlock()
		if _, ok := e.store.bindingRawOf("mi-readfail"); ok {
			t.Error("store read failure MUST NOT write binding")
		}
		if !strings.Contains(e.charxTaskRow("mi-readfail").LastError.String, "binding 读取失败") {
			t.Errorf("last_error = %q, want binding 读取失败", e.charxTaskRow("mi-readfail").LastError.String)
		}
	})

	t.Run("health_protocol_violation_not_preserved", func(t *testing.T) {
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		// Health err==nil 但 Healthy=false（协议违约）→ 非「验证通过」→ 落来源
		// 判定（C5：仅检查 err==nil 会把 healthy=false 当保留结论）。
		seedImportTask(e, "mi-unhealthy", "ses-anchor-mi-unhealthy", legacyEnv("legacy-random-pw", 1))
		data, _ := json.Marshal(struct {
			Port      int             `json:"port"`
			Directory string          `json:"directory"`
			Auth      v1AuthSeedShape `json:"auth"`
		}{Port: e.portHealthy(), Directory: "/data/worktrees/p1/mi-unhealthy",
			Auth: v1AuthSeedShape{Kind: appagent.LegacyCredKind, Password: "stale-pw"}})
		good := domain.TaskBinding{Version: domain.TaskBindingVersion, ProviderID: store.ProviderIDV1,
			ProviderInstanceID: store.ProviderInstanceV1Default, UpstreamSessionID: "ses-anchor-mi-unhealthy",
			TerminalRef: runtimeSessionName("mi-unhealthy"), Data: data}
		seedRaw := func() string { r, _ := good.Encode(); return r }()
		if err := e.store.SetTaskBinding(ctx, "mi-unhealthy", good); err != nil {
			t.Fatalf("seed binding: %v", err)
		}
		// 上游协议违约：/global/health 应答 200 但 healthy=false。
		e.srv.setHealthHealthy(false)
		t.Cleanup(func() { e.srv.setHealthHealthy(true) })
		// 判定链：healthy=false → 非「验证通过」→ 落来源判定——runtime env 无标记
		//（seedImportTask 未置 derived）→ legacy 路径读 env 端口 1（死端口健康失败）
		// → fail-closed 结论，binding 保持原值。
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		raw, _ := e.store.bindingRawOf("mi-unhealthy")
		if raw != seedRaw {
			t.Errorf("unhealthy binding MUST NOT be rewritten:\n old: %q\n new: %q", seedRaw, raw)
		}
		le := e.charxTaskRow("mi-unhealthy").LastError.String
		if !strings.Contains(le, "legacy runtime 健康检查失败") {
			t.Errorf("last_error = %q, want legacy 健康失败落账（healthy=false 走判定链）", le)
		}
	})

	t.Run("marker_read_error_and_unknown_value_failclosed", func(t *testing.T) {
		// 标记读取失败 → fail-closed（不得当作缺失滑入 legacy 导入路径）。
		e := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		seedImportTask(e, "mi-markerr", "ses-anchor-mi-markerr", legacyEnv("legacy-random-pw", e.portHealthy()))
		e.proc.mockProc.mu.Lock()
		e.proc.mockProc.showEnvErr = errors.New("tmux show-environment down")
		e.proc.mockProc.mu.Unlock()
		if err := e.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		e.proc.mockProc.mu.Lock()
		e.proc.mockProc.showEnvErr = nil
		e.proc.mockProc.mu.Unlock()
		if _, ok := e.store.bindingRawOf("mi-markerr"); ok {
			t.Error("marker read failure MUST NOT import binding")
		}
		if !strings.Contains(e.charxTaskRow("mi-markerr").LastError.String, "来源标记读取失败") {
			t.Errorf("last_error = %q, want 来源标记读取失败", e.charxTaskRow("mi-markerr").LastError.String)
		}

		// 未知标记值 → fail-closed 不读密码（不写 binding；密码值不被读取使用）。
		e2 := newCharxEnv(t, charxEnvOpts{deadPorts: true})
		seedImportTask(e2, "mi-markunk", "ses-anchor-mi-markunk", map[string]string{
			"OPENCODE_SERVER_PASSWORD": "legacy-random-pw",
			appagent.CredKindEnvKey:    "mutated-future-kind",
			"OCDECK_SERVE_PORT":        fmt.Sprintf("%d", e2.portHealthy()),
		})
		if err := e2.m.RunMigrationImport(ctx); err != nil {
			t.Fatalf("RunMigrationImport: %v", err)
		}
		if _, ok := e2.store.bindingRawOf("mi-markunk"); ok {
			t.Error("unknown marker MUST NOT import binding")
		}
		if !strings.Contains(e2.charxTaskRow("mi-markunk").LastError.String, "未知来源标记") {
			t.Errorf("last_error = %q, want 未知来源标记 fail-closed", e2.charxTaskRow("mi-markunk").LastError.String)
		}
	})
}

// TestCharxContract_RegistryDispatchAndUnregisteredFailClosed provider registry
// 分派与未注册 fail-closed（design D5 / specs「Provider registry 分派」两个
// scenario：消费点统一分派 + 未注册 fail-closed；生产装配形态 cmd 侧
// TestRegistryDispatchFactory_RegisteredAndFailClosed 已直测，本 fixture 为
// task 层生命周期级端到端变体——4.3 事实来源切换后 runtime 连接一律经
// resolveRuntimeBinding（binding → registryDispatch → data）解析）。
//
// 断言面：
//  1. 正常分派：激活后 taskOcClient 经持久化 binding 构造指向任务 runtime 的
//     客户端（ok=true、directory=WorktreePath；Health 打到 fake server 且
//     request 记录 +1——client 由 binding 端点分派产生，非硬编码）；
//  2. 未注册 fail-closed：binding.ProviderID 篡改为未注册目标 →
//     resolveRuntimeBinding 报 typed ErrProviderNotFound（errors.Is sentinel）；
//  3. 生命周期 fail-closed：active 任务换未注册 binding → Reconcile 落
//     suspended + last_error 非空；零 NewSession、零新网络请求；绑定原文、
//     锚定与归属行零修改（specs THEN：不产生任何进程/网络副作用，不修改
//     归属、锚定、绑定与任务状态——last_error 属允许的诊断性持久化）。
//
// mutation 证据（已验证 → 红 → 还原）：删除 resolveRuntimeBinding 中的
// registryDispatch 调用（分派旁路）→ ② 变红（err 为 nil/BindingInvalid 而非
// provider_not_found）；正常路径 ① 仍绿（分派旁路不影响已注册形态）。
func TestCharxContract_RegistryDispatchAndUnregisteredFailClosed(t *testing.T) {
	ctx := context.Background()
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	assertStatus(t, e.store, "t1", StatusActive)
	// 正常分派前置：激活提交持久化 binding（CommitActivation 事务）。
	if raw, ok := e.store.bindingRawOf("t1"); !ok {
		t.Fatal("activation must persist binding (D9 提交协议)")
	} else if b, err := domain.DecodeTaskBinding(raw); err != nil || b.ProviderID != store.ProviderIDV1 {
		t.Fatalf("binding provider = %+v err=%v, want %q", b, err, store.ProviderIDV1)
	}

	// ① 正常分派：taskOcClient 经 binding 构造 client，Health 打到 fake server。
	oc, dir, ok := e.m.taskOcClient(ctx, "t1")
	if !ok || oc == nil {
		t.Fatalf("taskOcClient ok=%v (oc=%v), want true（provider 已注册分派成功）", ok, oc)
	}
	if dir != "/data/worktrees/p1/t1" {
		t.Errorf("directory = %q, want binding data.directory 同值", dir)
	}
	before := len(e.srv.requests())
	if _, err := oc.Health(ctx); err != nil {
		t.Fatalf("Health via dispatched client: %v", err)
	}
	after := e.srv.requests()
	if len(after) != before+1 {
		t.Fatalf("requests delta = %d, want 1（client 必须真实指向 fake server）", len(after)-before)
	}
	if last := after[len(after)-1]; !strings.Contains(last, "GET /global/health") {
		t.Errorf("last request = %q, want GET /global/health", last)
	}

	// ② 未注册 fail-closed：binding.ProviderID → 未注册目标（唯一可达的
	// 未注册形态——本期 registry 仅 opencode-v1，删除清理临时绑定分支②
	// 固定按 v1 分派不属本 scenario）。篡改仅声明字段，其余保持原值。
	raw, _ := e.store.bindingRawOf("t1")
	var envP struct {
		Version            int             `json:"version"`
		ProviderID         string          `json:"provider_id"`
		ProviderInstanceID string          `json:"provider_instance_id"`
		UpstreamSessionID  string          `json:"upstream_session_id"`
		TerminalRef        string          `json:"terminal_ref"`
		Data               json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &envP); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	envP.ProviderID = "another-v2"
	tampered, _ := json.Marshal(envP)
	tamperedRaw := string(tampered)
	if err := e.store.SetTaskBinding(ctx, "t1", mustDecodeBinding(t, tamperedRaw)); err != nil {
		t.Fatalf("seed unregistered provider binding: %v", err)
	}
	_, _, errSD := e.m.resolveRuntimeBinding(ctx, "t1")
	if errSD == nil || !errors.Is(errSD, appagent.ErrProviderNotFound) {
		t.Fatalf("resolveRuntimeBinding err = %v, want typed provider_not_found", errSD)
	}
	// ③ 生命周期 fail-closed：watcher 语义外的启动时恢复形态（Reconcile）。
	sessionsBefore := len(e.proc.newSessionNamesSnapshot())
	reqsBefore := len(e.srv.requests())
	bindingBefore := tamperedRaw
	anchorBefore := e.charxTaskRow("t1").AnchorSessionID
	ownedBefore := e.charxOwnedSessions("t1")
	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// fail-closed：resume 前置（binding 经 registry 可分派）失守 → suspended。
	assertStatus(t, e.store, "t1", StatusSuspended)
	row := e.charxTaskRow("t1")
	if !row.LastError.Valid || row.LastError.String == "" {
		t.Errorf("last_error = %+v, want 恢复失败标注（诊断性持久化为允许副作用）", row.LastError)
	}
	// 零进程/网络副作用。
	if n := len(e.proc.newSessionNamesSnapshot()); n != sessionsBefore {
		t.Errorf("NewSession delta = %d, want 0（未注册不得触发重拉/临时进程）", n-sessionsBefore)
	}
	if n := len(e.srv.requests()); n != reqsBefore {
		t.Errorf("network requests delta = %d, want 0（未注册零网络副作用）", n-reqsBefore)
	}
	// 归属、锚定、绑定零修改（reconcile 只读 binding，不重写）。
	if rawAfter, _ := e.store.bindingRawOf("t1"); rawAfter != bindingBefore {
		t.Errorf("binding rewritten during fail-closed recovery: %q → %q", bindingBefore, rawAfter)
	}
	if anchorAfter := e.charxTaskRow("t1").AnchorSessionID; anchorAfter != anchorBefore {
		t.Errorf("anchor rewritten: %v → %v", anchorBefore, anchorAfter)
	}
	if ownedAfter := e.charxOwnedSessions("t1"); len(ownedAfter) != len(ownedBefore) {
		t.Errorf("ownership changed: %v → %v", ownedBefore, ownedAfter)
	}
	_ = e
}

// TestCharxContract_ProviderSwitchUnreachableFailClosed provider 切换入口
// 不可达 fail-closed（design D9a：本期 registry 仅 opencode-v1——specs
// 「Provider 身份模型」的切换语义为未来契约：第二 provider、归属行多元命名空间、
// 跨 provider session 迁移协议均未落地，不存在切换 API 入口）。
//
// 本期唯一可达形态＝目标 provider 声明已持久化但 registry 无实现。断言面
// （spec「Provider 身份模型」/「切换失败保持原状」的可达子集）：
//  1. 装配面：registry 仅注册 opencode-v1（Get(v1) 成功；Get(其他)→typed
//     ErrProviderNotFound）——切换目标不可达即装配事实；
//  2. 激活产生的 provider 标识恒为 opencode-v1（切换路径不存在 → 不可能
//     产生第二个 provider 的任务声明）；
//  3. 目标声明形态（binding.ProviderID=未注册目标）：恢复消费点 fail-closed
//     挂起，任务 provider 标识（binding 原文）、归属、锚定均保持原值
//     （provider 标识、归属、锚定与绑定保持原状；last_error 属允许的
//     诊断性持久化）。
//
// 与 TestCharxContract_RegistryDispatchAndUnregisteredFailClosed 的分工：彼处
// 断言分派正常路径 + 未注册在分派层的 typed sentinel 与零副作用；本处断言
// 「切换」视角——目标声明持久化后的任务级原状保持（binding/锚定/归属/状态）。
//
// mutation 证据（已验证 → 红 → 还原）：恢复路径把 commitSuspendedReconcile
// 换成清 binding 后挂起（模拟切换失败误清绑定）→ binding 原文断言变红。
func TestCharxContract_ProviderSwitchUnreachableFailClosed(t *testing.T) {
	ctx := context.Background()
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// ① 装配面：仅 opencode-v1 可达。
	if _, err := e.m.providerRegistry.Get(store.ProviderIDV1); err != nil {
		t.Fatalf("registry.Get(opencode-v1) = %v, want registered", err)
	}
	_, errOther := e.m.providerRegistry.Get("another-v2")
	if errOther == nil || !errors.Is(errOther, appagent.ErrProviderNotFound) {
		t.Fatalf("registry.Get(another-v2) = %v, want typed provider_not_found（切换目标不可达）", errOther)
	}
	// ② 激活 provider 标识恒为 opencode-v1。
	raw, ok := e.store.bindingRawOf("t1")
	if !ok {
		t.Fatal("activation must persist binding")
	}
	b, derr := domain.DecodeTaskBinding(raw)
	if derr != nil || b.ProviderID != store.ProviderIDV1 {
		t.Fatalf("activation binding provider = %+v (err=%v), want %q（无切换入口即无第二 provider 声明）", b, derr, store.ProviderIDV1)
	}

	// ③ 目标声明形态：binding.ProviderID 篡改为未注册目标（切换声明唯一的
	// 可达持久化形态）→ 恢复消费点 fail-closed + 原状保持。
	var envP struct {
		Version            int             `json:"version"`
		ProviderID         string          `json:"provider_id"`
		ProviderInstanceID string          `json:"provider_instance_id"`
		UpstreamSessionID  string          `json:"upstream_session_id"`
		TerminalRef        string          `json:"terminal_ref"`
		Data               json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &envP); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	envP.ProviderID = "another-v2"
	switchDeclared, _ := json.Marshal(envP)
	declaredRaw := string(switchDeclared)
	if err := e.store.SetTaskBinding(ctx, "t1", mustDecodeBinding(t, declaredRaw)); err != nil {
		t.Fatalf("seed switch-target declared binding: %v", err)
	}
	anchorBefore := e.charxTaskRow("t1").AnchorSessionID
	ownedBefore := e.charxOwnedSessions("t1")
	newsBefore := len(e.proc.newSessionNamesSnapshot()) // 激活期双启动已计入
	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, e.store, "t1", StatusSuspended)
	row := e.charxTaskRow("t1")
	if !row.LastError.Valid || row.LastError.String == "" {
		t.Errorf("last_error = %+v, want 恢复失败标注", row.LastError)
	}
	// 原状保持：任务 provider 标识（binding 原文）不得被消费路径改写/清除。
	if rawAfter, _ := e.store.bindingRawOf("t1"); rawAfter != declaredRaw {
		t.Errorf("binding (provider 标识) rewritten on failed switch: %q → %q", declaredRaw, rawAfter)
	}
	// 锚定与归属保持原值。
	if anchorAfter := e.charxTaskRow("t1").AnchorSessionID; anchorAfter != anchorBefore {
		t.Errorf("anchor rewritten: %v → %v", anchorBefore, anchorAfter)
	}
	if ownedAfter := e.charxOwnedSessions("t1"); len(ownedAfter) != len(ownedBefore) {
		t.Errorf("ownership changed: %v → %v", ownedBefore, ownedAfter)
	}
	// 零进程副作用：切换目标不可达不得触发重拉/临时 runtime（激活期双启动外零建进程）。
	if n := len(e.proc.newSessionNamesSnapshot()); n != newsBefore {
		t.Errorf("NewSession delta = %d, want 0（fail-closed 不重拉）", n-newsBefore)
	}
}

// TestCharxContract_DeleteCleanupEphemeralBinding 删除清理的临时绑定分支
// （design D9 表删除清理行 / D10 例外行；tasks 4.3 已接线）。
// 断言：无可复用 runtime（无 binding 的 suspended 存量任务）时删除经
// AcquireEphemeral 临时绑定完成 oc session 清理；临时 binding 不持久化（任务行
// task_binding 保持 NULL）；临时 runtime 成功终止后删除提交正常推进。
func TestCharxContract_DeleteCleanupEphemeralBinding(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	// 预埋 owned session：删除走 deleteOCSessions（无 binding → 分支②临时绑定）。
	_ = e.store.UpsertTaskSession(context.Background(), SessionRow{TaskID: "t1", SessionID: "s1"})

	if err := e.m.Delete(context.Background(), "t1", DeleteNormal, false); err != nil {
		t.Fatalf("Delete (ephemeral cleanup): %v", err)
	}
	// 行级联删除（成功提交点）。
	if _, err := e.store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatal("task row must be deleted")
	}
	if n := e.srv.deleteCallsCount(); n < 1 {
		t.Errorf("upstream DELETE calls = %d, want >= 1", n)
	}
}

// TestCharxContract_DeleteCleanup_EphemeralBindingNotPersisted 临时绑定不持久化的
// mutation 式有效性证据（tasks 4.3 行为测试本体）：同一 fixture 临时破坏——若删除
// 路径把 AcquireEphemeral 的返回值误写 tasks.task_binding（mutation），行级断言必须
// 捕获。当前实现零持久化 → 断言绿；还原后保持绿（有效性以注释内破坏点为证）。
//
// 破坏位置（已验证 → 红）：deleteOCSessions 尾部追加
// `_ = m.store.SetTaskBinding(ctx, row.ID, binding)` 后本测试 FAIL
// （bindingRawOf 观察到非NULL）；还原后恢复 PASS。
// 此处 binding 断言经删除后行级联不可行——改用「删除失败行保留」变体：
// 上游 DELETE 注入失败使任务行保留（deletion_failed），此时断言 task_binding 仍 NULL
// （临时绑定零持久化副作用在失败路径同样成立——D9 保留规则：删除失败旧 binding 保持原值，本例无旧值）。
func TestCharxContract_DeleteCleanup_EphemeralBindingNotPersisted(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	_ = e.store.UpsertTaskSession(context.Background(), SessionRow{TaskID: "t1", SessionID: "s1"})
	// 注入上游 DELETE 500 → 删除失败但行保留（可观察 task_binding 列）。
	e.srv.mu.Lock()
	e.srv.deleteStatusByID["s1"] = 500
	e.srv.mu.Unlock()
	t.Cleanup(func() {
		e.srv.mu.Lock()
		delete(e.srv.deleteStatusByID, "s1")
		e.srv.mu.Unlock()
	})

	if err := e.m.Delete(context.Background(), "t1", DeleteNormal, false); err == nil {
		t.Fatal("Delete must fail on injected upstream 500")
	}
	assertStatus(t, e.store, "t1", StatusDeletionFailed)
	// 临时绑定零持久化副作用：任务行 task_binding 保持 NULL。
	if raw, ok := e.store.bindingRawOf("t1"); ok {
		t.Errorf("ephemeral binding MUST NOT persist, got raw=%s", raw)
	}
	// 临时 runtime 会话不驻留（清理已聚合执行；非净态由 notice 承载）。
}

// TestCharxContract_EphemeralCleanupFailureBlocksDelete 临时 runtime 清理失败阻断
// 任务删除（D10 例外行，mutation 式证据）：注入 cleanup kill 非 clean disposition →
// Delete 落 deletion_failed（行保留），cleanup tickets 落 notice 可重试。
func TestCharxContract_EphemeralCleanupFailureBlocksDelete(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	_ = e.store.UpsertTaskSession(context.Background(), SessionRow{TaskID: "t1", SessionID: "s1"})
	// 临时 runtime 的 kill 产生非 clean disposition + tickets。
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-ephemeral"},
	}
	e.proc.mockProc.mu.Unlock()

	if err := e.m.Delete(context.Background(), "t1", DeleteNormal, false); err == nil {
		t.Fatal("Delete MUST be blocked by non-clean ephemeral cleanup")
	}
	assertStatus(t, e.store, "t1", StatusDeletionFailed)
	if _, err := e.store.GetTask(context.Background(), "t1"); err != nil {
		t.Fatal("task row MUST be retained (cleanup failure blocks delete commit)")
	}
	// tickets 落 notice（可重试，不随 CASCADE 丢失）。
	found := false
	for _, n := range e.charxNoticesOf("t1") {
		if tks, ok := n.Data["cleanupTickets"].([]interface{}); ok {
			for _, tk := range tks {
				if s, ok := tk.(string); ok && s == "tk-ephemeral" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("ephemeral cleanup tickets must persist in notice; notices=%+v", e.charxNoticesOf("t1"))
	}
}

// TestCharxContract_ResumeUsesPersistedBinding 重启恢复走持久化 binding 的
// mutation 式有效性证据（tasks 4.3 事实来源切换本体）：激活建立 binding →
// 篡改 binding data 的 auth.kind 为未知值（解析 fail-closed）→ 重启 Reconcile
// 恢复必须失败落 suspended——若恢复路径整体回退 tmux env 反查（mutation 破坏
// 证据，已实测：runtimeHealthyRecoverable+resumeActive+startSSE 全链 env 化 →
// 本断言变红「status=active want suspended」），存活 env 凭据健康成功 → 保持
// active，与本断言冲突。当前实现零 env 反查 → 断言绿。
func TestCharxContract_ResumeUsesPersistedBinding(t *testing.T) {
	srv := newFakeOCServer(t)
	store := newMockStore()
	proc := newCharxProc()
	e1 := newCharxEnvOnServer(t, srv, store, proc, charxEnvOpts{})
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	if err := e1.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// 前置：binding 已持久化（激活提交事务写入）。
	if _, ok := e1.store.rawBindingOf("t1"); !ok {
		t.Fatal("precondition: activation must persist binding")
	}
	// 篡改 binding data 的 auth.kind 为未知值（fail-closed：恢复前置可解析失守）。
	raw, _ := e1.store.bindingRawOf("t1")
	var envP struct {
		Version            int             `json:"version"`
		ProviderID         string          `json:"provider_id"`
		ProviderInstanceID string          `json:"provider_instance_id"`
		UpstreamSessionID  string          `json:"upstream_session_id"`
		TerminalRef        string          `json:"terminal_ref"`
		Data               json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &envP); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var data struct {
		Port      int    `json:"port"`
		Directory string `json:"directory"`
		TaskID    string `json:"taskId"`
		Auth      struct {
			Kind string `json:"kind"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(envP.Data, &data); err != nil {
		t.Fatalf("decode v1 data: %v", err)
	}
	data.Auth.Kind = "bogus"
	envP.Data, _ = json.Marshal(data)
	staleBytes, _ := json.Marshal(envP)
	if err := e1.store.SetTaskBinding(context.Background(), "t1", mustDecodeBinding(t, string(staleBytes))); err != nil {
		t.Fatalf("seed bogus binding: %v", err)
	}
	e1.lifeCancel()
	e2 := newCharxEnvOnServer(t, srv, store, proc, charxEnvOpts{})
	_ = e2.m.Reconcile(context.Background())
	// binding 解析失败 → 恢复失败 → reconciled suspended（fail-closed，不回退 env）。
	assertStatus(t, store, "t1", StatusSuspended)
}

// mustDecodeBinding 构造 domain.TaskBinding（fixture 断言 helper）。
func mustDecodeBinding(t *testing.T, raw string) domain.TaskBinding {
	t.Helper()
	b, err := domain.DecodeTaskBinding(raw)
	if err != nil {
		t.Fatalf("DecodeTaskBinding: %v", err)
	}
	return b
}

// TestCharxContract_RollbackUpgradeNamespaceBackfill 回滚再升级命名空间补齐的
// task Manager 层变体（design D6 阶段 1 / tasks 2.2；store 级等价 fixture
// internal/infrastructure/store/provider_namespace_test.go
// TestRollbackThenUpgrade_NamespaceConverges——DDL 与补齐语义的真值所在——
// 已在 Phase B 兑现；本 fixture 沿生产装配形态走 task 层消费面：真实 SQLite
// 升级（store.Open → Migrate 至 0018）→ Manager 激活持久化步骤（claim+锚定
// 三元组、binding 读写经 StoreAdapter）→ 旧二进制回滚窗口行为模拟（省略新列
// INSERT + 显式空值写）→ 再升级（重启序列：Migrate 幂等 + BackfillProviderNamespaces
// = namespaceBackfillGate）→ Manager 层消费点收敛）。
//
// 断言面：
//  1. 升级基线：StoreAdapter.ClaimTaskSessionAndSetAnchor（激活 bootstrap 的
//     持久化步骤，activate.go:1786 同一调用位）写入锚定三元组
//     (sess, opencode-v1, default)；ClaimTaskSession 三列谓词分派正常；
//  2. 旧二进制窗口：省略新列 INSERT 得 DEFAULT 命名空间；显式空值 UPDATE
//     产生 NULL/空串边界行——回滚窗口内 Manager 归属反查不失败（主键不变）；
//  3. 再升级收敛：Migrate 幂等 + Backfill 后全部行命名空间收敛 v1/default
//     （合法值保留）；Manager 消费点恢复：GetTask 正常读、三列谓词认领仍可
//     区分他任务冲突、锚定三元组完整、binding 写读正常。
//
// mutation 证据（已验证 → 红 → 还原）：把旧二进制窗口的显式空值 UPDATE 从
// anchor 三列改为仅 provider_id（模拟「补齐覆盖合法值」的回归写法）→ 再升级
// 后锚定三元组断言变红（backfill 对 NULL 锚定列不补——无锚定行语义）。反向
// mutation：删除 BackfillProviderNamespaces 调用 → 空串行走不出补齐 →
// 收敛断言变红（空串 provider_id 直接可见）。
func TestCharxContract_RollbackUpgradeNamespaceBackfill(t *testing.T) {
	ctx := context.Background()
	// 升级：真实 SQLite（openRealStore 走 store.Open → Migrate 全部 migration，
	// 含 0018 命名空间列；seed p1 + suspended 任务 t1，与生产装配同构的
	// StoreAdapter 持久化 seam）。
	adapter, db := openRealStore(t)

	// ① 升级基线：Manager 激活持久化步骤——claim + 锚定三元组。
	cres, err := adapter.ClaimTaskSessionAndSetAnchor(ctx, "t1", "sess-a", 100, 100, 100, "")
	if err != nil || !cres.Claimed {
		t.Fatalf("claim+anchor: res=%+v err=%v, want Claimed", cres, err)
	}
	pid, _, apid, aiid, asid := charxNsTaskRow(t, db, "t1")
	if pid != store.ProviderIDV1 || apid != store.ProviderIDV1 || aiid != store.ProviderInstanceV1Default || asid != "sess-a" {
		t.Fatalf("t1 anchor namespace after upgrade = (%v,%v,%v,%v), want v1 triple + sess-a", pid, apid, aiid, asid)
	}
	// binding 读写消费点（D3 envelope 经 StoreAdapter）。
	binding := domain.TaskBinding{Version: domain.TaskBindingVersion, ProviderID: store.ProviderIDV1,
		ProviderInstanceID: store.ProviderInstanceV1Default, UpstreamSessionID: "sess-a",
		TerminalRef: runtimeSessionName("t1"), Data: []byte(`{"port":50001,"directory":"/wt","taskId":"t1","auth":{"kind":"derived"}}`)}
	if err := adapter.SetTaskBinding(ctx, "t1", binding); err != nil {
		t.Fatalf("SetTaskBinding: %v", err)
	}

	// ② 旧二进制回滚窗口：省略新列 INSERT（DEFAULT 生效）+ 显式空值写（0600
	// 边界——TestRollbackThenUpgrade_NamespaceConverges 同形）。
	now := time.Now().Unix()
	if _, err := db.Exec(
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, anchor_session_id, created_at, updated_at)
		 VALUES ('t-rollback', 'p1', 'old task', 'b', 'suspended', '/wt-rb', 'sess-rb', ?, ?)`, now, now); err != nil {
		t.Fatalf("old-binary insert task: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at)
		 VALUES ('t-rollback', 'sess-rb', 1, 1, 1)`); err != nil {
		t.Fatalf("old-binary insert session: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET provider_id = '', anchor_provider_id = NULL WHERE id = 't-rollback'`); err != nil {
		t.Fatalf("old-binary explicit empty write: %v", err)
	}
	if _, err := db.Exec(`UPDATE task_sessions SET provider_id = '' WHERE task_id = 't-rollback' AND session_id = 'sess-rb'`); err != nil {
		t.Fatalf("old-binary empty session write: %v", err)
	}
	// 回滚窗口内 Manager 归属反查不失败（主键不变，rows 按行主键语义）。
	if got, ok := charxNsSessionTask(t, db, "sess-rb"); !ok || got != "t-rollback" {
		t.Fatalf("rollback-window ownership lookup sess-rb = (%q,%v), want t-rollback", got, ok)
	}

	// ③ 再升级（重启序列）：Migrate 幂等 + BackfillProviderNamespaces
	// （namespaceBackfillGate 的持久化步骤）。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-upgrade Migrate: %v", err)
	}
	if err := db.BackfillProviderNamespaces(ctx); err != nil {
		t.Fatalf("re-upgrade backfill: %v", err)
	}
	// 全部行命名空间收敛：t1（合法值保留）+ t-rollback（空值补齐 v1）。
	for _, tc := range []struct {
		id     string
		anchor string
	}{{"t1", "sess-a"}, {"t-rollback", "sess-rb"}} {
		pid, _, apid, aiid, asid := charxNsTaskRow(t, db, tc.id)
		if pid != store.ProviderIDV1 || apid != store.ProviderIDV1 || aiid != store.ProviderInstanceV1Default {
			t.Errorf("%s namespace after re-upgrade = (%v,%v,%v), want all v1/default", tc.id, pid, apid, aiid)
		}
		if asid != tc.anchor {
			t.Errorf("%s anchor_session_id = %v, want %s（补齐不得触碰锚定标识列）", tc.id, asid, tc.anchor)
		}
	}
	if spid, siid := charxNsSessionRow(t, db, "t-rollback", "sess-rb"); spid != store.ProviderIDV1 || siid != store.ProviderInstanceV1Default {
		t.Errorf("t-rollback session namespace after re-upgrade = (%v,%v), want v1/default", spid, siid)
	}
	// Manager 消费点恢复：
	// (a) GetTask 读路径在新列上正常（StoreAdapter 全字段映射）。
	if row, gerr := adapter.GetTask(ctx, "t-rollback"); gerr != nil || row.Status != StatusSuspended {
		t.Errorf("GetTask(t-rollback) after re-upgrade = (%+v, %v), want suspended row", row, gerr)
	}
	// (b) 三列谓词认领仍按命名空间分派：他任务认领 sess-a 必须冲突不写入
	//（v1/default 已被 t1 持有——补齐收敛维持归属判定冲突集合）。
	conflict, cerr := adapter.ClaimTaskSession(ctx, "t-rollback", "sess-a", 200, 200, 200, "")
	if cerr != nil || conflict.Claimed || conflict.OwnerTaskID != "t1" {
		t.Errorf("claim sess-a by t-rollback = (%+v, %v), want conflict owned by t1", conflict, cerr)
	}
	// (c) 锚定三元组：t1 认领新 sess-b 成功且三元组刷新（激活 bootstrap 重建
	// 形态——补齐后激活语义不受迁移影响）。
	cres2, aerr := adapter.ClaimTaskSessionAndSetAnchor(ctx, "t1", "sess-b", 300, 300, 300, "")
	if aerr != nil || !cres2.Claimed {
		t.Fatalf("re-claim+anchor after backfill: res=%+v err=%v, want Claimed", cres2, aerr)
	}
	if _, _, apid2, aiid2, asid2 := charxNsTaskRow(t, db, "t1"); apid2 != store.ProviderIDV1 || aiid2 != store.ProviderInstanceV1Default || asid2 != "sess-b" {
		t.Errorf("t1 anchor after re-claim = (%v,%v,%v), want v1/default/sess-b", apid2, aiid2, asid2)
	}
	// (d) binding 写读正常（D3 envelope 消费点在补齐行上语义不变）。
	gb, has, gberr := adapter.GetTaskBinding(ctx, "t1")
	if gberr != nil || !has || gb.UpstreamSessionID != "sess-a" {
		t.Errorf("GetTaskBinding(t1) = (%+v,%v,%v), want persisted binding", gb, has, gberr)
	}
}

// charxNsTaskRow 读单任务行命名空间列（fixture 3 断言 helper——raw SQL 读取
// store 包私有 helper 的同形最小集）。
func charxNsTaskRow(t *testing.T, db *store.DB, taskID string) (providerID, taskBinding, anchorPID, anchorIID, anchorSID interface{}) {
	t.Helper()
	err := db.QueryRow(
		`SELECT provider_id, task_binding, anchor_provider_id, anchor_provider_instance_id, anchor_session_id
		 FROM tasks WHERE id = ?`, taskID).
		Scan(&providerID, &taskBinding, &anchorPID, &anchorIID, &anchorSID)
	if err != nil {
		t.Fatalf("select task %s: %v", taskID, err)
	}
	return
}

// charxNsSessionRow 读单会话行命名空间列（fixture 3 断言 helper）。
func charxNsSessionRow(t *testing.T, db *store.DB, taskID, sessionID string) (providerID, providerIID interface{}) {
	t.Helper()
	err := db.QueryRow(
		`SELECT provider_id, provider_instance_id FROM task_sessions WHERE task_id = ? AND session_id = ?`,
		taskID, sessionID).Scan(&providerID, &providerIID)
	if err != nil {
		t.Fatalf("select session %s/%s: %v", taskID, sessionID, err)
	}
	return
}

// charxNsSessionTask 归属反查（回滚窗口形态断言 helper）：session_id 命中唯一任务。
func charxNsSessionTask(t *testing.T, db *store.DB, sessionID string) (taskID string, ok bool) {
	t.Helper()
	err := db.QueryRow(`SELECT task_id FROM task_sessions WHERE session_id = ?`, sessionID).Scan(&taskID)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("ownership lookup %s: %v", sessionID, err)
	}
	return taskID, true
}

// TestCharxContract_CapabilityHealthSurfacing 能力健康透出（tasks 3.8，design D7）。
// 归属包断言真值：appagent/health_test.go（迁移矩阵+epoch+重启归零）、api
// capabilities_test.go（TestServerStatus_CapabilitiesExactShape——T9 JSON 逐字段
// 形状 + TestServerStatus_CapabilitiesPrefilledUnknown——预填透出）、cmd
// main_test.go TestRegistryDispatchFactory_RegisteredAndFailClosed（装配面）、
// opencode spi_supervisor_test.go TestSupervisor_OnRuntimeReadyEpochHook（回调
// 接线恰一次）。本 fixture 从 charx 六观察面角度跨包锚定「比较例外」口径与
// 生产级接线链路，不复制跨包断言本体。
//
// 断言面：
//  1. 比较例外口径：行为保持矩阵六观察面（charxSurfaceNames）不含能力健康维度
//     ——capabilities 是唯一增量透出字段（旧基线无对应，不参与 golden 等价
//     比较，单独按 T9 形状验证在归属包）；
//  2. source-of-truth 字段链：CapabilityHealth 七字段 = T9 顶层七键的字段源
//     （primitive/scope/epoch/state/reason/taskId/session——toCapabilityDTOs
//     逐字段映射，无多余字段源）；
//  3. 生产接线链（组装根同形）：supervisor OnRuntimeReady → tracker 换代 →
//     Snapshot 透出：Acquire 成功恰一次回调 + epoch 递增 + 观测记录的 epoch
//     代际透出；失败路径零回调；
//  4. 重启归零透出语义：新 tracker 预填 7 原语 unknown/epoch0（/server/status
//     的 capabilities 数组数据源）。
//
// mutation 证据（已验证 → 红 → 还原）：
//   - 破坏 NewHealthTracker 预填循环（漏预填一个原语）→ ④ 变红（Snapshot len<7）；
//   - 破坏 OnRuntimeReady 接线（Acquire 成功不回调）→ ③ 变红（epoch=0）。
func TestCharxContract_CapabilityHealthSurfacing(t *testing.T) {
	// ① 比较例外口径：六观察面白名单不含能力健康维度（golden 矩阵只比较
	// store/sessions/ownership/notice/dto/process——capabilities 单独验证）。
	want := map[string]bool{
		"store/tasks": true, "store/sessions": true, "ownership": true,
		"notice": true, "dto/attention+agentStatus": true, "process": true,
	}
	got := map[string]bool{}
	for _, name := range charxSurfaceNames {
		if got[name] {
			t.Fatalf("charxSurfaceNames 重复面 %q", name)
		}
		got[name] = true
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("六观察面白名单漂移: got=%v want=%v（capabilities 为比较例外——本期新增透出不参与 golden 比较）", got, want)
	}

	// ② source-of-truth 字段链：CapabilityHealth 字段集合 = T9 形状七键的源
	//（+ ProviderID 装配字段——identity 去重键，DTO 层不透出）。
	hv := appagent.CapabilityHealth{}
	tp := reflect.TypeOf(hv)
	if tp.NumField() != 8 {
		t.Fatalf("CapabilityHealth 字段数 = %d, want 8（T9 七键字段源 + ProviderID）", tp.NumField())
	}
	fields := map[string]bool{}
	for i := 0; i < tp.NumField(); i++ {
		fields[tp.Field(i).Name] = true
	}
	// ProviderID 是 DTO 之外的字段源（T9 无顶层 providerId——api 形状断言覆盖；
	// 其余七者与 capabilityHealthDTO 七键一一对应）。
	if !fields["ProviderID"] || !fields["Primitive"] || !fields["Scope"] || !fields["Epoch"] || !fields["State"] || !fields["Reason"] || !fields["TaskID"] || !fields["Session"] {
		t.Errorf("CapabilityHealth 字段集合 = %v, want ProviderID+T9 全键字段源", fields)
	}

	// ③ 生产接线链（组装根同形）：supervisor OnRuntimeReady → tracker 换代。
	tr := appagent.NewHealthTracker()
	tr.SetProviderID(store.ProviderIDV1)
	prov := opencode.NewSPIProviderWithDeps(newCharxSupHealthDeps(t, tr))
	if _, err := prov.Acquire(context.Background(), appagent.TaskRef{TaskID: "t1", Directory: "/wt/t1", Mode: "worktree"}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if e := tr.RuntimeEpoch("t1"); e != 1 {
		t.Fatalf("Acquire 后 epoch = %d, want 1（OnRuntimeReady 恰一次回调）", e)
	}
	// 观测记录携带代际 → 透出记录可见 epoch=1（T9 epoch 键的运行时来源）。
	tr.Report(appagent.CapabilityHealth{ProviderID: store.ProviderIDV1,
		Primitive: appagent.PrimitiveSessionControl, Scope: appagent.ScopeRuntime,
		TaskID: "t1", Epoch: tr.RuntimeEpoch("t1"), State: appagent.CapAvailable})
	var surfaced *appagent.CapabilityHealth
	for _, h := range tr.Snapshot() {
		if h.TaskID == "t1" && h.Primitive == appagent.PrimitiveSessionControl {
			surfaced = &h
		}
	}
	if surfaced == nil || surfaced.Epoch != 1 || surfaced.State != appagent.CapAvailable {
		t.Fatalf("透出记录 = %+v, want epoch=1 available（snapshot 数据源可见换代）", surfaced)
	}
	// 失败路径零回调（BeforeLaunch 拒绝 → 零换代）。
	before := tr.RuntimeEpoch("t1")
	if _, err := prov.Resume(context.Background(), appagent.TaskRef{TaskID: "t1", Directory: "/wt/t1", Mode: "worktree"},
		domain.TaskBinding{},
		appagent.ResumePolicy{AllowSessionCreate: true, BeforeLaunch: func(ctx context.Context) error {
			return errors.New("budget exhausted fixture")
		}}); err == nil {
		t.Fatal("Resume with failing gate must error")
	}
	if after := tr.RuntimeEpoch("t1"); after != before {
		t.Errorf("失败路径代际 = %d, want %d（零回调）", after, before)
	}

	// ④ 重启归零透出语义：新 tracker（进程内内存归零）= /server/status
	// capabilities 数组数据源（api TestServerStatus_CapabilitiesPrefilledUnknown
	// 的 HTTP 形状断言真值在此确认）。
	restarted := appagent.NewHealthTracker()
	snap := restarted.Snapshot()
	if len(snap) != 7 {
		t.Fatalf("重启后 Snapshot len = %d, want 7（原语全集预填）", len(snap))
	}
	prims := map[appagent.CapabilityPrimitive]bool{}
	for _, h := range snap {
		if h.State != appagent.CapUnknown || h.Epoch != 0 || h.Scope != appagent.ScopeProvider {
			t.Fatalf("重启后预填 = %+v, want unknown/epoch0/provider 作用域", h)
		}
		prims[h.Primitive] = true
	}
	for _, p := range []appagent.CapabilityPrimitive{appagent.PrimitiveSessionControl,
		appagent.PrimitiveEventSource, appagent.PrimitiveAttentionIO, appagent.PrimitiveAsyncPrompt,
		appagent.PrimitiveStatusReader, appagent.PrimitiveTerminalIO, appagent.PrimitiveTitleUpdater} {
		if !prims[p] {
			t.Errorf("预填缺原语 %s", p)
		}
	}
}

// --- fixture ③ 的最小 supervisor seam（supervisor_deps 形状；健康 server 同形
// spi_supervisor_test newSupFixture(healthy=true)——Acquire 成功路径需要真实
// /global/health + POST /session）---

// charxcapSupProc 是 SupervisorProcSeam 的最小 fake（NewSession 落会话；
// KillSession clean——fixture ③ 无 kill 脚本需求）。
type charxcapSupProc struct {
	mu       sync.Mutex
	sessions map[string]bool
}

func newCharxSupFakeProc() *charxcapSupProc { return &charxcapSupProc{sessions: map[string]bool{}} }

func (p *charxcapSupProc) NewSession(spec process.SessionSpec) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[spec.Name] = true
	return nil
}

func (p *charxcapSupProc) KillSession(name string) (process.KillResult, error) {
	p.mu.Lock()
	delete(p.sessions, name)
	p.mu.Unlock()
	return process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}, nil
}

func (p *charxcapSupProc) HasSession(name string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[name], nil
}

// charxcapSupStore 是 SupervisorStoreSeam 的最小 fake（fixture ③ 无锚定：
// GetTaskAnchor 空 → 双启动 + claim+anchor）。
type charxcapSupStore struct {
	mu     sync.Mutex
	anchor map[string]string
}

func newCharxSupFakeSupStore() *charxcapSupStore {
	return &charxcapSupStore{anchor: map[string]string{}}
}

func (s *charxcapSupStore) GetTaskAnchor(ctx context.Context, taskID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.anchor[taskID], nil
}

func (s *charxcapSupStore) ClaimSessionKeyAndSetAnchor(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchor[taskID] = sessionID
	return application.ClaimResult{Claimed: true, Changed: true}, nil
}

func (s *charxcapSupStore) ClearTaskAnchorConditional(ctx context.Context, taskID, oldAnchor string) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anchor[taskID] != oldAnchor {
		return application.MutationResult{}, nil
	}
	delete(s.anchor, taskID)
	return application.MutationResult{Matched: true, Changed: true}, nil
}

func (s *charxcapSupStore) GetTaskLastPort(ctx context.Context, taskID string) (int, bool, error) {
	return 0, false, nil
}

// charxcapSupSideFx 是 SupervisorSideEffects 的最小 fake（ask 模式 + 空 env）。
type charxcapSupSideFx struct{}

func newCharxSupFakeSideFx() *charxcapSupSideFx { return &charxcapSupSideFx{} }

func (s *charxcapSupSideFx) ResolvePermissionMode(ctx context.Context, taskID string) (string, error) {
	return "ask", nil
}
func (s *charxcapSupSideFx) PersistServePort(ctx context.Context, taskID string, env map[string]string) error {
	return nil
}
func (s *charxcapSupSideFx) BuildServeEnv(ctx context.Context, taskID string) (map[string]string, error) {
	return map[string]string{"OCDECK_TASK_ID": taskID}, nil
}

// newCharxSupHealthDeps 构造 fixture ③ 的 SupervisorDeps：健康 server（/global/health
// + POST /session）+ 恒定端口分配 + OnRuntimeReady → tracker 换代（生产装配同形）。
func newCharxSupHealthDeps(t *testing.T, tr *appagent.HealthTracker) opencode.SupervisorDeps {
	t.Helper()
	// 派生凭据测试宇宙（supervisor passwordFor 默认派生实现）。
	t.Setenv(appagent.MasterSecretEnv, "charx-cap-health-secret")
	var mu sync.Mutex
	sessSeq := 0
	sessions := []map[string]interface{}{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/global/health":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"healthy": true, "version": opencode.ContractBaseline})
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			sessSeq++
			sess := map[string]interface{}{
				"id":   fmt.Sprintf("ses_fake_%03d", sessSeq),
				"time": map[string]interface{}{"created": 1000 + sessSeq, "updated": 1000 + sessSeq},
			}
			sessions = append(sessions, sess)
			_ = json.NewEncoder(w).Encode(sess)
		case r.URL.Path == "/session":
			// 列表：POST 生成的 session 供锚定校验（created.ID 必须在场）。
			_ = json.NewEncoder(w).Encode(sessions)
		case r.URL.Path == "/session/status":
			// 空状态 map（能力探测 Probe 依 /session/status 结构校验 + /session
			// limit=1 列表形状校验；C4 起 supervisor 建程后执行能力探测）。
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		default:
			_ = json.NewEncoder(w).Encode([]interface{}{})
		}
	}))
	t.Cleanup(ts.Close)
	port, errOfPort := strconv.Atoi(strings.TrimPrefix(ts.URL, "http://127.0.0.1:"))
	if errOfPort != nil {
		t.Fatalf("parse httptest port from %q: %v", ts.URL, errOfPort)
	}
	return opencode.SupervisorDeps{
		Proc:   newCharxSupFakeProc(),
		Store:  newCharxSupFakeSupStore(),
		SideFx: newCharxSupFakeSideFx(),
		AllocatePort: func(lastPort int, lastValid bool, exclude int) (int, error) {
			if port == exclude {
				return port + 1, nil
			}
			return port, nil
		},
		OnRuntimeReady: func(taskID string) { tr.BumpRuntimeEpoch(taskID) },
		ReadyTimeout:   60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
}

// TestCharxContract_SensitiveDataAudit 敏感数据审查（tasks 5.2 / design D3 敏感
// 数据保护）：legacy auth.password 与 master secret MUST NOT 进入 API DTO、日志、
// 事件、错误文本。审查面 = 生产写入链的存储投影：
//  1. DB 全表扫描（tasks / task_sessions / task_binding / recovery_debts）：
//     derived 任务（激活提交）的派生密码 MUST NOT 出现在任何列；env_snapshot /
//     notice / last_error 对两种任务均 MUST NOT 含密码；
//  2. legacy 仅标记字段形态：legacy 导入 binding 的 auth.password 仅存在于
//     task_binding 列（唯一存储事实），其余列零出现；
//  3. 日志面：RunMigrationImport 与激活补全链的 log 输出经捕获扫描（migration
//     runner 是唯一显式 log.Printf 的凭据邻近路径）；
//  4. 事件面：领域事件 publisher 输出全文扫描（Activate/Suspend/恢复全链）。
//
// mutation 证据（已验证 → 红 → 还原）：
//   - activationBinding 的 auth map 临时夹带 password → ①/② 红（binding 列
//     出现派生密码、derived 任务全表扫描命中）；
//   - migration runner 成功日志临时夹带密码 → ③ 红（log 捕获命中）。
func TestCharxContract_SensitiveDataAudit(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	ctx := context.Background()

	// 捕获 std log（migration runner 导入成功/失败均经 log.Printf 落账——
	// 审查「日志不携带密码值」直接可见面）。
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	// ① legacy 导入：旧二进制随机凭据 → runner 导入（binding auth.password
	// = legacy-random-audit-pw，唯一允许存储的凭据字段）。
	wt := "/data/worktrees/p1/mi-audit"
	e.store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
	e.store.tasks["mi-audit"] = TaskRow{ID: "mi-audit", ProjectID: "p1", Name: "t",
		Status: StatusActive, WorktreePath: wt,
		AnchorSessionID: sql.NullString{String: "ses-audit", Valid: true}}
	e.srv.seedSession(wt, "ses-audit", 1, 1)
	name := runtimeSessionName("mi-audit")
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.sessions[name] = true
	e.proc.mockProc.envValues[name] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "legacy-random-audit-pw",
		"OCDECK_SERVE_PORT":        fmt.Sprintf("%d", e.portHealthy()),
	}
	e.proc.mockProc.mu.Unlock()

	// ② derived 激活：suspend 任务 t1（锚定会话在场 → 直启单次成功）→ Activate
	// （派生凭据提交 binding + 状态链）。
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	e.store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "ses-existing", Valid: true}
	})
	e.srv.seedSession("/data/worktrees/p1/t1", "ses-existing", 1, 1)
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// ③ 重启恢复链（Suspend → Activate 第二轮：补偿/notice/last_error 全链再走一遍）。
	if err := e.m.Suspend(ctx, "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}

	if err := e.m.RunMigrationImport(ctx); err != nil {
		t.Fatalf("RunMigrationImport: %v", err)
	}

	// --- 审查断言 ---
	derivedPW := charxDerivedOf("t1")
	legacyPW := "legacy-random-audit-pw"
	e.store.mu.Lock()
	defer e.store.mu.Unlock()

	// ① tasks / task_binding / recovery_debts 全列扫描。
	for taskID, row := range e.store.tasks {
		for name, val := range map[string]string{
			"last_error":   row.LastError.String,
			"notice":       row.Notice.String,
			"env_snapshot": row.EnvSnapshot.String,
		} {
			for _, pw := range []string{derivedPW, legacyPW, charxMasterSecret} {
				if val != "" && strings.Contains(val, pw) {
					t.Errorf("tasks.%s(%s) 泄露凭据材料（%s 前缀）", name, taskID, pw[:8])
				}
			}
		}
	}
	// derived 任务 binding 列：派生密码零出现。
	if raw, ok := e.store.bindings["t1"]; ok && strings.Contains(raw, derivedPW) {
		t.Errorf("derived 任务 task_binding 列出现派生密码（MUST 现场派生不落库）")
	}
	// legacy 仅标记字段形态：密码仅允许出现在 task_binding 列。
	if raw, ok := e.store.bindings["mi-audit"]; ok {
		if !strings.Contains(raw, legacyPW) {
			t.Errorf("legacy binding 列缺失导入密码（导入失败形态）")
		}
	}
	// 除 task_binding 外的其余存储列对 legacy 密码零出现（①的循环已覆盖
	// tasks 列；此处补 sessions/debts 全扫描）。
	for taskID, rows := range e.store.sessions {
		for _, r := range rows {
			joined := fmt.Sprintf("%v", r)
			for _, pw := range []string{derivedPW, legacyPW} {
				if strings.Contains(joined, pw) {
					t.Errorf("task_sessions(%s) 泄露凭据材料", taskID)
				}
			}
		}
	}
	for key, debt := range e.store.recoveryDebts {
		joined := fmt.Sprintf("%+v", debt)
		for _, pw := range []string{derivedPW, legacyPW, charxMasterSecret} {
			if strings.Contains(joined, pw) {
				t.Errorf("recovery_debts(%s) 泄露凭据材料", key)
			}
		}
	}
	// ③ 日志面：捕获缓冲全文扫描。
	for _, pw := range []string{derivedPW, legacyPW, charxMasterSecret} {
		if strings.Contains(logBuf.String(), pw) {
			t.Errorf("日志输出泄露凭据材料（%s 前缀）", pw[:8])
		}
	}
	// ④ 事件面：领域事件全载荷扫描。
	e.pub.mu.Lock()
	var evDump strings.Builder
	for _, ev := range e.pub.events {
		evDump.WriteString(fmt.Sprintf("%+v", ev.Payload))
	}
	e.pub.mu.Unlock()
	for _, pw := range []string{derivedPW, legacyPW, charxMasterSecret} {
		if strings.Contains(evDump.String(), pw) {
			t.Errorf("领域事件输出泄露凭据材料（%s 前缀）", pw[:8])
		}
	}
}

// TestCharxContract_SensitiveDataAudit_SQLite 真实 SQLite 全列扫描（tasks 5.2 /
// design D3 敏感数据保护）：审查数据全部经生产写入链产生——
//  1. derived：Manager 激活提交（binding/env 快照/last_port/status 单事务写链）；
//  2. legacy：迁移导入 runner（候选 = 持久化 active；标记缺失 → 读旧随机凭据 →
//     健康验证 → binding 列唯一存储位置）；
//  3. 激活失败补偿：锚定 claim 冲突（他任务持有）→ last_error + cleanup notice
//     （kill 非净注入走 residual notice 分派）；
//  4. 恢复 debt：生产写入入口 persistRecoveryCleanupDebt → recovery_debts 行。
//
// 扫描相关表全部列（SELECT * + rows.Columns()）：唯一允许秘密的位置 = t-legacy
// 行 tasks.task_binding 的 data.auth.password（且其中仅 legacy 密码合法）；其余
// 任何位置对派生密码、legacy 密码、master secret 零出现。Scan 错误与 rows.Err()
// 必检；任一 binding 缺失即失败。
// mutation 证据（红→还原，生产写入点）：activationBinding 的 auth map 临时夹带
// 派生密码 → tasks.task_binding(t1) 出现派生密码 → 全列扫描红 → 还原。
func TestCharxContract_SensitiveDataAudit_SQLite(t *testing.T) {
	srv := newFakeOCServer(t)
	adapter, db := openRealStore(t)
	ctx := context.Background()

	// t1（openRealStore seed：repo kind + worktree 默认 mode = 合法组合）激活前置：
	// base_ref 基线 ref、锚定三元组、fake server 同目录会话。
	if _, err := db.Exec(`UPDATE tasks SET base_ref = 'refs/heads/b', anchor_session_id = 'ses-existing' WHERE id = 't1'`); err != nil {
		t.Fatalf("seed t1 anchor/base_ref: %v", err)
	}
	srv.seedSession("/wt", "ses-existing", 1, 1)
	srv.expectDirectory("/wt")
	e := newCharxEnvOnTaskStore(t, srv, adapter, newMockStore(), newCharxProc(), charxEnvOpts{})

	// ① derived：Manager 激活提交（生产写入链，runtime 凭据不落库）。
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate t1: %v", err)
	}

	// ② legacy：迁移导入 runner 真实落库（密码仅入 task_binding 列）。
	const legacyPW = "legacy-sqlite-audit-pw"
	if _, err := db.Exec(
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, anchor_session_id, created_at, updated_at)
		 VALUES ('t-legacy', 'p1', 'legacy', 'b', 'active', '/wt-legacy', 'ses-legacy', 1, 1)`); err != nil {
		t.Fatalf("seed t-legacy: %v", err)
	}
	legacyName := runtimeSessionName("t-legacy")
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.sessions[legacyName] = true
	e.proc.mockProc.envValues[legacyName] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": legacyPW,
		"OCDECK_SERVE_PORT":        fmt.Sprintf("%d", e.portHealthy()),
	}
	e.proc.mockProc.mu.Unlock()
	if err := e.m.RunMigrationImport(ctx); err != nil {
		t.Fatalf("RunMigrationImport: %v", err)
	}

	// ③ 失败场景：健康检查失败（进程已建）→ wait 就绪超时 → KillSession 门禁
	// 非净注入（reap_failed + tickets）→ residual cleanup notice + last_error
	//（激活统一补偿 runActivateFailureCompensation 的真实落账链）。
	if _, err := db.Exec(
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, anchor_session_id, created_at, updated_at)
		 VALUES ('t-conflict', 'p1', 'conflict', 'b', 'suspended', '/wt-conflict', 'refs/heads/b', 'ses-existing', 1, 1)`); err != nil {
		t.Fatalf("seed t-conflict: %v", err)
	}
	srv.seedSession("/wt-conflict", "ses-existing", 1, 1)
	srv.expectDirectory("/wt-conflict")
	srv.setHealthFailFirst(1 << 30)
	conflictName := runtimeSessionName("t-conflict")
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.killResults[conflictName] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-audit"},
	}
	e.proc.mockProc.mu.Unlock()
	if aerr := e.charxActivate("t-conflict"); aerr == nil {
		t.Fatal("健康失败激活必须失败（失败分支发生前置）")
	} else {
		t.Logf("t-conflict activate failed as expected: %v", aerr)
	}
	// ④ 恢复 debt：生产写入入口（恢复失败补偿唯一落账函数）。
	if derr := e.m.persistRecoveryCleanupDebt(ctx, "t1", runtimeSessionName("t1"),
		[]string{"tk-audit-debt"}, noticeReasonReapFailed, true, "audit debt fixture"); derr != nil {
		t.Fatalf("persist recovery debt: %v", derr)
	}

	// --- 断言各分支确实发生（审查数据非空、来源为生产写入链） ---
	// 秘密集合覆盖全部实际生成凭据的任务：t1（激活成功）与 t-conflict（激活
	// 失败但进程已派生凭据——错误文本若夹带其派生值同样必须检出）。
	derivedPW := charxDerivedOf("t1")
	secrets := []string{derivedPW, charxDerivedOf("t-conflict"), legacyPW, charxMasterSecret}
	t1Binding := charxSQLiteCol(t, db, `SELECT task_binding FROM tasks WHERE id = 't1'`)
	if !t1Binding.Valid || t1Binding.String == "" {
		t.Fatalf("t1 binding 缺失（激活提交未写入，derived 审查数据不成立）")
	}
	if strings.Contains(t1Binding.String, "password") || strings.Contains(t1Binding.String, derivedPW) {
		t.Errorf("derived task_binding 列含 password 成员/派生密码: %s", t1Binding.String)
	}
	legacyBinding := charxSQLiteCol(t, db, `SELECT task_binding FROM tasks WHERE id = 't-legacy'`)
	if !legacyBinding.Valid || legacyBinding.String == "" {
		t.Fatalf("t-legacy binding 缺失（迁移导入未写入，legacy 审查数据不成立）")
	}
	// legacy 例外精确构成（不整列放行）：envelope data.auth.kind=="legacy"、
	// data.auth.password==预期导入值；仅移除 auth.password 字段后扫描剩余
	// 完整 JSON——密码被移到 directory 或新增字段时在此红。
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(legacyBinding.String), &envelope); err != nil {
		t.Fatalf("decode legacy envelope: %v", err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("decode legacy data: %v", err)
	}
	var authMap map[string]json.RawMessage
	if err := json.Unmarshal(data["auth"], &authMap); err != nil {
		t.Fatalf("decode legacy auth: %v", err)
	}
	var authKind, authPW string
	if err := json.Unmarshal(authMap["kind"], &authKind); err != nil {
		t.Fatalf("decode auth.kind: %v", err)
	}
	if err := json.Unmarshal(authMap["password"], &authPW); err != nil {
		t.Fatalf("decode auth.password: %v", err)
	}
	if authKind != "legacy" {
		t.Errorf("legacy auth.kind = %q, want legacy", authKind)
	}
	if authPW != legacyPW {
		t.Errorf("legacy auth.password = %q, want %q（预期导入值精确相等）", authPW, legacyPW)
	}
	delete(authMap, "password")
	remaining, merr := json.Marshal(authMap)
	if merr != nil {
		t.Fatalf("marshal remaining auth: %v", merr)
	}
	data["auth"] = remaining
	remainingData, merr := json.Marshal(data)
	if merr != nil {
		t.Fatalf("marshal remaining data: %v", merr)
	}
	envelope["data"] = remainingData
	remainingEnvelope, merr := json.Marshal(envelope)
	if merr != nil {
		t.Fatalf("marshal remaining envelope: %v", merr)
	}
	for _, s := range secrets {
		if strings.Contains(string(remainingEnvelope), s) {
			t.Errorf("legacy binding 移除 auth.password 后仍泄露凭据材料（%.8s…）", s)
		}
	}
	// 唯一允许位置自身的秘密构成：仅允许 legacy 密码（派生密码/master secret 禁止）。
	for _, s := range []string{derivedPW, charxMasterSecret} {
		if strings.Contains(legacyBinding.String, s) {
			t.Errorf("legacy task_binding（唯一允许位置）出现非 legacy 凭据材料（%.8s…）", s)
		}
	}
	conflictErr := charxSQLiteCol(t, db, `SELECT last_error FROM tasks WHERE id = 't-conflict'`)
	if !conflictErr.Valid || conflictErr.String == "" {
		t.Fatalf("t-conflict last_error 空（激活失败分支未落账）")
	}
	conflictNotice := charxSQLiteCol(t, db, `SELECT notice FROM tasks WHERE id = 't-conflict'`)
	if !conflictNotice.Valid || !strings.Contains(conflictNotice.String, "tk-audit") {
		t.Fatalf("t-conflict cleanup notice 缺失（失败补偿分支未落账）: %+v", conflictNotice)
	}
	var debtCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM recovery_debts WHERE task_id = 't1'`).Scan(&debtCount); err != nil {
		t.Fatalf("scan recovery_debts count: %v", err)
	}
	if debtCount != 1 {
		t.Fatalf("recovery_debts 行数 = %d, want 1（debt 补偿分支未落账）", debtCount)
	}

	// --- 全列扫描：唯一允许位置 = t-legacy 行 tasks.task_binding ---
	for _, table := range []string{"tasks", "task_sessions", "recovery_debts", "cleanup_debts",
		"task_env_vars", "task_recovery_attempts"} {
		scanAuditTableColumns(t, db, table,
			map[string]map[string]struct{}{"t-legacy": {"task_binding": {}}}, secrets)
	}
}

// charxSQLiteCol 读单行单列（Scan 错误必检，err 非 sql.ErrNoRows 即失败）。
func charxSQLiteCol(t *testing.T, db *store.DB, query string) sql.NullString {
	t.Helper()
	var raw sql.NullString
	if err := db.QueryRow(query).Scan(&raw); err != nil && err != sql.ErrNoRows {
		t.Fatalf("scan (%s): %v", query, err)
	}
	return raw
}

// scanAuditTableColumns 全列扫描一张表的全部行/列（SELECT *）：秘密零出现
// （allow 声明的 (行任务 ID, 列名) 位置除外——其自身构成由调用方单独特断言）。
// Scan 错误与 rows.Err() 必检。
func scanAuditTableColumns(t *testing.T, db *store.DB, table string,
	allow map[string]map[string]struct{}, secrets []string) {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table)
	if err != nil {
		t.Fatalf("scan %s: %v", table, err)
	}
	defer rows.Close()
	cols, cerr := rows.Columns()
	if cerr != nil {
		t.Fatalf("scan %s columns: %v", table, cerr)
	}
	idIdx := -1
	for i, c := range cols {
		if c == "id" || c == "task_id" {
			idIdx = i
			break
		}
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s row: %v", table, err)
		}
		rowID := ""
		if idIdx >= 0 {
			rowID = vals[idIdx].String
		}
		for i, col := range cols {
			v := vals[i].String
			if !vals[i].Valid || v == "" {
				continue
			}
			allowed := false
			if allowedCols, ok := allow[rowID]; ok {
				_, allowed = allowedCols[col]
			}
			if allowed {
				continue
			}
			for _, s := range secrets {
				if strings.Contains(v, s) {
					t.Errorf("%s(%s).%s 泄露凭据材料（%.8s…）", table, rowID, col, s)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("scan %s rows: %v", table, err)
	}
}
