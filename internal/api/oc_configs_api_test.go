package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"ocdeck/internal/config"
)

// ocConfigTestBackend 实现 OCConfigService + 受影响任务回显。
type ocConfigTestBackend struct {
	mgr *config.OCConfigManager
}

func (b *ocConfigTestBackend) List() ([]config.OCConfigName, error)             { return b.mgr.List() }
func (b *ocConfigTestBackend) Read(name string) (config.OCConfigContent, error) { return b.mgr.Read(name) }
func (b *ocConfigTestBackend) Save(name, content string, expectedMtime int64, expectedHash string) (config.OCConfigContent, error) {
	return b.mgr.Save(name, content, expectedMtime, expectedHash)
}

// newOCConfigAPIServer 构造带 OCConfigService + TaskBackend（返回受影响任务）的 Server。
func newOCConfigAPIServer(t *testing.T, svc OCConfigService, tb TaskBackend) *Server {
	t.Helper()
	cfg := testConfig()
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
		tasks:     tb,
		ocCfgs:    svc,
	}
	s.registerRoutes()
	return s
}

func TestOCConfigAPI_ListAndRead(t *testing.T) {
	dir := t.TempDir()
	mgr := config.NewOCConfigManager(dir)
	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(`{"a":1}`), 0o600)
	svc := &ocConfigTestBackend{mgr: mgr}
	tb := &fakeTaskBackend{}
	s := newOCConfigAPIServer(t, svc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// list
	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/oc-configs", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var list ocConfigListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Configs) != 1 || list.Configs[0].Name != "opencode.json" {
		t.Errorf("list = %+v", list.Configs)
	}

	// read
	resp2, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/oc-configs/opencode.json", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var c ocConfigDTO
	if err := json.NewDecoder(resp2.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Content != `{"a":1}` || c.Hash == "" || c.Mtime == 0 {
		t.Errorf("read dto = %+v", c)
	}
}

func TestOCConfigAPI_Save_AffectedActiveTasksAndRestart(t *testing.T) {
	dir := t.TempDir()
	mgr := config.NewOCConfigManager(dir)
	svc := &ocConfigTestBackend{mgr: mgr}
	// 受影响任务回显 ["t-active"]。
	tb := &affectedTasksBackend{ids: []string{"t-active"}}
	s := newOCConfigAPIServer(t, svc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"content":"{\"a\":2}","mtime":0,"hash":""}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/oc-configs/opencode.json", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d, want 200", resp.StatusCode)
	}
	var sr ocConfigSaveResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if !sr.RestartRequired {
		t.Error("restartRequired should be true")
	}
	if len(sr.AffectedActiveTasks) != 1 || sr.AffectedActiveTasks[0] != "t-active" {
		t.Errorf("affectedActiveTasks = %+v, want [t-active]", sr.AffectedActiveTasks)
	}
	if sr.Hash == "" || sr.Mtime == 0 {
		t.Errorf("saved mtime/hash empty: %+v", sr)
	}
}

func TestOCConfigAPI_Save_Conflict409(t *testing.T) {
	dir := t.TempDir()
	mgr := config.NewOCConfigManager(dir)
	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(`{"a":1}`), 0o600)
	svc := &ocConfigTestBackend{mgr: mgr}
	tb := &fakeTaskBackend{}
	s := newOCConfigAPIServer(t, svc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 用错误 mtime+hash 触发冲突。
	body := `{"content":"{\"a\":2}","mtime":999,"hash":"deadbeef"}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/oc-configs/opencode.json", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeConflict {
		t.Errorf("code = %s, want conflict", eb.Error.Code)
	}
}

func TestOCConfigAPI_Save_SyntaxError422(t *testing.T) {
	dir := t.TempDir()
	mgr := config.NewOCConfigManager(dir)
	svc := &ocConfigTestBackend{mgr: mgr}
	tb := &fakeTaskBackend{}
	s := newOCConfigAPIServer(t, svc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"content":"{invalid","mtime":0,"hash":""}`
	resp, err := http.DefaultClient.Do(authedReq("PUT", ts.URL+"/api/v1/oc-configs/opencode.json", body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestOCConfigAPI_Read_NotFound404(t *testing.T) {
	dir := t.TempDir()
	mgr := config.NewOCConfigManager(dir)
	svc := &ocConfigTestBackend{mgr: mgr}
	tb := &fakeTaskBackend{}
	s := newOCConfigAPIServer(t, svc, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/oc-configs/missing.json", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// affectedTasksBackend 返回固定 active 任务 ID 列表。
type affectedTasksBackend struct {
	*fakeTaskBackend
	ids []string
}

func (a *affectedTasksBackend) ListAllActiveTaskIDs(ctx context.Context) ([]string, error) {
	return a.ids, nil
}