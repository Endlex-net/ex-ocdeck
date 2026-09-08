package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"ocdeck/internal/infrastructure/hostenv"
)

// stubHostEnvCapture 替换 hostenv.Capture 为 fake（返回值/计数可控），测试结束恢复
// TestMain 的 stub。api 包无法访问 hostenv 包内缓存状态，用例编排上不依赖初始态：
// 全部断言以「refresh 显式切换缓存」为锚点。
func stubHostEnvCapture(t *testing.T, fake func() map[string]string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := hostenv.Capture
	hostenv.Capture = func() map[string]string {
		calls.Add(1)
		return fake()
	}
	t.Cleanup(func() { hostenv.Capture = orig })
	return &calls
}

// findHostEnvVar 在响应 vars 中按 key 查找。
func findHostEnvVar(vars []hostEnvVarDTO, key string) *hostEnvVarDTO {
	for i := range vars {
		if vars[i].Key == key {
			return &vars[i]
		}
	}
	return nil
}

// getHostEnv / refreshHostEnv 请求辅助（authedReq + JSON 解码）。
func getHostEnv(t *testing.T, ts *httptest.Server) hostEnvListResponse {
	t.Helper()
	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/env/host", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /env/host status = %d, want 200", resp.StatusCode)
	}
	var list hostEnvListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	return list
}

func refreshHostEnv(t *testing.T, ts *httptest.Server) (int, errorBody, hostEnvListResponse) {
	t.Helper()
	resp, err := http.DefaultClient.Do(authedReq("POST", ts.URL+"/api/v1/env/host/refresh", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var errBody errorBody
	var list hostEnvListResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, errBody, list
	}
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, errBody, list
}

// TestHostEnvAPI 列举与刷新 API（design.md D4 / tasks 2.1-2.3）。子用例顺序编排：
// 先降级（缓存不可用），再 refresh 成功锚定已知缓存，最后 refresh 失败保留旧缓存。
// K4：每个子用例开头 ResetForTest 显式重置缓存态，-count=2 / -shuffle 下不残留状态。
func TestHostEnvAPI(t *testing.T) {
	s := newEnvAPIServer(t, newFakeProjectStore(), newFakeEnvStore(), &fakeTaskBackend{})
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	t.Cleanup(hostenv.ResetForTest)

	// --- 降级：捕获不可用（失败态/空集）→ 仅进程视图正常返回，不报错 ---
	t.Run("degraded process-only view", func(t *testing.T) {
		hostenv.ResetForTest() // 未加载态：首次 GET 必须触发懒加载捕获且失败
		calls := stubHostEnvCapture(t, func() map[string]string { return nil })

		// 首次 GET：未加载态触发一次懒加载捕获（失败）→ 按空集降级。
		list := getHostEnv(t, ts)
		if calls.Load() != 1 {
			t.Fatalf("capture calls after first GET = %d, want 1 (未加载态首次列举触发捕获)", calls.Load())
		}
		if len(list.Vars) == 0 {
			t.Fatal("degraded view = empty vars, want process env entries")
		}
		for _, v := range list.Vars {
			if v.Source != hostEnvSourceProcess {
				t.Fatalf("degraded view var %+v has source %q, want %q (no usable cache)", v, v.Source, hostEnvSourceProcess)
			}
		}
		// 再次 GET：失败态普通读取不重试，捕获计数不变。
		list2 := getHostEnv(t, ts)
		if calls.Load() != 1 {
			t.Fatalf("capture calls after second GET = %d, want 1 (failed cache must not auto-retry on list)", calls.Load())
		}
		if len(list2.Vars) != len(list.Vars) {
			t.Fatalf("second degraded view has %d vars, want %d (stable process-only view)", len(list2.Vars), len(list.Vars))
		}
	})

	// --- refresh 成功：最新合并视图（source 标注 + value 优先级）---
	t.Run("refresh success merged view", func(t *testing.T) {
		hostenv.ResetForTest() // 清掉降级用例的失败态，refresh 显式锚定成功缓存
		calls := stubHostEnvCapture(t, func() map[string]string {
			return map[string]string{
				"HOSTENV_ONLY":       "sv1",
				"HOSTENV_EMPTY":      "",
				"PATH":               "shell-path", // 进程 PATH 非空 → 值仍取进程
				"HOSTENV_SHELL2":     "sv-shell",   // 进程未设置 → 取捕获值
				"HOSTENV_BOTHV2":     "sv2",
				"HOSTENV_BOTH_EMPTY": "", // 双侧同键均空 → 值空串、source=both
			}
		})
		t.Setenv("HOSTENV_BOTHV2", "")
		t.Setenv("HOSTENV_BOTH_EMPTY", "")
		t.Setenv("HOSTENV_PROC_ONLY", "pv")

		status, errBody, list := refreshHostEnv(t, ts)
		if status != http.StatusOK {
			t.Fatalf("refresh status = %d (%+v), want 200", status, errBody)
		}
		if calls.Load() != 1 {
			t.Fatalf("capture calls = %d, want 1 (refresh recaptures)", calls.Load())
		}

		// 仅 shell 存在：非空值与空值键均保留（空值键存在性供 source 计算）。
		if v := findHostEnvVar(list.Vars, "HOSTENV_ONLY"); v == nil || v.Value != "sv1" || v.Source != hostEnvSourceShell {
			t.Errorf("HOSTENV_ONLY = %+v, want value=sv1 source=shell", v)
		}
		if v := findHostEnvVar(list.Vars, "HOSTENV_EMPTY"); v == nil || v.Value != "" || v.Source != hostEnvSourceShell {
			t.Errorf("HOSTENV_EMPTY = %+v, want value=\"\" source=shell (空值键保留)", v)
		}
		// 双侧存在：source=both；进程非空值优先。
		if v := findHostEnvVar(list.Vars, "PATH"); v == nil || v.Source != hostEnvSourceBoth || v.Value == "shell-path" {
			t.Errorf("PATH = %+v, want source=both with process value (进程非空优先)", v)
		}
		// 双侧存在、进程空串 → 取捕获值（source=both，值不一定来自进程）。
		if v := findHostEnvVar(list.Vars, "HOSTENV_BOTHV2"); v == nil || v.Value != "sv2" || v.Source != hostEnvSourceBoth {
			t.Errorf("HOSTENV_BOTHV2 = %+v, want value=sv2 source=both (进程空串取捕获值)", v)
		}
		// 双侧同键均为空串 → 值空串、source=both（spec「两侧均为空串」）。
		if v := findHostEnvVar(list.Vars, "HOSTENV_BOTH_EMPTY"); v == nil || v.Value != "" || v.Source != hostEnvSourceBoth {
			t.Errorf("HOSTENV_BOTH_EMPTY = %+v, want value=\"\" source=both (双侧均为空串)", v)
		}
		// 进程未设置、仅捕获存在非空值 → source=shell。
		if v := findHostEnvVar(list.Vars, "HOSTENV_SHELL2"); v == nil || v.Value != "sv-shell" || v.Source != hostEnvSourceShell {
			t.Errorf("HOSTENV_SHELL2 = %+v, want value=sv-shell source=shell (进程不存在)", v)
		}
		// 仅进程存在。
		if v := findHostEnvVar(list.Vars, "HOSTENV_PROC_ONLY"); v == nil || v.Value != "pv" || v.Source != hostEnvSourceProcess {
			t.Errorf("HOSTENV_PROC_ONLY = %+v, want value=pv source=process", v)
		}
		// 输出按键名排序（稳定顺序）。
		for i := 1; i < len(list.Vars); i++ {
			if list.Vars[i-1].Key > list.Vars[i].Key {
				t.Fatalf("vars not sorted by key at %d: %q > %q", i, list.Vars[i-1].Key, list.Vars[i].Key)
			}
		}

		// GET 复用同一缓存：视图与 refresh 响应一致，不再触发捕获。
		before := calls.Load()
		list2 := getHostEnv(t, ts)
		if got := findHostEnvVar(list2.Vars, "HOSTENV_ONLY"); got == nil || got.Value != "sv1" || got.Source != hostEnvSourceShell {
			t.Errorf("GET HOSTENV_ONLY = %+v, want value=sv1 source=shell (缓存服务)", got)
		}
		if calls.Load() != before {
			t.Fatalf("capture calls after GET = %d, want %d (list must not recapture)", calls.Load(), before)
		}
	})

	// --- refresh 失败：500 固定文案（不含捕获内容），旧缓存保留继续服务 ---
	t.Run("refresh failure keeps cache", func(t *testing.T) {
		// 显式建立成功缓存（不依赖前序子用例顺序，-shuffle 安全）。
		// 保存并恢复本用例入口的 Capture：seed 函数不得泄漏到后续测试
		//（若直接赋值，后续 stub helper 的 cleanup 只能恢复到 seed 而非 TestMain stub）。
		hostenv.ResetForTest()
		origCapture := hostenv.Capture
		t.Cleanup(func() { hostenv.Capture = origCapture })
		hostenv.Capture = func() map[string]string {
			return map[string]string{"HOSTENV_ONLY": "sv1"}
		}
		if err := hostenv.Refresh(); err != nil {
			t.Fatalf("seed refresh: %v", err)
		}

		stubHostEnvCapture(t, func() map[string]string { return nil })

		status, errBody, _ := refreshHostEnv(t, ts)
		if status != http.StatusInternalServerError {
			t.Fatalf("refresh failure status = %d, want 500", status)
		}
		if errBody.Error.Code != CodeInternal || errBody.Error.Message != "refresh host env failed" {
			t.Errorf("error body = %+v, want code=internal message=%q (固定文案，不含捕获内容)", errBody, "refresh host env failed")
		}

		// 旧成功缓存保留：GET 仍为 refresh 成功锚定的视图（HOSTENV_ONLY 仍在）。
		list := getHostEnv(t, ts)
		if v := findHostEnvVar(list.Vars, "HOSTENV_ONLY"); v == nil || v.Value != "sv1" || v.Source != hostEnvSourceShell {
			t.Errorf("HOSTENV_ONLY after failed refresh = %+v, want value=sv1 source=shell (D3 保留旧成功缓存)", v)
		}
	})
}
