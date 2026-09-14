package store

import (
	"context"
)

// AdminProviderCacheEffectivenessWindow 是 provider_cache_effectiveness 的一行
// （Node 的 ProviderCacheEffectivenessWindow，src/types/provider-cache-effectiveness.ts；
// 列名见 src/drizzle/schema.ts:1376-1395）。
//
// 时间列在 SQL 里就格式化成 Node 的 toISOString 形状（本仓既有成法，见 admin_error_rules.go:47）：
// row_to_json 会按**会话时区**渲染 timestamptz，直接解回 time.Time 再序列化会写
// `+08:00` 而 Node 写 `Z`——这是实测抓到的真分歧，不是风格选择。createdAt 可空，故用指针。
type AdminProviderCacheEffectivenessWindow struct {
	ID                      int64   `json:"id"`
	ProviderID              int64   `json:"providerId"`
	Model                   string  `json:"model"`
	CacheTTLBucket          string  `json:"cacheTtlBucket"`
	WindowStart             string  `json:"windowStart"`
	WindowEnd               string  `json:"windowEnd"`
	SampleCount             int     `json:"sampleCount"`
	EligibleCount           int     `json:"eligibleCount"`
	TheoreticalCacheTokens  int64   `json:"theoreticalCacheTokens"`
	ObservedCacheReadTokens int64   `json:"observedCacheReadTokens"`
	RawEffectivenessBp      int     `json:"rawEffectivenessBp"`
	ConfidenceBp            int     `json:"confidenceBp"`
	EffectivenessBp         int     `json:"effectivenessBp"`
	CreatedAt               *string `json:"createdAt"`
}

// AdminListProviderCacheEffectiveness 复刻 listProviderCacheEffectivenessWindows
// （src/repository/provider-cache-effectiveness.ts:27-45）。
//
// 与 Node 逐字对齐的三处：
//   - providerId 缺席时不过滤（Node 用 `options.providerId === undefined ? undefined : eq(...)`）；
//   - 排序 `window_start DESC, id DESC`（Node 注释说明用 windowStart 以吻合既有索引）；
//   - LIMIT 由调用方给（Node 在 repository 里 clamp 到 1..200，默认 50）。
func (p *Pools) AdminListProviderCacheEffectiveness(
	ctx context.Context,
	providerID *int64,
	limit int,
) ([]AdminProviderCacheEffectivenessWindow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	const query = `SELECT row_to_json(t)::text FROM (
		SELECT id,
		       provider_id AS "providerId",
		       model,
		       cache_ttl_bucket AS "cacheTtlBucket",
		       to_char(window_start AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "windowStart",
		       to_char(window_end AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "windowEnd",
		       sample_count AS "sampleCount",
		       eligible_count AS "eligibleCount",
		       theoretical_cache_tokens AS "theoreticalCacheTokens",
		       observed_cache_read_tokens AS "observedCacheReadTokens",
		       raw_effectiveness_bp AS "rawEffectivenessBp",
		       confidence_bp AS "confidenceBp",
		       effectiveness_bp AS "effectivenessBp",
		       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt"
		FROM provider_cache_effectiveness
		WHERE ($1::bigint IS NULL OR provider_id = $1::bigint)
		ORDER BY window_start DESC, id DESC
		LIMIT $2
	) t`
	return adminEndpointResourceRows[AdminProviderCacheEffectivenessWindow](ctx, pool, query, providerID, limit)
}
