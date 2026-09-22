package slowrate

import (
	"testing"
	"time"
)

// 本文件钉住提交前速率闸阈值的**推导口径**与停滞探测出厂值的**生效条件**。
//
// 为何单独钉推导：它是「基线（tok/s）⇒ 阈值（字节/秒）」的唯一换算点，单位与系数两处都容易
// 改错；而改错的后果是「闸永不触发」或「大范围误杀」，两种都只在生产上才看得出来。

func TestDerivePrecommitMinBytesPerSecond(t *testing.T) {
	cases := []struct {
		name     string
		baseline float64
		ratio    float64
		want     int
	}{
		// 生产实证量级：wb × deepseek-v4.1-flash 基线中位 242.6 tok/s、系数 0.3。
		// 242.6 × 0.3 × 4 = 291.12 ⇒ 四舍五入 291。
		{"wb 实测量级", 242.6, 0.3, 291},
		// 恰好整除时不引入浮点误差。
		{"整齐值", 50, 0.5, 100},
		// 小数进位（.5 向上，与 round-half-up 一致）。
		{"四舍五入向上", 10, 0.3, 12},
		// 任一输入非正 ⇒ 不启用。缺基线时**不得**退回某个猜测值。
		{"基线为 0", 0, 0.3, 0},
		{"基线为负", -1, 0.3, 0},
		{"系数为 0", 242.6, 0, 0},
		{"系数为负", 242.6, -0.3, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := DerivePrecommitMinBytesPerSecond(testCase.baseline, testCase.ratio)
			if got != testCase.want {
				t.Fatalf("推导应为 %d，实得 %d（baseline=%v ratio=%v）",
					testCase.want, got, testCase.baseline, testCase.ratio)
			}
		})
	}
}

// TestPrecommitBytesPerTokenIsApproximation 钉住换算比是**待标定常量**而非可调配置。
//
// 它若被改成看似精确的值（例如按语言分支），就得同时改掉「由影子数据标定」这条计划；
// 本用例的作用是让那次修改必须显式面对这条注释。
func TestPrecommitBytesPerTokenIsApproximation(t *testing.T) {
	if PrecommitBytesPerToken != 4 {
		t.Fatalf("换算比应为 4（英文/JSON 经验中值，待影子数据标定），实得 %d", PrecommitBytesPerToken)
	}
	if DefaultProbeAfterFirstByteSeconds != 30 {
		t.Fatalf("停滞探测出厂值应为 30，实得 %d", DefaultProbeAfterFirstByteSeconds)
	}
}

// TestProbeParamsZeroMeansDisabled 钉住「显式 0 = 不探测」这条存量语义不因本次修正而变。
//
// 修正只改「列 NULL（未覆盖）⇒ 取出厂值」这一步（在 dataplane.timeoutsFromRow），
// 判定函数本身对 <= 0 仍恒 false——两处若一起改，存量「主动关掉」的渠道会被静默打开。
func TestProbeParamsZeroMeansDisabled(t *testing.T) {
	if IsSlowProbe(ProbeInput{ElapsedSinceFirstByte: 999 * time.Second}, ProbeParams{AfterFirstByteSeconds: 0}) {
		t.Fatal("T=0 必须恒不触发（显式关闭）")
	}
	if !IsSlowProbe(ProbeInput{ElapsedSinceFirstByte: 31 * time.Second}, ProbeParams{AfterFirstByteSeconds: 30}) {
		t.Fatal("T=30、已过 31s 应触发")
	}
	// 边界：恰好到期即触发（判据是 >=，不是 >）。
	if !IsSlowProbe(ProbeInput{ElapsedSinceFirstByte: 30 * time.Second}, ProbeParams{AfterFirstByteSeconds: 30}) {
		t.Fatal("T=30、恰好 30s 应触发（判据为 >=）")
	}
}
