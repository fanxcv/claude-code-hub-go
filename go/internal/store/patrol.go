package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// UnsettledRequest 是一行「已开行但从未终态」的 message_request。
//
// 这一行的存在本身就是异常：开行在守卫链的 messageContext 步骤，终态由数据面写回；
// 两者之间进程被杀（滚动重启、部署、SIGKILL）就会留下 status_code IS NULL 且
// updated_at = created_at 的行。
type UnsettledRequest struct {
	ID        int64
	CreatedAt time.Time
}

// UnsettledCursor 是「未终态行」分页的键游标。
//
// 为什么是 (created_at, id) 行值而不是单一 id：按 id 排序会让首轮从主键头部扫描，
// 而 NULL 行是极稀有的——那等于每轮全表扫。按 (created_at, id) 走
// idx_message_request_created_at_id_active 的索引序，配合 created_at < cutoff 的范围，
// 每页都只读「阈值之前」的那段。
//
// 成本缺口扫描（ListCostGapRequests）复用同一形状：它同样按 (created_at, id) 走同一索引，
// 且同样稀有，没有另立一种游标的理由。
type UnsettledCursor struct {
	CreatedAt time.Time
	ID        int64
}

// listUnsettledRequestsQuery 有意用行值比较而不是 OFFSET：行值比较在任一页返回空之前
// 严格推进，因此「修不好的行」不会把后续行永远挡住（OFFSET 分页会让失败行反复占据首页）。
const listUnsettledRequestsQuery = `
SELECT id, created_at
FROM message_request
WHERE status_code IS NULL
  AND deleted_at IS NULL
  AND created_at < $1
  AND (created_at, id) > ($2, $3)
ORDER BY created_at ASC, id ASC
LIMIT $4`

// CostGapRequest 是一行「终态已提交、成本却未落库」的 message_request。
//
// 开行时 cost_usd 与 status_code 同为 NULL；结算分两步落库（先终态、后成本，见
// terminal 的 ErrCostWriteFailed 路径）。两步之间进程被杀、或成本写入重试耗尽，就留下这一行：
// 终态在、成本永久为空。它不进 usage_ledger，金额静默少算——因此需要可观测的兜底。
type CostGapRequest struct {
	ID        int64
	CreatedAt time.Time
}

// listCostGapRequestsQuery 与 listUnsettledRequestsQuery 同形：行值游标、无 OFFSET，
// 唯一区别是谓词取反（终态已提交、成本仍空）。
//
// 谓词必须叠加 BillingCondition：cost_usd 为空的行里有一批是**设计上**就不该有成本的，
// 它们不是缺口，每轮报出来只会把真缺口淹掉。已知三类：
//   - 被拦截行（blocked_by 非空）：warmup 显式不写成本（见 guard 的 RecordWarmup：
//     「成本 NULL（显式不写，避免前端显示 $0）」），敏感词等拦截行写 0 而非 NULL；
//   - 回放审计行（is_replay）：成本归零、不经计费（见 drizzle/0116 的重放归一化）；
//   - 不计费端点（count_tokens / responses/compact）：见 store.NonBillingEndpoints
//     的注释——「这两类端点会写 message_request，但不得进入可计费口径」，
//     它们的响应不带 usage，结算本就不计费。
//
// 复用 BillingCondition 而不是就地另抄一份排除清单：可计费口径只能有一个真源，
// 否则账本与巡检会各自漂移。
const listCostGapRequestsQuery = `
SELECT id, created_at
FROM message_request
WHERE status_code IS NOT NULL
  AND cost_usd IS NULL
  AND deleted_at IS NULL
  AND ` + BillingCondition + `
  AND created_at < $1
  AND (created_at, id) > ($2, $3)
ORDER BY created_at ASC, id ASC
LIMIT $4`

// ListCostGapRequests 取 cutoff 之前、游标之后的成本缺口行（有界）。
// 走 control 分道，理由同 ListUnsettledRequests：后台巡检不挤占热路径与 writer 预算。
func (p *Pools) ListCostGapRequests(
	ctx context.Context,
	cutoff time.Time,
	after UnsettledCursor,
	limit int,
) ([]CostGapRequest, error) {
	if limit <= 0 {
		return nil, errors.New("store: 成本缺口行查询必须有正的批量上限")
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, listCostGapRequestsQuery, cutoff, after.CreatedAt, after.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询成本缺口行失败: %w", err)
	}
	defer rows.Close()

	results := make([]CostGapRequest, 0, limit)
	for rows.Next() {
		var item CostGapRequest
		if err := rows.Scan(&item.ID, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 读取成本缺口行失败: %w", err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历成本缺口行失败: %w", err)
	}
	return results, nil
}

// ListUnsettledRequests 取 cutoff 之前、游标之后的未终态行（有界）。
//
// 走 control 分道而不是 data/writer：本查询是后台巡检，既不该挤占热路径的读取预算，
// 更不该排在 writer 分道（预算为 1 条连接）上——那次排队会直接推迟在途请求的终态写入，
// 而巡检的本意恰恰是修补终态缺失。
func (p *Pools) ListUnsettledRequests(
	ctx context.Context,
	cutoff time.Time,
	after UnsettledCursor,
	limit int,
) ([]UnsettledRequest, error) {
	if limit <= 0 {
		return nil, errors.New("store: 未终态行查询必须有正的批量上限")
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, listUnsettledRequestsQuery, cutoff, after.CreatedAt, after.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询未终态行失败: %w", err)
	}
	defer rows.Close()

	// 只两列且列集固定，直接用 pgx 扫描而不经 row_to_json：本查询不消费可变的列集。
	results := make([]UnsettledRequest, 0, limit)
	for rows.Next() {
		var item UnsettledRequest
		if err := rows.Scan(&item.ID, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 读取未终态行失败: %w", err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历未终态行失败: %w", err)
	}
	return results, nil
}
