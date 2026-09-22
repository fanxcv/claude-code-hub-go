package slowrate

import (
	"os"
	"strings"
	"testing"
)

// 本文件是**源码结构性钉子**：断言写侧真的把四个「生效参数」随状态一起落盘。
//
// 为什么必须有它：读侧（internal/route）据这四个字段把滑窗内的慢样本数折成惩罚；字段一旦
// 漏写或改名，读侧会判成「参数缺失」而**静默回退**去读快照——那就退回成本次刚修掉的
// 「惩罚只涨不落（+10 出现后一直不消失）」，而读侧那几条钉子用的是带参数的夹具，全都不会红。
// 这条接缝横跨两个包、字段名各写一遍字面量，没有任何一处单测盖得住，故用源码发现钉死
// （同类手法见 dataplane/custom_headers_wiring_nail_test.go 的说明）。
func TestStateWriteCarriesEveryEffectiveParam(t *testing.T) {
	source, err := os.ReadFile("recorder.go")
	if err != nil {
		t.Fatalf("读取 recorder.go 失败：%v", err)
	}
	for _, want := range []string{
		"StateFieldPenalty, penalty,",
		"StateFieldWindowMinutes, params.WindowMinutes,",
		"StateFieldTriggerCount, params.TriggerCount,",
		"StateFieldPenaltyStep, params.PenaltyStep,",
		"StateFieldPenaltyMax, params.PenaltyMax,",
	} {
		if !strings.Contains(string(source), want) {
			t.Errorf("recorder.go 的状态写入里找不到 %q：\n"+
				"  参数缺失会让读侧静默回退到快照，惩罚退化为「只涨不落」。", want)
		}
	}
}
