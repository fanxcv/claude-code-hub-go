package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// 本文件是 provider_vendors / provider_endpoints 的**管理面写路径**
// （Node 侧 src/repository/provider-endpoints.ts 的 updateProviderVendor / deleteProviderVendor /
// createProviderEndpoint / updateProviderEndpoint / softDeleteProviderEndpoint /
// tryDeleteProviderVendorIfEmpty，以及 src/repository/provider-endpoints-batch.ts 的两个批量查询、
// src/repository/provider-endpoints.ts:718-774 的两条「被启用供应商引用」查询）。
//
// 与读面（admin_provider_endpoints.go）分开的理由：读面只依赖 Control 分道，写面要落 Writer 分道
// 与事务；两者装不到一起时能各自降级。
//
// 三条与 Node 逐字对齐的语义：
//  1. **唯一性冲突靠库判定**：`uniq_provider_endpoints_vendor_type_url`（partial，deleted_at IS NULL）
//     是唯一真源。Go 侧不先 SELECT 再 INSERT——那是两处真源，且并发下会输给索引。
//     冲突按 pg 的 23505 分类（Node 的 isDirectEndpointEditConflictError 同样认 "23505"）。
//  2. **外键违约按 23503 分类**（Node 的 isForeignKeyViolationError），端点所属厂不存在时用它。
//  3. **软删过滤一致**：端点的每条写路径都带 `deleted_at IS NULL`，与读面同口径。
//
// 一处刻意不移植：Node updateProviderEndpoint 的 `readConsistentProviderEndpointAfterWrite`
// （写后读一致性重试）。那条重试是为对付主从读写分离的延迟；本实现写与回读都在 Writer 分道的
// 同一条连接上，读到旧值的窗口不存在。登记为差异。

// ErrProviderEndpointConflict 表示端点 URL 在 (vendor, type) 下已存在（pg 23505）。
var ErrProviderEndpointConflict = errors.New("store: 供应商端点 URL 冲突")

// ErrProviderVendorMissing 表示端点写入时所属厂不存在（pg 23503）。
var ErrProviderVendorMissing = errors.New("store: 供应商不存在")

// classifyProviderEndpointWriteError 把 pg 的约束违约翻成 store 的两个哨兵错误。
//
// 只翻这两个码：其余错误（连接断、超时）必须原样冒到调用方，否则管理面会把库故障报成 4xx。
func classifyProviderEndpointWriteError(err error, action string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return fmt.Errorf("%s: %w", action, ErrProviderEndpointConflict)
		case "23503":
			return fmt.Errorf("%s: %w", action, ErrProviderVendorMissing)
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}

// AdminProviderVendorPatch 是厂更新的字段袋。
//
// 三个字段都是 store.NullableString（Set 复刻 Node 的 `!== undefined`，Value=nil 复刻 `null`）：
// 单指针区分不了「不改」与「清空」，而 Node 的 schema 两者都要表达。
type AdminProviderVendorPatch struct {
	DisplayName NullableString
	WebsiteURL  NullableString
	FaviconURL  NullableString
}

// AdminUpdateProviderVendor 复刻 updateProviderVendor：只写给出的字段，返回更新后的行。
func (p *Pools) AdminUpdateProviderVendor(
	ctx context.Context,
	vendorID int64,
	patch AdminProviderVendorPatch,
) (*AdminProviderVendor, error) {
	assignments := make([]string, 0, 4)
	values := make([]any, 0, 4)
	if patch.DisplayName.Set {
		assignments = append(assignments, fmt.Sprintf("display_name = $%d", len(values)+1))
		values = append(values, patch.DisplayName.Value)
	}
	if patch.WebsiteURL.Set {
		assignments = append(assignments, fmt.Sprintf("website_url = $%d", len(values)+1))
		values = append(values, patch.WebsiteURL.Value)
	}
	if patch.FaviconURL.Set {
		assignments = append(assignments, fmt.Sprintf("favicon_url = $%d", len(values)+1))
		values = append(values, patch.FaviconURL.Value)
	}
	if len(assignments) == 0 {
		// Node 对空 payload 直接返回当前行（updateProviderVendor:865-867），不写 updated_at。
		return p.AdminGetProviderVendorByID(ctx, vendorID)
	}

	pool, err := p.Writer()
	if err != nil {
		return nil, err
	}
	assignments = append(assignments, "updated_at = now()")
	// UPDATE ... RETURNING 必须包在 WITH 里：PostgreSQL 不接受 FROM (UPDATE ... RETURNING *) 子查询
	// （实测报 syntax error at or near "SET"）。
	query := `WITH t AS (
		UPDATE provider_vendors SET ` + strings.Join(assignments, ", ") + `
		WHERE id = $` + fmt.Sprint(len(values)+1) + `
		RETURNING ` + providerVendorColumns + `
	) SELECT row_to_json(t)::text FROM t`

	rows, err := adminEndpointResourceRows[AdminProviderVendor](ctx, pool, query, append(values, vendorID)...)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

// AdminDeleteProviderVendor 复刻 deleteProviderVendor：一个事务里删端点、删供应商、删厂。
//
// providers 要显式删是因为 FK 是 onDelete: restrict（Node :901 的同一条注释）；端点本可由
// cascade 带走，仍显式删一次以对齐 Node 的顺序（先端点、后供应商、最后厂）。
func (p *Pools) AdminDeleteProviderVendor(ctx context.Context, vendorID int64) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: 删除供应商事务开启失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM provider_endpoints WHERE vendor_id = $1`, vendorID); err != nil {
		return false, fmt.Errorf("store: 删除厂下端点失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM providers WHERE provider_vendor_id = $1`, vendorID); err != nil {
		return false, fmt.Errorf("store: 删除厂下供应商失败: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM provider_vendors WHERE id = $1`, vendorID)
	if err != nil {
		return false, fmt.Errorf("store: 删除供应商厂失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: 删除供应商厂提交失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AdminDeleteProviderVendorIfEmpty 复刻 tryDeleteProviderVendorIfEmpty：厂里没有活着的
// 供应商与端点时硬删（先清掉软删供应商以满足 FK restrict）。
//
// 返回 true 表示这次确实删掉了厂。
func (p *Pools) AdminDeleteProviderVendorIfEmpty(ctx context.Context, vendorID int64) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: 清理空厂事务开启失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var hasActive bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM providers WHERE provider_vendor_id = $1 AND deleted_at IS NULL)`,
		vendorID).Scan(&hasActive); err != nil {
		return false, fmt.Errorf("store: 探测厂下活跃供应商失败: %w", err)
	}
	if hasActive {
		return false, nil
	}
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM provider_endpoints WHERE vendor_id = $1 AND deleted_at IS NULL)`,
		vendorID).Scan(&hasActive); err != nil {
		return false, fmt.Errorf("store: 探测厂下活跃端点失败: %w", err)
	}
	if hasActive {
		return false, nil
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM providers WHERE provider_vendor_id = $1 AND deleted_at IS NOT NULL`,
		vendorID); err != nil {
		return false, fmt.Errorf("store: 清理软删供应商失败: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM provider_vendors WHERE id = $1
		   AND NOT EXISTS (SELECT 1 FROM providers p
		                   WHERE p.provider_vendor_id = $1 AND p.deleted_at IS NULL)`,
		vendorID)
	if err != nil {
		return false, fmt.Errorf("store: 清理空厂失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: 清理空厂提交失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AdminProviderEndpointCreateInput 是一次端点创建的入参（Node createProviderEndpoint 的字段）。
type AdminProviderEndpointCreateInput struct {
	VendorID     int64
	ProviderType string
	URL          string
	Label        *string
	SortOrder    int
	IsEnabled    bool
}

// AdminCreateProviderEndpoint 复刻 createProviderEndpoint：插入一行并回读完整资源视图。
func (p *Pools) AdminCreateProviderEndpoint(
	ctx context.Context,
	in AdminProviderEndpointCreateInput,
) (*AdminProviderEndpoint, error) {
	pool, err := p.Writer()
	if err != nil {
		return nil, err
	}
	query := `WITH t AS (
		INSERT INTO provider_endpoints
			(vendor_id, provider_type, url, label, sort_order, is_enabled, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		RETURNING ` + providerEndpointColumns + `
	) SELECT row_to_json(t)::text FROM t`

	rows, err := adminEndpointResourceRows[AdminProviderEndpoint](
		ctx, pool, query,
		in.VendorID, in.ProviderType, in.URL, in.Label, in.SortOrder, in.IsEnabled,
	)
	if err != nil {
		return nil, classifyProviderEndpointWriteError(err, "store: 创建端点失败")
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

// AdminProviderEndpointPatch 是端点更新的字段袋。
//
// Label 用 NullableString（label 可空，要区分「不改」与「清空」）；其余三个字段在 Node 的
// schema 里不可为 null，单指针即可表达「不改」。
type AdminProviderEndpointPatch struct {
	URL       *string
	Label     NullableString
	SortOrder *int
	IsEnabled *bool
}

// AdminUpdateProviderEndpoint 复刻 updateProviderEndpoint：只写给出的字段并回读。
func (p *Pools) AdminUpdateProviderEndpoint(
	ctx context.Context,
	endpointID int64,
	patch AdminProviderEndpointPatch,
) (*AdminProviderEndpoint, error) {
	assignments := make([]string, 0, 5)
	values := make([]any, 0, 5)
	if patch.URL != nil {
		assignments = append(assignments, fmt.Sprintf("url = $%d", len(values)+1))
		values = append(values, *patch.URL)
	}
	if patch.Label.Set {
		assignments = append(assignments, fmt.Sprintf("label = $%d", len(values)+1))
		values = append(values, patch.Label.Value)
	}
	if patch.SortOrder != nil {
		assignments = append(assignments, fmt.Sprintf("sort_order = $%d", len(values)+1))
		values = append(values, *patch.SortOrder)
	}
	if patch.IsEnabled != nil {
		assignments = append(assignments, fmt.Sprintf("is_enabled = $%d", len(values)+1))
		values = append(values, *patch.IsEnabled)
	}
	if len(assignments) == 0 {
		return p.AdminGetProviderEndpointByID(ctx, endpointID)
	}

	pool, err := p.Writer()
	if err != nil {
		return nil, err
	}
	assignments = append(assignments, "updated_at = now()")
	query := `WITH t AS (
		UPDATE provider_endpoints SET ` + strings.Join(assignments, ", ") + `
		WHERE id = $` + fmt.Sprint(len(values)+1) + ` AND deleted_at IS NULL
		RETURNING ` + providerEndpointColumns + `
	) SELECT row_to_json(t)::text FROM t`

	rows, err := adminEndpointResourceRows[AdminProviderEndpoint](
		ctx, pool, query, append(values, endpointID)...,
	)
	if err != nil {
		return nil, classifyProviderEndpointWriteError(err, "store: 更新端点失败")
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

// AdminSoftDeleteProviderEndpoint 复刻 softDeleteProviderEndpoint：置 deleted_at、停用、回读是否命中。
func (p *Pools) AdminSoftDeleteProviderEndpoint(ctx context.Context, endpointID int64) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx,
		`UPDATE provider_endpoints
		    SET deleted_at = now(), is_enabled = false, updated_at = now()
		  WHERE id = $1 AND deleted_at IS NULL`,
		endpointID)
	if err != nil {
		return false, fmt.Errorf("store: 软删端点失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AdminProviderReference 是被启用的供应商对某个 (vendor, type, url) 的引用（只取展示用两列）。
type AdminProviderReference struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// AdminFindEnabledProviderReferencesForVendorTypeURL 复刻
// findEnabledProviderReferencesForVendorTypeUrl：端点删除前的引用检查。
func (p *Pools) AdminFindEnabledProviderReferencesForVendorTypeURL(
	ctx context.Context,
	vendorID int64,
	providerType string,
	rawURL string,
) ([]AdminProviderReference, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT id, name FROM providers
		 WHERE provider_vendor_id = $1 AND provider_type = $2 AND url = $3
		   AND is_enabled = true AND deleted_at IS NULL
		 ORDER BY id ASC
	) t`
	return adminEndpointResourceRows[AdminProviderReference](ctx, pool, query, vendorID, providerType, trimmed)
}

// AdminFindEnabledProviderIDsByVendorAndType 复刻 findEnabledProviderIdsByVendorAndType。
func (p *Pools) AdminFindEnabledProviderIDsByVendorAndType(
	ctx context.Context,
	vendorID int64,
	providerType string,
) ([]int64, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx,
		`SELECT id FROM providers
		  WHERE provider_vendor_id = $1 AND provider_type = $2
		    AND is_enabled = true AND deleted_at IS NULL
		  ORDER BY id ASC`,
		vendorID, providerType)
	if err != nil {
		return nil, fmt.Errorf("store: 查询厂下启用供应商失败: %w", err)
	}
	defer rows.Close()

	ids := make([]int64, 0, 8)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: 读取厂下启用供应商失败: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历厂下启用供应商失败: %w", err)
	}
	return ids, nil
}

// AdminVendorTypeEndpointStats 是某厂某类型的端点统计（Node VendorTypeEndpointStats）。
type AdminVendorTypeEndpointStats struct {
	VendorID  int64 `json:"vendorId"`
	Total     int64 `json:"total"`
	Enabled   int64 `json:"enabled"`
	Healthy   int64 `json:"healthy"`
	Unhealthy int64 `json:"unhealthy"`
	Unknown   int64 `json:"unknown"`
}

// AdminVendorTypeEndpointStatsBatch 复刻 findVendorTypeEndpointStatsBatch：一次按厂聚合。
func (p *Pools) AdminVendorTypeEndpointStatsBatch(
	ctx context.Context,
	vendorIDs []int64,
	providerType string,
) ([]AdminVendorTypeEndpointStats, error) {
	if len(vendorIDs) == 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT vendor_id AS "vendorId",
		       COUNT(*)::int AS total,
		       (COUNT(*) FILTER (WHERE is_enabled = true))::int AS enabled,
		       (COUNT(*) FILTER (WHERE is_enabled = true AND last_probe_ok = true))::int AS healthy,
		       (COUNT(*) FILTER (WHERE is_enabled = true AND last_probe_ok = false))::int AS unhealthy,
		       (COUNT(*) FILTER (WHERE is_enabled = true AND last_probe_ok IS NULL))::int AS unknown
		FROM provider_endpoints
		WHERE vendor_id = ANY($1) AND provider_type = $2 AND deleted_at IS NULL
		GROUP BY vendor_id
	) t`
	return adminEndpointResourceRows[AdminVendorTypeEndpointStats](ctx, pool, query, vendorIDs, providerType)
}

// AdminProviderEndpointProbeLogsBatch 复刻 findProviderEndpointProbeLogsBatch：每个端点取最新 N 条。
//
// 用 LATERAL + LIMIT 而不是窗口函数：端点日志多时窗口函数会退化成分区重扫（Node 的同一条注释，
// src/repository/provider-endpoints-batch.ts:96-100）。入参用 unnest 而不是拼 VALUES：参数化占位符
// 由驱动生成，不把 id 拼进 SQL 文本。
func (p *Pools) AdminProviderEndpointProbeLogsBatch(
	ctx context.Context,
	endpointIDs []int64,
	limitPerEndpoint int,
) (map[int64][]AdminProviderEndpointProbeLog, error) {
	if len(endpointIDs) == 0 {
		return map[int64][]AdminProviderEndpointProbeLog{}, nil
	}
	if limitPerEndpoint < 1 {
		limitPerEndpoint = 1
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT l.id, l.endpoint_id AS "endpointId", l.source, l.ok,
		       l.status_code AS "statusCode", l.latency_ms AS "latencyMs",
		       l.error_type AS "errorType", l.error_message AS "errorMessage",
		       to_char(l.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt"
		FROM unnest($1::bigint[]) AS e(endpoint_id)
		CROSS JOIN LATERAL (
			SELECT id, endpoint_id, source, ok, status_code, latency_ms,
			       error_type, error_message, created_at
			FROM provider_endpoint_probe_logs
			WHERE endpoint_id = e.endpoint_id
			ORDER BY created_at DESC NULLS LAST, id DESC
			LIMIT $2
		) l
		ORDER BY l.endpoint_id ASC, l.created_at DESC NULLS LAST, l.id DESC
	) t`

	rows, err := adminEndpointResourceRows[AdminProviderEndpointProbeLog](
		ctx, pool, query, endpointIDs, limitPerEndpoint,
	)
	if err != nil {
		return nil, err
	}
	result := make(map[int64][]AdminProviderEndpointProbeLog, len(endpointIDs))
	for _, row := range rows {
		result[row.EndpointID] = append(result[row.EndpointID], row)
	}
	// 防御：即使 SQL 变了也不越过每端点上限（Node 的同一道保险，batch.ts:160-166）。
	for id, logs := range result {
		if len(logs) > limitPerEndpoint {
			result[id] = logs[:limitPerEndpoint]
		}
	}
	return result, nil
}
