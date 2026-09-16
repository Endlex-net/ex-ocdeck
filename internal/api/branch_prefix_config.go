// branch_prefix_config.go 实现 GET/PUT /api/v1/config/branch-prefix
// （openspec change task-info-editable D4；镜像 palette_config.go 的 DTO/解码/错误映射）。
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"ocdeck/internal/infrastructure/branchprefix"
)

// branchPrefixPutBodyMax 限制 PUT 请求体上限为 4 KiB（与 palette 同构：超限 400 不进入校验/写盘）。
const branchPrefixPutBodyMax = 4096

func (s *Server) SetBranchPrefixStore(store *branchprefix.Store) {
	s.branchPrefix = store
}

func (s *Server) registerBranchPrefixRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/config/branch-prefix", s.handleGetBranchPrefix)
	mux.HandleFunc("PUT /api/v1/config/branch-prefix", s.handlePutBranchPrefix)
}

type branchPrefixDTO struct {
	Prefix string `json:"prefix"`
}

func branchPrefixDTOFromStore(store *branchprefix.Store) branchPrefixDTO {
	prefix := branchprefix.DefaultPrefix
	if store != nil {
		prefix = store.Prefix()
	}
	return branchPrefixDTO{Prefix: prefix}
}

func writeBranchPrefix(w http.ResponseWriter, dto branchPrefixDTO) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dto)
}

func (s *Server) handleGetBranchPrefix(w http.ResponseWriter, r *http.Request) {
	writeBranchPrefix(w, branchPrefixDTOFromStore(s.branchPrefix))
}

func (s *Server) handlePutBranchPrefix(w http.ResponseWriter, r *http.Request) {
	if s.branchPrefix == nil {
		writeJSONError(w, http.StatusInternalServerError, CodeInternal, "branch prefix store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, branchPrefixPutBodyMax)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusBadRequest, CodeInvalidInput, "request body exceeds 4096 bytes")
			return
		}
		writeJSONError(w, http.StatusBadRequest, CodeInvalidInput, "invalid request body")
		return
	}
	prefix, status, msg := decodeBranchPrefixPutBody(data)
	if status != 0 {
		writeJSONError(w, status, CodeInvalidInput, msg)
		return
	}
	// Put 内部同参数复验；此处先验以区分 422（校验失败）与 500（写盘失败）。
	if err := branchprefix.Validate(prefix); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, CodeInvalidInput, err.Error())
		return
	}
	if err := s.branchPrefix.Put(prefix); err != nil {
		writeJSONError(w, http.StatusInternalServerError, CodeInternal, "save branch prefix failed")
		return
	}
	writeBranchPrefix(w, branchPrefixDTOFromStore(s.branchPrefix))
}

// decodeBranchPrefixPutBody 按 spec 错误矩阵解码（镜像 decodePalettePutBody）：
// 空体/空白/语法/尾随第二值 → 400；顶层非对象/缺键/null/类型错误 → 422。
// 超限在 ReadAll 处拦截，本函数不写盘。
func decodeBranchPrefixPutBody(data []byte) (string, int, string) {
	if isEmptyOrWhitespace(data) {
		return "", http.StatusBadRequest, "request body is required"
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return "", http.StatusBadRequest, "invalid JSON body"
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return "", http.StatusBadRequest, "invalid JSON body"
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", http.StatusUnprocessableEntity, "branch prefix must be a JSON object"
	}
	prefix, err := branchprefix.DecodeConfig(raw)
	if err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return "", http.StatusUnprocessableEntity, err.Error()
		}
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return "", http.StatusBadRequest, "invalid JSON body"
		}
		return "", http.StatusUnprocessableEntity, err.Error()
	}
	return prefix, 0, ""
}
