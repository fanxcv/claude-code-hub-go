package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件实现可用性投影 outbox 消费侧所需的**全部事务内原语**：
// 认领未发布事件 → 幂等登记 → 累加 1 分钟桶 → 标记已发布 → 重算 avail_current。
//
// 复刻 src/lib/availability/projection-worker.ts:258-441（processBatch）与 :176-256
// （recomputeAvailCurrent）。**边界与 Node 完全一致：一个事务管完认领到重算**——
// Node 的 `db.transaction(...)` 就是这个范围，好处是任一步失败即整体回滚，
// 「认领」随事务中止自动释放（Node 侧没有独立的 visibility timeout，见报告 §1.2）。
//
// 为什么 SQL 放在 store 而编排放在 jobs：与本仓其余写路径同一分工——store 只认识表，
// jobs 只认识语义（可注入假实现做单测）。这里刻意把「一条语句一个方法」摊开，
// 便于用「假 tx」验证编排的正确性，而不必起真库。

// ProjectionOutboxEvent 是一条被认领的 outbox 事件。
type ProjectionOutboxEvent struct {
	// ID 是 outbox_events.id（bigserial）。
	ID int64
	// EventID 是 outbox_events.event_id（uuid 文本）。
	EventID string
	// Payload 是原始 jsonb（由调用方解析，与 Node 的 asPayload 同处分工）。
	Payload []byte
}

// ProjectionBucketDelta 是同一 (provider, 分钟桶) 内若干事件的累加结果。
type ProjectionBucketDelta struct {
	ProviderID    int64
	BucketStart   time.Time
	SuccessCnt    int
	FailureCnt    int
	ExcludedCnt   int
	LatencyCnt    int
	LatencySumMS  int64
	LastRequestAt time.Time
}

// ProjectionTx 是一次消费事务的句柄；所有方法都必须在该事务内调用。
type ProjectionTx struct {
	tx pgx.Tx
}

// RunProjectionTx 在 Writer 分道开一个事务，把句柄交给 fn；fn 返回错误则整体回滚。
//
// 用 Writer 分道而不是 Control：这一路是账务无关的批量写（投影桶），与 backfill 的入队
// （Control）分开，避免长事务占住控制面连接。
func (p *Pools) RunProjectionTx(ctx context.Context, fn func(ctx context.Context, tx *ProjectionTx) error) error {
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启投影消费事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, &ProjectionTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 投影消费事务提交失败: %w", err)
	}
	return nil
}

// ClaimOutboxBatch 认领一批未发布事件（`FOR UPDATE SKIP LOCKED`，按 id 升序）。
//
// 复刻 projection-worker.ts:259-266。SKIP LOCKED 让多实例可并发消费而不互等；本仓当前
// 单实例持锁消费（见报告 §3.1），语义仍与 Node 兼容。
func (t *ProjectionTx) ClaimOutboxBatch(ctx context.Context, limit int) ([]ProjectionOutboxEvent, error) {
	rows, err := t.tx.Query(ctx,
		`SELECT id, event_id, payload
		 FROM outbox_events
		 WHERE published_at IS NULL
		 ORDER BY id
		 FOR UPDATE SKIP LOCKED
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 认领投影事件失败: %w", err)
	}
	defer rows.Close()

	claimed := make([]ProjectionOutboxEvent, 0, limit)
	for rows.Next() {
		var (
			event ProjectionOutboxEvent
			raw   []byte
		)
		if err := rows.Scan(&event.ID, &event.EventID, &raw); err != nil {
			return nil, fmt.Errorf("store: 读取投影事件失败: %w", err)
		}
		event.Payload = raw
		claimed = append(claimed, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历投影事件失败: %w", err)
	}
	return claimed, nil
}

// InsertAppliedRequest 在事务内做幂等登记；返回 true 表示本次是新应用（投影应继续累加）。
//
// SQL 与 projection.go 的 InsertProjAppliedRequestWith 逐字相同——那份走 *Pool（非事务路径），
// 这份走 tx；两份共用一个语义：幂等键是 request_id。
func (t *ProjectionTx) InsertAppliedRequest(ctx context.Context, requestID int64, eventID string) (bool, error) {
	var returned int64
	err := t.tx.QueryRow(ctx,
		`INSERT INTO proj_applied_requests (request_id, event_id)
		 VALUES ($1, $2::uuid)
		 ON CONFLICT (request_id) DO NOTHING
		 RETURNING request_id`,
		requestID, eventID,
	).Scan(&returned)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 投影去重写入失败: %w", err)
	}
	return true, nil
}

// UpsertAvailBuckets 按 (provider_id, bucket_start) 升序累加写桶。
//
// 复刻 projection-worker.ts:369-405：冲突时**相加**（不是覆盖），last_request_at 取 GREATEST。
// 升序是为并发实例取锁顺序一致（Node 同注释：avoids deadlocks）。
func (t *ProjectionTx) UpsertAvailBuckets(ctx context.Context, deltas []ProjectionBucketDelta) error {
	sorted := make([]ProjectionBucketDelta, len(deltas))
	copy(sorted, deltas)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ProviderID != sorted[j].ProviderID {
			return sorted[i].ProviderID < sorted[j].ProviderID
		}
		return sorted[i].BucketStart.Before(sorted[j].BucketStart)
	})

	for _, delta := range sorted {
		if _, err := t.tx.Exec(ctx,
			`INSERT INTO avail_bucket_1m AS b (
			   provider_id, bucket_start, success_cnt, failure_cnt, excluded_cnt,
			   latency_cnt, latency_sum_ms, last_request_at
			 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (provider_id, bucket_start) DO UPDATE SET
			   success_cnt = b.success_cnt + EXCLUDED.success_cnt,
			   failure_cnt = b.failure_cnt + EXCLUDED.failure_cnt,
			   excluded_cnt = b.excluded_cnt + EXCLUDED.excluded_cnt,
			   latency_cnt = b.latency_cnt + EXCLUDED.latency_cnt,
			   latency_sum_ms = b.latency_sum_ms + EXCLUDED.latency_sum_ms,
			   last_request_at = GREATEST(
			     COALESCE(b.last_request_at, EXCLUDED.last_request_at),
			     EXCLUDED.last_request_at
			   )`,
			delta.ProviderID,
			delta.BucketStart,
			delta.SuccessCnt,
			delta.FailureCnt,
			delta.ExcludedCnt,
			delta.LatencyCnt,
			delta.LatencySumMS,
			delta.LastRequestAt,
		); err != nil {
			return fmt.Errorf("store: 累加可用性桶失败: %w", err)
		}
	}
	return nil
}

// MarkOutboxPublished 标记这批事件已发布（attempts +1）。
//
// 复刻 projection-worker.ts:407-441：正常行 last_error 置 NULL（清掉历史错误），
// 毒丸行写 last_error='invalid payload'——两者都把 published_at 置上，
// **毒丸因此不会阻塞后续批次**（这是 Node 的既有处理方式，不是我们的发明）。
func (t *ProjectionTx) MarkOutboxPublished(ctx context.Context, ids []int64, lastError *string) error {
	if len(ids) == 0 {
		return nil
	}
	sorted := make([]int64, len(ids))
	copy(sorted, ids)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	if _, err := t.tx.Exec(ctx,
		`UPDATE outbox_events
		 SET published_at = now(), attempts = attempts + 1, last_error = $2
		 WHERE id = ANY($1)`,
		sorted, lastError,
	); err != nil {
		return fmt.Errorf("store: 标记投影事件已发布失败: %w", err)
	}
	return nil
}

// RecomputeAvailCurrent 按 15 分钟窗口重算给定供应商的 avail_current（两条语句）。
//
// 复刻 projection-worker.ts:176-256。顺序与 Node 一致：先 upsert 有流量的、再把窗口内
// 无流量的置 unknown（「诚实的空态」）。两条都按 provider_id 升序（第二条用 FOR UPDATE 先锁行）。
func (t *ProjectionTx) RecomputeAvailCurrent(ctx context.Context, providerIDs []int64, windowMinutes int) error {
	if len(providerIDs) == 0 {
		return nil
	}
	sorted := make([]int64, len(providerIDs))
	copy(sorted, providerIDs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	if _, err := t.tx.Exec(ctx,
		`INSERT INTO avail_current AS c (
		   provider_id, state, availability, request_count, last_request_at, updated_at
		 )
		 SELECT
		   s.provider_id, s.state, s.availability, s.request_count, s.last_request_at, s.updated_at
		 FROM (
		   SELECT
		     b.provider_id,
		     CASE
		       WHEN COALESCE(SUM(b.success_cnt), 0) + COALESCE(SUM(b.failure_cnt), 0) <= 0 THEN 'unknown'
		       WHEN (COALESCE(SUM(b.success_cnt), 0)::float
		         / (COALESCE(SUM(b.success_cnt), 0) + COALESCE(SUM(b.failure_cnt), 0))) >= 0.8 THEN 'green'
		       WHEN (COALESCE(SUM(b.success_cnt), 0)::float
		         / (COALESCE(SUM(b.success_cnt), 0) + COALESCE(SUM(b.failure_cnt), 0))) >= 0.5 THEN 'yellow'
		       ELSE 'red'
		     END AS state,
		     CASE
		       WHEN COALESCE(SUM(b.success_cnt), 0) + COALESCE(SUM(b.failure_cnt), 0) <= 0 THEN 0
		       ELSE COALESCE(SUM(b.success_cnt), 0)::float
		         / (COALESCE(SUM(b.success_cnt), 0) + COALESCE(SUM(b.failure_cnt), 0))
		     END AS availability,
		     (COALESCE(SUM(b.success_cnt), 0) + COALESCE(SUM(b.failure_cnt), 0))::int AS request_count,
		     MAX(b.last_request_at) AS last_request_at,
		     now() AS updated_at
		   FROM avail_bucket_1m b
		   WHERE b.provider_id = ANY($1)
		     AND b.bucket_start >= now() - ($2 * INTERVAL '1 minute')
		   GROUP BY b.provider_id
		   ORDER BY b.provider_id ASC
		 ) s
		 ON CONFLICT (provider_id) DO UPDATE SET
		   state = EXCLUDED.state,
		   availability = EXCLUDED.availability,
		   request_count = EXCLUDED.request_count,
		   last_request_at = EXCLUDED.last_request_at,
		   updated_at = now()`,
		sorted, windowMinutes,
	); err != nil {
		return fmt.Errorf("store: 重算可用性现状失败: %w", err)
	}

	if _, err := t.tx.Exec(ctx,
		`WITH targets AS (
		   SELECT c.provider_id
		   FROM avail_current c
		   WHERE c.provider_id = ANY($1)
		     AND NOT EXISTS (
		       SELECT 1 FROM avail_bucket_1m b
		       WHERE b.provider_id = c.provider_id
		         AND b.bucket_start >= now() - ($2 * INTERVAL '1 minute')
		         AND (b.success_cnt + b.failure_cnt) > 0
		     )
		   ORDER BY c.provider_id ASC
		   FOR UPDATE OF c
		 )
		 UPDATE avail_current c
		 SET state = 'unknown', availability = 0, request_count = 0, updated_at = now()
		 FROM targets t
		 WHERE c.provider_id = t.provider_id`,
		sorted, windowMinutes,
	); err != nil {
		return fmt.Errorf("store: 重置无流量供应商的可用性现状失败: %w", err)
	}
	return nil
}

// DecodeProjectionPayload 解析 outbox payload（复刻 Node 的 asPayload：容忍字符串与对象）。
//
// 放在 store 只为就近复用 json 解码；语义上与表无关，故导出以便 jobs 侧单独测。
func DecodeProjectionPayload(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return map[string]any{}
	}
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case string:
		var nested any
		if err := json.Unmarshal([]byte(typed), &nested); err != nil {
			return map[string]any{}
		}
		if obj, ok := nested.(map[string]any); ok {
			return obj
		}
		return map[string]any{}
	default:
		return map[string]any{}
	}
}
