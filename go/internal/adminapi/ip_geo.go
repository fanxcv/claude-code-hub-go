package adminapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ipgeo"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// IP 归属地查询三条端点。
//
// 唯一真源：
//
//	根级 GET /api/ip-geo/{ip}   src/app/api/ip-geo/[ip]/route.ts（54 行）
//	GET /api/v1/ip-geo/{ip}     src/app/api/v1/resources/public/handlers.ts:105（lookupIpGeo）
//	GET /api/v1/me/ip-geo/{ip}  src/app/api/v1/resources/me/handlers.ts:82 → actions/my-usage.ts:864
//
// 三条的**相同处**：都要 `ipGeoLookupEnabled` 才可用；都调同一个 `lookupIp`；结果都是
// `{status, data?, error?}` 的直接透传。
//
// 三条的**不同处（逐条对齐，不能一把抓）**：
//
//  1. 权限档：根级是**管理员**（非管理员 403 `{"error":"forbidden"}`）；两条 v1 面是 read 档
//     （任意已认证）；me 面另有「该 IP 必须在**本密钥**的可见日志里出现过」这条可见性判据。
//  2. 关停时的形状：根级是裸 404 `{"error":"ip geolocation disabled"}`（Next route 手写）；
//     v1 面是问题信封 404 `ip_geo.disabled`（createProblemResponse）。
//  3. `lang` 的 schema：公开面 `min(1)`（空串即 400），自服务面是裸 string（空串放行）。
//     空串与「未传」在 Node 里**不同**（`options.lang ?? "en"` 只对未传兜底），故缺省值由
//     各 handler 决定、不由 ipgeo 包兜（见 ipgeo.LookupIP 的注释）。
//  4. me 面的错误档：`REQUIRED_FIELD`/`INVALID_STATE`/`OPERATION_FAILED` → 400，`NOT_FOUND` → 404。
//
// 与 Node 的**登记差异**：根级面的 401/403 走管理面问题信封（Node 是裸 `{"error":…}`）——
// 与 /api/admin/database/status 同类差异，原因是 Go 的认证在 Router 层统一完成。

// IPGeoLookup 是查询缝隙；nil 表示未装配（三条端点都不注册，整组回退 Node）。
//
// 放在接口后面而不是直接依赖 ipgeo 包：装配方要注入的是「配置好的查询器」（含上游地址、
// token、TTL、Redis 缓存），按值注入实现才能在测试里换掉上游。ipgeo 包提供 Redis 实现。
type IPGeoLookup interface {
	LookupIP(ctx context.Context, ip, lang string) ipgeo.Result
}

// RegisterIPGeoRoutes 注册三条 ip-geo 端点。
func RegisterIPGeoRoutes(router *Router, deps Deps) {
	if deps.Store == nil || deps.IPGeo == nil {
		logIPGeoUnwired(deps)
		return
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := deps.Problems
	if problems == nil {
		problems = NewProblems(logger)
	}
	api := &ipGeoAPI{
		pools:    deps.Store,
		lookup:   deps.IPGeo,
		problems: problems,
		logger:   logger,
		enabled:  nil, // nil 表示走默认实现（读 system_settings 行）
	}

	api.registerRoutes(router)
}

// registerRoutes 把三条 ip-geo 路由挂到路由器上。
func (api *ipGeoAPI) registerRoutes(router *Router) {
	// 根级：Next 的普通 route handler（不在 /api/v1 应用壳里）→ 不发管理面信封头。
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/ip-geo/{ip}",
		Access:               AccessAdmin,
		Module:               "public",
		OperationID:          "getRootIpGeo",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleRootLookup),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/ip-geo/{ip}",
		Access:      AccessRead,
		Module:      "public",
		OperationID: "lookupIpGeo",
		Handler:     http.HandlerFunc(api.handlePublicLookup),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/me/ip-geo/{ip}",
		Access:      AccessRead,
		Module:      "me",
		OperationID: "getMeIpGeo",
		Handler:     http.HandlerFunc(api.handleMeLookup),
	})
}

func logIPGeoUnwired(deps Deps) {
	if deps.Logger == nil {
		return
	}
	deps.Logger.Error("admin_ip_geo_unwired", map[string]any{
		"module": "public",
		"reason": "store_or_lookup_missing",
		"path":   "/ip-geo/{ip}",
	})
}

type ipGeoAPI struct {
	pools    *store.Pools
	lookup   IPGeoLookup
	problems ProblemWriter
	logger   *logx.Logger
	// enabled 是「开关读数」的可注入面；nil 时走 defaultEnabled（读 system_settings 行）。
	//
	// 为什么留可注入面：这一位是**全局共享**的系统设置，测试改它会给并发跑的其它用例
	// 看到（本仓已有先例：provider_circuit_admin_test.go 用假设置源而不改共享行）。
	enabled func(ctx context.Context) (bool, error)
}

// lookupEnabled 读开关；未注入实现时读库。
func (api *ipGeoAPI) lookupEnabled(ctx context.Context) (bool, error) {
	if api.enabled != nil {
		return api.enabled(ctx)
	}
	return api.defaultEnabled(ctx)
}

// defaultEnabled 读 `ipGeoLookupEnabled`（Node 的 getCachedSystemSettings().ipGeoLookupEnabled）。
//
// 读库而非读 Redis 缓存：Node 那份缓存只是为了少查库，值本身来自同一行；这里取权威值，
// 代价是一次按主键的单行读（系统设置只有一行）。差异记在报告里。
func (api *ipGeoAPI) defaultEnabled(ctx context.Context) (bool, error) {
	row, err := api.pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	return row.IPGeoLookupEnabled, nil
}

// handleRootLookup 复刻 GET /api/ip-geo/{ip}。
func (api *ipGeoAPI) handleRootLookup(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	ip := ParamsFrom(ctx)["ip"]

	enabled, err := api.lookupEnabled(ctx)
	if err != nil {
		api.logger.Error("ip_geo_settings_read_failed", map[string]any{"error": err.Error()})
		writeRawJSON(writer, http.StatusInternalServerError, map[string]any{"error": "获取系统设置失败"})
		return
	}
	if !enabled {
		// Node 该路由是手写 404，不是问题信封。
		writeRawJSON(writer, http.StatusNotFound, map[string]any{"error": "ip geolocation disabled"})
		return
	}

	// `url.searchParams.get("lang") ?? undefined`：未传才兜 "en"，传了空串就用空串。
	lang, provided := queryParam(request, "lang")
	if !provided {
		lang = "en"
	}
	writeIPGeoResult(api, writer, request, api.lookup.LookupIP(ctx, ip, lang))
}

// handlePublicLookup 复刻 GET /api/v1/ip-geo/{ip}（handlers.ts:105）。
func (api *ipGeoAPI) handlePublicLookup(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	ip := ParamsFrom(ctx)["ip"]
	if strings.TrimSpace(ip) == "" {
		api.problems.WriteValidationError(writer, request, []InvalidParam{{
			Path: []any{"ip"}, Code: "too_small",
			Message: "String must contain at least 1 character(s)",
		}})
		return
	}
	// IpGeoQuerySchema: lang 是 `z.string().min(1).optional()` —— 传了空串即校验失败。
	lang, provided := queryParam(request, "lang")
	if provided && lang == "" {
		api.problems.WriteValidationError(writer, request, []InvalidParam{{
			Path: []any{"lang"}, Code: "too_small",
			Message: "String must contain at least 1 character(s)",
		}})
		return
	}
	if !provided {
		lang = "en"
	}

	enabled, err := api.lookupEnabled(ctx)
	if err != nil {
		api.writeIPGeoFailure(writer, request, err)
		return
	}
	if !enabled {
		// Node：createProblemResponse 404 + `ip_geo.disabled` + 固定 detail。
		api.problems.WriteProblem(writer, request, http.StatusNotFound,
			"ip_geo.disabled", "IP geolocation lookup is disabled.")
		return
	}
	writeIPGeoResult(api, writer, request, api.lookup.LookupIP(ctx, ip, lang))
}

// handleMeLookup 复刻 GET /api/v1/me/ip-geo/{ip}（actions/my-usage.ts:864）。
func (api *ipGeoAPI) handleMeLookup(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	principal, ok := PrincipalFrom(ctx)
	if !ok || principal.UserID == 0 {
		api.problems.WriteProblem(writer, request, http.StatusUnauthorized, "auth.missing",
			"Authentication is required.")
		return
	}
	ip := strings.TrimSpace(ParamsFrom(ctx)["ip"])
	if ip == "" {
		// 动作内 `if (!ip)` → REQUIRED_FIELD（400）。空段不进路由，这条挡的是纯空白。
		api.problems.WriteActionError(writer, request,
			NewActionError("me", "REQUIRED_FIELD", http.StatusBadRequest, nil))
		return
	}
	// MeIpGeoQuerySchema: lang 是裸 `z.string().optional()`，空串放行（故不校验 min(1)）。
	lang, provided := queryParam(request, "lang")
	if !provided {
		lang = "en"
	}

	enabled, err := api.lookupEnabled(ctx)
	if err != nil {
		api.writeIPGeoFailure(writer, request, err)
		return
	}
	if !enabled {
		api.problems.WriteActionError(writer, request,
			NewActionError("me", "INVALID_STATE", http.StatusBadRequest, nil))
		return
	}

	// 可见性判据：该 IP 必须出现在**本密钥**的可见日志里（Node 的两段查询）。
	visible, err := api.ipVisibleForKey(ctx, principal.KeyID, ip)
	if err != nil {
		api.writeIPGeoFailure(writer, request, err)
		return
	}
	if !visible {
		api.problems.WriteActionError(writer, request,
			NewActionError("me", "NOT_FOUND", http.StatusNotFound, nil))
		return
	}

	writeIPGeoResult(api, writer, request, api.lookup.LookupIP(ctx, ip, lang))
}

// ipVisibleForKey 复刻 getMyIpGeoDetails 的可见性两段查询：
//
//	① message_request 里该密钥 + 该 client_ip 的未删除、非预热行；
//	② 否则看 usage_ledger：同一密钥 + 同一 client_ip，且对应的 message_request 行**已删除**
//	   （Node 的 `not exists (… deleted_at is null …)` 子查询——账本保留、请求行被清掉的场景）。
//
// keyID 为 0（ADMIN_TOKEN 合成身份）时无密钥可查：Node 那边 session.key 是合成键，同样查不到 →
// 按不可见处理（NOT_FOUND），与这里的早退一致。
func (api *ipGeoAPI) ipVisibleForKey(ctx context.Context, keyID int64, ip string) (bool, error) {
	if keyID == 0 {
		return false, nil
	}
	keyValue, found, err := api.pools.AdminKeyStringByID(ctx, keyID)
	if err != nil {
		return false, err
	}
	if !found || keyValue == "" {
		return false, nil
	}
	return api.pools.IPGeoVisibleForKey(ctx, keyValue, ip)
}

// writeIPGeoResult 把查询结果直接透传（Node：`Response.json(result)`）。
//
// 头也照抄：`Cache-Control: private, max-age=60`（Node 三条都设；根级那条注释说明服务端已缓存，
// 浏览器侧只留 60s 的私有缓存）。
func writeIPGeoResult(api *ipGeoAPI, writer http.ResponseWriter, request *http.Request, result ipgeo.Result) {
	writer.Header().Set("Cache-Control", "private, max-age=60")
	writeRawJSON(writer, http.StatusOK, result)
}

// writeIPGeoFailure 作答「读设置/查库失败」：Node 是 catch 分支的 500。
func (api *ipGeoAPI) writeIPGeoFailure(writer http.ResponseWriter, request *http.Request, err error) {
	api.logger.Error("ip_geo_dependency_failed", map[string]any{
		"path":  request.URL.Path,
		"error": err.Error(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		// 系统设置缺行：Node 的 getSystemSettings 会补默认行，这里视作依赖故障作答
		// （补行是 EnsureAdminSystemSettings 的职责，能到这步说明补行也失败了）。
		writeRawJSON(writer, http.StatusInternalServerError, map[string]any{"error": "获取系统设置失败"})
		return
	}
	writeRawJSON(writer, http.StatusInternalServerError, map[string]any{"error": "获取系统设置失败"})
}

// queryParam 取查询参数并区分「未传」与「传了空值」——Node 的 `?? "en"` 只对前者兜底。
func queryParam(request *http.Request, name string) (value string, provided bool) {
	values, ok := request.URL.Query()[name]
	if !ok || len(values) == 0 {
		return "", false
	}
	return values[0], true
}
