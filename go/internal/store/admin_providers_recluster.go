package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// 本文件是 **供应商→厂商重挂（recluster）** 的存储层：Node 侧 `reclusterProviderVendors`
// （src/actions/providers.ts:5748-5911）在 apply 分支里依次做三件事——
// ①逐条 getOrCreateProviderVendorIdFromUrls + 回写 provider_vendor_id；
// ②`backfillProviderEndpointsFromProviders()`（仅 apply 语义，见下）；
// ③对旧 vendor 逐个 tryDeleteProviderVendorIfEmpty。
//
// 为什么单列文件而不是改现有函数：`adminEnsureVendorTx` / `adminEnsureEndpointTx` 是
// provider 写路径的既有原语（同包可直接复用，避免第二份漂移源），本文件只把「按域名重挂」
// 与「端点回填」两件事包成可调用入口。

// ProviderReclusterRow 是 recluster 需要的最小供应商行视图。
//
// 口径：**未软删**（Node 的 findAllProvidersFresh，含禁用行）——与 AdminListProviders 同判，
// 但不复用其完整列（recluster 只读 6 列，避免把 60+ 列拉进内存）。
type ProviderReclusterRow struct {
	ID               int64
	Name             string
	URL              string
	WebsiteURL       *string
	ProviderType     string
	ProviderVendorID *int64
}

// AdminListProvidersForRecluster 读全部未软删供应商（按 id 升序，保证 changes 顺序稳定）。
func (p *Pools) AdminListProvidersForRecluster(ctx context.Context) ([]ProviderReclusterRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT id, name, url, website_url, provider_type, provider_vendor_id
		FROM providers WHERE deleted_at IS NULL ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 读供应商重挂行失败: %w", err)
	}
	defer rows.Close()
	result := make([]ProviderReclusterRow, 0, 64)
	for rows.Next() {
		var row ProviderReclusterRow
		if err := rows.Scan(&row.ID, &row.Name, &row.URL, &row.WebsiteURL,
			&row.ProviderType, &row.ProviderVendorID); err != nil {
			return nil, fmt.Errorf("store: 读供应商重挂行失败: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商重挂行失败: %w", err)
	}
	return result, nil
}

// AdminProviderVendorDomainsByIDs 取一批厂商 id 的 website_domain（Node 的
// findProviderVendorsByIds 在 recluster 里只用来读 currentDomain）。
func (p *Pools) AdminProviderVendorDomainsByIDs(
	ctx context.Context,
	vendorIDs []int64,
) (map[int64]string, error) {
	result := make(map[int64]string, len(vendorIDs))
	if len(vendorIDs) == 0 {
		return result, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx,
		`SELECT id, website_domain FROM provider_vendors WHERE id = ANY($1)`, vendorIDs)
	if err != nil {
		return nil, fmt.Errorf("store: 读厂商域名失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var domain string
		if err := rows.Scan(&id, &domain); err != nil {
			return nil, fmt.Errorf("store: 读厂商域名失败: %w", err)
		}
		result[id] = domain
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历厂商域名失败: %w", err)
	}
	return result, nil
}

// AdminReassignProviderVendors 把给定供应商按「providerID → 厂商域名」重挂到对应厂商
// （需要时建厂商行），**单事务**内完成；返回实际改动的行数。
//
// 与 Node 的逐条 `getOrCreateProviderVendorIdFromUrls({providerUrl, websiteUrl}, {tx})` 同判：
// 只带 website_url（不带 favicon/displayName），已存在厂商时只补空字段。
//
// assignments 的遍历顺序固定为 id 升序：并发建厂商时靠唯一索引兜底，但顺序固定后
// 冲突路径可复现（避免 flaky）。
func (p *Pools) AdminReassignProviderVendors(
	ctx context.Context,
	assignments map[int64]string,
) (int64, error) {
	if len(assignments) == 0 {
		return 0, nil
	}
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	rows, err := pool.Query(ctx, `
		SELECT id, website_url FROM providers
		WHERE id = ANY($1) AND deleted_at IS NULL`, idKeys(assignments))
	if err != nil {
		return 0, fmt.Errorf("store: 读供应商重挂输入失败: %w", err)
	}
	websiteURLs := make(map[int64]*string, len(assignments))
	for rows.Next() {
		var id int64
		var websiteURL *string
		if err := rows.Scan(&id, &websiteURL); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: 读供应商重挂输入失败: %w", err)
		}
		websiteURLs[id] = websiteURL
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: 遍历供应商重挂输入失败: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: 开启重挂事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var moved int64
	for _, providerID := range idKeys(assignments) {
		websiteURL, present := websiteURLs[providerID]
		if !present {
			// 事务外读到的行在此期间被删除：Node 的 providerMap 同样会跳过。
			continue
		}
		vendorID, err := adminEnsureVendorTx(ctx, tx, assignments[providerID], map[string]any{
			"website_url": websiteURL,
		})
		if err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE providers SET provider_vendor_id = $2, updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL`, providerID, vendorID)
		if err != nil {
			return 0, fmt.Errorf("store: 重挂供应商失败: %w", err)
		}
		moved += tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: 提交重挂事务失败: %w", err)
	}
	return moved, nil
}

// AdminBackfillProviderEndpointsFromProviders 为**启用且未软删**的供应商确保端点行存在
// （Node 的 backfillProviderEndpointsFromProviders 的 apply 语义，provider-endpoints.ts:1705）。
//
// 为什么只做 apply：Node 那个函数带一套「候选键 → 活跃端点差集 → 历史软删端点」的
// **报表机制**（risk/sample 分类），recluster 调用时不传 options（mode=apply）且丢弃 summary；
// 真正的写副作用就是「为每个 (vendor_id, provider_type, trimmed url) 确保活跃端点行」，
// 而 `ON CONFLICT (vendor_id, provider_type, url) WHERE deleted_at IS NULL DO NOTHING`
// 正是该语义（唯一索引 uniq_provider_endpoints_vendor_type_url 同名）。
//
// 分批：与 Node 同形按 id 游标分页（pageSize 1000），**每页一条批量 INSERT**（Node 的
// insertBatchSize 500；这里按页整批，减少往返而不改变结果）。
//
// 返回尝试确保的行数（不是插入数）：Node 的 inserted/skipped 统计被调用方丢弃，故不在此复刻。
func (p *Pools) AdminBackfillProviderEndpointsFromProviders(ctx context.Context) (int64, error) {
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	const pageSize = 1000
	lastID := int64(0)
	var ensured int64
	for {
		rows, err := pool.Query(ctx, `
			SELECT id, provider_vendor_id, provider_type, url FROM providers
			WHERE deleted_at IS NULL AND is_enabled = true AND id > $1
			ORDER BY id ASC LIMIT $2`, lastID, pageSize)
		if err != nil {
			return ensured, fmt.Errorf("store: 读端点回填候选失败: %w", err)
		}
		type candidate struct {
			id           int64
			vendorID     *int64
			providerType string
			url          string
		}
		batch := make([]candidate, 0, pageSize)
		for rows.Next() {
			var row candidate
			if err := rows.Scan(&row.id, &row.vendorID, &row.providerType, &row.url); err != nil {
				rows.Close()
				return ensured, fmt.Errorf("store: 读端点回填候选失败: %w", err)
			}
			batch = append(batch, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return ensured, fmt.Errorf("store: 遍历端点回填候选失败: %w", err)
		}
		if len(batch) == 0 {
			return ensured, nil
		}

		// 与 Node 的两条 skippedInvalid 对应：vendor 无效或 URL 为空即不参与。
		vendorIDs := make([]int64, 0, len(batch))
		providerTypes := make([]string, 0, len(batch))
		urls := make([]string, 0, len(batch))
		for _, row := range batch {
			lastID = row.id
			trimmed := strings.TrimSpace(row.url)
			if row.vendorID == nil || *row.vendorID <= 0 || trimmed == "" {
				continue
			}
			vendorIDs = append(vendorIDs, *row.vendorID)
			providerTypes = append(providerTypes, row.providerType)
			urls = append(urls, trimmed)
		}
		if len(vendorIDs) > 0 {
			if _, err := pool.Exec(ctx, `
				INSERT INTO provider_endpoints (vendor_id, provider_type, url, updated_at)
				SELECT data.vendor_id, data.provider_type, data.url, now()
				FROM unnest($1::bigint[], $2::text[], $3::text[])
					AS data(vendor_id, provider_type, url)
				ON CONFLICT (vendor_id, provider_type, url)
					WHERE deleted_at IS NULL DO NOTHING`,
				vendorIDs, providerTypes, urls); err != nil {
				return ensured, fmt.Errorf("store: 回填端点行失败: %w", err)
			}
			ensured += int64(len(vendorIDs))
		}
		if len(batch) < pageSize {
			return ensured, nil
		}
	}
}

// idKeys 返回 map 的键并按升序排列。
func idKeys(values map[int64]string) []int64 {
	keys := make([]int64, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
