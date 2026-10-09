package task

// charx_fakeserver_test.go 实现 opencode-v1-spi-provider 阶段 0 的脚本化 fake
// opencode server（tasks.md 1.1）：固定监听 127.0.0.1:<port> 的 httptest server，
// 按 internal/infrastructure/opencode/CONTRACT.md 锚点形状应答，SSE 事件可编程
//（初始脚本 + 运行期广播 + 断流注入）。
//
// 真实 *opencode.Client 经 OCFactory 指向本 server，使 characterization fixture
// 覆盖「task 编排 → 真实 client 解析/SSE/重连 → fake 上游」的完整被测路径。

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/opencode"
)

// fakeOCServer 是脚本化 opencode v1 上游（CONTRACT.md 端点契约摘要的实现子集）。
// 认证语义：要求请求携带 Basic Auth（任意凭据可过，模拟已配置 OPENCODE_SERVER_PASSWORD
// 的中间件；真实密码由 task 层随机生成，server 不做值比对）。
type fakeOCServer struct {
	t    *testing.T
	port int
	srv  *httptest.Server

	mu sync.Mutex
	// sessions 按 directory 组织（共享目录的 dir/local-path 任务看到同一列表），
	// 保持插入顺序（ListSessions 返回顺序为对齐 complete/overflow 判定的输入）。
	sessions []fakeSession
	nextID   int

	// healthFailFirst 剩余本次数的 /global/health 返回 503（健康超时脚本）。
	healthFailFirst int

	// statuses 是 GET /session/status 的 sessionID → type 映射。
	statuses map[string]string

	// docMode 控制 GET /doc：prompt_async 路径存在与否 / 500（diffreview 能力探测三态）。
	docMode fakeDocMode

	// titleStatus 控制 PATCH /session/:id 的 HTTP 状态（标题同步三态脚本）；
	// 0 = 正常 200。patchLog 记录 (id,title) 调用序列。
	titleStatus int
	patchLog    []string

	// deleteStatusByID 按sessionID 注入 DELETE 状态码（删除失败脚本）；未命中返回 200+true。
	deleteStatusByID map[string]int

	// sseInitial 是每个新 SSE 连接在 server.connected 后按序投递的事件 JSON。
	sseInitial []string
	// SSE 订阅者（广播）与断流信号。
	subs     []chan string
	reset    chan struct{}
	reqLog   []string // "METHOD path dir=<directory> user=<basic 用户名>"
	replyLog []string // "perm|<requestID>|<body>" / "quest|<requestID>|<body>"

	// expectDirs 登记的预期 directory（A6 残留：/event 与 /session/status 的
	// 目录校验——非空但错误的目录不得被宽容放行）。
	expectDirs map[string]bool

	// titleNetFail=true 时 PATCH /session/:id 中断连接（网络失败脚本，标题第三态）。
	titleNetFail bool

	// deleteCalls 供 fixture 观测上游删除调用次数。
	deleteCalls int
}

type fakeDocMode int

const (
	fakeDocPromptAsync fakeDocMode = iota
	fakeDocNoPromptAsync
	fakeDocBroken
)

type fakeSession struct {
	ID        string
	Directory string
	Title     string
	ParentID  string
	Created   float64
	Updated   float64
}

// newFakeOCServer 在固定端口起 fake server。端口从候选表中选择（本机可用性探测），
// 且要求 P-3..P-1 空闲——端口重试 fixture 依赖「P 之前的相邻死端口」构成确定性
// 端口序列；候选不可用时依次顺延。
func newFakeOCServer(t *testing.T) *fakeOCServer {
	t.Helper()
	for _, p := range []int{50123, 50234, 50345, 50456, 50567, 50678, 50789} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		s := &fakeOCServer{
			t:                t,
			port:             p,
			statuses:         map[string]string{},
			deleteStatusByID: map[string]int{},
			expectDirs:       map[string]bool{},
			reset:            make(chan struct{}),
		}
		hs := httptest.NewUnstartedServer(s)
		hs.Listener = ln
		hs.Start()
		s.srv = hs
		t.Cleanup(hs.Close)
		return s
	}
	t.Fatal("fakeOCServer: no candidate port bindable")
	return nil
}

// --- fixture 编程接口 ---

// seedSession 预置一个 session（返回自身便于链式）。
func (s *fakeOCServer) seedSession(dir, id string, created, updated float64) fakeSession {
	sess := fakeSession{ID: id, Directory: dir, Created: created, Updated: updated}
	s.mu.Lock()
	s.sessions = append(s.sessions, sess)
	s.mu.Unlock()
	return sess
}

// setHealthFailFirst 设定前 n 次 health 返回 503。
func (s *fakeOCServer) setHealthFailFirst(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthFailFirst = n
}

// setStatus 设置 /session/status 映射项。
func (s *fakeOCServer) setStatus(sessionID, statusType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[sessionID] = statusType
}

// expectDirectory 登记预期 directory（A6 残留：/event 与 /session/status 校验用）。
func (s *fakeOCServer) expectDirectory(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expectDirs[dir] = true
}

// checkExpectedDirectory 校验请求 directory 已登记且非空（A6 残留）：
// 非空但未登记的目录 = 协议违规（令测试失败）；返回是否放行。
func (s *fakeOCServer) checkExpectedDirectory(endpoint, dir string) bool {
	if dir == "" {
		s.violation("%s 未携带 directory 参数", endpoint)
		return false
	}
	s.mu.Lock()
	registered := len(s.expectDirs) > 0 && s.expectDirs[dir]
	s.mu.Unlock()
	if !registered {
		s.violation("%s directory=%q 未在 fixture 预期目录集合中登记", endpoint, dir)
		return false
	}
	return true
}

// sessionCount 返回当前 session 总数（跨 directory）。
func (s *fakeOCServer) sessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// sessionByID 返回 session（存在性断言用，如标题写入结果）。
func (s *fakeOCServer) sessionByID(id string) (fakeSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ID == id {
			return sess, true
		}
	}
	return fakeSession{}, false
}

// patchCalls 返回 PATCH 标题调用序列拷贝。
func (s *fakeOCServer) patchCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.patchLog...)
}

// requests 返回已记录的请求序列拷贝（"METHOD path"）。
func (s *fakeOCServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqLog...)
}

// deleteCallsCount 返回 DELETE /session/{id} 的调用总数（删除清理 fixture 断言）。
func (s *fakeOCServer) deleteCallsCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteCalls
}

// broadcastEvent 向全部 SSE 订阅者广播一个事件 envelope（opencode.Event JSON 形状）。
func (s *fakeOCServer) broadcastEvent(t *testing.T, ev opencode.Event) {
	t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("broadcastEvent marshal: %v", err)
	}
	s.broadcastRaw(string(data))
}

// broadcastRaw 向全部 SSE 订阅者广播原始事件 JSON。订阅者缓冲满 = 事件被静默
// 丢弃，直接令测试失败（A6：不得静默吞掉广播——那会让 fixture 在事件未送达时
// 虚假通过等待屏障或误判静默忽略）。
func (s *fakeOCServer) broadcastRaw(payload string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range s.subs {
		select {
		case sub <- payload:
		default:
			s.t.Errorf("fakeOCServer: sse 订阅者缓冲满，广播被丢弃: %.120s", payload)
		}
	}
}

// breakStreams 终止全部当前 SSE 连接（触发客户端断流重连 → onReconnect 对齐屏障）。
func (s *fakeOCServer) breakStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.reset)
	s.reset = make(chan struct{})
}

// waitSubscribers 等待活跃 SSE 订阅者数量达到 want。
func (s *fakeOCServer) waitSubscribers(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.subs)
		s.mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	s.mu.Lock()
	n := len(s.subs)
	s.mu.Unlock()
	t.Fatalf("fakeOCServer: subscribers=%d, want >= %d", n, want)
}

// subscriberCount 返回当前订阅者数（零副作用断言用：非法组合不得建立 SSE）。
func (s *fakeOCServer) subscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// --- HTTP handler（CONTRACT.md 端点契约摘要） ---

// violation 记录协议违规（A6：错误请求令测试失败，而非被宽容 server 吞掉——
// 保护网必须能发现 client 侧 directory 路由/请求形状漂移）。
func (s *fakeOCServer) violation(format string, args ...interface{}) {
	s.t.Errorf("fakeOCServer 协议违规: "+format, args...)
}

// ServeHTTP 入口：认证（Basic Auth 必须携带）+ 请求日志（方法/路径/directory/
// 认证形状，A6）。
func (s *fakeOCServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pw, ok := r.BasicAuth()
	if !ok || pw == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.record(r.Method, r.URL.Path, r.URL.Query().Get("directory"), user)

	switch {
	case r.URL.Path == "/global/health" && r.Method == http.MethodGet:
		s.handleHealth(w)
	case r.URL.Path == "/doc" && r.Method == http.MethodGet:
		s.handleDoc(w)
	case r.URL.Path == "/session" && r.Method == http.MethodGet:
		s.handleList(w, r)
	case r.URL.Path == "/session" && r.Method == http.MethodPost:
		s.handleCreate(w, r)
	case r.URL.Path == "/session/status" && r.Method == http.MethodGet:
		s.handleStatus(w, r)
	case strings.HasPrefix(r.URL.Path, "/session/") && r.Method == http.MethodGet:
		s.handleGet(w, r)
	case strings.HasPrefix(r.URL.Path, "/session/") && r.Method == http.MethodDelete:
		s.handleDelete(w, r)
	case strings.HasPrefix(r.URL.Path, "/session/") && r.Method == http.MethodPatch:
		s.handlePatchTitle(w, r)
	case r.URL.Path == "/event" && r.Method == http.MethodGet:
		s.handleEvent(w, r)
	case r.URL.Path == "/permission" && r.Method == http.MethodGet:
		// experimental pending 列表（CONTRACT.md）：空列表 = 能力可用且无 pending。
		writeJSON(w, http.StatusOK, []interface{}{})
	case r.URL.Path == "/question" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, []interface{}{})
	case strings.HasPrefix(r.URL.Path, "/permission/") && strings.HasSuffix(r.URL.Path, "/reply") && r.Method == http.MethodPost:
		s.handleReply(w, r, "/permission/")
	case strings.HasPrefix(r.URL.Path, "/question/") && strings.HasSuffix(r.URL.Path, "/reply") && r.Method == http.MethodPost:
		s.handleReply(w, r, "/question/")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *fakeOCServer) record(method, p, dir, user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqLog = append(s.reqLog, fmt.Sprintf("%s %s dir=%s user=%s", method, p, dir, user))
}

// handleReply 校验 reply 请求形状（A8）：路径按真实端点形状剥离
// （POST /permission/{requestID}/reply、POST /question/{requestID}/reply，
// 对照 permission_reply.go:52 的 client 路径构造），requestID 非空且不含路径段；
// body 必须是非 null 的 JSON 对象，permission reply 必须携带字符串 reply 字段且
// 值 ∈ {once, reject}（client 固定发送 {"reply": ...}，task-permission-mode D6 契约）；
// 违规令测试失败并返回 400。合法请求记录 replyLog。
// 成功响应为 JSON true（permission_reply.go 成功回包形状，非 _tag 对象）。
func (s *fakeOCServer) handleReply(w http.ResponseWriter, r *http.Request, pathPrefix string) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, pathPrefix), "/reply")
	if id == "" || strings.Contains(id, "/") {
		s.violation("reply 路径 requestID 非法: %q（前缀 %q）", r.URL.Path, pathPrefix)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4*1024))
	var obj map[string]interface{}
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		s.violation("reply body 不是 JSON 对象（含 null body）: %q", string(body))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	kind := "quest"
	if pathPrefix == "/permission/" {
		kind = "perm"
		v, ok := obj["reply"].(string)
		if !ok {
			s.violation("permission reply 缺失/非字符串 reply 字段: %q", string(body))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if v != "once" && v != "reject" {
			s.violation("permission reply 值越界: %q（契约值域 once/reject）", v)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	// question reply 的 body 契约在 opencode client 中尚无实现方
	//（CONTRACT.md experimental）；仅强制非 null 对象，不虚构必填字段。
	s.mu.Lock()
	s.replyLog = append(s.replyLog, kind+"|"+id+"|"+string(body))
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, true)
}

// replyCalls 返回 reply 调用记录拷贝。
func (s *fakeOCServer) replyCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.replyLog...)
}

func (s *fakeOCServer) handleHealth(w http.ResponseWriter) {
	s.mu.Lock()
	fail := s.healthFailFirst > 0
	if fail {
		s.healthFailFirst--
	}
	s.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"healthy": true, "version": opencode.ContractBaseline})
}

func (s *fakeOCServer) handleDoc(w http.ResponseWriter) {
	s.mu.Lock()
	mode := s.docMode
	s.mu.Unlock()
	if mode == fakeDocBroken {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	paths := map[string]interface{}{}
	if mode == fakeDocPromptAsync {
		paths["/session/{sessionID}/prompt_async"] = map[string]interface{}{
			"post": map[string]interface{}{"operationId": "session.prompt_async"},
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"paths": paths})
}

// handleList GET /session?directory=&limit=：按 directory 过滤 + 应用 limit 截断
// （count==limit 由 client 判定溢出，ErrSessionOverflow）。
func (s *fakeOCServer) handleList(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("directory")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 1000
	}
	s.mu.Lock()
	var out []map[string]interface{}
	for _, sess := range s.sessions {
		if sess.Directory != dir {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, sessionJSON(sess))
	}
	s.mu.Unlock()
	if out == nil {
		out = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *fakeOCServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("directory")
	var body struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.nextID++
	sess := fakeSession{
		ID:        fmt.Sprintf("ses_fake_%03d", s.nextID),
		Directory: dir,
		Title:     body.Title,
		Created:   1000 + float64(s.nextID),
		Updated:   1000 + float64(s.nextID),
	}
	s.sessions = append(s.sessions, sess)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, sessionJSON(sess))
}

// dirMismatchOrAbsent 查找 session 并校验请求 directory（A6：单 session 端点的
// directory 路由必须命中 session 实际目录——client 侧 directory 漂移令测试失败）。
// 返回 (session, found, dirViolation)。directory 为空视为违规（生产路径恒显式携带）。
func (s *fakeOCServer) dirMismatchOrAbsent(r *http.Request, id string) (fakeSession, bool, bool) {
	dir := r.URL.Query().Get("directory")
	if dir == "" {
		s.violation("%s %s 未携带 directory 参数", r.Method, r.URL.Path)
		return fakeSession{}, false, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ID == id {
			if sess.Directory != dir {
				s.violation("%s %s directory=%q 与 session 实际目录 %q 不符", r.Method, r.URL.Path, dir, sess.Directory)
				return fakeSession{}, false, true
			}
			return sess, true, false
		}
	}
	return fakeSession{}, false, false
}

// handleStatus GET /session/status?directory=：directory 非空（真实消费路径）时
// 必须命中预期目录集合且仅返回该目录 session 的状态（A6 残留：错误目录 → violation，
// 不因 statuses 为空而静默返回相同结果）；directory 为空（能力探测路径 Probe）返回
// 全量映射。
func (s *fakeOCServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("directory")
	if dir != "" && !s.checkExpectedDirectory("/session/status", dir) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	dirOf := func(sid string) (string, bool) {
		for _, sess := range s.sessions {
			if sess.ID == sid {
				return sess.Directory, true
			}
		}
		return "", false
	}
	out := map[string]interface{}{}
	for sid, typ := range s.statuses {
		if dir != "" {
			sd, known := dirOf(sid)
			if known && sd != dir {
				continue
			}
			if !known {
				s.violation("/session/status 引用不存在的 session %s（directory=%s）", sid, dir)
			}
		}
		out[sid] = map[string]interface{}{"type": typ}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *fakeOCServer) handleGet(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	sess, found, violation := s.dirMismatchOrAbsent(r, id)
	if violation {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, sessionJSON(sess))
}

func (s *fakeOCServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	_, _, violation := s.dirMismatchOrAbsent(r, id)
	if violation {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.deleteCalls++
	status, injected := s.deleteStatusByID[id]
	if injected && status != http.StatusOK {
		s.mu.Unlock()
		w.WriteHeader(status)
		return
	}
	kept := s.sessions[:0]
	for _, sess := range s.sessions {
		if sess.ID != id {
			kept = append(kept, sess)
		}
	}
	s.sessions = kept
	s.mu.Unlock()
	// CONTRACT.md：200 + JSON true（非 204）。
	writeJSON(w, http.StatusOK, true)
}

func (s *fakeOCServer) handlePatchTitle(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	var body struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.patchLog = append(s.patchLog, id+"|"+body.Title)
	status := s.titleStatus
	netFail := s.titleNetFail
	s.mu.Unlock()
	if netFail {
		// 网络失败脚本：响应建立前中断连接（client 得到传输层错误，非 HTTP 状态码）。
		panic(http.ErrAbortHandler)
	}
	_, found, violation := s.dirMismatchOrAbsent(r, id)
	if violation {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	s.mu.Lock()
	var sess fakeSession
	found = false
	for i := range s.sessions {
		if s.sessions[i].ID == id {
			s.sessions[i].Title = body.Title
			sess = s.sessions[i]
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, sessionJSON(sess))
}

// handleEvent SSE GET /event：首事件 server.connected（client 就绪信号），随后按序
// 投递初始脚本事件，再进入广播循环。reset 信号 = 断流（handler 返回关闭连接，
// client 指数退避重连 → onReconnect）。
// directory 校验（A6 残留）：缺失或「非空但未登记」的目录 = 协议违规（令测试
// 失败）——SSE 订阅必须路由到本 fixture 任务的目录，错误目录订阅同一广播流会
// 让事件串流被宽容。
func (s *fakeOCServer) handleEvent(w http.ResponseWriter, r *http.Request) {
	if !s.checkExpectedDirectory("/event", r.URL.Query().Get("directory")) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	write := func(payload string) bool {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !write(`{"type":"server.connected","properties":{}}`) {
		return
	}
	s.mu.Lock()
	initial := append([]string(nil), s.sseInitial...)
	reset := s.reset
	s.mu.Unlock()
	for _, ev := range initial {
		if !write(ev) {
			return
		}
	}
	sub := make(chan string, 64)
	s.mu.Lock()
	s.subs = append(s.subs, sub)
	reset = s.reset
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for i, ch := range s.subs {
			if ch == sub {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-reset:
			return
		case msg := <-sub:
			if !write(msg) {
				return
			}
		}
	}
}

// sessionJSON 构造 client parseSession 可接受的 Session 形状（顶层 id + time.updated）。
func sessionJSON(sess fakeSession) map[string]interface{} {
	return map[string]interface{}{
		"id":       sess.ID,
		"title":    sess.Title,
		"parentID": sess.ParentID,
		"time": map[string]interface{}{
			"created": sess.Created,
			"updated": sess.Updated,
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- charx fixture 事件构造 helper（CONTRACT.md 字段路径） ---

// charxSessionEvent 构造 session.created/updated/deleted 事件（info.id + directory
// 同源校验 + time；parentID 可选）。
func charxSessionEvent(typ, sid, directory string, updated float64, parentID string) opencode.Event {
	info := map[string]interface{}{
		"id":        sid,
		"directory": directory,
		"time":      map[string]interface{}{"created": updated, "updated": updated},
	}
	if parentID != "" {
		info["parentID"] = parentID
	}
	return opencode.Event{Type: typ, Properties: map[string]interface{}{"info": info}}
}

// charxStatusEvent 构造 session.status 事件（properties.sessionID + status.type）。
func charxStatusEvent(sid, statusType string) opencode.Event {
	return opencode.Event{Type: "session.status", Properties: map[string]interface{}{
		"sessionID": sid,
		"status":    map[string]interface{}{"type": statusType},
	}}
}

// charxPermAsked 构造 permission.asked 事件；omitField 允许制造缺字段畸形事件。
func charxPermAsked(typ, id, sid, permission string, patterns []string, omitField string) opencode.Event {
	props := map[string]interface{}{}
	if id != "" || omitField != "id" {
		props["id"] = id
	}
	if omitField != "sessionID" {
		props["sessionID"] = sid
	}
	if omitField != "permission" {
		props["permission"] = permission
	}
	if omitField != "patterns" {
		props["patterns"] = patterns
	}
	return opencode.Event{Type: typ, Properties: props}
}

// charxPermAskedMeta 构造带 metadata 完整对象的 permission.asked 事件
// （A1/A3：attention DTO 断言保留 metadata 原对象）。
func charxPermAskedMeta(typ, id, sid, permission string, patterns []string, metadata map[string]interface{}) opencode.Event {
	ev := charxPermAsked(typ, id, sid, permission, patterns, "")
	ev.Properties["metadata"] = metadata
	return ev
}

// charxPermReplied 构造 permission.replied / permission.v2.replied 事件。
func charxPermReplied(typ, sid, requestID string) opencode.Event {
	return opencode.Event{Type: typ, Properties: map[string]interface{}{
		"sessionID": sid, "requestID": requestID,
	}}
}

// charxQuestAsked 构造 question.asked 事件；emptyQuestions 制造空 questions 畸形。
func charxQuestAsked(typ, id, sid, header, question string, emptyQuestions bool) opencode.Event {
	questions := []map[string]interface{}{{"header": header, "question": question}}
	if emptyQuestions {
		questions = nil
	}
	props := map[string]interface{}{}
	if id != "" {
		props["id"] = id
	}
	props["sessionID"] = sid
	props["questions"] = questions
	return opencode.Event{Type: typ, Properties: props}
}

// charxQuestReplied 构造 question.replied / rejected（v1/v2 家族）事件。
func charxQuestReplied(typ, sid, requestID string) opencode.Event {
	return opencode.Event{Type: typ, Properties: map[string]interface{}{
		"sessionID": sid, "requestID": requestID,
	}}
}

// sortedStrings 返回排序拷贝（store dump 行集排序用）。
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
