package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// 本文件是**排行榜**（Node 的 src/repository/leaderboard.ts 1246 行 + provider-cache-effectiveness.ts）
// 的 Go 落点：五个 scope（user / userCacheHitRate / provider / providerCacheHitRate / model）
// 乘五个周期（daily / weekly / monthly / allTime / custom）的九条 SQL，加上两族缓存系数查询。
//
// 与 Node 的三处**登记差异**：
//  1. 时间条件照 Node 的 buildDateCondition **在 SQL 里算**（`CURRENT_TIMESTAMP AT TIME ZONE tz` +
//     DATE_TRUNC），不把时区运算搬到 Go——搬过去就要重做 date-fns-tz 那套边界语义。
//  2. 缓存系数的窗口（Node 的 resolveLeaderboardWindow，用 date-fns 在 JS 侧算 [start, end]）
//     同样改在 SQL 里表达：`window_end > <start 表达式> AND window_end <= <end 表达式>`。
//     端点语义一致（都是「窗口结束时刻落在周期内」），只是算的位置不同。
//  3. 金额一律以 numeric 文本出库（`::text`）再由调用方 ParseFloat：Node 侧是 `parseFloat(string)`，
//     两者对同一个十进制串得到同一个 float64。排序仍在 numeric 上做（文本序会把 10 排在 9 前）。
//
// 计数列用 `::double precision` 而不是 bigint：Node 的 count(*) 走 JS number（float64），
// 出库类型必须同形，否则 JSON 里出现 int 与 float 的差别。

// AdminLeaderboardQuery 是一次排行榜查询的全部外部条件（对应 Node 的各 find* 入参）。
type AdminLeaderboardQuery struct {
	// Period 取 daily / weekly / monthly / allTime / custom。
	Period string
	// Timezone 是系统时区名（IANA）。
	Timezone string
	// StartDate / EndDate 仅在 Period=custom 时有意义（YYYY-MM-DD）。
	StartDate string
	EndDate   string
	// ProviderType 仅在两个 provider scope 上生效（空串表示不过滤）。
	ProviderType string
	// UserTags / UserGroups 仅在两个 user scope 上生效（各自 OR 逻辑，两者之间也是 OR）。
	UserTags   []string
	UserGroups []string
	// BillingModelSource 取 Node 的 `original` / `redirected`（决定模型字段的优先级与 basis 披露）。
	// 判据是 `== "original"`：其余取值（含 `redirected` 与空串）一律优先 model 列——与 Node 的
	// `billingModelSource === "original" ? originalModel : model` 同判。
	BillingModelSource string
}

// leaderboardArgs 是占位符分配器：按调用顺序编号，避免手写错 $n。
type leaderboardArgs struct{ values []any }

func (a *leaderboardArgs) add(value any) string {
	a.values = append(a.values, value)
	return fmt.Sprintf("$%d", len(a.values))
}

// leaderboardLedgerCondition 复刻 buildDateCondition。
//
// 三个日历周期共用同一个时区占位符（Node 侧也是同一个 timezone 变量）。custom 的左端与右端
// 各取一次时区位（右端表达式里还要 endDate 参数，位次不能复用）。
func leaderboardLedgerCondition(a *leaderboardArgs, query AdminLeaderboardQuery) string {
	switch query.Period {
	case "allTime":
		return "1=1"
	case "last24h":
		return "usage_ledger.created_at >= (CURRENT_TIMESTAMP - INTERVAL '24 hours')"
	case "custom":
		startTimezone := a.add(query.Timezone)
		start := "((" + a.add(query.StartDate) + "::date)::timestamp AT TIME ZONE " + startTimezone + ")"
		endTimezone := a.add(query.Timezone)
		endExclusive := "(((" + a.add(query.EndDate) +
			"::date) + INTERVAL '1 day') AT TIME ZONE " + endTimezone + ")"
		return "usage_ledger.created_at >= " + start + " AND usage_ledger.created_at < " + endExclusive
	case "weekly", "monthly", "daily":
		unit, interval := "day", "1 day"
		if query.Period == "weekly" {
			unit, interval = "week", "1 week"
		}
		if query.Period == "monthly" {
			unit, interval = "month", "1 month"
		}
		timezone := a.add(query.Timezone)
		startLocal := "DATE_TRUNC('" + unit + "', CURRENT_TIMESTAMP AT TIME ZONE " + timezone + ")"
		start := "(" + startLocal + " AT TIME ZONE " + timezone + ")"
		endExclusive := "((" + startLocal + " + INTERVAL '" + interval +
			"') AT TIME ZONE " + timezone + ")"
		return "usage_ledger.created_at >= " + start + " AND usage_ledger.created_at < " + endExclusive
	default:
		return "1=1"
	}
}

// leaderboardCoefficientWindow 是系数族查询的窗口条件。
//
// Node 的状态是 `window_end > start AND window_end <= end`（左开右闭），故右端不能直接复用
// 账本条件的右端（那是 `<`）。allTime 的左端是 Node 的 `new Date(0)`；last24h 是 now-24h。
func leaderboardCoefficientWindow(a *leaderboardArgs, query AdminLeaderboardQuery) string {
	var start string
	switch query.Period {
	case "custom":
		start = "((" + a.add(query.StartDate) + "::date)::timestamp AT TIME ZONE " +
			a.add(query.Timezone) + ")"
	case "last24h":
		start = "(CURRENT_TIMESTAMP - INTERVAL '24 hours')"
	case "allTime":
		start = "TIMESTAMPTZ '1970-01-01T00:00:00Z'"
	default:
		unit := "day"
		if query.Period == "weekly" {
			unit = "week"
		}
		if query.Period == "monthly" {
			unit = "month"
		}
		timezone := a.add(query.Timezone)
		start = "(DATE_TRUNC('" + unit + "', CURRENT_TIMESTAMP AT TIME ZONE " + timezone +
			") AT TIME ZONE " + timezone + ")"
	}

	var end string
	switch query.Period {
	case "custom":
		end = "(((" + a.add(query.EndDate) + "::date) + INTERVAL '1 day') AT TIME ZONE " +
			a.add(query.Timezone) + ")"
	case "weekly", "monthly", "daily":
		unit, interval := "day", "1 day"
		if query.Period == "weekly" {
			unit, interval = "week", "1 week"
		}
		if query.Period == "monthly" {
			unit, interval = "month", "1 month"
		}
		timezone := a.add(query.Timezone)
		startLocal := "DATE_TRUNC('" + unit + "', CURRENT_TIMESTAMP AT TIME ZONE " + timezone + ")"
		end = "((" + startLocal + " + INTERVAL '" + interval + "') AT TIME ZONE " + timezone + ")"
	default:
		end = "CURRENT_TIMESTAMP"
	}
	return "window_end > " + start + " AND window_end <= " + end
}

// leaderboardUserFilter 复刻 buildUserFilterCondition：标签与分组各自 OR，两者之间也是 OR。
//
// 标签用 jsonb 的 `?` 存在算子；分组按 Node 的正则把 provider_group 拆成数组再判包含。
func leaderboardUserFilter(a *leaderboardArgs, tags, groups []string) string {
	var tagParts []string
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			continue
		}
		tagParts = append(tagParts, "users.tags ? "+a.add(trimmed))
	}
	var groupParts []string
	for _, group := range groups {
		trimmed := strings.TrimSpace(group)
		if trimmed == "" {
			continue
		}
		groupParts = append(groupParts, a.add(trimmed)+
			` = ANY(regexp_split_to_array(coalesce(users.provider_group, ''), '\s*[,，\n\r]+\s*'))`)
	}
	switch {
	case len(tagParts) > 0 && len(groupParts) > 0:
		return "((" + strings.Join(tagParts, " OR ") + ") OR (" +
			strings.Join(groupParts, " OR ") + "))"
	case len(tagParts) > 0:
		return "(" + strings.Join(tagParts, " OR ") + ")"
	case len(groupParts) > 0:
		return "(" + strings.Join(groupParts, " OR ") + ")"
	default:
		return ""
	}
}

// 两条 join 片段照 Node 的 innerJoin（软删行不进榜）。
const (
	leaderboardUserJoin     = "INNER JOIN users ON usage_ledger.user_id = users.id AND users.deleted_at IS NULL"
	leaderboardProviderJoin = "INNER JOIN providers ON usage_ledger.final_provider_id = providers.id AND providers.deleted_at IS NULL"
)

// 共用聚合表达式（列名照 drizzle schema；口径照 leaderboard.ts 的 SQL 片段）。
const (
	lbTotalRequests = `count(*)::double precision`
	// lbTotalCostText / lbTotalCostNumeric 是同一个值两种用法：出库走文本（parseFloat 对齐），
	// 排序走 numeric（文本序会把 10 排在 9 前）。
	lbTotalCostText    = `COALESCE(sum(usage_ledger.cost_usd), 0)::text`
	lbTotalCostNumeric = `COALESCE(sum(usage_ledger.cost_usd), 0)`
	lbTotalTokens      = `COALESCE(sum(
		usage_ledger.input_tokens + usage_ledger.output_tokens
		+ COALESCE(usage_ledger.cache_creation_input_tokens, 0)
		+ COALESCE(usage_ledger.cache_read_input_tokens, 0)
	)::double precision, 0::double precision)`
	// 分子只数 success、分母数 countable（success + failure）——两者不是同一个条件，
	// 写成同一个会让比率恒为 1（这个错在集成用例里被抓到过）。
	lbSuccessRate = `count(CASE WHEN usage_ledger.success_rate_outcome = 'success' THEN 1 END)::double precision
		/ NULLIF(count(CASE WHEN usage_ledger.success_rate_outcome IN ('success', 'failure') THEN 1 END)::double precision, 0)`
	lbAvgTtftMS          = `COALESCE(avg(usage_ledger.ttfb_ms)::double precision, 0::double precision)`
	lbAvgTokensPerSecond = `COALESCE(avg(
		CASE
			WHEN usage_ledger.output_tokens > 0
				AND usage_ledger.duration_ms IS NOT NULL
				AND usage_ledger.first_byte_ms IS NOT NULL
				AND usage_ledger.first_byte_ms < usage_ledger.duration_ms
				AND (usage_ledger.duration_ms - usage_ledger.first_byte_ms) >= 100
			THEN (usage_ledger.output_tokens::double precision)
				/ ((usage_ledger.duration_ms - usage_ledger.first_byte_ms) / 1000.0)
		END
	)::double precision, 0::double precision)`
	lbTotalInputTokens = `COALESCE(sum(
		COALESCE(usage_ledger.input_tokens, 0)::double precision
		+ COALESCE(usage_ledger.cache_creation_input_tokens, 0)::double precision
		+ COALESCE(usage_ledger.cache_read_input_tokens, 0)::double precision
	)::double precision, 0::double precision)`
	lbCacheReadTokens   = `COALESCE(sum(COALESCE(usage_ledger.cache_read_input_tokens, 0))::double precision, 0::double precision)`
	lbCacheCreationCost = `COALESCE(sum(CASE
		WHEN COALESCE(usage_ledger.cache_creation_input_tokens, 0) > 0 THEN usage_ledger.cost_usd
		ELSE 0 END), 0)::text`
	lbCacheRequired = `(COALESCE(usage_ledger.cache_creation_input_tokens, 0) > 0
		OR COALESCE(usage_ledger.cache_read_input_tokens, 0) > 0)`
	lbCacheHitRate = `COALESCE(` + lbCacheReadTokens + ` / NULLIF(` + lbTotalInputTokens +
		`, 0::double precision), 0::double precision)`
)

// leaderboardModelField 按 billingModelSource 选模型列（original 优先 original_model）。
//
// withNullIf 对应 Node 的两处差别：user 面与 provider 面用带 NULLIF(TRIM(..)) 的版本
// （provider 面另在 Go 里 `if (!row.model) continue`）；model 面**不带 TRIM 也不带 NULLIF**
// （空模型的过滤在 Go 侧做）。
func leaderboardModelField(source string, withNullIf bool) string {
	raw := `COALESCE(usage_ledger.original_model, usage_ledger.model)`
	if source != "original" {
		raw = `COALESCE(usage_ledger.model, usage_ledger.original_model)`
	}
	if withNullIf {
		return `NULLIF(TRIM(` + raw + `), '')`
	}
	return raw
}

// leaderboardWhere 组装账本类查询的 WHERE 子句与参数。
func leaderboardWhere(query AdminLeaderboardQuery, extra ...string) (string, []any) {
	args := &leaderboardArgs{}
	conditions := []string{
		BillingCondition,
		leaderboardLedgerCondition(args, query),
	}
	if query.ProviderType != "" {
		conditions = append(conditions, "providers.provider_type = "+args.add(query.ProviderType))
	}
	if filter := leaderboardUserFilter(args, query.UserTags, query.UserGroups); filter != "" {
		conditions = append(conditions, filter)
	}
	conditions = append(conditions, extra...)
	return strings.Join(conditions, " AND "), args.values
}

// AdminLeaderboardUserRow 是 user scope 的一行。
type AdminLeaderboardUserRow struct {
	UserID        int64   `json:"userId"`
	UserName      string  `json:"userName"`
	TotalRequests float64 `json:"totalRequests"`
	TotalCostText string  `json:"totalCost"`
	TotalTokens   float64 `json:"totalTokens"`
}

// AdminLeaderboardUserModelRow 是 user scope 的按模型拆分行（model 可空）。
type AdminLeaderboardUserModelRow struct {
	UserID        int64   `json:"userId"`
	Model         *string `json:"model"`
	TotalRequests float64 `json:"totalRequests"`
	TotalCostText string  `json:"totalCost"`
	TotalTokens   float64 `json:"totalTokens"`
}

// AdminLeaderboardUserCacheRow 是 userCacheHitRate scope 的一行。
type AdminLeaderboardUserCacheRow struct {
	UserID            int64   `json:"userId"`
	UserName          string  `json:"userName"`
	TotalRequests     float64 `json:"totalRequests"`
	TotalCostText     string  `json:"totalCost"`
	CacheReadTokens   float64 `json:"cacheReadTokens"`
	CacheCreationCost string  `json:"cacheCreationCost"`
	TotalInputTokens  float64 `json:"totalInputTokens"`
	CacheHitRate      float64 `json:"cacheHitRate"`
}

// AdminLeaderboardUserCacheModelRow 是 userCacheHitRate scope 的按模型拆分行。
type AdminLeaderboardUserCacheModelRow struct {
	UserID           int64   `json:"userId"`
	Model            *string `json:"model"`
	TotalRequests    float64 `json:"totalRequests"`
	CacheReadTokens  float64 `json:"cacheReadTokens"`
	TotalInputTokens float64 `json:"totalInputTokens"`
	CacheHitRate     float64 `json:"cacheHitRate"`
}

// AdminLeaderboardProviderRow 是 provider scope 的一行。
type AdminLeaderboardProviderRow struct {
	ProviderID      int64    `json:"providerId"`
	ProviderName    string   `json:"providerName"`
	TotalRequests   float64  `json:"totalRequests"`
	TotalCostText   string   `json:"totalCost"`
	TotalTokens     float64  `json:"totalTokens"`
	SuccessRate     *float64 `json:"successRate"`
	AvgTtftMS       float64  `json:"avgTtftMs"`
	AvgTokensPerSec float64  `json:"avgTokensPerSecond"`
}

// AdminLeaderboardProviderModelRow 是 provider scope 的按模型拆分行。
type AdminLeaderboardProviderModelRow struct {
	ProviderID      int64    `json:"providerId"`
	Model           *string  `json:"model"`
	TotalRequests   float64  `json:"totalRequests"`
	TotalCostText   string   `json:"totalCost"`
	TotalTokens     float64  `json:"totalTokens"`
	SuccessRate     *float64 `json:"successRate"`
	AvgTtftMS       float64  `json:"avgTtftMs"`
	AvgTokensPerSec float64  `json:"avgTokensPerSecond"`
}

// AdminLeaderboardProviderCacheRow 是 providerCacheHitRate scope 的一行。
type AdminLeaderboardProviderCacheRow struct {
	ProviderID        int64   `json:"providerId"`
	ProviderName      string  `json:"providerName"`
	TotalRequests     float64 `json:"totalRequests"`
	TotalCostText     string  `json:"totalCost"`
	CacheReadTokens   float64 `json:"cacheReadTokens"`
	CacheCreationCost string  `json:"cacheCreationCost"`
	TotalInputTokens  float64 `json:"totalInputTokens"`
	CacheHitRate      float64 `json:"cacheHitRate"`
}

// AdminLeaderboardProviderCacheModelRow 是 providerCacheHitRate scope 的按模型拆分行。
type AdminLeaderboardProviderCacheModelRow struct {
	ProviderID       int64   `json:"providerId"`
	Model            *string `json:"model"`
	TotalRequests    float64 `json:"totalRequests"`
	CacheReadTokens  float64 `json:"cacheReadTokens"`
	TotalInputTokens float64 `json:"totalInputTokens"`
	CacheHitRate     float64 `json:"cacheHitRate"`
}

// AdminLeaderboardModelRow 是 model scope 的一行（空模型的过滤在 Go 侧做，故可空）。
type AdminLeaderboardModelRow struct {
	Model         *string  `json:"model"`
	TotalRequests float64  `json:"totalRequests"`
	TotalCostText string   `json:"totalCost"`
	TotalTokens   float64  `json:"totalTokens"`
	SuccessRate   *float64 `json:"successRate"`
}

// AdminUserLeaderboard 复刻 findLeaderboardWithTimezone 的主榜查询。
func (p *Pools) AdminUserLeaderboard(
	ctx context.Context,
	query AdminLeaderboardQuery,
) ([]AdminLeaderboardUserRow, error) {
	where, args := leaderboardWhere(query)
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT usage_ledger.user_id AS "userId", users.name AS "userName",
		       ` + lbTotalRequests + ` AS "totalRequests",
		       ` + lbTotalCostText + ` AS "totalCost",
		       ` + lbTotalTokens + ` AS "totalTokens"
		FROM usage_ledger ` + leaderboardUserJoin + `
		WHERE ` + where + `
		GROUP BY usage_ledger.user_id, users.name
		ORDER BY ` + lbTotalCostNumeric + ` DESC
	) t`
	return adminLeaderboardRows[AdminLeaderboardUserRow](ctx, p, sqlText, args)
}

// AdminUserModelLeaderboard 复刻 user scope 的按模型拆分查询。
func (p *Pools) AdminUserModelLeaderboard(
	ctx context.Context,
	query AdminLeaderboardQuery,
) ([]AdminLeaderboardUserModelRow, error) {
	modelField := leaderboardModelField(query.BillingModelSource, true)
	where, args := leaderboardWhere(query)
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT usage_ledger.user_id AS "userId", ` + modelField + ` AS "model",
		       ` + lbTotalRequests + ` AS "totalRequests",
		       ` + lbTotalCostText + ` AS "totalCost",
		       ` + lbTotalTokens + ` AS "totalTokens"
		FROM usage_ledger ` + leaderboardUserJoin + `
		WHERE ` + where + `
		GROUP BY usage_ledger.user_id, ` + modelField + `
		ORDER BY ` + lbTotalCostNumeric + ` DESC
	) t`
	return adminLeaderboardRows[AdminLeaderboardUserModelRow](ctx, p, sqlText, args)
}

// AdminUserCacheLeaderboard 复刻 findUserCacheHitRateLeaderboardWithTimezone 的主榜查询。
func (p *Pools) AdminUserCacheLeaderboard(
	ctx context.Context,
	query AdminLeaderboardQuery,
) ([]AdminLeaderboardUserCacheRow, error) {
	where, args := leaderboardWhere(query, lbCacheRequired)
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT usage_ledger.user_id AS "userId", users.name AS "userName",
		       ` + lbTotalRequests + ` AS "totalRequests",
		       ` + lbTotalCostText + ` AS "totalCost",
		       ` + lbCacheReadTokens + ` AS "cacheReadTokens",
		       ` + lbCacheCreationCost + ` AS "cacheCreationCost",
		       ` + lbTotalInputTokens + ` AS "totalInputTokens",
		       ` + lbCacheHitRate + ` AS "cacheHitRate"
		FROM usage_ledger ` + leaderboardUserJoin + `
		WHERE ` + where + `
		GROUP BY usage_ledger.user_id, users.name
		ORDER BY ` + lbCacheHitRate + ` DESC, count(*) DESC,
		         users.name ASC, usage_ledger.user_id ASC
	) t`
	return adminLeaderboardRows[AdminLeaderboardUserCacheRow](ctx, p, sqlText, args)
}

// AdminUserCacheModelLeaderboard 复刻 userCacheHitRate scope 的按模型拆分查询。
func (p *Pools) AdminUserCacheModelLeaderboard(
	ctx context.Context,
	query AdminLeaderboardQuery,
) ([]AdminLeaderboardUserCacheModelRow, error) {
	modelField := leaderboardModelField(query.BillingModelSource, true)
	where, args := leaderboardWhere(query, lbCacheRequired)
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT usage_ledger.user_id AS "userId", ` + modelField + ` AS "model",
		       ` + lbTotalRequests + ` AS "totalRequests",
		       ` + lbCacheReadTokens + ` AS "cacheReadTokens",
		       ` + lbTotalInputTokens + ` AS "totalInputTokens",
		       ` + lbCacheHitRate + ` AS "cacheHitRate"
		FROM usage_ledger ` + leaderboardUserJoin + `
		WHERE ` + where + `
		GROUP BY usage_ledger.user_id, ` + modelField + `
		ORDER BY ` + lbCacheHitRate + ` DESC, count(*) DESC, ` + modelField + ` ASC
	) t`
	return adminLeaderboardRows[AdminLeaderboardUserCacheModelRow](ctx, p, sqlText, args)
}

// AdminProviderLeaderboard 复刻 findProviderLeaderboardWithTimezone 的主榜查询。
//
// `cacheCoefficientBp` 不在这条 SQL 里（Node 也是查完再合并），由调用方用系数查询补。
func (p *Pools) AdminProviderLeaderboard(
	ctx context.Context,
	query AdminLeaderboardQuery,
) ([]AdminLeaderboardProviderRow, error) {
	where, args := leaderboardWhere(query)
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT usage_ledger.final_provider_id AS "providerId", providers.name AS "providerName",
		       ` + lbTotalRequests + ` AS "totalRequests",
		       ` + lbTotalCostText + ` AS "totalCost",
		       ` + lbTotalTokens + ` AS "totalTokens",
		       ` + lbSuccessRate + ` AS "successRate",
		       ` + lbAvgTtftMS + ` AS "avgTtftMs",
		       ` + lbAvgTokensPerSecond + ` AS "avgTokensPerSecond"
		FROM usage_ledger ` + leaderboardProviderJoin + `
		WHERE ` + where + `
		GROUP BY usage_ledger.final_provider_id, providers.name
		ORDER BY ` + lbTotalCostNumeric + ` DESC
	) t`
	return adminLeaderboardRows[AdminLeaderboardProviderRow](ctx, p, sqlText, args)
}

// AdminProviderModelLeaderboard 复刻 provider scope 的按模型拆分查询。
func (p *Pools) AdminProviderModelLeaderboard(
	ctx context.Context,
	query AdminLeaderboardQuery,
) ([]AdminLeaderboardProviderModelRow, error) {
	modelField := leaderboardModelField(query.BillingModelSource, true)
	where, args := leaderboardWhere(query)
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT usage_ledger.final_provider_id AS "providerId", ` + modelField + ` AS "model",
		       ` + lbTotalRequests + ` AS "totalRequests",
		       ` + lbTotalCostText + ` AS "totalCost",
		       ` + lbTotalTokens + ` AS "totalTokens",
		       ` + lbSuccessRate + ` AS "successRate",
		       ` + lbAvgTtftMS + ` AS "avgTtftMs",
		       ` + lbAvgTokensPerSecond + ` AS "avgTokensPerSecond"
		FROM usage_ledger ` + leaderboardProviderJoin + `
		WHERE ` + where + `
		GROUP BY usage_ledger.final_provider_id, ` + modelField + `
		ORDER BY ` + lbTotalCostNumeric + ` DESC, count(*) DESC
	) t`
	return adminLeaderboardRows[AdminLeaderboardProviderModelRow](ctx, p, sqlText, args)
}

// AdminProviderCacheLeaderboard 复刻 findProviderCacheHitRateLeaderboardWithTimezone 的主榜查询。
func (p *Pools) AdminProviderCacheLeaderboard(
	ctx context.Context,
	query AdminLeaderboardQuery,
) ([]AdminLeaderboardProviderCacheRow, error) {
	where, args := leaderboardWhere(query, lbCacheRequired)
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT usage_ledger.final_provider_id AS "providerId", providers.name AS "providerName",
		       ` + lbTotalRequests + ` AS "totalRequests",
		       ` + lbTotalCostText + ` AS "totalCost",
		       ` + lbCacheReadTokens + ` AS "cacheReadTokens",
		       ` + lbCacheCreationCost + ` AS "cacheCreationCost",
		       ` + lbTotalInputTokens + ` AS "totalInputTokens",
		       ` + lbCacheHitRate + ` AS "cacheHitRate"
		FROM usage_ledger ` + leaderboardProviderJoin + `
		WHERE ` + where + `
		GROUP BY usage_ledger.final_provider_id, providers.name
		ORDER BY ` + lbCacheHitRate + ` DESC, count(*) DESC
	) t`
	return adminLeaderboardRows[AdminLeaderboardProviderCacheRow](ctx, p, sqlText, args)
}

// AdminProviderCacheModelLeaderboard 复刻 providerCacheHitRate scope 的按模型拆分查询。
func (p *Pools) AdminProviderCacheModelLeaderboard(
	ctx context.Context,
	query AdminLeaderboardQuery,
) ([]AdminLeaderboardProviderCacheModelRow, error) {
	modelField := leaderboardModelField(query.BillingModelSource, true)
	where, args := leaderboardWhere(query, lbCacheRequired)
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT usage_ledger.final_provider_id AS "providerId", ` + modelField + ` AS "model",
		       ` + lbTotalRequests + ` AS "totalRequests",
		       ` + lbCacheReadTokens + ` AS "cacheReadTokens",
		       ` + lbTotalInputTokens + ` AS "totalInputTokens",
		       ` + lbCacheHitRate + ` AS "cacheHitRate"
		FROM usage_ledger ` + leaderboardProviderJoin + `
		WHERE ` + where + `
		GROUP BY usage_ledger.final_provider_id, ` + modelField + `
		ORDER BY ` + lbCacheHitRate + ` DESC, count(*) DESC
	) t`
	return adminLeaderboardRows[AdminLeaderboardProviderCacheModelRow](ctx, p, sqlText, args)
}

// AdminModelLeaderboard 复刻 findModelLeaderboardWithTimezone。
//
// 这条**不 join users/providers**，模型字段也不带 TRIM/NULLIF：空模型行的过滤在 Go 侧做
// （Node 的 `filter(entry => entry.model !== null && entry.model !== "")`）。
func (p *Pools) AdminModelLeaderboard(
	ctx context.Context,
	query AdminLeaderboardQuery,
) ([]AdminLeaderboardModelRow, error) {
	modelField := leaderboardModelField(query.BillingModelSource, false)
	where, args := leaderboardWhere(query)
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT ` + modelField + ` AS "model",
		       ` + lbTotalRequests + ` AS "totalRequests",
		       ` + lbTotalCostText + ` AS "totalCost",
		       ` + lbTotalTokens + ` AS "totalTokens",
		       ` + lbSuccessRate + ` AS "successRate"
		FROM usage_ledger
		WHERE ` + where + `
		GROUP BY ` + modelField + `
		ORDER BY ` + lbTotalCostNumeric + ` DESC, count(*) DESC
	) t`
	return adminLeaderboardRows[AdminLeaderboardModelRow](ctx, p, sqlText, args)
}

// AdminCacheCoefficientRow 是缓存系数汇总的一行（四列大整数以文本出库，调用方走 big.Int）。
//
// 两族系数查询共用这一个行型：provider 维度那族把 model 取成空串。
type AdminCacheCoefficientRow struct {
	ProviderID              int64  `json:"providerId"`
	Model                   string `json:"model"`
	SampleCountText         string `json:"sampleCount"`
	EligibleCountText       string `json:"eligibleCount"`
	TheoreticalCacheTokens  string `json:"theoreticalCacheTokens"`
	ObservedCacheReadTokens string `json:"observedCacheReadTokens"`
}

// CacheCoefficient 是 provider（或 provider+model）维度的缓存系数。
type CacheCoefficient struct {
	ProviderID    int64
	Model         string
	CoefficientBP int64
	SampleCount   int64
}

// AdminProviderCacheCoefficients 复刻 getProviderCacheCoefficients：按 provider 汇总窗口内数据。
func (p *Pools) AdminProviderCacheCoefficients(
	ctx context.Context,
	query AdminLeaderboardQuery,
) (map[int64]CacheCoefficient, error) {
	args := &leaderboardArgs{}
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT provider_id AS "providerId",
		       '' AS "model",
		       COALESCE(sum(sample_count), 0)::text AS "sampleCount",
		       COALESCE(sum(eligible_count), 0)::text AS "eligibleCount",
		       COALESCE(sum(theoretical_cache_tokens), 0)::text AS "theoreticalCacheTokens",
		       COALESCE(sum(observed_cache_read_tokens), 0)::text AS "observedCacheReadTokens"
		FROM provider_cache_effectiveness
		WHERE ` + leaderboardCoefficientWindow(args, query) + `
		GROUP BY provider_id
	) t`
	rows, err := adminLeaderboardRows[AdminCacheCoefficientRow](ctx, p, sqlText, args.values)
	if err != nil {
		return nil, err
	}
	coefficients := make(map[int64]CacheCoefficient, len(rows))
	for _, row := range rows {
		coefficients[row.ProviderID] = cacheCoefficientOf(row)
	}
	return coefficients, nil
}

// AdminProviderModelCacheCoefficients 复刻 getProviderModelCacheCoefficients：再加一层 model。
func (p *Pools) AdminProviderModelCacheCoefficients(
	ctx context.Context,
	query AdminLeaderboardQuery,
) (map[int64]map[string]CacheCoefficient, error) {
	args := &leaderboardArgs{}
	sqlText := `SELECT row_to_json(t)::text FROM (
		SELECT provider_id AS "providerId",
		       TRIM(model) AS "model",
		       COALESCE(sum(sample_count), 0)::text AS "sampleCount",
		       COALESCE(sum(eligible_count), 0)::text AS "eligibleCount",
		       COALESCE(sum(theoretical_cache_tokens), 0)::text AS "theoreticalCacheTokens",
		       COALESCE(sum(observed_cache_read_tokens), 0)::text AS "observedCacheReadTokens"
		FROM provider_cache_effectiveness
		WHERE ` + leaderboardCoefficientWindow(args, query) + ` AND TRIM(model) <> ''
		GROUP BY provider_id, TRIM(model)
	) t`
	rows, err := adminLeaderboardRows[AdminCacheCoefficientRow](ctx, p, sqlText, args.values)
	if err != nil {
		return nil, err
	}
	coefficients := make(map[int64]map[string]CacheCoefficient, len(rows))
	for _, row := range rows {
		models, ok := coefficients[row.ProviderID]
		if !ok {
			models = map[string]CacheCoefficient{}
			coefficients[row.ProviderID] = models
		}
		models[row.Model] = cacheCoefficientOf(row)
	}
	return coefficients, nil
}

// cacheCoefficientOf 把四列汇总值代进定点公式（复刻 computeCoefficientBp）。
func cacheCoefficientOf(row AdminCacheCoefficientRow) CacheCoefficient {
	sample := leaderboardBigInt(row.SampleCountText)
	return CacheCoefficient{
		ProviderID: row.ProviderID,
		Model:      row.Model,
		CoefficientBP: computeCacheCoefficientBP(
			sample,
			leaderboardBigInt(row.EligibleCountText),
			leaderboardBigInt(row.TheoreticalCacheTokens),
			leaderboardBigInt(row.ObservedCacheReadTokens),
		),
		SampleCount: sample.Int64(),
	}
}

var leaderboardBigZero = big.NewInt(0)
var leaderboardBPScale = big.NewInt(10000)

// leaderboardBigInt 解析 numeric 文本（解析失败按 0，与 Node 的兜底同义）。
func leaderboardBigInt(text string) *big.Int {
	value, ok := new(big.Int).SetString(strings.TrimSpace(text), 10)
	if !ok {
		return big.NewInt(0)
	}
	return value
}

// computeCacheCoefficientBP 复刻 computeCoefficientBp：clamp 后的 raw 乘可观测率与样本量分档。
//
// 全 BigInt 整数运算（与 provider-cache-effectiveness.ts 逐行对应）：
//
//	rawBp        = clamp(observed * 10000 / theoretical, 0, 10000)
//	observableBp = eligible * 10000 / sample
//	confidenceBp = observableBp * 样本量分档 / 10000
//	结果          = rawBp * confidenceBp / 10000
func computeCacheCoefficientBP(sample, eligible, theoretical, observed *big.Int) int64 {
	rawBP := new(big.Int)
	if theoretical.Cmp(leaderboardBigZero) > 0 {
		rawBP.Mul(observed, leaderboardBPScale)
		rawBP.Div(rawBP, theoretical)
	}
	if rawBP.Cmp(leaderboardBPScale) > 0 {
		rawBP.Set(leaderboardBPScale)
	}
	if rawBP.Cmp(leaderboardBigZero) < 0 {
		rawBP.SetInt64(0)
	}

	// 样本量分档：>=100 满分；>=30 六折；>=5 三折；否则一折。
	sampleFactorBP := big.NewInt(1000)
	switch {
	case eligible.Cmp(big.NewInt(100)) >= 0:
		sampleFactorBP = big.NewInt(10000)
	case eligible.Cmp(big.NewInt(30)) >= 0:
		sampleFactorBP = big.NewInt(6000)
	case eligible.Cmp(big.NewInt(5)) >= 0:
		sampleFactorBP = big.NewInt(3000)
	}

	observableBP := new(big.Int)
	if sample.Cmp(leaderboardBigZero) > 0 {
		observableBP.Mul(eligible, leaderboardBPScale)
		observableBP.Div(observableBP, sample)
	}
	confidenceBP := new(big.Int).Div(
		new(big.Int).Mul(observableBP, sampleFactorBP), leaderboardBPScale)
	return new(big.Int).Div(
		new(big.Int).Mul(rawBP, confidenceBP), leaderboardBPScale).Int64()
}

// adminLeaderboardRows 是「row_to_json 文本行 -> 结构体」的读法（与其它 store 文件同形）。
func adminLeaderboardRows[T any](
	ctx context.Context,
	pools *Pools,
	query string,
	args []any,
) ([]T, error) {
	pool, err := pools.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询排行榜失败: %w", err)
	}
	defer rows.Close()

	results := make([]T, 0, 16)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取排行榜行失败: %w", err)
		}
		var item T
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			return nil, fmt.Errorf("store: 解析排行榜行失败: %w", err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历排行榜行失败: %w", err)
	}
	return results, nil
}
