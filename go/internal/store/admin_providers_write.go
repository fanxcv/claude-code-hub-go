package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是 providers 的**写入面**（创建 / 部分更新 / 软删 / 恢复 + 厂商与端点行维护）。
//
// 唯一真源：
//   - 字段归一与列取值：src/repository/provider.ts 的 createProvider（:201-368）与 updateProvider（:649-...）
//   - 软删：同文件 deleteProvider（:1020-1083）；恢复：restoreProviderInTransaction（:155-196）与
//     restoreSoftDeletedEndpointForProvider（:66-150）
//   - 厂商与端点：src/repository/provider-endpoints.ts 的 getOrCreateProviderVendorIdFromUrls（:427-496）
//     与 ensureProviderEndpointExistsForUrl（:1103-1136）
//
// 为什么用「字段名 → 列」的规格表而不是给每个字段写一个函数：Node 侧的写路径本来就是
// `if (providerData.X !== undefined) dbData.x = ...` 的直线（60 余个字段），Go 侧若展开成 60 个
// 可选参数，调用点会先烂掉。规格表让「字段是否被给出」保持为**调用方的语义**
// （map 里没有这个键 = Node 的 undefined），这正是 PATCH 部分更新与撤销回写的共同需求。

// adminProviderValueKind 是列绑定类型。
type adminProviderValueKind int

const (
	// providerTextKind：非空 text/varchar（Go 值 string）。
	providerTextKind adminProviderValueKind = iota
	// providerNullableTextKind：可空 text/varchar（Go 值 *string）。
	providerNullableTextKind
	// providerIntKind：非空 int（Go 值 int64）。
	providerIntKind
	// providerNullableIntKind：可空 int（Go 值 *int64）。
	providerNullableIntKind
	// providerBoolKind：非空 bool。
	providerBoolKind
	// providerNumericKind：numeric（Go 值 *float64）；Node 侧走 toString()，但 PG 侧仍是数值列。
	providerNumericKind
	// providerJSONKind：jsonb（Go 值 json.RawMessage；nil 写 NULL）。
	providerJSONKind
)

// adminProviderWriteField 是「Node payload 字段 → providers 列」的一条映射。
type adminProviderWriteField struct {
	Payload string
	Column  string
	Kind    adminProviderValueKind
}

// adminProviderWriteFields 是写路径允许触及的全部列。
//
// 逐条对照 src/repository/provider.ts:201-280（create 的 dbData）与 :649-...（update 的 dbData）。
// **不在表里的字段一律拒绝**：Node 的 payload 由 zod `.strict()` 把关，Go 侧若默默忽略未知键，
// 打错字段名的手工调用会得到「200 但什么都没改」——那是最难查的一类管理面缺陷。
var adminProviderWriteFields = []adminProviderWriteField{
	{"name", "name", providerTextKind},
	{"url", "url", providerTextKind},
	{"key", "key", providerTextKind},
	{"description", "description", providerNullableTextKind},
	{"is_enabled", "is_enabled", providerBoolKind},
	{"weight", "weight", providerIntKind},
	{"priority", "priority", providerIntKind},
	{"cost_multiplier", "cost_multiplier", providerNumericKind},
	{"group_tag", "group_tag", providerNullableTextKind},
	{"group_priorities", "group_priorities", providerJSONKind},
	{"provider_type", "provider_type", providerTextKind},
	{"preserve_client_ip", "preserve_client_ip", providerBoolKind},
	{"disable_session_reuse", "disable_session_reuse", providerBoolKind},
	{"model_redirects", "model_redirects", providerJSONKind},
	{"allowed_models", "allowed_models", providerJSONKind},
	{"allowed_clients", "allowed_clients", providerJSONKind},
	{"blocked_clients", "blocked_clients", providerJSONKind},
	{"active_time_start", "active_time_start", providerNullableTextKind},
	{"active_time_end", "active_time_end", providerNullableTextKind},
	{"mcp_passthrough_type", "mcp_passthrough_type", providerTextKind},
	{"mcp_passthrough_url", "mcp_passthrough_url", providerNullableTextKind},
	{"protocol_conversion_enabled", "protocol_conversion_enabled", providerBoolKind},
	{"limit_5h_usd", "limit_5h_usd", providerNumericKind},
	{"limit_5h_reset_mode", "limit_5h_reset_mode", providerTextKind},
	{"limit_daily_usd", "limit_daily_usd", providerNumericKind},
	{"daily_reset_mode", "daily_reset_mode", providerTextKind},
	{"daily_reset_time", "daily_reset_time", providerTextKind},
	{"limit_weekly_usd", "limit_weekly_usd", providerNumericKind},
	{"limit_monthly_usd", "limit_monthly_usd", providerNumericKind},
	{"limit_total_usd", "limit_total_usd", providerNumericKind},
	{"limit_concurrent_sessions", "limit_concurrent_sessions", providerNullableIntKind},
	{"max_retry_attempts", "max_retry_attempts", providerNullableIntKind},
	{"circuit_breaker_failure_threshold", "circuit_breaker_failure_threshold", providerIntKind},
	{"circuit_breaker_open_duration", "circuit_breaker_open_duration", providerIntKind},
	{
		"circuit_breaker_half_open_success_threshold",
		"circuit_breaker_half_open_success_threshold",
		providerIntKind,
	},
	// 等待阶梯：可空（null = 不启用），故用 nullableInt 而不是 Int——写入路径不该把「未填」
	// 当成 0 之外的东西，也不该在部分更新时把它变成必填。
	{"circuit_breaker_release_increment", "circuit_breaker_release_increment", providerNullableIntKind},
	{"circuit_breaker_max_open_count", "circuit_breaker_max_open_count", providerNullableIntKind},
	{"proxy_url", "proxy_url", providerNullableTextKind},
	{"proxy_fallback_to_direct", "proxy_fallback_to_direct", providerBoolKind},
	{"custom_headers", "custom_headers", providerJSONKind},
	{"first_byte_timeout_streaming_ms", "first_byte_timeout_streaming_ms", providerIntKind},
	{"streaming_idle_timeout_ms", "streaming_idle_timeout_ms", providerIntKind},
	{"request_timeout_non_streaming_ms", "request_timeout_non_streaming_ms", providerIntKind},
	{"website_url", "website_url", providerNullableTextKind},
	{"favicon_url", "favicon_url", providerNullableTextKind},
	{"cache_ttl_preference", "cache_ttl_preference", providerNullableTextKind},
	{"swap_cache_ttl_billing", "swap_cache_ttl_billing", providerBoolKind},
	{"context_1m_preference", "context_1m_preference", providerNullableTextKind},
	{"codex_reasoning_effort_preference", "codex_reasoning_effort_preference", providerNullableTextKind},
	{"codex_reasoning_summary_preference", "codex_reasoning_summary_preference", providerNullableTextKind},
	{"codex_text_verbosity_preference", "codex_text_verbosity_preference", providerNullableTextKind},
	{"codex_parallel_tool_calls_preference", "codex_parallel_tool_calls_preference", providerNullableTextKind},
	{"codex_image_generation_preference", "codex_image_generation_preference", providerNullableTextKind},
	{"codex_service_tier_preference", "codex_service_tier_preference", providerNullableTextKind},
	{"codex_max_tokens_preference", "codex_max_tokens_preference", providerNullableTextKind},
	{"anthropic_max_tokens_preference", "anthropic_max_tokens_preference", providerNullableTextKind},
	{"anthropic_thinking_budget_preference", "anthropic_thinking_budget_preference", providerNullableTextKind},
	{"anthropic_adaptive_thinking", "anthropic_adaptive_thinking", providerJSONKind},
	{"openai_max_tokens_preference", "openai_max_tokens_preference", providerNullableTextKind},
	{"gemini_google_search_preference", "gemini_google_search_preference", providerNullableTextKind},
	{"tpm", "tpm", providerNullableIntKind},
	{"rpm", "rpm", providerNullableIntKind},
	{"rpd", "rpd", providerNullableIntKind},
	{"cc", "cc", providerNullableIntKind},
}

// AdminProviderWriteFieldSupported 报告某个 payload 字段是否在写入白名单内。
func AdminProviderWriteFieldSupported(payload string) bool {
	_, ok := adminProviderWriteFieldFor(payload)
	return ok
}

func adminProviderWriteFieldFor(payload string) (adminProviderWriteField, bool) {
	for _, field := range adminProviderWriteFields {
		if field.Payload == payload {
			return field, true
		}
	}
	return adminProviderWriteField{}, false
}

// adminProviderBind 把字段袋编译成 `SET`/`INSERT` 用的列与值。
//
// 字段袋的键是 payload 名，值与规格表的 kind 对应（见各 kind 注释）。类型不符即报错——
// 静默写 NULL 会让「改一个字段把另一个字段清空」成为可能。
func adminProviderBind(
	fields map[string]any,
) (columns []string, values []any, err error) {
	payloads := make([]string, 0, len(fields))
	for payload := range fields {
		payloads = append(payloads, payload)
	}
	sort.Strings(payloads)

	for _, payload := range payloads {
		field, ok := adminProviderWriteFieldFor(payload)
		if !ok {
			return nil, nil, fmt.Errorf("store: 供应商字段 %q 不在写入白名单内", payload)
		}
		bound, err := adminProviderBindValue(field, fields[payload])
		if err != nil {
			return nil, nil, err
		}
		columns = append(columns, field.Column)
		values = append(values, bound)
	}
	return columns, values, nil
}

func adminProviderBindValue(field adminProviderWriteField, value any) (any, error) {
	invalid := func() error {
		return fmt.Errorf("store: 供应商字段 %q 的值类型不符（列 %s）", field.Payload, field.Column)
	}
	switch field.Kind {
	case providerTextKind:
		text, ok := value.(string)
		if !ok {
			return nil, invalid()
		}
		return text, nil
	case providerNullableTextKind:
		if value == nil {
			return nil, nil
		}
		text, ok := value.(*string)
		if !ok {
			return nil, invalid()
		}
		if text == nil {
			return nil, nil
		}
		return *text, nil
	case providerIntKind:
		number, ok := value.(int64)
		if !ok {
			return nil, invalid()
		}
		return number, nil
	case providerNullableIntKind:
		if value == nil {
			return nil, nil
		}
		number, ok := value.(*int64)
		if !ok {
			return nil, invalid()
		}
		if number == nil {
			return nil, nil
		}
		return *number, nil
	case providerBoolKind:
		flag, ok := value.(bool)
		if !ok {
			return nil, invalid()
		}
		return flag, nil
	case providerNumericKind:
		if value == nil {
			return nil, nil
		}
		number, ok := value.(*float64)
		if !ok {
			return nil, invalid()
		}
		if number == nil {
			return nil, nil
		}
		return *number, nil
	case providerJSONKind:
		if value == nil {
			return nil, nil
		}
		raw, ok := value.(json.RawMessage)
		if !ok {
			return nil, invalid()
		}
		if len(raw) == 0 {
			return nil, nil
		}
		return string(raw), nil
	}
	return nil, invalid()
}

// AdminCreateProvider 插入一行供应商，返回新行 id。
//
// 与 Node 的 createProvider 对齐的四处：
//   - `provider_vendor_id` 由 `AdminEnsureProviderVendor` 的结果给出（见该函数）；
//   - 之后调 `AdminEnsureProviderEndpoint` 补端点行（Node 在同一事务里做）；
//   - 未给出的字段留给列默认值（Node 侧是显式填默认值，两者取到的值相同——默认值本就定义在列上，
//     差异只在「Node 显式写默认」与「PG 用列默认」之间，见 admin_providers_write_test.go 的断言）；
//   - 事务包住三步，避免出现「有供应商无端点」的中间态。
func (p *Pools) AdminCreateProvider(
	ctx context.Context,
	fields map[string]any,
	websiteDomain string,
) (int64, error) {
	columns, values, err := adminProviderBind(fields)
	if err != nil {
		return 0, err
	}
	if len(columns) == 0 {
		return 0, fmt.Errorf("store: 创建供应商需要至少一个字段")
	}
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: 创建供应商事务开启失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	vendorID, err := adminEnsureVendorTx(ctx, tx, websiteDomain, fields)
	if err != nil {
		return 0, err
	}

	placeholders := make([]string, 0, len(columns)+1)
	for index := range columns {
		placeholders = append(placeholders, fmt.Sprintf("$%d", index+1))
	}
	placeholders = append(placeholders, fmt.Sprintf("$%d", len(columns)+1))
	query := fmt.Sprintf(
		"INSERT INTO providers (%s, provider_vendor_id) VALUES (%s) RETURNING id",
		strings.Join(columns, ", "),
		strings.Join(placeholders, ", "),
	)
	var id int64
	if err := tx.QueryRow(ctx, query, append(values, vendorID)...).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: 创建供应商失败: %w", err)
	}

	providerType := "claude"
	if raw, ok := fields["provider_type"].(string); ok && raw != "" {
		providerType = raw
	}
	url, _ := fields["url"].(string)
	if err := adminEnsureEndpointTx(ctx, tx, vendorID, providerType, url); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: 创建供应商提交失败: %w", err)
	}
	return id, nil
}

// AdminPatchProvider 部分更新：只写给出的字段（Node 的 `if (data.x !== undefined)` 语义）。
//
// 返回 false 表示目标不存在（含已软删）。
func (p *Pools) AdminPatchProvider(
	ctx context.Context,
	id int64,
	fields map[string]any,
) (bool, error) {
	if len(fields) == 0 {
		// Node 的 updateProvider 对空 payload 直接返回当前行（:651-654），不写 updated_at。
		pool, err := p.Control()
		if err != nil {
			return false, err
		}
		var exists bool
		err = pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM providers WHERE id = $1 AND deleted_at IS NULL)`, id,
		).Scan(&exists)
		if err != nil {
			return false, fmt.Errorf("store: 探测供应商存在性失败: %w", err)
		}
		return exists, nil
	}
	columns, values, err := adminProviderBind(fields)
	if err != nil {
		return false, err
	}
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	assignments := make([]string, 0, len(columns)+1)
	for index, column := range columns {
		assignments = append(assignments, fmt.Sprintf("%s = $%d", column, index+1))
	}
	assignments = append(assignments, "updated_at = now()")
	query := fmt.Sprintf(
		"UPDATE providers SET %s WHERE id = $%d AND deleted_at IS NULL",
		strings.Join(assignments, ", "),
		len(columns)+1,
	)
	tag, err := pool.Exec(ctx, query, append(values, id)...)
	if err != nil {
		return false, fmt.Errorf("store: 更新供应商失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AdminSoftDeleteProviders 软删一批供应商，返回实际删除行数。
//
// 复刻 deleteProvider（src/repository/provider.ts:1020-1083）的两步：
//  1. 置 deleted_at（已删的行不重复计数）；
//  2. 若该 (vendor, type, url) 已无**启用且未删**的其它供应商引用，连带软删端点行并把
//     is_enabled 置 false——不这么做，端点会长期留着被探活与展示（Node 侧的 #781）。
func (p *Pools) AdminSoftDeleteProviders(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: 删除供应商事务开启失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE providers SET deleted_at = now(), updated_at = now()
		 WHERE id = ANY($1) AND deleted_at IS NULL`, ids)
	if err != nil {
		return 0, fmt.Errorf("store: 软删供应商失败: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE provider_endpoints e
		 SET deleted_at = now(), is_enabled = false, updated_at = now()
		 WHERE e.deleted_at IS NULL
		   AND EXISTS (
		     SELECT 1 FROM providers p
		     WHERE p.id = ANY($1)
		       AND p.provider_vendor_id IS NOT NULL
		       AND p.provider_vendor_id = e.vendor_id
		       AND p.provider_type = e.provider_type
		       AND p.url = e.url
		       AND p.deleted_at IS NOT NULL
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM providers p2
		     WHERE p2.provider_vendor_id = e.vendor_id
		       AND p2.provider_type = e.provider_type
		       AND p2.url = e.url
		       AND p2.is_enabled = true
		       AND p2.deleted_at IS NULL
		   )`, ids); err != nil {
		return 0, fmt.Errorf("store: 连带软删端点失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: 删除供应商提交失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// AdminRestoreProviders 恢复一批软删的供应商，返回实际恢复行数。
//
// 复刻 restoreProviderInTransaction（:155-196）与 restoreSoftDeletedEndpointForProvider（:66-150）：
// 只恢复 60 秒内删除的行（Node 的 PROVIDER_RESTORE_MAX_AGE_MS），并把**同一删除时刻**（±1 秒容差）
// 连带软删的端点行恢复回来。
func (p *Pools) AdminRestoreProviders(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: 恢复供应商事务开启失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var restored int64
	for _, id := range ids {
		// 先取候选行的删除时刻，再清 deleted_at：端点的恢复窗口要拿**供应商被删的时刻**做参照，
		// 清了之后这个时刻就没了（第一版就踩了这个坑：把 provider 的 deleted_at 置空后
		// 子查询读到 NULL，端点恒不恢复）。
		var deletedAt time.Time
		err := tx.QueryRow(ctx,
			`SELECT deleted_at FROM providers
			 WHERE id = $1 AND deleted_at IS NOT NULL
			   AND now() - deleted_at <= interval '60 seconds'`, id).Scan(&deletedAt)
		if err == pgx.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("store: 读供应商 %d 的删除时刻失败: %w", id, err)
		}
		// CAS：删除时刻必须与快照一致，否则说明期间有人又删了一次，此时恢复会把新的删除一并撤销。
		tag, err := tx.Exec(ctx,
			`UPDATE providers SET deleted_at = NULL, updated_at = now()
			 WHERE id = $1 AND deleted_at = $2`, id, deletedAt)
		if err != nil {
			return 0, fmt.Errorf("store: 恢复供应商 %d 失败: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		restored++
		if _, err := tx.Exec(ctx,
			`UPDATE provider_endpoints e
			 SET deleted_at = NULL, is_enabled = true, updated_at = now()
			 WHERE e.deleted_at IS NOT NULL
			   AND EXISTS (
			     SELECT 1 FROM providers p
			     WHERE p.id = $1
			       AND p.provider_vendor_id IS NOT NULL
			       AND p.provider_vendor_id = e.vendor_id
			       AND p.provider_type = e.provider_type
			       AND p.url = e.url
			   )
			   AND NOT EXISTS (
			     SELECT 1 FROM provider_endpoints e2
			     WHERE e2.vendor_id = e.vendor_id
			       AND e2.provider_type = e.provider_type
			       AND e2.url = e.url
			       AND e2.deleted_at IS NULL
			   )
			   AND NOT EXISTS (
			     SELECT 1 FROM providers p2
			     WHERE p2.provider_vendor_id = e.vendor_id
			       AND p2.provider_type = e.provider_type
			       AND p2.url = e.url
			       AND p2.is_enabled = true
			       AND p2.deleted_at IS NULL
			       AND p2.id <> $1
			   )
			   AND abs(extract(epoch FROM (e.deleted_at - $2::timestamptz))) <= 1`,
			id, deletedAt); err != nil {
			return 0, fmt.Errorf("store: 恢复供应商 %d 的端点失败: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: 恢复供应商提交失败: %w", err)
	}
	return restored, nil
}

// adminEnsureVendorTx 取或建厂商行（getOrCreateProviderVendorIdFromUrls 的 Go 版）。
//
// 三处对齐：①按 website_domain 唯一定位；②已存在时只补空字段（display_name/website_url/favicon_url），
// 不覆盖已有值；③并发下靠唯一索引 + ON CONFLICT 兜底再回查一次。
func adminEnsureVendorTx(
	ctx context.Context,
	tx pgx.Tx,
	websiteDomain string,
	fields map[string]any,
) (int64, error) {
	displayName, _ := adminStringFieldValue(fields["name"])
	websiteURL, _ := adminStringFieldValue(fields["website_url"])
	faviconURL, _ := adminStringFieldValue(fields["favicon_url"])

	var existingID int64
	var currentDisplay, currentWebsite, currentFavicon *string
	err := tx.QueryRow(ctx,
		`SELECT id, display_name, website_url, favicon_url
		 FROM provider_vendors WHERE website_domain = $1`, websiteDomain,
	).Scan(&existingID, &currentDisplay, &currentWebsite, &currentFavicon)
	if err == nil {
		// 参数显式转 text：nil 的 *string 在 pgx 里是不带类型信息的 NULL，
		// 而 `$2 IS NOT NULL` 需要 PG 能推出类型（否则 ERROR 42P08）。
		if _, err := tx.Exec(ctx,
			`UPDATE provider_vendors SET
			   display_name = COALESCE(NULLIF(display_name, ''), $2::text),
			   website_url  = COALESCE(website_url, $3::text),
			   favicon_url  = COALESCE(favicon_url, $4::text),
			   updated_at = now()
			 WHERE id = $1
			   AND (
			     (COALESCE(NULLIF(display_name, ''), '') = '' AND $2::text IS NOT NULL)
			     OR (website_url IS NULL AND $3::text IS NOT NULL)
			     OR (favicon_url IS NULL AND $4::text IS NOT NULL)
			   )`,
			existingID, nullableTrimmed(displayName), nullableTrimmed(websiteURL),
			nullableTrimmed(faviconURL)); err != nil {
			return 0, fmt.Errorf("store: 补齐厂商信息失败: %w", err)
		}
		return existingID, nil
	}
	if err != pgx.ErrNoRows {
		return 0, fmt.Errorf("store: 查询厂商失败: %w", err)
	}

	var insertedID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain, display_name, website_url, favicon_url, updated_at)
		 VALUES ($1, $2::text, $3::text, $4::text, now())
		 ON CONFLICT (website_domain) DO NOTHING
		 RETURNING id`,
		websiteDomain, nullableTrimmed(displayName), nullableTrimmed(websiteURL),
		nullableTrimmed(faviconURL)).Scan(&insertedID)
	if err == nil {
		return insertedID, nil
	}
	if err != pgx.ErrNoRows {
		return 0, fmt.Errorf("store: 创建厂商失败: %w", err)
	}
	// ON CONFLICT DO NOTHING 命中：并发下另一个写入者刚建了同一域名，回查一次。
	if err := tx.QueryRow(ctx,
		`SELECT id FROM provider_vendors WHERE website_domain = $1`, websiteDomain,
	).Scan(&insertedID); err != nil {
		return 0, fmt.Errorf("store: 回查厂商失败: %w", err)
	}
	return insertedID, nil
}

// adminEnsureEndpointTx 保证端点行存在（ensureProviderEndpointExistsForUrl 的 Go 版）。
func adminEnsureEndpointTx(
	ctx context.Context,
	tx pgx.Tx,
	vendorID int64,
	providerType string,
	url string,
) error {
	trimmed := strings.TrimSpace(url)
	if trimmed == "" {
		return fmt.Errorf("store: 端点 URL 不能为空")
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO provider_endpoints (vendor_id, provider_type, url, updated_at)
		 VALUES ($1, $2, $3, now())
		 ON CONFLICT (vendor_id, provider_type, url) WHERE deleted_at IS NULL DO NOTHING`,
		vendorID, providerType, trimmed); err != nil {
		return fmt.Errorf("store: 创建端点行失败: %w", err)
	}
	return nil
}

// adminStringFieldValue 读字段袋里的一个字符串字段（不存在或非字符串都返回空）。
func adminStringFieldValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case *string:
		if typed == nil {
			return "", false
		}
		return *typed, true
	}
	return "", false
}

// nullableTrimmed 把空串转成 NULL（Node 侧 `input.displayName?.trim() || null`）。
func nullableTrimmed(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
