package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// CacheEffectivenessAdvisoryLockKey 是缓存效果窗口聚合的**事务级** advisory 锁键。
//
// 与 Node 逐字一致（`src/lib/cache-effectiveness/service.ts:22` 的 `LOCK_KEY = 20260722`）：
// 切换期两侧抢同一把锁，避免 Node 与 Go 同时聚合同一窗口、写出互相重叠的历史行。
//
// 取值落在**单键（bigint）**锁空间：Node 传的是字面量 `20260722`（int4），PG 在单参调用下
// 走 `pg_try_advisory_xact_lock(bigint)`，故这里也必须显式 `$1::bigint`——若误用双键形态
// `(int, int)`，两侧会各自拿到一把不冲突的锁，互斥静默失效。
const CacheEffectivenessAdvisoryLockKey int64 = 20260722

// CacheEffectivenessEnabledSetting 读 `system_settings.cache_effectiveness_enabled`。
//
// 返回 nil 表示**未设置**：Node 的语义是 `settings.cacheEffectivenessEnabled ?? env`
// （`src/lib/system-settings/proxy-runtime.ts:77-78`），故 nil 必须与 false 区分——
// 前者回落 env，后者是显式关闭。无行时同样返回 nil（Node 侧取首行，无行即未设置）。
func (p *Pools) CacheEffectivenessEnabledSetting(ctx context.Context) (*bool, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	var enabled *bool
	if err := pool.QueryRow(ctx,
		`SELECT cache_effectiveness_enabled FROM system_settings ORDER BY id ASC LIMIT 1`,
	).Scan(&enabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return enabled, nil
}

// TryCacheEffectivenessXactLock 尝试取事务级 advisory 锁，语义同 Node 的
// `SELECT pg_try_advisory_xact_lock(20260722)`（`service.ts:48-51`）：
// 返回 false 表示另一实例正在聚合，本轮应**整轮跳过**（不排队、不等待——Node 同样直接返回）。
//
// 锁随事务结束（提交或回滚）自动释放，故调用方必须在本事务内完成聚合。
func (p *Pools) TryCacheEffectivenessXactLock(ctx context.Context, tx pgx.Tx) (bool, error) {
	var acquired bool
	if err := tx.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock($1::bigint)`, CacheEffectivenessAdvisoryLockKey,
	).Scan(&acquired); err != nil {
		return false, err
	}
	return acquired, nil
}

// LastCacheEffectivenessWindowEnd 返回已聚合窗口的最大终点（Node `service.ts:63-65`）。
//
// 这是窗口推进的唯一依据：下一轮的起点就是它，故窗口之间既不重叠也不留缝。
func (p *Pools) LastCacheEffectivenessWindowEnd(ctx context.Context, tx pgx.Tx) (*time.Time, error) {
	var last *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT MAX(window_end) FROM provider_cache_effectiveness`,
	).Scan(&last); err != nil {
		return nil, err
	}
	return last, nil
}

// InsertCacheEffectivenessWindow 按 `{provider_id, model, cache_ttl_bucket}` 聚合窗口，
// 写入历史行并返回写入的分组数（Node 的 `groupsWritten`，`service.ts:147`）。
//
// 与 Node 逐字对齐（`src/lib/cache-effectiveness/service.ts:86-145`）：
//   - 过滤四件套：`cache_compatibility_key IS NOT NULL`、`deleted_at IS NULL`、`provider_id > 0`、
//     `created_at ∈ [windowStart, windowEnd)`（左闭右开——窗口推进时不重不漏靠它）；
//   - 分组键：`COALESCE(model,”)`、`COALESCE(cache_ttl_bucket,'5m')`；
//   - 定点整数（万分比 bp），禁浮点；PG 整数除法向零截断，与 Node 的 `::int` 同义；
//   - 样本量分档 `>=100 → 10000`、`>=30 → 6000`、`>=5 → 3000`、否则 `1000`；
//   - `confidence_bp = observable_bp × sample_factor_bp / 10000`；
//     `effectiveness_bp = raw_bp × confidence_bp / 10000`；
//   - 写入用**普通 INSERT**（无 ON CONFLICT）：不重叠由窗口推进保证，不靠 upsert 去重。
//
// 一条语句完成「分组 → 定点数学 → 写入 → 计数」：拆成多语句会在 `MAX(window_end)` 与写入
// 之间留出竞态窗口，而 Node 正是靠单事务闭合它。计数用外层 CTE 取 `count(*)`（等价于 Node 的
// `RETURNING id` 后取数组长度），省一次往返。
func (p *Pools) InsertCacheEffectivenessWindow(
	ctx context.Context,
	tx pgx.Tx,
	windowStart time.Time,
	windowEnd time.Time,
) (int, error) {
	var written int
	if err := tx.QueryRow(ctx, `
		WITH grouped AS (
			SELECT
				mr.provider_id,
				COALESCE(mr.model, '') AS model,
				COALESCE(mr.cache_ttl_bucket, '5m') AS cache_ttl_bucket,
				COUNT(*)::bigint AS sample_count,
				COUNT(*) FILTER (WHERE mr.cache_score_eligible)::bigint AS eligible_count,
				COALESCE(SUM(mr.theoretical_cache_tokens) FILTER (WHERE mr.cache_score_eligible), 0)::bigint AS theoretical_tokens,
				COALESCE(SUM(mr.cache_read_input_tokens) FILTER (WHERE mr.cache_score_eligible), 0)::bigint AS observed_tokens
			FROM message_request mr
			WHERE mr.cache_compatibility_key IS NOT NULL
				AND mr.deleted_at IS NULL
				AND mr.provider_id > 0
				AND mr.created_at >= $1
				AND mr.created_at < $2
			GROUP BY mr.provider_id, COALESCE(mr.model, ''), COALESCE(mr.cache_ttl_bucket, '5m')
		),
		scored AS (
			SELECT
				g.*,
				CASE
					WHEN g.theoretical_tokens > 0
					THEN GREATEST(LEAST((g.observed_tokens * 10000) / g.theoretical_tokens, 10000), 0)::int
					ELSE 0
				END AS raw_bp,
				CASE
					WHEN g.eligible_count >= 100 THEN 10000
					WHEN g.eligible_count >= 30 THEN 6000
					WHEN g.eligible_count >= 5 THEN 3000
					ELSE 1000
				END AS sample_factor_bp,
				CASE
					WHEN g.sample_count > 0
					THEN ((g.eligible_count * 10000) / g.sample_count)::int
					ELSE 0
				END AS observable_bp
			FROM grouped g
		),
		inserted AS (
			INSERT INTO provider_cache_effectiveness (
				provider_id, model, cache_ttl_bucket, window_start, window_end,
				sample_count, eligible_count, theoretical_cache_tokens, observed_cache_read_tokens,
				raw_effectiveness_bp, confidence_bp, effectiveness_bp
			)
			SELECT
				s.provider_id,
				s.model,
				s.cache_ttl_bucket,
				$1,
				$2,
				s.sample_count,
				s.eligible_count,
				s.theoretical_tokens,
				s.observed_tokens,
				s.raw_bp,
				((s.observable_bp * s.sample_factor_bp) / 10000)::int,
				((s.raw_bp * ((s.observable_bp * s.sample_factor_bp) / 10000)) / 10000)::int
			FROM scored s
			RETURNING id
		)
		SELECT count(*)::int FROM inserted
	`, windowStart.UTC(), windowEnd.UTC()).Scan(&written); err != nil {
		return 0, err
	}
	return written, nil
}
