package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	appuploads "ocdeck/internal/application/uploads"
)

// 上传接口三段计量常量（spec「文件上传接口与安全约束」唯一计量表）：
//   - 文件 payload 0 ≤ N ≤ M 由 application WriteUploadBody 的 M+1 探测强制（→413）；
//   - 非文件开销（preamble/boundary 行/part headers/CRLF/connId 值/epilogue）≤ 64KiB（→400）；
//   - 请求总量 M+1MiB 由 http.MaxBytesReader 兜底（→413）。
const (
	uploadOverheadLimit  = 64 << 10
	uploadTotalSlack     = 1 << 20
	uploadStalledMessage = "upload stalled for 1 hour" // spec 错误表：该文案唯一来源
)

// 上传扫描哨兵错误（handler 映射：overhead/malformed → 400 invalid_input）。
var (
	errUploadOverhead  = errors.New("upload non-file overhead exceeds limit")
	errUploadMalformed = errors.New("malformed multipart body")
)

// uploadOrchestrator 上传编排消费方窄接口（terminal-file-paste-drop 3.2）。
// 准入决策（任务存在/活跃、connId 归属）唯一归属 application 编排，handler 仅做
// multipart 协议解析、三段计量与错误映射；流式落盘经 WriteUploadBody 委托
// infrastructure。*appuploads.Orchestrator 结构性满足。
type uploadOrchestrator interface {
	// AdmitUpload 任务存在/活跃准入（D1 阶段①，先于请求体解析）。
	AdmitUpload(ctx context.Context, taskID string) error
	// RegisterActiveUpload 登记活跃上传并启动停滞计时器（connId 允许空串，
	// 首 part 解析后经 BindConnID 绑定）。
	RegisterActiveUpload(taskID, connID string) (*appuploads.ActiveUpload, error)
	// BindConnID 绑定首 part 解析出的 connId（阶段③准入前调用；异值重绑 → ErrConnAlreadyBound）。
	BindConnID(u *appuploads.ActiveUpload, connID string) error
	// AdmitUploadConn connId 归属准入（D1 阶段③，先于任何文件落盘）。
	AdmitUploadConn(ctx context.Context, taskID, connID string) error
	// BindOriginalName 绑定原始文件名并计算受管落盘名（原始名不进入落盘路径）。
	BindOriginalName(u *appuploads.ActiveUpload, originalName string) string
	// WriteUploadBody 流式写文件内容到 .partial（M+1 探测 → ErrUploadTooLarge；
	// 非零字节块重置停滞计时器）。
	WriteUploadBody(ctx context.Context, u *appuploads.ActiveUpload, r io.Reader) (int64, error)
	// FinalizeUpload per-task 协调锁内复查并提交（停滞取消 → ErrUploadStalled）。
	FinalizeUpload(ctx context.Context, u *appuploads.ActiveUpload) error
	// AbortUpload 异常收尾（幂等：注销登记并清理 .partial；已提交不删文件）。
	AbortUpload(ctx context.Context, u *appuploads.ActiveUpload)
}

// registerUploadRoutes 注册上传路由（terminal-file-paste-drop 3.3）：/api/v1 子 mux
// 统一挂 Bearer 中间件（server.go registerRoutes）。
func (s *Server) registerUploadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/tasks/{taskID}/attachments", s.handleUploadAttachment)
}

// handleUploadAttachment POST /api/v1/tasks/{taskID}/attachments（multipart/form-data）。
//
// 分阶段固定顺序（D1）：Bearer（子 mux 中间件）→ Origin 白名单（复用 checkWSOrigin
// 同一判定，403 先于任何写盘）→ 任务存在/活跃准入（先于请求体解析）→ 登记活跃上传
// （停滞计时器起点）→ 首 part 必须 connId 文本字段（否则 400，文件内容不落盘）→
// connId 归属准入（403）→ 唯一 file part 流式落盘（三段计量）→ 完整尾部校验
// （额外 part/损坏尾部 → 400 并清理）→ 协调锁内 finalize 复查提交 → 201 uploadId。
//
// 登记后契约：所有返回路径统一结束登记——成功经 FinalizeUpload（markDone+注销），
// 失败一律经 uploadBodyFailure / uploadFinalizeFailure（幂等 AbortUpload 或停滞
// 收尾），不得存在绕过收尾的提前 return。停滞进度由扫描器每次非零读取上报
// （u.TouchProgress），覆盖 header/connId/payload/epilogue 全阶段。
//
// 未接线降级：路由随 api.New 即存在，组合根注入（SetUploadAdmission/SetUploadLimits）
// 前到达的请求返回 404 not_found——与旧 server 无该路由不可区分（D7 前端按
// 「上传目标不可用，任务可能已删除或服务端不支持该接口」终止当前项，不永久降级）。
func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	if s.uploadOrch == nil || s.uploadLimits.MaxBytes <= 0 {
		writeError(w, CodeNotFound, "upload pipeline not configured")
		return
	}
	taskID := r.PathValue("taskID")

	// ① Origin → 任务存在/活跃（均先于请求体读取与写盘）。
	if !s.checkWSOrigin(r) {
		writeJSONError(w, http.StatusForbidden, CodeForbidden, "origin not allowed")
		return
	}
	if err := s.uploadOrch.AdmitUpload(r.Context(), taskID); err != nil {
		s.writeUploadAdmissionError(w, err)
		return
	}

	// 登记活跃上传（认证/Origin/任务准入后、开始读取请求体时；connId 尚未解析）。
	u, err := s.uploadOrch.RegisterActiveUpload(taskID, "")
	if err != nil {
		writeError(w, CodeInternal, "upload registration failed")
		return
	}

	// 停滞取消挂接（spec 取消收尾②）：application 计时器触发后 close(CancelCh)，
	// 此处取消读取 ctx 并把读截止时间拨到过去，中断阻塞中的请求体读取；随后
	// finalize 复查给出固定 408。读取正常结束时经 stopWatch 退出。
	readCtx, cancelRead := context.WithCancel(r.Context())
	defer cancelRead()
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-u.CancelCh():
			cancelRead()
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		case <-stopWatch:
		}
	}()

	// 三段计量之外层兜底：请求总量 M+1MiB（按实际读取字节判定，不信任 Content-Length）。
	body := http.MaxBytesReader(w, r.Body, s.uploadLimits.MaxBytes+uploadTotalSlack)

	boundary, ok := multipartBoundary(r)
	if !ok {
		// 登记后的失败统一经 uploadBodyFailure 幂等收尾：立即注销活跃上传并结束
		// 停滞计时（否则计时器触发后 stallCancel 永久等待 done，登记悬挂）。
		s.uploadBodyFailure(w, u, errUploadMalformed)
		return
	}
	sc := newUploadScanner(body, boundary, uploadOverheadLimit, u.TouchProgress)

	// ② 首 part：必须为 connId 文本字段（否则 400，文件内容不落盘）。
	p1, err := sc.nextPart()
	if err != nil {
		s.uploadBodyFailure(w, u, err)
		return
	}
	if p1.formName != "connId" || p1.fileName != "" {
		s.uploadBodyFailure(w, u, errUploadMalformed)
		return
	}
	connValue, err := sc.readFieldValue()
	if err != nil {
		s.uploadBodyFailure(w, u, err)
		return
	}
	connID := string(connValue)

	// ③ connId 归属准入（先于任何文件落盘）。
	if err := s.uploadOrch.BindConnID(u, connID); err != nil {
		s.uploadBodyFailure(w, u, err) // ErrConnAlreadyBound → 400
		return
	}
	if err := s.uploadOrch.AdmitUploadConn(r.Context(), taskID, connID); err != nil {
		s.uploadBodyFailure(w, u, err) // ErrConnNotCurrent → 403；查询故障 → 500
		return
	}

	// ④ 唯一 file part：字段名 file、携带文件名；非 identity 的
	// Content-Transfer-Encoding 拒绝（raw part 语义，不隐式解码）。
	p2, err := sc.nextPart()
	if err != nil {
		s.uploadBodyFailure(w, u, err) // 无 file part（io.EOF）→ 400
		return
	}
	if p2.formName != "file" || p2.fileName == "" || !identityTransferEncoding(p2.cte) {
		s.uploadBodyFailure(w, u, errUploadMalformed)
		return
	}
	s.uploadOrch.BindOriginalName(u, filepath.Base(p2.fileName))

	if _, err := s.uploadOrch.WriteUploadBody(readCtx, u, sc.content()); err != nil {
		s.uploadBodyFailure(w, u, err) // ErrUploadTooLarge → 413；停滞 → 408；其余按下文映射
		return
	}

	// ⑤ 完整尾部校验：唯一 file part 之后必须是结束 boundary（额外 part/损坏尾部 → 400）。
	if err := sc.finish(); err != nil {
		s.uploadBodyFailure(w, u, err)
		return
	}

	// 尾随 epilogue：有界消费至 EOF（计入非文件开销与请求总量兜底）后才能提交。
	if err := sc.drainEpilogue(); err != nil {
		s.uploadBodyFailure(w, u, err)
		return
	}

	// ⑥ 协调锁内复查并提交（提交顺序：rename → sidecar → 入可投递索引）。
	// 用 Background：客户端在收尾瞬间断开不得让提交失败误映射为 500。
	if err := s.uploadOrch.FinalizeUpload(context.Background(), u); err != nil {
		s.uploadFinalizeFailure(w, u, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"uploadId": u.ID})
}

// uploadBodyFailure 请求体解析/落盘/connId 准入失败的统一收尾：优先裁决停滞取消
// （固定 408），其余按 spec 错误表映射。失败一律 AbortUpload 清理（幂等；停滞取消时
// stallCancel 步③已清理，Abort 复核登记后为 no-op）。
func (s *Server) uploadBodyFailure(w http.ResponseWriter, u *appuploads.ActiveUpload, err error) {
	if uploadStalled(u) {
		// 停滞取消已触发：finalize 仅作 408 裁决（不可提交标记，MUST NOT 发布/返回 201）。
		_ = s.uploadOrch.FinalizeUpload(context.Background(), u)
		writeJSONError(w, http.StatusRequestTimeout, CodeInvalidInput, uploadStalledMessage)
		return
	}
	s.uploadOrch.AbortUpload(context.Background(), u)
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, appuploads.ErrUploadTooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "file exceeds size limit")
	case errors.As(err, &mbe):
		writeJSONError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body exceeds size limit")
	case errors.Is(err, appuploads.ErrConnNotCurrent):
		writeJSONError(w, http.StatusForbidden, CodeForbidden, "connection is not the current TUI connection")
	case errors.Is(err, errUploadOverhead), errors.Is(err, errUploadMalformed),
		errors.Is(err, appuploads.ErrConnAlreadyBound), errors.Is(err, appuploads.ErrUploadNotBound),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		writeJSONError(w, http.StatusBadRequest, CodeInvalidInput, "malformed upload body")
	default:
		// 其余（磁盘写失败、任务/存储基础设施故障）→ 500 internal，不伪造业务码。
		writeError(w, CodeInternal, "internal server error")
	}
}

// uploadFinalizeFailure finalize 复查/提交失败的映射（spec 错误表：只有已确认
// 不存在/非活跃/非当前连接才映射业务码，基础设施故障一律 500）。
func (s *Server) uploadFinalizeFailure(w http.ResponseWriter, u *appuploads.ActiveUpload, err error) {
	if errors.Is(err, appuploads.ErrUploadStalled) {
		// 停滞取消：固定 408。.partial 由 stallCancel 步③清理，无需 Abort。
		writeJSONError(w, http.StatusRequestTimeout, CodeInvalidInput, uploadStalledMessage)
		return
	}
	s.uploadOrch.AbortUpload(context.Background(), u)
	switch {
	case errors.Is(err, appuploads.ErrUploadTaskNotFound):
		writeError(w, CodeNotFound, "task not found")
	case errors.Is(err, appuploads.ErrUploadTaskInactive):
		writeJSONError(w, http.StatusConflict, CodeInvalidState, "task is not active")
	case errors.Is(err, appuploads.ErrConnNotCurrent):
		writeJSONError(w, http.StatusForbidden, CodeForbidden, "connection is not the current TUI connection")
	case errors.Is(err, appuploads.ErrUploadNotBound):
		writeJSONError(w, http.StatusBadRequest, CodeInvalidInput, "malformed upload body")
	default:
		writeError(w, CodeInternal, "internal server error")
	}
}

// writeUploadAdmissionError 阶段①任务准入结果映射（AdmitUpload 哨兵）。
func (s *Server) writeUploadAdmissionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, appuploads.ErrUploadTaskNotFound):
		writeError(w, CodeNotFound, "task not found")
	case errors.Is(err, appuploads.ErrUploadTaskInactive):
		writeJSONError(w, http.StatusConflict, CodeInvalidState, "task is not active")
	default:
		// 任务查询基础设施故障（errTaskQuery）→ 500，不误映射 404/409。
		writeError(w, CodeInternal, "internal server error")
	}
}

// uploadStalled 报告停滞取消是否已触发（CancelCh close 前置条件是不可提交标记
// 已在协调锁内生效，见 application stallCancel 步①）。
func uploadStalled(u *appuploads.ActiveUpload) bool {
	select {
	case <-u.CancelCh():
		return true
	default:
		return false
	}
}

// multipartBoundary 从 Content-Type 提取 multipart boundary。
func multipartBoundary(r *http.Request) (string, bool) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		return "", false
	}
	b := params["boundary"]
	return b, b != ""
}

// identityTransferEncoding 报告 CTE 是否为不改变字节的恒等编码（raw part 语义；
// quoted-printable/base64 等变换编码拒绝，不隐式解码改变计量）。
func identityTransferEncoding(cte string) bool {
	switch cte {
	case "", "identity", "7bit", "8bit", "binary":
		return true
	}
	return false
}

// --- 有界 multipart 线上格式扫描器（本接口专用的三段计量实现） ---

// uploadPartHeader part 头部视图（仅本接口关心的字段）。
type uploadPartHeader struct {
	formName string // Content-Disposition name
	fileName string // Content-Disposition filename（文本字段为空）
	cte      string // Content-Transfer-Encoding（小写；缺省为空）
}

// uploadScanner 有界 multipart 扫描器。与 mime/multipart 相比存在的理由是计量
// 精确性：multipart.Reader 内部 bufio 读前哨与结束 boundary 后的 epilogue 均不可
// 观测，无法实现 spec 唯一计量表的「非文件开销恰好 64KiB 通过、首次违规优先、
// epilogue 有界消费至 EOF」。本扫描器逐字节消费并入账：除 file part 内容外的全部
// 线上字节实时计入非文件开销并在越界即刻失败（协议解析顺序首次违规）。
type uploadScanner struct {
	r        io.Reader
	boundary string
	limit    int64  // 非文件开销上限（64KiB）
	progress func() // 请求体非零读取进度回调（活跃上传停滞计时器刷新；nil 安全）

	buf      []byte // 读前窗口（未消费字节）
	eof      bool
	overhead int64 // 已消费的非文件字节

	partsRead  int
	finalSeen  bool // 已消费结束 boundary
	partReady  bool // 上一 part 内容收尾后下一 part 头部就绪
	pendingErr error
}

func newUploadScanner(r io.Reader, boundary string, overheadLimit int64, progress func()) *uploadScanner {
	return &uploadScanner{r: r, boundary: boundary, limit: overheadLimit, progress: progress}
}

// fill 追加读取一块到窗口。每次非零读取在边界上报进度（spec 活跃上传进度表：
// 读取到任何非零请求体字节即刷新 lastProgressAt，覆盖全阶段）。
func (sc *uploadScanner) fill() error {
	if sc.eof {
		return io.EOF
	}
	var tmp [4096]byte
	n, err := sc.r.Read(tmp[:])
	if n > 0 {
		sc.buf = append(sc.buf, tmp[:n]...)
		if sc.progress != nil {
			sc.progress()
		}
	}
	if err == io.EOF {
		sc.eof = true
		return io.EOF
	}
	if err != nil {
		return err
	}
	return nil
}

// account 消费窗口前 n 字节并入账非文件开销（越界即刻失败：首次违规优先）。
func (sc *uploadScanner) account(n int) error {
	sc.buf = sc.buf[n:]
	sc.overhead += int64(n)
	if sc.overhead > sc.limit {
		return errUploadOverhead
	}
	return nil
}

// readLine 消费一行（含结尾 \n；流末尾无 \n 的残余也算一行）。返回行内容（不含
// \n，保留 \r 供调用方裁剪）。结构区行受开销预算约束：未闭合行超预算即失败。
func (sc *uploadScanner) readLine() ([]byte, error) {
	for {
		if i := bytes.IndexByte(sc.buf, '\n'); i >= 0 {
			line := sc.buf[:i]
			if err := sc.account(i + 1); err != nil {
				return nil, err
			}
			return line, nil
		}
		if sc.eof {
			if len(sc.buf) == 0 {
				return nil, io.EOF
			}
			line := sc.buf
			n := len(line)
			sc.buf = nil
			sc.overhead += int64(n)
			if sc.overhead > sc.limit {
				return nil, errUploadOverhead
			}
			return line, nil
		}
		if int64(len(sc.buf)) > sc.limit {
			// 未闭合行已超开销预算：这些字节必然全部计入非文件开销。
			return nil, errUploadOverhead
		}
		if err := sc.fill(); err != nil {
			if err == io.EOF {
				continue
			}
			return nil, err
		}
	}
}

// trimBoundaryLine 裁剪行尾 \r 与传输填充（RFC 2046 LWSP：空格/制表符）。
func trimBoundaryLine(line []byte) []byte {
	for len(line) > 0 {
		switch line[len(line)-1] {
		case '\r', ' ', '\t':
			line = line[:len(line)-1]
		default:
			return line
		}
	}
	return line
}

// nextPart 消费至下一 part 的头部就绪点并解析头部。首个 part 含 preamble 与首个
// boundary 行；此后各 part 的 boundary 行已在其前置内容收尾时消费。
// 到达结束 boundary（含空 multipart）→ io.EOF。
func (sc *uploadScanner) nextPart() (uploadPartHeader, error) {
	if sc.finalSeen {
		return uploadPartHeader{}, io.EOF
	}
	if sc.pendingErr != nil {
		return uploadPartHeader{}, sc.pendingErr
	}
	if sc.partsRead == 0 {
		// preamble + 首 boundary 行（逐行消费，全部计入非文件开销）。
		for boundaryFound := false; !boundaryFound; {
			line, err := sc.readLine()
			if err == io.EOF {
				return uploadPartHeader{}, errUploadMalformed // 无 boundary
			}
			if err != nil {
				return uploadPartHeader{}, err
			}
			switch string(trimBoundaryLine(line)) {
			case "--" + sc.boundary + "--":
				sc.finalSeen = true
				return uploadPartHeader{}, io.EOF // 空 multipart：无 part
			case "--" + sc.boundary:
				boundaryFound = true // 首 part 头部就绪，进入下方头部解析
			default:
				// preamble 行（已入账），继续找首 boundary 行
			}
		}
	} else {
		if !sc.partReady {
			return uploadPartHeader{}, errUploadMalformed
		}
	}

	// 头部行（空行结束；全部计入非文件开销）。
	var h uploadPartHeader
	for {
		line, err := sc.readLine()
		if err == io.EOF {
			return uploadPartHeader{}, errUploadMalformed // 头部中途断流
		}
		if err != nil {
			return uploadPartHeader{}, err
		}
		trimmed := bytes.TrimRight(line, "\r")
		if len(trimmed) == 0 {
			break // 空行：头部结束，内容开始
		}
		name, value, ok := bytes.Cut(trimmed, []byte(":"))
		if !ok {
			return uploadPartHeader{}, errUploadMalformed
		}
		switch strings.ToLower(strings.TrimSpace(string(name))) {
		case "content-disposition":
			_, params, perr := mime.ParseMediaType(strings.TrimSpace(string(value)))
			if perr != nil {
				return uploadPartHeader{}, errUploadMalformed
			}
			h.formName = params["name"]
			h.fileName = params["filename"]
		case "content-transfer-encoding":
			h.cte = strings.ToLower(strings.TrimSpace(string(value)))
		}
	}
	sc.partsRead++
	return h, nil
}

// content 返回当前 part 内容读取器：交付内容字节直到 CRLF--boundary，结束 boundary
// 行在内容收尾时消费并分类（regular → partReady；final → finalSeen）。
func (sc *uploadScanner) content() io.Reader {
	return &uploadContentReader{sc: sc, delim: []byte("\r\n--" + sc.boundary)}
}

type uploadContentReader struct {
	sc    *uploadScanner
	delim []byte
	done  bool
}

// Read 交付内容字节（不计入非文件开销）。内容字节全部交付后消费 delimiter 与
// boundary 行剩余（计入非文件开销），后续 Read 返回 io.EOF。
func (c *uploadContentReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if c.done {
			return n, io.EOF
		}
		if len(c.sc.buf) == 0 {
			if c.sc.eof {
				c.done = true
				return n, errUploadMalformed // 内容在 delimiter 前断流（尾部损坏/截断）
			}
			if err := c.sc.fill(); err != nil {
				if err == io.EOF {
					continue
				}
				c.done = true
				return n, err
			}
			continue
		}
		if i := bytes.Index(c.sc.buf, c.delim); i >= 0 {
			k := copy(p[n:], c.sc.buf[:i])
			c.sc.buf = c.sc.buf[k:]
			n += k
			if k < i {
				return n, nil // 内容残留待下次交付（delimiter 仍在窗口）
			}
			// delimiter 字节属非文件开销（spec 计量表），入账后丢弃。
			if err := c.sc.account(len(c.delim)); err != nil {
				c.done = true
				return n, err
			}
			c.done = true
			if err := c.sc.consumeBoundaryLine(); err != nil {
				return n, err // 尾部损坏：内容已交付，错误同次返回
			}
			return n, nil
		}
		// 无 delimiter：交付除潜在 delimiter 前缀（holdback）外的全部内容。
		keep := len(c.delim) - 1
		if len(c.sc.buf) <= keep {
			if c.sc.eof {
				c.done = true
				return n, errUploadMalformed // 残余不足以构成 delimiter 且已 EOF：尾部损坏
			}
			if err := c.sc.fill(); err != nil {
				if err == io.EOF {
					continue
				}
				c.done = true
				return n, err
			}
			continue
		}
		deliverable := len(c.sc.buf) - keep
		k := copy(p[n:], c.sc.buf[:deliverable])
		c.sc.buf = c.sc.buf[k:] // 仅前移实际交付量（p 将满时残余内容留存）
		n += k
	}
	return n, nil
}

// consumeBoundaryLine 消费 boundary 行剩余（传输填充 + CRLF，计入非文件开销）并
// 分类："--" → 结束 boundary；空/LWSP → regular boundary（下一 part 头部就绪）。
func (sc *uploadScanner) consumeBoundaryLine() error {
	line, err := sc.readLine()
	if err == io.EOF {
		return errUploadMalformed // regular boundary 后断流
	}
	if err != nil {
		return err
	}
	switch string(trimBoundaryLine(line)) {
	case "--":
		sc.finalSeen = true
		return nil
	case "":
		sc.partReady = true
		return nil
	default:
		return errUploadMalformed
	}
}

// readFieldValue 读取当前 part（connId 文本字段）的全部内容。字段值计入非文件
// 开销（spec 计量表：connId 字段值属非文件开销）：逐块入账、首次超限即刻停止，
// 缓存受同一预算约束（不整字段读入内存后才判定）。
func (sc *uploadScanner) readFieldValue() ([]byte, error) {
	var out []byte
	var chunk [4096]byte
	cr := sc.content()
	for {
		n, rerr := cr.Read(chunk[:])
		if n > 0 {
			out = append(out, chunk[:n]...)
			sc.overhead += int64(n)
			if sc.overhead > sc.limit {
				return nil, errUploadOverhead
			}
		}
		if rerr == io.EOF {
			return out, nil
		}
		if rerr != nil {
			return nil, rerr
		}
	}
}

// finish 校验唯一 file part 之后必须是结束 boundary（额外 part/损坏尾部 → 错误）。
func (sc *uploadScanner) finish() error {
	if !sc.finalSeen {
		if _, err := sc.nextPart(); !errors.Is(err, io.EOF) {
			if err == nil {
				return errUploadMalformed // 额外 part
			}
			return err
		}
	}
	if !sc.finalSeen {
		return errUploadMalformed
	}
	return nil
}

// drainEpilogue 结束 boundary 之后的有界尾随消费：读到 EOF，全部计入非文件开销
// （越界即刻失败）与请求总量兜底（MaxBytesReader）。
func (sc *uploadScanner) drainEpilogue() error {
	for {
		if len(sc.buf) == 0 {
			if sc.eof {
				return sc.overheadResult()
			}
			if err := sc.fill(); err != nil {
				if err == io.EOF {
					// fill 可能已同时读入最后一块数据（n>0, io.EOF）：继续循环入账。
					continue
				}
				return err
			}
			continue
		}
		if err := sc.account(len(sc.buf)); err != nil {
			return err
		}
	}
}

func (sc *uploadScanner) overheadResult() error {
	if sc.overhead > sc.limit {
		return errUploadOverhead
	}
	return nil
}
