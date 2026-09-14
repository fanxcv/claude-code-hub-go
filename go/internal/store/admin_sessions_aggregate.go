package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本文件是 sessions 资源族的**聚合底座**，供后续端点波（列表 / 详情 / 终止）共用。
//
// 唯一真源：
//   - src/repository/message.ts 的 aggregateMultipleSessionStats（:1731-2103）：列表页数据源，
//     同时是批量终止 requestedSessionIds 映射的来源。
//   - src/repository/message.ts 的 resolveSessionIdentity（:1484-1526）：identity 种类、
//     scopeTag、fingerprint 集合。
//
// 与同目录 admin_sessions.go 的分工：那个文件管「一次规范 identity 的所有权」（窄查询），
// 本文件管「一批 identity 的账本聚合」。**两者不可互替**：Node 的 aggregateMultipleSessionStats
// 在身份归并后还 INNER JOIN users/keys，被联结掉的行不会出现在结果里；而
// ResolveAdminSessionOwner 不带这两个联结、会照样答出所有者。故本文件不复用那条查询。
//
// 三处照抄 Node 的细节，改动即语义漂移：
//
//  1. **身份归并是 unnest(ARRAY[ids]) CROSS JOIN LATERAL + UNION ALL 两支**：第二支只在
//     入参不是 pfx:/sid: 保留前缀时生效（`NOT LIKE 'pfx:%' AND NOT LIKE 'sid:%'`），把
//     「物理 session_id 恰好等于入参」的行归到它的规范 identity 上。同一规范 identity 可被
//     多个入参命中，故 userInfoMap 以规范 identity 为键、**首个命中者胜**（按 ordinality 序）。
//  2. **账本条件按「每个规范 identity + 它自己的所有者」逐条 OR**（:1915）：不是所有 identity
//     共用一个 owner 条件。前缀亲和下同一 pfx: identity 可能被多个用户使用，共用一个条件会把
//     不同用户的账本行合并计数。
//  3. **cacheTtlApplied 是三态**：无值 → null，单值 → 原值，多值 → 字面量 "mixed"（:2064-2069）。

// AdminSessionProvider 是会话涉及的一个供应商。
type AdminSessionProvider struct {
	ID   int64
	Name string
}

// AdminSessionSummary 是 aggregateMultipleSessionStats 的一个结果项。
//
// 字段与 Node 的返回对象逐字对应：数值型聚合在 Node 侧是 `::double precision`（JS number），
// 这里用 int64——两者的 JSON 输出相同（这些列本身都是整数），差别只在 Go 侧不做浮点装箱。
// 唯一的字符串金额是 TotalCostUSD（Node 侧是 numeric 的文本形式）。
type AdminSessionSummary struct {
	// SessionID 是规范 identity（COALESCE(session_identity, session_id)）。
	SessionID string
	// RequestedSessionIDs 是归并到该规范 identity 的入参（按入参顺序，去重）。
	RequestedSessionIDs []string
	// SessionIdentityKind 只有 "session_id" / "prefix_affinity" 两值（Node 的 CASE 兜底）。
	SessionIdentityKind string
	SessionFingerprint  *string

	RequestCount             int64
	TotalCostUSD             string
	TotalInputTokens         int64
	TotalOutputTokens        int64
	TotalCacheCreationTokens int64
	TotalCacheReadTokens     int64
	TotalDurationMS          int64
	FirstRequestAt           *time.Time
	LastRequestAt            *time.Time

	Providers []AdminSessionProvider
	Models    []string

	UserName  string
	UserID    int64
	KeyName   string
	KeyID     int64
	UserAgent *string
	APIType   *string

	// CacheTTLApplied 可能是具体值或字面量 "mixed"（见文件头第 3 条）。
	CacheTTLApplied *string
}

// AdminSessionIdentity 是 resolveSessionIdentity 的结果。
type AdminSessionIdentity struct {
	Identity        string
	SourceSessionID *string
	IdentityKind    *string
	ScopeTag        *string
	Fingerprint     *string
	Fingerprints    []string
}

// adminSessionIdentityRow 是身份归并查询（Node :1757-1802）针对一个入参的结果。
type adminSessionIdentityRow struct {
	RequestedSessionID  string
	SessionID           string
	UserName            string
	UserID              int64
	KeyName             string
	KeyID               int64
	IdentityKind        *string
	Fingerprint         *string
	UserAgent           *string
	APIType             *string
	RequestedSessionIDs []string
}

// adminSessionLedgerRef 是一条待聚合的规范 identity 与它自己的所有者。
type adminSessionLedgerRef struct {
	CanonicalIdentity string
	UserID            int64
}

// adminSessionLedgerStats 是账本聚合的一行。
type adminSessionLedgerStats struct {
	RequestCount             int64
	TotalCostUSD             string
	TotalInputTokens         int64
	TotalOutputTokens        int64
	TotalCacheCreationTokens int64
	TotalCacheReadTokens     int64
	TotalDurationMS          int64
	FirstRequestAt           *time.Time
	LastRequestAt            *time.Time
}

// normalizeAdminSessionIdentityKind 复刻 Node 的 CASE（:1770-1773）：
// 只有字面量 'prefix_affinity' 归为该类，其余（含 NULL）都是 "session_id"。
func normalizeAdminSessionIdentityKind(kind *string) string {
	if kind != nil && *kind == "prefix_affinity" {
		return "prefix_affinity"
	}
	return "session_id"
}

// resolveAdminSessionIdentityKind 复刻 resolveSessionIdentity 的集合归并（:1514-1520）：
// 全部行同一种类时给出该种类，混用时给 null。
func resolveAdminSessionIdentityKind(rows []*string) *string {
	kinds := map[string]struct{}{}
	for _, kind := range rows {
		kinds[normalizeAdminSessionIdentityKind(kind)] = struct{}{}
	}
	if len(kinds) != 1 {
		return nil
	}
	for kind := range kinds {
		value := kind
		return &value
	}
	return nil
}

// resolveAdminSessionCacheTTLApplied 复刻 Node :2064-2069 的三态判定。
func resolveAdminSessionCacheTTLApplied(values []string) *string {
	switch len(values) {
	case 0:
		return nil
	case 1:
		value := values[0]
		return &value
	default:
		mixed := "mixed"
		return &mixed
	}
}

// parseAdminSessionFingerprintChain 解析 affinity_fingerprint_chain（jsonb string[]）。
//
// Node 侧是 `if (Array.isArray(row.fingerprintChain))` 后逐项取非空字符串（:1504-1510）；
// 非数组（null / 对象 / 标量）与数组内的非字符串项都跳过。解析失败同样按「非数组」处理——
// 与 JS 的 Array.isArray 判定同判。
func parseAdminSessionFingerprintChain(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var decoded []any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	fingerprints := make([]string, 0, len(decoded))
	for _, item := range decoded {
		if value, ok := item.(string); ok && value != "" {
			fingerprints = append(fingerprints, value)
		}
	}
	return fingerprints
}

// ledgerCanonicalSessionCondition 复刻 ledgerCanonicalSessionCondition（message.ts:42-46）：
// 保留前缀要求规范列与**物理** session_identity 列同时命中（把「物理 session_id 恰好等于这个
// 保留串」的行排除掉），非保留前缀只比规范列。值占位符复用一个，两处引用同一参数。
func ledgerCanonicalSessionCondition(identity string, args *[]any) string {
	*args = append(*args, identity)
	placeholder := "$" + strconv.Itoa(len(*args))
	condition := "COALESCE(session_identity, session_id) = " + placeholder
	if IsReservedSessionIdentity(identity) {
		condition += " AND session_identity = " + placeholder
	}
	return condition
}

// ledgerPerSessionOwnerCondition 复刻 Node :1915 的 `or(...)`：每个规范 identity 配**它自己的**
// user_id 条件再 OR 起来（见文件头第 2 条）。整体括号包住，便于与账本计费条件相加。
func ledgerPerSessionOwnerCondition(refs []adminSessionLedgerRef, args *[]any) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		condition := ledgerCanonicalSessionCondition(ref.CanonicalIdentity, args)
		*args = append(*args, ref.UserID)
		parts = append(parts, "("+condition+" AND user_id = $"+strconv.Itoa(len(*args))+")")
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// AggregateAdminSessionStats 复刻 aggregateMultipleSessionStats（message.ts:1731-2103）。
//
// ownerUserID <= 0 表示不加所有者条件（Node 的 ownerUserId === undefined，管理员路径）。
// 入参为空返回空切片（不是 nil），与 Node 的 `if (sessionIds.length === 0) return []` 同判。
//
// 查询次序与 Node 同：身份归并一趟，随后账本统计 / 供应商 / 模型 / Cache TTL 各一趟，共五趟。
// 四条聚合查询共用同一组「逐 identity + 所有者」条件与计费条件。
func (p *Pools) AggregateAdminSessionStats(
	ctx context.Context,
	sessionIDs []string,
	ownerUserID int64,
) ([]AdminSessionSummary, error) {
	if len(sessionIDs) == 0 {
		return []AdminSessionSummary{}, nil
	}

	rows, err := p.loadAdminSessionIdentityRows(ctx, sessionIDs, ownerUserID)
	if err != nil {
		return nil, err
	}

	// userInfoMap 以规范 identity 为键，**首个命中者胜**（行序 = 入参 ordinality 序）。
	userInfo := map[string]adminSessionIdentityRow{}
	canonicalByRequested := map[string]string{}
	for _, row := range rows {
		canonicalByRequested[row.RequestedSessionID] = row.SessionID
		existing, seen := userInfo[row.SessionID]
		if !seen {
			userInfo[row.SessionID] = row
			continue
		}
		if !containsString(existing.RequestedSessionIDs, row.RequestedSessionID) {
			existing.RequestedSessionIDs = append(existing.RequestedSessionIDs, row.RequestedSessionID)
			userInfo[row.SessionID] = existing
		}
	}

	canonicalIDs := make([]string, 0, len(rows))
	refs := make([]adminSessionLedgerRef, 0, len(rows))
	seenCanonical := map[string]struct{}{}
	for _, requested := range sessionIDs {
		canonical, mapped := canonicalByRequested[requested]
		if !mapped {
			continue
		}
		if _, duplicate := seenCanonical[canonical]; duplicate {
			continue
		}
		seenCanonical[canonical] = struct{}{}
		canonicalIDs = append(canonicalIDs, canonical)
		refs = append(refs, adminSessionLedgerRef{
			CanonicalIdentity: canonical,
			UserID:            userInfo[canonical].UserID,
		})
	}
	if len(canonicalIDs) == 0 {
		return []AdminSessionSummary{}, nil
	}

	stats, err := p.loadAdminSessionLedgerStats(ctx, refs)
	if err != nil {
		return nil, err
	}
	providers, err := p.loadAdminSessionProviders(ctx, refs)
	if err != nil {
		return nil, err
	}
	models, err := p.loadAdminSessionModels(ctx, refs)
	if err != nil {
		return nil, err
	}
	cacheTTLs, err := p.loadAdminSessionCacheTTLs(ctx, refs)
	if err != nil {
		return nil, err
	}

	results := make([]AdminSessionSummary, 0, len(canonicalIDs))
	for _, canonical := range canonicalIDs {
		info, found := userInfo[canonical]
		if !found {
			// Node 同样跳过（:2033-2036）；身份归并表里必有条目，这里只是照抄那条防御。
			continue
		}
		summary := AdminSessionSummary{
			SessionID:           canonical,
			RequestedSessionIDs: info.RequestedSessionIDs,
			SessionIdentityKind: normalizeAdminSessionIdentityKind(info.IdentityKind),
			SessionFingerprint:  info.Fingerprint,
			Providers:           providers[canonical],
			Models:              models[canonical],
			UserName:            info.UserName,
			UserID:              info.UserID,
			KeyName:             info.KeyName,
			KeyID:               info.KeyID,
			UserAgent:           info.UserAgent,
			APIType:             info.APIType,
			CacheTTLApplied:     resolveAdminSessionCacheTTLApplied(cacheTTLs[canonical]),
			// 无账本行时 Node 用全零兜底（:2037-2048），金额的零是字符串 "0"。
			TotalCostUSD: "0",
		}
		if billing, ok := stats[canonical]; ok {
			summary.RequestCount = billing.RequestCount
			summary.TotalCostUSD = billing.TotalCostUSD
			summary.TotalInputTokens = billing.TotalInputTokens
			summary.TotalOutputTokens = billing.TotalOutputTokens
			summary.TotalCacheCreationTokens = billing.TotalCacheCreationTokens
			summary.TotalCacheReadTokens = billing.TotalCacheReadTokens
			summary.TotalDurationMS = billing.TotalDurationMS
			summary.FirstRequestAt = billing.FirstRequestAt
			summary.LastRequestAt = billing.LastRequestAt
		}
		results = append(results, summary)
	}
	return results, nil
}

// loadAdminSessionIdentityRows 复刻 Node 的身份归并查询（:1757-1802）。
func (p *Pools) loadAdminSessionIdentityRows(
	ctx context.Context,
	sessionIDs []string,
	ownerUserID int64,
) ([]adminSessionIdentityRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}

	args := make([]any, 0, len(sessionIDs)+1)
	placeholders := make([]string, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		args = append(args, id)
		placeholders = append(placeholders, "$"+strconv.Itoa(len(args)))
	}
	// owner 条件出现在两个 UNION 支里，共用同一个占位符——Node 的 ownerCondition 两处引用
	// 同一个 sql 片段，展开后也是同一个参数值。
	ownerCondition := ""
	if ownerUserID > 0 {
		args = append(args, ownerUserID)
		ownerCondition = " AND user_id = $" + strconv.Itoa(len(args))
	}

	// candidates 里的 `sid` 是 LATERAL 外层 unnest 的列，两个支都靠它命中。
	query := `SELECT
		requested.sid AS requested_session_id,
		COALESCE(mr.session_identity, mr.session_id) AS session_id,
		u.name, u.id, k.name, k.id,
		CASE
			WHEN mr.session_identity_kind = 'prefix_affinity' THEN 'prefix_affinity'
			ELSE 'session_id'
		END AS session_identity_kind,
		mr.affinity_fingerprint, mr.user_agent, mr.api_type
		FROM unnest(ARRAY[` + strings.Join(placeholders, ", ") + `]::varchar[])
			WITH ORDINALITY AS requested(sid, ordinality)
		CROSS JOIN LATERAL (
			SELECT
				id, session_id, session_identity, user_id, key, session_identity_kind,
				affinity_fingerprint, user_agent, api_type
			FROM (
				SELECT
					id, session_id, session_identity, user_id, key, session_identity_kind,
					affinity_fingerprint, user_agent, api_type, created_at,
					CASE WHEN session_identity = sid THEN 0 ELSE 1 END AS identity_priority
				FROM message_request
				WHERE COALESCE(session_identity, session_id) = sid
					AND deleted_at IS NULL` + ownerCondition + `

				UNION ALL

				SELECT
					id, session_id, session_identity, user_id, key, session_identity_kind,
					affinity_fingerprint, user_agent, api_type, created_at,
					1 AS identity_priority
				FROM message_request
				WHERE sid NOT LIKE 'pfx:%'
					AND sid NOT LIKE 'sid:%'
					AND session_identity IS NOT NULL
					AND session_identity <> sid
					AND session_id = sid
					AND deleted_at IS NULL` + ownerCondition + `
			) candidates
			ORDER BY identity_priority, created_at DESC NULLS LAST, id DESC
			LIMIT 1
		) mr
		INNER JOIN users u ON mr.user_id = u.id
		INNER JOIN keys k ON mr.key = k.key
		ORDER BY requested.ordinality`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 会话身份归并查询失败: %w", err)
	}
	defer rows.Close()

	results := make([]adminSessionIdentityRow, 0, len(sessionIDs))
	for rows.Next() {
		var row adminSessionIdentityRow
		if err := rows.Scan(
			&row.RequestedSessionID, &row.SessionID,
			&row.UserName, &row.UserID, &row.KeyName, &row.KeyID,
			&row.IdentityKind, &row.Fingerprint, &row.UserAgent, &row.APIType,
		); err != nil {
			return nil, fmt.Errorf("store: 读取会话身份行失败: %w", err)
		}
		row.RequestedSessionIDs = []string{row.RequestedSessionID}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话身份行失败: %w", err)
	}
	return results, nil
}

// loadAdminSessionLedgerStats 复刻 Node 的批量统计查询（:1918-1938）。
func (p *Pools) loadAdminSessionLedgerStats(
	ctx context.Context,
	refs []adminSessionLedgerRef,
) (map[string]adminSessionLedgerStats, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(refs)*2)
	condition := ledgerPerSessionOwnerCondition(refs, &args)

	query := `SELECT
		COALESCE(session_identity, session_id) AS session_id,
		count(*)::bigint,
		COALESCE(SUM(cost_usd), 0)::text,
		COALESCE(SUM(input_tokens), 0)::bigint,
		COALESCE(SUM(output_tokens), 0)::bigint,
		COALESCE(SUM(cache_creation_input_tokens), 0)::bigint,
		COALESCE(SUM(cache_read_input_tokens), 0)::bigint,
		COALESCE(SUM(duration_ms), 0)::bigint,
		MIN(created_at), MAX(created_at)
		FROM usage_ledger
		WHERE ` + condition + ` AND ` + BillingCondition + `
		GROUP BY COALESCE(session_identity, session_id)`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 会话账本聚合查询失败: %w", err)
	}
	defer rows.Close()

	results := map[string]adminSessionLedgerStats{}
	for rows.Next() {
		var sessionID string
		var stats adminSessionLedgerStats
		if err := rows.Scan(
			&sessionID, &stats.RequestCount, &stats.TotalCostUSD,
			&stats.TotalInputTokens, &stats.TotalOutputTokens,
			&stats.TotalCacheCreationTokens, &stats.TotalCacheReadTokens,
			&stats.TotalDurationMS, &stats.FirstRequestAt, &stats.LastRequestAt,
		); err != nil {
			return nil, fmt.Errorf("store: 读取会话账本聚合行失败: %w", err)
		}
		results[sessionID] = stats
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话账本聚合行失败: %w", err)
	}
	return results, nil
}

// loadAdminSessionProviders 复刻 Node 的供应商查询（:1944-1957）。
//
// 一处**登记过的差异**：Node 用 `selectDistinct` 且**无 ORDER BY**，数组顺序由查询计划决定；
// 这里按「首次使用」定序（每个供应商取它最早那行，再按该时刻排序），结果确定。列表页取
// `providers[0].id` 当会话的 providerId，定序后该值可复现；Node 侧不可复现。
func (p *Pools) loadAdminSessionProviders(
	ctx context.Context,
	refs []adminSessionLedgerRef,
) (map[string][]AdminSessionProvider, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(refs)*2)
	condition := ledgerPerSessionOwnerCondition(refs, &args)

	// 外层不加表别名：只用全限定的列名兜住 JOIN 的同名列，计费条件里的裸列名因此也不必改前缀。
	query := `SELECT latest.session_id, latest.provider_id, latest.provider_name
		FROM (
			SELECT DISTINCT ON (
					COALESCE(usage_ledger.session_identity, usage_ledger.session_id),
					usage_ledger.final_provider_id
				)
				COALESCE(usage_ledger.session_identity, usage_ledger.session_id) AS session_id,
				usage_ledger.final_provider_id AS provider_id,
				providers.name AS provider_name,
				usage_ledger.created_at, usage_ledger.id
			FROM usage_ledger
			LEFT JOIN providers ON providers.id = usage_ledger.final_provider_id
			WHERE ` + condition + ` AND ` + BillingCondition + `
				AND usage_ledger.final_provider_id IS NOT NULL
			ORDER BY
				COALESCE(usage_ledger.session_identity, usage_ledger.session_id),
				usage_ledger.final_provider_id,
				usage_ledger.created_at, usage_ledger.id
		) latest
		ORDER BY latest.session_id, latest.created_at, latest.id`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 会话供应商查询失败: %w", err)
	}
	defer rows.Close()

	results := map[string][]AdminSessionProvider{}
	for rows.Next() {
		var sessionID string
		var providerID int64
		var providerName *string
		if err := rows.Scan(&sessionID, &providerID, &providerName); err != nil {
			return nil, fmt.Errorf("store: 读取会话供应商行失败: %w", err)
		}
		results[sessionID] = append(results[sessionID], AdminSessionProvider{
			ID:   providerID,
			Name: adminSessionProviderName(providerName, providerID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话供应商行失败: %w", err)
	}
	return results, nil
}

// loadAdminSessionModels 复刻 Node 的模型查询（:1982-1994）；定序理由同供应商。
func (p *Pools) loadAdminSessionModels(
	ctx context.Context,
	refs []adminSessionLedgerRef,
) (map[string][]string, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(refs)*2)
	condition := ledgerPerSessionOwnerCondition(refs, &args)

	query := `SELECT latest.session_id, latest.model
		FROM (
			SELECT DISTINCT ON (
					COALESCE(session_identity, session_id), model
				)
				COALESCE(session_identity, session_id) AS session_id,
				model, created_at, id
			FROM usage_ledger
			WHERE ` + condition + ` AND ` + BillingCondition + ` AND model IS NOT NULL
			ORDER BY COALESCE(session_identity, session_id), model, created_at, id
		) latest
		ORDER BY latest.session_id, latest.created_at, latest.id`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 会话模型查询失败: %w", err)
	}
	defer rows.Close()

	results := map[string][]string{}
	for rows.Next() {
		var sessionID string
		var model *string
		if err := rows.Scan(&sessionID, &model); err != nil {
			return nil, fmt.Errorf("store: 读取会话模型行失败: %w", err)
		}
		if model != nil {
			results[sessionID] = append(results[sessionID], *model)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话模型行失败: %w", err)
	}
	return results, nil
}

// loadAdminSessionCacheTTLs 复刻 Node 的 Cache TTL 查询（:2003-2014）：返回每个会话的**去重值
// 列表**，组装时才判 单值 / "mixed"。
func (p *Pools) loadAdminSessionCacheTTLs(
	ctx context.Context,
	refs []adminSessionLedgerRef,
) (map[string][]string, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(refs)*2)
	condition := ledgerPerSessionOwnerCondition(refs, &args)

	query := `SELECT
		COALESCE(session_identity, session_id) AS session_id, cache_ttl_applied
		FROM usage_ledger
		WHERE ` + condition + ` AND ` + BillingCondition + `
			AND cache_ttl_applied IS NOT NULL
		GROUP BY COALESCE(session_identity, session_id), cache_ttl_applied
		ORDER BY 1, 2`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 会话 Cache TTL 查询失败: %w", err)
	}
	defer rows.Close()

	results := map[string][]string{}
	for rows.Next() {
		var sessionID string
		var cacheTTL *string
		if err := rows.Scan(&sessionID, &cacheTTL); err != nil {
			return nil, fmt.Errorf("store: 读取会话 Cache TTL 行失败: %w", err)
		}
		if cacheTTL != nil {
			results[sessionID] = append(results[sessionID], *cacheTTL)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话 Cache TTL 行失败: %w", err)
	}
	return results, nil
}

// ResolveAdminSessionIdentity 复刻 resolveSessionIdentity（message.ts:1484-1526）。
//
// ownerUserID <= 0 表示不加所有者条件（管理员）。查不到时返回 ok=false（Node 返回 null）。
//
// 一处**登记过的差异**：Node 的 orderBy 只有 `created_at DESC`，并列时取哪一行由计划决定；
// 这里补 `id DESC` 作次键，使 sourceSessionId / scopeTag / fingerprint / fingerprints 的取值
// 可复现。identity 与 identityKind 是全行集合的归并，两种写法同判。
func (p *Pools) ResolveAdminSessionIdentity(
	ctx context.Context,
	identity string,
	ownerUserID int64,
) (AdminSessionIdentity, bool, error) {
	pool, err := p.Control()
	if err != nil {
		return AdminSessionIdentity{}, false, err
	}

	condition, args := adminSessionCanonicalCondition(identity, ownerUserID, "")
	query := `SELECT
		COALESCE(session_identity, session_id), session_id, session_identity_kind,
		affinity_scope_tag, affinity_fingerprint, affinity_fingerprint_chain
		FROM message_request
		WHERE ` + condition + ` AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return AdminSessionIdentity{}, false, fmt.Errorf("store: 解析会话 identity 失败: %w", err)
	}
	defer rows.Close()

	resolved := AdminSessionIdentity{Fingerprints: []string{}}
	fingerprintSet := map[string]struct{}{}
	identityKinds := make([]*string, 0, 8)
	seen := false
	for rows.Next() {
		var rowIdentity string
		var sourceSessionID *string
		var identityKind *string
		var scopeTag *string
		var fingerprint *string
		var fingerprintChain []byte
		if err := rows.Scan(
			&rowIdentity, &sourceSessionID, &identityKind, &scopeTag, &fingerprint,
			&fingerprintChain,
		); err != nil {
			return AdminSessionIdentity{}, false, fmt.Errorf("store: 读取会话 identity 行失败: %w", err)
		}
		if !seen {
			// Node :1513 用 `rows[0].identity ?? identity`；本查询的规范列与入参同判，故取列值。
			resolved.Identity = rowIdentity
			seen = true
		}
		if resolved.SourceSessionID == nil && sourceSessionID != nil {
			resolved.SourceSessionID = sourceSessionID
		}
		if resolved.ScopeTag == nil && scopeTag != nil {
			resolved.ScopeTag = scopeTag
		}
		if resolved.Fingerprint == nil && fingerprint != nil {
			resolved.Fingerprint = fingerprint
		}
		// fingerprint 与整条 chain 一起进集合，顺序为「行序 → 先 fingerprint 后 chain」（:1504-1510）。
		if fingerprint != nil && *fingerprint != "" {
			if _, duplicate := fingerprintSet[*fingerprint]; !duplicate {
				fingerprintSet[*fingerprint] = struct{}{}
				resolved.Fingerprints = append(resolved.Fingerprints, *fingerprint)
			}
		}
		for _, item := range parseAdminSessionFingerprintChain(fingerprintChain) {
			if _, duplicate := fingerprintSet[item]; duplicate {
				continue
			}
			fingerprintSet[item] = struct{}{}
			resolved.Fingerprints = append(resolved.Fingerprints, item)
		}
		identityKinds = append(identityKinds, identityKind)
	}
	if err := rows.Err(); err != nil {
		return AdminSessionIdentity{}, false, fmt.Errorf("store: 遍历会话 identity 行失败: %w", err)
	}
	if !seen {
		return AdminSessionIdentity{}, false, nil
	}
	resolved.IdentityKind = resolveAdminSessionIdentityKind(identityKinds)
	return resolved, true, nil
}

// adminSessionProviderName 复刻 Node 的 `p.providerName || `Provider #${id}“（:1966）：
// 名称为空串时同样走兜底（JS 里空串是假值）。
func adminSessionProviderName(name *string, providerID int64) string {
	if name == nil || *name == "" {
		return "Provider #" + strconv.FormatInt(providerID, 10)
	}
	return *name
}

// containsString 报 value 是否已在 items 里（Node 用 Array.includes）。
func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
