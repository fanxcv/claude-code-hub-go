package store

import (
	"context"
	"fmt"
	"time"
)

// 本文件是**可用性读端点**要的三条查询
// （Node 侧 src/app/api/availability/current/route.ts → src/lib/availability/availability-service.ts:476
// getCurrentProviderStatus 的两级数据源）。
//
// 为什么单独一个文件：这三条只服务根级 /api/availability/* 三条端点，与 /api/v1 的资源读面
// 没有共用列形状（这里的列名是 camelCase 直出，不做 providerEndpointColumns 那套投影）。

// AdminProviderNameRef 是启用供应商的 id/name 对（Node 的 providerList）。
type AdminProviderNameRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// AdminListEnabledProviderNames 复刻 getCurrentProviderStatus 的供应商列表查询。
//
// 刻意**不加 ORDER BY**：Node 那条查询也没有（顺序由规划器决定）。加一个排序会让「同样的库、
// 同样的行」在两边的作答顺序不同，反而破坏对拍。
func (p *Pools) AdminListEnabledProviderNames(ctx context.Context) ([]AdminProviderNameRef, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT id, name FROM providers WHERE is_enabled = true AND deleted_at IS NULL
	) t`
	return adminEndpointResourceRows[AdminProviderNameRef](ctx, pool, query)
}

// AdminAvailabilityCurrent 是 avail_current 的一行（getCurrentProviderStatus 的 CurrentRow）。
type AdminAvailabilityCurrent struct {
	ProviderID    int64     `json:"providerId"`
	State         string    `json:"state"`
	Availability  float64   `json:"availability"`
	RequestCount  int64     `json:"requestCount"`
	LastRequestAt time.Time `json:"lastRequestAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// AdminListAvailabilityCurrent 复刻那条 `WHERE provider_id IN (...)` 的 avail_current 读。
//
// 用 `= ANY($1)` 而不是拼 IN 列表：参数化占位符由驱动生成，id 不进 SQL 文本。
func (p *Pools) AdminListAvailabilityCurrent(
	ctx context.Context,
	providerIDs []int64,
) ([]AdminAvailabilityCurrent, error) {
	if len(providerIDs) == 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	// COALESCE 把可空的 last_request_at 折成零值时间：Node 侧由 getTimeValue 把 null 当 0 处理，
	// Go 的 time.Time 无法表达 null，故在 SQL 里先把 null 变成「纪元前哨」再统一按零值走。
	query := `SELECT row_to_json(t)::text FROM (
		SELECT provider_id AS "providerId", state, availability,
		       request_count AS "requestCount",
		       COALESCE(last_request_at, '0001-01-01T00:00:00Z'::timestamptz) AS "lastRequestAt",
		       updated_at AS "updatedAt"
		FROM avail_current
		WHERE provider_id = ANY($1)
	) t`
	return adminEndpointResourceRows[AdminAvailabilityCurrent](ctx, pool, query, providerIDs)
}

// AdminAvailabilityBucketRow 是**按展示桶宽聚合**后的一行（Node 的 AggregatedAvailabilityBucketRow）。
//
// p50/p95/p99 与 avg 同值：1 分钟投影桶只有 sum/count，Node 侧也把三个分位当均值近似
// （availability-service.ts 的 SQL 里三条 CASE 逐字相同），字段名保留是为了 API 兼容。
type AdminAvailabilityBucketRow struct {
	ProviderID    int64     `json:"providerId"`
	BucketStart   time.Time `json:"bucketStart"`
	GreenCount    int64     `json:"greenCount"`
	RedCount      int64     `json:"redCount"`
	LatencyCount  int64     `json:"latencyCount"`
	LatencySumMS  float64   `json:"latencySumMs"`
	AvgLatencyMS  float64   `json:"avgLatencyMs"`
	P50LatencyMS  float64   `json:"p50LatencyMs"`
	P95LatencyMS  float64   `json:"p95LatencyMs"`
	P99LatencyMS  float64   `json:"p99LatencyMs"`
	LastRequestAt time.Time `json:"lastRequestAt"`
}

// AdminListAvailabilityBuckets 复刻 queryProviderAvailability 的那条 CTE 查询
// （availability-service.ts:queryProviderAvailability）：先把 1 分钟投影桶 date_bin 成展示桶，
// 再按 provider 取**最新 maxBuckets 个非空桶**（ROW_NUMBER 降序 + rn <= maxBuckets），
// 最后按 (providerId, bucketStart) 升序出。
//
// 三个 IN 列表都走 `= ANY($n)`；桶宽走参数而不是拼进 SQL 文本。
// `GROUP BY provider_id, 2` 沿用 Node 的位置编号（第 2 列是 date_bin 的结果）。
func (p *Pools) AdminListAvailabilityBuckets(
	ctx context.Context,
	providerIDs []int64,
	bucketSizeMinutes float64,
	rangeStart time.Time,
	rangeEnd time.Time,
	maxBuckets int,
) ([]AdminAvailabilityBucketRow, error) {
	if len(providerIDs) == 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	// p50/p95/p99 与 avgLatencyMs 三条 CASE 在 Node 里逐字相同，这里也逐字保留（不做"去重"改写）：
	// 它们是三个独立字段的口径，将来换成 sketch 分位时只有这三条会变。
	query := `
	SELECT row_to_json(t)::text FROM (
	WITH provider_bucket_stats AS (
		SELECT
			provider_id AS "providerId",
			date_bin(
				($2::double precision * INTERVAL '1 minute'),
				bucket_start,
				TIMESTAMPTZ '1970-01-01T00:00:00Z'
			) AS "bucketStart",
			SUM(success_cnt)::int AS "greenCount",
			SUM(failure_cnt)::int AS "redCount",
			SUM(latency_cnt)::int AS "latencyCount",
			COALESCE(SUM(latency_sum_ms), 0)::double precision AS "latencySumMs",
			CASE
				WHEN SUM(latency_cnt) > 0 THEN (SUM(latency_sum_ms)::double precision / SUM(latency_cnt))
				ELSE 0
			END AS "avgLatencyMs",
			CASE
				WHEN SUM(latency_cnt) > 0 THEN (SUM(latency_sum_ms)::double precision / SUM(latency_cnt))
				ELSE 0
			END AS "p50LatencyMs",
			CASE
				WHEN SUM(latency_cnt) > 0 THEN (SUM(latency_sum_ms)::double precision / SUM(latency_cnt))
				ELSE 0
			END AS "p95LatencyMs",
			CASE
				WHEN SUM(latency_cnt) > 0 THEN (SUM(latency_sum_ms)::double precision / SUM(latency_cnt))
				ELSE 0
			END AS "p99LatencyMs",
			COALESCE(MAX(last_request_at), '0001-01-01T00:00:00Z'::timestamptz) AS "lastRequestAt"
		FROM avail_bucket_1m
		WHERE provider_id = ANY($1)
			AND bucket_start >= $3::timestamptz
			AND bucket_start <= $4::timestamptz
		GROUP BY provider_id, 2
	),
	limited_provider_bucket_stats AS (
		SELECT
			*,
			ROW_NUMBER() OVER (PARTITION BY "providerId" ORDER BY "bucketStart" DESC) AS rn
		FROM provider_bucket_stats
	)
	SELECT
		"providerId",
		"bucketStart",
		"greenCount",
		"redCount",
		"latencyCount",
		"latencySumMs",
		"avgLatencyMs",
		"p50LatencyMs",
		"p95LatencyMs",
		"p99LatencyMs",
		"lastRequestAt"
	FROM limited_provider_bucket_stats
	WHERE rn <= $5
	ORDER BY "providerId" ASC, "bucketStart" ASC
	) t`
	rows, err := adminEndpointResourceRows[AdminAvailabilityBucketRow](
		ctx, pool, query, providerIDs, bucketSizeMinutes, rangeStart, rangeEnd, maxBuckets)
	if err != nil {
		return nil, fmt.Errorf("store: 查询可用性展示桶失败: %w", err)
	}
	return rows, nil
}

// AdminAvailabilityProviderRef 是分桶聚合的供应商行（Node 的 providerList 项）。
//
// 与 AdminProviderNameRef 的关系：那一条只取启用供应商（/current 用），这一条的启用与否可配
// （includeDisabled），且要带类型与启用位供作答。
type AdminAvailabilityProviderRef struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	ProviderType *string `json:"providerType"`
	Enabled      *bool   `json:"enabled"`
}

// AdminListAvailabilityProviders 复刻 queryProviderAvailability 的供应商列表查询：
// `deleted_at IS NULL` 恒有；`includeDisabled` 为假才加 `is_enabled = true`；给了 id 列表才加 `id = ANY`。
//
// 刻意不加 ORDER BY（Node 那条也没有），顺序由规划器决定。
func (p *Pools) AdminListAvailabilityProviders(
	ctx context.Context,
	providerIDs []int64,
	includeDisabled bool,
) ([]AdminAvailabilityProviderRef, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT id, name, provider_type AS "providerType", is_enabled AS "enabled"
		FROM providers
		WHERE deleted_at IS NULL
		  AND ($1::boolean OR is_enabled = true)
		  AND (COALESCE(cardinality($2::bigint[]), 0) = 0 OR id = ANY($2))
	) t`
	rows, err := adminEndpointResourceRows[AdminAvailabilityProviderRef](
		ctx, pool, query, includeDisabled, providerIDs)
	if err != nil {
		return nil, fmt.Errorf("store: 查询可用性供应商列表失败: %w", err)
	}
	return rows, nil
}

// AdminAvailabilityBucketSummary 是 1 分钟桶的窗口汇总（Node 的回落查询结果）。
type AdminAvailabilityBucketSummary struct {
	ProviderID    int64     `json:"providerId"`
	GreenCount    int64     `json:"greenCount"`
	RedCount      int64     `json:"redCount"`
	LastRequestAt time.Time `json:"lastRequestAt"`
}

// AdminListAvailabilityBucketSummary 复刻缺失供应商的 1 分钟桶回落查询。
//
// 窗口表达式与 Node 逐字同形：`NOW() - (N * INTERVAL '1 minute')`。窗口分钟数取参数而不是拼
// 字符串，避免把常量写死在 SQL 里（Node 用的是 CURRENT_PROVIDER_STATUS_WINDOW_MINUTES 常量）。
func (p *Pools) AdminListAvailabilityBucketSummary(
	ctx context.Context,
	providerIDs []int64,
	windowMinutes int,
) ([]AdminAvailabilityBucketSummary, error) {
	if len(providerIDs) == 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT provider_id AS "providerId",
		       SUM(success_cnt)::int AS "greenCount",
		       SUM(failure_cnt)::int AS "redCount",
		       COALESCE(MAX(last_request_at), '0001-01-01T00:00:00Z'::timestamptz) AS "lastRequestAt"
		FROM avail_bucket_1m
		WHERE provider_id = ANY($1)
		  AND bucket_start >= NOW() - ($2 * INTERVAL '1 minute')
		GROUP BY provider_id
	) t`
	rows, err := adminEndpointResourceRows[AdminAvailabilityBucketSummary](
		ctx, pool, query, providerIDs, windowMinutes)
	if err != nil {
		return nil, fmt.Errorf("store: 查询可用性桶汇总失败: %w", err)
	}
	return rows, nil
}
