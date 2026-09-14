package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 **`GET /api/leaderboard`** 的 Go 落点。
//
// 唯一真源：src/app/api/leaderboard/route.ts（参数校验、权限、作答形状、格式化）
// + src/lib/redis/leaderboard-cache.ts（乐观缓存：键方案、60 秒 TTL）。
//
// 权限：Node 是「有会话且（管理员 或 allowGlobalUsageView）」，故路由挂 AccessRead（任意已认证用户），
// 行内的 403 判定自己作答。作答是**裸 JSON 数组**（NoManagementEnvelope，与同族根级端点一致）。
//
// 三处登记差异：
//  1. **省了缓存空窗期的分布式锁**。Node 在缓存未命中时抢先 `SET NX EX 10`，没抢到的轮询 5 秒再回落查库；
//     Go 只做「读缓存 → 未命中查库 → 写缓存 60 秒」。代价是多实例同时冷启动时会有并发查库
//     （最多 N 次重复计算），语义上仍满足「≤60 秒陈旧」。升级路径：给 LeaderboardCacheStore
//     加 TryLock/Unlock 两个方法。
//  2. 非管理员被拒的正文形状：Go 这里是裸 `{"error":"..."}`（与 Node 同形），但**未登录**的 401 由守卫
//     作答（problem 信封）——与 /api/availability 那三条同一条登记。
//  3. 币种不在白名单时 Go 回退 USD（Node 会在格式化处 500），见 leaderboard_format.go。
//
// 另有一处**必须**说清的取舍：缓存键里嵌着币种与查询选项（照 Node 的 buildCacheKey），
// 故改设置后的失效走 Node 那三族的 `leaderboard:*` 前缀清理（本包 CacheInvalidator 已清）。

const (
	// leaderboardCacheShapeVersion 照 leaderboard-cache.ts 的 CACHE_SHAPE_VERSION。
	leaderboardCacheShapeVersion = "v4"
	// leaderboardCacheTTL 照 Node 的 setex(cacheKey, 60, ...)。
	leaderboardCacheTTL = 60 * time.Second
)

// leaderboardPeriods 照 route.ts 的 VALID_PERIODS（顺序影响 400 正文里的列举）。
var leaderboardPeriods = []string{"daily", "weekly", "monthly", "allTime", "custom"}

// leaderboardScopes 照 route.ts 的五个合法 scope（顺序同样进 400 正文）。
var leaderboardScopes = []string{"user", "userCacheHitRate", "provider", "providerCacheHitRate", "model"}

// leaderboardProviderTypes 照 route.ts 的 isProviderType 白名单（含两个隐藏类型）。
var leaderboardProviderTypes = []string{
	"claude", "claude-auth", "codex", "gemini", "gemini-cli", "openai-compatible",
}

// leaderboardDatePattern 照 route.ts 的 DATE_REGEX。
var leaderboardDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// LeaderboardCacheStore 是乐观缓存的读写面（Redis 实现见下）。
//
// nil 表示未装配（无 Redis 命令连接）：按 Node 的「Redis 不可用则直查」降级，路由照常温。
type LeaderboardCacheStore interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	SetEX(ctx context.Context, key string, ttl time.Duration, value []byte) error
}

// redisLeaderboardCache 是 LeaderboardCacheStore 的 Redis 实现。
type redisLeaderboardCache struct {
	client redis.UniversalClient
}

// NewRedisLeaderboardCache 装配排行榜缓存；client 为 nil 时返回 nil（调用方据此不缓存）。
func NewRedisLeaderboardCache(client redis.UniversalClient) LeaderboardCacheStore {
	if client == nil {
		return nil
	}
	return &redisLeaderboardCache{client: client}
}

func (c *redisLeaderboardCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := c.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(value), true, nil
}

func (c *redisLeaderboardCache) SetEX(
	ctx context.Context,
	key string,
	ttl time.Duration,
	value []byte,
) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

// RegisterLeaderboardRoutes 注册 `GET /api/leaderboard`（Store 未装配时不注册，回退 Node）。
func RegisterLeaderboardRoutes(router *Router, deps Deps, cache LeaderboardCacheStore) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_leaderboard_store_unwired", map[string]any{
				"module": "leaderboard",
				"action": "routes_not_registered",
			})
		}
		return
	}
	api := &leaderboardAPI{
		pools:  deps.Store,
		deps:   deps,
		logger: adminLoggerOf(deps),
		cache:  cache,
	}
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/leaderboard",
		Access:               AccessRead,
		Module:               "leaderboard",
		OperationID:          "getLeaderboard",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleLeaderboard),
	})
}

// leaderboardAPI 持有排行榜端点要的依赖。
type leaderboardAPI struct {
	pools  *store.Pools
	deps   Deps
	logger *logx.Logger
	cache  LeaderboardCacheStore
}

// leaderboardRequestOptions 是路由层解析出来的查询选项（对应 route.ts 的局部变量）。
type leaderboardRequestOptions struct {
	period                string
	scope                 string
	startDate             string
	endDate               string
	providerType          string
	includeModelStats     bool
	includeUserModelStats bool
	userTags              []string
	userGroups            []string
}

// handleLeaderboard 复刻 GET /api/leaderboard 的整条链路。
func (api *leaderboardAPI) handleLeaderboard(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	principal, _ := PrincipalFrom(ctx)

	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		api.logger.Error("admin_leaderboard_settings_failed", map[string]any{"error": err.Error()})
		leaderboardError(writer, http.StatusInternalServerError, "获取排行榜数据失败")
		return
	}
	// Node 的判定顺序是「先会话、再读设置、再看权限」，这里会话已由守卫处理。
	isAdmin := principal.IsAdmin
	allowGlobal := settings != nil && settings.AllowGlobalUsageView
	if !isAdmin && !allowGlobal {
		leaderboardError(writer, http.StatusForbidden,
			"无权限访问排行榜，请联系管理员开启全站使用权限")
		return
	}

	options, status, message := leaderboardParseOptions(request, isAdmin)
	if message != "" {
		leaderboardError(writer, status, message)
		return
	}

	entries, err := api.queryLeaderboard(ctx, options, settings, isAdmin)
	if err != nil {
		api.logger.Error("admin_leaderboard_query_failed", map[string]any{
			"scope": options.scope, "period": options.period, "error": err.Error(),
		})
		leaderboardError(writer, http.StatusInternalServerError, "获取排行榜数据失败")
		return
	}

	// Node 的 Cache-Control：带用户级模型拆分或未开全站可见时禁止共享缓存。
	cacheControl := "public, s-maxage=60, stale-while-revalidate=120"
	if options.includeUserModelStats || !allowGlobal {
		cacheControl = "private, no-store"
	}
	writer.Header().Set("Cache-Control", cacheControl)
	adminWriteJSON(writer, http.StatusOK, entries)
}

// leaderboardParseOptions 复刻 route.ts 的参数校验，按 Node 的判定顺序返回。
func leaderboardParseOptions(
	request *http.Request,
	isAdmin bool,
) (leaderboardRequestOptions, int, string) {
	query := request.URL.Query()
	options := leaderboardRequestOptions{
		period: query.Get("period"),
		scope:  query.Get("scope"),
	}
	if options.period == "" {
		options.period = "daily"
	}
	if options.scope == "" {
		options.scope = "user"
	}
	if !leaderboardContains(leaderboardPeriods, options.period) {
		return options, http.StatusBadRequest,
			"参数 period 必须是 " + strings.Join(leaderboardPeriods, ", ") + " 之一"
	}
	if !leaderboardContains(leaderboardScopes, options.scope) {
		return options, http.StatusBadRequest,
			"参数 scope 必须是 'user'、'userCacheHitRate'、'provider'、'providerCacheHitRate' 或 'model'"
	}

	options.startDate = query.Get("startDate")
	options.endDate = query.Get("endDate")
	if options.period == "custom" {
		if options.startDate == "" || options.endDate == "" {
			return options, http.StatusBadRequest,
				"当 period=custom 时，必须提供 startDate 和 endDate 参数"
		}
		if !leaderboardDatePattern.MatchString(options.startDate) ||
			!leaderboardDatePattern.MatchString(options.endDate) {
			return options, http.StatusBadRequest, "日期格式必须是 YYYY-MM-DD"
		}
		// Node 是 `new Date(startDate) > new Date(endDate)`：解析不出日期时比较为 false（放行）。
		// 这里同判——放行后交给 SQL，`'2026-13-45'::date` 会以 500 收场，与 Node 相同。
		start, startErr := time.Parse("2006-01-02", options.startDate)
		end, endErr := time.Parse("2006-01-02", options.endDate)
		if startErr == nil && endErr == nil && start.After(end) {
			return options, http.StatusBadRequest, "startDate 不能大于 endDate"
		}
	}

	if (options.scope == "provider" || options.scope == "providerCacheHitRate") &&
		query.Get("providerType") != "" && query.Get("providerType") != "all" {
		if !leaderboardContains(leaderboardProviderTypes, query.Get("providerType")) {
			return options, http.StatusBadRequest, "参数 providerType 不合法"
		}
		options.providerType = query.Get("providerType")
	}

	options.includeModelStats = options.scope == "provider" &&
		leaderboardFlagEnabled(query.Get("includeModelStats"))
	options.includeUserModelStats =
		(options.scope == "user" || options.scope == "userCacheHitRate") &&
			leaderboardFlagEnabled(query.Get("includeUserModelStats"))
	if options.includeUserModelStats && !isAdmin {
		return options, http.StatusForbidden, "INCLUDE_USER_MODEL_STATS_ADMIN_REQUIRED"
	}

	if options.scope == "user" || options.scope == "userCacheHitRate" {
		options.userTags = leaderboardParseList(query.Get("userTags"))
		options.userGroups = leaderboardParseList(query.Get("userGroups"))
	}
	return options, http.StatusOK, ""
}

// leaderboardFlagEnabled 复刻 route.ts 的三值开关判定（1 / true / yes）。
func leaderboardFlagEnabled(value string) bool {
	return value == "1" || value == "true" || value == "yes"
}

// leaderboardParseList 复刻 parseListParam：逗号分隔、去空白、丢空项、最多 20 项。
func leaderboardParseList(raw string) []string {
	if raw == "" {
		return nil
	}
	items := make([]string, 0, 4)
	for _, token := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" {
			continue
		}
		items = append(items, trimmed)
		if len(items) == 20 {
			break
		}
	}
	if len(items) == 0 {
		return nil
	}
	return items
}

// leaderboardContains 是字符串白名单判定。
func leaderboardContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// leaderboardStoreQuery 把路由选项翻成 store 的查询条件。
func leaderboardStoreQuery(
	options leaderboardRequestOptions,
	settings *store.SystemSettings,
	timezone string,
) store.AdminLeaderboardQuery {
	// 取值与缺省均以 Node 为契约：设置行缺失 → "original"（详见 resolveBillingModelSource）。
	// 曾误用域外值 "model"（Node 的取值域只有 original/redirected）作为回退，
	// 等于在设置行缺失时按「重定向后模型」聚合，与 Node 相反。
	billingModelSource := resolveBillingModelSource(settings)
	return store.AdminLeaderboardQuery{
		Period:             options.period,
		Timezone:           timezone,
		StartDate:          options.startDate,
		EndDate:            options.endDate,
		ProviderType:       options.providerType,
		UserTags:           options.userTags,
		UserGroups:         options.userGroups,
		BillingModelSource: billingModelSource,
	}
}

// queryLeaderboard 取一份数据（先缓存，未命中查库再回写），并按 scope 组装作答。
//
// 缓存里存的是**已格式化的作答条目**。与 Node 的差别登记：Node 缓存 repository 的原始行、
// 每次请求再格式化；这里直接缓存终态。两者在 TTL 内的可观测结果相同（币种与查询选项都嵌在
// 缓存键里，故不会出现「换了币种拿到旧格式」），代价是改动作答形状后旧值的存活期最长 60 秒
// ——缓存键里的 CACHE_SHAPE_VERSION 就是为这种改动准备的（与 Node 同机制）。
func (api *leaderboardAPI) queryLeaderboard(
	ctx context.Context,
	options leaderboardRequestOptions,
	settings *store.SystemSettings,
	isAdmin bool,
) (any, error) {
	timezone := api.pools.AdminSystemTimezoneOrUTC(ctx)
	currency := "USD"
	if settings != nil && settings.CurrencyDisplay != "" {
		currency = settings.CurrencyDisplay
	}
	query := leaderboardStoreQuery(options, settings, timezone)

	cacheKey := leaderboardCacheKey(options, timezone, currency, query.BillingModelSource)
	if payload, ok := api.readCachedLeaderboard(ctx, cacheKey); ok {
		return leaderboardDecodeCached(payload, options, currency, query.BillingModelSource)
	}

	entries, err := api.computeLeaderboard(ctx, options, query, currency, isAdmin)
	if err != nil {
		return nil, err
	}
	api.writeCachedLeaderboard(ctx, cacheKey, entries)
	return entries, nil
}

// readCachedLeaderboard 读缓存。Redis 报错按 Node 的降级走直查（只记日志）。
func (api *leaderboardAPI) readCachedLeaderboard(ctx context.Context, key string) ([]byte, bool) {
	if api.cache == nil {
		return nil, false
	}
	payload, ok, err := api.cache.Get(ctx, key)
	if err != nil {
		api.logger.Warn("admin_leaderboard_cache_read_failed", map[string]any{"error": err.Error()})
		return nil, false
	}
	return payload, ok
}

// writeCachedLeaderboard 回写缓存（失败只记日志，不影响作答）。
func (api *leaderboardAPI) writeCachedLeaderboard(ctx context.Context, key string, entries any) {
	if api.cache == nil {
		return
	}
	payload, err := json.Marshal(entries)
	if err != nil {
		api.logger.Warn("admin_leaderboard_cache_encode_failed", map[string]any{"error": err.Error()})
		return
	}
	if err := api.cache.SetEX(ctx, key, leaderboardCacheTTL, payload); err != nil {
		api.logger.Warn("admin_leaderboard_cache_write_failed", map[string]any{"error": err.Error()})
	}
}

// leaderboardDecodeCached 把缓存的 JSON 还原成作答条目（按 scope 选容器类型）。
func leaderboardDecodeCached(
	payload []byte,
	options leaderboardRequestOptions,
	currency string,
	billingModelSource string,
) (any, error) {
	target := leaderboardEntrySlice(options, billingModelSource)
	if err := json.Unmarshal(payload, target); err != nil {
		return nil, fmt.Errorf("adminapi: 解析排行榜缓存失败: %w", err)
	}
	return target, nil
}

// leaderboardError 复刻这几条的失败正文：`{"error": "..."}`。
func leaderboardError(writer http.ResponseWriter, status int, message string) {
	adminWriteJSON(writer, status, map[string]string{"error": message})
}

// leaderboardCacheKey 复刻 buildCacheKey（leaderboard:v4:{scope}:...）。
//
// 周键是 ISO 周（`yyyy-'W'ww`），故用 ISOWeek 而不是自然周。
func leaderboardCacheKey(
	options leaderboardRequestOptions,
	timezone string,
	currency string,
	billingModelSource string,
) string {
	prefix := "leaderboard:" + leaderboardCacheShapeVersion + ":" + options.scope
	providerTypeSuffix := ""
	if options.providerType != "" {
		providerTypeSuffix = ":providerType:" + options.providerType
	}
	includeSuffix := ""
	if (options.scope == "provider" || options.scope == "user" ||
		options.scope == "userCacheHitRate") && options.includeModelStatsRequested() {
		includeSuffix = ":includeModelStats"
	}
	userFilterSuffix := ""
	if options.scope == "user" || options.scope == "userCacheHitRate" {
		userFilterSuffix = leaderboardUserFilterSuffix(options)
	}
	tail := ":tz:" + timezone + ":" + currency + providerTypeSuffix + includeSuffix + userFilterSuffix

	location, err := time.LoadLocation(timezone)
	now := time.Now()
	if err == nil {
		now = now.In(location)
	}
	switch options.period {
	case "custom":
		return prefix + ":custom:" + options.startDate + "_" + options.endDate + tail
	case "weekly":
		year, week := now.ISOWeek()
		return prefix + ":weekly:" + fmt.Sprintf("%04d-W%02d", year, week) + tail
	case "monthly":
		return prefix + ":monthly:" + now.Format("2006-01") + tail
	case "allTime":
		return prefix + ":allTime" + tail
	default:
		return prefix + ":daily:" + now.Format("2006-01-02") + tail
	}
}

// includeModelStatsRequested 是缓存键里的 includeModelStats 段判定。
//
// Node 的缓存键只看 `filters.includeModelStats`，而调用方传的是 `includeModelStats || includeUserModelStats`
// （见 route.ts 的 getLeaderboardWithCache 入参）——故两个开关任一为真都算。
func (o leaderboardRequestOptions) includeModelStatsRequested() bool {
	return o.includeModelStats || o.includeUserModelStats
}

// leaderboardUserFilterSuffix 复刻缓存键里的标签/分组段（各自**排序后**拼接）。
func leaderboardUserFilterSuffix(options leaderboardRequestOptions) string {
	suffix := ""
	if len(options.userTags) > 0 {
		tags := append([]string(nil), options.userTags...)
		leaderboardSortStrings(tags)
		suffix += ":tags:" + strings.Join(tags, ",")
	}
	if len(options.userGroups) > 0 {
		groups := append([]string(nil), options.userGroups...)
		leaderboardSortStrings(groups)
		suffix += ":groups:" + strings.Join(groups, ",")
	}
	return suffix
}

// leaderboardSortStrings 是最小插入排序：这些列表最多 20 项，且只为稳定复现缓存的键（不做本地化排序）。
func leaderboardSortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for inner := index; inner > 0 && values[inner] < values[inner-1]; inner-- {
			values[inner], values[inner-1] = values[inner-1], values[inner]
		}
	}
}

// leaderboardEntrySlice 按 scope 给出作答容器（缓存解码与查库共用同一套类型）。
func leaderboardEntrySlice(
	options leaderboardRequestOptions,
	billingModelSource string,
) any {
	switch options.scope {
	case "userCacheHitRate":
		if options.includeUserModelStats {
			return &[]leaderboardUserCacheEntryWithModels{}
		}
		return &[]leaderboardUserCacheEntry{}
	case "provider":
		if options.includeModelStats {
			return &[]leaderboardProviderEntryWithModels{}
		}
		return &[]leaderboardProviderEntry{}
	case "providerCacheHitRate":
		return &[]leaderboardProviderCacheEntry{}
	case "model":
		return &[]leaderboardModelEntry{}
	default:
		if options.includeUserModelStats {
			return &[]leaderboardUserEntryWithModels{}
		}
		return &[]leaderboardUserEntry{}
	}
}

// computeLeaderboard 查库并把结果整理成作答条目（未格式化之外的字段都在这里定）。
func (api *leaderboardAPI) computeLeaderboard(
	ctx context.Context,
	options leaderboardRequestOptions,
	query store.AdminLeaderboardQuery,
	currency string,
	isAdmin bool,
) (any, error) {
	switch options.scope {
	case "user":
		return api.userLeaderboard(ctx, query, currency, options.includeUserModelStats)
	case "userCacheHitRate":
		return api.userCacheLeaderboard(ctx, query, currency, options.includeUserModelStats)
	case "provider":
		return api.providerLeaderboard(ctx, query, currency, options.includeModelStats)
	case "providerCacheHitRate":
		return api.providerCacheLeaderboard(ctx, query, currency)
	default:
		return api.modelLeaderboard(ctx, query, currency)
	}
}

// clampRatio01 照 leaderboard.ts 的 clampRatio01。
func clampRatio01(value float64) float64 {
	return math.Min(math.Max(value, 0), 1)
}

// clampRatio01Ptr 照 clampRatio01Nullable（nil 仍是 nil）。
func clampRatio01Ptr(value *float64) *float64 {
	if value == nil {
		return nil
	}
	clamped := clampRatio01(*value)
	return &clamped
}

// strconvFloatText 把 numeric 文本转 float64（空串按 0，与 Node 的 parseFloat 分支同义）。
func strconvFloatText(text string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0
	}
	return value
}
