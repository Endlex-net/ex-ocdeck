// Package api 实现 HTTP/WS 端点、token 中间件与统一错误结构（design.md §14/§21）。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ocdeck/internal/ai"
	"ocdeck/internal/config"
	storepkg "ocdeck/internal/store"
)

// Server 持有 HTTP 服务与依赖。本任务提供骨架与占位路由。
type Server struct {
	cfg             *config.Config
	mux             *http.ServeMux
	auth            *TokenAuthenticator
	store           StoreRO
	projs           ProjectStore
	tasks           TaskBackend
	envs            EnvStore
	lifecycleCfgs   LifecycleConfigStore
	ocCfgs          OCConfigService
	aiConfig        *ai.Store
	eventSubscriber EventSubscriber
	httpSrv         *http.Server

	// sseCoalesce/sseHeartbeat SSE 流合并窗口与心跳间隔（sse-active-sessions P2.3）。
	// 零值用生产默认（500ms/25s）；仅供同包测试注入短间隔，不改变生产语义。
	sseCoalesce  time.Duration
	sseHeartbeat time.Duration

	// watchdogStateProvider 返回 watchdog 运行态字符串（off/running/degraded），
	// 供 /server/status 接线（design.md §21）。nil 时回退 "off"。
	watchdogStateProvider func() string

	// wsClients 单交互客户端注册表（design.md §21：同一终端新连接替换旧连接，4009）。
	wsClients *wsClientRegistry
}

// StoreRO store 包的只读接口占位；本任务只挂 server/status，
// 完整 store 注入由后续 TaskManager 阶段接入。
type StoreRO interface {
	// Ping 验证 DB 可达，供 /server/status 健康检查。
	PingContext(ctx context.Context) error
}

// ProjectStore 项目注册所需的 store 能力（design.md §21 projects 路由）。
// 复用 internal/store 既有 Queries 方法签名。
type ProjectStore interface {
	CreateProject(ctx context.Context, id, name, path, defaultBranch, kind string) error
	GetProject(ctx context.Context, id string) (storeProjectRow, error)
	GetProjectByPath(ctx context.Context, path string) (storeProjectRow, error)
	ListProjects(ctx context.Context) ([]storeProjectRow, error)
	// DeleteProjectIfEmpty 原子删除项目（B9）：仅当无任务时删除，返回是否删除。
	DeleteProjectIfEmpty(ctx context.Context, id string) (bool, error)
	CountProjectTasks(ctx context.Context, projectID string) (storeTaskCounts, error)
	HasProjectTasks(ctx context.Context, projectID string) (bool, error)
}

// storeProjectRow 解耦 store.ProjectRow，避免 api 直接依赖 store 包结构。
// Kind ∈ repo | dir（migration 0008，add-plain-dir-project D1）。
type storeProjectRow struct {
	ID            string
	Name          string
	Path          string
	DefaultBranch string
	Kind          string
	CreatedAt     int64
}

// storeTaskCounts 解耦 store.ProjectTaskCountsRow。
type storeTaskCounts struct {
	Total    int
	ByStatus map[string]int
}

// WithProjectStore 构造注入 ProjectStore 的 Server。
func WithProjectStore(cfg *config.Config, store StoreRO, projs ProjectStore) *Server {
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		store:     store,
		projs:     projs,
		wsClients: newWSClientRegistry(),
	}
	s.registerRoutes()
	return s
}

// SetTaskBackend 注入 TaskBackend（design.md §18 task→api 接线）。
// 注意：task 路由在 registerRoutes 时按 s.tasks != nil 注册；延迟注入需调用 RebuildRoutes。
func (s *Server) SetTaskBackend(tb TaskBackend) {
	s.tasks = tb
}

// SetEnvStore 注入 EnvStore（design.md §21 env 路由）。延迟注入需调用 RebuildRoutes。
func (s *Server) SetEnvStore(envs EnvStore) {
	s.envs = envs
}

// SetLifecycleConfigStore 注入 LifecycleConfigStore（design.md §8 lifecycle-config 路由）。
// 延迟注入需调用 RebuildRoutes。
func (s *Server) SetLifecycleConfigStore(store LifecycleConfigStore) {
	s.lifecycleCfgs = store
}

// SetOCConfigService 注入全局配置管理服务（design.md §13/§21 oc-configs 路由）。
// 延迟注入需调用 RebuildRoutes。
func (s *Server) SetOCConfigService(svc OCConfigService) {
	s.ocCfgs = svc
}

// RebuildRoutes 重建路由（供延迟注入 ProjectStore/TaskBackend 后调用）。
// 重置 mux 并重新注册全部路由。
func (s *Server) RebuildRoutes() {
	s.mux = http.NewServeMux()
	s.registerRoutes()
}

// New 构造 Server。store 可为 nil（本阶段未接入完整存储编排）。
// 若 store 为 *store.DB，则自动包装为 ProjectStore 注入项目路由。
func New(cfg *config.Config, store StoreRO) *Server {
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		store:     store,
		wsClients: newWSClientRegistry(),
	}
	if db, ok := store.(*storepkg.DB); ok && db != nil {
		s.projs = NewProjectStoreAdapter(db)
		s.envs = NewEnvStoreAdapter(db)
		s.lifecycleCfgs = NewLifecycleConfigStoreAdapter(db)
	}
	s.registerRoutes()
	return s
}

// staticExemptPaths 静态资源豁免认证的路径前缀（design.md §14/§15 内嵌前端）。
// 根资源与 /assets/* 由浏览器直接加载，豁免认证；API 前缀 /api/v1 必须认证。
func staticExemptPaths(path string) bool {
	if path == "/" || path == "/favicon.ico" {
		return true
	}
	// 内嵌前端构建产物（JS/CSS bundle）由浏览器直接加载，不携带 bearer token。
	return strings.HasPrefix(path, "/assets/")
}

// registerRoutes 注册路由（design.md §21 路由表）。
// /api/v1/* 走 api 子 mux + Bearer 认证中间件；
// /ws/* 单独挂载，不走 api 子 mux，但仍过 token（首帧 auth）+ Origin 校验（design.md §21）。
func (s *Server) registerRoutes() {
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /api/v1/server/status", s.handleServerStatus)
	s.registerProjectRoutes(apiMux)
	s.registerTaskRoutes(apiMux)            // task 路由接入主 api 子 mux
	s.registerGitRoutes(apiMux)             // git 状态/diff/commit/push（design.md §21）
	s.registerEnvRoutes(apiMux)             // project/task env CRUD（design.md §21）
	s.registerLifecycleConfigRoutes(apiMux) // project lifecycle config（design.md §8）
	s.registerOCConfigRoutes(apiMux)        // 全局 oc 配置管理（design.md §13/§21）
	s.registerAIConfigRoutes(apiMux)        // 全局 AI provider 配置（design.md D6）

	// /api/v1 前缀统一挂认证中间件（design.md §14/§21）。
	// 已认证请求的未知路由/方法返回统一 JSON 404/405（design.md §21 错误结构）。
	s.mux.Handle("/api/v1/", s.auth.AuthMiddleware(staticExemptPaths, jsonNotFoundHandler(apiMux)))
	s.mux.Handle("/api/v1", s.auth.AuthMiddleware(staticExemptPaths, jsonNotFoundHandler(apiMux)))

	// /ws/* 端点单独挂载：不走 api 子 mux，token 由首帧认证（wsAuthHandshake）完成，
	// Origin 由 checkWSOrigin 在升级前校验（design.md §21）。
	s.registerWSRoutes(s.mux)

	// 根路由与静态资源（design.md §14：embed.FS 内嵌前端，SPA fallback）。
	s.registerStaticRoutes()
}

// registerWSRoutes 注册 WS 终端端点（design.md §21）。
// WS 不走 api 子 mux 的 Bearer 中间件；token 经首帧 auth 帧（wsAuthHandshake）校验，
// Origin 经 checkWSOrigin 在升级前校验。s.tasks 为 nil 时跳过（延迟注入后 RebuildRoutes）。
func (s *Server) registerWSRoutes(mux *http.ServeMux) {
	if s.tasks == nil {
		return
	}
	mux.HandleFunc("GET /ws/terminal/{taskID}", s.handleWSTUI)
	mux.HandleFunc("GET /ws/terminal/shell/{tid}", s.handleWSShell)
}

// jsonNotFoundHandler 包装 next，对 next 未命中（404）或方法不允许（405）的请求
// 统一返回 design.md §21 错误结构（保留 next 写入的 HTTP 状态码 404/405）。
// ServeMux 对未注册路径写 404 纯文本、对已注册路径但方法不匹配写 405 纯文本——
// 这里改写为 JSON 错误体，避免裸文本泄露内部 mux 行为。
func jsonNotFoundHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		switch rec.status {
		case http.StatusNotFound:
			writeJSONError(w, http.StatusNotFound, CodeNotFound, "no route for "+r.Method+" "+r.URL.Path)
		case http.StatusMethodNotAllowed:
			writeJSONError(w, http.StatusMethodNotAllowed, CodeNotFound, "method not allowed for "+r.URL.Path)
		}
	})
}

// statusRecorder 记录下游写入的状态码，用于在不破坏 next 写入的前提下判断 404/405。
// 404/405 的 header 与 body 由 jsonNotFoundHandler 在 next 返回后统一重写，
// 因此对 404/405 不转发 header、吞掉 body；其他状态码照常转发，保证 handler 自定义
// 状态码（如 201/204）能正确返回。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = code
	if code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
		// 404/405 不转发，后续由 writeJSONError 统一写状态码与 body。
		return
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// 吞掉 ServeMux 默认 404/405 纯文本 body，由 writeJSONError 统一输出 JSON。
	if r.status == http.StatusNotFound || r.status == http.StatusMethodNotAllowed {
		return len(b), nil
	}
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap 暴露底层 ResponseWriter（sse-active-sessions P2.5）。http.ResponseController
// 经 Unwrap 链获取底层扩展能力；缺失 Unwrap 时控制器无法到达真实连接。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// FlushError 刷新底层连接并传播错误（sse-active-sessions P2.5）。SSE handler 经
// http.NewResponseController(w).Flush() 写帧：控制器优先匹配 FlushError，仅实现
// 无返回值 Flush() 会吞底层 flush 错误；此处委托控制底层 ResponseWriter 完成刷新。
func (r *statusRecorder) FlushError() error {
	return http.NewResponseController(r.ResponseWriter).Flush()
}

// SetWatchdogStateProvider 注入 watchdog 运行态查询函数（design.md §21）。
// 由 cmd/ocdeck-server 在 kill_immediate 模式下注入 WatchdogManager.StateString；
// 其他模式 nil，/server/status 回退 "off"。
func (s *Server) SetWatchdogStateProvider(fn func() string) {
	s.watchdogStateProvider = fn
}

// handleServerStatus 返回服务端状态（design.md §21）。
// watchdogState 反映 watchdog 真实运行态（off/running/degraded）；
// 非 kill_immediate 模式恒为 "off"。
func (s *Server) handleServerStatus(w http.ResponseWriter, r *http.Request) {
	watchdogState := "off"
	if s.watchdogStateProvider != nil {
		watchdogState = s.watchdogStateProvider()
	}
	status := map[string]any{
		"opencodeVersion":    s.cfg.OpenCodeVersion,
		"tmuxVersion":        s.cfg.TmuxVersion,
		"shutdownPolicy":     string(s.cfg.ShutdownPolicy),
		"watchdogState":      watchdogState,
		"contractBaseline":   config.ContractBaseline,
		"contractMinVersion": config.ContractMinVersion,
		"versionVerified":    s.cfg.VersionVerified,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// Start 启动 HTTP 服务。阻塞直到 ctx 取消或监听出错。
func (s *Server) Start(ctx context.Context) error {
	port := s.cfg.ListenPort
	if port < 0 {
		port = 0
	}
	addr := net.JoinHostPort(s.cfg.ListenAddr, strconv.Itoa(port))
	s.httpSrv = &http.Server{
		Addr:    addr,
		Handler: s.mux,
		// BaseContext：请求 ctx 派生自服务进程 ctx（sse-active-sessions P2.4）——
		// 进程取消先传导到 in-flight 请求（含 SSE stream：handler 经 r.Context()
		// 观测取消退出），再进入下方 5s Shutdown 预算，关停顺序为「先取消 stream
		// 再 Shutdown」。读超时防慢速攻击；无写超时（WS/长任务需要）。
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	// 成功 bind 后记录实际监听地址（含系统分配端口，design.md §14）。
	log.Printf("ocdeck-server HTTP listening on %s", ln.Addr().String())
	errCh := make(chan error, 1)
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
