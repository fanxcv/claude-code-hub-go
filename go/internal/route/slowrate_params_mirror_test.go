package route

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 本文件钉住「低速四个降权参数的**出厂值**与**归一化判据**在读写两侧不分叉」。
//
// 为何需要跨包钉子：本包（route）不能 import slowrate —— slowrate → session → guard → route
// 成环（见 slowrate.go 文件头）。于是同一份出厂值必然在仓内写两遍，唯一的防线是测试；
// 同类手法见 slowrate_keys_mirror_test.go（键形制）与 slowrate_ratio_unit_mirror_test.go（系数单位）。
//
// 与那两条不同的是，本文件不只比字面量，还比**判据**（归一化的边界规则）：只比字面量挡不住
// 「字面量对、闸门逻辑分叉」——例如一侧把 `≤ 0 取默认` 写成 `== 0 取默认`，负值就会一路乘进
// 判定，而两侧的出厂值仍然相等。

// slowRateWriteSideSource 读写侧源码。路径相对本包目录（与既有镜像钉子同法）。
func slowRateWriteSideSource(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("../slowrate/recorder.go")
	if err != nil {
		t.Fatalf("读取写侧源码失败：%v", err)
	}
	return string(source)
}

// slowRateFuncBody 截出某个函数的函数体（按首个 `{` 到行首 `}` 收尾）。
func slowRateFuncBody(t *testing.T, source, signature string) string {
	t.Helper()
	start := strings.Index(source, signature)
	if start < 0 {
		t.Fatalf("写侧源码里找不到 %q", signature)
	}
	rest := source[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("写侧源码里 %q 的函数体没有收尾", signature)
	}
	return rest[:end]
}

// slowRateIntField 从函数体里抽出 `Field: <整数>` 的字面量。
func slowRateIntField(t *testing.T, body, field string) int {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(field) + `:\s*(-?\d+),`)
	match := pattern.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("写侧源码里找不到字段 %s 的整数字面量", field)
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("解析字段 %s 失败：%v", field, err)
	}
	return value
}

// TestSlowRatePenaltyDefaultsMirrorWriteSide 从写侧源码里抽出四个出厂值，与读侧逐值比对。
//
// 为何从源码抽而不是在用例里再写一遍：在用例里写第三份字面量，只是把「两处要同步」变成
// 「三处要同步」，而且判据会跟着一起改错（改完两边都错、测试仍绿）。
func TestSlowRatePenaltyDefaultsMirrorWriteSide(t *testing.T) {
	body := slowRateFuncBody(t, slowRateWriteSideSource(t), "func DefaultParams() Params {")
	got := slowRatePenaltyDefaults()
	for field, actual := range map[string]int{
		"WindowMinutes": got.windowMinutes,
		"TriggerCount":  got.triggerCount,
		"PenaltyStep":   got.penaltyStep,
		"PenaltyMax":    got.penaltyMax,
	} {
		want := slowRateIntField(t, body, field)
		if actual != want {
			t.Errorf("出厂值分叉：读侧 %s=%d，写侧 slowrate.DefaultParams() %s=%d——两侧必须逐值相同",
				field, actual, field, want)
		}
	}
}

// TestSlowRatePenaltyNormalizeRuleMirrorWriteSide 钉住四个字段的归一化判据在两侧同为
// 「≤ 0 即取出厂默认」。
//
// 为什么判据也要钉：出厂值相等而闸门不同，行为就会分叉——写侧若写成 `== 0 取默认`，
// 阈值 -1 会被原样采用（每一条样本都算慢）；读侧若同样放宽，负窗长会算出 `now - (-x)`
// 这种把整个滑窗搬到未来的下界，计数恒 0。两侧必须同判。
func TestSlowRatePenaltyNormalizeRuleMirrorWriteSide(t *testing.T) {
	body := slowRateFuncBody(t, slowRateWriteSideSource(t), "func (p Params) normalize() Params {")
	for _, field := range []string{"WindowMinutes", "TriggerCount", "PenaltyStep", "PenaltyMax"} {
		gate := "if p." + field + " <= 0 {"
		if !strings.Contains(body, gate) {
			t.Errorf("写侧 normalize 里找不到 %q——四个字段的判据必须都是「≤ 0 即取默认」，与读侧同判",
				gate)
		}
	}
}

// TestSlowRateRatioGateStaysPositiveConjunction 钉住写侧系数的闸门是**正向合取**
// `!(p.Ratio > 0 && p.Ratio <= 1)`，而不是 `p.Ratio <= 0`。
//
// 为什么这条跨包钉子写在这里（系数并不参与读侧的档位计算）：它对读侧有真实后果——写侧按系数
// 判「这条样本慢不慢」，判据若被「简化」成 `<= 0`，NaN 会一路乘进低速线，使判定静默恒假
// （永不标慢），于是滑窗里永远没有成员，读侧的降权**永远不触发**。两侧耦合的是同一件事：
// 写侧判不出慢，读侧就无权重可算。
//
// 边界行为本身（NaN 收敛到默认）由写侧包内的 TestNormalizeRejectsInvalidFraction 钉；
// 这里只钉判据形态，不在两侧各写一份行为断言。
func TestSlowRateRatioGateStaysPositiveConjunction(t *testing.T) {
	source := slowRateWriteSideSource(t)
	if !strings.Contains(source, "if !(p.Ratio > 0 && p.Ratio <= 1) {") {
		t.Error("写侧系数闸门不再是正向合取 `!(p.Ratio > 0 && p.Ratio <= 1)`——" +
			"`<= 0` 会漏掉 NaN（它既非 >0 也非 <=0），使低速判定静默恒假，读侧的降权因此永不触发")
	}
}
