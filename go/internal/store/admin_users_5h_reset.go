package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ResetAdminUser5hCostMarker 只推进用户级 5h 成本重置标记，**不动 cost_reset_at**。
//
// 对应 Node 的 `updateUserCostResetMarkers({ limit5hCostResetAt, enforceLimit5hMonotonic: true })`
// （`src/repository/user.ts`）：列值取 `greatest(coalesce(col, $1), $1)`，故重复重置不会把标记
// 回退（单调）。与同文件的 ResetAdminUserCostMarkers 刻意分开——后者同时推进
// `cost_reset_at` 与 `limit_5h_cost_reset_at`，对应 UI 上的「重置限额」按钮；本方法对应
// 「仅重置 5H 限额」按钮。合并两者会让后者悄悄扩大到全量语义。
//
// 返回 false 表示用户不存在或已软删（与 Node 的 `result.length > 0` 同判据，调用方据此回
// USER_NOT_FOUND）。`updated_at` 用同一个 resetAt 而不是再取一次 now：Node 侧两者是相邻的
// 两次 `new Date()`，差异只在毫秒，这里按同一时刻写更可预期。
func (p *Pools) ResetAdminUser5hCostMarker(
	ctx context.Context,
	userID int64,
	resetAt time.Time,
) (bool, error) {
	pool, err := p.Control()
	if err != nil {
		return false, err
	}
	var returnedID int64
	err = pool.QueryRow(ctx,
		`UPDATE users
		    SET limit_5h_cost_reset_at = greatest(coalesce(limit_5h_cost_reset_at, $1::timestamptz), $1::timestamptz),
		        updated_at = $1::timestamptz
		  WHERE id = $2 AND deleted_at IS NULL
		  RETURNING id`,
		resetAt, userID,
	).Scan(&returnedID)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 重置用户 5h 成本标记失败: %w", err)
	}
	return true, nil
}
