package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appuploads "ocdeck/internal/application/uploads"
	uploadsinfra "ocdeck/internal/infrastructure/uploads"
)

// --- 测试基建 ---

const (
	uploadTestToken   = "testtoken"
	uploadTestTaskID  = "t1"
	uploadTestConnID  = "0123abcd-00ab-4cd0-9ef0-0123456789ab"
	uploadTestBdry    = "ocdeckupload"
	uploadTestM       = int64(64 << 10) // M=64KiB：边界用例避免构造大 payload
	uploadStallTimout = 400 * time.Millisecond

	uploadTestContentType = "multipart/form-data; boundary=" + uploadTestBdry
)

// fakeUploadTasks uploads.TaskPort fake（任务存在/活跃）。
type fakeUploadTasks struct {
	exists, active bool
	err            error
}

func (f *fakeUploadTasks) TaskActive(context.Context, string) (bool, bool, error) {
	return f.exists, f.active, f.err
}

// fakeUploadConns uploads.ConnPort fake（当前 TUI 连接）。
type fakeUploadConns struct {
	current string
	found   bool
	err     error
}

func (f *fakeUploadConns) CurrentTUIConn(context.Context, string) (string, bool, error) {
	return f.current, f.found, f.err
}

// countingUploadOrch 委托真实编排并统计 AdmitUpload/AbortUpload 调用（准入前置
// 顺序、登记收尾断言）。
type countingUploadOrch struct {
	inner      uploadOrchestrator
	mu         sync.Mutex
	admitCalls int
	abortCalls int
}

func (c *countingUploadOrch) AdmitUpload(ctx context.Context, taskID string) error {
	c.mu.Lock()
	c.admitCalls++
	c.mu.Unlock()
	return c.inner.AdmitUpload(ctx, taskID)
}

func (c *countingUploadOrch) admitCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.admitCalls
}

func (c *countingUploadOrch) RegisterActiveUpload(taskID, connID string) (*appuploads.ActiveUpload, error) {
	return c.inner.RegisterActiveUpload(taskID, connID)
}

func (c *countingUploadOrch) BindConnID(u *appuploads.ActiveUpload, connID string) error {
	return c.inner.BindConnID(u, connID)
}

func (c *countingUploadOrch) AdmitUploadConn(ctx context.Context, taskID, connID string) error {
	return c.inner.AdmitUploadConn(ctx, taskID, connID)
}

func (c *countingUploadOrch) BindOriginalName(u *appuploads.ActiveUpload, originalName string) string {
	return c.inner.BindOriginalName(u, originalName)
}

func (c *countingUploadOrch) WriteUploadBody(ctx context.Context, u *appuploads.ActiveUpload, r io.Reader) (int64, error) {
	return c.inner.WriteUploadBody(ctx, u, r)
}

func (c *countingUploadOrch) FinalizeUpload(ctx context.Context, u *appuploads.ActiveUpload) error {
	return c.inner.FinalizeUpload(ctx, u)
}

func (c *countingUploadOrch) AbortUpload(ctx context.Context, u *appuploads.ActiveUpload) {
	c.mu.Lock()
	c.abortCalls++
	c.mu.Unlock()
	c.inner.AbortUpload(ctx, u)
}

func (c *countingUploadOrch) abortCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.abortCalls
}

// errUploadStore 包装真实存储：WritePartial 注入失败（磁盘写失败 → 500 场景），
// RemovePartial 记录调用（失败清理断言）。
type errUploadStore struct {
	appuploads.StorePort
	writeErr error

	mu      sync.Mutex
	removed []string
}

func (s *errUploadStore) WritePartial(context.Context, string, string, io.Reader, func(int64)) (int64, error) {
	return 0, s.writeErr
}

func (s *errUploadStore) RemovePartial(taskID, name string) error {
	s.mu.Lock()
	s.removed = append(s.removed, taskID+"/"+name)
	s.mu.Unlock()
	return s.StorePort.RemovePartial(taskID, name)
}

func (s *errUploadStore) removedCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.removed...)
}

// uploadServerOpts 测试服务装配选项（nil 字段用默认：任务 active、connId 当前、
// 真实 store、停滞 1h、已接线）。
type uploadServerOpts struct {
	tasks       *fakeUploadTasks
	conns       *fakeUploadConns
	stall       time.Duration
	store       appuploads.StorePort
	noAdmission bool // 组合根未注入（降级语义测试）
	onWired     func(s *Server, orch *appuploads.Orchestrator)
}

// uploadTestServer 组合根形态的测试服务：api.New 完整路由（/api/v1 Bearer 中间件）+
// SetUploadAdmission/SetUploadLimits 注入真实 application 编排（infrastructure store
// 落盘 t.TempDir()）+ fake 任务/连接端口。停滞联动测试依赖真实网络连接
// （ResponseController.SetReadDeadline 需真实 conn）。
type uploadTestServer struct {
	srv  *httptest.Server
	orch *appuploads.Orchestrator
	dir  string
}

func newUploadTestServer(t *testing.T, opts *uploadServerOpts) *uploadTestServer {
	t.Helper()
	if opts == nil {
		opts = &uploadServerOpts{}
	}
	dir := t.TempDir()
	store := opts.store
	if store == nil {
		store = uploadsinfra.NewStore(dir)
	}
	tasks := opts.tasks
	if tasks == nil {
		tasks = &fakeUploadTasks{exists: true, active: true}
	}
	conns := opts.conns
	if conns == nil {
		conns = &fakeUploadConns{current: uploadTestConnID, found: true}
	}
	orch := appuploads.New(appuploads.Options{
		Cfg:          appuploads.Config{UploadDir: dir, MaxBytes: uploadTestM},
		Store:        store,
		Tasks:        tasks,
		Conns:        conns,
		StallTimeout: opts.stall,
	})
	s := New(testConfig(), nil)
	if !opts.noAdmission {
		s.SetUploadAdmission(orch)
		s.SetUploadLimits(uploadTestM, dir)
	}
	if opts.onWired != nil {
		opts.onWired(s, orch)
	}
	// 与 Serve 生产装配一致：根 mux 作为 Handler（含 /api/v1 Bearer 中间件）。
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	return &uploadTestServer{srv: ts, orch: orch, dir: dir}
}

// --- 请求体构造 ---

// uploadBodyOpts multipart 请求体构造（默认形态 = 合规上传，字段裁剪出各场景）。
type uploadBodyOpts struct {
	preamble    []byte // 前置于首个 boundary
	connID      string // connId 字段值（默认 uploadTestConnID）
	connHeaders []byte // connId part 附加头（撑前置开销）
	fileCTE     string // file part 的 Content-Transfer-Encoding
	fileHeaders []byte // file part 附加头
	fileContent []byte
	secondFile  []byte // 非 nil：唯一 file part 之后追加第二个 file part
	epilogue    []byte // 结束 boundary 之后的尾随垃圾
	noFile      bool   // connId part 后直接结束（无 file part）
	omitFinal   bool   // 省略结束 boundary（尾部损坏）
	firstIsFile bool   // 首 part 即 file part（首 part 非 connId）
}

func buildUploadBody(o uploadBodyOpts) []byte {
	var b bytes.Buffer
	b.Write(o.preamble)
	b.WriteString("--" + uploadTestBdry + "\r\n")
	if o.firstIsFile {
		writeFilePartHeader(&b, o)
		b.Write(o.fileContent)
		b.WriteString("\r\n")
		if !o.omitFinal {
			b.WriteString("--" + uploadTestBdry + "--\r\n")
		}
		b.Write(o.epilogue)
		return b.Bytes()
	}
	connID := o.connID
	if connID == "" {
		connID = uploadTestConnID
	}
	b.WriteString("Content-Disposition: form-data; name=\"connId\"\r\n")
	b.Write(o.connHeaders)
	b.WriteString("\r\n")
	b.WriteString(connID)
	b.WriteString("\r\n")
	if o.noFile {
		b.WriteString("--" + uploadTestBdry + "--\r\n")
		b.Write(o.epilogue)
		return b.Bytes()
	}
	b.WriteString("--" + uploadTestBdry + "\r\n")
	writeFilePartHeader(&b, o)
	b.Write(o.fileContent)
	b.WriteString("\r\n")
	if o.secondFile != nil {
		b.WriteString("--" + uploadTestBdry + "\r\n")
		b.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"b.png\"\r\n\r\n")
		b.Write(o.secondFile)
		b.WriteString("\r\n")
	}
	if !o.omitFinal {
		b.WriteString("--" + uploadTestBdry + "--\r\n")
	}
	b.Write(o.epilogue)
	return b.Bytes()
}

func writeFilePartHeader(b *bytes.Buffer, o uploadBodyOpts) {
	b.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"a.png\"\r\n")
	if o.fileCTE != "" {
		b.WriteString("Content-Transfer-Encoding: " + o.fileCTE + "\r\n")
	}
	b.Write(o.fileHeaders)
	b.WriteString("\r\n")
}

// padPreambleToOverhead 追加 preamble 使总非文件开销恰为 target
// （开销 = 请求体总字节 − file part 内容字节，与唯一计量表定义一致）。
// 填充为逐字节 '\n' 的空 preamble 行：保证 boundary 位于行首（RFC 2046 合规形态），
// 且每个填充字节均被逐行计入开销。
func padPreambleToOverhead(t *testing.T, body []byte, contentLen, target int64) []byte {
	t.Helper()
	base := int64(len(body)) - contentLen
	need := target - base
	if need < 0 {
		t.Fatalf("base overhead %d already exceeds target %d", base, target)
	}
	out := make([]byte, 0, int(need)+len(body))
	out = append(out, bytes.Repeat([]byte{'\n'}, int(need))...)
	return append(out, body...)
}

// --- 请求与断言辅助 ---

// postUpload 发送已认证上传请求（contentLength <0 → chunked，即缺失 Content-Length）。
func (uts *uploadTestServer) postUpload(t *testing.T, body []byte, contentLength int64, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, uts.srv.URL+"/api/v1/tasks/"+uploadTestTaskID+"/attachments", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+uploadTestToken)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+uploadTestBdry)
	if contentLength < 0 {
		req.ContentLength = -1 // chunked：缺失 Content-Length
	} else {
		req.ContentLength = contentLength
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := uts.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post upload: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// decodeUploadError 解析统一错误信封，返回 (状态码, code, message)。
func decodeUploadError(t *testing.T, resp *http.Response) (int, ErrorCode, string) {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read error body: %v", err)
	}
	var eb errorBody
	if err := json.Unmarshal(raw, &eb); err != nil {
		t.Fatalf("error envelope %q: %v", raw, err)
	}
	return resp.StatusCode, eb.Error.Code, eb.Error.Message
}

// decodeUploadSuccess 断言 201 与 uploadId 32hex 形态并返回 uploadId。
func decodeUploadSuccess(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body %s, want 201", resp.StatusCode, raw)
	}
	var ok struct {
		UploadID string `json:"uploadId"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil {
		t.Fatalf("success body %q: %v", raw, err)
	}
	if len(ok.UploadID) != 32 {
		t.Fatalf("uploadId %q not 32 chars", ok.UploadID)
	}
	for _, c := range ok.UploadID {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("uploadId %q not hex", ok.UploadID)
		}
	}
	return ok.UploadID
}

// taskUploadEntries 列出任务上传目录现存条目（目录不存在返回 nil）。
func taskUploadEntries(t *testing.T, dir, taskID string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, taskID))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func assertNoUploadFiles(t *testing.T, dir, taskID string) {
	t.Helper()
	if files := taskUploadEntries(t, dir, taskID); len(files) != 0 {
		t.Fatalf("upload dir not clean: %v", files)
	}
}

// assertError 断言响应状态码与错误码。
func assertError(t *testing.T, resp *http.Response, wantStatus int, wantCode ErrorCode) {
	t.Helper()
	status, code, _ := decodeUploadError(t, resp)
	if status != wantStatus || code != wantCode {
		t.Fatalf("status/code = %d/%s, want %d/%s", status, code, wantStatus, wantCode)
	}
}

// stallUploadClient raw TCP 上传客户端：头部 + 分块可控发送（停滞联动测试用）。
type stallUploadClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

// newStallUploadClient 建立 raw TCP 连接并写入请求头。chunked=true 用
// Transfer-Encoding（缺失 Content-Length）；否则声明 contentLength（可伪造）。
// contentType 传入请求头 Content-Type（坏 Content-Type 场景传非法值）。
func newStallUploadClient(t *testing.T, srvURL string, chunked bool, contentLength int64, contentType string) *stallUploadClient {
	t.Helper()
	host := strings.TrimPrefix(srvURL, "http://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	head := "POST /api/v1/tasks/" + uploadTestTaskID + "/attachments HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Authorization: Bearer " + uploadTestToken + "\r\n" +
		"Content-Type: " + contentType + "\r\n"
	if chunked {
		head += "Transfer-Encoding: chunked\r\n"
	} else {
		head += "Content-Length: " + strconv.FormatInt(contentLength, 10) + "\r\n"
	}
	head += "\r\n"
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatalf("write head: %v", err)
	}
	return &stallUploadClient{t: t, conn: conn, br: bufio.NewReader(conn)}
}

func (c *stallUploadClient) send(b []byte) {
	c.t.Helper()
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

// readResponse 有界读取状态行与响应体（按 Content-Length 精确读取）。
func (c *stallUploadClient) readResponse() (string, []byte) {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		c.t.Fatalf("set read deadline: %v", err)
	}
	line, err := c.br.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read status line: %v", err)
	}
	var contentLength int64 = -1
	for {
		h, herr := c.br.ReadString('\n')
		if herr != nil {
			c.t.Fatalf("read header: %v", herr)
		}
		if h == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(h), "content-length:") {
			contentLength, _ = strconv.ParseInt(strings.TrimSpace(h[len("content-length:"):]), 10, 64)
		}
	}
	var body []byte
	if contentLength >= 0 {
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(c.br, body); err != nil {
			c.t.Fatalf("read body: %v", err)
		}
	}
	return line, body
}

// assertStallResponse 断言停滞 408 响应：状态行、固定 message、信封 code，
// 且时序有界（停滞计时器先行、无锁等待环）。
func assertStallResponse(t *testing.T, status string, body []byte, elapsed, stall time.Duration) {
	t.Helper()
	if !strings.Contains(status, "408") {
		t.Fatalf("status line %q, want 408", status)
	}
	if !strings.Contains(string(body), `"code":"invalid_input"`) {
		t.Fatalf("body %q missing invalid_input code", body)
	}
	if !strings.Contains(string(body), uploadStalledMessage) {
		t.Fatalf("body %q missing fixed stall message %q", body, uploadStalledMessage)
	}
	if elapsed < stall-150*time.Millisecond {
		t.Errorf("responded after %v, stall cancel must gate the response", elapsed)
	}
	if elapsed > stall+3*time.Second {
		t.Errorf("closeout took %v, want bounded (no lock-wait ring)", elapsed)
	}
}

// awaitStallCleanup 轮询等待停滞取消收尾（异步步③）清理 .partial。
func awaitStallCleanup(t *testing.T, dir, taskID string) {
	t.Helper()
	waitFor(t, 3*time.Second, "stall cleanup removes partial", func() bool {
		return len(taskUploadEntries(t, dir, taskID)) == 0
	})
}

// --- 3.3：路由存在、401 与未接线降级 ---

// TestUploadUnauthorized401（3.3 + spec 401 行）：未认证请求 401 unauthorized
// （/api/v1 子 mux Bearer 中间件），无文件落盘。
func TestUploadUnauthorized401(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	req, err := http.NewRequest(http.MethodPost, uts.srv.URL+"/api/v1/tasks/"+uploadTestTaskID+"/attachments", bytes.NewReader(buildUploadBody(uploadBodyOpts{fileContent: []byte("x")})))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+uploadTestBdry)
	resp, err := uts.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	assertError(t, resp, http.StatusUnauthorized, CodeUnauthorized)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// TestUploadDegradedWhenNotWired：组合根未注入 → 404 not_found，与旧 server
// 无该路由不可区分（handler 未接线语义见 uploads.go 注释）。
func TestUploadDegradedWhenNotWired(t *testing.T) {
	uts := newUploadTestServer(t, &uploadServerOpts{noAdmission: true})
	resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: []byte("x")}), -1, nil)
	assertError(t, resp, http.StatusNotFound, CodeNotFound)
}

// --- spec 错误表：403 Origin（先于准入与任何写盘） ---

func TestUploadForbiddenOriginBeforeAdmission(t *testing.T) {
	var counted *countingUploadOrch
	uts := newUploadTestServer(t, &uploadServerOpts{onWired: func(s *Server, orch *appuploads.Orchestrator) {
		counted = &countingUploadOrch{inner: orch}
		s.SetUploadAdmission(counted)
	}})
	resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: []byte("x")}), -1,
		map[string]string{"Origin": "https://evil.example"})
	assertError(t, resp, http.StatusForbidden, CodeForbidden)
	if got := counted.admitCallCount(); got != 0 {
		t.Errorf("AdmitUpload called %d times, want 0 (Origin check must precede admission)", got)
	}
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// --- spec 错误表：404 任务不存在 / 409 任务非活跃（阶段①，先于请求体解析） ---

func TestUploadTaskAdmission404And409(t *testing.T) {
	t.Run("task not found", func(t *testing.T) {
		uts := newUploadTestServer(t, &uploadServerOpts{tasks: &fakeUploadTasks{exists: false}})
		resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: []byte("x")}), -1, nil)
		assertError(t, resp, http.StatusNotFound, CodeNotFound)
		assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
	})
	t.Run("task inactive", func(t *testing.T) {
		uts := newUploadTestServer(t, &uploadServerOpts{tasks: &fakeUploadTasks{exists: true, active: false}})
		resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: []byte("x")}), -1, nil)
		assertError(t, resp, http.StatusConflict, CodeInvalidState)
		assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
	})
	t.Run("task query infra failure", func(t *testing.T) {
		uts := newUploadTestServer(t, &uploadServerOpts{tasks: &fakeUploadTasks{err: fmt.Errorf("db gone")}})
		resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: []byte("x")}), -1, nil)
		// 基础设施故障 MUST NOT 误映射 404/409（spec 错误表）。
		assertError(t, resp, http.StatusInternalServerError, CodeInternal)
	})
}

// --- spec 错误表：400 首 part 非 connId / 403 connId 非当前（均零落盘） ---

func TestUploadFirstPartFileNotConnID400(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{
		firstIsFile: true,
		fileContent: []byte("binary"),
	}), -1, nil)
	assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

func TestUploadConnNotCurrent403NoDiskWrite(t *testing.T) {
	uts := newUploadTestServer(t, &uploadServerOpts{conns: &fakeUploadConns{current: "replaced-conn", found: true}})
	resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: []byte("binary")}), -1, nil)
	assertError(t, resp, http.StatusForbidden, CodeForbidden)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// --- spec 计量表：大小边界 M-1 / M / M+1 与成功形态 ---

func TestUploadSizeBoundaries(t *testing.T) {
	t.Run("M-1 and M accepted", func(t *testing.T) {
		for _, size := range []int64{uploadTestM - 1, uploadTestM} {
			uts := newUploadTestServer(t, nil)
			content := bytes.Repeat([]byte("A"), int(size))
			resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: content}), -1, nil)
			id := decodeUploadSuccess(t, resp)
			got, err := os.ReadFile(filepath.Join(uts.dir, uploadTestTaskID, id+".png"))
			if err != nil {
				t.Fatalf("size %d: committed file: %v", size, err)
			}
			if int64(len(got)) != size {
				t.Fatalf("size %d: committed %d bytes", size, len(got))
			}
			if _, err := os.Stat(filepath.Join(uts.dir, uploadTestTaskID, id+".png.meta.json")); err != nil {
				t.Fatalf("size %d: sidecar missing: %v", size, err)
			}
		}
	})
	t.Run("M+1 rejected", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{
			fileContent: bytes.Repeat([]byte("A"), int(uploadTestM)+1),
		}), -1, nil)
		assertError(t, resp, http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
		// 不留可投递文件（413 清理 .partial）。
		assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
	})
}

// TestUploadSuccess201Shape：成功上传原子落盘、201 与 uploadId（spec 成功场景）。
func TestUploadSuccess201Shape(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	content := []byte("PNGDATA-content")
	resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: content}), -1, nil)
	id := decodeUploadSuccess(t, resp)
	got, err := os.ReadFile(filepath.Join(uts.dir, uploadTestTaskID, id+".png"))
	if err != nil {
		t.Fatalf("read committed file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("committed content %q, want %q", got, content)
	}
}

// --- spec：伪造或缺失 Content-Length 以实际字节判定 ---

func TestUploadContentLengthForgedOrMissing(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), int(uploadTestM)+1)
	full := buildUploadBody(uploadBodyOpts{fileContent: payload})
	t.Run("forged large content-length", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		// raw TCP 伪造超长 Content-Length，仅发送 M+1 payload 后关闭写侧：
		// server 以实际读取字节判定 → 413，不因 Content-Length 放行。
		c := newStallUploadClient(t, uts.srv.URL, false, 64<<20, uploadTestContentType)
		c.send(full)
		if err := c.conn.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatalf("close write: %v", err)
		}
		status, body := c.readResponse()
		if !strings.Contains(status, "413") {
			t.Fatalf("status %q, want 413 (judged by actual bytes)", status)
		}
		if !strings.Contains(string(body), "payload_too_large") {
			t.Fatalf("body %q missing payload_too_large", body)
		}
		assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
	})
	t.Run("missing content-length (chunked)", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		resp := uts.postUpload(t, full, -1, nil)
		assertError(t, resp, http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
		assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
	})
}

// --- spec：额外文件 part 与尾部格式损坏（400 + 临时文件清理） ---

func TestUploadExtraFilePart400CleansPartial(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{
		fileContent: []byte("first"),
		secondFile:  []byte("second"),
	}), -1, nil)
	assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

func TestUploadCorruptedTail400CleansPartial(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{
		fileContent: []byte("content"),
		omitFinal:   true, // 缺结束 boundary
	}), -1, nil)
	assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// --- spec：尾随垃圾（epilogue）——合规消费至 EOF 提交；超开销 400 ---

func TestUploadEpilogueGarbage(t *testing.T) {
	t.Run("compliant epilogue committed", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{
			fileContent: []byte("content"),
			epilogue:    bytes.Repeat([]byte("g"), 8<<10),
		}), -1, nil)
		decodeUploadSuccess(t, resp)
	})
	t.Run("epilogue pushing overhead beyond 64KiB rejected", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		content := []byte("content")
		base := buildUploadBody(uploadBodyOpts{fileContent: content})
		baseOverhead := int64(len(base)) - int64(len(content))
		epi := bytes.Repeat([]byte("g"), int(uploadOverheadLimit-baseOverhead+1))
		resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{
			fileContent: content,
			epilogue:    epi,
		}), -1, nil)
		assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
		assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
	})
}

// --- spec：非 identity 编码 file part 拒绝（raw 语义，不隐式解码） ---

func TestUploadQuotedPrintableRejected(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{
		fileContent: []byte("quoted=3Dcontent"),
		fileCTE:     "quoted-printable",
	}), -1, nil)
	assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// --- spec 计量表：同时违规按协议解析顺序首次违规优先 ---

// TestUploadLeadingOverheadWins400：前置开销（preamble）先超 64KiB，payload 也超 M
// → 按解析顺序返回 400（不是 413）。
func TestUploadLeadingOverheadWins400(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	payload := bytes.Repeat([]byte("A"), int(uploadTestM)+1)
	body := buildUploadBody(uploadBodyOpts{fileContent: payload})
	body = padPreambleToOverhead(t, body, int64(len(payload)), uploadOverheadLimit+1)
	resp := uts.postUpload(t, body, -1, nil)
	assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// TestUploadPayloadViolationWins413：前置开销合规、payload 流中先超 M（尾部开销
// 随后才会超限）→ 413。
func TestUploadPayloadViolationWins413(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	payload := bytes.Repeat([]byte("A"), int(uploadTestM)+1)
	body := buildUploadBody(uploadBodyOpts{
		fileContent: payload,
		epilogue:    bytes.Repeat([]byte("g"), 8<<10), // 若开销在 payload 后才判定则超 64KiB
	})
	body = padPreambleToOverhead(t, body, int64(len(payload)), uploadOverheadLimit-4<<10) // 前置开销合规
	resp := uts.postUpload(t, body, -1, nil)
	assertError(t, resp, http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// TestUploadOverheadExactBoundary：非文件开销恰好 64KiB 可正常解析；超出 → 400。
func TestUploadOverheadExactBoundary(t *testing.T) {
	t.Run("exactly 64KiB accepted", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		content := []byte("content")
		body := buildUploadBody(uploadBodyOpts{fileContent: content})
		body = padPreambleToOverhead(t, body, int64(len(content)), uploadOverheadLimit)
		resp := uts.postUpload(t, body, -1, nil)
		decodeUploadSuccess(t, resp)
	})
	t.Run("64KiB+1 rejected", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		content := []byte("content")
		body := buildUploadBody(uploadBodyOpts{fileContent: content})
		body = padPreambleToOverhead(t, body, int64(len(content)), uploadOverheadLimit+1)
		resp := uts.postUpload(t, body, -1, nil)
		assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
		assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
	})
}

// --- spec：multipart 畸形变体（400） ---

func TestUploadMalformedVariants(t *testing.T) {
	t.Run("empty body", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		resp := uts.postUpload(t, nil, -1, nil)
		assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
	})
	t.Run("missing boundary", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: []byte("x")}), -1,
			map[string]string{"Content-Type": "multipart/form-data"})
		assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
	})
	t.Run("no file part", func(t *testing.T) {
		uts := newUploadTestServer(t, nil)
		resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{noFile: true}), -1, nil)
		assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
		assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
	})
}

// --- spec 错误表：500 磁盘写失败（不伪造业务码，清理 .partial） ---

func TestUploadDiskWriteFailure500(t *testing.T) {
	real := uploadsinfra.NewStore(t.TempDir())
	store := &errUploadStore{StorePort: real, writeErr: fmt.Errorf("write a.partial: no space left on device")}
	uts := newUploadTestServer(t, &uploadServerOpts{store: store})
	resp := uts.postUpload(t, buildUploadBody(uploadBodyOpts{fileContent: []byte("x")}), -1, nil)
	assertError(t, resp, http.StatusInternalServerError, CodeInternal)
	if calls := store.removedCalls(); len(calls) != 1 {
		t.Errorf("RemovePartial calls = %v, want exactly 1 (cleanup on failure)", calls)
	}
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// --- HTTP/application 联动停滞：四类阻塞下计时器触发取消 ---

// TestUploadStallDuringPayloadRead（读取阻塞期间）：payload 中途停滞 → 读取被
// 真实中断、固定 408、不返回 201、收尾异步清理 .partial。
func TestUploadStallDuringPayloadRead(t *testing.T) {
	uts := newUploadTestServer(t, &uploadServerOpts{stall: uploadStallTimout})
	content := bytes.Repeat([]byte("A"), int(uploadTestM)/2)
	full := buildUploadBody(uploadBodyOpts{fileContent: content})
	prefix := full[:len(full)-len(content)/2-16] // 发送至 payload 中段后停滞
	c := newStallUploadClient(t, uts.srv.URL, false, int64(len(full)), uploadTestContentType)
	c.send(prefix)

	start := time.Now()
	status, body := c.readResponse()
	assertStallResponse(t, status, body, time.Since(start), uploadStallTimout)
	awaitStallCleanup(t, uts.dir, uploadTestTaskID)
}

// TestUploadStallFirstPartIncomplete（首 part 未读完）：connId part 头部中途停滞。
func TestUploadStallFirstPartIncomplete(t *testing.T) {
	uts := newUploadTestServer(t, &uploadServerOpts{stall: uploadStallTimout})
	full := buildUploadBody(uploadBodyOpts{fileContent: []byte("x")})
	c := newStallUploadClient(t, uts.srv.URL, false, int64(len(full)), uploadTestContentType)
	c.send(full[:40]) // 首 part 头部行中途

	start := time.Now()
	status, body := c.readResponse()
	assertStallResponse(t, status, body, time.Since(start), uploadStallTimout)
	awaitStallCleanup(t, uts.dir, uploadTestTaskID)
}

// TestUploadStallFileHeaderBlocked（file header 阻塞）：connId part 完成、file part
// 头部中途停滞（零落盘，408 固定文案）。
func TestUploadStallFileHeaderBlocked(t *testing.T) {
	uts := newUploadTestServer(t, &uploadServerOpts{stall: uploadStallTimout})
	full := buildUploadBody(uploadBodyOpts{fileContent: []byte("x")})
	idx := bytes.Index(full, []byte(`name="file"`))
	if idx < 0 {
		t.Fatal("file part header not found in body")
	}
	c := newStallUploadClient(t, uts.srv.URL, false, int64(len(full)), uploadTestContentType)
	c.send(full[:idx+8]) // file part 头部中途

	start := time.Now()
	status, body := c.readResponse()
	assertStallResponse(t, status, body, time.Since(start), uploadStallTimout)
	awaitStallCleanup(t, uts.dir, uploadTestTaskID)
}

// TestUploadStallTailWaitingEOF（尾部等待 EOF）：完整 body（含结束 boundary）经
// chunked 发送但不发结束 chunk——epilogue 消费阻塞至停滞取消，读取被中断。
func TestUploadStallTailWaitingEOF(t *testing.T) {
	uts := newUploadTestServer(t, &uploadServerOpts{stall: uploadStallTimout})
	full := buildUploadBody(uploadBodyOpts{fileContent: []byte("content")})
	c := newStallUploadClient(t, uts.srv.URL, true, 0, uploadTestContentType)
	c.send([]byte(fmt.Sprintf("%x\r\n", len(full))))
	c.send(full)
	// 不发送结束 chunk：请求体悬置。

	start := time.Now()
	status, body := c.readResponse()
	assertStallResponse(t, status, body, time.Since(start), uploadStallTimout)
	awaitStallCleanup(t, uts.dir, uploadTestTaskID)
}

// --- R6（最终评审 ora-14）：停滞进度覆盖全部请求体读取 ---

// TestUploadSlowDripReadsKeepUploadAlive：header/connId/file header 阶段持续小块
// 读取跨越初始停滞阈值——旧实现仅在 payload 落盘时刷新计时器，会在此误取消 408；
// 每次非零读取上报进度后不取消，最终 201。
func TestUploadSlowDripReadsKeepUploadAlive(t *testing.T) {
	uts := newUploadTestServer(t, &uploadServerOpts{stall: time.Second})
	full := buildUploadBody(uploadBodyOpts{fileContent: []byte("content")})
	c := newStallUploadClient(t, uts.srv.URL, false, int64(len(full)), uploadTestContentType)
	for off := 0; off < len(full); off += 32 {
		c.send(full[off:min(off+32, len(full))])
		time.Sleep(300 * time.Millisecond) // 首个 payload 字节前跨越 1s 阈值
	}
	status, _ := c.readResponse()
	if !strings.Contains(status, "201") {
		t.Fatalf("status %q, want 201 (non-zero reads must refresh stall timer)", status)
	}
}

// --- R7（最终评审 ora-14）：坏 Content-Type 立即注销活跃上传、无悬挂收尾 ---

// TestUploadBadContentTypeAbortsRegistration：登记后 boundary 非法返回 400，活跃项
// 立即注销（AbortUpload 恰一次）；停滞计时器窗口过后无悬挂收尾（bug 形态：
// stallCancel 永久等待 <-u.done，goroutine 与登记均无法释放）。
func TestUploadBadContentTypeAbortsRegistration(t *testing.T) {
	var counted *countingUploadOrch
	uts := newUploadTestServer(t, &uploadServerOpts{
		stall: 150 * time.Millisecond,
		onWired: func(s *Server, orch *appuploads.Orchestrator) {
			counted = &countingUploadOrch{inner: orch}
			s.SetUploadAdmission(counted)
		},
	})
	body := buildUploadBody(uploadBodyOpts{fileContent: []byte("x")})
	// raw TCP 客户端：响应后显式关闭连接，goroutine 计数确定性回落（排除
	// keep-alive 空闲连接 goroutine 干扰）。
	baseline := runtime.NumGoroutine()
	c := newStallUploadClient(t, uts.srv.URL, false, int64(len(body)), "multipart/form-data") // 缺 boundary
	c.send(body)
	status, respBody := c.readResponse()
	if !strings.Contains(status, "400") || !strings.Contains(string(respBody), `"code":"invalid_input"`) {
		t.Fatalf("status %q body %q, want 400 invalid_input", status, respBody)
	}
	if err := c.conn.Close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}
	if got := counted.abortCallCount(); got != 1 {
		t.Errorf("AbortUpload calls = %d, want 1 (bad Content-Type must unregister immediately)", got)
	}
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline+1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline+1 {
		t.Errorf("goroutines = %d (baseline %d): dangling stall closeout goroutine", n, baseline)
	}
	if got := counted.abortCallCount(); got != 1 {
		t.Errorf("AbortUpload calls after stall window = %d, want still 1 (no dangling second closeout)", got)
	}
}

// --- R8（最终评审 ora-14）：connId 逐块计入开销预算、首次超限即停 ---

// TestUploadLongConnIDNoFinalBoundary400：connId 字段值超长（2× 开销上限）且其后
// 无结束 boundary → 预算内即刻 400，无文件落盘（不整字段读入内存后才判定）。
func TestUploadLongConnIDNoFinalBoundary400(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	var b bytes.Buffer
	b.WriteString("--" + uploadTestBdry + "\r\n")
	b.WriteString("Content-Disposition: form-data; name=\"connId\"\r\n\r\n")
	b.Write(bytes.Repeat([]byte("a"), 128<<10))
	resp := uts.postUpload(t, b.Bytes(), int64(b.Len()), nil)
	assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// TestUploadConnIDOverheadBeatsTotalLimit400：connId 字段值同时触发非文件开销超限
// 与请求总量兜底超限 → 前置开销按协议解析顺序首次违规返回 400（不是 413）。
func TestUploadConnIDOverheadBeatsTotalLimit400(t *testing.T) {
	uts := newUploadTestServer(t, nil)
	body := buildUploadBody(uploadBodyOpts{
		connID:      strings.Repeat("a", int(uploadTestM)+(2<<20)), // 超过 M+1MiB 总量兜底
		fileContent: []byte("content"),
	})
	resp := uts.postUpload(t, body, -1, nil)
	assertError(t, resp, http.StatusBadRequest, CodeInvalidInput)
	assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
}

// TestUploadConnIDValueOverheadBoundaries：connId 字段值计入非文件开销的精确边界：
// 总开销 64KiB-1/64KiB 接受并正常提交；64KiB+1 → 400。
func TestUploadConnIDValueOverheadBoundaries(t *testing.T) {
	content := []byte("c")
	// connLen=8 形态的基准开销（connId 值之外的全部结构字节），据此推导目标长度。
	base := buildUploadBody(uploadBodyOpts{fileContent: content, connID: "aaaaaaaa"})
	baseOverhead := int64(len(base)) - int64(len(content))
	for _, tc := range []struct {
		name       string
		target     int64
		wantStatus int
	}{
		{"64KiB-1 accepted", uploadOverheadLimit - 1, http.StatusCreated},
		{"64KiB accepted", uploadOverheadLimit, http.StatusCreated},
		{"64KiB+1 rejected", uploadOverheadLimit + 1, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connValue := strings.Repeat("a", 8+int(tc.target-baseOverhead))
			uts := newUploadTestServer(t, &uploadServerOpts{
				conns: &fakeUploadConns{current: connValue, found: true},
			})
			body := buildUploadBody(uploadBodyOpts{fileContent: content, connID: connValue})
			resp := uts.postUpload(t, body, int64(len(body)), nil)
			if tc.wantStatus == http.StatusCreated {
				decodeUploadSuccess(t, resp)
				return
			}
			assertError(t, resp, tc.wantStatus, CodeInvalidInput)
			assertNoUploadFiles(t, uts.dir, uploadTestTaskID)
		})
	}
}
