package jobs

import (
	"math"
	"os"
	"strings"
	"testing"
)

// 本文件钉住**系数单位**（千分比整数 → 0-1 小数，用户 2026-09-22 裁决）在**本包这一侧**的实现。
//
// 为何必须单独钉：系数的乘法在两个包各写一遍——本包 SlowLine（`median * ratio`）与
// slowrate 的 isSlow（`rate < baseline*ratio`）。改单位时只改一处，两侧判定的低速线就相差
// 1000 倍，而且**静默**：慢样本判定（isSlow）用一条线、基线里记下的 SlowLine 是另一条，
// 两条线各自都有用例，谁都不红。故本包同时钉「行为」与「源码里没有 /1000 残留」。

// TestSlowLineUsesFractionRatioWithoutPerMilleDivision 钉住 SlowLine 直接乘系数、不除 1000。
func TestSlowLineUsesFractionRatioWithoutPerMilleDivision(t *testing.T) {
	// 中位数 100、系数 0.3 ⇒ 低速线 30（千分比写法会给 0.03）。
	if got := SlowLine(100, 0.3); math.Abs(got-30) > 1e-9 {
		t.Fatalf("SlowLine(100, 0.3) = %v，期望 30——若得到 0.03 说明仍在除 1000", got)
	}
	// 非法值一律回落默认 0.3，尤其 NaN（小数域里 `<=0` 挡不住它）。
	for _, invalid := range []float64{0, -0.3, 1.5, 300, math.NaN()} {
		if got := SlowLine(100, invalid); math.Abs(got-30) > 1e-9 {
			t.Errorf("SlowLine(100, %v) = %v，期望回落默认后的 30", invalid, got)
		}
	}
}

// TestSlowLineSourceHasNoPerMilleDivision 是**源码结构性钉子**：本包不得再有 `ratio` 除以 1000 的写法。
//
// 为何行为用例不够：行为用例只覆盖「传进来的 ratio 是小数」这一种情形。若有人把 SlowLine 写成
// 「先乘再除 1000」而同时把调用方改成传千分比（两处一起改回旧口径），行为用例会按新口径全绿，
// 而与 slowrate.isSlow 的**口径分叉**（那边仍按小数）就无人发现。这条钉子把「不除 1000」这件事
// 钉在源码上：该函数的函数体内不得出现 `/ 1000` 或 `/1000`。
func TestSlowLineSourceHasNoPerMilleDivision(t *testing.T) {
	source, err := os.ReadFile("slowrate_baseline.go")
	if err != nil {
		t.Fatalf("读取 slowrate_baseline.go 失败：%v", err)
	}
	body := slowLineFunctionBody(string(source))
	if body == "" {
		t.Fatal("找不到 func SlowLine 的函数体（改名了？同步更新本钉子）")
	}
	for _, forbidden := range []string{"/ 1000", "/1000"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("SlowLine 的函数体里仍有 %q：系数是 0-1 小数，不除 1000。\n"+
				"  与 slowrate.isSlow 的口径分叉会让两侧低速线相差 1000 倍且静默。\n函数体:\n%s",
				forbidden, body)
		}
	}
}

// slowLineFunctionBody 截出 func SlowLine 的{...}体（够用的朴素括号配对，不引解析器）。
func slowLineFunctionBody(source string) string {
	start := strings.Index(source, "func SlowLine(")
	if start < 0 {
		return ""
	}
	open := strings.Index(source[start:], "{")
	if open < 0 {
		return ""
	}
	open += start
	depth := 0
	for index := open; index < len(source); index++ {
		switch source[index] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[open : index+1]
			}
		}
	}
	return ""
}

// TestEffectiveRatioAcceptsOnlyOpenClosedUnitInterval 钉住逐渠道覆写的合法域 (0,1]。
func TestEffectiveRatioAcceptsOnlyOpenClosedUnitInterval(t *testing.T) {
	for _, valid := range []float64{0.01, 0.3, 1} {
		value := valid
		if got := effectiveRatio(&value); got != valid {
			t.Errorf("effectiveRatio(&%v) = %v，合法值不该被改写", valid, got)
		}
	}
	for _, invalid := range []float64{0, -0.1, 1.0001, math.NaN()} {
		value := invalid
		if got := effectiveRatio(&value); got != slowRateBaselineDefaultRatio {
			t.Errorf("effectiveRatio(&%v) = %v，应回落默认 %v", invalid, got, slowRateBaselineDefaultRatio)
		}
	}
}
