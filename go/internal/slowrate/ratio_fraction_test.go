package slowrate

import (
	"math"
	"testing"
)

// 本文件钉住**系数单位**（千分比整数 → 0-1 小数，用户 2026-09-22 裁决）。
//
// 为何必须单独钉：系数的乘法在**两个包各写一遍**——本包 isSlow（`rate < baseline*ratio`）
// 与 jobs 的 SlowLine（`median * ratio`）。改单位时只改一处，两侧判定的低速线就相差 1000 倍，
// 而且**静默**：错的那一侧会让判定恒真或恒假，不报任何错、测试也照绿（两条路径各有自己的用例）。
// 故本包钉「没有 /1000 残留」，jobs 包用同范式钉同一件事（那边测不到本包的函数）。

// TestIsSlowUsesFractionRatioWithoutPerMilleDivision 钉住 isSlow 直接乘系数、不除 1000。
func TestIsSlowUsesFractionRatioWithoutPerMilleDivision(t *testing.T) {
	// 基线 100、系数 0.3 ⇒ 低速线 30；29 该判慢、31 不该。
	fraction := Params{Ratio: 0.3}
	if !isSlow(29, 100, fraction) {
		t.Fatal("29 < 100×0.3=30，应判为低速（若这里不红而下面红了，说明还在除 1000）")
	}
	if isSlow(31, 100, fraction) {
		t.Fatal("31 > 30，不该判为低速")
	}
	// 反面：若仍按千分比解释（ratio=0.3 ⇒ 0.0003），低速线会变成 0.03，上面第一条就红；
	// 若按「300 千分比」硬乘（ratio 被当 300 用），低速线变 30000，这里必红。
	if isSlow(29999, 100, fraction) {
		t.Fatal("29999 < 30000 不该判慢——红在这里说明系数被当成了千分比整数")
	}
}

// TestNormalizeRejectsInvalidFraction 钉住非法系数一律回落默认 0.3，**尤其 NaN**。
//
// 为何单列 NaN：小数域里 `ratio <= 0` 这种写法对 NaN 恒为假（NaN 与任何值比较都为假），
// 于是 NaN 会一路乘进低速线，使判定**静默恒假**（永不标慢）。正向合取的闸门才挡得住它。
func TestNormalizeRejectsInvalidFraction(t *testing.T) {
	for _, invalid := range []float64{0, -0.1, -1, 1.5, 2, 300, math.NaN(), math.Inf(1), math.Inf(-1)} {
		got := Params{Ratio: invalid}.normalize().Ratio
		if got != DefaultParams().Ratio {
			t.Errorf("系数 %v 应收敛到默认 0.3，得到 %v", invalid, got)
		}
	}
	// 合法区间内的值必须原样保留（含边界 1）。
	for _, valid := range []float64{0.01, 0.3, 0.5, 1} {
		value := valid
		got := Params{Ratio: value}.normalize().Ratio
		if got != valid {
			t.Errorf("合法系数 %v 被改写为 %v", valid, got)
		}
	}
}

// TestIsSlowStaysFalseWhenRatioIsNaN 是上一条的**行为侧对照**。
//
// normalize 是收敛点，但 isSlow 也可能被直接传参调用（测试与将来的调用方）；把 NaN 交给它时，
// 绝不能出现「恒判慢」或「恒不判慢」之外的荒唐结果——这里钉住的是「比较不 panic 且给出确定
// 布尔值」，真正的防 NaN 责任在 normalize。
func TestIsSlowStaysFalseWhenRatioIsNaN(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("isSlow 不应对 NaN 系数 panic: %v", recovered)
		}
	}()
	if isSlow(1, 100, Params{Ratio: math.NaN()}) {
		t.Fatal("NaN 系数下不应判为低速（NaN 比较恒假 ⇒ 判 false）")
	}
}
