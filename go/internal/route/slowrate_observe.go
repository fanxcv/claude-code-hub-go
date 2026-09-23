package route

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是「隔离运行态」的管理面只读面：把某渠道全部 (模型) 组合的隔离状态摊平成一行一组合。
//
// 为什么要有它：隔离态的真源散在四族键里（state / samples / streak / 探针租约），而管理面
// 原本只有 per-渠道的**聚合降权**读数（providers_health 的 slowRate）——它回答「被压了多少」，
// 回答不了「隔离到底触发过没有、现在放行多少」。没有这个读数，运维只能翻 Redis 猜。
//
// 判定**不复刻**：准入门槛走 quarantinePermilleForStreak，生效参数走 slowRateEffectiveParams，
// 活窗计数与 Assess 用同一个下界——都是选路侧正在用的函数，本文件不另写一份。
// 之所以不复用 Assess 本身：它只回「**当前**被隔离的组合」，而排障最需要的恰恰是
// 「有可用基线、有干净计数、但从未慢到触发隔离」这一态（生产 2026-09-22 实测的组合就是它，
// 它连状态键都没有，故 Assess 必然看不见）。

// slowRateObserveScanCount 是枚举组合键时每次 SCAN 的提示条数（与管理面其余 SCAN 同档）。
const slowRateObserveScanCount = 256

// SlowRateStateObservation 是某 (渠道, 模型) 组合的隔离运行态快照。
//
// 字段刻意分两层：**原始事实**（StateExists / CleanStreak / SampleLiveCount / BaselineUsable /
// 租约）与**派生读数**（Penalty / Quarantined / AdmissionPermille，后者与选路逐条同判）。
// 只回派生值会让「为什么它没被挡」无从归因（是没慢过？是基线没了？还是窗里已经没样本了）。
type SlowRateStateObservation struct {
	// ModelKey 是归一后的模型分量（键里的原文，未再归一）。
	ModelKey string
	// StateExists 为真表示该组合**真的被计过惩罚**：状态键只由写侧的慢路径创建
	// （窗内慢样本达触发阈值才建键，见 slowrate.advanceSlowState）。
	StateExists bool
	// Quarantined 是选路侧当前是否会把它当隔离（与 Assess 的 Quarantine 同一判据：
	// 状态标过隔离 **且** 活窗派生的惩罚为正 **且** 基线可用）。
	//
	// 它与「状态里那个 quarantine 字段」不是一回事：状态键一旦创建就带着该字段（写侧同一次
	// HSet 写下），故「标过」恒真于「存在」。这里要的是**当前是否真在挡流量**——活窗空了就
	// 不再挡，而状态键还能活到 2 倍窗长。
	Quarantined bool
	// Penalty 是当前生效降权量：参数齐备时由活窗计数当场派生（与选路同一个函数），
	// 否则回退状态里的快照值；基线不可用时一律 0（选路侧同样不施惩罚）。
	Penalty int
	// CleanStreak 是连续干净样本数——准入阶梯的唯一输入。任何一条慢事实都会删掉计数键，
	// 故它归零是即时的。
	CleanStreak int
	// AdmissionPermille 是当前**有效放行比例**（千分比）：1000 表示不挡流量
	// （未隔离，或隔离已无活窗慢样本支撑）。隔离中取 quarantinePermilleForStreak 的阶梯值。
	AdmissionPermille int
	// SampleLiveCount 是活窗内的慢样本数（ZCount，下界与选路同一算法：当前时刻减去生效窗长）。
	//
	// 不用 ZCARD：滑窗只在**下一次慢写**时才裁掉窗外成员，而键的 TTL 是 2 倍窗长，
	// 故 ZCARD 会把窗外成员算进来，读数会比选路看到的惩罚偏大——排障时正是这个偏差最误导人。
	SampleLiveCount int
	// BaselineUsable 表示该组合的基线**可用**（键在且 source 属 primary/extended）。
	//
	// 它是「这条组合到底进没进监控」的判据：写侧无可用基线即整段返回（不采样、不计数），
	// 故 BaselineUsable 为假时下面所有计数必然为 0，读数的含义是「没在看」而非「看过没慢」。
	BaselineUsable bool
	// ProbeLeaseHeld 表示探针租约键存在（有探针在飞，或刚飞完还没到 TTL）。
	ProbeLeaseHeld bool
	// ProbeLeaseTTL 是租约的剩余寿命；为负表示无租约键（Redis PTTL 对不存在的键回负数）。
	ProbeLeaseTTL time.Duration
}

// ObserveStates 扫出该渠道的全部 (模型) 组合并读回其隔离运行态。
//
// 组合的枚举口径是**四族键的并集**（state / samples / streak / baseline）：只看状态键会漏掉
// 「监控在跑但从未慢过」的组合（生产实测就是它：状态键不存在、滑窗为空、streak=2），
// 而那恰恰是「机制到底动不动」最需要看见的一态。基线键也只由已开启监控的渠道产出
// （jobs.SlowRateBaseline 的入参是 SlowRateEnabledProviders），故它的存在即「这条组合在监控范围内」。
//
// provider 只用到 ID 与四个降权参数（后者决定活窗下界与档位）：调用方应传**渠道行**，
// 这样管理面与数据面看到同一个窗长（只传 ID 会让读侧回退状态里的旧参数）。
//
// 读失败**返回错误**而不是回空表：空表在管理面的含义是「没有组合」，而读不到是另一回事——
// 把两者混同，正是这次要修的「无法自证」本身。调用方（管理面）据此在响应里带
// unavailableReason，而不是给 5xx。
func (r *SlowRateReader) ObserveStates(
	ctx context.Context,
	provider Provider,
) ([]SlowRateStateObservation, error) {
	if r == nil || r.redis == nil || provider.ID <= 0 {
		return nil, nil
	}
	modelKeys, err := r.scanSlowRateModelKeys(ctx, provider.ID)
	if err != nil {
		return nil, err
	}
	if len(modelKeys) == 0 {
		return nil, nil
	}

	// 第一段：每组合读四件事（状态 Hash / 干净计数 / 基线 / 租约），一次往返。
	states := make([]*redis.SliceCmd, len(modelKeys))
	streaks := make([]*redis.StringCmd, len(modelKeys))
	baselines := make([]*redis.StringCmd, len(modelKeys))
	leases := make([]*redis.DurationCmd, len(modelKeys))
	_, err = r.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, modelKey := range modelKeys {
			states[index] = pipe.HMGet(ctx, SlowRateStateKey(provider.ID, modelKey),
				SlowRateStateFieldPenalty,
				SlowRateStateFieldWindowMinutes,
				SlowRateStateFieldTriggerCount,
				SlowRateStateFieldPenaltyStep,
				SlowRateStateFieldPenaltyMax,
				SlowRateStateFieldQuarantine,
			)
			streaks[index] = pipe.Get(ctx, SlowRateCleanStreakKey(provider.ID, modelKey))
			baselines[index] = pipe.Get(ctx, SlowRateBaselineKey(provider.ID, modelKey))
			// PTTL 而不是 Exists：一次往返同时拿到「在不在」与「还能在多久」。
			leases[index] = pipe.PTTL(ctx, SlowProbeLeaseKey(provider.ID, modelKey))
		}
		return nil
	})
	// 整批失败即整批返回错误（批内 redis.Nil 是「键不存在」的正常情形，不在此列）：
	// 逐条吞掉会让「Redis 挂了」表现为「所有组合都没状态」，正是本端点要消灭的误读。
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("route: 读低速隔离态失败: %w", err)
	}

	// 第二段：活窗计数。下界依赖窗长，而窗长要等第一段读回状态才知道（与 Assess 同一分段的
	// 理由），故不能并进第一段。
	//
	// **对每个组合都数**，而不是只对「状态键存在」的组合：滑窗成员先于状态键出现——
	// 慢样本不足触发阈值时写侧只 ZADD 不建状态键（见 slowrate.advanceSlowState 的门槛），
	// 而「滑窗里有慢样本但从未触发隔离」正是排障要看见的一态。少发这几次 ZCount 省不下什么，
	// 却会把那一态读成 0。
	resolved := make([]slowRatePenaltyParams, len(modelKeys))
	derivable := make([]bool, len(modelKeys))
	liveCounts := make([]int, len(modelKeys))
	for index := range modelKeys {
		state := decodeSlowRateState(states[index])
		params, ok := slowRateEffectiveParams(provider, state)
		if !ok {
			// 渠道行四列全 NULL 且状态里也没有参数（状态键还不存在，或是旧版写下的状态）：
			// 窗长只能取出厂值——写侧裁窗用的也是同一份归一（同一配置源），故这是可用的下界。
			// 注意此时 derivable 仍为假：选路对这类组合也不派生惩罚（它回退状态快照）。
			params = slowRatePenaltyParamsFromProvider(provider).normalize()
		}
		resolved[index] = params
		derivable[index] = ok && state.exists
	}
	nowMS := r.now().UnixMilli()
	counts := make([]*redis.IntCmd, len(modelKeys))
	_, err = r.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, modelKey := range modelKeys {
			lower := nowMS - int64(resolved[index].windowMinutes)*60*1000
			counts[index] = pipe.ZCount(
				ctx,
				SlowRateSamplesKey(provider.ID, modelKey),
				strconv.FormatInt(lower, 10),
				"+inf",
			)
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("route: 读低速活窗失败: %w", err)
	}
	for index := range modelKeys {
		// 读失败与「窗内无慢样本」同判（计 0），与选路的 fail-open 一致。
		if value, err := counts[index].Result(); err == nil {
			liveCounts[index] = int(value)
		}
	}

	out := make([]SlowRateStateObservation, 0, len(modelKeys))
	for index, modelKey := range modelKeys {
		state := decodeSlowRateState(states[index])
		observation := SlowRateStateObservation{
			ModelKey:    modelKey,
			StateExists: state.exists,
			CleanStreak: slowRateIntResult(streaks[index]),
			// 未隔离的放行比例是 1000（全放），故默认值即「不挡」。
			AdmissionPermille: quarantineAdmissionFullPermille,
			BaselineUsable:    slowRateBaselineUsable(baselines[index]),
			SampleLiveCount:   liveCounts[index],
		}
		// PTTL 对不存在的键回负数（go-redis 原样存成纳秒量级的 Duration），故「是否为负」
		// 就是「租约键在不在」——直接用读到的值，负值原样透出（不折叠成 0，否则「没有租约」
		// 与「租约刚到期」在读数上不可分）。
		if ttl, err := leases[index].Result(); err == nil {
			observation.ProbeLeaseTTL = ttl
			observation.ProbeLeaseHeld = ttl >= 0
		}
		if state.exists && observation.BaselineUsable {
			// 参数齐备则当场派生（惩罚不能只涨不落）；否则回退状态里的快照，与 Assess 同判。
			observation.Penalty = state.penalty
			if derivable[index] {
				observation.Penalty = deriveSlowRatePenalty(liveCounts[index], resolved[index])
			}
			// 隔离只对「活窗派生 + 标过隔离 + 惩罚为正」的组合生效：回退快照惩罚的组合
			// （旧数据、参数缺失）不入隔离——与 Assess 逐条同判。
			if derivable[index] && state.quarantined && observation.Penalty > 0 {
				observation.Quarantined = true
				observation.AdmissionPermille = quarantinePermilleForStreak(observation.CleanStreak)
			}
		}
		out = append(out, observation)
	}
	return out, nil
}

// scanSlowRateModelKeys 枚举某渠道在低速键族里出现过的全部模型键。
//
// 模式是 `cch:slow:{<pid>:*}`（含 hash tag 左半）：`{` 在 Redis 的 glob 里是普通字符，
// 而前缀里带着 `:` 结尾的渠道 id，故 pid=16 不会误配 pid=167 的键。
//
// 成本边界（如实登记）：SCAN 的**匹配**开销随全库键数增长（本仓生产实测全库约 2.4 万键），
// 本端点由排障人工触发，量级与 /providers/health 那条既有的全库 SCAN 同档。
func (r *SlowRateReader) scanSlowRateModelKeys(ctx context.Context, providerID int64) ([]string, error) {
	scopePrefix := slowRateScopePrefix(providerID)
	seen := make(map[string]struct{})
	cursor := uint64(0)
	for {
		keys, next, err := r.redis.Scan(ctx, cursor, scopePrefix+"*", slowRateObserveScanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("route: 扫描低速组合键失败: %w", err)
		}
		for _, key := range keys {
			if modelKey, ok := slowRateModelKeyOfKey(key, scopePrefix); ok {
				seen[modelKey] = struct{}{}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	out := make([]string, 0, len(seen))
	for modelKey := range seen {
		out = append(out, modelKey)
	}
	// 排序只为读数稳定（同一份 Redis 状态两次请求给同一顺序），不承载任何语义。
	sort.Strings(out)
	return out, nil
}

// slowRateScopePrefix 是某渠道全部组合键的公共前缀（形制见 SlowRateStateKey）。
func slowRateScopePrefix(providerID int64) string {
	return slowRateKeyPrefix + "{" + strconv.FormatInt(providerID, 10) + ":"
}

// slowRateModelKeyOfKey 从组合键反解模型键。
//
// 只认四种已知后缀（state / samples / streak / baseline），其余键（含 `cch:slowprobe:` 那族，
// 它不带 `cch:slow:` 前缀）一律不算——枚举口径宁窄勿宽：把未知键当成组合会让管理面出现
// 一条永远读不到状态的幽灵行。
//
// 模型键里可以含冒号（如 `global:deepseek-v4.1-flash`），故**只**剥前后缀、不按冒号切分。
func slowRateModelKeyOfKey(key, scopePrefix string) (string, bool) {
	rest, found := strings.CutPrefix(key, scopePrefix)
	if !found {
		return "", false
	}
	for _, suffix := range []string{
		slowRateStateSuffix,
		slowRateSamplesSuffix,
		slowRateCleanStreakSuffix,
		slowRateBaselineSuffix,
	} {
		if modelKey, ok := strings.CutSuffix(rest, "}"+suffix); ok && modelKey != "" {
			return modelKey, true
		}
	}
	return "", false
}

// slowRateIntResult 解出 STRING 计数键的整数值；键缺失或非数字一律 0。
//
// 与「键不存在即 0 个干净样本」同判（fail-open）：调用方不区分这两者，故不返回错误。
func slowRateIntResult(cmd *redis.StringCmd) int {
	value, err := cmd.Result()
	if err != nil {
		return 0
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return number
}
