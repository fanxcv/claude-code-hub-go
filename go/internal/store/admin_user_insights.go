package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是 admin-user-insights 三条聚合查询的存储层。
//
// 唯一真源：src/repository/admin-user-insights.ts。口径、分组与排序逐条对齐该文件：
//   - 计费口径 LEDGER_BILLING_CONDITION（store.BillingCondition）。
//   - 时间条件按**系统时区**的日历日切分：startDate 起当日 0 点（含）、endDate 次日 0 点（不含）。
//   - model 维度按 billingModelSource 选列（model 或 originalModel），再做 NULLIF(TRIM(...))。
//   - provider 维度 INNER JOIN providers 并按 providers.id 分组。
//   - 两者都按 cost 倒序。
//
// 未移植的一条（key-trend）不在本文件：它读的是统计缓存（getStatisticsWithCache），
// 属于统计面（statistics.ts 1236 行）的依赖，单独成波。

// AdminUserInsightsOverview 是 overview 的四个指标（对应 UserInsightsOverviewMetrics）。
type AdminUserInsightsOverview struct {
	RequestCount    int64   `json:"requestCount"`
	TotalCost       float64 `json:"totalCost"`
	AvgResponseTime float64 `json:"avgResponseTime"`
	ErrorRate       float64 `json:"errorRate"`
}

// AdminUserInsightsFilters 是两条 breakdown 的公共筛选。
type AdminUserInsightsFilters struct {
	StartDate string
	EndDate   string
	// KeyID 非零时按 keys.key 过滤（Node 用子查询取该 id 的 key 串）。
	KeyID int64
	// ProviderID / Model 各自只在对应维度上有意义。
	ProviderID int64
	Model      string
}

// AdminUserModelBreakdownItem 对应 AdminUserModelBreakdownItem。
type AdminUserModelBreakdownItem struct {
	Model               *string `json:"model"`
	Requests            int64   `json:"requests"`
	Cost                float64 `json:"cost"`
	InputTokens         float64 `json:"inputTokens"`
	OutputTokens        float64 `json:"outputTokens"`
	CacheCreationTokens float64 `json:"cacheCreationTokens"`
	CacheReadTokens     float64 `json:"cacheReadTokens"`
}

// AdminUserProviderBreakdownItem 对应 AdminUserProviderBreakdownItem。
type AdminUserProviderBreakdownItem struct {
	ProviderID          int64   `json:"providerId"`
	ProviderName        *string `json:"providerName"`
	Requests            int64   `json:"requests"`
	Cost                float64 `json:"cost"`
	InputTokens         float64 `json:"inputTokens"`
	OutputTokens        float64 `json:"outputTokens"`
	CacheCreationTokens float64 `json:"cacheCreationTokens"`
	CacheReadTokens     float64 `json:"cacheReadTokens"`
}

// AdminUserOverviewMetrics 复刻 getUserOverviewMetrics。
func (p *Pools) AdminUserOverviewMetrics(
	ctx context.Context,
	userID int64,
	timezone string,
	startDate, endDate string,
) (AdminUserInsightsOverview, error) {
	pool, err := p.Control()
	if err != nil {
		return AdminUserInsightsOverview{}, err
	}
	conditions, args := insightsDateConditions(timezone, startDate, endDate)
	query := `SELECT
			COUNT(*)::bigint AS "requestCount",
			COALESCE(SUM(cost_usd)::double precision, 0) AS "totalCost",
			COALESCE(AVG(duration_ms)::double precision, 0) AS "avgDuration",
			COUNT(*) FILTER (WHERE NOT is_success)::bigint AS "errorCount"
		FROM usage_ledger
		WHERE ` + BillingCondition + ` AND user_id = $1` + conditions

	args = append([]any{userID}, args...)
	var requestCount, errorCount int64
	var totalCost, avgDuration float64
	if err := pool.QueryRow(ctx, query, args...).
		Scan(&requestCount, &totalCost, &avgDuration, &errorCount); err != nil {
		return AdminUserInsightsOverview{}, fmt.Errorf("store: 查询用户概览指标失败: %w", err)
	}

	// Node: totalCost 走 toDecimalPlaces(6)；avgResponseTime 取整；errorRate 保留两位小数。
	overview := AdminUserInsightsOverview{
		RequestCount:    requestCount,
		TotalCost:       insightsRoundFloat(totalCost, 6),
		AvgResponseTime: float64(int64(avgDuration + 0.5)),
	}
	if requestCount > 0 {
		overview.ErrorRate = insightsRoundFloat(float64(errorCount)/float64(requestCount)*100, 2)
	}
	return overview, nil
}

// AdminUserModelBreakdown 复刻 getUserModelBreakdown。
func (p *Pools) AdminUserModelBreakdown(
	ctx context.Context,
	userID int64,
	billingModelSource string,
	filters AdminUserInsightsFilters,
	timezone string,
) ([]AdminUserModelBreakdownItem, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	modelColumn := "COALESCE(model, original_model)"
	if billingModelSource == "original" {
		modelColumn = "COALESCE(original_model, model)"
	}
	modelField := "NULLIF(TRIM(" + modelColumn + "), '')"

	conditions, args := insightsDateConditions(timezone, filters.StartDate, filters.EndDate)
	args = append([]any{userID}, args...)
	extra := ""
	if filters.KeyID > 0 {
		args = append(args, filters.KeyID)
		extra += fmt.Sprintf(` AND key = (SELECT k."key" FROM "keys" k WHERE k."id" = $%d)`, len(args))
	}
	if filters.ProviderID > 0 {
		args = append(args, filters.ProviderID)
		extra += fmt.Sprintf(" AND final_provider_id = $%d", len(args))
	}

	query := `SELECT
			` + modelField + ` AS "model",
			COUNT(*)::bigint AS "requests",
			COALESCE(SUM(cost_usd)::double precision, 0) AS "cost",
			COALESCE(SUM(input_tokens)::double precision, 0) AS "inputTokens",
			COALESCE(SUM(output_tokens)::double precision, 0) AS "outputTokens",
			COALESCE(SUM(cache_creation_input_tokens)::double precision, 0) AS "cacheCreationTokens",
			COALESCE(SUM(cache_read_input_tokens)::double precision, 0) AS "cacheReadTokens"
		FROM usage_ledger
		WHERE ` + BillingCondition + ` AND user_id = $1` + conditions + extra + `
		GROUP BY ` + modelField + `
		ORDER BY SUM(cost_usd) DESC NULLS LAST`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询模型维度失败: %w", err)
	}
	defer rows.Close()

	items := make([]AdminUserModelBreakdownItem, 0, 8)
	for rows.Next() {
		var item AdminUserModelBreakdownItem
		if err := rows.Scan(&item.Model, &item.Requests, &item.Cost, &item.InputTokens,
			&item.OutputTokens, &item.CacheCreationTokens, &item.CacheReadTokens); err != nil {
			return nil, fmt.Errorf("store: 读取模型维度失败: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历模型维度失败: %w", err)
	}
	return items, nil
}

// AdminUserProviderBreakdown 复刻 getUserProviderBreakdown。
func (p *Pools) AdminUserProviderBreakdown(
	ctx context.Context,
	userID int64,
	filters AdminUserInsightsFilters,
	timezone string,
) ([]AdminUserProviderBreakdownItem, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	conditions, args := insightsDateConditions(timezone, filters.StartDate, filters.EndDate)
	args = append([]any{userID}, args...)
	extra := ""
	if filters.KeyID > 0 {
		args = append(args, filters.KeyID)
		extra += fmt.Sprintf(` AND l.key = (SELECT k."key" FROM "keys" k WHERE k."id" = $%d)`, len(args))
	}
	if filters.Model != "" {
		args = append(args, filters.Model)
		extra += fmt.Sprintf(" AND (l.model ILIKE $%d OR l.original_model ILIKE $%d)",
			len(args), len(args))
	}

	query := `SELECT
			p.id AS "providerId",
			p.name AS "providerName",
			COUNT(*)::bigint AS "requests",
			COALESCE(SUM(l.cost_usd)::double precision, 0) AS "cost",
			COALESCE(SUM(l.input_tokens)::double precision, 0) AS "inputTokens",
			COALESCE(SUM(l.output_tokens)::double precision, 0) AS "outputTokens",
			COALESCE(SUM(l.cache_creation_input_tokens)::double precision, 0) AS "cacheCreationTokens",
			COALESCE(SUM(l.cache_read_input_tokens)::double precision, 0) AS "cacheReadTokens"
		FROM usage_ledger l
		INNER JOIN providers p ON l.final_provider_id = p.id
		WHERE ` + LedgerBillingConditionFor("l") + ` AND l.user_id = $1` +
		insightsAliasedDateConditions(conditions) + extra + `
		GROUP BY p.id, p.name
		ORDER BY SUM(l.cost_usd) DESC NULLS LAST`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商维度失败: %w", err)
	}
	defer rows.Close()

	items := make([]AdminUserProviderBreakdownItem, 0, 8)
	for rows.Next() {
		var item AdminUserProviderBreakdownItem
		if err := rows.Scan(&item.ProviderID, &item.ProviderName, &item.Requests, &item.Cost,
			&item.InputTokens, &item.OutputTokens, &item.CacheCreationTokens,
			&item.CacheReadTokens); err != nil {
			return nil, fmt.Errorf("store: 读取供应商维度失败: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商维度失败: %w", err)
	}
	return items, nil
}

// insightsDateConditions 复刻 buildSystemTimezoneDateConditions。
//
// 返回的片段以 ` AND ` 开头，供直接拼在 WHERE 子句后。占位符编号从 **$2** 起：
// 调用方约定 user_id 是 $1，之后按本函数返回的 args 顺序追加。
func insightsDateConditions(timezone, startDate, endDate string) (string, []any) {
	clauses := ""
	args := []any{}
	next := 2
	if startDate != "" {
		args = append(args, startDate, timezone)
		clauses += fmt.Sprintf(" AND created_at >= ($%d::date AT TIME ZONE $%d)", next, next+1)
		next += 2
	}
	if endDate != "" {
		args = append(args, endDate, timezone)
		clauses += fmt.Sprintf(
			" AND created_at < (($%d::date + INTERVAL '1 day') AT TIME ZONE $%d)", next, next+1)
		next += 2
	}
	return clauses, args
}

// insightsAliasedDateConditions 给已加别名前缀的日期条件（供应商维度用 l.created_at）。
func insightsAliasedDateConditions(clause string) string {
	return strings.ReplaceAll(clause, "created_at", "l.created_at")
}

// insightsRoundFloat 复刻 Node 的 Decimal.toDecimalPlaces（ROUND_HALF_UP）与 toFixed 语义：
// 走十进制字符串再回 float，避免二进制浮点在 .005 这类边界上抖动。
func insightsRoundFloat(value float64, places int) float64 {
	text := fmt.Sprintf("%.*f", places+4, value)
	rounded, err := decimalRound(text, places)
	if err != nil {
		return value
	}
	parsed, err := strconv.ParseFloat(rounded, 64)
	if err != nil {
		return value
	}
	return parsed
}
