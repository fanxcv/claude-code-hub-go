package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是管理面的 **me 资源**（自服务面 /api/v1/me/*），逐条对齐 Node 的
// src/app/api/v1/resources/me/{router,handlers}.ts 与 src/actions/my-usage.ts。
//
// 它与其它资源模块的**唯一结构性差别**：主体不是路径参数而是**认证身份**。Node 侧靠
// getSession({ allowReadOnlyAccess: true }) 取 session.key/session.user；Go 侧取
// PrincipalFrom(ctx) 的 KeyID/UserID 再按 id 读行。因此：
//
//  1. 本文件里**没有任何主体筛选参数**——越权在签名层面不可能（与 keys 的属主判定不同，
//     那条是「路径参数 vs 身份」的比较，这条连比较都不需要）。
//  2. Access 档位一律 read（Node 的 x-required-access: read）：未认证由守卫拒绝，
//     能进到处理器的调用方一定有身份。
//
// 三处照抄 Node 的细节：
//
//   - **虚拟管理员会话**（ADMIN_TOKEN，KeyID = -1）：Node 合成一份 key/user（auth.ts:185-233：
//     key.name "ADMIN_TOKEN"、user.name "Admin Token"、限额全 null、dailyResetMode "fixed"、
//     limit5hResetMode "rolling"）。Go 侧同形输出；唯一差异是**成本类读数恒 0**——Node 会拿
//     ADMIN_TOKEN 字符串去查账本（实务上无行，因为该串不是代理密钥），而 Go 侧拿不到这个串
//     （Principal 不含密钥原文）。差异已在报告里登记，取值与 Node 的实际结果一致（0）。
//   - **5h 固定窗口走 Redis 运行态**（`CostWindows`），滚动窗口走账本聚合；两者在 Node 里是
//     两条分支（service.ts:1264-1266），不能合并成一条。
//   - **密钥维度成本的起算点**取 key 与 user 两个 costResetAt 的**较晚者**（Node 的
//     resolveKeyCostResetAt），用户维度则分开取（5h 用较晚者、其余用 user.costResetAt）。
//
// 未实现并原样回退 Node 的端点：GET /me/ip-geo/{ip}（依赖 @/lib/ip-geo/client 的外部归属地
// 服务，Go 侧无该客户端；缺它就注册等于用假数据作答）。

// UserSessionCounter 读 User 级活跃会话数（Node 的 SessionTracker.getUserSessionCount）。
//
// 为什么与 SessionCounter（Key 级）分开：Node 的 userCurrentConcurrentSessions 取的是
// **User 维度 ZSET** 的计数，而不是该用户各密钥计数之和——同一会话跨密钥续用时两者不同。
// 该 ZSET 由数据面维护（internal/session/tracker.go），故这里只需读。
type UserSessionCounter interface {
	UserSessionCount(ctx context.Context, userID int64) (int, error)
}

// UserFixed5hWindowReader 读 User 维度的 5h 固定窗口累计值。
//
// 与 Fixed5hWindowReader（Key 维度）分开的理由同 UserSessionCounter：Node 侧
// RateLimitService.getCurrentCost(user.id, "user", "5h", ...) 的键按 user 维度构造。
type UserFixed5hWindowReader interface {
	UserFixed5hWindowState(ctx context.Context, userID int64, now time.Time) (limit.Fixed5hState, error)
}

// meAPI 是 me 资源处理器的依赖集合（与 keysAPI/usersAPI 同构，装配期构造一次）。
type meAPI struct {
	deps     Deps
	pools    *store.Pools
	problems ProblemWriter
	logger   *logx.Logger
	now      func() time.Time
	// 四个 Redis 运行态读数均可空：缺任一侧时只影响**用到它的那一条**端点
	// （quota 缺读数就不注册；metadata/today 完全不需要 Redis）。
	keySessions  SessionCounter
	userSessions UserSessionCounter
	keyFixed5h   Fixed5hWindowReader
	userFixed5h  UserFixed5hWindowReader
}

// newMeAPI 组装处理器依赖；Store 未装配时返回 nil（调用方据此不注册路由）。
func newMeAPI(deps Deps) *meAPI {
	if deps.Store == nil {
		return nil
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := deps.Problems
	if problems == nil {
		problems = NewProblems(logger)
	}
	return &meAPI{
		deps:         deps,
		pools:        deps.Store,
		problems:     problems,
		logger:       logger,
		now:          time.Now,
		keySessions:  deps.SessionCounts,
		userSessions: deps.UserSessionCounts,
		keyFixed5h:   deps.Fixed5hWindows,
		userFixed5h:  deps.UserFixed5hWindows,
	}
}

// RegisterMeRoutes 注册 me 资源的全部端点。
//
// 降级规则（与本包同一条纪律：宁可不答，不可乱答）：
//   - Store 未装配 → 全部不注册（回退 Node）。
//   - Redis 运行态读数缺任一侧 → 只不注册 /me/quota（它的 userCurrentConcurrentSessions 与
//     5h 固定窗口读不出来；恒 0 是静默错数）。
func RegisterMeRoutes(router *Router, deps Deps) {
	api := newMeAPI(deps)
	if api == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_me_store_unwired", map[string]any{
				"module": "me",
				"action": "routes_not_registered",
			})
		}
		return
	}
	api.register(router)
}

// register 逐条登记路由。Access 取自 Node OpenAPI 的 x-required-access（一律 read）。
func (api *meAPI) register(router *Router) {
	type entry struct {
		method string
		path   string
		op     string
		handle http.HandlerFunc
	}
	entries := []entry{
		{http.MethodGet, "/me/metadata", "getMeMetadata", api.handleMeMetadata},
		{http.MethodGet, "/me/today", "getMeToday", api.handleMeToday},
		{http.MethodGet, "/me/usage-logs", "listMeUsageLogs", api.handleMeUsageLogs},
		{http.MethodGet, "/me/usage-logs/full", "listMeUsageLogsFull", api.handleMeUsageLogsFull},
		{http.MethodGet, "/me/usage-logs/models", "listMeUsageModels", api.handleMeUsageModels},
		{http.MethodGet, "/me/usage-logs/endpoints", "listMeUsageEndpoints", api.handleMeUsageEndpoints},
		{http.MethodGet, "/me/usage-logs/stats-summary", "getMeStatsSummary", api.handleMeStatsSummary},
	}
	if api.hasQuotaRuntime() {
		entries = append(entries, entry{http.MethodGet, "/me/quota", "getMeQuota", api.handleMeQuota})
	} else if api.logger != nil {
		api.logger.Warn("admin_me_quota_unwired", map[string]any{
			"path":   "/me/quota",
			"reason": "session_or_fixed5h_reader_missing",
			"action": "route_not_registered",
		})
	}
	for _, item := range entries {
		router.Add(Route{
			Method:      item.method,
			Path:        item.path,
			Access:      AccessRead,
			Module:      "me",
			OperationID: item.op,
			Handler:     item.handle,
		})
	}
}

// hasQuotaRuntime 判断 quota 所需的四个运行态读数是否齐备。
func (api *meAPI) hasQuotaRuntime() bool {
	return api.keySessions != nil && api.userSessions != nil &&
		api.keyFixed5h != nil && api.userFixed5h != nil
}

// meSubject 是当前调用方的主体：数据库行（虚拟管理员会话时为 nil）与身份。
type meSubject struct {
	principal Principal
	key       *store.AdminKeyRecord
	user      *store.AdminUserRow
}

// isVirtual 表示这是 ADMIN_TOKEN 合成的会话（Node auth.ts:185-233）。
func (s meSubject) isVirtual() bool { return s.key == nil }

// resolveMeSubject 按身份读主体行。
//
// 虚拟管理员会话（KeyID = -1）不读库：Node 也不读（它整份合成），且 -1 在库里没有行。
func (api *meAPI) resolveMeSubject(ctx context.Context) (meSubject, error) {
	principal, _ := PrincipalFrom(ctx)
	subject := meSubject{principal: principal}
	if principal.KeyID <= 0 {
		return subject, nil
	}
	key, err := api.pools.FindAdminKeyByID(ctx, principal.KeyID)
	if err != nil {
		return subject, err
	}
	subject.key = key
	if principal.UserID > 0 {
		user, userErr := api.pools.FindAdminUserByID(ctx, principal.UserID)
		if userErr != nil {
			return subject, userErr
		}
		subject.user = user
	}
	return subject, nil
}

// meMetadataBody 逐字对应 Node 的 MyUsageMetadata（my-usage.ts:157-170），键序一致。
type meMetadataBody struct {
	KeyName            string `json:"keyName"`
	KeyProviderGroup   any    `json:"keyProviderGroup"`
	KeyExpiresAt       any    `json:"keyExpiresAt"`
	KeyIsEnabled       bool   `json:"keyIsEnabled"`
	UserName           string `json:"userName"`
	UserProviderGroup  any    `json:"userProviderGroup"`
	UserExpiresAt      any    `json:"userExpiresAt"`
	UserIsEnabled      bool   `json:"userIsEnabled"`
	DailyResetMode     string `json:"dailyResetMode"`
	DailyResetTime     string `json:"dailyResetTime"`
	CurrencyCode       string `json:"currencyCode"`
	BillingModelSource string `json:"billingModelSource"`
}

// handleMeMetadata 复刻 getMeMetadata（my-usage.ts:301-330）。
func (api *meAPI) handleMeMetadata(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	subject, err := api.resolveMeSubject(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}
	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}

	body := meMetadataBody{
		CurrencyCode:       settings.CurrencyDisplay,
		BillingModelSource: settings.BillingModelSource,
		DailyResetMode:     "fixed",
		DailyResetTime:     "00:00",
	}
	if subject.isVirtual() {
		// Node 的合成会话（auth.ts:193-221）：限额与分组全 null，重置模式 fixed/00:00。
		body.KeyName = virtualAdminKeyName
		body.UserName = virtualAdminUserName
		body.KeyIsEnabled = true
		body.UserIsEnabled = true
		writeShellJSON(writer, http.StatusOK, body)
		return
	}

	record := subject.key
	body.KeyName = record.Name
	body.KeyProviderGroup = nilIfNilString(record.ProviderGroup)
	body.KeyExpiresAt = store.JSONDatePtr(record.ExpiresAt)
	body.KeyIsEnabled = boolOrTrue(record.IsEnabled)
	body.DailyResetMode = string(keysResetModeOrDefault(record.DailyResetMode, limit.ResetFixed))
	body.DailyResetTime = limit.NormalizeResetTime(usersStringOr(record.DailyResetTime, "00:00"))

	if user := subject.user; user != nil {
		body.UserName = user.Name
		body.UserProviderGroup = nilIfNilString(user.ProviderGroup)
		body.UserExpiresAt = store.JSONDatePtr(user.ExpiresAt)
		body.UserIsEnabled = boolOrTrue(user.IsEnabled)
	} else {
		// 用户行缺失（软删或数据不一致）：Node 的 session.user 来自认证链路，缺了就没有会话，
		// 因此这里是不可达兜底——留空而不是编造。
		body.UserName = subject.principal.Username
		body.UserIsEnabled = true
	}
	writeShellJSON(writer, http.StatusOK, body)
}

// meTodayModelBody 逐字对应 MyTodayStats.modelBreakdown 的单项（my-usage.ts:214-221）。
type meTodayModelBody struct {
	Model        any     `json:"model"`
	BillingModel any     `json:"billingModel"`
	Calls        int64   `json:"calls"`
	CostUSD      float64 `json:"costUsd"`
	InputTokens  float64 `json:"inputTokens"`
	OutputTokens float64 `json:"outputTokens"`
}

// meTodayBody 逐字对应 MyTodayStats（my-usage.ts:209-227）。
type meTodayBody struct {
	Calls              int64              `json:"calls"`
	InputTokens        float64            `json:"inputTokens"`
	OutputTokens       float64            `json:"outputTokens"`
	CostUSD            float64            `json:"costUsd"`
	ModelBreakdown     []meTodayModelBody `json:"modelBreakdown"`
	CurrencyCode       string             `json:"currencyCode"`
	BillingModelSource string             `json:"billingModelSource"`
}

// handleMeToday 复刻 getMyTodayStats（my-usage.ts:541-635）。
//
// 时间窗用**密钥的** dailyResetTime/dailyResetMode（Node 用 key 而非 user，见 :557-562）。
func (api *meAPI) handleMeToday(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	subject, err := api.resolveMeSubject(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}
	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}

	body := meTodayBody{
		ModelBreakdown:     []meTodayModelBody{},
		CurrencyCode:       settings.CurrencyDisplay,
		BillingModelSource: settings.BillingModelSource,
	}
	if subject.isVirtual() {
		// 虚拟会话的成本读数为 0（差异登记见文件头）。
		writeShellJSON(writer, http.StatusOK, body)
		return
	}

	location, err := meSystemLocation(ctx, api.pools)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}
	resetMode := keysResetModeOrDefault(subject.key.DailyResetMode, limit.ResetFixed)
	resetTime := limit.NormalizeResetTime(usersStringOr(subject.key.DailyResetTime, "00:00"))
	now := api.now()
	start := limit.WindowStart(limit.PeriodDaily, now, resetTime, resetMode, location)

	rows, err := api.pools.MeTodayLedgerBreakdown(ctx, subject.key.Key, start, now)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}

	billingSource := settings.BillingModelSource
	// 累加顺序与 Node 相同（同一个循环里做加总与逐行映射），因此浮点累加次序也一致。
	for _, row := range rows {
		billingModel := row.Model
		if billingSource == "original" {
			billingModel = row.OriginalModel
		}
		cost := parseCostText(row.CostUSD)
		body.Calls += row.Calls
		body.InputTokens += row.InputTokens
		body.OutputTokens += row.OutputTokens
		body.CostUSD += cost
		body.ModelBreakdown = append(body.ModelBreakdown, meTodayModelBody{
			Model:        nilIfNilString(row.Model),
			BillingModel: nilIfNilString(billingModel),
			Calls:        row.Calls,
			CostUSD:      cost,
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
		})
	}
	writeShellJSON(writer, http.StatusOK, body)
}

// meQuotaBody 逐字对应 Node 的 MyUsageQuota（my-usage.ts:172-207），键序一致。
type meQuotaBody struct {
	KeyLimit5hUSD                any     `json:"keyLimit5hUsd"`
	KeyLimitDailyUSD             any     `json:"keyLimitDailyUsd"`
	KeyLimitWeeklyUSD            any     `json:"keyLimitWeeklyUsd"`
	KeyLimitMonthlyUSD           any     `json:"keyLimitMonthlyUsd"`
	KeyLimitTotalUSD             any     `json:"keyLimitTotalUsd"`
	KeyLimitConcurrentSessions   int     `json:"keyLimitConcurrentSessions"`
	KeyCurrent5hUSD              float64 `json:"keyCurrent5hUsd"`
	KeyCurrentDailyUSD           float64 `json:"keyCurrentDailyUsd"`
	KeyCurrentWeeklyUSD          float64 `json:"keyCurrentWeeklyUsd"`
	KeyCurrentMonthlyUSD         float64 `json:"keyCurrentMonthlyUsd"`
	KeyCurrentTotalUSD           float64 `json:"keyCurrentTotalUsd"`
	KeyCurrentConcurrentSessions int     `json:"keyCurrentConcurrentSessions"`

	UserLimit5hUSD           any     `json:"userLimit5hUsd"`
	UserLimitWeeklyUSD       any     `json:"userLimitWeeklyUsd"`
	UserLimitMonthlyUSD      any     `json:"userLimitMonthlyUsd"`
	UserLimitTotalUSD        any     `json:"userLimitTotalUsd"`
	UserLimitConcurrentSess  any     `json:"userLimitConcurrentSessions"`
	UserRPMLimit             any     `json:"userRpmLimit"`
	UserCurrent5hUSD         float64 `json:"userCurrent5hUsd"`
	UserCurrentDailyUSD      float64 `json:"userCurrentDailyUsd"`
	UserCurrentWeeklyUSD     float64 `json:"userCurrentWeeklyUsd"`
	UserCurrentMonthlyUSD    float64 `json:"userCurrentMonthlyUsd"`
	UserCurrentTotalUSD      float64 `json:"userCurrentTotalUsd"`
	UserCurrentConcurrentSum int     `json:"userCurrentConcurrentSessions"`

	UserLimitDailyUSD  any      `json:"userLimitDailyUsd"`
	UserExpiresAt      any      `json:"userExpiresAt"`
	UserProviderGroup  any      `json:"userProviderGroup"`
	UserName           string   `json:"userName"`
	UserIsEnabled      bool     `json:"userIsEnabled"`
	KeyProviderGroup   any      `json:"keyProviderGroup"`
	KeyName            string   `json:"keyName"`
	KeyIsEnabled       bool     `json:"keyIsEnabled"`
	UserAllowedModels  []string `json:"userAllowedModels"`
	UserAllowedClients []string `json:"userAllowedClients"`
	ExpiresAt          any      `json:"expiresAt"`
	DailyResetMode     string   `json:"dailyResetMode"`
	DailyResetTime     string   `json:"dailyResetTime"`
}

// handleMeQuota 复刻 getMyQuota（my-usage.ts:332-538）。
func (api *meAPI) handleMeQuota(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	subject, err := api.resolveMeSubject(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}

	// 虚拟管理员会话：Node 的合成主体（src/lib/auth.ts:196-212）把 rpm 与 dailyQuota 都写死 0，
	// 而 getMyQuota 直接取 `user.rpm ?? null` / `user.dailyQuota ?? null`（my-usage.ts:508/516），
	// 于是这两项是**数字 0**而不是 null；其余限额（limit5hUsd / limitConcurrentSessions 等）
	// 合成主体里没有对应字段，`?? null` 落回 null。成本全 0（getKeyStringByIdCached(-1) 为空）。
	if subject.isVirtual() {
		writeShellJSON(writer, http.StatusOK, meQuotaBody{
			KeyLimitConcurrentSessions: 0,
			UserRPMLimit:               0,
			UserLimitDailyUSD:          0,
			KeyCurrent5hUSD:            0, KeyCurrentDailyUSD: 0,
			KeyCurrentWeeklyUSD: 0, KeyCurrentMonthlyUSD: 0, KeyCurrentTotalUSD: 0,
			KeyCurrentConcurrentSessions: 0,
			UserCurrent5hUSD:             0, UserCurrentDailyUSD: 0,
			UserCurrentWeeklyUSD: 0, UserCurrentMonthlyUSD: 0, UserCurrentTotalUSD: 0,
			UserCurrentConcurrentSum: 0,
			UserName:                 virtualAdminUserName,
			UserIsEnabled:            true,
			KeyName:                  virtualAdminKeyName,
			KeyIsEnabled:             true,
			UserAllowedModels:        []string{},
			UserAllowedClients:       []string{},
			DailyResetMode:           "fixed",
			DailyResetTime:           "00:00",
		})
		return
	}

	body, failure := api.loadMeQuota(ctx, subject)
	if failure != nil {
		api.writeMeFailure(writer, request, failure)
		return
	}
	writeShellJSON(writer, http.StatusOK, body)
}

// loadMeQuota 是 quota 的计算主体（Node 的 :360-537 那段）。
func (api *meAPI) loadMeQuota(ctx context.Context, subject meSubject) (meQuotaBody, error) {
	record := subject.key
	user := subject.user
	if user == nil {
		return meQuotaBody{}, errors.New("adminapi: 自服务 quota 需要用户行")
	}
	location, err := meSystemLocation(ctx, api.pools)
	if err != nil {
		return meQuotaBody{}, err
	}
	now := api.now()

	// Key 窗口：daily 用密钥自己的重置配置；5h 用密钥自己的模式。
	keyResetMode := keysResetModeOrDefault(record.DailyResetMode, limit.ResetFixed)
	keyResetTime := limit.NormalizeResetTime(usersStringOr(record.DailyResetTime, "00:00"))
	keyLimit5hMode := keysResetModeOrDefault(record.Limit5hResetMode, limit.ResetRolling)
	// User 窗口：daily 用用户自己的重置配置；5h 模式取用户列。
	userResetMode := keysResetModeOrDefault(user.DailyResetMode, limit.ResetFixed)
	userResetTime := limit.NormalizeResetTime(usersStringOr(user.DailyResetTime, "00:00"))
	userLimit5hMode := keysResetModeOrDefault(user.Limit5hResetMode, limit.ResetRolling)

	// 起算点：Key 取 key/user 两个 reset 的较晚者；User 的 5h 另算（含 limit5hCostResetAt）。
	keyCostResetAt := limit.LaterReset(record.CostResetAt, user.CostResetAt)
	userCostResetAt := user.CostResetAt
	user5hCostResetAt := limit.LaterReset(user.CostResetAt, user.Limit5hCostResetAt)

	keyDailyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodDaily, now, keyResetTime, keyResetMode, location), keyCostResetAt)
	keyWeeklyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodWeekly, now, keyResetTime, keyResetMode, location), keyCostResetAt)
	keyMonthlyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodMonthly, now, keyResetTime, keyResetMode, location), keyCostResetAt)

	userDailyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodDaily, now, userResetTime, userResetMode, location), userCostResetAt)
	userWeeklyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodWeekly, now, userResetTime, userResetMode, location), userCostResetAt)
	userMonthlyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodMonthly, now, userResetTime, userResetMode, location), userCostResetAt)

	// 4 个窗口一次查完（Node 也是批量：sumKeyQuotaCostsById / sumUserQuotaCosts 各一次）。
	keyCosts, err := api.pools.SumLedgerQuotaCosts(ctx, store.LedgerEntityKey, record.Key, []store.TimeRange{
		{Start: limit.ClipStartByResetAt(now.Add(-5*time.Hour), keyCostResetAt), End: now},
		{Start: keyDailyStart, End: now},
		{Start: keyWeeklyStart, End: now},
		{Start: keyMonthlyStart, End: now},
	})
	if err != nil {
		return meQuotaBody{}, err
	}
	keyTotal, err := api.pools.SumLedgerTotalCost(ctx, store.LedgerEntityKey, record.Key, keyCostResetAt)
	if err != nil {
		return meQuotaBody{}, err
	}

	userCosts, err := api.pools.SumLedgerQuotaCosts(ctx, store.LedgerEntityUser, user.ID, []store.TimeRange{
		{Start: limit.ClipStartByResetAt(now.Add(-5*time.Hour), user5hCostResetAt), End: now},
		{Start: userDailyStart, End: now},
		{Start: userWeeklyStart, End: now},
		{Start: userMonthlyStart, End: now},
	})
	if err != nil {
		return meQuotaBody{}, err
	}
	userTotal, err := api.pools.SumLedgerTotalCost(ctx, store.LedgerEntityUser, user.ID, userCostResetAt)
	if err != nil {
		return meQuotaBody{}, err
	}

	// 5h 固定模式的读数取 Redis 运行态窗口（Node 的两条分支，service.ts:1264-1266）。
	keyCurrent5h, err := api.me5hCurrent(ctx, keyLimit5hMode, true, record.ID, keyCosts[0], now)
	if err != nil {
		return meQuotaBody{}, err
	}
	userCurrent5h, err := api.me5hCurrent(ctx, userLimit5hMode, false, user.ID, userCosts[0], now)
	if err != nil {
		return meQuotaBody{}, err
	}

	keyConcurrent, err := api.meSessionCount(ctx, true, record.ID)
	if err != nil {
		return meQuotaBody{}, err
	}
	userConcurrent, err := api.meSessionCount(ctx, false, user.ID)
	if err != nil {
		return meQuotaBody{}, err
	}

	effectiveConcurrency := limit.ResolveKeyConcurrentSessionLimit(
		keysOptionalInt(record.LimitConcurrentSessions), optionalInt64ToInt(user.LimitConcurrentSessions))

	return meQuotaBody{
		KeyLimit5hUSD:                nilIfNilNumber(record.Limit5hUSD),
		KeyLimitDailyUSD:             nilIfNilNumber(record.LimitDailyUSD),
		KeyLimitWeeklyUSD:            nilIfNilNumber(record.LimitWeeklyUSD),
		KeyLimitMonthlyUSD:           nilIfNilNumber(record.LimitMonthlyUSD),
		KeyLimitTotalUSD:             nilIfNilNumber(record.LimitTotalUSD),
		KeyLimitConcurrentSessions:   effectiveConcurrency,
		KeyCurrent5hUSD:              keyCurrent5h,
		KeyCurrentDailyUSD:           parseCostText(keyCosts[1]),
		KeyCurrentWeeklyUSD:          parseCostText(keyCosts[2]),
		KeyCurrentMonthlyUSD:         parseCostText(keyCosts[3]),
		KeyCurrentTotalUSD:           parseCostText(keyTotal),
		KeyCurrentConcurrentSessions: keyConcurrent,

		UserLimit5hUSD:           nilIfNilNumber(user.Limit5hUSD),
		UserLimitWeeklyUSD:       nilIfNilNumber(user.LimitWeeklyUSD),
		UserLimitMonthlyUSD:      nilIfNilNumber(user.LimitMonthlyUSD),
		UserLimitTotalUSD:        nilIfNilNumber(user.LimitTotalUSD),
		UserLimitConcurrentSess:  nilIfNilInt64(user.LimitConcurrentSessions),
		UserRPMLimit:             nilIfNilInt64(user.RPM),
		UserCurrent5hUSD:         userCurrent5h,
		UserCurrentDailyUSD:      parseCostText(userCosts[1]),
		UserCurrentWeeklyUSD:     parseCostText(userCosts[2]),
		UserCurrentMonthlyUSD:    parseCostText(userCosts[3]),
		UserCurrentTotalUSD:      parseCostText(userTotal),
		UserCurrentConcurrentSum: userConcurrent,

		UserLimitDailyUSD:  nilIfNilNumber(user.DailyQuota),
		UserExpiresAt:      store.JSONDatePtr(user.ExpiresAt),
		UserProviderGroup:  nilIfNilString(user.ProviderGroup),
		UserName:           user.Name,
		UserIsEnabled:      boolOrTrue(user.IsEnabled),
		KeyProviderGroup:   nilIfNilString(record.ProviderGroup),
		KeyName:            record.Name,
		KeyIsEnabled:       boolOrTrue(record.IsEnabled),
		UserAllowedModels:  nonNilStrings(user.AllowedModels),
		UserAllowedClients: nonNilStrings(user.AllowedClients),
		ExpiresAt:          store.JSONDatePtr(record.ExpiresAt),
		DailyResetMode:     string(keyResetMode),
		DailyResetTime:     keyResetTime,
	}, nil
}

// me5hCurrent 取 5h 当期读数：固定模式走 Redis 运行态窗口，滚动模式用账本聚合值。
func (api *meAPI) me5hCurrent(
	ctx context.Context,
	mode limit.ResetMode,
	keyScoped bool,
	id int64,
	rollingValue string,
	now time.Time,
) (float64, error) {
	if mode != limit.ResetFixed {
		return parseCostText(rollingValue), nil
	}
	if keyScoped {
		state, err := api.keyFixed5h.Fixed5hWindowState(ctx, id, now)
		if err != nil {
			return 0, err
		}
		return state.Current, nil
	}
	state, err := api.userFixed5h.UserFixed5hWindowState(ctx, id, now)
	if err != nil {
		return 0, err
	}
	return state.Current, nil
}

// meSessionCount 读活跃会话数：Node 把读失败吞成 0（展示语义，my-usage.ts:957-965），此处照抄并留痕。
func (api *meAPI) meSessionCount(ctx context.Context, keyScoped bool, id int64) (int, error) {
	if keyScoped {
		count, err := api.keySessions.KeySessionCount(ctx, id)
		if err != nil {
			api.logger.Warn("admin_me_key_session_count_failed", map[string]any{
				"keyId": id, "error": err.Error(),
			})
			return 0, nil
		}
		return count, nil
	}
	count, err := api.userSessions.UserSessionCount(ctx, id)
	if err != nil {
		api.logger.Warn("admin_me_user_session_count_failed", map[string]any{
			"userId": id, "error": err.Error(),
		})
		return 0, nil
	}
	return count, nil
}

// writeMeFailure 作答自服务的失败。
//
// 状态码取 Node 的 actionError 表（me/handlers.ts:176-196）：401（未认证）、404（未找到）、
// 403（含「权限」）、其余 400。守卫已在认证层拦下 401，故这里实际只有 404/400 两条路径。
func (api *meAPI) writeMeFailure(writer http.ResponseWriter, request *http.Request, err error) {
	status := http.StatusBadRequest
	detail := ""
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case err != nil && err.Error() != "":
		detail = err.Error()
	}
	if api.logger != nil {
		api.logger.Warn("admin_me_request_failed", map[string]any{
			"path":  request.URL.Path,
			"error": detail,
		})
	}
	api.problems.WriteProblem(writer, request, status, "", "")
}

// meSystemLocation 复刻 resolveSystemTimezone 的三级取值：system_settings.timezone →
// 环境变量 TZ（未设置取 `Asia/Shanghai`，见 config.EnvTimezoneDefault）→ UTC。
//
// 与 usersAPI.systemLocation（users.go:935）是同一条规则的两个实例：那边是 usersAPI 的方法，
// me 资源不是那种类型的接收者。二者的默认值与校验都落在 config.ResolveLocationFromEnv，
// 这里不再自读环境变量——自读会丢掉 TZ 的默认值，退化成「库里没设时区就按 UTC 算日界」。
func meSystemLocation(ctx context.Context, pools *store.Pools) (*time.Location, error) {
	raw, err := pools.AdminSystemTimezone(ctx)
	if err != nil {
		return nil, err
	}
	return config.ResolveLocationFromEnv(raw), nil
}

// parseCostText 把 numeric 文本转成展示用浮点。
//
// Node 侧是 Number(row.costUsd ?? 0) 且对非有限值归 0（my-usage.ts:583-585）。
func parseCostText(value string) float64 {
	parsed := usersNumber(json.Number(strings.TrimSpace(value)))
	if parsed == nil {
		return 0
	}
	return *parsed
}

// nonNilStrings 保证 JSON 里是数组而不是 null（Node 的 `?? []`）。
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// optionalInt64ToInt 把可空 int64 归一为 int（限额 0 = 无限制）。
func optionalInt64ToInt(value *int64) int {
	if value == nil {
		return 0
	}
	return int(*value)
}

// boolOrTrue 复刻 Node 的 `?? true` 兜底（isEnabled 为 null 时视为启用）。
func boolOrTrue(value *bool) bool {
	if value == nil {
		return true
	}
	return *value
}

// 虚拟管理员会话的字面值（Node auth.ts:193-221 的合成主体）。
const (
	virtualAdminUserName = "Admin Token"
	virtualAdminKeyName  = "ADMIN_TOKEN"
)
