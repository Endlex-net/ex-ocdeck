// Package api：批注与提交（diff review）HTTP 端点（diff-review-workbench design.md D8）。
//
// handler 仅做 HTTP/DTO 编解码与错误映射；领域校验全部在 diffreview.Service（lane 3）。
// 所有带 body 的端点 MUST 经统一 helper decodeBoundedJSON（wire 上限 + 首值解码 +
// 强制 io.EOF + MaxBytesError 分类）。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"ocdeck/internal/application"
	"ocdeck/internal/application/diffreview"
)

// wire 上限（design.md D8 逐端点表）。
const (
	annotationCreateMaxBytes int64 = 1 << 20   // 1MiB：≈6× snapshot/comment decoded 上限 128KB + 结构余量
	annotationPatchMaxBytes  int64 = 512 << 10 // 512KiB
	submissionCreateMaxBytes int64 = 512 << 10 // 512KiB：note decoded ≤65536 + annotations ≤500 + 结构余量
	gitFileWriteMaxBytes     int64 = 4 << 20   // 4MiB：≈6× content decoded 上限 512KiB + 结构余量
)

// decodeBoundedJSON 统一有界 JSON 请求体解码（design.md D8，tasks 4.1）：
//  1. 安装 http.MaxBytesReader 限制 wire body 上限（超限读取返回 *http.MaxBytesError）。
//  2. 解码首个 JSON 值到 v。
//  3. 再次解码并强制 io.EOF——拒绝尾随数据/第二 JSON 值（公共 decodeJSON 无界且只解首值）；
//     超限尾随数据/第二 JSON 值在第二次解码同样命中 MaxBytesError，与首值超限同分类。
//  4. MaxBytesError 与解析失败统一映射 invalid_input；超限零业务副作用
//     （handler 在解码失败时不得触达任何 service 调用）。
func decodeBoundedJSON(r *http.Request, v any, limit int64) *ApiError {
	if r.Body == nil {
		return NewError(CodeInvalidInput, "request body is required")
	}
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return boundedJSONDecodeErr(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return boundedJSONDecodeErr(err)
		}
		return NewError(CodeInvalidInput, "invalid JSON body: unexpected trailing data")
	}
	return nil
}

// boundedJSONDecodeErr 将首值解码错误分类：MaxBytesError → invalid_input（携带上限），
// 其余（畸形 JSON/类型不符等）→ invalid_input。
func boundedJSONDecodeErr(err error) *ApiError {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return NewError(CodeInvalidInput, fmt.Sprintf("request body exceeds %d bytes limit", mbe.Limit))
	}
	return NewError(CodeInvalidInput, "invalid JSON body")
}

// --- DTO（design.md D8 逐字，JSON 字段一律 camelCase） ---

// annotationDTO 批注 wire 形态。stale 由 D4 惰性计算，不落库。
type annotationDTO struct {
	ID                string `json:"id"`
	Path              string `json:"path"`
	Side              string `json:"side"`
	Ref               string `json:"ref"`
	Untracked         bool   `json:"untracked"`
	StartLine         int    `json:"startLine"`
	EndLine           int    `json:"endLine"`
	SnapshotStartLine int    `json:"snapshotStartLine"`
	SnapshotLineCount int    `json:"snapshotLineCount"`
	Snapshot          string `json:"snapshot"`
	Comment           string `json:"comment"`
	Revision          int    `json:"revision"`
	Stale             bool   `json:"stale"`
	CreatedAt         int64  `json:"createdAt"`
	UpdatedAt         int64  `json:"updatedAt"`
}

func newAnnotationDTO(rec diffreview.DiffAnnotationRecord, stale bool) annotationDTO {
	return annotationDTO{
		ID:                rec.ID,
		Path:              rec.Path,
		Side:              rec.Side,
		Ref:               rec.Ref,
		Untracked:         rec.Untracked,
		StartLine:         rec.StartLine,
		EndLine:           rec.EndLine,
		SnapshotStartLine: rec.SnapshotStartLine,
		SnapshotLineCount: rec.SnapshotLineCount,
		Snapshot:          rec.Snapshot,
		Comment:           rec.Comment,
		Revision:          rec.Revision,
		Stale:             stale,
		CreatedAt:         rec.CreatedAt,
		UpdatedAt:         rec.UpdatedAt,
	}
}

// submissionItemDTO 提交快照条目 wire 形态。
// 不含 snapshotLineCount：由 snapshot 行数推导，不属于提交快照契约（design.md D8 DTO 注）。
type submissionItemDTO struct {
	AnnotationID      string `json:"annotationId"`
	Path              string `json:"path"`
	Side              string `json:"side"`
	Ref               string `json:"ref"`
	Untracked         bool   `json:"untracked"`
	StartLine         int    `json:"startLine"`
	EndLine           int    `json:"endLine"`
	SnapshotStartLine int    `json:"snapshotStartLine"`
	Snapshot          string `json:"snapshot"`
	Comment           string `json:"comment"`
}

func newSubmissionItemDTO(it diffreview.DiffReviewSubmissionItemRecord) submissionItemDTO {
	return submissionItemDTO{
		AnnotationID:      it.AnnotationID,
		Path:              it.Path,
		Side:              it.Side,
		Ref:               it.Ref,
		Untracked:         it.Untracked,
		StartLine:         it.StartLine,
		EndLine:           it.EndLine,
		SnapshotStartLine: it.SnapshotStartLine,
		Snapshot:          it.Snapshot,
		Comment:           it.Comment,
	}
}

// submissionDTO 提交 wire 形态。sentAt 仅 status=sent 时非 null（number|null）；
// items 始终存在（可为空数组，非 null）。
type submissionDTO struct {
	ID        string              `json:"id"`
	Status    string              `json:"status"`
	Note      string              `json:"note"`
	Payload   string              `json:"payload"`
	Truncated bool                `json:"truncated"`
	Error     string              `json:"error"`
	CreatedAt int64               `json:"createdAt"`
	SentAt    *int64              `json:"sentAt"`
	Items     []submissionItemDTO `json:"items"`
}

func newSubmissionDTO(rec diffreview.DiffReviewSubmissionRecord, items []diffreview.DiffReviewSubmissionItemRecord) submissionDTO {
	dto := submissionDTO{
		ID:        rec.ID,
		Status:    rec.Status,
		Note:      rec.Note,
		Payload:   rec.Payload,
		Truncated: rec.Truncated,
		Error:     rec.Error,
		CreatedAt: rec.CreatedAt,
		Items:     make([]submissionItemDTO, 0, len(items)),
	}
	if rec.Status == "sent" && rec.SentAt != 0 {
		sentAt := rec.SentAt
		dto.SentAt = &sentAt
	}
	for _, it := range items {
		dto.Items = append(dto.Items, newSubmissionItemDTO(it))
	}
	return dto
}

// submitCapabilityDTO 提交能力（design.md D8）。
type submitCapabilityDTO struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// annotationsListResponse GET /annotations 响应。
type annotationsListResponse struct {
	Annotations      []annotationDTO     `json:"annotations"`
	SubmitCapability submitCapabilityDTO `json:"submitCapability"`
}

// --- 请求 DTO ---

// createAnnotationReq POST /annotations 请求（design.md D8 表）。
type createAnnotationReq struct {
	Path              string `json:"path"`
	Side              string `json:"side"`
	Ref               string `json:"ref"`
	Untracked         bool   `json:"untracked"`
	StartLine         int    `json:"startLine"`
	EndLine           int    `json:"endLine"`
	SnapshotStartLine int    `json:"snapshotStartLine"`
	SnapshotLineCount int    `json:"snapshotLineCount"`
	Snapshot          string `json:"snapshot"`
	Comment           string `json:"comment"`
}

// updateAnnotationCommentReq PATCH /annotations/{aid} 请求。
type updateAnnotationCommentReq struct {
	Comment string `json:"comment"`
}

// submissionItemReq 提交批次条目（id + revision；revision MUST 为 1..MaxInt64 JSON 整数）。
type submissionItemReq struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

// createSubmissionReq POST /annotation-submissions 请求。
type createSubmissionReq struct {
	Annotations []submissionItemReq `json:"annotations"`
	Note        string              `json:"note"`
}

// fileEditWriteReq POST /git/file 请求（design.md D8/D5）。
type fileEditWriteReq struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	BaseHash   string `json:"baseHash"`
	LineEnding string `json:"lineEnding"`
	BaseMode   string `json:"baseMode"`
}

// fileEditWriteResp POST /git/file 响应。
type fileEditWriteResp struct {
	BaseHash string `json:"baseHash"`
}

// fileEditEditableDTO / git/file 读取判别联合 editable=true 分支（design.md D8/D5）。
// 两组分支结构互斥 marshal，保证 wire 上不出现空零值字段。
type fileEditEditableDTO struct {
	Editable   bool   `json:"editable"`
	Content    string `json:"content"`
	BaseHash   string `json:"baseHash"`
	LineEnding string `json:"lineEnding"`
	HasBom     bool   `json:"hasBom"`
	Mode       string `json:"mode"`
}

// fileEditNotEditableDTO 判别联合 editable=false 分支。
type fileEditNotEditableDTO struct {
	Editable   bool   `json:"editable"`
	ReasonCode string `json:"reasonCode"`
	Reason     string `json:"reason"`
}

// --- 路由 ---

// registerAnnotationRoutes 注册批注/提交路由（design.md D8 表）。
// s.diffreview 为 nil 时跳过（延迟注入 SetDiffReviewService 后需 RebuildRoutes）。
func (s *Server) registerAnnotationRoutes(mux *http.ServeMux) {
	if s.diffreview == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/tasks/{id}/annotations", s.handleListAnnotations)
	mux.HandleFunc("POST /api/v1/tasks/{id}/annotations", s.handleCreateAnnotation)
	mux.HandleFunc("PATCH /api/v1/tasks/{id}/annotations/{aid}", s.handleUpdateAnnotationComment)
	mux.HandleFunc("DELETE /api/v1/tasks/{id}/annotations/{aid}", s.handleDeleteAnnotation)
	mux.HandleFunc("POST /api/v1/tasks/{id}/annotation-submissions", s.handleCreateSubmission)
	mux.HandleFunc("GET /api/v1/tasks/{id}/annotation-submissions", s.handleListSubmissions)
	mux.HandleFunc("POST /api/v1/tasks/{id}/annotation-submissions/{sid}/cancel", s.handleCancelSubmission)
	mux.HandleFunc("DELETE /api/v1/tasks/{id}/annotation-submissions/{sid}", s.handleDeleteSubmission)
}

// writeJSONBody 以 application/json 编码 v（handler 共用）。
func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleListAnnotations GET /api/v1/tasks/{id}/annotations。
// 响应含 submitCapability（state ∈ supported|unsupported|unknown，D8：不暴露 absent）。
func (s *Server) handleListAnnotations(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	views, err := s.diffreview.ListAnnotations(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapDiffReviewErr(err))
		return
	}
	state, reason, err := s.diffreview.SubmitCapability(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapDiffReviewErr(err))
		return
	}
	anns := make([]annotationDTO, 0, len(views))
	for _, v := range views {
		anns = append(anns, newAnnotationDTO(v.DiffAnnotationRecord, v.Stale))
	}
	writeJSONBody(w, http.StatusOK, annotationsListResponse{
		Annotations:      anns,
		SubmitCapability: submitCapabilityDTO{State: string(state), Reason: reason},
	})
}

// handleCreateAnnotation POST /api/v1/tasks/{id}/annotations（wire 上限 1MiB，201）。
// 批注 id 由 api 层生成；领域校验（1-based 闭区间/窗口自洽/来源组合/comment 上限）在 service。
func (s *Server) handleCreateAnnotation(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var req createAnnotationReq
	if ae := decodeBoundedJSON(r, &req, annotationCreateMaxBytes); ae != nil {
		writeApiError(w, ae)
		return
	}
	view, err := s.diffreview.CreateAnnotation(r.Context(), diffreview.CreateDiffAnnotationInput{
		ID:                newID(),
		TaskID:            taskID,
		Path:              req.Path,
		Side:              req.Side,
		Ref:               req.Ref,
		Untracked:         req.Untracked,
		StartLine:         req.StartLine,
		EndLine:           req.EndLine,
		SnapshotStartLine: req.SnapshotStartLine,
		SnapshotLineCount: req.SnapshotLineCount,
		Snapshot:          req.Snapshot,
		Comment:           req.Comment,
	})
	if err != nil {
		writeApiError(w, mapDiffReviewErr(err))
		return
	}
	// F12：service 直接返回真实 Annotation 视图（含 stale，由已取得的 rec 计算）；
	// 不得在写成功后再读持久层（避免 mutation 成功但响应 500 致客户端重试重复创建）。
	writeJSONBody(w, http.StatusCreated, newAnnotationDTO(view.DiffAnnotationRecord, view.Stale))
}

// handleUpdateAnnotationComment PATCH /api/v1/tasks/{id}/annotations/{aid}（512KiB）。
func (s *Server) handleUpdateAnnotationComment(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	annotationID := r.PathValue("aid")
	var req updateAnnotationCommentReq
	if ae := decodeBoundedJSON(r, &req, annotationPatchMaxBytes); ae != nil {
		writeApiError(w, ae)
		return
	}
	view, err := s.diffreview.UpdateComment(r.Context(), taskID, annotationID, req.Comment)
	if err != nil {
		writeApiError(w, mapDiffReviewErr(err))
		return
	}
	// F12：service 直接返回真实 Annotation 视图（含 stale，由已取得的 rec 计算），
	// 不写成功后再读持久层。
	writeJSONBody(w, http.StatusOK, newAnnotationDTO(view.DiffAnnotationRecord, view.Stale))
}

// handleDeleteAnnotation DELETE /api/v1/tasks/{id}/annotations/{aid}（204）。
func (s *Server) handleDeleteAnnotation(w http.ResponseWriter, r *http.Request) {
	err := s.diffreview.DeleteAnnotation(r.Context(), r.PathValue("id"), r.PathValue("aid"))
	if err != nil {
		writeApiError(w, mapDiffReviewErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCreateSubmission POST /api/v1/tasks/{id}/annotation-submissions（512KiB，201）。
// revision 解析失败（非整数/溢出）在 decodeBoundedJSON 即 invalid_input，先于任何 service 调用。
func (s *Server) handleCreateSubmission(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var req createSubmissionReq
	if ae := decodeBoundedJSON(r, &req, submissionCreateMaxBytes); ae != nil {
		writeApiError(w, ae)
		return
	}
	anns := make([]diffreview.SubmissionItemRequest, len(req.Annotations))
	for i, it := range req.Annotations {
		anns[i] = diffreview.SubmissionItemRequest{ID: it.ID, Revision: it.Revision}
	}
	rec, items, err := s.diffreview.CreateSubmission(r.Context(), diffreview.CreateSubmissionRequest{
		TaskID:      taskID,
		Annotations: anns,
		Note:        req.Note,
	})
	if err != nil {
		writeApiError(w, mapDiffReviewErr(err))
		return
	}
	writeJSONBody(w, http.StatusCreated, newSubmissionDTO(rec, items))
}

// submissionsListResponse GET /annotation-submissions 分区列表（design.md D8 排序在 service/store）。
type submissionsListResponse struct {
	Queue    []submissionDTO `json:"queue"`
	History  []submissionDTO `json:"history"`
	Failures []submissionDTO `json:"failures"`
}

// handleListSubmissions GET /api/v1/tasks/{id}/annotation-submissions。
// 单一用例返回一致快照（同一 SQLite 读事务）：queue=queued/sending 按 seq ASC；
// history=sent 按 sent_at DESC,seq DESC；failures=failed/delivery_unknown 按
// created_at DESC,seq DESC（秒级同秒以 seq 决胜）。同一 submission 只出现在一个分区。
func (s *Server) handleListSubmissions(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	parts, err := s.diffreview.ListSubmissionPartitions(r.Context(), taskID)
	if err != nil {
		writeApiError(w, mapDiffReviewErr(err))
		return
	}
	writeJSONBody(w, http.StatusOK, submissionsListResponse{
		Queue:    newSubmissionDTOs(parts.Queue),
		History:  newSubmissionDTOs(parts.History),
		Failures: newSubmissionDTOs(parts.Failures),
	})
}

// newSubmissionDTOs 视图切片 → DTO 切片（空切片保持非 null）。
func newSubmissionDTOs(views []diffreview.SubmissionView) []submissionDTO {
	out := make([]submissionDTO, 0, len(views))
	for _, v := range views {
		out = append(out, newSubmissionDTO(v.Submission, v.Items))
	}
	return out
}

// handleCancelSubmission POST .../annotation-submissions/{sid}/cancel（仅 queued，204）。
func (s *Server) handleCancelSubmission(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	sid := r.PathValue("sid")
	if err := s.diffreview.CancelSubmission(r.Context(), taskID, sid); err != nil {
		writeApiError(w, mapDiffReviewErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteSubmission DELETE .../annotation-submissions/{sid}（仅终态，204）。
func (s *Server) handleDeleteSubmission(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	sid := r.PathValue("sid")
	if err := s.diffreview.DeleteSubmission(r.Context(), taskID, sid); err != nil {
		writeApiError(w, mapDiffReviewErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mapDiffReviewErr 将 diffreview service / adapter 层错误映射为 *ApiError（design.md D8 错误列）：
//   - 文件编辑领域格式错误（FileEditErr，ReasonInvalidInput）→ invalid_input；
//   - service sentinel 错误逐项映射（not_found 仅任务与批注/提交记录；跨任务 id、revision 非法、
//     重复 id、字段超限等 → invalid_input；任务未运行/无锚定/能力非 supported/非 queued 撤回等 →
//     invalid_state；复核失败 → conflict）；
//   - adapter 层错误（文件编辑 git 面、diff 读取，*application.OpError）经 mapTaskErr 透传
//     （invalid_input/invalid_state/conflict/git_error/internal）；
//   - 其余未知错误 → internal（不泄露细节）。
func mapDiffReviewErr(err error) *ApiError {
	var fe *diffreview.FileEditErr
	if errors.As(err, &fe) {
		return NewError(CodeInvalidInput, err.Error())
	}
	switch {
	case errors.Is(err, diffreview.ErrTaskNotFound):
		return NewError(CodeNotFound, err.Error())
	case errors.Is(err, diffreview.ErrUnknownProjectKind):
		return NewError(CodeInternal, err.Error())
	case errors.Is(err, diffreview.ErrDirProject):
		return NewError(CodeInvalidInput, err.Error())
	case errors.Is(err, diffreview.ErrRevisionConflict):
		return NewError(CodeConflict, err.Error())
	case errors.Is(err, diffreview.ErrSubmissionNotFound),
		errors.Is(err, diffreview.ErrAnnotationNotFound):
		return NewError(CodeNotFound, err.Error())
	case errors.Is(err, diffreview.ErrCapabilityNotReady),
		errors.Is(err, diffreview.ErrTaskNotRunning),
		errors.Is(err, diffreview.ErrNoAnchorSession),
		errors.Is(err, diffreview.ErrInvalidState):
		return NewError(CodeInvalidState, err.Error())
	case errors.Is(err, diffreview.ErrEmptyComment),
		errors.Is(err, diffreview.ErrInvalidAnnotationPath),
		errors.Is(err, diffreview.ErrInvalidAnnotationSide),
		errors.Is(err, diffreview.ErrInvalidAnnotationSource),
		errors.Is(err, diffreview.ErrInvalidAnnotationRange),
		errors.Is(err, diffreview.ErrInvalidSnapshotWindow),
		errors.Is(err, diffreview.ErrFieldTooLarge),
		errors.Is(err, diffreview.ErrEmptySubmission),
		errors.Is(err, diffreview.ErrDuplicateAnnotationID),
		errors.Is(err, diffreview.ErrInvalidAnnotationID),
		errors.Is(err, diffreview.ErrTooManyAnnotations),
		errors.Is(err, diffreview.ErrNoteTooLarge),
		errors.Is(err, diffreview.ErrCrossTaskAnnotation),
		errors.Is(err, diffreview.ErrPayloadTooLarge),
		errors.Is(err, diffreview.ErrInvalidAnnotationRevision):
		return NewError(CodeInvalidInput, err.Error())
	}
	if application.OpErrorCode(err) != "" {
		return mapTaskErr(err)
	}
	return NewError(CodeInternal, "internal error")
}
