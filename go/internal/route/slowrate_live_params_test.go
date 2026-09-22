package route

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
)

// 本文件钉住「低速四参数取自**渠道行实时值**」——用户 2026-09-22 裁决「窗口改完要真热生效」。
//
// 改前的读侧只认状态 Hash 里的参数，而那份参数只在**下一次慢样本写入**时刷新：渠道没有新的慢
// 样本时，改完窗口仍按旧窗口算，直到状态键 TTL（2 倍窗长）到期。下面第一条用例钉的正是这一点：
// 同一份状态、同一批样本，只改渠道行 ⇒ 降权当场变。

// slowRatePtr 造可空整数列的值（NULL 用 nil 表示）。
func slowRatePtr(value int) *int { return &value }

// slowRateProviderWithParams 造一个带实时四参数的渠道行（nil 表示该列 NULL）。
func slowRateProviderWithParams(
	id int64,
	windowMinutes, triggerCount, penaltyStep, penaltyMax *int,
) Provider {
	provider := slowRateProvider(id)
	provider.SlowRateWindowMinutes = windowMinutes
	provider.SlowRateTriggerCount = triggerCount
	provider.SlowRatePenaltyStep = penaltyStep
	provider.SlowRatePenaltyMax = penaltyMax
	return provider
}

// slowRateStateWithBaseline 造「状态键 + 可用基线 + 滑窗成员」三件套。
func slowRateStateWithBaseline(
	t *testing.T,
	penalty, windowMinutes, triggerCount, step, max int,
	members []redis.Z,
) *slowRateRedis {
	t.Helper()
	return &slowRateRedis{
		values: map[string]string{
			SlowRateStateKey(9, "m1"):    slowRateStateValueWithParams(t, penalty, windowMinutes, triggerCount, step, max),
			SlowRateBaselineKey(9, "m1"): slowRateBaselineValue(t, "primary"),
		},
		zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): members},
	}
}

// TestSlowRatePenaltyFollowsLiveRowParams 是本次改动的**主钉子**。
//
// 状态里刻意留下与渠道行**不同**的参数（窗长 30/阈值 3/步长 10/上限 30），于是「用了哪一侧的
// 参数」在结果上可判别；两个子例都只差渠道行，故同时证明「改完立即生效」——无需等下一次慢样本
// 把参数刷进状态。
//
// 反证：把 slowRateEffectiveParams 改回「一律取状态参数」，两个子例都变红。
func TestSlowRatePenaltyFollowsLiveRowParams(t *testing.T) {
	t.Run("档位取自渠道行", func(t *testing.T) {
		// 行上阈值 1、步长 7、上限 21；窗内 6 条 ⇒ floor(6/1)*7 = 42，封顶 21。
		// 状态里的参数会给出 floor(6/3)*10 = 20，故 21 与 20 可判别。
		reader := newSlowRateReaderAt(slowRateStateWithBaseline(t, 999, 30, 3, 10, 30, slowRateSamples(6, 60_000)))
		provider := slowRateProviderWithParams(9, slowRatePtr(30), slowRatePtr(1), slowRatePtr(7), slowRatePtr(21))

		got := reader.Penalties(context.Background(), []Provider{provider}, "m1")
		if got[9] != 21 {
			t.Errorf("降权 = %d，期望 21（渠道行 1/7/21）；若得 20 说明取了状态里的旧参数", got[9])
		}
	})

	t.Run("窗长取自渠道行", func(t *testing.T) {
		// 6 条样本：3 条在 1 分钟内（落在 5 分钟窗内），3 条在 10 分钟前（只落在 30 分钟窗内）。
		// 行上窗长 5 分钟 ⇒ 只数 3 条 ⇒ floor(3/3)*10 = 10；状态里的 30 分钟会数出 6 条 ⇒ 20。
		members := append(slowRateSamples(3, 60_000), slowRateSamples(3, 600_000)...)
		reader := newSlowRateReaderAt(slowRateStateWithBaseline(t, 999, 30, 3, 10, 30, members))
		provider := slowRateProviderWithParams(9, slowRatePtr(5), slowRatePtr(3), slowRatePtr(10), slowRatePtr(30))

		got := reader.Penalties(context.Background(), []Provider{provider}, "m1")
		if got[9] != 10 {
			t.Errorf("降权 = %d，期望 10（5 分钟窗只数到 3 条）；若得 20 说明用了状态里的 30 分钟窗", got[9])
		}
	})
}

// TestSlowRatePenaltyNormalizesRowParams 钉住渠道行的 NULL / 0 / 负数都收敛为出厂默认
// （30/3/10/30），与写侧 slowrate.Params.normalize 同判——渠道列可空，NULL 即取出厂值。
//
// 「只设窗长」那一子例是判据的判别点：它要求其余三项也走归一化。若归一化只认「整行全空」，
// 阈值会以 0 参与 `floor(计数/阈值)`。
func TestSlowRatePenaltyNormalizesRowParams(t *testing.T) {
	zero, negative := 0, -7
	for _, tc := range []struct {
		name     string
		provider Provider
	}{
		{"全为 NULL", slowRateProviderWithParams(9, nil, nil, nil, nil)},
		{"全为 0", slowRateProviderWithParams(9, &zero, &zero, &zero, &zero)},
		{"全为负", slowRateProviderWithParams(9, &negative, &negative, &negative, &negative)},
		{"只设窗长", slowRateProviderWithParams(9, slowRatePtr(30), nil, nil, nil)},
		{"显式出厂值", slowRateProviderWithParams(9, slowRatePtr(30), slowRatePtr(3), slowRatePtr(10), slowRatePtr(30))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := newSlowRateReaderAt(slowRateStateWithBaseline(t, 999, 30, 3, 10, 30, slowRateSamples(6, 60_000)))

			got := reader.Penalties(context.Background(), []Provider{tc.provider}, "m1")
			if got[9] != 20 {
				t.Errorf("降权 = %d，期望 20（出厂默认 30/3/10/30 数 6 条）", got[9])
			}
		})
	}
}

// TestSlowRatePenaltyUsesStateParamsWhenRowCarriesNone 钉住回退：候选不带参数列时
// （管理面的健康投影历史上构造的合成 Provider 只有 id 与开关），改取状态里记录的生效值。
//
// 为何保留这条回退：状态里的参数正是写侧 normalize 的产物，语义与「normalize 行上的 NULL」
// 一致；没有它，这类候选会一律按出厂默认算，而渠道上实际配的档位被忽略。
func TestSlowRatePenaltyUsesStateParamsWhenRowCarriesNone(t *testing.T) {
	// 状态里阈值 1、步长 7、上限 21；窗内 6 条 ⇒ 21（与出厂默认的 20 可判别）。
	reader := newSlowRateReaderAt(slowRateStateWithBaseline(t, 999, 30, 1, 7, 21, slowRateSamples(6, 60_000)))

	got := reader.Penalties(context.Background(), []Provider{slowRateProvider(9)}, "m1")
	if got[9] != 21 {
		t.Errorf("降权 = %d，期望 21（状态里的 1/7/21）；若得 20 说明按出厂默认算了", got[9])
	}
}

// TestSlowRateSkipsCountWithoutStateKey 钉住闸门：候选没有状态键（从未慢过）时，一个 ZCount 都不发。
//
// 状态键只由写侧的慢路径创建（见 slowrate.Record 的 statePipe），故「键不存在」即「这家从未慢过」。
// 这是「默认全关时零往返」之外的第二道零开销：把区间计数发给全部已开启监控的候选，会让每请求的
// 往返数随候选数放大（正是 TestSlowRateRoundTripsDoNotScaleWithCandidates 要防的形态）。
//
// 反证：把闸门从「状态键存在」改成「无条件发计数」，本用例的 zcountRanges 非空 ⇒ 红。
func TestSlowRateSkipsCountWithoutStateKey(t *testing.T) {
	client := &slowRateRedis{
		values: map[string]string{
			// 只有基线键，没有状态键。
			SlowRateBaselineKey(9, "m1"): slowRateBaselineValue(t, "primary"),
		},
		zsets: map[string][]redis.Z{SlowRateSamplesKey(9, "m1"): slowRateSamples(6, 60_000)},
	}
	reader := newSlowRateReaderAt(client)
	provider := slowRateProviderWithParams(9, slowRatePtr(30), slowRatePtr(3), slowRatePtr(10), slowRatePtr(30))

	got := reader.Penalties(context.Background(), []Provider{provider}, "m1")
	if len(got) != 0 {
		t.Fatalf("无状态键却报降权 %v，期望无降权", got)
	}
	if len(client.zcountRanges) != 0 {
		t.Errorf("无状态键却发了区间计数 %v，期望零命令", client.zcountRanges)
	}
}
