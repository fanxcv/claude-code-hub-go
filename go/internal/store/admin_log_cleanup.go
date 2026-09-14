package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本文件是 usage 日志清理（/api/admin/log-cleanup/manual）的存储层。
//
// 唯一真源：src/lib/log-cleanup/service.ts。SQL 的形状、批大小（10000）、批间隔（100ms）、
// 两阶段删除（先删活跃行、再清软删行）与 VACUUM 的非致命语义都照该文件。
//
// 几条必须保留的性质：
//  1. usage_ledger **不参与**清理（该文件顶部显式声明）。本层只动 message_request。
//  2. 无条件的清理请求**不做任何删除**，直接回 "No cleanup conditions specified"（防误删全表）。
//  3. 每批用 CTE + FOR UPDATE SKIP LOCKED，避免与并发任务死锁。
//  4. VACUUM 不能在事务块里跑，且失败不影响结果（只记 vacuumPerformed=false）。

// AdminLogCleanupConditions 是清理条件（全部可空；全空即拒绝执行）。
type AdminLogCleanupConditions struct {
	BeforeDate      *time.Time
	AfterDate       *time.Time
	UserIDs         []int64
	ProviderIDs     []int64
	StatusCodes     []int
	StatusCodeRange *AdminStatusCodeRange
	OnlyBlocked     bool
}

// AdminStatusCodeRange 是状态码闭区间。
type AdminStatusCodeRange struct {
	Min int
	Max int
}

// AdminLogCleanupResult 对应 Node 的 CleanupResult（字段名原样，供 handler 直出）。
type AdminLogCleanupResult struct {
	TotalDeleted      int64  `json:"totalDeleted"`
	BatchCount        int    `json:"batchCount"`
	DurationMS        int64  `json:"durationMs"`
	SoftDeletedPurged int64  `json:"softDeletedPurged"`
	VacuumPerformed   bool   `json:"vacuumPerformed"`
	Error             string `json:"error,omitempty"`
}

// adminLogCleanupBatchSize 复刻 `options.batchSize || 10000`。
const adminLogCleanupBatchSize = 10000

// adminLogCleanupBatchSleep 复刻 BATCH_SLEEP_MS：一批删满 10000 行后歇 100ms，给库喘口气。
const adminLogCleanupBatchSleep = 100 * time.Millisecond

// AdminCleanupUsageLogs 复刻 cleanupLogs。
//
// dryRun 为 true 时只数不删（Node 同法）。
func (p *Pools) AdminCleanupUsageLogs(
	ctx context.Context,
	conditions AdminLogCleanupConditions,
	dryRun bool,
) AdminLogCleanupResult {
	started := time.Now()
	result := AdminLogCleanupResult{}

	where, args := conditions.adminWhereClause()
	if where == "" {
		result.DurationMS = time.Since(started).Milliseconds()
		result.Error = "No cleanup conditions specified"
		return result
	}

	pool, err := p.Writer()
	if err != nil {
		result.DurationMS = time.Since(started).Milliseconds()
		result.Error = err.Error()
		return result
	}

	if dryRun {
		count, err := p.adminCountUsageLogs(ctx, pool, where, args)
		result.DurationMS = time.Since(started).Milliseconds()
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.TotalDeleted = count
		return result
	}

	activeWhere := where + " AND deleted_at IS NULL"
	for {
		deleted, err := p.adminDeleteUsageLogBatch(ctx, pool, activeWhere, args)
		if err != nil {
			result.DurationMS = time.Since(started).Milliseconds()
			result.Error = err.Error()
			return result
		}
		if deleted == 0 {
			break
		}
		result.TotalDeleted += deleted
		result.BatchCount++
		if deleted == adminLogCleanupBatchSize {
			select {
			case <-ctx.Done():
				result.DurationMS = time.Since(started).Milliseconds()
				result.Error = ctx.Err().Error()
				return result
			case <-time.After(adminLogCleanupBatchSleep):
			}
		}
	}

	purgeWhere := where + " AND deleted_at IS NOT NULL"
	for {
		deleted, err := p.adminDeleteUsageLogBatch(ctx, pool, purgeWhere, args)
		if err != nil {
			result.DurationMS = time.Since(started).Milliseconds()
			result.Error = err.Error()
			return result
		}
		if deleted == 0 {
			break
		}
		result.SoftDeletedPurged += deleted
		if deleted == adminLogCleanupBatchSize {
			select {
			case <-ctx.Done():
				result.DurationMS = time.Since(started).Milliseconds()
				result.Error = ctx.Err().Error()
				return result
			case <-time.After(adminLogCleanupBatchSleep):
			}
		}
	}

	if result.TotalDeleted > 0 || result.SoftDeletedPurged > 0 {
		result.VacuumPerformed = p.adminRunVacuum(ctx)
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

// adminCountUsageLogs 是 dryRun 的计数查询。
func (p *Pools) adminCountUsageLogs(
	ctx context.Context,
	pool *Pool,
	where string,
	args []any,
) (int64, error) {
	var count int64
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*)::bigint FROM message_request WHERE "+where, args...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: 统计待清理日志失败: %w", err)
	}
	return count, nil
}

// adminDeleteUsageLogBatch 删一批（CTE + FOR UPDATE SKIP LOCKED + RETURNING 1），返回删除行数。
func (p *Pools) adminDeleteUsageLogBatch(
	ctx context.Context,
	pool *Pool,
	whereClause string,
	args []any,
) (int64, error) {
	// 批大小是包内常量，直接内联（它不来自请求，不存在注入面）。
	query := `WITH ids_to_delete AS (
		SELECT id FROM message_request
		WHERE ` + whereClause + `
		ORDER BY created_at ASC
		LIMIT ` + strconv.Itoa(adminLogCleanupBatchSize) + `
		FOR UPDATE SKIP LOCKED
	)
	DELETE FROM message_request WHERE id IN (SELECT id FROM ids_to_delete) RETURNING 1`
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: 清理日志失败: %w", err)
	}
	defer rows.Close()

	var deleted int64
	for rows.Next() {
		deleted++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: 清理日志失败: %w", err)
	}
	return deleted, nil
}

// adminRunVacuum 跑 VACUUM ANALYZE message_request；失败只回 false（Node 同法）。
//
// 用底层池而不是分道池：VACUUM 不能在事务里执行，而分道池的 Exec 会经过准入与（潜在的）上下文
// 包装，直接走 Raw 更贴近 Node 的 `db.execute(sql\`VACUUM ...\`)`。
func (p *Pools) adminRunVacuum(ctx context.Context) bool {
	pool, err := p.Writer()
	if err != nil {
		return false
	}
	if _, err := pool.Raw().Exec(ctx, "VACUUM ANALYZE message_request"); err != nil {
		return false
	}
	return true
}

// adminWhereClause 复刻 buildWhereConditions：把条件拼成 SQL 片段与参数（不含 deleted_at）。
//
// 返回空串表示「没有任何条件」，调用方据此拒绝执行。
func (c AdminLogCleanupConditions) adminWhereClause() (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 6)
	placeholder := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}

	if c.BeforeDate != nil {
		clauses = append(clauses, "created_at <= "+placeholder(*c.BeforeDate)+"::timestamptz")
	}
	if c.AfterDate != nil {
		clauses = append(clauses, "created_at >= "+placeholder(*c.AfterDate)+"::timestamptz")
	}
	if len(c.UserIDs) > 0 {
		clauses = append(clauses, "user_id = ANY("+placeholder(c.UserIDs)+"::int[])")
	}
	if len(c.ProviderIDs) > 0 {
		clauses = append(clauses, "provider_id = ANY("+placeholder(c.ProviderIDs)+"::int[])")
	}
	if len(c.StatusCodes) > 0 {
		clauses = append(clauses, "status_code = ANY("+placeholder(c.StatusCodes)+"::int[])")
	}
	if c.StatusCodeRange != nil {
		clauses = append(clauses, "status_code BETWEEN "+placeholder(c.StatusCodeRange.Min)+
			" AND "+placeholder(c.StatusCodeRange.Max))
	}
	if c.OnlyBlocked {
		clauses = append(clauses, "blocked_by IS NOT NULL")
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return strings.Join(clauses, " AND "), args
}
