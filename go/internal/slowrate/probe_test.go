package slowrate

import (
	"testing"
	"time"
)

// 本文件钉住「首字后停滞探测」的判据（IsSlowProbe）。
//
// 这条判据的用途是**中止一个正在进行的请求**——它的误判代价不对称：
//   - 误判（本该继续却换了家）⇒ 白烧一次上游调用 + 用户多等一轮；
//   - 漏判（本该换家却让它卡完）⇒ 用户继续干等，但至少结果是对的。
//
// 判据只有时间一条，故测试面窄，但「关闭」与「到点边界」两条必须钉死。

func TestIsSlowProbe(t *testing.T) {
	const threshold = 30

	cases := []struct {
		name    string
		elapsed time.Duration
		params  ProbeParams
		// useParams 为真时用 params 原值；为假时用 threshold。
		// 不靠 `params == (ProbeParams{})` 判断——T=0 的那两个用例恰好就是零值，会被误当「未提供」。
		useParams bool
		wantTrue  bool
	}{
		{
			name:     "已到点仍未出内容 ⇒ 判停滞",
			elapsed:  40 * time.Second,
			wantTrue: true,
		},
		{
			name:     "边界：耗时恰等于 T ⇒ 判停滞（判据是 >=）",
			elapsed:  30 * time.Second,
			wantTrue: true,
		},
		{
			name:     "边界：耗时比 T 少 1ms ⇒ 不判",
			elapsed:  30*time.Second - time.Millisecond,
			wantTrue: false,
		},
		{
			name:     "未到点 ⇒ 不判",
			elapsed:  20 * time.Second,
			wantTrue: false,
		},
		{
			name:      "T = 0 ⇒ 恒不判（机制关闭）",
			elapsed:   40 * time.Second,
			params:    ProbeParams{AfterFirstByteSeconds: 0},
			useParams: true,
			wantTrue:  false,
		},
		{
			name:      "T < 0 ⇒ 恒不判（同样表示关闭）",
			elapsed:   40 * time.Second,
			params:    ProbeParams{AfterFirstByteSeconds: -1},
			useParams: true,
			wantTrue:  false,
		},
		{
			name:      "T = 0 且耗时极长 ⇒ 仍不判（关闭是绝对的）",
			elapsed:   time.Hour,
			params:    ProbeParams{AfterFirstByteSeconds: 0},
			useParams: true,
			wantTrue:  false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			params := testCase.params
			if !testCase.useParams {
				params = ProbeParams{AfterFirstByteSeconds: threshold}
			}
			if got := IsSlowProbe(ProbeInput{ElapsedSinceFirstByte: testCase.elapsed}, params); got != testCase.wantTrue {
				t.Errorf("IsSlowProbe(elapsed=%v, T=%d) = %v，期望 %v",
					testCase.elapsed, params.AfterFirstByteSeconds, got, testCase.wantTrue)
			}
		})
	}
}

// TestProbeThresholdDefaultIsThirtySeconds 钉住出厂阈值（用户 2026-09-21 指定）。
func TestProbeThresholdDefaultIsThirtySeconds(t *testing.T) {
	if DefaultProbeAfterFirstByteSeconds != 30 {
		t.Errorf("DefaultProbeAfterFirstByteSeconds = %d，期望 30", DefaultProbeAfterFirstByteSeconds)
	}
}

// TestProbeHasNoTokenOrRateGate 钉住「判据只有时间一条」这条约束。
//
// 2026-09-21 的裁决：门控在首次分类出内容帧时即提交，故探测窗口内已生成 token 恒为 0，
// token 闸与速率闸在其唯一适用窗口里恒不成立。若有人把速率判据加回来，本用例的
// 编译期与语义双重约束会红——ProbeInput 不再有对应字段，加闸必须先扩结构。
func TestProbeHasNoTokenOrRateGate(t *testing.T) {
	// 语义反证：窗口内 token 恒 0 的前提下，判据**只**随时间推进而翻转。
	params := ProbeParams{AfterFirstByteSeconds: 30}
	if IsSlowProbe(ProbeInput{ElapsedSinceFirstByte: 29 * time.Second}, params) {
		t.Error("未到点不得判停滞")
	}
	if !IsSlowProbe(ProbeInput{ElapsedSinceFirstByte: 31 * time.Second}, params) {
		t.Error("到点且无内容应判停滞——判据不得依赖任何速率或 token 量")
	}
}
