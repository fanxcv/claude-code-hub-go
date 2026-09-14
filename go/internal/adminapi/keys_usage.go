package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 keys 资源的三条**读档**端点（A1-2 的剩余三条），与前两条 lane 的差别只有一处：
// 它们要读 Redis 运行态，因此单独成文，依赖 Deps.SessionCounts / Deps.Fixed5hWindows。
//
// 与 Node 的一一对应（src/app/api/v1/resources/keys/handlers.ts → src/actions/*.ts）：
//
//	GET /keys/{keyId}             getKey           → getKeyLimitUsage 外套 { id, limitUsage }
//	GET /keys/{keyId}/limit-usage getKeyLimitUsage → getKeyLimitUsage（正文即 data）
//	GET /keys/{keyId}/quota       getKeyQuotaUsage → getKeyQuotaUsage（src/actions/key-quota.ts）
//
// 三处容易抄错的细节（均已按 Node 落地）：
//
//  1. getKeyLimitUsage 的权限失败**没有** errorCode ⇒ 兜底码 key.action_failed；而 quota 的权限
//     失败带 PERMISSION_DENIED ⇒ 正文码就是 PERMISSION_DENIED。同是 403，错误码来源不同。
//  2. quota 的余额不足/未找到等错误在 keys 的状态码表里只有 404/403/400 三档（handlers.ts:309-334），
//     所以 INTERNAL_ERROR 是 **400** 而不是 500；显式指定码表外的状态会分叉。
//  3. costTotal.resetAt 取 Key 与 User 两个 costResetAt 的**较晚者**，而滚动窗口的起算点用的是同
//     一个值去收敛（clipStart）；5h 固定窗口则完全不看它（重置由窗口 TTL 决定）。
//
// 认证层的「会话缺失」分支（Node 的 getSession() 返回 null → "未登录" / UNAUTHORIZED）在 Go 侧
// 不可达：access 为 admin 时守卫已用 allowReadOnlyAccess=false 校验 can_login_web_ui 并按
// auth.invalid 401 拒绝（auth.go 与 auth-middleware.ts:89-99 同构），故此处不再复刻死分支。

// keysLimitAmount 复刻 `parseNumericLimit`（src/actions/key-quota.ts:94-98）：空为 null，
// 其余按数值保留（0 也是 0）。
func keysLimitAmount(value json.Number) any {
	return nilIfNilNumber(value)
}

// keysCostBucket 是 cost5h/costDaily/costWeekly/costMonthly/costTotal 的桶。
//
// resetAt 缺失时**不写该键**：Node 给的是 undefined，JSON.stringify 会整个删掉这个键，
// 而 Go 的 map 里放 nil 会写成 "resetAt": null —— 字段存在性与 null 在 A2 契约比对里是两回事。
func keysCostBucket(current string, limit any, resetAt *time.Time) map[string]any {
	bucket := map[string]any{"current": usersCostNumber(current), "limit": limit}
	if resetAt != nil {
		bucket["resetAt"] = store.JSONDate(*resetAt)
	}
	return bucket
}

// keysLimitUsageBody 是三个桶 + 会话数之外的两个共用装配步骤的结果。
type keysLimitUsageBody struct {
	row      *store.AdminKeyQuotaRow
	location *time.Location
	costs    keysLimitCosts
}

// keysLimitCosts 是五个周期 + 并发会话的实时读数。
type keysLimitCosts struct {
	cost5h                  string
	costDaily               string
	costWeekly              string
	costMonthly             string
	costTotal               string
	cost5hResetAt           *time.Time
	dailyResetInfo          limit.ResetInfo
	weeklyResetInfo         limit.ResetInfo
	monthlyResetInfo        limit.ResetInfo
	concurrentSessions      int
	limit5hResetMode        limit.ResetMode
	dailyResetMode          limit.ResetMode
	dailyResetTime          string
	effectiveKeyConcurrency int
	costResetAt             *time.Time
}

// keysUsageFailureKind 是读档失败的**种类**（而不是已经对好码的错误）。
//
// 为什么不直接返回 ActionError：同一次失败在两条端点上的正文码不同——getKeyLimitUsage 的
// not-found 无 errorCode（⇒ 兜底 key.not_found）而 quota 带 NOT_FOUND；内部错误同理
// （前者 400 key.action_failed、后者 400 INTERNAL_ERROR）。把映射留给各自的 handler，
// 才能不改共享代码地保持两份码表一致。
type keysUsageFailureKind int

const (
	// keysUsageNotFound 表示密钥不存在（Node 里就是那句无码的「密钥不存在」）。
	keysUsageNotFound keysUsageFailureKind = iota
	// keysUsageInternal 表示查询/聚合失败。
	keysUsageInternal
)

// keysUsageFailure 是读档失败的载体：种类 + 原始错误（原始错误只入日志，不进响应）。
type keysUsageFailure struct {
	kind keysUsageFailureKind
	err  error
}

// loadLimitUsage 取密钥行（含属主两列）并算出各周期读数。
//
// **调用方必须先判「不存在」再判权限**（Node 也是先查库再判权限：非属主查一条不存在的密钥得 404）。
// 返回错误时不给状态码：映射在各自的 handler 里（见 keysUsageFailureKind 的说明）。
func (api *keysAPI) loadLimitUsage(
	request *http.Request,
) (*keysLimitUsageBody, *keysUsageFailure) {
	keyID, ok := keyIDFromParams(request)
	if !ok {
		// 非数字 id 本不会命中路由（路径正则限定 [0-9]+），到这里只可能是路由被改坏。
		return nil, &keysUsageFailure{kind: keysUsageNotFound}
	}
	row, err := api.pools.FindAdminKeyQuotaRow(request.Context(), keyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &keysUsageFailure{kind: keysUsageNotFound}
		}
		return nil, &keysUsageFailure{kind: keysUsageInternal, err: err}
	}

	location, err := api.systemLocation(request.Context())
	if err != nil {
		return nil, &keysUsageFailure{kind: keysUsageInternal, err: err}
	}

	costs, err := api.loadLimitCosts(request.Context(), row, location)
	if err != nil {
		return nil, &keysUsageFailure{kind: keysUsageInternal, err: err}
	}
	return &keysLimitUsageBody{row: row, location: location, costs: costs}, nil
}

// loadLimitCosts 复刻两个 action 共用的那段计算（keys.ts:1046-1105 与 key-quota.ts:96-147）。
func (api *keysAPI) loadLimitCosts(
	ctx context.Context,
	row *store.AdminKeyQuotaRow,
	location *time.Location,
) (keysLimitCosts, error) {
	now := api.now()
	record := row.AdminKeyRecord

	limit5hResetMode := keysResetModeOrDefault(record.Limit5hResetMode, limit.ResetRolling)
	dailyResetMode := keysResetModeOrDefault(record.DailyResetMode, limit.ResetFixed)
	dailyResetTime := limit.NormalizeResetTime(usersStringOr(record.DailyResetTime, "00:00"))

	// 有效并发上限与重置标记：Key 优先、User 兜底（属主软删时用户侧为空，等价于只看 Key）。
	effectiveConcurrency := limit.ResolveKeyConcurrentSessionLimit(
		keysOptionalInt(record.LimitConcurrentSessions), keysOptionalInt(row.UserLimitConcurrentSessions))
	costResetAt := limit.LaterReset(record.CostResetAt, row.UserCostResetAt)

	costs := keysLimitCosts{
		cost5hResetAt:           nil,
		limit5hResetMode:        limit5hResetMode,
		dailyResetMode:          dailyResetMode,
		dailyResetTime:          dailyResetTime,
		effectiveKeyConcurrency: effectiveConcurrency,
		costResetAt:             costResetAt,
	}

	// 5h：fix 走 Redis 运行态窗口（重置时刻由 TTL 反推），rolling 走账本聚合。
	if limit5hResetMode == limit.ResetFixed {
		if api.fixed5h == nil {
			return costs, errors.New("adminapi: 5h 固定窗口读取器未装配")
		}
		state, err := api.fixed5h.Fixed5hWindowState(ctx, record.ID, now)
		if err != nil {
			return costs, err
		}
		costs.cost5h = limit.FormatCostText(state.Current)
		costs.cost5hResetAt = state.ResetAt
	} else {
		start := limit.ClipStartByResetAt(now.Add(-5*time.Hour), costResetAt)
		value, err := api.pools.SumLedgerCostInTimeRange(
			ctx, store.LedgerEntityKey, record.Key, start, now)
		if err != nil {
			return costs, err
		}
		costs.cost5h = value
	}

	dailyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodDaily, now, dailyResetTime, dailyResetMode, location), costResetAt)
	weeklyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodWeekly, now, dailyResetTime, dailyResetMode, location), costResetAt)
	monthlyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodMonthly, now, dailyResetTime, dailyResetMode, location), costResetAt)

	daily, err := api.pools.SumLedgerCostInTimeRange(
		ctx, store.LedgerEntityKey, record.Key, dailyStart, now)
	if err != nil {
		return costs, err
	}
	weekly, err := api.pools.SumLedgerCostInTimeRange(
		ctx, store.LedgerEntityKey, record.Key, weeklyStart, now)
	if err != nil {
		return costs, err
	}
	monthly, err := api.pools.SumLedgerCostInTimeRange(
		ctx, store.LedgerEntityKey, record.Key, monthlyStart, now)
	if err != nil {
		return costs, err
	}
	total, err := api.pools.SumLedgerTotalCost(ctx, store.LedgerEntityKey, record.Key, costResetAt)
	if err != nil {
		return costs, err
	}
	costs.costDaily = daily
	costs.costWeekly = weekly
	costs.costMonthly = monthly
	costs.costTotal = total

	costs.dailyResetInfo = limit.ResetInfoFor(
		limit.PeriodDaily, now, dailyResetTime, dailyResetMode, nil, location)
	costs.weeklyResetInfo = limit.ResetInfoFor(
		limit.PeriodWeekly, now, dailyResetTime, limit.ResetFixed, nil, location)
	costs.monthlyResetInfo = limit.ResetInfoFor(
		limit.PeriodMonthly, now, dailyResetTime, limit.ResetFixed, nil, location)

	// 并发会话数：Node 的 getKeySessionCount 把异常吞成 0（展示语义），此处照抄并留痕——
	// 读不到计数时宁可显示 0，也不该让整条读档变成错误。
	if api.sessions == nil {
		return costs, errors.New("adminapi: 会话计数读取器未装配")
	}
	count, err := api.sessions.KeySessionCount(ctx, record.ID)
	if err != nil {
		api.logger.Warn("admin_key_session_count_failed", map[string]any{
			"keyId": record.ID,
			"error": err.Error(),
		})
		count = 0
	}
	costs.concurrentSessions = count
	return costs, nil
}

// limitUsageBody 复刻 getKeyLimitUsage 的返回数据（Node 的 result.data）。
func (b keysLimitUsageBody) limitUsageBody() map[string]any {
	return map[string]any{
		"cost5h": keysCostBucket(
			b.costs.cost5h, keysMoneyValue(b.row.Limit5hUSD), b.costs.cost5hResetAt),
		"costDaily": keysCostBucket(
			b.costs.costDaily, keysMoneyValue(b.row.LimitDailyUSD), b.costs.dailyResetInfo.ResetAt),
		"costWeekly": keysCostBucket(
			b.costs.costWeekly, keysMoneyValue(b.row.LimitWeeklyUSD), b.costs.weeklyResetInfo.ResetAt),
		"costMonthly": keysCostBucket(
			b.costs.costMonthly, keysMoneyValue(b.row.LimitMonthlyUSD), b.costs.monthlyResetInfo.ResetAt),
		"costTotal": keysCostBucket(
			b.costs.costTotal, keysTotalMoneyValue(b.row.LimitTotalUSD), b.costs.costResetAt),
		"concurrentSessions": map[string]any{
			"current": b.costs.concurrentSessions,
			"limit":   b.costs.effectiveKeyConcurrency,
		},
	}
}

// requireKeyOwnerOrAdmin 复刻 getKeyLimitUsage / getKeyQuotaUsage 里的属主判定
// （keys.ts:1042-1044、key-quota.ts:76-80）：非管理员只能看自己的密钥。
//
// 两者的**错误码不同**（前者无码走兜底 key.action_failed，后者带 PERMISSION_DENIED），
// 因此码由调用方给，此处只判定。
func (api *keysAPI) requireKeyOwnerOrAdmin(
	request *http.Request,
	row *store.AdminKeyQuotaRow,
) bool {
	principal, _ := PrincipalFrom(request.Context())
	return principal.IsAdmin || principal.UserID == row.UserID
}

// writeKeysUsageFailure 把读档失败映射成本端点的错误码并作答。
//
// explicitCodes 为 false 时走 getKeyLimitUsage 的口径（无 errorCode ⇒ 兜底码 key.not_found /
// key.action_failed）；为 true 时走 quota 的口径（显式 NOT_FOUND / INTERNAL_ERROR）。
func (api *keysAPI) writeKeysUsageFailure(
	writer http.ResponseWriter,
	request *http.Request,
	failure *keysUsageFailure,
	explicitCodes bool,
) {
	if failure.err != nil {
		api.logger.Warn("admin_key_usage_failed", map[string]any{
			"path":  request.URL.Path,
			"error": failure.err.Error(),
		})
	}
	if failure.kind == keysUsageNotFound {
		if explicitCodes {
			api.problems.WriteActionError(writer, request, keysActionError("NOT_FOUND", 0, nil))
			return
		}
		api.problems.WriteActionError(writer, request,
			&ActionError{Resource: "key", Status: http.StatusNotFound})
		return
	}
	if explicitCodes {
		api.problems.WriteActionError(writer, request,
			keysActionError("INTERNAL_ERROR", 0, failure.err))
		return
	}
	// getKeyLimitUsage 的 catch 分支同样无码且不是「不存在」⇒ 兜底码 key.action_failed（400）。
	api.problems.WriteActionError(writer, request, &ActionError{Resource: "key", Err: failure.err})
}

// getKeyLimitUsage 复刻 getKeyLimitUsage（read 档）：正文即数据本体（actionJson 的透传）。
func (api *keysAPI) getKeyLimitUsage(writer http.ResponseWriter, request *http.Request) {
	body, failure := api.loadLimitUsage(request)
	if failure != nil {
		api.writeKeysUsageFailure(writer, request, failure, false)
		return
	}
	if !api.requireKeyOwnerOrAdmin(request, body.row) {
		// Node 无 errorCode ⇒ 403 + key.action_failed。
		api.problems.WriteActionError(writer, request,
			&ActionError{Resource: "key", Status: http.StatusForbidden})
		return
	}
	writeShellJSON(writer, http.StatusOK, body.limitUsageBody())
}

// getKey 复刻 getKey（admin 档）：把 limitUsage 套进 { id, limitUsage }。
func (api *keysAPI) getKey(writer http.ResponseWriter, request *http.Request) {
	body, failure := api.loadLimitUsage(request)
	if failure != nil {
		api.writeKeysUsageFailure(writer, request, failure, false)
		return
	}
	// 路由是管理档，守卫已保证 IsAdmin；仍按同一判定走一遍，与 Node 的 action 层一致。
	if !api.requireKeyOwnerOrAdmin(request, body.row) {
		api.problems.WriteActionError(writer, request,
			&ActionError{Resource: "key", Status: http.StatusForbidden})
		return
	}
	writeShellJSON(writer, http.StatusOK, map[string]any{
		"id":         body.row.ID,
		"limitUsage": body.limitUsageBody(),
	})
}

// getKeyQuotaUsage 复刻 getKeyQuotaUsage（admin 档，src/actions/key-quota.ts）。
//
// 与 limit-usage 的差别：items 是数组（含 type/mode/time），并多一个 currencyCode。
func (api *keysAPI) getKeyQuotaUsage(writer http.ResponseWriter, request *http.Request) {
	body, failure := api.loadLimitUsage(request)
	if failure != nil {
		// quota 的失败带显式码（NOT_FOUND / INTERNAL_ERROR），与 limit-usage 的兜底码不同。
		api.writeKeysUsageFailure(writer, request, failure, true)
		return
	}
	if !api.requireKeyOwnerOrAdmin(request, body.row) {
		api.problems.WriteActionError(writer, request,
			keysActionError("PERMISSION_DENIED", 0, nil))
		return
	}

	settings, err := api.pools.FindSystemSettings(request.Context())
	if err != nil {
		api.problems.WriteActionError(writer, request,
			keysActionError("INTERNAL_ERROR", 0, err))
		return
	}

	record := body.row.AdminKeyRecord
	costs := body.costs
	items := []map[string]any{
		{
			"type":    "limit5h",
			"current": usersCostNumber(costs.cost5h),
			"limit":   keysLimitAmount(record.Limit5hUSD),
			"mode":    string(costs.limit5hResetMode),
		},
		{
			"type":    "limitDaily",
			"current": usersCostNumber(costs.costDaily),
			"limit":   keysLimitAmount(record.LimitDailyUSD),
			"mode":    string(costs.dailyResetMode),
			"time":    costs.dailyResetTime,
		},
		{
			"type":    "limitWeekly",
			"current": usersCostNumber(costs.costWeekly),
			"limit":   keysLimitAmount(record.LimitWeeklyUSD),
		},
		{
			"type":    "limitMonthly",
			"current": usersCostNumber(costs.costMonthly),
			"limit":   keysLimitAmount(record.LimitMonthlyUSD),
		},
	}
	totalItem := map[string]any{
		"type":    "limitTotal",
		"current": usersCostNumber(costs.costTotal),
		"limit":   keysLimitAmount(record.LimitTotalUSD),
	}
	// resetAt 与 limit-usage 同规矩：没有就不写这个键（Node 给 undefined）。
	if costs.costResetAt != nil {
		totalItem["resetAt"] = store.JSONDate(*costs.costResetAt)
	}
	items = append(items, totalItem)

	// limitSessions 的 limit 与 costTotal 的判据不同：**有效上限为 0 时给 null**（不是 0）。
	sessionLimit := any(nil)
	if costs.effectiveKeyConcurrency > 0 {
		sessionLimit = costs.effectiveKeyConcurrency
	}
	items = append(items, map[string]any{
		"type":    "limitSessions",
		"current": costs.concurrentSessions,
		"limit":   sessionLimit,
	})

	writeShellJSON(writer, http.StatusOK, map[string]any{
		"keyName":      record.Name,
		"items":        items,
		"currencyCode": settings.CurrencyDisplay,
	})
}

// keysResetModeOrDefault 把可空重置模式映射成 limit.ResetMode，空值取该字段的 Node 默认值
// （keys 的 limit5hResetMode 默认 rolling、dailyResetMode 默认 fixed，见 transformers.ts:76-99）。
func keysResetModeOrDefault(value *string, fallback limit.ResetMode) limit.ResetMode {
	if value == nil || *value == "" {
		return fallback
	}
	if *value == string(limit.ResetRolling) {
		return limit.ResetRolling
	}
	return limit.ResetFixed
}

// keysOptionalInt 把可空 int32 归一为 int（缺省 0 = 无限制）。
func keysOptionalInt(value *int32) int {
	if value == nil {
		return 0
	}
	return int(*value)
}
