package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// 本文件是 provider_vendors / provider_endpoints / provider_endpoint_probe_logs 的**管理面只读**
// 查询（Node 侧 src/repository/provider-endpoints.ts 的 findProviderVendors / findProviderVendorById /
// findProviderEndpointsByVendor{,AndType} / findDashboardProviderEndpointsByVendorAndType /
// findProviderEndpointById / findProviderEndpointProbeLogs）。
//
// 列形状与 Node 的 drizzle select 逐字对齐（时间列渲染成 JS Date#toISOString 的 24 字符形状），
// 因为 /api/v1/provider-* 的响应正文会被 UI 直接渲染、也会被切流对拍逐字节比较。
//
// 数据面的热路径（findEnabledProviderEndpointsByVendorAndType）不在这里——它走 read.go 里带缓存的
// 那份实现。

// AdminProviderVendor 是 provider_vendors 的读取视图（Node 的 ProviderVendor）。
type AdminProviderVendor struct {
	ID            int64   `json:"id"`
	WebsiteDomain string  `json:"websiteDomain"`
	DisplayName   *string `json:"displayName"`
	WebsiteURL    *string `json:"websiteUrl"`
	FaviconURL    *string `json:"faviconUrl"`
	CreatedAt     *string `json:"createdAt"`
	UpdatedAt     *string `json:"updatedAt"`
}

// AdminProviderEndpoint 是 provider_endpoints 的读取视图（Node 的 ProviderEndpoint）。
//
// lastProbe* 五列是「上次探活结果」的快照，随探活更新；deletedAt 恒为 null（查询已过滤软删）。
type AdminProviderEndpoint struct {
	ID                    int64   `json:"id"`
	VendorID              int64   `json:"vendorId"`
	ProviderType          string  `json:"providerType"`
	URL                   string  `json:"url"`
	Label                 *string `json:"label"`
	SortOrder             int     `json:"sortOrder"`
	IsEnabled             bool    `json:"isEnabled"`
	LastProbedAt          *string `json:"lastProbedAt"`
	LastProbeOK           *bool   `json:"lastProbeOk"`
	LastProbeStatusCode   *int    `json:"lastProbeStatusCode"`
	LastProbeLatencyMS    *int    `json:"lastProbeLatencyMs"`
	LastProbeErrorType    *string `json:"lastProbeErrorType"`
	LastProbeErrorMessage *string `json:"lastProbeErrorMessage"`
	CreatedAt             *string `json:"createdAt"`
	UpdatedAt             *string `json:"updatedAt"`
	DeletedAt             *string `json:"deletedAt"`
}

// AdminProviderEndpointProbeLog 是 provider_endpoint_probe_logs 的读取视图。
type AdminProviderEndpointProbeLog struct {
	ID           int64   `json:"id"`
	EndpointID   int64   `json:"endpointId"`
	Source       string  `json:"source"`
	OK           bool    `json:"ok"`
	StatusCode   *int    `json:"statusCode"`
	LatencyMS    *int    `json:"latencyMs"`
	ErrorType    *string `json:"errorType"`
	ErrorMessage *string `json:"errorMessage"`
	CreatedAt    *string `json:"createdAt"`
}

// providerVendorColumns 与 Node 的 drizzle select 同列。
const providerVendorColumns = `
	id, website_domain AS "websiteDomain", display_name AS "displayName",
	website_url AS "websiteUrl", favicon_url AS "faviconUrl",
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt"`

// providerEndpointColumns 与 Node 的 drizzle select 同列。
const providerEndpointColumns = `
	id, vendor_id AS "vendorId", provider_type AS "providerType", url,
	label, sort_order AS "sortOrder", is_enabled AS "isEnabled",
	to_char(last_probed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "lastProbedAt",
	last_probe_ok AS "lastProbeOk", last_probe_status_code AS "lastProbeStatusCode",
	last_probe_latency_ms AS "lastProbeLatencyMs",
	last_probe_error_type AS "lastProbeErrorType",
	last_probe_error_message AS "lastProbeErrorMessage",
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt",
	NULL::text AS "deletedAt"`

// AdminListProviderVendors 复刻 findProviderVendors(limit, offset)：按 created_at 降序。
func (p *Pools) AdminListProviderVendors(ctx context.Context, limit, offset int) ([]AdminProviderVendor, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + providerVendorColumns +
		` FROM provider_vendors ORDER BY created_at DESC LIMIT $1 OFFSET $2) t`
	return adminEndpointResourceRows[AdminProviderVendor](ctx, pool, query, limit, offset)
}

// AdminProviderVendorTypePair 是「启用供应商声明的（vendorId, providerType）去重对」。
//
// 它是 dashboard 口径 vendor 列表的唯一数据源（Node 的 findEnabledProviderVendorTypePairs）：
// 只有**真有启用供应商**的 vendor 才该出现在「添加供应商」的表单里。
type AdminProviderVendorTypePair struct {
	VendorID     int64  `json:"vendorId"`
	ProviderType string `json:"providerType"`
}

// AdminFindEnabledProviderVendorTypePairs 复刻 findEnabledProviderVendorTypePairs。
//
// SQL 与 Node 的 drizzle 查询逐条对齐（and(isNull(deletedAt), eq(isEnabled, true),
// isNotNull(providerVendorId), gt(providerVendorId, 0))）：**`IS NOT NULL` 与 `> 0` 两条都要有**，
// 只写前者会把 vendor_id = 0 这种占位值也带进来。
//
// 不在这里过滤空 providerType / 非正 vendorId：Node 是在 SQL 之后做 map + filter 的，
// 同口径下沉到调用方（handler）做，便于与 Node 的输出逐条对拍。
func (p *Pools) AdminFindEnabledProviderVendorTypePairs(
	ctx context.Context,
) ([]AdminProviderVendorTypePair, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	// DISTINCT 与 ORDER BY 同列：Postgres 要求排序列出现在 DISTINCT 的选择列表里，两列都在，故合法。
	query := `SELECT row_to_json(t)::text FROM (
	    SELECT DISTINCT provider_vendor_id AS "vendorId", provider_type AS "providerType"
	      FROM providers
	     WHERE deleted_at IS NULL
	       AND is_enabled = true
	       AND provider_vendor_id IS NOT NULL
	       AND provider_vendor_id > 0
	     ORDER BY provider_vendor_id ASC, provider_type ASC) t`
	return adminEndpointResourceRows[AdminProviderVendorTypePair](ctx, pool, query)
}

// AdminListProviderVendorsByIDs 复刻 findProviderVendorsByIds：按 id 批量取 vendor，
// **按 created_at 降序**（Node 的 orderBy(desc(createdAt))）；id 去重且只留正整数。
//
// 为何单开一条批量查询而不循环 AdminGetProviderVendorByID：dashboard 分支每次只取「有启用
// 供应商的 vendor」，但**该端点是前端表单每 2 秒轮询一次**的（生产日志实测），逐 id 循环会把
// N 次往返放大成「每客户端每 2 秒 N 次」。单条 `= ANY($1)` 与 Node 的 inArray 同形且只一次往返。
func (p *Pools) AdminListProviderVendorsByIDs(
	ctx context.Context,
	vendorIDs []int64,
) ([]AdminProviderVendor, error) {
	ids := distinctPositiveIDs(vendorIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	// 并列时用 id 降序做确定性收敛（Postgres 对并列不保证顺序，Node 也一样；钉死一个顺序便于用例断言）。
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + providerVendorColumns +
		` FROM provider_vendors
		   WHERE id = ANY($1)
		   ORDER BY created_at DESC, id DESC) t`
	return adminEndpointResourceRows[AdminProviderVendor](ctx, pool, query, ids)
}

// distinctPositiveIDs 复刻 Node 的 `Array.from(new Set(ids)).filter(id => Number.isInteger(id) && id > 0)`，
// 并保持首次出现的顺序（与 Node 的 Set 插入序一致）。
func distinctPositiveIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

// AdminGetProviderVendorByID 复刻 findProviderVendorById；不存在时 ErrNotFound。
func (p *Pools) AdminGetProviderVendorByID(ctx context.Context, vendorID int64) (*AdminProviderVendor, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + providerVendorColumns +
		` FROM provider_vendors WHERE id = $1 LIMIT 1) t`
	rows, err := adminEndpointResourceRows[AdminProviderVendor](ctx, pool, query, vendorID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

// AdminListProviderEndpointsByVendor 复刻 findProviderEndpointsByVendor。
//
// 该函数没有 providerType 过滤：它服务「按厂看全部端点」这一读法（端点管理页的首屏）。
func (p *Pools) AdminListProviderEndpointsByVendor(
	ctx context.Context,
	vendorID int64,
) ([]AdminProviderEndpoint, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + providerEndpointColumns +
		` FROM provider_endpoints
		   WHERE vendor_id = $1 AND deleted_at IS NULL
		   ORDER BY sort_order ASC, id ASC) t`
	return adminEndpointResourceRows[AdminProviderEndpoint](ctx, pool, query, vendorID)
}

// AdminListProviderEndpointsByVendorAndType 复刻 findProviderEndpointsByVendorAndType。
func (p *Pools) AdminListProviderEndpointsByVendorAndType(
	ctx context.Context,
	vendorID int64,
	providerType string,
) ([]AdminProviderEndpoint, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + providerEndpointColumns +
		` FROM provider_endpoints
		   WHERE vendor_id = $1 AND provider_type = $2 AND deleted_at IS NULL
		   ORDER BY sort_order ASC, id ASC) t`
	return adminEndpointResourceRows[AdminProviderEndpoint](ctx, pool, query, vendorID, providerType)
}

// AdminListDashboardProviderEndpointsByVendorAndType 复刻同名的 dashboard 变体：多一条
// 「该 vendor+type 下存在启用态供应商」的存在性条件（没供应商就没什么可展示的）。
func (p *Pools) AdminListDashboardProviderEndpointsByVendorAndType(
	ctx context.Context,
	vendorID int64,
	providerType string,
) ([]AdminProviderEndpoint, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + providerEndpointColumns +
		` FROM provider_endpoints
		   WHERE vendor_id = $1 AND provider_type = $2 AND deleted_at IS NULL
		     AND EXISTS (SELECT 1 FROM providers p
		                 WHERE p.provider_vendor_id = $1 AND p.provider_type = $2
		                   AND p.is_enabled = true AND p.deleted_at IS NULL)
		   ORDER BY sort_order ASC, id ASC) t`
	return adminEndpointResourceRows[AdminProviderEndpoint](ctx, pool, query, vendorID, providerType)
}

// AdminGetProviderEndpointByID 复刻 findProviderEndpointById；不存在（含软删）时 ErrNotFound。
func (p *Pools) AdminGetProviderEndpointByID(
	ctx context.Context,
	endpointID int64,
) (*AdminProviderEndpoint, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + providerEndpointColumns +
		` FROM provider_endpoints WHERE id = $1 AND deleted_at IS NULL LIMIT 1) t`
	rows, err := adminEndpointResourceRows[AdminProviderEndpoint](ctx, pool, query, endpointID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

// AdminListProviderEndpointProbeLogs 复刻 findProviderEndpointProbeLogs（created_at 降序、
// NULLS LAST，再按 id 降序兜底同刻并列）。
func (p *Pools) AdminListProviderEndpointProbeLogs(
	ctx context.Context,
	endpointID int64,
	limit int,
	offset int,
) ([]AdminProviderEndpointProbeLog, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT id, endpoint_id AS "endpointId", source, ok,
		       status_code AS "statusCode", latency_ms AS "latencyMs",
		       error_type AS "errorType", error_message AS "errorMessage",
		       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt"
		FROM provider_endpoint_probe_logs
		WHERE endpoint_id = $1
		ORDER BY created_at DESC NULLS LAST, id DESC
		LIMIT $2 OFFSET $3) t`
	return adminEndpointResourceRows[AdminProviderEndpointProbeLog](ctx, pool, query, endpointID, limit, offset)
}

// adminProviderRows 是「row_to_json 文本行 -> 结构体」的通用读法（与其它 store 文件同形）。
func adminEndpointResourceRows[T any](
	ctx context.Context,
	pool *Pool,
	query string,
	args ...any,
) ([]T, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商端点资源失败: %w", err)
	}
	defer rows.Close()

	results := make([]T, 0, 8)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取供应商端点资源行失败: %w", err)
		}
		var item T
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			return nil, fmt.Errorf("store: 解析供应商端点资源行失败: %w", err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商端点资源失败: %w", err)
	}
	return results, nil
}

// AdminCloudVendorSummary 是「云端价格表 vendor 汇总」的一行（Node /api/prices/vendors 的降级分支）。
type AdminCloudVendorSummary struct {
	Vendor     string
	ModelCount int64
}

// AdminListCloudVendorSummaries 复刻 /api/prices/vendors 的降级查询：按 price_data->>'vendor'
// 统计去重模型数，计数降序；vendor 为空的行由调用方过滤（Node 在 map 之后就 filter）。
//
// 显示名与图标不在这里取：那是 vendor 展示表的职责（internal/jobs），SQL 只出数据。
func (p *Pools) AdminListCloudVendorSummaries(ctx context.Context) ([]AdminCloudVendorSummary, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT price_data->>'vendor' AS vendor, COUNT(DISTINCT model_name) AS count
		FROM model_prices
		WHERE price_data->>'vendor' IS NOT NULL
		GROUP BY price_data->>'vendor'
		ORDER BY COUNT(DISTINCT model_name) DESC
	) t`
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: 查询云端 vendor 汇总失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminCloudVendorSummary, 0, 8)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取云端 vendor 汇总行失败: %w", err)
		}
		var row struct {
			Vendor string `json:"vendor"`
			Count  int64  `json:"count"`
		}
		if err := json.Unmarshal([]byte(payload), &row); err != nil {
			return nil, fmt.Errorf("store: 解析云端 vendor 汇总行失败: %w", err)
		}
		results = append(results, AdminCloudVendorSummary{Vendor: row.Vendor, ModelCount: row.Count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历云端 vendor 汇总失败: %w", err)
	}
	return results, nil
}
