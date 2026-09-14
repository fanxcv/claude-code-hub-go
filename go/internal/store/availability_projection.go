package store

import (
	"context"
	"fmt"
)

// 本文件是可用性投影**回填**需要的写入面。
//
// 唯一真源：src/lib/availability/projection-worker.ts（bootstrapBackfill / enqueueBackfillChunk）。
//
// 分工：本文件只有「回填入队」与「回填完成标记」两件事。投影的增量消费（outbox → 1 分钟桶）
// 不在本轮范围内（既有 store/projection.go 只提供去重写入原语）。

// projectionBackfillDoneKey 是回填完成标记在 projection_meta 里的键名。
const projectionBackfillDoneKey = "backfill_done"

// ProjectionBackfillDone 对应 Node 的 `SELECT key FROM projection_meta WHERE key = 'backfill_done' LIMIT 1`。
func (p *Pools) ProjectionBackfillDone(ctx context.Context) (bool, error) {
	pool, err := p.Control()
	if err != nil {
		return false, err
	}
	var key string
	err = pool.QueryRow(ctx, `SELECT key FROM projection_meta WHERE key = $1 LIMIT 1`,
		projectionBackfillDoneKey).Scan(&key)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: 读取投影回填标记失败: %w", err)
	}
	return true, nil
}

// EnqueueProjectionBackfillChunk 复刻 enqueueBackfillChunk：把一个时间窗内的终态请求
// 以 request_finalized 事件入队到 outbox_events，返回本次实际入队的行数。
//
// 逐字对齐 Node 的谓词，因为它们共同决定「哪些历史请求会被投影」：
//   - status_code IS NOT NULL（未落终态的行不入队）
//   - created_at >= from AND < to（左闭右开，与分块游标一致）
//   - COALESCE(is_replay, false) = false
//   - NOT EXISTS (proj_applied_requests)：去重，避免与增量消费或将来的重跑重复计数
//   - fn_compute_message_request_success_rate_outcome(...) IS NOT NULL：结果不可判定的请求跳过
//
// payload 的字段集合与顺序无关（jsonb 会重排键），但字段清单必须一致。
func (p *Pools) EnqueueProjectionBackfillChunk(
	ctx context.Context,
	fromISO string,
	toISO string,
) (int, error) {
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	var inserted int
	if err := pool.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO outbox_events (event_type, aggregate_type, aggregate_id, occurred_at, payload)
			SELECT
				'request_finalized',
				'message_request',
				mr.id,
				mr.created_at,
				jsonb_build_object(
					'request_id', mr.id,
					'provider_id', mr.provider_id,
					'model', mr.model,
					'occurred_at', mr.created_at,
					'status_code', mr.status_code,
					'duration_ms', mr.duration_ms,
					'ttfb_ms', mr.ttfb_ms,
					'blocked_by', mr.blocked_by,
					'outcome', fn_compute_message_request_success_rate_outcome(
						mr.blocked_by, mr.status_code, mr.error_message, mr.provider_chain
					),
					'group_tag', p.group_tag,
					'is_replay', COALESCE(mr.is_replay, false)
				)
			FROM message_request mr
			LEFT JOIN providers p ON p.id = mr.provider_id
			WHERE mr.status_code IS NOT NULL
				AND mr.created_at >= $1::timestamptz
				AND mr.created_at < $2::timestamptz
				AND COALESCE(mr.is_replay, false) = false
				AND NOT EXISTS (SELECT 1 FROM proj_applied_requests a WHERE a.request_id = mr.id)
				AND fn_compute_message_request_success_rate_outcome(
						mr.blocked_by, mr.status_code, mr.error_message, mr.provider_chain
					) IS NOT NULL
			RETURNING 1
		)
		SELECT count(*)::int FROM inserted`, fromISO, toISO).Scan(&inserted); err != nil {
		return 0, fmt.Errorf("store: 投影回填入队失败: %w", err)
	}
	return inserted, nil
}

// MarkProjectionBackfillDone 复刻 Node 写 projection_meta 的那条 upsert。
//
// value 的形状与 Node 逐字段一致：{"at": now, "note": "availability backfill",
// "rangeDays": N, "inserted": N}。
func (p *Pools) MarkProjectionBackfillDone(
	ctx context.Context,
	rangeDays int,
	inserted int,
) error {
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO projection_meta (key, value, updated_at)
		VALUES (
			$1,
			jsonb_build_object(
				'at', now(),
				'note', 'availability backfill',
				'rangeDays', $2::int,
				'inserted', $3::int
			),
			now()
		)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		projectionBackfillDoneKey, rangeDays, inserted); err != nil {
		return fmt.Errorf("store: 写入投影回填标记失败: %w", err)
	}
	return nil
}
