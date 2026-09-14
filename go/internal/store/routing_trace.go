package store

import (
	"context"
	"fmt"
)

// PersistRoutingTraceMonotonic 复刻 persistRoutingTraceMonotonically（Node
// src/repository/routing-trace-outbox.ts:355 + routing-trace-persistence.ts:22）。
//
// 语义：routing_trace.updatedAt 是**逻辑修订时钟**（每次变更严格递增），因此重放顺序不影响
// 最终值。只有待写入的 revision 大于库中已有 revision 时才覆盖，否则原样保留（含 updated_at）。
// 「库中 revision」的取法必须与 Node 逐字一致：非数字的 updatedAt 视作 -Infinity，
// 否则 `NULL < x` 求值为 NULL 会让首写被静默丢弃。
//
// 返回目标行是否存在：不存在（已删除 / 请求行尚未开）时调用方应丢弃该 outbox 条目。
func (p *Pools) PersistRoutingTraceMonotonic(
	ctx context.Context,
	requestID int64,
	trace []byte,
	revision float64,
) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}

	const storedRevision = `COALESCE(
		CASE
			WHEN jsonb_typeof(routing_trace->'updatedAt') = 'number'
			THEN (routing_trace->>'updatedAt')::numeric
		END,
		'-Infinity'::numeric
	)`

	rows, err := pool.Query(
		ctx,
		`UPDATE message_request SET
			routing_trace = CASE WHEN `+storedRevision+` < $2::numeric
				THEN $3::jsonb ELSE routing_trace END,
			updated_at = CASE WHEN `+storedRevision+` < $2::numeric
				THEN NOW() ELSE updated_at END
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING id`,
		requestID, revision, string(trace),
	)
	if err != nil {
		return false, fmt.Errorf("store: 路由轨迹单调落库失败: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("store: 路由轨迹单调落库读取失败: %w", err)
		}
		return false, nil
	}
	var id int64
	if err := rows.Scan(&id); err != nil {
		return false, fmt.Errorf("store: 路由轨迹单调落库扫描失败: %w", err)
	}
	return true, nil
}
