package store

import (
	"context"
	"fmt"
	"time"
)

// 本文件是管理面 me 资源（自服务面 /api/v1/me/*）的读面，逐条对齐 Node 的
// src/actions/my-usage.ts 与 src/repository/usage-logs.ts 的 for-key 变体。
//
// 与 usage-logs 资源读面的关系：那条面按**筛选器**读（管理员的视角，可跨用户）；
// 这条面把主体固定成**当前调用方自己的密钥**，因此不暴露任何主体筛选参数——
// 越权在类型层面就不可能（Node 侧同样如此：findUsageLogsForKey 的签名里没有 keyId 之外的维度）。
//
// 读走 control 分道（与 usage-logs 一致；TS 侧 /api/v1 的 getDb() 在非 data scope 时取 control）。

// MeTodayModelRow 是 me/today 的一行模型聚合。
//
// 字段与 Node 的 select 一一对应（my-usage.ts:552-576）：成本以 numeric 文本返回再由上层转数，
// token 列是 double precision。
type MeTodayModelRow struct {
	Model         *string
	OriginalModel *string
	Calls         int64
	CostUSD       string
	InputTokens   float64
	OutputTokens  float64
}

// MeTodayLedgerBreakdown 复刻 getMyTodayStats 的聚合查询（my-usage.ts:556-576）。
//
// 口径细节（都照抄）：
//   - 过滤列是 usage_ledger.key 的**密钥串**（Node 的 eq(usageLedger.key, session.key.key)），
//     不是 key_id。
//   - 上界是**排他**的（Node 用 lt(createdAt, endTime)）。
//   - 没有 ORDER BY：Node 也没有，行序由 PG 决定。
func (p *Pools) MeTodayLedgerBreakdown(
	ctx context.Context,
	keyValue string,
	start time.Time,
	end time.Time,
) ([]MeTodayModelRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT model,
		       original_model,
		       count(*)::int AS calls,
		       COALESCE(sum(cost_usd), 0)::text AS cost_usd,
		       COALESCE(sum(input_tokens), 0)::double precision AS input_tokens,
		       COALESCE(sum(output_tokens), 0)::double precision AS output_tokens
		FROM usage_ledger
		WHERE key = $1 AND `+BillingCondition+`
		  AND created_at >= $2 AND created_at < $3
		GROUP BY model, original_model`,
		keyValue, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: 自服务日用量聚合失败: %w", err)
	}
	defer rows.Close()

	results := make([]MeTodayModelRow, 0, 8)
	for rows.Next() {
		var row MeTodayModelRow
		if err := rows.Scan(
			&row.Model, &row.OriginalModel, &row.Calls,
			&row.CostUSD, &row.InputTokens, &row.OutputTokens,
		); err != nil {
			return nil, fmt.Errorf("store: 读取自服务日用量行失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历自服务日用量行失败: %w", err)
	}
	return results, nil
}
