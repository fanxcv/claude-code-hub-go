package route

import (
	"os"
	"strings"
	"testing"
)

// 本文件钉住「低速系数是 0-1 小数」这一口径在**两个包之间不分叉**（用户 2026-09-22 改单位）。
//
// 为何需要一条跨包钉子：系数的乘法在两个包各写一遍——
//   - 写/判定侧 slowrate：`isSlow` 用 `rate < baseline*ratio`，且 `Params.Ratio` 是小数；
//   - 基线侧 jobs：`SlowLine` 用 `median * ratio`，写进基线 payload 的 slowLine 字段。
//
// 两侧若一处按小数、一处按千分比，低速线相差 1000 倍且**静默**（各自的用例都不红）。
// 本包（route）不能 import 那两个包的内部实现细节，故用**源码结构性钉子**读它们的源码，
// 断言两边的系数乘法都是 `× ratio` 而没有 `/1000` 残留——这正是「只改一处」会留下的痕迹。
//
// 同类手法见 slowrate_keys_mirror_test.go（键形制的跨包镜像钉子）。

// TestSlowRateRatioUnitIsFractionOnBothSides 钉住两侧的系数乘法口径一致（都是小数、都不除 1000）。
func TestSlowRateRatioUnitIsFractionOnBothSides(t *testing.T) {
	sides := []struct {
		name string
		path string
		want string // 必须出现的乘法形态（小数直乘）
	}{
		{
			name: "slowrate.isSlow",
			path: "../slowrate/recorder.go",
			want: "return rate < baseline*params.Ratio",
		},
		{
			name: "jobs.SlowLine",
			path: "../jobs/slowrate_baseline.go",
			want: "return median * ratio",
		},
	}
	for _, side := range sides {
		source, err := os.ReadFile(side.path)
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", side.path, err)
		}
		text := string(source)
		if !strings.Contains(text, side.want) {
			t.Errorf("%s 里找不到小数口径的乘法 %q——\n"+
				"  该侧可能仍在按千分比除 1000，与另一侧相差 1000 倍且静默（用户 2026-09-22 裁决改小数）。",
				side.name, side.want)
		}
		// 反向：那两处乘法里不得再有 /1000。
		for _, forbidden := range []string{"/ 1000", "/1000"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s 里仍有 %q 残留：系数是 0-1 小数，不除 1000（两侧口径必须一致）",
					side.name, forbidden)
			}
		}
	}
}
