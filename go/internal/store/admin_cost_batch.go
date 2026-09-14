package store

import (
	"context"
	"fmt"
)

// 本文件实现「批量累计成本读数」——`POST /api/v1/users:costBatch` 与 `/keys:costBatch` 的取数层。
//
// 唯一真源（Node）：`src/repository/statistics.ts` 的 `sumUserTotalCostBatch` / `sumKeyTotalCostBatchByIds`，
// 以及它们**在改造前那一版 SSR 页面**里的实际调用方式
// （`src/app/[locale]/dashboard/quotas/users/page.tsx`，见提交 `6fdde976^`）：
//
//	sumUserTotalCostBatch(allUserIds, Infinity, userResetAtMap)
//	sumKeyTotalCostBatchByIds(allKeyIds, Infinity, keyResetAtMap)
//	userResetAtMap: user.costResetAt
//	keyResetAtMap:  resolveKeyCostResetAt(key.costResetAt, user.costResetAt)   // 取两者较晚者
//
// 逐条等价性（这是本文件唯一的正确性依据，改动前先复核）：
//
//  1. **聚合表是 `usage_ledger`，不是 `message_request`**。派单曾按后者描述，与代码不符；
//     以数为准：Node 的两个函数（`statistics.ts:568-700`）都 `from(usageLedger)`。
//  2. **计费条件** = `BillingCondition`（复刻 `LEDGER_BILLING_CONDITION`：排除 blocked_by、
//     is_replay 与非计费端点）。
//  3. **时间上限**：调用方传 `Infinity` ⇒ **不设下界**（与「总额度」语义一致，不是 365 天默认值——
//     365 只是这两个函数的形参默认，改造前的调用方显式传了 Infinity）。
//  4. **重置语义**：Node 对「带 resetAt 的实体」逐条单查（`created_at >= resetAt`），
//     其余走一次 GROUP BY。本实现用 `created_at >= COALESCE(<reset>, '-infinity')` 的
//     **相关子查询/连接**一次算完，语义相同（无 reset ⇒ 无下界）且避免 N+1。
//     密钥维度另有一处：`resolveKeyCostResetAt` 取 **key 与 user 两个重置时刻中较晚者**，
//     用 Postgres 的 `GREATEST` 表达（它忽略 NULL，与 `resolveLaterResetAt` 同判）。
//  5. **入参里每个 id 都有结果**（无账本行或实体不存在 ⇒ `"0"`），与 Node 预填 0 一致。
//  6. 成本以 **numeric 文本** 返回，不在取数层做浮点转换（与 `SumLedgerTotalCost` 同一条纪律）。

// UserCostBatchTotalCost 批量返回用户的**全时段**可计费成本合计（键为 user id，值为 numeric 文本）。
//
// 不设时间下界、按各用户自己的 `cost_reset_at` 起算；id 不存在或账本无行时为 "0"。
func (p *Pools) UserCostBatchTotalCost(ctx context.Context, userIDs []int64) (map[int64]string, error) {
	result := make(map[int64]string, len(userIDs))
	if len(userIDs) == 0 {
		return result, nil
	}
	for _, id := range userIDs {
		result[id] = "0"
	}

	// JOIN users 只为取该用户的 cost_reset_at；用户不存在时该 id 没有账本行可言，
	// 预填的 "0" 即为答案（与 Node 一致）。
	query := fmt.Sprintf(
		`SELECT l.user_id::text AS entity_id, COALESCE(SUM(l.cost_usd), 0)::text AS total
		   FROM usage_ledger l
		   JOIN users u ON u.id = l.user_id
		  WHERE l.user_id = ANY($1)
		    AND %s
		    AND l.created_at >= COALESCE(u.cost_reset_at, '-infinity'::timestamptz)
		  GROUP BY l.user_id`,
		BillingCondition,
	)

	// `= ANY($1)` 收的是**单个数组参数**（pgx 直接编码 []int64），不是展开后的元素序列。
	return p.scanBatchTotalsAsInt64Keys(ctx, query, []any{userIDs}, result, "用户")
}

// KeyCostBatchTotalCost 批量返回密钥的**全时段**可计费成本合计（键为 key id，值为 numeric 文本）。
//
// 与 Node 同一套两步语义：先按 PK 把 key id 解析成 key 字符串，再在账本上按 key 列聚合
// （Node 刻意不 JOIN varchar，以命中 `idx_usage_ledger_key_cost`）；本实现用 JOIN 表达，
// 查询计划由 PG 决定，语义与结果一致。
//
// 重置时刻取 **key 与 user 两者较晚者**（`resolveKeyCostResetAt`），用 `GREATEST` 表达（忽略 NULL）。
func (p *Pools) KeyCostBatchTotalCost(ctx context.Context, keyIDs []int64) (map[int64]string, error) {
	result := make(map[int64]string, len(keyIDs))
	if len(keyIDs) == 0 {
		return result, nil
	}
	for _, id := range keyIDs {
		result[id] = "0"
	}

	// LEFT JOIN users：密钥所属用户可能已被删除（deleted_at 非空但行仍在），此时 u 侧为 NULL，
	// GREATEST 退化为 key 自身的时间（与 resolveLaterResetAt 在有一侧为 null 时同判）。
	query := fmt.Sprintf(
		`SELECT k.id::text AS entity_id, COALESCE(SUM(l.cost_usd), 0)::text AS total
		   FROM keys k
		   JOIN usage_ledger l ON l.key = k.key
		   LEFT JOIN users u ON u.id = k.user_id
		  WHERE k.id = ANY($1)
		    AND %s
		    AND l.created_at >= COALESCE(GREATEST(k.cost_reset_at, u.cost_reset_at), '-infinity'::timestamptz)
		  GROUP BY k.id`,
		BillingCondition,
	)

	// 同上：单数组参数。
	return p.scanBatchTotalsAsInt64Keys(ctx, query, []any{keyIDs}, result, "密钥")
}

// scanBatchTotalsAsInt64Keys 执行批量成本查询并把结果写回预填了 "0" 的 map。
//
// 单独抽出来是因为两个维度的 SQL 只差表与重置表达式，扫描与错误语义必须逐字一致
// （否则「用户维度报错但密钥维度不报」这类差异会在读侧表现为界面上一半数字消失）。
func (p *Pools) scanBatchTotalsAsInt64Keys(
	ctx context.Context,
	query string,
	args []any,
	result map[int64]string,
	label string,
) (map[int64]string, error) {
	pool, err := p.Data()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 批量%s成本查询失败: %w", label, err)
	}
	defer rows.Close()
	for rows.Next() {
		var entityID string
		var total string
		if err := rows.Scan(&entityID, &total); err != nil {
			return nil, fmt.Errorf("store: 读取批量%s成本行失败: %w", label, err)
		}
		var id int64
		if _, err := fmt.Sscan(entityID, &id); err != nil {
			return nil, fmt.Errorf("store: 批量%s成本返回了非数字主键 %q: %w", label, entityID, err)
		}
		result[id] = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 批量%s成本遍历失败: %w", label, err)
	}
	return result, nil
}
