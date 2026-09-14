// Package httpapi 承载 cchd 的 HTTP 面：探针、前门中间件的挂载点、数据面与管理面处理器。
//
// 分工固定：探针（`/readyz`、`/v1/_ping`）**不经过前门**——探针是运维面，不该受路由归属与
// 排空闸门影响，否则空窗期连「我为什么不健康」都问不到；其余 `/v1`、`/v1beta`、`/api/v1` 一律
// 经前门中间件（`egress.FrontDoor.Middleware`）判定归属：判给 Go 的交给对应处理器，判给 Node
// 的由前门原样反代。管理面必须显式包中间件（不能只靠 ServerOptions.FrontDoor）——它只作用于
// 数据面，管理面前缀要自己包，否则规则判定与 `egress_decision` 日志在管理面上都会缺失。
//
// 本包不实现任何数据面/管理面逻辑，也不决定路由归属：只提供挂载点。因此处理器未装配时返回
// 503 而不是 501——501 会把「Go 还没实现」谎报成「协议不支持」，让客户端放弃重试。
//
// 唯一的例外是**出站压缩**（compress.go）：它既不属于数据面也不属于管理面，而是整条响应路径的
// 传输层关切，故挂在 mux 之外一次生效——此前运行期生成的响应完全不经压缩（见该文件头）。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// DependencyStatus 是一个外部依赖的当前状态。
type DependencyStatus string

const (
	StatusOK          DependencyStatus = "ok"
	StatusError       DependencyStatus = "error"
	StatusNotSet      DependencyStatus = "not_configured"
	StatusNotLoaded   DependencyStatus = "not_loaded"
	StatusUnavailable DependencyStatus = "unavailable"
)

// ReadyReport 是 /readyz 的响应体。四项状态各自独立，任何一项非 ok 即 503。
type ReadyReport struct {
	Ready bool             `json:"ready"`
	Self  DependencyStatus `json:"self"`
	PG    DependencyStatus `json:"pg"`
	Redis DependencyStatus `json:"redis"`
	Rules DependencyStatus `json:"rules"`
	// Failing 列出非 ok 的项名，便于调用方直接定位故障项而不必解析 notes。
	Failing []string          `json:"failing,omitempty"`
	Notes   map[string]string `json:"notes,omitempty"`
	At      string            `json:"at"`
}

// Prober 报告各项依赖的真实状态。
type Prober interface {
	// PingPG 探测 PostgreSQL；未配置时返回 StatusNotSet。
	PingPG(ctx context.Context) (DependencyStatus, string)
	// PingRedis 探测 Redis；未配置时返回 StatusNotSet。
	PingRedis(ctx context.Context) (DependencyStatus, string)
	// RulesStatus 报告规则快照是否已装载。
	RulesStatus() (DependencyStatus, string)
}

// ServerOptions 是装配参数。
type ServerOptions struct {
	// Logger 为请求日志与内部事件使用。
	Logger *logx.Logger
	// Prober 为 nil 时 /readyz 对三项依赖一律返回 unavailable，避免误报就绪。
	Prober Prober
	// Listening 报告自身监听是否已就绪。
	Listening func() bool
	// Draining 报告前门是否已停止接纳新请求；排空期间 /readyz 必须报 503，
	// 否则探针会把流量继续导向一个正在排空的后端。
	Draining func() bool
	// FrontDoor 是前门中间件（egress.FrontDoor.Middleware）：
	// 入参是本进程自己承载数据面的处理器，返回已包好归属判定与回退反代的处理器。
	// nil 时视为「前门未装配」，数据面请求直接交给 DataPlane。
	FrontDoor func(next http.Handler) http.Handler
	// Pages 是页面面归属中间件（egress.FrontDoor.PagesMiddleware）：挂在「非 API 路径」上，
	// 默认把 SSR 页面与 /_next 静态资源原样反代回 Node，使 Go 能独占对外监听。
	// nil 时非 API 路径由本进程自答（与「Node 独占监听」时期的行为一致）。
	Pages func(next http.Handler) http.Handler
	// DataPlane 是本进程自己承载的数据面处理器；nil 时 /v1 返回 503。
	DataPlane http.Handler
	// AdminPlane 是本进程自己承载的管理面处理器（挂 /api/v1）。
	// nil 时不挂载该前缀：/api/v1 落 handleRoot 的 404，与「管理面全在 Node」的旧行为一致。
	AdminPlane http.Handler
	// ProbeTimeout 限制单次依赖探测的时长。
	ProbeTimeout time.Duration
	// Configuration 返回与就绪无关但排障必需的配置结论（例如亲和是否启用及其来源）；
	// 非 nil 时其条目进入 /readyz 的 notes。nil 时不报。
	Configuration func() map[string]string
	// Version 报告应用版本（不带 `v` 前缀的展示值由本包去前缀），供 `/api/health*` 与 Node 的
	// `getAppVersion()` 对齐。nil 时退到 VERSION 文件/环境变量/常量（见 health_api.go 的 appVersion）。
	Version func() string
}

// Server 是 cchd 的 HTTP 处理器。
type Server struct {
	logger        *logx.Logger
	prober        Prober
	configuration func() map[string]string
	listening     func() bool
	draining      func() bool
	frontDoor     func(http.Handler) http.Handler
	pages         func(http.Handler) http.Handler
	dataPlane     http.Handler
	adminPlane    http.Handler
	probeTimeout  time.Duration
	version       func() string
}

// New 装配处理器。
func New(options ServerOptions) *Server {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	listening := options.Listening
	if listening == nil {
		listening = func() bool { return true }
	}
	draining := options.Draining
	if draining == nil {
		draining = func() bool { return false }
	}
	probeTimeout := options.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = 2 * time.Second
	}
	return &Server{
		logger:        logger,
		prober:        options.Prober,
		configuration: options.Configuration,
		listening:     listening,
		draining:      draining,
		frontDoor:     options.FrontDoor,
		pages:         options.Pages,
		dataPlane:     options.DataPlane,
		adminPlane:    options.AdminPlane,
		probeTimeout:  probeTimeout,
		version:       options.Version,
	}
}

// Handler 返回已挂载中间件的根处理器。
func (s *Server) Handler() http.Handler {
	// 运维面：探针与未知路径的自报。
	ops := http.NewServeMux()
	ops.HandleFunc("/readyz", s.handleReadyz)
	ops.HandleFunc("/v1/_ping", s.handlePing)
	// Node 的 /api/health* 三条：同样属运维面（不查登录、不做归属判定），故与探针同挂 ops。
	// 必须在 root 上按精确路径**先于** "/api/" 注册：ServeMux 取最长匹配，故这三条不会被
	// 管理面处理器接管，也不会因前门把 /api/* 判给 Node 而回退掉（探针不该受归属影响）。
	ops.HandleFunc("/api/health", s.handleHealthAPI)
	ops.HandleFunc("/api/health/ready", s.handleHealthAPI)
	ops.HandleFunc("/api/health/live", s.handleHealthLive)
	ops.HandleFunc("/", s.handleRoot)

	// 数据面：进前门做归属判定，判给 Go 的落到 DataPlane。
	dataPlane := s.dataPlaneHandler()

	root := http.NewServeMux()
	// 探针比 "/v1/" 更具体，ServeMux 会优先匹配，故探针不进前门。
	root.Handle("/v1/_ping", ops)
	root.Handle("/readyz", ops)
	// /api/health* 比 "/api/" 更具体（ServeMux 取最长前缀匹配），故它们同样不进前门。
	root.Handle("/api/health", ops)
	root.Handle("/api/health/", ops)
	root.Handle("/v1/", dataPlane)
	root.Handle("/v1beta/", dataPlane)
	// 管理面：判给 Go 的落 AdminPlane，其余由前门原样反代 Node。同时注册不带尾斜杠的
	// 精确路径，避免 ServeMux 对 `/api/v1` 发 301 跳转（Node 不发，跳转会改变对拍结果）。
	if s.adminPlane != nil {
		admin := s.adminPlaneHandler()
		root.Handle("/api/v1/", admin)
		root.Handle("/api/v1", admin)
		// 根级 /api/* 面（/api/auth/login、/api/auth/logout 等）同样落在管理面处理器上：
		// 该处理器未命中本进程路由时原样回退 Node，故这条更宽的挂载不会吞掉 Node 的端点。
		// 没有它，根级认证面只能走页面面回退，纯 Go 部署就登不进后台。
		root.Handle("/api/", admin)
	}
	// 页面面：非 API 路径（SSR 页面、/_next 静态资源及其它未接管后缀）的归属。
	// 它排在最后：ServeMux 先剥掉探针与三个 API 前缀，落到这里的必然是非 API 路径，
	// 因此不需要在中间件里再判一次「是不是 API」（判两次就会与 mux 的优先级分叉）。
	var pagePlane http.Handler = ops
	if s.pages != nil {
		pagePlane = s.pages(ops)
	}
	root.Handle("/", pagePlane)
	// 压缩包在 mux 内侧：它必须以「处理器直接看到的写入器」的身份存在——内部两处流式路径用
	// `writer.(http.Flusher)` 决定能否流式（见 compress.go 约束③）。请求日志留在最外层，
	// 故它记到的仍是真实终态（压缩只改正文编码，不改状态码）。
	return s.withRequestLog(s.withCompression(root))
}

// adminPlaneHandler 把前门中间件包在 AdminPlane 外面。
//
// 与数据面同构：未命中本进程路由的请求由 AdminPlane 自己交回前门的 NodeFallback（装配时设置），
// 这里只保证归属判定与排空闸门对管理面同样生效。
func (s *Server) adminPlaneHandler() http.Handler {
	if s.frontDoor == nil {
		return s.adminPlane
	}
	return s.frontDoor(s.adminPlane)
}

// dataPlaneHandler 把前门中间件包在 DataPlane 外面。
func (s *Server) dataPlaneHandler() http.Handler {
	next := s.dataPlane
	if next == nil {
		next = http.HandlerFunc(s.handleDataPlaneMissing)
	}
	if s.frontDoor == nil {
		return next
	}
	return s.frontDoor(next)
}

func (s *Server) handleReadyz(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), s.probeTimeout)
	defer cancel()

	report := ReadyReport{
		Self:  StatusOK,
		PG:    StatusUnavailable,
		Redis: StatusUnavailable,
		Rules: StatusUnavailable,
		Notes: map[string]string{},
		At:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	switch {
	case s.draining():
		// 排空中：进程还活着但不再接纳，必须报 503，让调用方改打另一后端。
		report.Self = StatusError
		report.Notes["self"] = "draining"
	case !s.listening():
		report.Self = StatusError
		report.Notes["self"] = "listener not ready"
	}

	if s.prober == nil {
		report.Notes["pg"] = "prober not wired"
		report.Notes["redis"] = "prober not wired"
		report.Notes["rules"] = "prober not wired"
	} else {
		report.PG, report.Notes["pg"] = s.prober.PingPG(ctx)
		report.Redis, report.Notes["redis"] = s.prober.PingRedis(ctx)
		report.Rules, report.Notes["rules"] = s.prober.RulesStatus()
	}

	// 配置结论不影响就绪（不问 status 项），只进入 notes：它们回答「为什么行为与预期不同」。
	if s.configuration != nil {
		for key, value := range s.configuration() {
			if value != "" {
				report.Notes[key] = value
			}
		}
	}

	for _, item := range []struct {
		name   string
		status DependencyStatus
	}{
		{"self", report.Self},
		{"pg", report.PG},
		{"redis", report.Redis},
		{"rules", report.Rules},
	} {
		if item.status != StatusOK {
			report.Failing = append(report.Failing, item.name)
		}
	}

	for key, value := range report.Notes {
		if value == "" {
			delete(report.Notes, key)
		}
	}
	if len(report.Notes) == 0 {
		report.Notes = nil
	}

	report.Ready = len(report.Failing) == 0

	status := http.StatusOK
	if !report.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(writer, status, report)
}

func (s *Server) handlePing(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"status": "pong"})
}

// handleDataPlaneMissing 在数据面尚未装配时作答：用 503 而不是 501。
func (s *Server) handleDataPlaneMissing(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{
			"type":    "data_plane_unavailable",
			"message": "本进程未装配数据面处理器",
			"path":    request.URL.Path,
		},
	})
}

func (s *Server) handleRoot(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/" {
		writeJSON(writer, http.StatusOK, map[string]any{
			"service": "cchd",
			"hint":    "data plane routes live under /v1",
		})
		return
	}
	// 管理面默认在 Node；装配了 AdminPlane 时它挂在 /api/v1，未命中的路径仍由前门反代 Node。
	writeJSON(writer, http.StatusNotFound, map[string]any{
		"error": map[string]any{"type": "not_found", "path": request.URL.Path},
	})
}

// statusRecorder 记录状态码，用于请求日志。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(payload []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(payload)
}

// Flush 让流式响应经本包装后仍可逐块下发（前门与数据面都依赖它）。
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// withRequestLog 只记录方法、路径、状态与耗时。
// 正文、查询串里的凭据与 Authorization 头一律不记。
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		startedAt := time.Now()
		recorder := &statusRecorder{ResponseWriter: writer}
		next.ServeHTTP(recorder, request)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Info("http_request", map[string]any{
			"method":   request.Method,
			"path":     request.URL.Path,
			"status":   status,
			"duration": time.Since(startedAt).Milliseconds(),
		})
	})
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}
