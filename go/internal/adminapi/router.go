package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// defaultCSP 逐字取自 Node 的 DEFAULT_CSP_VALUE（src/lib/security/security-headers.ts:28-31），
// 管理面以 report-only 模式下发（src/lib/api/v1/_shared/management-security-headers.ts:8）。
const defaultCSP = "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self'; " +
	"font-src 'self' data:; frame-ancestors 'none'"

// Route 是一条管理路由。Path 相对 MountPrefix，写法与 §2 表一致。
type Route struct {
	// Method 是大写 HTTP 方法。
	Method string
	// Path 以 / 开头；参数写 {name}，带约束写 {name:正则}，参数后跟字面后缀写 {name:正则}:后缀。
	Path string
	// Access 是权限档位，取自 Node OpenAPI 的 x-required-access。
	Access AccessLevel
	// Module 是资源模块名（keys/users/usage-logs/...），用于日志与失效广播。
	Module string
	// OperationID 与 Node OpenAPI 的 operationId 对齐，供 A2 的契约测试核对。
	OperationID string
	// NoManagementEnvelope 表示该路由**不是**管理面端点（例如根级 /api/auth/*）：命中时不发
	// 管理面信封里的 X-API-Version，只发 no-store 与安全头。Node 的 /api/auth/* 在管理面应用
	// 之外，本来就没有这个头，Go 侧补上只会与 Node 分叉。
	NoManagementEnvelope bool
	Handler              http.Handler
}

// Options 是 Router 的建造参数。
type Options struct {
	Deps Deps
	// EnableHSTS 与 Node 的 enableHsts 同源：getEnvConfig().ENABLE_SECURE_COOKIES。
	EnableHSTS bool
}

// Router 是管理面路由表，实现 http.Handler。
//
// 注册约定：Add 只在装配期调用（服务开始前），Router 内部不加锁；注册完成后对并发请求只读，
// 因此并发安全。运行期改表不是本包的能力，也不该有。
type Router struct {
	deps     Deps
	logger   *logx.Logger
	hsts     bool
	notFound http.Handler
	routes   []compiledRoute
}

// New 建造路由表。
func New(options Options) *Router {
	logger := options.Deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	return &Router{deps: options.Deps, logger: logger, hsts: options.EnableHSTS}
}

// SetNotFound 设置未命中时的兜底处理器。
//
// 生产必须设为前门的 NodeFallback（egress.FrontDoor.NodeFallback）：未注册的管理路由要原样
// 反代回 Node，而不是在 Go 侧作答。未设置时一律 503——宁可让调用方重试到 Node，也不要让
// Go 用一个猜出来的 404 冒充 Node 的语义。
func (r *Router) SetNotFound(handler http.Handler) {
	r.notFound = handler
}

// Add 注册一条路由，并按 Deps.Guard 包装其处理器。
//
// 路径模式非法时 panic（启动期编程错误，见 compilePath）。
//
// 认证守卫未装配时**拒绝注册**并记 Error 日志：此时该请求会照常回退 Node（那里有完整的认证），
// 而不是由 Go 放行一个未认证的管理请求。拒绝注册同时让「已注册 0 条」在启动日志里一目了然，
// 也让 A2 的 72 条契约测试直接失败——比「注册了但没人守」安全，也比 per-request 静默回退可查。
//
// 「方法+路径」重复时保留首次注册并记 Error 日志——重复注册是并行人手写路由表的常见失误，
// 报错但不崩，由 A2 的契约测试收口。
func (r *Router) Add(route Route) {
	segments, specificity, err := compilePath(route.Path)
	if err != nil {
		panic(err)
	}
	if route.Handler == nil {
		panic("adminapi: 路由 " + route.Method + " " + route.Path + " 缺少处理器")
	}
	method := strings.ToUpper(route.Method)
	if r.deps.Guard == nil {
		r.logger.Error("admin_auth_unwired", map[string]any{
			"method": method,
			"path":   route.Path,
			"module": route.Module,
			"action": "route_not_registered",
		})
		return
	}
	for _, existing := range r.routes {
		if existing.route.Method == method && existing.route.Path == route.Path {
			r.logger.Error("admin_route_duplicate", map[string]any{
				"method": method,
				"path":   route.Path,
				"module": route.Module,
			})
			return
		}
	}
	route.Method = method
	route.Handler = r.deps.Guard.Wrap(route.Access, route.Handler)
	r.routes = append(r.routes, compiledRoute{
		route:       route,
		segments:    segments,
		specificity: specificity,
		order:       len(r.routes),
	})
}

// RouteCount 返回已注册的路由条数。
func (r *Router) RouteCount() int { return len(r.routes) }

// RouteList 返回已注册路由的副本（不含处理器），供契约测试与自报使用。
func (r *Router) RouteList() []Route {
	routes := make([]Route, 0, len(r.routes))
	for _, compiled := range r.routes {
		route := compiled.route
		route.Handler = nil
		routes = append(routes, route)
	}
	return routes
}

// ServeHTTP 按「命中则本进程作答，未命中则交兜底」处理请求。
//
// 方法不匹配也走兜底：Node 会给出它自己的 405 与 Allow 头，Go 自己拼一个只会与 Node 分叉。
func (r *Router) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	path := normalizePath(request.URL.Path)
	route, params := r.lookup(strings.ToUpper(request.Method), path)
	if route == nil {
		// 这条日志是切换演练的关键证据：Go 接住了请求但没实现，故原样回退 Node。
		r.logger.Info("admin_route_unmatched", map[string]any{
			"method": strings.ToUpper(request.Method),
			"path":   path,
			"owner":  "node",
		})
		r.serveNotFound(writer, request)
		return
	}
	compiled := r.routes[*route]
	if compiled.route.NoManagementEnvelope {
		applyAuthEnvelopeHeaders(writer.Header(), r.hsts)
	} else {
		r.applyEnvelopeHeaders(writer.Header())
	}
	compiled.route.Handler.ServeHTTP(writer, request.WithContext(WithParams(request.Context(), params)))
}

// lookup 返回命中的路由下标与路径参数。择路规则：先比权重（静态优先），同权取注册序。
func (r *Router) lookup(method, path string) (*int, map[string]string) {
	selected := -1
	var selectedParams map[string]string
	for index := range r.routes {
		compiled := r.routes[index]
		if compiled.route.Method != method {
			continue
		}
		// 权重不高于已选中者时直接跳过：它无法胜出，无需再做匹配（同权保留先注册者）。
		if selected >= 0 && compiled.specificity <= r.routes[selected].specificity {
			continue
		}
		params, matched := compiled.match(path)
		if !matched {
			continue
		}
		selected = index
		selectedParams = params
	}
	if selected < 0 {
		return nil, nil
	}
	return &selected, selectedParams
}

// serveNotFound 交兜底处理器；未设置兜底时 503。
func (r *Router) serveNotFound(writer http.ResponseWriter, request *http.Request) {
	if r.notFound != nil {
		r.notFound.ServeHTTP(writer, request)
		return
	}
	r.logger.Error("admin_fallback_unwired", map[string]any{"path": request.URL.Path})
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"status":    http.StatusServiceUnavailable,
		"errorCode": "admin.fallback_unwired",
		"detail":    "管理面未命中兜底处理器未装配",
	})
}

// applyEnvelopeHeaders 下发管理面的统一响应头。
//
// 版本头与不缓存来自 Node 的 /api/v1 应用壳（src/app/api/v1/_root/app.ts:56-59 与 :118,134 的
// withNoStoreHeaders）；安全头来自 src/lib/api/v1/_shared/management-security-headers.ts 与
// src/lib/security/security-headers.ts:38-58。注意 Node 的 /api/v1 应用壳自身只发版本头与
// 不缓存，安全头由共享模块在部分路径上补——Go 侧统一补上是**有意的超集**，A2 的对拍需把这条
// 登记进允许差异白名单。
func (r *Router) applyEnvelopeHeaders(header http.Header) {
	header.Set(VersionHeader, APIVersion)
	header.Set("Cache-Control", "no-store, no-cache, must-revalidate")
	header.Set("Pragma", "no-cache")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	header.Set("X-DNS-Prefetch-Control", "off")
	header.Set("Content-Security-Policy-Report-Only", defaultCSP)
	if r.hsts {
		header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

// normalizePath 归一化请求路径：去查询串、合并重复斜杠、去尾斜杠，并剥掉挂载前缀。
// 复用 egress 的归一化，保证与管理面归属判定（前门）对「同一条路径」的理解完全一致。
func normalizePath(rawPath string) string {
	path := egress.NormalizePath(rawPath)
	if path == MountPrefix {
		return "/"
	}
	if trimmed, found := strings.CutPrefix(path, MountPrefix+"/"); found {
		return "/" + trimmed
	}
	return path
}

// paramsContextKey 是路径参数在请求上下文里的键。
type paramsContextKey struct{}

// WithParams 把路径参数放进上下文。处理器实现（A1）通过 ParamsFrom 读取。
func WithParams(ctx context.Context, params map[string]string) context.Context {
	if len(params) == 0 {
		return ctx
	}
	return context.WithValue(ctx, paramsContextKey{}, params)
}

// ParamsFrom 取回路径参数；未命中或无参数时返回 nil。
func ParamsFrom(ctx context.Context) map[string]string {
	params, _ := ctx.Value(paramsContextKey{}).(map[string]string)
	return params
}
