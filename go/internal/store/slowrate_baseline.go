package store

import (
	"context"
	"fmt"
	"time"
)

// 低速降级基线的读取面（《渠道低速降级》B3）。
//
// 为什么 SQL 落在 store：本仓的数据读取查询一律在 store，jobs 只做编排与调度
// （对照 admin_cache_effectiveness_job.go 与 jobs/cache_effectiveness.go 的分工）。

// slowRateModelKeyExpr 是「模型键」的 SQL 表达。
//
// 它镜像 pubstatus.ResolveSuccessRateModelKey：original_model 优先，空则回落 model。
// 该函数收 *string，无法在 SQL 里调用，故以表达式复刻一份；两处必须同口径，
// 否则 B3 算的基线与公开状态页 / 排行榜的成功率口径会对不上。改任一处都要同步另一处。
const slowRateModelKeyExpr = `COALESCE(NULLIF(btrim(mr.original_model), ''), NULLIF(btrim(mr.model), ''))`

// slowRateSampleCleaningPredicate 是样本清洗条件（设计稿 §2）。基线与判定共用同一组条件。
//
//   - status_code = 200：失败请求的 duration 不含正常生成语义；
//   - output_tokens >= 50：短输出速率受帧解析与网络往返主导，与生成能力无关；
//   - duration_ms > first_byte_ms：分母必须为正；
//   - first_byte_ms / duration_ms <= 0.9：剔除「整包一次到达」的伪高值。实证：r>=500 的样本
//     里 92%~97% 满足 fb/dur > 0.9，中位生成窗仅 89~128ms——不剔除会把假尾算进基线。
const slowRateSampleCleaningPredicate = `
		mr.deleted_at IS NULL
		AND mr.status_code = 200
		AND mr.output_tokens >= 50
		AND mr.first_byte_ms IS NOT NULL
		AND mr.duration_ms IS NOT NULL
		AND mr.duration_ms > mr.first_byte_ms
		AND mr.duration_ms > 0
		AND mr.first_byte_ms::float8 / mr.duration_ms::float8 <= 0.9`

// SlowRateScopeSamples 是单个（渠道 × 模型）在两个窗口内的样本计数。
//
// 两个窗口互不重叠：W1 = [w1Start, now)，W2 = [w2Start, w1Start)。设计稿要求「取 W2 单独的中位数
// 而非 W1∪W2 合并」，故计数必须分开报，不能只给一个总数。
type SlowRateScopeSamples struct {
	ProviderID int64
	ModelKey   string
	// W1Samples 是主窗（最近 3 天）内的清洗后样本数。
	W1Samples int64
	// W2Samples 是扩展窗（now-30d 到 now-3d）内的清洗后样本数。
	W2Samples int64
}

// SlowRateScopeCounts 数出给定渠道在各（渠道 × 模型）组合上的两窗样本数。
//
// 为什么先数数再取行：中位数必须在 Go 侧算（复用 pubstatus.ComputeTokensPerSecond，
// 避免第二套速率口径），但直接拉 30 天的原始行在大渠道上不可控。先用一条纯聚合拿到计数，
// 只有真正要发布基线的组合才去拉行（见 SlowRateScopeRates）。
//
// providerIDs 为空时返回空集且不发查询——这是「未开启监控的渠道零开销」的实现基础。
func (p *Pools) SlowRateScopeCounts(
	ctx context.Context,
	providerIDs []int64,
	w1Start int64,
	w2Start int64,
) ([]SlowRateScopeSamples, error) {
	if len(providerIDs) == 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT
		mr.provider_id,
		` + slowRateModelKeyExpr + ` AS model_key,
		COUNT(*) FILTER (WHERE mr.created_at >= to_timestamp($1::float8 / 1000)) AS w1_samples,
		COUNT(*) FILTER (WHERE mr.created_at <  to_timestamp($1::float8 / 1000)) AS w2_samples
	FROM message_request mr
	WHERE ` + slowRateSampleCleaningPredicate + `
		AND mr.provider_id::bigint = ANY($3::bigint[])
		AND mr.created_at >= to_timestamp($2::float8 / 1000)
		AND ` + slowRateModelKeyExpr + ` IS NOT NULL
	GROUP BY 1, 2`

	rows, err := pool.Query(ctx, query, w1Start, w2Start, providerIDs)
	if err != nil {
		return nil, fmt.Errorf("store: 统计低速基线样本数失败: %w", err)
	}
	defer rows.Close()

	out := make([]SlowRateScopeSamples, 0, len(providerIDs))
	for rows.Next() {
		var item SlowRateScopeSamples
		if err := rows.Scan(&item.ProviderID, &item.ModelKey, &item.W1Samples, &item.W2Samples); err != nil {
			return nil, fmt.Errorf("store: 读取低速基线样本计数失败: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历低速基线样本计数失败: %w", err)
	}
	return out, nil
}

// SlowRateOldestSampleAt 返回给定渠道集合中最老的清洗后样本时刻；无任何样本时返回 nil。
//
// 为什么需要它：扩展窗 W2 的设计上界是「30 天前」。但 message_request 会被 logcleanup
// 按保留天数**物理删除**（`jobs/logcleanup.go` 的 logCleanupDefaultRetentionDays = 30），
// 即表本身可能比 30 天短。此时查 30 天只会去扫一段不存在的区间（索引扫描虽快，但没意义）。
// 调用方用本函数把 W2 下界 clamp 到「表内实际最老样本」，避免空扫。
//
// 返回 nil 且 err 为 nil 表示「该集合内一条清洗后样本都没有」——这与「读失败」是两回事，
// 调用方据此决定是回退到 30 天还是报错。
//
// providerIDs 为空时返回 (nil, nil)，不发查询。
func (p *Pools) SlowRateOldestSampleAt(ctx context.Context, providerIDs []int64) (*time.Time, error) {
	if len(providerIDs) == 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT MIN(mr.created_at)
	FROM message_request mr
	WHERE ` + slowRateSampleCleaningPredicate + `
		AND mr.provider_id::bigint = ANY($1::bigint[])`

	var oldest *time.Time
	if err := pool.QueryRow(ctx, query, providerIDs).Scan(&oldest); err != nil {
		return nil, fmt.Errorf("store: 读取低速样本最老时刻失败: %w", err)
	}
	return oldest, nil
}

// SlowRateRateRow 是算中位数所需的最小一行事实。
//
// 只取三列：速率由 Go 侧 pubstatus.ComputeTokensPerSecond 算，故这里原样带出它的三个入参。
type SlowRateRateRow struct {
	OutputTokens *int64
	DurationMS   *float64
	FirstByteMS  *float64
}

// SlowRateScopeRates 取单个（渠道 × 模型）在指定窗口内的清洗后原始行。
//
// 只取该 scope 的原始事实、不做任何速率计算：速率口径的唯一真源是
// pubstatus.ComputeTokensPerSecond，若在 SQL 里再写一遍公式，就成了第二套度量。
//
// limit 是行数上限（调用方传 ceiling+1 以探测截断）。触顶时只是样本被截短，
// 中位数仍有界可用，但调用方必须记日志——见 jobs/slowrate_baseline.go 的说明。
//
// 排序取 `created_at DESC`：触顶时保留**最近**的样本（贴近当前形态），而非窗口起点的旧样本。
// 走 `idx_message_request_provider_created_at_active` 的倒序扫描。
func (p *Pools) SlowRateScopeRates(
	ctx context.Context,
	providerID int64,
	modelKey string,
	windowStart int64,
	windowEnd int64,
	limit int,
) ([]SlowRateRateRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT mr.output_tokens, mr.duration_ms::float8, mr.first_byte_ms::float8
	FROM message_request mr
	WHERE ` + slowRateSampleCleaningPredicate + `
		AND mr.provider_id = $1
		AND ` + slowRateModelKeyExpr + ` = $2
		AND mr.created_at >= to_timestamp($3::float8 / 1000)
		AND mr.created_at <  to_timestamp($4::float8 / 1000)
	ORDER BY mr.created_at DESC
	LIMIT $5`

	rows, err := pool.Query(ctx, query, providerID, modelKey, windowStart, windowEnd, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 读取低速基线样本失败: %w", err)
	}
	defer rows.Close()

	out := make([]SlowRateRateRow, 0, 256)
	for rows.Next() {
		var item SlowRateRateRow
		if err := rows.Scan(&item.OutputTokens, &item.DurationMS, &item.FirstByteMS); err != nil {
			return nil, fmt.Errorf("store: 读取低速基线样本行失败: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历低速基线样本行失败: %w", err)
	}
	return out, nil
}

// SlowRateProviderConfig 是「已开启低速监控」的渠道及其逐渠道参数覆写。
//
// 参数列可空（B1 的迁移：NULL 表示取代码默认值），故一律用指针；调用方负责回落默认值。
type SlowRateProviderConfig struct {
	ProviderID    int64
	MinSamples    *int
	RatioPerMille *int
	// WindowSeconds 是 **B2 判定滑窗**的长度（秒）：慢样本 ZSET 保留多久、状态键 TTL 多久。
	//
	// 它**不是**基线主窗——两者尺度差三个数量级（分钟 vs 天），共用一列会让「用户把判定窗调成
	// 30 分钟」顺带把基线打坏（生产实证：wb 设 1800 后 B3 改按 30 分钟聚合，样本凑不齐
	// min_samples，基线落到扩展窗并降级为 extended_stale，渠道级降权静默失效）。故拆列。
	WindowSeconds *int
	// BaselineWindowSeconds 是 **B3 基线主窗 W1** 的长度（秒）：从 message_request 聚合多长
	// 历史来算中位数。NULL/非正回落 slowRateBaselineW1Span（3 天）。
	BaselineWindowSeconds *int
}

// SlowRateEnabledProviders 读回**已开启低速监控且未软删**的渠道及其参数覆写。
//
// 这是「未开启渠道零开销」的实现基础（设计稿 §8）：本任务每轮只问一次这张清单，
// 未开启的渠道既不进计数查询、也不写键。查询走 providers 表的小结果集（开启的应是少数）。
func (p *Pools) SlowRateEnabledProviders(ctx context.Context) ([]SlowRateProviderConfig, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT id, slow_rate_min_samples, slow_rate_ratio_per_mille, slow_rate_window_seconds,
		slow_rate_baseline_window_seconds
	FROM providers
	WHERE deleted_at IS NULL
		AND is_enabled = true
		AND slow_rate_monitor_enabled = true
	ORDER BY id`

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: 读取已开启低速监控的渠道失败: %w", err)
	}
	defer rows.Close()

	out := make([]SlowRateProviderConfig, 0, 8)
	for rows.Next() {
		var item SlowRateProviderConfig
		if err := rows.Scan(&item.ProviderID, &item.MinSamples, &item.RatioPerMille, &item.WindowSeconds, &item.BaselineWindowSeconds); err != nil {
			return nil, fmt.Errorf("store: 读取低速监控渠道行失败: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历低速监控渠道失败: %w", err)
	}
	return out, nil
}
