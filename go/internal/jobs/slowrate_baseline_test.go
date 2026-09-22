package jobs

import (
	"math"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是无库无 Redis 单测：钉住 B3 里「写错了也不报错、只是静静不写键或用错窗」的部分
// ——四种「空」的分流（设计稿 §3 的 A1-A4）、中位数算法、清扫用的键形制、系数换算。
//
// SQL 语义（计数与取行）由真库集成用例覆盖；本文件不碰 store / redis。

// TestDecideBaselineFourEmptyCases 钉住设计稿 §3「四种空」的分流。
//
// 这四种情形的处理**完全不同**，混为一谈会导致两种事故：把冷渠道永久判死，
// 或让刚重启的渠道用陈旧基线误杀。
func TestDecideBaselineFourEmptyCases(t *testing.T) {
	const min = 100
	cases := []struct {
		name        string
		w1          int64
		w2          int64
		wantPublish bool
		wantExt     bool
		wantSource  BaselineSource
	}{
		// A0：主窗达标，最常见。
		{"A0 主窗达标 -> primary", 100, 500, true, false, BaselineSourcePrimary},
		{"A0 主窗远超下限 -> primary", 5000, 0, true, false, BaselineSourcePrimary},
		{"A0 主窗恰好等于下限 -> primary", 100, 0, true, false, BaselineSourcePrimary},

		// A1：全期无行（防御性，计数查询本不会返回这种组合）。
		{"A1 两窗皆零 -> 不发布", 0, 0, false, false, ""},

		// A2：曾有请求、3 天内静默（W1 不足但连 10 条都没有），W2 达标。
		{"A2 W1 静默 W2 达标 -> extended", 0, 100, true, true, BaselineSourceExtended},
		{"A2 W1 少量但仍 < 10 -> extended", 9, 100, true, true, BaselineSourceExtended},
		{"A2 W1 恰好 9 条 -> extended（10 是 A4 的门槛）", 9, 500, true, true, BaselineSourceExtended},

		// A4：刚从故障/下线恢复——W1 有 >= 10 条但仍不足下限，W2 达标。
		{"A4 W1 恰好 10 条 -> extended_stale", 10, 100, true, true, BaselineSourceExtendedStale},
		{"A4 W1 恢复中（50 条）-> extended_stale", 50, 500, true, true, BaselineSourceExtendedStale},
		{"A4 W1 差一条到下限 -> extended_stale", 99, 200, true, true, BaselineSourceExtendedStale},

		// A3：W2 不足即不发布——样本下限是**硬约束**（用户裁决 2026-09-22）。
		// 这一组是本次反转的核心：旧实现只看 W1 是否 < 10 条，于是 W1 再多也能用 W2 的
		// 陈旧样本发布 extended_stale，而该来源会供 Recorder 判慢并强制会话冷却。
		{"A3 两窗皆不足且 W1 < 10 -> 不发布", 5, 50, false, false, ""},
		{"A3 两窗皆不足且 W1 = 0 -> 不发布", 0, 99, false, false, ""},
		{"A3 W1 充足但 W2 不足 -> 不发布（反转：旧实现发 extended_stale）", 20, 99, false, false, ""},
		{"A3 W1 与 W2 都差一条到下限 -> 不发布（下限是硬约束）", 99, 99, false, false, ""},

		// A4 的正面控制：W2 恰好达标时，W1 >= 10 条仍发 extended_stale（硬约束只砍 W2 不足）。
		{"A4 W2 恰好等于下限 -> extended_stale", 20, 100, true, true, BaselineSourceExtendedStale},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideBaseline(tc.w1, tc.w2, min)
			if got.Publish != tc.wantPublish {
				t.Fatalf("DecideBaseline(%d,%d,%d).Publish = %v，期望 %v",
					tc.w1, tc.w2, min, got.Publish, tc.wantPublish)
			}
			if !tc.wantPublish {
				return
			}
			if got.UseExtendedWindow != tc.wantExt {
				t.Fatalf("DecideBaseline(%d,%d,%d).UseExtendedWindow = %v，期望 %v",
					tc.w1, tc.w2, min, got.UseExtendedWindow, tc.wantExt)
			}
			if got.Source != tc.wantSource {
				t.Fatalf("DecideBaseline(%d,%d,%d).Source = %q，期望 %q",
					tc.w1, tc.w2, min, got.Source, tc.wantSource)
			}
		})
	}
}

// TestDecideBaselineRespectsMinSamplesOverride 钉住样本下限可逐渠道覆写（B1 的列）。
func TestDecideBaselineRespectsMinSamplesOverride(t *testing.T) {
	// 下限 10 时，30 条样本就该走主窗（默认下限 100 下会退回扩展窗）。
	if got := DecideBaseline(30, 500, 10); !got.Publish || got.UseExtendedWindow {
		t.Fatalf("下限 10 时 W1=30 应走主窗 primary，实得 %+v", got)
	}
	// 下限 50 时，30 条不足 → 走扩展窗；且 W1 >= 10 故为 extended_stale。
	if got := DecideBaseline(30, 500, 50); got.Source != BaselineSourceExtendedStale {
		t.Fatalf("下限 50 时 W1=30 应走 extended_stale，实得 %+v", got)
	}
	// 非正下限回落默认（100）：W1=50 < 100 故不进主窗。这条同时证明回落真的发生了——
	// 若未回落（按 0 判定），W1=50 >= 0 会走主窗，此处即红。
	if got := DecideBaseline(50, 500, 0); !got.Publish || !got.UseExtendedWindow {
		t.Fatalf("下限非正应回落默认 100（W1=50 不足）→ 扩展窗，实得 %+v", got)
	}
}

// TestMedianRate 钉住中位数算法（含偶数个样本、全相同、极端值、输入不被修改）。
//
// 为何用中位数而非均值：劣化段约 26% 的慢请求会把均值拖低，中位数纹丝不动
// （设计稿 §3 实证：p50 204.3 vs 健康段 240.3）。这条测试是那个决策的执行保证。
func TestMedianRate(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"单元素", []float64{42}, 42},
		{"奇数个（无序）", []float64{5, 1, 3}, 3},
		{"奇数个（已序）", []float64{1, 3, 5}, 3},
		{"偶数个取中间两者均值", []float64{1, 2, 3, 4}, 2.5},
		{"偶数个（无序）", []float64{4, 1, 3, 2}, 2.5},
		{"全相同", []float64{7, 7, 7, 7}, 7},
		{"两元素", []float64{10, 20}, 15},
		// 极端值：均值会被 1 拖到 50.3，中位数仍是 100（这正是选它的理由）。
		{"右尾极端值不拖动中位数", []float64{100, 100, 100, 1}, 100},
		{"左尾极端值不拖动中位数", []float64{100, 100, 100, 1e6}, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MedianRate(tc.values); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("MedianRate(%v) = %v，期望 %v", tc.values, got, tc.want)
			}
		})
	}
}

// TestMedianRateDoesNotMutateInput 钉住「输入不被修改」——调用方可能复用同一切片。
func TestMedianRateDoesNotMutateInput(t *testing.T) {
	values := []float64{5, 1, 3}
	_ = MedianRate(values)
	if values[0] != 5 || values[1] != 1 || values[2] != 3 {
		t.Fatalf("MedianRate 修改了入参：%v", values)
	}
}

// TestMedianRateEmpty 钉住空输入返回 NaN（调用方必须先判空，见 medianForWindow）。
func TestMedianRateEmpty(t *testing.T) {
	if got := MedianRate(nil); !math.IsNaN(got) {
		t.Fatalf("MedianRate(nil) = %v，期望 NaN", got)
	}
}

// TestSlowLineRatioIsPerMille 钉住「系数是千分比整数」只有一处定义（默认 300 = 0.3）。
//
// wb 验算：历史中位 239.7 × 0.3 ≈ 71.9 tok/s（系数原为 0.2 时的 47.9 见设计稿 §3，
// 用户 2026-09-21 上调到 0.3 以收紧低速判定）。
func TestSlowLineRatioIsPerMille(t *testing.T) {
	if got := SlowLine(241.1, 200); math.Abs(got-48.22) > 1e-6 {
		t.Fatalf("SlowLine(241.1, 200) = %v，期望 48.22", got)
	}
	if got := SlowLine(100, 500); math.Abs(got-50) > 1e-9 {
		t.Fatalf("SlowLine(100, 500) = %v，期望 50", got)
	}
	// 非正系数回落默认 300 ⇒ 100 × 0.3 = 30。
	if got := SlowLine(100, 0); math.Abs(got-30) > 1e-9 {
		t.Fatalf("SlowLine(100, 0) = %v，期望回落默认后的 30", got)
	}
}

// TestBaselineKeyHasHashTag 钉住键形制（设计稿 §4，冻结）。
//
// 花括号是 Redis Cluster hash tag：同一（渠道 x 模型）的三个键（samples/state/baseline）
// 必须落同一槽，否则跨键操作会被 CROSSSLOT 拒绝。
func TestBaselineKeyHasHashTag(t *testing.T) {
	if got := BaselineKey(167, "deepseek-v4.1-flash"); got != "cch:slow:{167:deepseek-v4.1-flash}:baseline" {
		t.Fatalf("BaselineKey = %q", got)
	}
	// 形制必须能被清扫前缀匹配到（否则残留键永远清不掉）。
	key := BaselineKey(1, "m")
	if key[:len(baselineKeyPrefix)] != baselineKeyPrefix {
		t.Fatalf("键 %q 不以清扫前缀 %q 开头", key, baselineKeyPrefix)
	}
	if key[len(key)-len(baselineKeySuffix):] != baselineKeySuffix {
		t.Fatalf("键 %q 不以 %q 结尾", key, baselineKeySuffix)
	}
}

// TestEffectiveHelpersFallBackOnUnset 钉住逐渠道参数为空/NULL 时的回落（B1 的列可空）。
func TestEffectiveHelpersFallBackOnUnset(t *testing.T) {
	if got := effectiveMinSamples(nil); got != slowRateBaselineDefaultMinSamples {
		t.Fatalf("effectiveMinSamples(nil) = %d", got)
	}
	if got := effectiveRatioPerMille(nil); got != slowRateBaselineDefaultRatioPerMille {
		t.Fatalf("effectiveRatioPerMille(nil) = %d", got)
	}
	if got := effectiveW1Span(0); got != slowRateBaselineW1Span {
		t.Fatalf("effectiveW1Span(0) = %v", got)
	}
	// 逐渠道覆写要生效。
	min, ratio := 7, 333
	if got := effectiveMinSamples(&min); got != 7 {
		t.Fatalf("effectiveMinSamples(&7) = %d", got)
	}
	if got := effectiveRatioPerMille(&ratio); got != 333 {
		t.Fatalf("effectiveRatioPerMille(&333) = %d", got)
	}
	// 非正值一律回落（0 与负值都是「没设」的等价写法）。
	zero := 0
	if got := effectiveMinSamples(&zero); got != slowRateBaselineDefaultMinSamples {
		t.Fatalf("effectiveMinSamples(&0) = %d，期望回落默认", got)
	}
}

// TestGroupProviderConfigsByWindow 钉住「按基线窗口长度分组」——基线窗逐渠道可覆写，
// 而窗口边界进的是同一条计数查询，故必须先分组再查：同组共用一个 w1Start。
//
// 注意：分组只看 slow_rate_baseline_window_seconds，**不看**判定滑窗
// （slow_rate_window_seconds）。两者曾共用一列，导致「调判定窗打坏基线」；
// 该缺陷的专项回归见 slowrate_window_split_test.go。
func TestGroupProviderConfigsByWindow(t *testing.T) {
	short := 600
	configs := []store.SlowRateProviderConfig{
		{ProviderID: 1}, // 默认窗
		{ProviderID: 2}, // 默认窗
		{ProviderID: 3, BaselineWindowSeconds: &short}, // 覆写为 600s
	}
	groups := groupProviderConfigsByWindow(configs)
	if len(groups) != 2 {
		t.Fatalf("应分成 2 组，实得 %d 组：%v", len(groups), groups)
	}
	defaultWindow := int(slowRateBaselineW1Span / time.Second)
	if len(groups[defaultWindow]) != 2 {
		t.Fatalf("默认窗组应有 2 个渠道，实得 %d", len(groups[defaultWindow]))
	}
	if len(groups[600]) != 1 {
		t.Fatalf("600s 组应有 1 个渠道，实得 %d", len(groups[600]))
	}

	// providerIDsOf 必须把组内 ID 原样带出（计数查询的入参）。
	ids := providerIDsOf(groups[defaultWindow])
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("providerIDsOf = %v，期望 [1 2]", ids)
	}

	// 非正覆写回落默认窗（与「没设」同义）。
	zero := 0
	fallback := groupProviderConfigsByWindow([]store.SlowRateProviderConfig{{ProviderID: 9, BaselineWindowSeconds: &zero}})
	if len(fallback[defaultWindow]) != 1 {
		t.Fatalf("基线窗 0 应回落默认窗，实得 %v", fallback)
	}
}

// TestEffectiveW1SpanHonorsOverride 钉住窗口长度覆写真的生效（不只是分组）。
func TestEffectiveW1SpanHonorsOverride(t *testing.T) {
	if got := effectiveW1Span(600); got != 10*time.Minute {
		t.Fatalf("effectiveW1Span(600) = %v，期望 10m", got)
	}
	if got := effectiveW1Span(-1); got != slowRateBaselineW1Span {
		t.Fatalf("effectiveW1Span(-1) = %v，期望回落默认", got)
	}
}
