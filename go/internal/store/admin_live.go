package store

import (
	"context"
	"fmt"
	"time"
)

// 本文件是 dashboard 两条「实时面」端点的库内读面：
//
//	GET /api/v1/dashboard/proxy-status  ← src/lib/proxy-status-tracker.ts
//	GET /api/v1/dashboard/realtime 的活动流 ← src/repository/activity-stream.ts
//
// **先纠一处容易误判的前提**：Node 的 ProxyStatusTracker 并不是进程内观测态。它的
// startRequest/endRequest 是空实现（函数体就是 `void params;`），getAllUsersStatus 是**库内
// 聚合**——活跃请求 = `status_code IS NULL` 的那批行，最近一次 = 每个用户 `updated_at` 最新的
// 一行。源码注释原文：「当前实现基于数据库数据聚合，确保多运行时环境下的一致性」。
//
// 所以这里同样走库内聚合，**不新增任何运行态观测写键**。数据面只维护会话观测 ZSET（另一条
// 链，见 internal/adminapi/dashboard_runtime.go），两者互不依赖；若照「进程内观测态」去实现，
// 得到的会是与 Node 判据不同的第二套真源（活跃判据、最近一次判据都会漂）。

// AdminProxyStatusUserRow 是一个未软删用户（作答里每用户一条）。
type AdminProxyStatusUserRow struct {
	UserID   int64
	UserName string
}

// AdminProxyStatusActiveRow 是一条**未终局**的请求（ProxyStatusTracker.loadActiveRequests）。
type AdminProxyStatusActiveRow struct {
	RequestID    int64
	UserID       int64
	KeyString    string
	KeyName      *string
	ProviderID   int64
	ProviderName string
	Model        *string
	CreatedAt    *time.Time
}

// AdminProxyStatusLastRow 是一个用户**最近一次已终局**的请求（loadLastRequests）。
type AdminProxyStatusLastRow struct {
	UserID       int64
	RequestID    int64
	KeyString    string
	KeyName      *string
	ProviderID   int64
	ProviderName string
	Model        *string
	EndTime      *time.Time
}

// AdminActivityRow 是活动流的一条请求（findRecentActivityStream 的 ActivityStreamItem 子集：
// 只取 dashboard/realtime 真正读的字段）。
type AdminActivityRow struct {
	RequestID     int64
	SessionID     *string
	UserName      *string
	ProviderName  *string
	Model         *string
	OriginalModel *string
	StatusCode    *int64
	DurationMS    *int64
	CostUSD       *string
	CreatedAt     *time.Time
}

// AdminProxyStatusUsers 复刻 ProxyStatusTracker 的 `select id,name from users where deleted_at is null`。
func (p *Pools) AdminProxyStatusUsers(ctx context.Context) ([]AdminProxyStatusUserRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx,
		`SELECT id, name FROM users WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: 代理状态用户查询失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminProxyStatusUserRow, 0)
	for rows.Next() {
		var row AdminProxyStatusUserRow
		if err := rows.Scan(&row.UserID, &row.UserName); err != nil {
			return nil, fmt.Errorf("store: 读取代理状态用户行失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历代理状态用户行失败: %w", err)
	}
	return results, nil
}

// AdminProxyStatusActiveRequests 复刻 loadActiveRequests。
//
// 四个照抄的判据（缺一条就会把不该算的请求算成活跃）：
//   - `status_code IS NULL`：未终局（终局后 status_code 必被写入）。
//   - `created_at >= now() - interval '24 hours'`：24 小时窗（Node 的硬编码）。
//   - `is_replay = false` 且 `blocked_by is null or <> 'warmup'`：回放行与预热行都不算。
//   - 供应商未软删。
//
// Node 侧没有 ORDER BY（结果顺序不确定），这里同样不排序。
func (p *Pools) AdminProxyStatusActiveRequests(
	ctx context.Context,
) ([]AdminProxyStatusActiveRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT mr.id, mr.user_id, mr.key, k.name,
		       p.id, p.name, mr.model, mr.created_at
		FROM message_request mr
		INNER JOIN providers p ON mr.provider_id = p.id
		LEFT JOIN keys k ON k.key = mr.key AND k.deleted_at IS NULL
		WHERE mr.deleted_at IS NULL
		  AND mr.status_code IS NULL
		  AND mr.created_at >= now() - interval '24 hours'
		  AND mr.is_replay = false
		  AND (mr.blocked_by IS NULL OR mr.blocked_by <> 'warmup')
		  AND p.deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: 代理状态活跃请求查询失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminProxyStatusActiveRow, 0)
	for rows.Next() {
		var row AdminProxyStatusActiveRow
		if err := rows.Scan(&row.RequestID, &row.UserID, &row.KeyString, &row.KeyName,
			&row.ProviderID, &row.ProviderName, &row.Model, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 读取代理状态活跃请求行失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历代理状态活跃请求行失败: %w", err)
	}
	return results, nil
}

// AdminProxyStatusLastRequests 复刻 loadLastRequests。
//
// `DISTINCT ON (user_id)` 的 ORDER BY 必须与 Node 逐字一致（`updated_at DESC NULLS LAST, id DESC`）：
// 换一种排序就会把「最近一次」挑成另一行。
func (p *Pools) AdminProxyStatusLastRequests(
	ctx context.Context,
) ([]AdminProxyStatusLastRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT ON (mr.user_id)
		       mr.user_id, mr.id, mr.key, k.name,
		       mr.provider_id, p.name, mr.model, mr.updated_at
		FROM message_request mr
		JOIN providers p ON mr.provider_id = p.id AND p.deleted_at IS NULL
		LEFT JOIN keys k ON k.key = mr.key AND k.deleted_at IS NULL
		WHERE mr.deleted_at IS NULL
		  AND mr.is_replay = false
		  AND mr.status_code IS NOT NULL
		  AND (mr.blocked_by IS NULL OR mr.blocked_by <> 'warmup')
		ORDER BY mr.user_id, mr.updated_at DESC NULLS LAST, mr.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: 代理状态最近请求查询失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminProxyStatusLastRow, 0)
	for rows.Next() {
		var row AdminProxyStatusLastRow
		if err := rows.Scan(&row.UserID, &row.RequestID, &row.KeyString, &row.KeyName,
			&row.ProviderID, &row.ProviderName, &row.Model, &row.EndTime); err != nil {
			return nil, fmt.Errorf("store: 读取代理状态最近请求行失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历代理状态最近请求行失败: %w", err)
	}
	return results, nil
}

// adminActivityCanonicalIdentity 是 Node 的 messageSessionIdentity：
// `COALESCE(session_identity, session_id)`（activity-stream.ts:8-10）。
const adminActivityCanonicalIdentity = "COALESCE(mr.session_identity, mr.session_id)"

// adminActivitySelectColumns 是活动流两条查询共用的投影（含 JOIN）。
//
// keys 的 JOIN 照抄 Node：`leftJoin(keys, eq(messageRequest.key, keysTable.key))` —— 这里
// **没有** `keys.deleted_at is null` 条件（与 proxy-status 的 keys JOIN 不同），少写会改变
// keyName 的取值。
const adminActivitySelectColumns = `
		mr.id,
		` + adminActivityCanonicalIdentity + ` AS session_id,
		u.name,
		p.name,
		mr.model,
		mr.original_model,
		mr.status_code,
		mr.duration_ms,
		mr.cost_usd::text,
		mr.created_at
	FROM message_request mr
	LEFT JOIN users u ON mr.user_id = u.id
	LEFT JOIN keys k ON mr.key = k.key
	LEFT JOIN providers p ON mr.provider_id = p.id`

// AdminActivityLatestRequestsBySessions 复刻 findRecentActivityStream 的第 2 步：
// 活跃会话里每个规范 identity 取最新一条，再按 created_at 降序限 N。
//
// Node 的两层顺序都要照抄：内层 `orderBy(identity, desc(created_at))` 决定 DISTINCT ON 取哪条，
// 外层 `orderBy(desc(created_at)).limit(limit)` 决定最终截断。
func (p *Pools) AdminActivityLatestRequestsBySessions(
	ctx context.Context,
	sessionIdentities []string,
	limit int,
) ([]AdminActivityRow, error) {
	if len(sessionIdentities) == 0 || limit <= 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `
		SELECT * FROM (
			SELECT DISTINCT ON (` + adminActivityCanonicalIdentity + `)
				` + adminActivitySelectColumns + `
			WHERE mr.deleted_at IS NULL
			  AND mr.is_replay = false
			  AND ` + adminActivityCanonicalIdentity + ` = ANY($1::varchar[])
			ORDER BY ` + adminActivityCanonicalIdentity + `, mr.created_at DESC
		) latest
		ORDER BY created_at DESC
		LIMIT $2`
	rows, err := pool.Query(ctx, query, sessionIdentities, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 活动流活跃会话查询失败: %w", err)
	}
	defer rows.Close()
	return scanAdminActivityRows(rows)
}

// AdminActivityRecentRequests 复刻 findRecentActivityStream 的第 3 步：补齐数据库最新请求，
// 排除已包含的 identity。
func (p *Pools) AdminActivityRecentRequests(
	ctx context.Context,
	limit int,
	excludeIdentities []string,
) ([]AdminActivityRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	excluded := ""
	args := []any{limit}
	if len(excludeIdentities) > 0 {
		args = append(args, excludeIdentities)
		excluded = " AND " + adminActivityCanonicalIdentity + " <> ALL($2::varchar[])"
	}
	query := `
		SELECT ` + adminActivitySelectColumns + `
		WHERE mr.deleted_at IS NULL
		  AND mr.is_replay = false` + excluded + `
		ORDER BY mr.created_at DESC
		LIMIT $1`
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 活动流最新请求查询失败: %w", err)
	}
	defer rows.Close()
	return scanAdminActivityRows(rows)
}

// scanAdminActivityRows 读活动流行（两条查询共用投影，故扫描也共用）。
func scanAdminActivityRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]AdminActivityRow, error) {
	results := make([]AdminActivityRow, 0)
	for rows.Next() {
		var row AdminActivityRow
		if err := rows.Scan(&row.RequestID, &row.SessionID, &row.UserName, &row.ProviderName,
			&row.Model, &row.OriginalModel, &row.StatusCode, &row.DurationMS,
			&row.CostUSD, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 读取活动流行失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历活动流行失败: %w", err)
	}
	return results, nil
}
