// Package api 静态资源同源服务（design.md §14：embed.FS 内嵌前端 + SPA fallback）。
package api

import (
	"io/fs"
	"log"
	"net/http"
	"strings"

	"ocdeck/web"
)

// staticFS 内嵌前端产物的 fs.FS（design.md §14）。
// 构建顺序：先 `pnpm --dir web build` 产出 web/dist，再 `go build`（go:embed 编译期固化）。
var staticFS fs.FS

func init() {
	fsys, err := web.DistFS()
	if err != nil {
		// 仅在构建顺序错误（dist 缺失）时发生，直接 panic 让构建/启动阶段尽早暴露。
		log.Fatalf("api/static: embed dist unavailable (run `pnpm --dir web build` first): %v", err)
	}
	staticFS = fsys
}

// spaHandler 同源服务内嵌前端产物（design.md §14）。
// /assets/* 直接由文件系统提供；其余未知非文件路径 fallback 到 index.html（SPA history 路由）。
// 静态资源豁免认证（staticExemptPaths），数据 API（/api/v1）不豁免。
type spaHandler struct {
	fileServer http.Handler
	fsys       fs.FS
}

func newSPAHandler() *spaHandler {
	return &spaHandler{
		fileServer: http.FileServer(http.FS(staticFS)),
		fsys:       staticFS,
	}
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clean := strings.TrimPrefix(r.URL.Path, "/")
	// 文件存在（含 /assets/* 与 /favicon.ico 等具名资源）→ 直接由 FileServer 提供。
	if clean != "" {
		if info, err := fs.Stat(h.fsys, clean); err == nil && !info.IsDir() {
			h.fileServer.ServeHTTP(w, r)
			return
		}
	}
	// 目录或不存在路径 → fallback index.html（SPA history 路由）。
	// index.html 是 SPA 入口，客户端路由自行解析路径。
	r.URL.Path = "/"
	h.fileServer.ServeHTTP(w, r)
}

// registerStaticRoutes 注册根路由与静态资源（design.md §14）。
// /assets/* 豁免认证（静态资源 MAY 豁免）；其余根资源走 SPA fallback（同样豁免）。
func (s *Server) registerStaticRoutes() {
	h := newSPAHandler()
	// /assets/* 豁免认证：静态资源由浏览器直接加载，不携带 bearer token。
	s.mux.Handle("/assets/", h)
	// 根路由兜底：命中具名文件走 FileServer，未知路径 SPA fallback 到 index.html。
	s.mux.HandleFunc("/", h.ServeHTTP)
}