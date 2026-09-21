package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 渠道低速降级的**基线定时任务**（《渠道低速降级》B3）。
//
// 职责：每小时对**已开启监控的渠道**逐「渠道 × 模型」重算历史中位数，把基线写进 Redis，
// 供 B2 的判定侧（读 slowLine）与 B4 的选路侧（读 source / median）消费。本块**不做**
// 样本写入（B2）、不做选路集成（B4）。
//
// 设计口径（基线窗口、四种「空」的语义、键与 JSON 形状）见仓库外的本地方案稿
// docs/design-slow-provider-demotion.md（已不进库），改动本文件的语义前先与之一致。
const (
	// SlowRateBaselineTaskName 是任务名（与同包既有任务同风格：小写短横线）。
	SlowRateBaselineTaskName = "slow-rate-baseline"

	// SlowRateBaselineDefaultEvery 是重算间隔（设计稿 §3：基线是慢变量，每小时一次）。
	SlowRateBaselineDefaultEvery = time.Hour

	// slowRateBaselineTaskTimeout 是单轮上限。
	//
	// 每轮要跑「一次计数聚合 + N 个组合的行读取」，N 是已开启监控的渠道数（设计稿 D2 默认全关，
	// 开启的应是少数）。给 10 分钟：正常秒级完成；超出即整轮取消并计一次失败，
	// 不让一个卡死的任务永久占住调度循环（与同包 defaultTaskTimeout 同判）。
	slowRateBaselineTaskTimeout = 10 * time.Minute

	// SlowRateBaselineKeyTTL 是基线键的存活期（设计稿 §4：7 天）。
	//
	// 与「扩展窗 30 天」无关：W2 是**查询窗口**不是缓存窗口。TTL 7 天保证渠道长期静默后
	// 键自然消失（设计稿 §5「长期无流量 → TTL 到期」），下次有流量再算。
	SlowRateBaselineKeyTTL = 7 * 24 * time.Hour

	// slowRateBaselineW1Span 是主窗长度（设计稿 §3：默认 3 天）。
	slowRateBaselineW1Span = 3 * 24 * time.Hour

	// slowRateBaselineW2Span 是扩展窗的**下界**跨度（设计稿 §3：now-30d 到 now-3d）。
	//
	// 为何 30 天：覆盖多数「静默但未下线」的场景，同时避免查到早已不存在的旧形态。
	// 但 message_request 会被 logcleanup 按保留天数**物理删除**（默认 30 天，见
	// jobs/logcleanup.go 的 logCleanupDefaultRetentionDays）——W2 的外沿恰好压在保留边界上，
	// 实际可用范围由表内最老行决定，故查询下界取「30 天」与「表内最老行」的较晚者（见
	// store.SlowRateOldestSampleAt 与 RunOnce 里的 clampW2Start）。
	slowRateBaselineW2Span = 30 * 24 * time.Hour

	// slowRateBaselineDefaultMinSamples 是样本下限（设计稿 §3：100 条）。
	//
	// 基线可信度与判定可信度**故意取同值**（设计稿 §3「三个下限不可混用」一节）：
	// 用两个不同数字会制造「基线看似可信但判定不该做」的灰色地带。
	slowRateBaselineDefaultMinSamples = 100

	// slowRateBaselineStaleFloorSamples 是 A4 的门槛（设计稿 §3：W1 有 ≥ 10 条样本）。
	//
	// W1 不达标但**有**这点样本，说明渠道**刚刚**恢复流量（不是长期静默），
	// 此时发布 extended_stale 基线只供会话级降级用。
	slowRateBaselineStaleFloorSamples = 10

	// slowRateBaselineDefaultRatioPerMille 是低速线系数的默认值（设计稿 §3 原为 0.2，
	// 用户 2026-09-21 改为 0.3）。
	//
	// 用千分比整数存（300 = 0.3），与 providers 表的 *_per_mille 列同形（B1）。
	slowRateBaselineDefaultRatioPerMille = 300

	// slowRateBaselineSampleCeiling 是单个 scope 拉取样本行的上限。
	//
	// 中位数只需要中位那个值，拉全窗的行在大渠道上不可控。取 20000：远大于样本下限（100），
	// 大到中位数几乎不可能因截断而偏（2 万条里取中位，截断只影响尾部）；触顶时记日志，
	// 不静默。
	slowRateBaselineSampleCeiling = 20000
)

// SlowRateBaselineOptions 是基线任务的构造参数。
type SlowRateBaselineOptions struct {
	// Pools 是共享连接池；必填（nil 时构造返回错误，装配缺陷应显式可见）。
	Pools *store.Pools
	// Redis 是基线键的写入面；必填（基线只存在于 Redis，未配置时任务无意义）。
	Redis redis.UniversalClient
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Now 可注入时钟；nil 时用 time.Now。
	Now func() time.Time
	// EnabledProviders 返回「已开启低速监控」的渠道及其逐渠道参数覆写。
	//
	// 这个缝是为**零开销**而留（设计稿 §8「未开启渠道的零开销保证」）：装配侧从配置快照读，
	// 本任务每轮只问一次。未开启的渠道不查询、不写键。
	EnabledProviders func(ctx context.Context) ([]store.SlowRateProviderConfig, error)
}

// SlowRateBaseline 每小时重算渠道低速基线。
type SlowRateBaseline struct {
	pools            *store.Pools
	redis            redis.UniversalClient
	logger           *logx.Logger
	now              func() time.Time
	enabledProviders func(ctx context.Context) ([]store.SlowRateProviderConfig, error)
}

// NewSlowRateBaseline 构造任务；Pools 或 Redis 为 nil 时报错。
func NewSlowRateBaseline(options SlowRateBaselineOptions) (*SlowRateBaseline, error) {
	if options.Pools == nil {
		return nil, errors.New("jobs: 低速基线任务需要数据库连接池")
	}
	if options.Redis == nil {
		return nil, errors.New("jobs: 低速基线任务需要 Redis 连接")
	}
	if options.EnabledProviders == nil {
		return nil, errors.New("jobs: 低速基线任务需要已开启监控渠道的来源")
	}
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &SlowRateBaseline{
		pools:            options.Pools,
		redis:            options.Redis,
		logger:           logger,
		now:              now,
		enabledProviders: options.EnabledProviders,
	}, nil
}

// Task 返回可登记进 Scheduler 的任务定义（照同包 CacheEffectiveness.Task 的形状）。
func (b *SlowRateBaseline) Task(interval time.Duration) Task {
	if interval <= 0 {
		interval = SlowRateBaselineDefaultEvery
	}
	return Task{
		Name:     SlowRateBaselineTaskName,
		Interval: interval,
		Timeout:  slowRateBaselineTaskTimeout,
		Run: func(ctx context.Context) error {
			_, err := b.RunOnce(ctx)
			return err
		},
	}
}

// SlowRateBaselineResult 是单轮结果（供日志与巡检断言）。
type SlowRateBaselineResult struct {
	// Scopes 是本轮评估的（渠道 × 模型）组合数。
	Scopes int
	// Published 是写入基线键的组合数。
	Published int
	// Skipped 是按 A1/A3 未发布（无基线）的组合数。
	Skipped int
	// Swept 是清扫掉的残留键数（渠道已停用 / 模型已删）。
	Swept int
	// Truncated 是样本触顶被截断的组合数。
	Truncated int
}

// BaselineSource 是基线来源的标记（设计稿 §3 的 A1-A4 四种情形）。
type BaselineSource string

const (
	// BaselineSourcePrimary 是 A0：主窗 W1 达标，直接用 W1（最常见）。
	BaselineSourcePrimary BaselineSource = "primary"
	// BaselineSourceExtended 是 A2：曾有请求、3 天内静默；回退扩展窗 W2。
	BaselineSourceExtended BaselineSource = "extended"
	// BaselineSourceExtendedStale 是 A4：刚从故障/下线恢复；W2 基线**降级使用**——
	// 只做会话级降级，不做渠道级 penalty（该判定由 B4 消费 source 得出）。
	BaselineSourceExtendedStale BaselineSource = "extended_stale"
)

// SlowRateBaselinePayload 是基线键的 JSON 值（设计稿 §4，形状**冻结**）。
//
// 消费方约定：B2 只读 SlowLine 做判定；B4 读 Source 与 Median。
type SlowRateBaselinePayload struct {
	// Median 是基线（历史中位数，tok/s）。
	Median float64 `json:"median"`
	// Samples 是算出该中位数的样本数。
	Samples int64 `json:"samples"`
	// ComputedAt 是本条的计算时刻（Unix 毫秒）。
	ComputedAt int64 `json:"computedAt"`
	// Source 是来源标记：primary / extended / extended_stale。
	Source BaselineSource `json:"source"`
	// SlowLine 是低速线 = Median × 系数，已乘好；判定侧直接比较，不再自己乘。
	SlowLine float64 `json:"slowLine"`
}

// BaselineKey 返回某个（渠道 × 模型）的基线键。
//
// 形制 `cch:slow:{<providerID>:<modelKey>}:baseline`：花括号是 Redis Cluster hash tag，
// 与既有 `cch:pfx:{<scopeTag>}` 同构——同一组合的多个键（samples/state/baseline）落同一槽，
// 多键操作才不会被 CROSSSLOT 拒绝（B2 写的 samples/state 用同一 tag）。
func BaselineKey(providerID int64, modelKey string) string {
	return fmt.Sprintf("cch:slow:{%d:%s}:baseline", providerID, modelKey)
}

// baselineKeyPrefix 是清扫时的扫描前缀（无 hash tag：扫全量再按内容判定）。
const baselineKeyPrefix = "cch:slow:"

// baselineKeySuffix 用于从键名反解出 scope。
const baselineKeySuffix = ":baseline"

// RunOnce 执行一轮基线重算。
func (b *SlowRateBaseline) RunOnce(ctx context.Context) (SlowRateBaselineResult, error) {
	var result SlowRateBaselineResult

	configs, err := b.enabledProviders(ctx)
	if err != nil {
		return result, fmt.Errorf("jobs: 读取已开启监控渠道失败: %w", err)
	}
	// 未开启任何监控时：既不查询也不写键，但仍做一次清扫（可能有刚被关掉的渠道留下残留键）。
	if len(configs) == 0 {
		swept, sweepErr := b.sweepStaleKeys(ctx, nil)
		result.Swept = swept
		if sweepErr != nil {
			// 清扫失败不整轮失败：基线才是本职，残留键只会多占内存。
			b.logger.Warn("slow_rate_baseline_sweep_failed", map[string]any{"error": sweepErr.Error()})
		}
		return result, nil
	}

	now := b.now()
	// 主窗长度可逐渠道覆写（slow_rate_baseline_window_seconds），而窗口边界进的是同一条计数查询，
	// 故按「有效窗口长度」分组，每组一次查询。默认全用同一值时只有一组。
	byWindow := groupProviderConfigsByWindow(configs)
	// 本轮实际遇到的 scope 集合，供清扫判定（不在集合内且存在的键即残留）。
	seen := make(map[string]struct{}, len(configs))

	for windowSeconds, group := range byWindow {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		w1Start := now.Add(-effectiveW1Span(windowSeconds))
		w2Floor := b.clampW2Floor(ctx, now, providerIDsOf(group))

		counts, err := b.pools.SlowRateScopeCounts(ctx,
			providerIDsOf(group), w1Start.UnixMilli(), w2Floor.UnixMilli())
		if err != nil {
			return result, err
		}

		byID := make(map[int64]store.SlowRateProviderConfig, len(group))
		for _, config := range group {
			byID[config.ProviderID] = config
		}

		for _, scope := range counts {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			result.Scopes++
			seen[BaselineKey(scope.ProviderID, scope.ModelKey)] = struct{}{}

			config := byID[scope.ProviderID]
			published, truncated, err := b.publishScope(ctx, scope, w1Start, w2Floor, now, config)
			if err != nil {
				// 单个 scope 失败不整轮失败：其余组合的基线照发。
				b.logger.Warn("slow_rate_baseline_scope_failed", map[string]any{
					"providerId": scope.ProviderID,
					"modelKey":   scope.ModelKey,
					"error":      err.Error(),
				})
				continue
			}
			if truncated {
				result.Truncated++
			}
			if published {
				result.Published++
			} else {
				result.Skipped++
			}
		}
	}

	swept, sweepErr := b.sweepStaleKeys(ctx, seen)
	result.Swept = swept
	if sweepErr != nil {
		b.logger.Warn("slow_rate_baseline_sweep_failed", map[string]any{"error": sweepErr.Error()})
	}

	return result, nil
}

// clampW2Floor 求扩展窗的实际下界。
//
// 设计上是「30 天前」，但 message_request 会被 logcleanup 按保留天数**物理删除**
// （jobs/logcleanup.go 的 logCleanupDefaultRetentionDays = 30），表本身可能比 30 天短。
// 取「30 天前」与「表内最老样本时刻」的较晚者，避免去扫一段不存在的区间。
//
// 读不到最老行时**不阻断**：退回纯 30 天这个更宽的界（多扫一点，但结果不错）。
func (b *SlowRateBaseline) clampW2Floor(
	ctx context.Context,
	now time.Time,
	providerIDs []int64,
) time.Time {
	floor := now.Add(-slowRateBaselineW2Span)
	oldest, err := b.pools.SlowRateOldestSampleAt(ctx, providerIDs)
	if err != nil {
		b.logger.Warn("slow_rate_baseline_oldest_unavailable", map[string]any{
			"error":  err.Error(),
			"action": "退回 30 天窗口下界",
		})
		return floor
	}
	if oldest != nil && oldest.After(floor) {
		return *oldest
	}
	return floor
}

// publishScope 决定并写入单个 scope 的基线，返回是否发布与是否被截断。
//
// 四种「空」在此分流（设计稿 §3 的关键章节）：
//
//	A0 W1 达标                      -> 用 W1，source=primary
//	A1 全期无行（不会走到：SlowRateScopeCounts 只返回有行的组合）
//	A2 W1 不足、W2 达标且 W1 < 10   -> 用 W2，source=extended
//	A3 W1 与 W2 都不足且 W1 < 10    -> 不发布并**撤销旧键**（fail-open）
//	A4 W1 不足但 >= 10 条、W2 达标  -> 用 W2，source=extended_stale（只供会话级降级）
func (b *SlowRateBaseline) publishScope(
	ctx context.Context,
	scope store.SlowRateScopeSamples,
	w1Start time.Time,
	w2Floor time.Time,
	now time.Time,
	config store.SlowRateProviderConfig,
) (published bool, truncated bool, err error) {
	decision := DecideBaseline(scope.W1Samples, scope.W2Samples, effectiveMinSamples(config.MinSamples))
	if !decision.Publish {
		// A3：本轮**成功**判定为无基线（样本不足），必须把旧键撤掉。
		//
		// 为何必须撤：基线键 TTL 是 7 天，而 A3 意味着当前形态已不足以支撑一条基线；留着旧键
		// 会让 recorder 继续据陈旧中位数判慢、写 state 与冷却，直到 TTL 到期（重建/形态变化后的
		// 组合尤其如此）。设计稿对 A3 的要求是 fail-open，而读侧只在**键不存在**时才 fail-open，
		// 所以「删键」正是 fail-open 的落地方式。
		//
		// 与「查询失败」的分界：查询失败时本函数在上面的 err 分支提前返回，根本走不到这里，
		// 旧键因此保留——那是设计稿明定的「宁可留着，也不要在查询抖动时把基线清空」。
		return false, false, b.revokeBaseline(ctx, scope)
	}

	var windowStart, windowEnd time.Time
	if decision.UseExtendedWindow {
		// 取 W2 **单独**的中位数，**不与 W1 合并**：合并会让陈旧数据稀释当前形态，且两窗样本量
		// 差异会把结果偏向样本多的一侧（设计稿 §3 明确要求）。
		windowStart, windowEnd = w2Floor, w1Start
	} else {
		windowStart, windowEnd = w1Start, now
	}

	median, total, cut, err := b.medianForWindow(ctx, scope, windowStart, windowEnd)
	if err != nil {
		return false, false, err
	}
	if median == nil {
		// 计数达标但算不出中位数（清洗后速率全为 nil 的极端情形）——按无基线处理。
		return false, cut, nil
	}
	return true, cut, b.writeBaseline(ctx, scope, *median, total, decision.Source, now, config)
}

// BaselineDecision 是一次「该不该发布基线、用哪个窗、标什么来源」的结论。
type BaselineDecision struct {
	// Publish 为 false 表示不发布基线（fail-open，不写键）。
	Publish bool
	// UseExtendedWindow 为 true 时取扩展窗 W2 的中位数，false 取主窗 W1。
	UseExtendedWindow bool
	// Source 是来源标记（仅 Publish 为 true 时有效）。
	Source BaselineSource
}

// DecideBaseline 是四种「空」的分流（设计稿 §3）——**纯函数**，无 IO。
//
// 独立成纯函数的原因：这是本块唯一「写错了也不报错、只是静静不写键或用错窗」的判定，
// 必须能在无库无 Redis 的条件下逐情形钉住（见 slowrate_baseline_test.go）。
//
// 情形对照：
//
//	A0 w1 >= 下限                     -> 发布，W1，source=primary
//	A1 w2 == 0（全期无行）             -> 不发布（防御性；计数查询本就不会返回这种组合）
//	A2 w1 < 下限、w2 >= 下限、w1 < 10  -> 发布，W2，source=extended
//	A3 w1 与 w2 都 < 下限且 w1 < 10    -> 不发布（长期静默，fail-open）
//	A4 w1 < 下限但 w1 >= 10、w2 达标   -> 发布，W2，source=extended_stale（只供会话级降级）
func DecideBaseline(w1Samples, w2Samples int64, minSamples int) BaselineDecision {
	if minSamples <= 0 {
		minSamples = slowRateBaselineDefaultMinSamples
	}
	floor := int64(minSamples)

	// A0：主窗达标——最常见路径。
	if w1Samples >= floor {
		return BaselineDecision{Publish: true, Source: BaselineSourcePrimary}
	}

	// A1：全期无行。计数查询的 WHERE 覆盖两窗，计数为 0 的组合根本不会出现；显式挡一道，
	// 避免将来调用方式变更时静默错算。
	if w2Samples == 0 {
		return BaselineDecision{}
	}

	// A3：两窗都不足，且 W1 连 10 条都没有——长期静默，不发布（fail-open）。
	if w2Samples < floor && w1Samples < slowRateBaselineStaleFloorSamples {
		return BaselineDecision{}
	}

	source := BaselineSourceExtended
	// A2 与 A4 的区分点是 **W1 是否有 >= 10 条样本**，而不是 W2 是否充足：
	//   - W1 >= 10 条：渠道**刚恢复**流量（刚恢复才有的少量样本），陈旧基线可能已不反映
	//     恢复后的形态 -> extended_stale，只供会话级降级；
	//   - W1 < 10 条：3 天内几乎静默（很可能只是没被选中），恢复时旧基线仍有效 -> extended。
	if w1Samples >= slowRateBaselineStaleFloorSamples {
		source = BaselineSourceExtendedStale
	}
	return BaselineDecision{Publish: true, UseExtendedWindow: true, Source: source}
}

// medianForWindow 拉取窗口内的清洗后样本，算出速率中位数。
//
// 速率口径的唯一真源是 pubstatus.ComputeTokensPerSecond（与公开状态页 / 排行榜同口径）；
// 本函数只做「取值 → 过滤 nil → 排序 → 取中位」。
//
// 返回 (nil, 0, false, nil) 表示该窗口内没有任何可算速率的样本。
func (b *SlowRateBaseline) medianForWindow(
	ctx context.Context,
	scope store.SlowRateScopeSamples,
	windowStart time.Time,
	windowEnd time.Time,
) (*float64, int64, bool, error) {
	rows, err := b.pools.SlowRateScopeRates(ctx,
		scope.ProviderID, scope.ModelKey,
		windowStart.UnixMilli(), windowEnd.UnixMilli(),
		slowRateBaselineSampleCeiling+1)
	if err != nil {
		return nil, 0, false, err
	}
	truncated := len(rows) > slowRateBaselineSampleCeiling
	if truncated {
		b.logger.Warn("slow_rate_baseline_samples_truncated", map[string]any{
			"providerId": scope.ProviderID,
			"modelKey":   scope.ModelKey,
			"ceiling":    slowRateBaselineSampleCeiling,
			"action":     "中位数仍按截断后的样本计算（取最近样本）",
		})
		rows = rows[:slowRateBaselineSampleCeiling]
	}

	rates := make([]float64, 0, len(rows))
	for _, row := range rows {
		rate := pubstatus.ComputeTokensPerSecond(row.OutputTokens, row.DurationMS, row.FirstByteMS)
		if rate == nil {
			continue
		}
		rates = append(rates, *rate)
	}
	if len(rates) == 0 {
		return nil, 0, truncated, nil
	}
	median := MedianRate(rates)
	return &median, int64(len(rates)), truncated, nil
}

// revokeBaseline 撤销某个 scope 的基线键（A3「成功判定为无基线」的唯一动作）。
//
// 为什么删而不是改写成一个「无基线」值：读侧（B4）的判定是「键不存在 ⇒ 不生成 penalty」
// （fail-open），删键即达成；写一个哨兵值要在读侧多一条分支，多一处可能分叉的口径。
//
// 删键失败返回错误、由调用方按单 scope 失败处理（记 scope_failed 后继续）——不静默吞。
func (b *SlowRateBaseline) revokeBaseline(
	ctx context.Context,
	scope store.SlowRateScopeSamples,
) error {
	key := BaselineKey(scope.ProviderID, scope.ModelKey)
	if err := b.redis.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("jobs: 撤销陈旧低速基线失败: %w", err)
	}
	b.logger.Info("slow_rate_baseline_revoked", map[string]any{
		"providerId": scope.ProviderID,
		"modelKey":   scope.ModelKey,
		"reason":     "no_baseline",
	})
	return nil
}

// writeBaseline 计算低速线并写键。
func (b *SlowRateBaseline) writeBaseline(
	ctx context.Context,
	scope store.SlowRateScopeSamples,
	median float64,
	samples int64,
	source BaselineSource,
	now time.Time,
	config store.SlowRateProviderConfig,
) error {
	payload := SlowRateBaselinePayload{
		Median:     median,
		Samples:    samples,
		ComputedAt: now.UnixMilli(),
		Source:     source,
		SlowLine:   SlowLine(median, effectiveRatioPerMille(config.RatioPerMille)),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("jobs: 编码低速基线失败: %w", err)
	}
	key := BaselineKey(scope.ProviderID, scope.ModelKey)
	if err := b.redis.Set(ctx, key, encoded, SlowRateBaselineKeyTTL).Err(); err != nil {
		return fmt.Errorf("jobs: 写入低速基线失败: %w", err)
	}
	b.logger.Info("slow_rate_baseline_published", map[string]any{
		"providerId": scope.ProviderID,
		"modelKey":   scope.ModelKey,
		"median":     median,
		"slowLine":   payload.SlowLine,
		"samples":    samples,
		"source":     string(source),
	})
	return nil
}

// sweepStaleKeys 清掉「不在本轮 scope 集合内」的基线键。
//
// 对应设计稿 §5 的边界情形「渠道被停用 / 已删模型 → 清掉该 scope 的键」。
//
// 为什么靠扫描而非精确清理：关闭监控、停用渠道、删除模型这三条路都发生在本任务之外
// （管理面写路径与配置同步），在那边埋钩子要把 jobs 反向注入管理面。改为一轮扫一次：
// 键的数量级是「已开启监控的渠道 × 其模型数」，很小，且每小时才扫一次。
//
// seen 为 nil 表示「无任何已开启渠道」——此时除键一律清掉。注意这与「读不到配置」不同：
// 后者应保持键（宁可留着也不要在配置抖动时把基线全清空）。
func (b *SlowRateBaseline) sweepStaleKeys(ctx context.Context, seen map[string]struct{}) (int, error) {
	var cursor uint64
	swept := 0
	for {
		keys, next, err := b.redis.Scan(ctx, cursor, baselineKeyPrefix+"*"+baselineKeySuffix, 256).Result()
		if err != nil {
			return swept, fmt.Errorf("jobs: 扫描低速基线键失败: %w", err)
		}
		if len(keys) > 0 {
			stale := make([]string, 0, len(keys))
			for _, key := range keys {
				if _, keep := seen[key]; !keep {
					stale = append(stale, key)
				}
			}
			if len(stale) > 0 {
				if err := b.redis.Del(ctx, stale...).Err(); err != nil {
					return swept, fmt.Errorf("jobs: 清扫低速基线键失败: %w", err)
				}
				swept += len(stale)
			}
		}
		cursor = next
		if cursor == 0 {
			return swept, nil
		}
	}
}

// MedianRate 返回一组速率的**中位数**；**输入为空时返回 NaN**（调用方须先判空，
// 见 medianForWindow 的处理：空即视为该窗无可用样本）。
//
// 为什么用中位数而不是均值：劣化段约 26% 的慢请求会把均值拖低，而中位数纹丝不动
// （设计稿 §3 的实证：p50 204.3 vs 健康段 240.3，均值则被显著拉低）。判定阈值必须锚在
// 「常态速率」上，否则慢样本一多，阈值自己就跟着降下去，越慢越抓不到。
//
// 偶数个样本取中间两个的算术平均（与 SQL 的 percentile_cont(0.5) 同义；
// 仓内无既有的 percentile_cont 用法可供复用，故按此定义实现）。
//
// 输入不被修改（内部复制后排序）——调用方的切片不因本函数而改变顺序。
func MedianRate(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// SlowLine 返回低速线 = 基线 × 系数（系数以千分比整数给出，200 = 0.2）。
//
// 单独成函数是为了让「系数是千分比」这件事只有一处定义：调用方与测试都不必各自 /1000。
func SlowLine(median float64, ratioPerMille int) float64 {
	if ratioPerMille <= 0 {
		ratioPerMille = slowRateBaselineDefaultRatioPerMille
	}
	return median * float64(ratioPerMille) / 1000
}

// groupProviderConfigsByWindow 按「有效基线窗口长度」把渠道分组。
//
// 窗口长度是逐渠道可覆写的，而窗口边界进的是同一条计数查询，故必须先分组再查：
// 同一组共用一个 w1Start。默认全用同一值时只有一组。
//
// **只读 BaselineWindowSeconds**：判定滑窗（WindowSeconds）与本窗尺度不同（分钟 vs 天），
// 历史上共用 slow_rate_window_seconds 一列导致「调判定窗打坏基线」（见 store.SlowRateProviderConfig）。
func groupProviderConfigsByWindow(configs []store.SlowRateProviderConfig) map[int][]store.SlowRateProviderConfig {
	out := make(map[int][]store.SlowRateProviderConfig)
	for _, config := range configs {
		seconds := int(slowRateBaselineW1Span / time.Second)
		if config.BaselineWindowSeconds != nil && *config.BaselineWindowSeconds > 0 {
			seconds = *config.BaselineWindowSeconds
		}
		out[seconds] = append(out[seconds], config)
	}
	return out
}

// providerIDsOf 取出分组内的渠道 ID（计数与最老样本查询的入参）。
func providerIDsOf(configs []store.SlowRateProviderConfig) []int64 {
	out := make([]int64, 0, len(configs))
	for _, config := range configs {
		out = append(out, config.ProviderID)
	}
	return out
}

// effectiveW1Span 是主窗（基线）的有效长度（0 或负值回落默认 3 天）。
//
// 入参是**基线窗**取值（slow_rate_baseline_window_seconds），不是判定滑窗。
func effectiveW1Span(windowSeconds int) time.Duration {
	if windowSeconds <= 0 {
		return slowRateBaselineW1Span
	}
	return time.Duration(windowSeconds) * time.Second
}

// effectiveMinSamples 是有效样本下限（nil 或非正值回落默认 100）。
func effectiveMinSamples(value *int) int {
	if value == nil || *value <= 0 {
		return slowRateBaselineDefaultMinSamples
	}
	return *value
}

// effectiveRatioPerMille 是有效系数（nil 或非正值回落默认 300，即 0.3）。
func effectiveRatioPerMille(value *int) int {
	if value == nil || *value <= 0 {
		return slowRateBaselineDefaultRatioPerMille
	}
	return *value
}
