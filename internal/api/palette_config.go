package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	domainpalette "ocdeck/internal/domain/palette"
	"ocdeck/internal/infrastructure/palette"
)

const paletteConfigPutBodyMax = 1024

func (s *Server) SetPaletteConfigStore(store *palette.Store) {
	s.paletteConfig = store
}

func (s *Server) registerPaletteConfigRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/palette/config", s.handleGetPaletteConfig)
	mux.HandleFunc("PUT /api/v1/palette/config", s.handlePutPaletteConfig)
}

type paletteConfigDTO struct {
	Hotkey      string `json:"hotkey"`
	TriggerWord string `json:"triggerWord"`
	MatchMode   string `json:"matchMode"`
}

func paletteDTOFromStore(store *palette.Store) paletteConfigDTO {
	cfg := domainpalette.DefaultConfig()
	if store != nil {
		cfg = store.Config()
	}
	return paletteConfigDTO{
		Hotkey:      cfg.Hotkey,
		TriggerWord: cfg.TriggerWord,
		MatchMode:   cfg.MatchMode,
	}
}

func writePaletteConfig(w http.ResponseWriter, dto paletteConfigDTO) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dto)
}

func (s *Server) handleGetPaletteConfig(w http.ResponseWriter, r *http.Request) {
	writePaletteConfig(w, paletteDTOFromStore(s.paletteConfig))
}

func (s *Server) handlePutPaletteConfig(w http.ResponseWriter, r *http.Request) {
	if s.paletteConfig == nil {
		writeJSONError(w, http.StatusInternalServerError, CodeInvalidState, "palette config store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, paletteConfigPutBodyMax)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusBadRequest, CodeInvalidInput, "request body exceeds 1024 bytes")
			return
		}
		writeJSONError(w, http.StatusBadRequest, CodeInvalidInput, "invalid request body")
		return
	}
	cfg, status, msg := decodePalettePutBody(data)
	if status != 0 {
		writeJSONError(w, status, CodeInvalidInput, msg)
		return
	}
	if err := cfg.Validate(); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, CodeInvalidInput, err.Error())
		return
	}
	if err := s.paletteConfig.Put(cfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, CodeInternal, "save palette config failed")
		return
	}
	writePaletteConfig(w, paletteDTOFromStore(s.paletteConfig))
}

// decodePalettePutBody 按 spec 错误矩阵解码：空体/空白/语法/尾随第二值 → 400；
// 顶层非对象/缺键/null/类型错误 → 422。超限在 ReadAll 处拦截，本函数不写盘。
func decodePalettePutBody(data []byte) (domainpalette.Config, int, string) {
	if isEmptyOrWhitespace(data) {
		return domainpalette.Config{}, http.StatusBadRequest, "request body is required"
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return domainpalette.Config{}, http.StatusBadRequest, "invalid JSON body"
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return domainpalette.Config{}, http.StatusBadRequest, "invalid JSON body"
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return domainpalette.Config{}, http.StatusUnprocessableEntity, "palette config must be a JSON object"
	}
	cfg, err := palette.DecodeConfig(raw)
	if err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return domainpalette.Config{}, http.StatusUnprocessableEntity, err.Error()
		}
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return domainpalette.Config{}, http.StatusBadRequest, "invalid JSON body"
		}
		return domainpalette.Config{}, http.StatusUnprocessableEntity, err.Error()
	}
	return cfg, 0, ""
}

func isEmptyOrWhitespace(data []byte) bool {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\n', '\r', '\v', '\f':
		default:
			return false
		}
	}
	return true
}
