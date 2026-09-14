package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// 本文件是**统计面**（图表数据）在 store 层的实现：Node `src/repository/statistics.ts` 里
// 「按时间桶 × 实体」展开的三条聚合。
//
// 唯一真源：src/repository/statistics.ts 的
//
//	getUserStatisticsFromDB   → AdminChartUserStatistics（AdminChartMixedStatistics 的 others 半）
//	getKeyStatisticsFromDB    → AdminChartKeyStatistics
//	getMixedStatisticsFromDB  → AdminChartMixedStatistics
//
// 连同零填充 zeroFillUserStats / zeroFillKeyStats / zeroFillMixedOthersStats 一并复刻。
//
// 口径（逐条照抄）：
//   - 时间窗按**系统时区**的当地日界：today = 当日 0 点起 24 个小时桶；7days / 30days = 含今日在内的
//     7 / 30 个日桶；thisMonth = 本月 1 日到今天。
//   - 桶表达式是 `DATE_TRUNC('hour'|'day', created_at AT TIME ZONE tz)`，取到的是
//     **timestamp without time zone**（当地墙上时间）。回程用 to_char 取文本，再按该时区解释成时刻
//     ——与 Node 的 normalizeBucketInstant（formatLocalDateTime + fromZonedTime）同一件事。
//   - 计费口径是 BillingCondition（排除拦截行、replay 行、不计费端点）。
//   - 零填充是「桶 × 实体」的笛卡尔积：缺行补 `api_calls = 0`、`total_cost = 0`。
//     行的顺序是**桶升序为主、实体名升序为次**（Node 的两层 for 循环）。
//   - 实体清单是「未软删」的全量（users 表全库 / keys 表按 user_id），**与时间段无关**。
//
// 两处登记（都不影响数值与键名，只影响顺序 / 每请求查询次数）：
//
//  1. **实体排序**：Node 是 `Array.sort((a, b) => a.name.localeCompare(b.name))`，走 ICU 排序
//     （大小写不敏感、按语言习惯）；Go 侧用字节序。名字是纯小写 ASCII 时两者等价，
//     含大小写混排或非 ASCII 时顺序会不同。顺序只影响图例排列与 `users` 数组次序。
//  2. **Redis 缓存层未移植**（Node 的 getStatisticsWithCache：30 秒 TTL + SET NX 互斥 +
//     抢不到锁时轮询 5 秒）。少了它只是每个请求多两次查询；键里嵌着时间段与时区，命中与未命中
//     返回同一份数据，**响应不受影响**。失效路径不受牵连：`statistics:*` 前缀已由管理面的
//     DashboardCacheInvalidator 清理，不缓存也就不会留下过期数据。
//
// 分道：一律走 control（管理面只读面的既有纪律，不占数据面连接）。

// AdminStatisticsRange 是统计面的时间范围（Node 的 TimeRange，src/types/statistics.ts:1）。
type AdminStatisticsRange string

const (
	AdminStatisticsToday     AdminStatisticsRange = "today"
	AdminStatistics7Days     AdminStatisticsRange = "7days"
	AdminStatistics30Days    AdminStatisticsRange = "30days"
	AdminStatisticsThisMonth AdminStatisticsRange = "thisMonth"
)

// AdminStatisticsResolution 复刻 TIME_RANGE_OPTIONS 的 resolution：today 按小时，其余按日。
func (r AdminStatisticsRange) Resolution() string {
	if r == AdminStatisticsToday {
		return "hour"
	}
	return "day"
}

// Valid 报告是否为合法的四个取值（Node 的 TIME_RANGE_OPTIONS.find 命中与否）。
func (r AdminStatisticsRange) Valid() bool {
	switch r {
	case AdminStatisticsToday, AdminStatistics7Days, AdminStatistics30Days, AdminStatisticsThisMonth:
		return true
	default:
		return false
	}
}

// AdminStatisticsEntity 是统计的实体维度（用户或密钥）的最小投影：只要 id 与名字。
type AdminStatisticsEntity struct {
	ID   int64
	Name string
}

// AdminStatisticsUserRow 是 users 维度零填充后的一行（Node 的 DatabaseStatRow）。
type AdminStatisticsUserRow struct {
	UserID   int64
	UserName string
	Bucket   time.Time
	APICalls int64
	// CostText 是 numeric 文本（零填充行是 "0"）。
	// 与 keys 维度不同之处：Node 侧零填充写的是**数字** 0 而不是字符串，
	// 但 users 维度的消费经 formatCostForStorage 一律转成 15 位小数的**字符串**，
	// 两种来源最终同形，故这里用同一个字段表达。
	CostText string
}

// AdminStatisticsKeyRow 是 keys 维度零填充后的一行（Node 的 DatabaseKeyStatRow）。
//
// ZeroFilled 是必须的区分：Node 侧零填充行的 total_cost 是**数字** 0，数据库行的
// total_cost 是 **numeric 文本**（字符串）。key-trend 直接把这两种值透出到 JSON，
// 故类型不能归一。
type AdminStatisticsKeyRow struct {
	KeyID      int64
	KeyName    string
	Bucket     time.Time
	APICalls   int64
	CostText   string
	ZeroFilled bool
}

// adminStatisticsWindowSQL 是某个时间段对应的四段 SQL 片段（复刻 getTimeRangeSqlConfig）。
//
// 时间参数一律是 $1（时区名）；startExpr / endExpr 是带时区的时刻，bucketExpr 是当地墙上时间的
// 桶表达式（引用 usage_ledger 的别名 l）。
type adminStatisticsWindowSQL struct {
	startExpr   string
	endExpr     string
	bucketExpr  string
	seriesQuery string
}

// adminStatisticsBucketLayout 是桶文本的格式（秒级足够：桶按小时或日对齐）。
const adminStatisticsBucketLayout = "2006-01-02T15:04:05"

// adminStatisticsBucketExpr 是 SQL 里把桶表达式渲染成文本的格式串（与上面的 layout 同形）。
const adminStatisticsBucketExpr = `'YYYY-MM-DD"T"HH24:MI:SS'`

// adminStatisticsWindow 复刻 getTimeRangeSqlConfig：四个时间段各自的窗口与桶。
func adminStatisticsWindow(timeRange AdminStatisticsRange) (adminStatisticsWindowSQL, error) {
	localDay := "DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1)"
	localDayExpr := "(" + localDay + " AT TIME ZONE $1)"
	nextDayExpr := "((" + localDay + " + INTERVAL '1 day') AT TIME ZONE $1)"

	switch timeRange {
	case AdminStatisticsToday:
		return adminStatisticsWindowSQL{
			startExpr:  localDayExpr,
			endExpr:    nextDayExpr,
			bucketExpr: "DATE_TRUNC('hour', l.created_at AT TIME ZONE $1)",
			seriesQuery: "SELECT generate_series(" + localDay + ", " + localDay +
				" + INTERVAL '23 hours', '1 hour'::interval)",
		}, nil
	case AdminStatistics7Days:
		return adminStatisticsWindowSQL{
			startExpr:  "((" + localDay + " - INTERVAL '6 days') AT TIME ZONE $1)",
			endExpr:    nextDayExpr,
			bucketExpr: "DATE_TRUNC('day', l.created_at AT TIME ZONE $1)",
			seriesQuery: "SELECT generate_series((CURRENT_TIMESTAMP AT TIME ZONE $1)::date - INTERVAL '6 days', " +
				"(CURRENT_TIMESTAMP AT TIME ZONE $1)::date, '1 day'::interval)",
		}, nil
	case AdminStatistics30Days:
		return adminStatisticsWindowSQL{
			startExpr:  "((" + localDay + " - INTERVAL '29 days') AT TIME ZONE $1)",
			endExpr:    nextDayExpr,
			bucketExpr: "DATE_TRUNC('day', l.created_at AT TIME ZONE $1)",
			seriesQuery: "SELECT generate_series((CURRENT_TIMESTAMP AT TIME ZONE $1)::date - INTERVAL '29 days', " +
				"(CURRENT_TIMESTAMP AT TIME ZONE $1)::date, '1 day'::interval)",
		}, nil
	case AdminStatisticsThisMonth:
		return adminStatisticsWindowSQL{
			startExpr:  "((DATE_TRUNC('month', CURRENT_TIMESTAMP AT TIME ZONE $1)) AT TIME ZONE $1)",
			endExpr:    nextDayExpr,
			bucketExpr: "DATE_TRUNC('day', l.created_at AT TIME ZONE $1)",
			seriesQuery: "SELECT generate_series(DATE_TRUNC('month', CURRENT_TIMESTAMP AT TIME ZONE $1), " +
				localDay + ", '1 day'::interval)",
		}, nil
	default:
		return adminStatisticsWindowSQL{}, fmt.Errorf("store: 不支持的统计时间段 %q", timeRange)
	}
}

// AdminActiveUserEntities 复刻 getActiveUsersFromDB：未软删用户的 (id, name)，按名字升序。
func (p *Pools) AdminActiveUserEntities(ctx context.Context) ([]AdminStatisticsEntity, error) {
	return p.adminActiveEntities(ctx,
		`SELECT id, name FROM users WHERE deleted_at IS NULL ORDER BY name ASC`)
}

// AdminActiveKeyEntities 复刻 getActiveKeysForUserFromDB：该用户未软删密钥的 (id, name)。
func (p *Pools) AdminActiveKeyEntities(
	ctx context.Context,
	userID int64,
) ([]AdminStatisticsEntity, error) {
	return p.adminActiveEntities(ctx,
		`SELECT id, name FROM keys WHERE user_id = $1 AND deleted_at IS NULL ORDER BY name ASC`, userID)
}

// adminActiveEntities 读一列 (id, name)。
func (p *Pools) adminActiveEntities(
	ctx context.Context,
	query string,
	args ...any,
) ([]AdminStatisticsEntity, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询统计实体清单失败: %w", err)
	}
	defer rows.Close()

	entities := make([]AdminStatisticsEntity, 0, 16)
	for rows.Next() {
		var entity AdminStatisticsEntity
		if err := rows.Scan(&entity.ID, &entity.Name); err != nil {
			return nil, fmt.Errorf("store: 读取统计实体失败: %w", err)
		}
		entities = append(entities, entity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历统计实体失败: %w", err)
	}
	return entities, nil
}

// adminStatisticsBuckets 复刻 getTimeBuckets：按时间段生成桶序列，升序。
func (p *Pools) adminStatisticsBuckets(
	ctx context.Context,
	timeRange AdminStatisticsRange,
	timezone string,
) ([]time.Time, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	window, err := adminStatisticsWindow(timeRange)
	if err != nil {
		return nil, err
	}
	query := "SELECT to_char(bucket, " + adminStatisticsBucketExpr + ") FROM (" +
		window.seriesQuery + ") AS series(bucket)"

	rows, err := pool.Query(ctx, query, timezone)
	if err != nil {
		return nil, fmt.Errorf("store: 生成统计时间桶失败: %w", err)
	}
	defer rows.Close()

	buckets := make([]time.Time, 0, 32)
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("store: 读取统计时间桶失败: %w", err)
		}
		bucket, ok := adminStatisticsBucketInstant(text, timezone)
		if !ok {
			continue
		}
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历统计时间桶失败: %w", err)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Before(buckets[j]) })
	return buckets, nil
}

// adminStatisticsBucketInstant 把当地墙上时间文本解释成系统时区的时刻（UTC 表示）。
func adminStatisticsBucketInstant(text, timezone string) (time.Time, bool) {
	location, err := time.LoadLocation(timezone)
	if err != nil || location == nil {
		location = time.UTC
	}
	parsed, err := time.ParseInLocation(adminStatisticsBucketLayout, text, location)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// adminStatisticsBucketKey 是零填充用的查表键（实体 id + 桶的绝对时刻）。
func adminStatisticsBucketKey(entityID int64, bucket time.Time) string {
	return fmt.Sprintf("%d:%d", entityID, bucket.Unix())
}

// adminStatisticsSortEntities 按名字升序排实体（见文件头的 ICU 排序登记）。
func adminStatisticsSortEntities(entities []AdminStatisticsEntity) []AdminStatisticsEntity {
	sorted := append([]AdminStatisticsEntity{}, entities...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return sorted
}

// AdminChartUserStatistics 复刻 getUserStatisticsFromDB：全库用户的图表数据（含零填充）。
func (p *Pools) AdminChartUserStatistics(
	ctx context.Context,
	timeRange AdminStatisticsRange,
	timezone string,
) ([]AdminStatisticsUserRow, error) {
	window, err := adminStatisticsWindow(timeRange)
	if err != nil {
		return nil, err
	}
	buckets, err := p.adminStatisticsBuckets(ctx, timeRange, timezone)
	if err != nil {
		return nil, err
	}
	entities, err := p.AdminActiveUserEntities(ctx)
	if err != nil {
		return nil, err
	}

	query := adminStatisticsOuterSelect(`
		SELECT u.id AS entity_id, u.name AS entity_name,
		       ` + window.bucketExpr + ` AS bucket,
		       COUNT(l.id)::bigint AS api_calls,
		       COALESCE(SUM(l.cost_usd), 0)::text AS cost_text
		FROM users u
		LEFT JOIN usage_ledger l ON u.id = l.user_id
			AND l.created_at >= ` + window.startExpr + `
			AND l.created_at < ` + window.endExpr + `
			AND ` + LedgerBillingConditionFor("l") + `
		WHERE u.deleted_at IS NULL
		GROUP BY u.id, u.name, ` + window.bucketExpr + `
	`)

	rows, err := p.adminStatisticsBucketRows(ctx, query, timezone)
	if err != nil {
		return nil, err
	}
	return adminStatisticsFillUsers(rows, entities, buckets), nil
}

// AdminChartKeyStatistics 复刻 getKeyStatisticsFromDB：某用户自己密钥的图表数据（含零填充）。
func (p *Pools) AdminChartKeyStatistics(
	ctx context.Context,
	userID int64,
	timeRange AdminStatisticsRange,
	timezone string,
) ([]AdminStatisticsKeyRow, error) {
	window, err := adminStatisticsWindow(timeRange)
	if err != nil {
		return nil, err
	}
	buckets, err := p.adminStatisticsBuckets(ctx, timeRange, timezone)
	if err != nil {
		return nil, err
	}
	entities, err := p.AdminActiveKeyEntities(ctx, userID)
	if err != nil {
		return nil, err
	}

	rows, err := p.adminStatisticsBucketRows(ctx, p.adminKeyStatisticsQuery(window), timezone, userID)
	if err != nil {
		return nil, err
	}
	return adminStatisticsFillKeys(rows, entities, buckets), nil
}

// AdminChartMixedStatistics 复刻 getMixedStatisticsFromDB：自己的密钥明细 + 其他用户的汇总。
//
// othersAggregate 的实体是虚拟的（user_id = -1、user_name = "__others__"），与 Node 同。
func (p *Pools) AdminChartMixedStatistics(
	ctx context.Context,
	userID int64,
	timeRange AdminStatisticsRange,
	timezone string,
) ([]AdminStatisticsKeyRow, []AdminStatisticsUserRow, error) {
	window, err := adminStatisticsWindow(timeRange)
	if err != nil {
		return nil, nil, err
	}
	buckets, err := p.adminStatisticsBuckets(ctx, timeRange, timezone)
	if err != nil {
		return nil, nil, err
	}
	entities, err := p.AdminActiveKeyEntities(ctx, userID)
	if err != nil {
		return nil, nil, err
	}

	ownRows, err := p.adminStatisticsBucketRows(ctx, p.adminKeyStatisticsQuery(window), timezone, userID)
	if err != nil {
		return nil, nil, err
	}

	// 虚拟实体的 id 必须是 -1：零填充按「实体 id + 桶」查表，而 fill 的实体清单是 {ID: -1}，
	// 两边的 id 不一致就会把真实行全当缺失（mixed 的 others 半恒为 0）。
	othersQuery := adminStatisticsOuterSelect(`
		SELECT (-1)::bigint AS entity_id, '__others__'::varchar AS entity_name,
		       ` + window.bucketExpr + ` AS bucket,
		       COUNT(l.id)::bigint AS api_calls,
		       COALESCE(SUM(l.cost_usd), 0)::text AS cost_text
		FROM usage_ledger l
		WHERE l.user_id <> $2
			AND l.created_at >= ` + window.startExpr + `
			AND l.created_at < ` + window.endExpr + `
			AND ` + LedgerBillingConditionFor("l") + `
		GROUP BY ` + window.bucketExpr + `
	`)

	otherRows, err := p.adminStatisticsBucketRows(ctx, othersQuery, timezone, userID)
	if err != nil {
		return nil, nil, err
	}

	// Node 的 othersAggregate 走 zeroFillMixedOthersStats：按桶聚合，实体恒为 -1 / "__others__"。
	others := adminStatisticsFillUsers(otherRows, []AdminStatisticsEntity{
		{ID: -1, Name: "__others__"},
	}, buckets)

	return adminStatisticsFillKeys(ownRows, entities, buckets), others, nil
}

// adminKeyStatisticsQuery 是 keys 维度的聚合（getKeyStatisticsFromDB 与 mixed 的 ownKeys 半共用）。
//
// 占位符：$1 时区、$2 user_id。键的关联是**密钥原文**（usage_ledger.key = keys.key），与 Node 同。
func (p *Pools) adminKeyStatisticsQuery(window adminStatisticsWindowSQL) string {
	return adminStatisticsOuterSelect(`
		SELECT k.id AS entity_id, k.name AS entity_name,
		       ` + window.bucketExpr + ` AS bucket,
		       COUNT(l.id)::bigint AS api_calls,
		       COALESCE(SUM(l.cost_usd), 0)::text AS cost_text
		FROM keys k
		LEFT JOIN usage_ledger l ON l.key = k.key
			AND l.user_id = $2
			AND l.created_at >= ` + window.startExpr + `
			AND l.created_at < ` + window.endExpr + `
			AND ` + LedgerBillingConditionFor("l") + `
		WHERE k.user_id = $2
			AND k.deleted_at IS NULL
		GROUP BY k.id, k.name, ` + window.bucketExpr + `
	`)
}

// adminStatisticsOuterSelect 把聚合子查询的桶列用 to_char 渲染成**当地墙上时间文本**。
//
// 为什么要过一层文本：桶是 timestamp without time zone，直读成 time.Time 会带上会话时区，
// 而契约要的是「当地墙上时间再按系统时区解释」（见文件头的口径说明）。
// 内层子查询须按 (entity_id, entity_name, bucket, api_calls, cost_text) 五列命名。
func adminStatisticsOuterSelect(inner string) string {
	return "SELECT t.entity_id, t.entity_name, to_char(t.bucket, " + adminStatisticsBucketExpr +
		") AS bucket_text, t.api_calls, t.cost_text FROM (" + inner + ") t"
}

// adminStatisticsRawRow 是聚合查询的原始一行（桶已回落成时刻，消费还是 numeric 文本）。
type adminStatisticsRawRow struct {
	EntityID   int64
	EntityName string
	Bucket     time.Time
	APICalls   int64
	CostText   string
}

// adminStatisticsBucketRows 跑一条聚合查询并把桶文本解释成时刻。
//
// args 里后续参数从 $2 起（$1 固定是时区名）。
func (p *Pools) adminStatisticsBucketRows(
	ctx context.Context,
	query string,
	timezone string,
	args ...any,
) ([]adminStatisticsRawRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	args = append([]any{timezone}, args...)
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询统计桶失败: %w", err)
	}
	defer rows.Close()

	results := make([]adminStatisticsRawRow, 0, 64)
	for rows.Next() {
		var row adminStatisticsRawRow
		// bucket 可能为 NULL：左连时实体在该时间窗内没有任何账本行就是 NULL。
		// Node 的 normalizeBucketInstant 对 null 返回 null，零填充里的 `if (!bucket) continue`
		// 把这行丢掉并交给笛卡尔积补 0，这里必须同判，不能当成错误。
		var bucketText *string
		if err := rows.Scan(&row.EntityID, &row.EntityName, &bucketText, &row.APICalls,
			&row.CostText); err != nil {
			return nil, fmt.Errorf("store: 读取统计桶失败: %w", err)
		}
		if bucketText == nil {
			continue
		}
		bucket, ok := adminStatisticsBucketInstant(*bucketText, timezone)
		if !ok {
			continue
		}
		row.Bucket = bucket
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历统计桶失败: %w", err)
	}
	return results, nil
}

// adminStatisticsFillUsers 复刻 zeroFillUserStats。
func adminStatisticsFillUsers(
	rows []adminStatisticsRawRow,
	entities []AdminStatisticsEntity,
	buckets []time.Time,
) []AdminStatisticsUserRow {
	type cell struct {
		apiCalls int64
		costText string
	}
	cells := make(map[string]cell, len(rows))
	for _, row := range rows {
		cells[adminStatisticsBucketKey(row.EntityID, row.Bucket)] = cell{
			apiCalls: row.APICalls,
			costText: row.CostText,
		}
	}

	sorted := adminStatisticsSortEntities(entities)
	filled := make([]AdminStatisticsUserRow, 0, len(buckets)*len(sorted))
	for _, bucket := range buckets {
		for _, entity := range sorted {
			value, ok := cells[adminStatisticsBucketKey(entity.ID, bucket)]
			result := AdminStatisticsUserRow{
				UserID:   entity.ID,
				UserName: entity.Name,
				Bucket:   bucket,
			}
			if ok {
				result.APICalls = value.apiCalls
				result.CostText = value.costText
			} else {
				result.CostText = "0"
			}
			filled = append(filled, result)
		}
	}
	return filled
}

// adminStatisticsFillKeys 复刻 zeroFillKeyStats（零填充行的 ZeroFilled 为真，见类型说明）。
func adminStatisticsFillKeys(
	rows []adminStatisticsRawRow,
	entities []AdminStatisticsEntity,
	buckets []time.Time,
) []AdminStatisticsKeyRow {
	type cell struct {
		apiCalls int64
		costText string
	}
	cells := make(map[string]cell, len(rows))
	for _, row := range rows {
		cells[adminStatisticsBucketKey(row.EntityID, row.Bucket)] = cell{
			apiCalls: row.APICalls,
			costText: row.CostText,
		}
	}

	sorted := adminStatisticsSortEntities(entities)
	filled := make([]AdminStatisticsKeyRow, 0, len(buckets)*len(sorted))
	for _, bucket := range buckets {
		for _, entity := range sorted {
			value, ok := cells[adminStatisticsBucketKey(entity.ID, bucket)]
			result := AdminStatisticsKeyRow{
				KeyID:      entity.ID,
				KeyName:    entity.Name,
				Bucket:     bucket,
				ZeroFilled: !ok,
			}
			if ok {
				result.APICalls = value.apiCalls
				result.CostText = value.costText
			} else {
				result.CostText = "0"
			}
			filled = append(filled, result)
		}
	}
	return filled
}
