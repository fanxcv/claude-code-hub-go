package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// 本文件是**通知数据生成器**的取数面（Node：src/lib/notification/tasks/*）。
//
// 为什么单独一个文件：三个生成器（日报/成本预警/缓存命中率预警）原本只取数于
// Node 的 repository 层，Go 迁移时把「生成器」整块留空了。取数查询集中在此，
// 便于与 Node 的 SQL 逐条对拍，也不去改动既有的账务/排行榜查询。
//
// 与 Node 的对照：
//   - NotifyKeysWithCostLimits        ← tasks/cost-alert.ts checkUserQuotas 的 keys 选择列
//   - NotifyProvidersWithCostLimits   ← tasks/cost-alert.ts checkProviderQuotas 的 providers 选择列
//   - NotifyProviderRefs              ← repository/provider.ts findAllProviders（告警里补 name/type）
//   - NotifyProviderModelCacheMetrics ← repository/cache-hit-rate-alert.ts
//     findProviderModelCacheHitRateMetricsForAlert（窗口内「按供应商×模型」的缓存命中口径）

// NotifyKeyCostLimit 是「设了成本限额」的密钥一行（Node 侧 keys 表的四个选择列）。
//
// 限额列用 ::text 取回：Node 的 drizzle numeric 映射为 string，Go 侧保持文本可避免
// float 抖动，再按同一 parseFloat 口径比较。
type NotifyKeyCostLimit struct {
	ID         int64
	Key        string
	UserName   string
	Limit5h    *string
	LimitWeek  *string
	LimitMonth *string
}

// NotifyKeysWithCostLimits 取所有「5h/周/月任一限额 > 0」的密钥。
//
// Node 的 SQL 无 ORDER BY（顺序由库决定）；本实现按 id 排序，让告警顺序可复现——
// 这是**登记差异**：Node 侧同一条 SQL 在不同库上可能给出不同顺序。
func (p *Pools) NotifyKeysWithCostLimits(ctx context.Context) ([]NotifyKeyCostLimit, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	const query = `SELECT id, key, name,
			limit_5h_usd::text, limit_weekly_usd::text, limit_monthly_usd::text
		FROM keys
		WHERE limit_5h_usd > 0 OR limit_weekly_usd > 0 OR limit_monthly_usd > 0
		ORDER BY id`
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: 查询通知用密钥限额失败: %w", err)
	}
	defer rows.Close()

	results := make([]NotifyKeyCostLimit, 0, 16)
	for rows.Next() {
		var item NotifyKeyCostLimit
		if err := rows.Scan(
			&item.ID, &item.Key, &item.UserName, &item.Limit5h, &item.LimitWeek, &item.LimitMonth,
		); err != nil {
			return nil, fmt.Errorf("store: 读取通知用密钥限额失败: %w", err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历通知用密钥限额失败: %w", err)
	}
	return results, nil
}

// NotifyProviderCostLimit 是「设了成本限额」的供应商一行。
//
// Node 的供应商告警只看周/月两档（无 5h），故这里不带 5h 列。
type NotifyProviderCostLimit struct {
	ID         int64
	Name       string
	LimitWeek  *string
	LimitMonth *string
}

// NotifyProvidersWithCostLimits 取所有「周/月任一限额 > 0」的供应商（含已软删者，与 Node 一致）。
func (p *Pools) NotifyProvidersWithCostLimits(ctx context.Context) ([]NotifyProviderCostLimit, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	const query = `SELECT id, name, limit_weekly_usd::text, limit_monthly_usd::text
		FROM providers
		WHERE limit_weekly_usd > 0 OR limit_monthly_usd > 0
		ORDER BY id`
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: 查询通知用供应商限额失败: %w", err)
	}
	defer rows.Close()

	results := make([]NotifyProviderCostLimit, 0, 16)
	for rows.Next() {
		var item NotifyProviderCostLimit
		if err := rows.Scan(&item.ID, &item.Name, &item.LimitWeek, &item.LimitMonth); err != nil {
			return nil, fmt.Errorf("store: 读取通知用供应商限额失败: %w", err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历通知用供应商限额失败: %w", err)
	}
	return results, nil
}

// NotifyProviderRef 是供应商的最小投影（告警正文里的 providerName / providerType）。
type NotifyProviderRef struct {
	ID           int64
	Name         string
	ProviderType string
}

// NotifyProviderRefs 复刻 findAllProviders（src/repository/provider.ts:555）的三列投影。
//
// 与 Node 同判：**筛软删**（findAllProvidersFresh 的 `isNull(deletedAt)`）。不排序——调用方
// 只是按 id 建查找表，排序没有语义。
func (p *Pools) NotifyProviderRefs(ctx context.Context) ([]NotifyProviderRef, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	const query = `SELECT id, name, provider_type FROM providers WHERE deleted_at IS NULL`
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商投影失败: %w", err)
	}
	defer rows.Close()

	results := make([]NotifyProviderRef, 0, 32)
	for rows.Next() {
		var item NotifyProviderRef
		if err := rows.Scan(&item.ID, &item.Name, &item.ProviderType); err != nil {
			return nil, fmt.Errorf("store: 读取供应商投影失败: %w", err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商投影失败: %w", err)
	}
	return results, nil
}

// NotifyCacheMetric 是「供应商 × 模型」在某个时间窗内的缓存命中口径（窗口切分前的原料）。
//
// 字段与 Node 的 ProviderModelCacheHitRateAlertMetric 同名同义：
//   - DenominatorTokens = Σ(input + cache_creation + cache_read)；
//   - HitRateTokens     = cache_read / DenominatorTokens（clamp 到 [0,1]）；
//   - Eligible*         = 只统计「上一发请求在同一会话、序号连续、且间隔不超过缓存 TTL」的行，
//     即真正有机会复用前缀缓存的那部分请求。
type NotifyCacheMetric struct {
	ProviderID   int64
	ProviderType string
	Model        string

	TotalRequests       float64
	CacheSignalRequests float64
	CacheHitRequests    float64

	SumInputTokens         float64
	SumCacheCreationTokens float64
	SumCacheReadTokens     float64
	DenominatorTokens      float64
	HitRateTokens          float64
	EngagementRate         float64

	EligibleRequests          float64
	EligibleDenominatorTokens float64
	EligibleCacheReadTokens   float64
	HitRateTokensEligible     float64
}

// notifyCacheModelField 复刻 cache-hit-rate-alert.ts 的 modelField 选择（数据源是 message_request，
// 故与排行榜那份 usage_ledger 版本分开写）。
func notifyCacheModelField(billingModelSource string) string {
	if billingModelSource == "original" {
		return `COALESCE(mr.original_model, mr.model)`
	}
	return `COALESCE(mr.model, mr.original_model)`
}

// NotifyProviderModelCacheMetrics 复刻 findProviderModelCacheHitRateMetricsForAlert 的默认调用口径：
// windowMode=rolling、statusCodeMode=2xx、TTL fallback 用内置表（codex / openai-compatible = 600s，其余 3600s）。
//
// 与 Node 的差异（登记）：
//  1. Node 的 SQL 把 prev 请求的 warmup 过滤写成 left join 条件，这里等价保留；
//     `EXCLUDE_WARMUP_CONDITION` 对主行同样保留。
//  2. Node 未对结果排序（DB 顺序）；这里按 totalRequests 倒序，与 Node 的 orderBy(desc(totalRequests)) 一致。
//
// 判定依据（TTL）：优先用「真实 TTL」——swap_cache_ttl_applied 只用于**计费口径**的 5m/1h 翻转，
// 缓存语义口径要把它还原回去，否则 gap<=TTL 的判定会整体反向。这一段与 Node 逐条对齐。
func (p *Pools) NotifyProviderModelCacheMetrics(
	ctx context.Context,
	start time.Time,
	end time.Time,
	billingModelSource string,
) ([]NotifyCacheMetric, error) {
	if !end.After(start) {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}

	modelField := notifyCacheModelField(billingModelSource)

	// ttl_applied 的 5m/1h 还原（见上文「判定依据」）。
	const ttlAppliedForTTL = `(CASE WHEN COALESCE(mr.swap_cache_ttl_applied, false) THEN (
			CASE WHEN mr.cache_ttl_applied = '5m' THEN '1h'
			     WHEN mr.cache_ttl_applied = '1h' THEN '5m'
			     ELSE mr.cache_ttl_applied END
		) ELSE mr.cache_ttl_applied END)`
	const cacheCreation5mForTTL = `(CASE WHEN COALESCE(mr.swap_cache_ttl_applied, false)
		THEN mr.cache_creation_1h_input_tokens ELSE mr.cache_creation_5m_input_tokens END)`
	const cacheCreation1hForTTL = `(CASE WHEN COALESCE(mr.swap_cache_ttl_applied, false)
		THEN mr.cache_creation_5m_input_tokens ELSE mr.cache_creation_1h_input_tokens END)`
	// 纯数字 TTL 的位数/范围护栏：异常配置不得让 ::int 溢出把整条查询打挂。
	const ttlNumberText = `substring(` + ttlAppliedForTTL + ` from '^[0-9]+')`
	const ttlFallback = `(CASE WHEN p.provider_type IN ('codex', 'openai-compatible') THEN 600 ELSE 3600 END)`

	ttlSeconds := `(CASE
		WHEN COALESCE(` + cacheCreation1hForTTL + `, 0) > 0 THEN 3600
		WHEN COALESCE(` + cacheCreation5mForTTL + `, 0) > 0 THEN 300
		WHEN ` + ttlAppliedForTTL + ` = '1h' THEN 3600
		WHEN ` + ttlAppliedForTTL + ` = '5m' THEN 300
		WHEN ` + ttlAppliedForTTL + ` = 'mixed' THEN 3600
		WHEN ` + ttlAppliedForTTL + ` ~ '^[0-9]+h$' THEN (
			CASE WHEN char_length(` + ttlNumberText + `) > 9 THEN ` + ttlFallback + `
			     WHEN (` + ttlNumberText + `)::int > 168 THEN ` + ttlFallback + `
			     ELSE (` + ttlNumberText + `)::int * 3600 END)
		WHEN ` + ttlAppliedForTTL + ` ~ '^[0-9]+m$' THEN (
			CASE WHEN char_length(` + ttlNumberText + `) > 9 THEN ` + ttlFallback + `
			     WHEN (` + ttlNumberText + `)::int > 10080 THEN ` + ttlFallback + `
			     ELSE (` + ttlNumberText + `)::int * 60 END)
		WHEN ` + ttlAppliedForTTL + ` ~ '^[0-9]+s$' THEN (
			CASE WHEN char_length(` + ttlNumberText + `) > 9 THEN ` + ttlFallback + `
			     WHEN (` + ttlNumberText + `)::int > 604800 THEN ` + ttlFallback + `
			     ELSE (` + ttlNumberText + `)::int END)
		ELSE ` + ttlFallback + ` END)`

	denominator := `(COALESCE(mr.input_tokens, 0)::double precision
		+ COALESCE(mr.cache_creation_input_tokens, 0)::double precision
		+ COALESCE(mr.cache_read_input_tokens, 0)::double precision)`
	cacheRead := `COALESCE(mr.cache_read_input_tokens, 0)::double precision`

	eligible := `(mr.session_id IS NOT NULL AND btrim(mr.session_id) <> ''
		AND mr.request_sequence > 1
		AND prev.created_at IS NOT NULL
		AND EXTRACT(EPOCH FROM (mr.created_at - prev.created_at))::double precision >= 0
		AND EXTRACT(EPOCH FROM (mr.created_at - prev.created_at))::double precision <= ` + ttlSeconds + `)`

	query := `SELECT row_to_json(t)::text FROM (
		SELECT r.provider_id AS "providerId",
		       r.provider_type AS "providerType",
		       r.model AS "model",
		       count(*)::double precision AS "totalRequests",
		       count(*) FILTER (WHERE r.cache_signal)::double precision AS "cacheSignalRequests",
		       count(*) FILTER (WHERE r.cache_hit)::double precision AS "cacheHitRequests",
		       COALESCE(sum(r.input_tokens)::double precision, 0::double precision) AS "sumInputTokens",
		       COALESCE(sum(r.cache_creation_tokens)::double precision, 0::double precision) AS "sumCacheCreationTokens",
		       COALESCE(sum(r.cache_read_tokens)::double precision, 0::double precision) AS "sumCacheReadTokens",
		       COALESCE(sum(r.denominator_tokens)::double precision, 0::double precision) AS "denominatorTokens",
		       COALESCE(sum(r.cache_read_tokens)::double precision / NULLIF(COALESCE(sum(r.denominator_tokens)::double precision, 0::double precision), 0::double precision), 0::double precision) AS "hitRateTokens",
		       COALESCE(count(*) FILTER (WHERE r.cache_signal)::double precision / NULLIF(count(*)::double precision, 0::double precision), 0::double precision) AS "engagementRate",
		       count(*) FILTER (WHERE r.eligible)::double precision AS "eligibleRequests",
		       COALESCE(sum(r.denominator_tokens) FILTER (WHERE r.eligible)::double precision, 0::double precision) AS "eligibleDenominatorTokens",
		       COALESCE(sum(r.cache_read_tokens) FILTER (WHERE r.eligible)::double precision, 0::double precision) AS "eligibleCacheReadTokens",
		       COALESCE(sum(r.cache_read_tokens) FILTER (WHERE r.eligible)::double precision / NULLIF(COALESCE(sum(r.denominator_tokens) FILTER (WHERE r.eligible)::double precision, 0::double precision), 0::double precision), 0::double precision) AS "hitRateTokensEligible"
		FROM (
			SELECT mr.provider_id AS provider_id,
			       p.provider_type AS provider_type,
			       ` + modelField + ` AS model,
			       COALESCE(mr.input_tokens, 0)::double precision AS input_tokens,
			       COALESCE(mr.cache_creation_input_tokens, 0)::double precision AS cache_creation_tokens,
			       ` + cacheRead + ` AS cache_read_tokens,
			       ` + denominator + ` AS denominator_tokens,
			       (COALESCE(mr.cache_creation_input_tokens, 0) > 0 OR COALESCE(mr.cache_read_input_tokens, 0) > 0) AS cache_signal,
			       (COALESCE(mr.cache_read_input_tokens, 0) > 0) AS cache_hit,
			       ` + eligible + ` AS eligible
			FROM message_request mr
			JOIN providers p ON mr.provider_id = p.id AND p.deleted_at IS NULL
			LEFT JOIN message_request prev
			       ON prev.session_id = mr.session_id
			      AND prev.request_sequence = (mr.request_sequence - 1)
			      AND prev.deleted_at IS NULL
			      AND prev.is_replay = false
			      AND (prev.blocked_by IS NULL OR prev.blocked_by <> 'warmup')
			WHERE mr.deleted_at IS NULL
			  AND mr.is_replay = false
			  AND (mr.blocked_by IS NULL OR mr.blocked_by <> 'warmup')
			  AND mr.created_at >= $1 AND mr.created_at < $2
			  AND mr.status_code >= 200 AND mr.status_code < 300
			  AND ` + modelField + ` IS NOT NULL AND btrim(` + modelField + `) <> ''
		) r
		GROUP BY r.provider_id, r.provider_type, r.model
		ORDER BY count(*) DESC
	) t`

	rows, err := pool.Query(ctx, query, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: 查询缓存命中口径失败: %w", err)
	}
	defer rows.Close()

	type metricRow struct {
		ProviderID                int64   `json:"providerId"`
		ProviderType              string  `json:"providerType"`
		Model                     string  `json:"model"`
		TotalRequests             float64 `json:"totalRequests"`
		CacheSignalRequests       float64 `json:"cacheSignalRequests"`
		CacheHitRequests          float64 `json:"cacheHitRequests"`
		SumInputTokens            float64 `json:"sumInputTokens"`
		SumCacheCreationTokens    float64 `json:"sumCacheCreationTokens"`
		SumCacheReadTokens        float64 `json:"sumCacheReadTokens"`
		DenominatorTokens         float64 `json:"denominatorTokens"`
		HitRateTokens             float64 `json:"hitRateTokens"`
		EngagementRate            float64 `json:"engagementRate"`
		EligibleRequests          float64 `json:"eligibleRequests"`
		EligibleDenominatorTokens float64 `json:"eligibleDenominatorTokens"`
		EligibleCacheReadTokens   float64 `json:"eligibleCacheReadTokens"`
		HitRateTokensEligible     float64 `json:"hitRateTokensEligible"`
	}

	results := make([]NotifyCacheMetric, 0, 64)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取缓存命中口径失败: %w", err)
		}
		var item metricRow
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			return nil, fmt.Errorf("store: 解析缓存命中口径失败: %w", err)
		}
		results = append(results, NotifyCacheMetric{
			ProviderID:                item.ProviderID,
			ProviderType:              item.ProviderType,
			Model:                     item.Model,
			TotalRequests:             item.TotalRequests,
			CacheSignalRequests:       item.CacheSignalRequests,
			CacheHitRequests:          item.CacheHitRequests,
			SumInputTokens:            item.SumInputTokens,
			SumCacheCreationTokens:    item.SumCacheCreationTokens,
			SumCacheReadTokens:        item.SumCacheReadTokens,
			DenominatorTokens:         item.DenominatorTokens,
			HitRateTokens:             clampRate01(item.HitRateTokens),
			EngagementRate:            clampRate01(item.EngagementRate),
			EligibleRequests:          item.EligibleRequests,
			EligibleDenominatorTokens: item.EligibleDenominatorTokens,
			EligibleCacheReadTokens:   item.EligibleCacheReadTokens,
			HitRateTokensEligible:     clampRate01(item.HitRateTokensEligible),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历缓存命中口径失败: %w", err)
	}
	return results, nil
}

// clampRate01 与 Node 的同名函数一致：非有限值归 0，其余夹到 [0,1]。
func clampRate01(value float64) float64 {
	switch {
	case value != value: // NaN
		return 0
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}
