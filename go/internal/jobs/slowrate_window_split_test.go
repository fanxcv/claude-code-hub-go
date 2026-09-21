package jobs

import (
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 slow_rate_window_seconds 的**一列两语义**缺陷修复（2026-09-21，方案 A）。
//
// 缺陷形态：该列同时被两处按不同尺度读——
//   - B2 判定滑窗（slowrate.DefaultParams().WindowSeconds，分钟级）；
//   - B3 基线主窗 W1（本包，天级）。
//
// 用户把 wb 的 slow_rate_window_seconds 设成 1800（本意「统计窗口 30 分钟」），B3 便改按
// 最近 30 分钟聚合算基线；而样本下限是 50，30 分钟内凑不齐，基线于是落到扩展窗并降级为
// extended_stale（只做会话级降级、不做渠道级 penalty）——**渠道级降权静默失效**，且无任何报错。
//
// 修法：新增 slow_rate_baseline_window_seconds 专供 B3，判定滑窗仍走原列。本文件钉住
// 「B3 只看基线列」这一条，因为它是本次缺陷能被静默放过的地方：两列都叫 window，读错了
// 功能照跑、测试照绿（历史上就是这么漏过去的）。

// TestBaselineWindowIgnoresSamplingWindow 是本次缺陷的**直接回归**。
//
// 变异反证：把 groupProviderConfigsByWindow 改回读 config.WindowSeconds → 本用例必红。
func TestBaselineWindowIgnoresSamplingWindow(t *testing.T) {
	// 判定滑窗给一个「不像默认」的值：1800s（生产 wb 的实际取值，本意 30 分钟统计窗）。
	samplingOnly := 1800
	// 基线窗给另一个明确值：7200s（2 小时）。
	baselineOnly := 7200

	configs := []store.SlowRateProviderConfig{
		// 这条只设了判定滑窗：基线窗未设 ⇒ 必须落**默认 3 天**那组，
		// 绝不能因 1800 而落进 1800 那组（那就是缺陷本身）。
		{ProviderID: 1, WindowSeconds: &samplingOnly},
		// 这条只设了基线窗：必须落 7200 那组。
		{ProviderID: 2, BaselineWindowSeconds: &baselineOnly},
	}
	groups := groupProviderConfigsByWindow(configs)

	defaultWindow := int(slowRateBaselineW1Span / time.Second)
	if len(groups) != 2 {
		t.Fatalf("应分成 2 组（默认天级 + 7200s），实得 %d 组：%v", len(groups), groups)
	}

	if got := groups[defaultWindow]; len(got) != 1 || got[0].ProviderID != 1 {
		t.Fatalf("只设判定滑窗(1800) 的渠道应落默认窗(%ds)那组，实得 %v", defaultWindow, got)
	}
	if got := groups[baselineOnly]; len(got) != 1 || got[0].ProviderID != 2 {
		t.Fatalf("设基线窗(7200) 的渠道应落 7200 组，实得 %v", got)
	}
	// 反面：绝不允许出现「按判定滑窗 1800 分组」的组。
	if _, exists := groups[samplingOnly]; exists {
		t.Fatalf("出现了按判定滑窗(%d) 分组的组：B3 又在读 slow_rate_window_seconds 了（本次缺陷复发）", samplingOnly)
	}
}

// TestEffectiveW1SpanTakesBaselineValue 钉住 effectiveW1Span 收到的确实是基线窗取值。
//
// 与上面互补：上面钉「分组用哪列」，这里钉「跨度算出来是多大」——两处都读对才算修好。
func TestEffectiveW1SpanTakesBaselineValue(t *testing.T) {
	twoHours := 7200
	if got := effectiveW1Span(twoHours); got != 2*time.Hour {
		t.Fatalf("effectiveW1Span(%d) = %v，期望 2h", twoHours, got)
	}
	// nil / 非正一律回落默认 3 天（与「没设」同义）。
	for _, zero := range []int{0, -1} {
		if got := effectiveW1Span(zero); got != slowRateBaselineW1Span {
			t.Fatalf("effectiveW1Span(%d) = %v，期望回落默认 3 天", zero, got)
		}
	}
	// 非正基线窗列也要回落默认（NULL 与 0 是「没设」的等价写法）。
	zero := 0
	groups := groupProviderConfigsByWindow([]store.SlowRateProviderConfig{{ProviderID: 9, BaselineWindowSeconds: &zero}})
	defaultWindow := int(slowRateBaselineW1Span / time.Second)
	if len(groups[defaultWindow]) != 1 {
		t.Fatalf("基线窗 0 应回落默认窗，实得 %v", groups)
	}
}

// TestBaselineDefaultRatioPerMilleIsThreeTenths 钉住系数默认值（用户 2026-09-21 定 0.3）。
func TestBaselineDefaultRatioPerMilleIsThreeTenths(t *testing.T) {
	if slowRateBaselineDefaultRatioPerMille != 300 {
		t.Fatalf("低速线系数默认 %d，应为 300（0.3）", slowRateBaselineDefaultRatioPerMille)
	}
	// 回落路径要用的是**新**默认，不是旧字面量。
	if got := effectiveRatioPerMille(nil); got != 300 {
		t.Fatalf("effectiveRatioPerMille(nil) = %d，应为 300", got)
	}
	// 逐渠道覆写仍优先。
	custom := 250
	if got := effectiveRatioPerMille(&custom); got != 250 {
		t.Fatalf("effectiveRatioPerMille(&250) = %d，覆写未生效", got)
	}
	// 低速线 = 中位数 × 系数：400 × 0.3 = 120。
	if got := SlowLine(400, 0); got != 120 {
		t.Fatalf("SlowLine(400, 0) = %v，期望 120（400×0.3）", got)
	}
}
