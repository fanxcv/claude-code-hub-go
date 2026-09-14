package store

import (
	"context"
	"fmt"
	"time"
)

// 本文件是用户统计重置的落库面，复刻 src/lib/user-statistics-reset/reset-service.ts 的四段 SQL。
//
// 分道：删除与更新走 Writer（写路径），只读的存在性检查与键清单走 Data。与 Node 的差别仅在于
// Node 用同一个 `db` 连接，Go 侧按「读走 Data、写走 Writer」的既有纪律分开（见 pool.go 的分道注释）。

// 重置分批大小，与 reset-service.ts:8 的 RESET_BATCH_SIZE 一致。
const UserStatisticsResetBatchSize = 1000

// AdminUserResetKey 是重置要清 Redis 缓存的一把密钥：id 与**明文密钥**。
//
// 明文密钥不可省：`total_cost:key:<明文>` 是 Node 的既有键布局（cost-cache-cleanup.ts:283-296
// 把 key.key 当作 keyHash 传进扫描模式），少了它就清不掉按键取值的总消费缓存。
type AdminUserResetKey struct {
	ID  int64
	Key string
}

// ListAdminUserResetKeys 复刻 findUserStatisticsResetKeyIds（reset-service.ts:9-16）并多带明文密钥。
//
// 只取未软删的键（deleted_at IS NULL）：软删的键已经不在限额体系里，清它的缓存只会多花一次扫描。
func (p *Pools) ListAdminUserResetKeys(ctx context.Context, userID int64) ([]AdminUserResetKey, error) {
	pool, err := p.Data()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(
		ctx,
		`SELECT id, key FROM keys WHERE user_id = $1 AND deleted_at IS NULL`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 查询用户名下密钥失败: %w", err)
	}
	defer rows.Close()

	keys := make([]AdminUserResetKey, 0, 8)
	for rows.Next() {
		var item AdminUserResetKey
		if err := rows.Scan(&item.ID, &item.Key); err != nil {
			return nil, fmt.Errorf("store: 读取用户名下密钥失败: %w", err)
		}
		keys = append(keys, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历用户名下密钥失败: %w", err)
	}
	return keys, nil
}

// DrainAdminUserMessageRequests 删除该用户 cut 时刻及之前的一批 message_request，返回删除行数。
//
// 逐字复刻 reset-service.ts:60-79：CTE 里 `FOR UPDATE SKIP LOCKED` 选出至多 batch 行，
// 再按 id 删除。**必须带 SKIP LOCKED**：并发写入（数据面正在结算同一用户的请求）持有行锁时，
// 不带它就让整个重置卡在等锁上；SKIP LOCKED 让本轮跳过那些行，由 drain 循环的尾部检查发现残留
// 并报 ROWS_LOCKED（可重试），而不是静默漏删。
//
// message_request 的谓词含 `created_at IS NULL`：该列历史上有空值行，漏掉它们等于「重置后
// 统计里仍有旧数据」，且这些行永远删不掉。
func (p *Pools) DrainAdminUserMessageRequests(
	ctx context.Context,
	userID int64,
	cut time.Time,
	batch int,
) (int64, error) {
	return p.drainUserResetBatch(ctx, `
		WITH doomed AS (
			SELECT id
			FROM message_request
			WHERE user_id = $1
				AND (created_at IS NULL OR created_at <= $2)
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM message_request mr
		USING doomed
		WHERE mr.id = doomed.id`, userID, cut, batch)
}

// DrainAdminUserUsageLedger 删除该用户 cut 时刻及之前的一批 usage_ledger（reset-service.ts:81-100）。
//
// 与 message_request 的差别：usage_ledger.created_at 是非空列（未加 Nullable），故谓词里没有
// 空值分支——多写一个 `IS NULL` 只会让计划器少用一个可用的索引条件。
func (p *Pools) DrainAdminUserUsageLedger(
	ctx context.Context,
	userID int64,
	cut time.Time,
	batch int,
) (int64, error) {
	return p.drainUserResetBatch(ctx, `
		WITH doomed AS (
			SELECT id
			FROM usage_ledger
			WHERE user_id = $1
				AND created_at <= $2
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM usage_ledger ul
		USING doomed
		WHERE ul.id = doomed.id`, userID, cut, batch)
}

// drainUserResetBatch 执行一条分批删除（两条语句只差表名与谓词）。
func (p *Pools) drainUserResetBatch(
	ctx context.Context,
	query string,
	userID int64,
	cut time.Time,
	batch int,
) (int64, error) {
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, query, userID, cut, batch)
	if err != nil {
		return 0, fmt.Errorf("store: 分批删除用户统计失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// HasAdminUserRemainingRows 判断该用户是否还有 cut 时刻及之前的行（reset-service.ts:102-122）。
//
// 为什么要这一问：分批删除走到「最后一批不足 batch」并不等于删干净了——并发写入者的行锁会让
// SKIP LOCKED 跳过它们。这一问把「仍有残留」变成一个显式错误（ROWS_LOCKED，可重试），
// 而不是让重置以「成功」收尾却留下旧数据。
func (p *Pools) HasAdminUserRemainingRows(
	ctx context.Context,
	table string,
	userID int64,
	cut time.Time,
) (bool, error) {
	pool, err := p.Data()
	if err != nil {
		return false, err
	}
	var query string
	switch table {
	case "message_request":
		query = `SELECT EXISTS (
			SELECT 1 FROM message_request
			WHERE user_id = $1 AND (created_at IS NULL OR created_at <= $2)
		)`
	case "usage_ledger":
		query = `SELECT EXISTS (
			SELECT 1 FROM usage_ledger
			WHERE user_id = $1 AND created_at <= $2
		)`
	default:
		// 表名来自本包内部的常量集合，不接受调用方传字面量：拼进 SQL 的标识符必须是编译期已知的。
		return false, fmt.Errorf("store: 未知的重置目标表 %q", table)
	}
	var remaining bool
	if err := pool.QueryRow(ctx, query, userID, cut).Scan(&remaining); err != nil {
		return false, fmt.Errorf("store: 检查用户统计残留失败: %w", err)
	}
	return remaining, nil
}

// ClearAdminUserResetTimestamps 复刻 reset-service.ts:223-233 的 users 更新。
//
// 语义：把「成本重置标记」推回 NULL——**只针对切点及之前的标记**（`<= cut` 时置空，之后的保留）。
// 保留更新时刻之后的标记，是因为那代表管理端在请求排队期间刚做过一次重置，抹掉它等于丢掉一次
// 已经生效的重置时点。
func (p *Pools) ClearAdminUserResetTimestamps(
	ctx context.Context,
	userID int64,
	cut time.Time,
) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx, `
		UPDATE users
		SET cost_reset_at = CASE
				WHEN cost_reset_at IS NULL OR cost_reset_at <= $2 THEN NULL
				ELSE cost_reset_at
			END,
			limit_5h_cost_reset_at = CASE
				WHEN limit_5h_cost_reset_at IS NULL OR limit_5h_cost_reset_at <= $2 THEN NULL
				ELSE limit_5h_cost_reset_at
			END,
			updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`,
		userID, cut,
	)
	if err != nil {
		return false, fmt.Errorf("store: 清除用户成本重置标记失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
