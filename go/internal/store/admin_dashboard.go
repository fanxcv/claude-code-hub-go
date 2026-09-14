package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本文件是 dashboard 资源族的读面（/api/v1/dashboard/*）在 store 层的部分：
//
//	AdminDashboardOverview   复刻 src/repository/overview.ts（getOverviewMetricsWithComparison）
//	AdminActiveUserVersions  复刻 src/repository/client-versions.ts（getActiveUserVersions）
//	AdminRateLimitEventRows  复刻 src/repository/statistics.ts（getRateLimitEventStats 的取数部分）
//
// provider-slots 与 client-versions 的供应商/统计聚合分别复用 AdminListProviders 与本文件的
// 用户版本查询，不在这里另写一份。
//
// 分道：一律走 control（管理面只读面的既有纪律，不占数据面连接）。

// AdminDashboardOverview 是概览指标的原始读数（Node 的 OverviewMetricsWithComparison）。
//
// 成本与均值以文本/浮点原样取回，换算（保留 6 位、四舍五入到毫秒）由 adminapi 层做——
// 存储层不替调用方决定展示精度。
type AdminDashboardOverview struct {
	TodayRequests                    int64
	TodayCost                        float64
	TodayAvgDurationMs               float64
	TodayErrorCount                  int64
	YesterdaySamePeriodRequests      int64
	YesterdaySamePeriodCost          float64
	YesterdaySamePeriodAvgDurationMs float64
	RecentMinuteRequests             int64
}

// AdminDashboardOverview 复刻 getOverviewMetricsWithComparison 的三段聚合。
//
// 口径细节（逐条照抄）：
//   - 时间窗用**服务端时区的当地日界**：todayStart = DATE_TRUNC('day', now AT TIME ZONE tz)
//     再 AT TIME ZONE tz 还原成 timestamptz；上界是次日零点（**排他**）。
//   - 昨日同时段 = 昨日零点 + (now - 今日零点)（在「当地无时区」空间里做加法，与 Node 一致）。
//   - 统计口径是 LEDGER_BILLING_CONDITION（store.BillingCondition），并排除 blocked_by 行。
//   - 错误数用 `count(*) FILTER (WHERE NOT is_success)`；错误率由上层算。
//   - RPM 是**绝对一分钟**（CURRENT_TIMESTAMP - 1 minute），与时区无关。
//   - userID 非 nil 时只统计该用户（Node 的 canViewGlobalData 分支）。
func (p *Pools) AdminDashboardOverview(
	ctx context.Context,
	userID *int64,
	timezone string,
) (AdminDashboardOverview, error) {
	pool, err := p.Control()
	if err != nil {
		return AdminDashboardOverview{}, err
	}
	query := `
		WITH bounds AS (
			SELECT
				(DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1) AT TIME ZONE $1) AS today_start,
				((DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1) + INTERVAL '1 day') AT TIME ZONE $1)
					AS tomorrow_start,
				((DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1) - INTERVAL '1 day') AT TIME ZONE $1)
					AS yesterday_start,
				(((DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1) - INTERVAL '1 day')
					+ (CURRENT_TIMESTAMP AT TIME ZONE $1 - DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1)))
					AT TIME ZONE $1) AS yesterday_end
		)
		SELECT
			(SELECT count(*) FROM usage_ledger, bounds
				WHERE ` + BillingCondition + ` AND ($2::bigint IS NULL OR user_id = $2)
				  AND created_at >= bounds.today_start AND created_at < bounds.tomorrow_start),
			COALESCE((SELECT sum(cost_usd) FROM usage_ledger, bounds
				WHERE ` + BillingCondition + ` AND ($2::bigint IS NULL OR user_id = $2)
				  AND created_at >= bounds.today_start AND created_at < bounds.tomorrow_start), 0)::text,
			COALESCE((SELECT avg(duration_ms) FROM usage_ledger, bounds
				WHERE ` + BillingCondition + ` AND ($2::bigint IS NULL OR user_id = $2)
				  AND created_at >= bounds.today_start AND created_at < bounds.tomorrow_start), 0)::float8,
			(SELECT count(*) FILTER (WHERE NOT is_success) FROM usage_ledger, bounds
				WHERE ` + BillingCondition + ` AND ($2::bigint IS NULL OR user_id = $2)
				  AND created_at >= bounds.today_start AND created_at < bounds.tomorrow_start),
			(SELECT count(*) FROM usage_ledger, bounds
				WHERE ` + BillingCondition + ` AND ($2::bigint IS NULL OR user_id = $2)
				  AND created_at >= bounds.yesterday_start AND created_at < bounds.yesterday_end),
			COALESCE((SELECT sum(cost_usd) FROM usage_ledger, bounds
				WHERE ` + BillingCondition + ` AND ($2::bigint IS NULL OR user_id = $2)
				  AND created_at >= bounds.yesterday_start AND created_at < bounds.yesterday_end), 0)::text,
			COALESCE((SELECT avg(duration_ms) FROM usage_ledger, bounds
				WHERE ` + BillingCondition + ` AND ($2::bigint IS NULL OR user_id = $2)
				  AND created_at >= bounds.yesterday_start AND created_at < bounds.yesterday_end), 0)::float8,
			(SELECT count(*) FROM usage_ledger
				WHERE ` + BillingCondition + ` AND ($2::bigint IS NULL OR user_id = $2)
				  AND created_at >= CURRENT_TIMESTAMP - INTERVAL '1 minute')`

	var overview AdminDashboardOverview
	var todayCostText, yesterdayCostText string
	row := pool.QueryRow(ctx, query, timezone, userID)
	if err := row.Scan(
		&overview.TodayRequests,
		&todayCostText,
		&overview.TodayAvgDurationMs,
		&overview.TodayErrorCount,
		&overview.YesterdaySamePeriodRequests,
		&yesterdayCostText,
		&overview.YesterdaySamePeriodAvgDurationMs,
		&overview.RecentMinuteRequests,
	); err != nil {
		return AdminDashboardOverview{}, fmt.Errorf("store: 概览聚合查询失败: %w", err)
	}
	overview.TodayCost = parseNumericText(todayCostText)
	overview.YesterdaySamePeriodCost = parseNumericText(yesterdayCostText)
	return overview, nil
}

// AdminActiveUserVersion 是活跃用户版本分布的一行（Node 的 RawUserVersion）。
//
// LastSeen 是 ISO 8601 串（Node 侧是 Date，序列化成 toISOString() 的同形文本）。
type AdminActiveUserVersion struct {
	UserID    int64  `json:"userId"`
	Username  string `json:"username"`
	UserAgent string `json:"userAgent"`
	LastSeen  string `json:"lastSeen"`
}

// AdminActiveUserVersions 复刻 getActiveUserVersions(days)。
//
// 与 store.ActiveUserAgents 的差别（两份查询都留着，各有各的用途）：那份只取
// (user_id, user_agent) 去重供 GA 判定用；这份要用户名与最后活动时刻供 dashboard 列表展示，
// 故按 Node 的原式 group by (user_id, users.name, user_agent) 并取 MAX(created_at)。
//
// 两处照抄 Node：
//   - 用户在 users 表里被软删或不存在时，名称落回 `User {id}`（Node 的 `row.username || ...`，
//     空串也算「没有名字」）。
//   - 排序是 `MAX(created_at) DESC`，没有二级键——同刻并列的行序由 PG 决定。
func (p *Pools) AdminActiveUserVersions(
	ctx context.Context,
	days int,
) ([]AdminActiveUserVersion, error) {
	if days <= 0 {
		days = 7
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	query := `SELECT row_to_json(t)::text FROM (
		SELECT message_request.user_id AS "userId",
		       COALESCE(NULLIF(users.name, ''), 'User ' || message_request.user_id) AS "username",
		       message_request.user_agent AS "userAgent",
		       to_char(MAX(message_request.created_at) AT TIME ZONE 'UTC',
		               'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "lastSeen"
		FROM message_request
		LEFT JOIN users ON message_request.user_id = users.id AND users.deleted_at IS NULL
		WHERE message_request.created_at >= $1 AND message_request.user_agent IS NOT NULL
		GROUP BY message_request.user_id, users.name, message_request.user_agent
		ORDER BY MAX(message_request.created_at) DESC NULLS LAST
	) t`

	rows, err := pool.Query(ctx, query, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: 查询活跃用户版本失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminActiveUserVersion, 0, 16)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取活跃用户版本行失败: %w", err)
		}
		var row AdminActiveUserVersion
		if err := json.Unmarshal([]byte(payload), &row); err != nil {
			return nil, fmt.Errorf("store: 活跃用户版本行反序列化失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历活跃用户版本行失败: %w", err)
	}
	return results, nil
}

// AdminRateLimitEventRow 是限流事件扫描的一行（Node getRateLimitEventStats 的 select 投影）。
//
// Hour 是「按服务端时区的当地整点」的 ISO 形文本（见下方注释里的取舍说明）。
type AdminRateLimitEventRow struct {
	ID           int64   `json:"id"`
	UserID       int64   `json:"userId"`
	ProviderID   *int64  `json:"providerId"`
	ErrorMessage string  `json:"errorMessage"`
	Hour         *string `json:"hour"`
}

// AdminRateLimitEventFilters 复刻 RateLimitEventFilters 中路由能传进来的部分。
type AdminRateLimitEventFilters struct {
	UserID     *int64
	ProviderID *int64
	KeyString  *string
	StartTime  *time.Time
	EndTime    *time.Time
}

// AdminRateLimitEventRows 复刻 getRateLimitEventStats 的取数（统计聚合在 adminapi 层做）。
//
// 之所以把聚合留在上层：Node 的这条统计是**在 JS 里**解析 error_message 里的
// `rate_limit_metadata: {...}` 再分桶的（不是 SQL 聚合），逐条搬进 Go 才能保证
// 「total_events 是扫描行数而不是解析成功数」这类细节一致。
//
// hour 的格式取舍：Node 取 `DATE_TRUNC('hour', created_at AT TIME ZONE tz)`，那是
// **timestamp without time zone**（当地墙上时间），node-postgres 把它按**进程本地时区**解成
// Date，toISOString() 再转一次。容器里 TZ=UTC 是默认值，此时输出就是「当地整点当作 UTC」，
// 本查询的 to_char 与之逐字一致；若部署把 TZ 设成别的值，Node 的输出会再平移一次，Go 侧不跟随
// （登记为允许差异，因为「跟随进程 TZ」不是可移植的语义）。
func (p *Pools) AdminRateLimitEventRows(
	ctx context.Context,
	filters AdminRateLimitEventFilters,
	timezone string,
) ([]AdminRateLimitEventRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}

	conditions := []string{
		`error_message LIKE '%rate_limit_metadata%'`,
		`deleted_at IS NULL`,
	}
	args := []any{timezone}
	add := func(expression string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(expression, len(args)))
	}
	if filters.UserID != nil {
		add("user_id = $%d", *filters.UserID)
	}
	if filters.ProviderID != nil {
		add("provider_id = $%d", *filters.ProviderID)
	}
	if filters.KeyString != nil {
		add("key = $%d", *filters.KeyString)
	}
	if filters.StartTime != nil {
		add("created_at >= $%d::timestamptz", *filters.StartTime)
	}
	if filters.EndTime != nil {
		add("created_at <= $%d::timestamptz", *filters.EndTime)
	}

	query := `SELECT row_to_json(t)::text FROM (
		SELECT id, user_id AS "userId", provider_id AS "providerId",
		       error_message AS "errorMessage",
		       to_char(DATE_TRUNC('hour', created_at AT TIME ZONE $1),
		               'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "hour"
		FROM message_request
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY created_at
	) t`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询限流事件失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminRateLimitEventRow, 0, 32)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取限流事件行失败: %w", err)
		}
		var row AdminRateLimitEventRow
		if err := json.Unmarshal([]byte(payload), &row); err != nil {
			return nil, fmt.Errorf("store: 限流事件行反序列化失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历限流事件行失败: %w", err)
	}
	return results, nil
}

// AdminKeyStringByID 取密钥原文（Node 的 getKeyStringByIdCached）。
//
// 限流事件按 **key 串**过滤（message_request.key 存的是原文），而路由传进来的是 key id，
// 故要先换一次。不存在时返回 ok=false——Node 侧此时直接作答「空统计」，不是 404。
func (p *Pools) AdminKeyStringByID(ctx context.Context, id int64) (string, bool, error) {
	pool, err := p.Control()
	if err != nil {
		return "", false, err
	}
	var key string
	if err := pool.QueryRow(ctx, `SELECT key FROM keys WHERE id = $1`, id).Scan(&key); err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: 查询密钥原文失败: %w", err)
	}
	return key, true, nil
}

// parseNumericText 把 numeric 文本转成浮点（空串与非法值按 0，与 Node 的 `?? 0` 同序）。
func parseNumericText(value string) float64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0
	}
	return parsed
}
