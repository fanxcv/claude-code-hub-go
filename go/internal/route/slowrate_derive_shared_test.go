package route

import (
	"os"
	"strings"
	"testing"
)

// 本文件钉住「低速惩罚的**派生公式**在读写两侧只有一个实现」。
//
// 为什么需要它（这是既有 mirror 钉子的**盲区**，另一审点出）：
// `slowrate_params_mirror_test.go` 只比两件事——
//  1. 四个参数的**出厂值**（从写侧源码抽字面量）；
//  2. **归一化闸门**的形态（`<= 0` 取默认）。
//
// 它**抓不住派生公式本身的漂移**：公式（阈值门 → 整数分档 → 封顶）不在它的比对范围内。
// 具体漏检例子：写侧把封顶写成 `if penalty > params.PenaltyMax+10`（或干脆删掉封顶），
// 四个出厂值与归一化闸门仍然逐字相同 ⇒ 既有 mirror 全绿，而**界面显示的降权量与选路
// 实际用的降权量从此是两个数**（读侧读时派生、写侧落库快照），且静默。
//
// 修法（比「两边各写一份公式再对拍源码」更彻底）：**两侧共用同一个纯函数**
// `route.DeriveSlowRatePenalty`。`slowrate` 可以 import `route`（反向才成环：
// slowrate → session → guard → route），故本包是能放下这个函数的唯一位置。
// 于是「不漂移」不再靠测试，而是**结构上不可能漂移**。
//
// 本文件的钉子因此转为钉「那两处**确实都在调它**」——否则有人可能把公式又抄回写侧、
// 留下一个看似共用实则分叉的假象。

// TestSlowRateWriteSideUsesSharedDerive 钉住写侧（slowrate）调共用纯函数，而不是自己算一遍。
func TestSlowRateWriteSideUsesSharedDerive(t *testing.T) {
	source, err := os.ReadFile("../slowrate/recorder.go")
	if err != nil {
		t.Fatalf("读取写侧源码失败：%v", err)
	}
	body := string(source)
	if !strings.Contains(body, "route.DeriveSlowRatePenalty(") {
		t.Error("写侧不再调 route.DeriveSlowRatePenalty——公式被抄回写侧后两侧即可静默分叉" +
			"（既有 mirror 钉子只比出厂值与归一化闸门，抓不住派生公式的漂移）")
	}
	// 反向：写侧不得再出现「自己分档」的形态（`level := count / params.TriggerCount`）。
	if strings.Contains(body, "level := count / params.TriggerCount") {
		t.Error("写侧又出现了自行分档的代码——派生公式必须只有一处实现")
	}
}

// TestSlowRateReadSideUsesSharedDerive 钉住读侧（本包）也走同一个函数。
func TestSlowRateReadSideUsesSharedDerive(t *testing.T) {
	source, err := os.ReadFile("slowrate.go")
	if err != nil {
		t.Fatalf("读取本包源码失败：%v", err)
	}
	body := string(source)
	if !strings.Contains(body, "DeriveSlowRatePenalty(liveCount,") {
		t.Error("读侧不再调 DeriveSlowRatePenalty——派生公式必须两处共用")
	}
}

// TestDeriveSlowRatePenaltyShape 钉住共用函数的三个环节（阈值门 / 分档 / 封顶）。
//
// 用行为断言而非源码断言：这里是唯一实现，行为就是事实。
// 取用户 2026-09-22 确认的口径（阈值 3、步长 10、封顶 30）与「3→10、6→20、9→30」的映射。
func TestDeriveSlowRatePenaltyShape(t *testing.T) {
	const (
		trigger = 3
		step    = 10
		max     = 30
	)
	cases := []struct {
		liveCount int
		want      int
		why       string
	}{
		{0, 0, "无慢样本不降权"},
		{1, 0, "未达触发阈值不降权（阈值 3 ⇒ 第 1、2 条都不降）"},
		{2, 0, "同上，边界值 2"},
		{3, 10, "达阈值进第一档"},
		{5, 10, "不足两档仍为第一档（整数分档）"},
		{6, 20, "两档"},
		{9, 30, "三档"},
		{30, 30, "封顶：再多也不超过 max"},
		{300, 30, "封顶在大数值上同样成立"},
	}
	for _, item := range cases {
		if got := DeriveSlowRatePenalty(item.liveCount, trigger, step, max); got != item.want {
			t.Errorf("liveCount=%d：%s，期望 %d，实得 %d", item.liveCount, item.why, item.want, got)
		}
	}
	// 阈值 ≤ 0 是「参数缺失」的形态：必须返回 0（不能除零 panic，也不能恒降权）。
	if got := DeriveSlowRatePenalty(5, 0, step, max); got != 0 {
		t.Errorf("阈值 0（参数缺失）应返回 0，实得 %d——若不设这道门会除零", got)
	}
}
