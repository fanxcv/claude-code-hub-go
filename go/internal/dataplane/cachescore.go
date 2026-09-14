package dataplane

import (
	"context"
	"strconv"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// cacheScoreKey 是门控缓存的键：system_settings 是单行表，键恒为 0（与本包计费侧的
// billingSourceKey 同构）。
const cacheScoreKey = 0

// cacheScoreGate 是 F3b 缓存模拟列的开关与事实装配。
//
// 为什么需要它：这五列**只在开关开启时**才写（Node 的 isCacheEffectivenessEnabled 门控，
// 见 src/app/v1/_lib/proxy/response-handler.ts:5430），而开关是「设置行优先、env 兜底」
// 的两级来源。不接门控就写＝在开关关闭的部署上凭空造数据，那是账务列，不能这么干。
type cacheScoreGate struct {
	settings guard.SettingsSource
	// envDefault 是 env 出厂默认（Node 的 envCacheEffectivenessDefault：ENABLE_CACHE_EFFECTIVENESS，
	// 默认 true；且 Node 在读取失败时也回落到 true）。
	envDefault bool
	cached     *cfgsync.TTLMap[int, bool]
}

// newCacheScoreGate 建门控。settings 为 nil 时只用 env 默认值（等价于「设置行读不到」）。
func newCacheScoreGate(settings guard.SettingsSource, envDefault bool) *cacheScoreGate {
	return &cacheScoreGate{
		settings:   settings,
		envDefault: envDefault,
		cached:     cfgsync.NewTTLMap[int, bool](cfgsync.Spec(cfgsync.DomainSystemSettings).TTL, 1),
	}
}

// enabled 复刻 Node 的 isCacheEffectivenessEnabled：
// `settings.cacheEffectivenessEnabled ?? envCacheEffectivenessDefault()`。
//
// nil 与 false 必须区分：前者回落 env（默认 true），后者是显式关闭。读失败也写缓存，
// 否则一次数据库抖动会把热路径变成「每请求一次设置查询」（与计费侧 billingSource 同策）。
func (g *cacheScoreGate) enabled(ctx context.Context) bool {
	if g == nil {
		// 未接线：不写 F3b 列。返回 false 而不是 env 默认值，避免在测试/降级路径上
		// 依赖一个没人接的事实。
		return false
	}
	if value, ok := g.cached.Get(cacheScoreKey); ok {
		return value
	}
	value := g.envDefault
	if g.settings != nil {
		settings, err := g.settings.FindSystemSettings(ctx)
		if err == nil && settings != nil && settings.CacheEffectivenessEnabled != nil {
			value = *settings.CacheEffectivenessEnabled
		}
	}
	g.cached.Set(cacheScoreKey, value)
	return value
}

// cacheEffectivenessEnvDefault 复刻 Node 的 envCacheEffectivenessDefault
// （src/lib/system-settings/proxy-runtime.ts:40-46）：读 env 契约的
// ENABLE_CACHE_EFFECTIVENESS（出厂默认 true），**读不到就照 true 算**——Node 的 catch
// 分支同样是 true，两侧在坏环境下同判。
func cacheEffectivenessEnvDefault() bool {
	env, err := config.LoadEnv(config.LookupFromOS())
	if err != nil {
		return true
	}
	return env.EnableCacheEffectiveness
}

// cacheScoreFacts 是选路包固化下来的亲和事实（route.AffinityWriteback 实现）。
//
// 用本地接口做类型断言而不是改 pctx 的接口：终态层只拿到 pctx 的中性句柄
// （pctx.AffinityWriteback），本包按需取自己认得的那部分事实，既不引入 route 类型，
// 也不把 route 的字段形状泄露到终端层。
type cacheScoreFacts interface {
	CacheScoreFacts() (scopeTag, matchedFP, tipFP string, tipPrefixBytes int, hasTip bool)
}

// cacheScoreTerms 是一次 F3b 派生所需的终态事实。
type cacheScoreTerms struct {
	succeeded       bool
	usageObservable bool
	streamTruncated bool
	cacheTTL        string
}

// fields 派生五个列值。第二个返回值为 false 表示本次不写（开关关闭或未接线）。
func (g *cacheScoreGate) fields(
	ctx context.Context,
	pc *pctx.Context,
	terms cacheScoreTerms,
) (terminal.CacheScoreFields, bool) {
	if !g.enabled(ctx) {
		return terminal.CacheScoreFields{}, false
	}
	input := terminal.CacheScoreInput{
		Succeeded:       terms.succeeded,
		UsageObservable: terms.usageObservable,
		StreamTruncated: terms.streamTruncated,
		CacheTTL:        terms.cacheTTL,
	}
	if pc != nil {
		if writeback, ok := pc.AffinityWriteback(); ok {
			if facts, ok := writeback.(cacheScoreFacts); ok {
				input.ScopeTag, input.MatchedFingerprint, input.TipFingerprint,
					input.TipPrefixBytes, input.HasTip = facts.CacheScoreFacts()
			}
		}
	}
	return terminal.ComputeCacheScoreFields(input), true
}

// streamSucceeded 是 Node `finalized.isSuccessfulCompletion` 在 Go 事实下的等价判定。
//
// Node 的判据（response-handler.ts:2359）是：
//
//	!isIncompleteCompletion && !detected.isError && 2xx && (streamEndedNormally || clientAbortCompleteSuccess)
//
// Go 侧与它同源的最接近事实是 forward 层自己的成功判定
// （stream.go 的 `Success: outcome.Kind == TerminalCompleted`），即「2xx 且上游发出协议终止标记」。
//
// **上限（有意，登记在此）**：Node 的 `clientAbortCompleteSuccess`（客户端在流**已自然收束**
// 之后才断开）在 Go 的终态词表里落 TerminalClientAborted，与「流未收束就被断开」同形，
// 无法区分，故这类请求 Go 侧算 attempt_failed 而 Node 算成功。
// 差异只影响 F3b 的排除原因取值（eligible 在两种情形下都是 false 之外的分支不受影响），
// 详见 的残余风险。
func streamSucceeded(outcome forward.StreamOutcome) bool {
	if outcome.StatusCode < 200 || outcome.StatusCode >= 300 {
		return false
	}
	return outcome.Kind == forward.TerminalCompleted
}

// applyCostMultipliers 把本次请求实际使用的两个成本倍率落成 numeric 文本。
//
// 为什么在终态写而不是建行时就写：Node 在建行时写（src/app/v1/_lib/proxy/message-service.ts:125-126
// 的 `cost_multiplier: provider.costMultiplier` / `group_cost_multiplier: session.getGroupCostMultiplier()`），
// 而 Go 建行时（守卫链的 messageContext 步）拿不到倍率——pctx.ProviderSelection 只带
// ProviderID/Name/Type/Endpoint。两个候选做法都需要改选路包的数据形状；本实现选择在终态写：
// 两列都在账本触发器的监视列表里（terminal.ledgerMonitoredColumns），终态写同样会驱动
// 账本行更新，且此处的值就是计费用的那份（不会出现「列里一个值、账里另一个值」）。
//
// 残余差异：在建行到终态之间该两列为 NULL；从未走到终态的行（进程被杀）会永远为 NULL。
// Node 侧同样可能没有终态，但那两列它在建行时就已写上。详见报告。
func applyCostMultipliers(settlement *terminal.Settlement, state *RequestState) {
	if settlement == nil || state == nil {
		return
	}
	if state.ProviderMultiplier != nil {
		value := formatMultiplier(*state.ProviderMultiplier)
		settlement.CostMultiplier = &value
	}
	if state.GroupMultiplier != nil {
		value := formatMultiplier(*state.GroupMultiplier)
		settlement.GroupCostMultiplier = &value
	}
}

// formatMultiplier 把倍率写成 numeric 列的文本形。
//
// 用 'f' + -1 精度复刻 JS 的 Number#toString 对常见倍率（1、0.3、1.5）的书写：
// 不使用科学计数法，且不带多余尾零——列是 numeric(10,4)，超出部分由 PG 自行取整。
func formatMultiplier(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// applyCacheScoreFields 把 F3b 五列写进终态。
//
// 只在**流式**终态调用：Node 的 computeCacheScoreFields 只有一处调用点
// （response-handler.ts:5431，流式结算），非流式行在 Node 侧恒为 NULL——这里保持一致，
// 否则同一列在两种路径上有两种含义。
func (s *storeSettler) applyCacheScoreFields(
	ctx context.Context,
	settlement *terminal.Settlement,
	pc *pctx.Context,
	outcome forward.StreamOutcome,
) {
	fields, ok := s.cacheScore.fields(ctx, pc, cacheScoreTerms{
		succeeded:       streamSucceeded(outcome),
		usageObservable: settlement.Usage.InputTokens != nil,
		streamTruncated: outcome.Kind != forward.TerminalCompleted,
		cacheTTL:        settlement.Usage.CacheTTL,
	})
	if !ok {
		return
	}
	settlement.CacheCompatibilityKey = fields.CacheCompatibilityKey
	settlement.CacheScoreExcludedReason = fields.CacheScoreExcludedReason
	settlement.TheoreticalCacheTokens = fields.TheoreticalCacheTokens
	settlement.CacheTTLBucket = fields.CacheTTLBucket
	eligible := fields.CacheScoreEligible
	settlement.CacheScoreEligible = &eligible
}
