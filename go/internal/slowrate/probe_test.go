package slowrate

import (
	"testing"
	"time"
)

// 本文件钉住「首字后低速探测」的判据（IsSlowProbe）。
//
// 这条判据的用途是**中止一个正在进行的请求**——它的误判代价不对称：
//   - 误判为「慢」（本该继续却换了家）⇒ 白烧一次上游调用 + 用户多等一轮；
//   - 漏判（本该换家却让它磨完）⇒ 用户继续干等，但至少结果是对的。
//
// 故三道 gate 各有专测：时间未到、样本太少、速率达标，都必须**不判慢**。

// 生产实测的 wb 基线（09-21 的慢线基准）：239.6755 tok/s，系数 0.3 ⇒ 慢线 71.90665 tok/s。
const probeTestBaseline = 239.6755

func probeParams(thresholdSeconds, minTokens, ratioPerMille int) ProbeParams {
	return ProbeParams{
		AfterFirstByteSeconds: thresholdSeconds,
		MinTokens:             minTokens,
		RatioPerMille:         ratioPerMille,
	}
}

func TestIsSlowProbe(t *testing.T) {
	params := probeParams(30, 0, 300)

	cases := []struct {
		name  string
		input ProbeInput
		// params 为 nil 时用上面的默认参数。
		params ProbeParams
		want   bool
	}{
		{
			name: "到点且速率远低于慢线 ⇒ 判慢",
			// 50 token / 40s = 1.25 tok/s，慢线 71.9 ⇒ 慢。
			input: ProbeInput{TokensSoFar: 50, ElapsedSinceFirstByte: 40 * time.Second, Baseline: probeTestBaseline},
			want:  true,
		},
		{
			name: "边界：耗时恰等于 T ⇒ 判慢（判据是 >=）",
			// 50 token / 30s = 1.667 tok/s，同样远低于慢线；关键是 30s 这一刻必须已经算「到点」。
			input: ProbeInput{TokensSoFar: 50, ElapsedSinceFirstByte: 30 * time.Second, Baseline: probeTestBaseline},
			want:  true,
		},
		{
			name:  "边界：耗时比 T 少 1ms ⇒ 不判慢",
			input: ProbeInput{TokensSoFar: 50, ElapsedSinceFirstByte: 30*time.Second - time.Millisecond, Baseline: probeTestBaseline},
			want:  false,
		},
		{
			name:  "已生成 token 低于下限 ⇒ 不判慢（防把思考阶段误判成慢）",
			input: ProbeInput{TokensSoFar: 10, ElapsedSinceFirstByte: 40 * time.Second, Baseline: probeTestBaseline},
			want:  false,
		},
		{
			name:  "未到点 ⇒ 不判慢",
			input: ProbeInput{TokensSoFar: 50, ElapsedSinceFirstByte: 20 * time.Second, Baseline: probeTestBaseline},
			want:  false,
		},
		{
			name:   "T = 0 ⇒ 恒不判慢（机制关闭）",
			input:  ProbeInput{TokensSoFar: 50, ElapsedSinceFirstByte: 40 * time.Second, Baseline: probeTestBaseline},
			params: probeParams(0, 0, 300),
			want:   false,
		},
		{
			name:   "T < 0 ⇒ 恒不判慢",
			input:  ProbeInput{TokensSoFar: 50, ElapsedSinceFirstByte: 40 * time.Second, Baseline: probeTestBaseline},
			params: probeParams(-1, 0, 300),
			want:   false,
		},
		{
			name:  "无基线 ⇒ 不判慢（fail-open）",
			input: ProbeInput{TokensSoFar: 50, ElapsedSinceFirstByte: 40 * time.Second, Baseline: 0},
			want:  false,
		},
		{
			name:  "速率为负的基线 ⇒ 不判慢（fail-open）",
			input: ProbeInput{TokensSoFar: 50, ElapsedSinceFirstByte: 40 * time.Second, Baseline: -5},
			want:  false,
		},
		{
			name: "速率达标 ⇒ 不判慢",
			// 4000 token / 40s = 100 tok/s > 慢线 71.9。
			input: ProbeInput{TokensSoFar: 4000, ElapsedSinceFirstByte: 40 * time.Second, Baseline: probeTestBaseline},
			want:  false,
		},
		{
			name: "速率恰高于慢线 ⇒ 不判慢",
			// 慢线 71.90665 tok/s；4000/55.6s ≈ 71.94 略高于它。
			input: ProbeInput{TokensSoFar: 4000, ElapsedSinceFirstByte: 55600 * time.Millisecond, Baseline: probeTestBaseline},
			want:  false,
		},
		{
			name:  "耗时为零 ⇒ 不判慢（速率的除数非法，算不出就不判）",
			input: ProbeInput{TokensSoFar: 50, ElapsedSinceFirstByte: 0, Baseline: probeTestBaseline},
			want:  false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			effective := params
			if testCase.params != (ProbeParams{}) {
				effective = testCase.params
			}
			if got := IsSlowProbe(testCase.input, effective); got != testCase.want {
				t.Errorf("IsSlowProbe(%+v, %+v) = %v，期望 %v", testCase.input, effective, got, testCase.want)
			}
		})
	}
}

// TestProbeParamsNormalizeKeepsDisabledThreshold 钉住一处**刻意不收敛**。
//
// Params.normalize 会把所有零值收敛到默认值，但探测阈值 T 的零值有明确语义（关闭机制）。
// 若照搬那套「零值即取默认」，管理员把它配成 0 想关掉时会被静默改回 30——
// 「我关了它却还在换家」是最难查的一类问题。
func TestProbeParamsNormalizeKeepsDisabledThreshold(t *testing.T) {
	if got := (ProbeParams{}).normalize(); got.AfterFirstByteSeconds != 0 {
		t.Errorf("normalize 不得把 T=0 改成默认值（0 是「关闭」的明确语义）: 得到 %d", got.AfterFirstByteSeconds)
	}
	if got := (ProbeParams{AfterFirstByteSeconds: -7}).normalize(); got.AfterFirstByteSeconds != -7 {
		t.Errorf("normalize 不得改动负的 T（同样表示关闭）: 得到 %d", got.AfterFirstByteSeconds)
	}
}

// TestProbeMinTokensSharesTerminalFloor 钉住「最低 token 数复用终态口径」这条约束。
//
// 两个数字若各写一个，改了一处漏另一处就会出现「终态判不出慢、中途却判出慢」的分裂。
func TestProbeMinTokensSharesTerminalFloor(t *testing.T) {
	if DefaultProbeMinTokens != minOutputTokens {
		t.Errorf("DefaultProbeMinTokens = %d，必须复用 minOutputTokens = %d", DefaultProbeMinTokens, minOutputTokens)
	}
	if got := (ProbeParams{}).normalize().MinTokens; got != minOutputTokens {
		t.Errorf("MinTokens 留空时应落到 %d，得到 %d", minOutputTokens, got)
	}
}

// TestProbeThresholdDefaultIsThirtySeconds 钉住出厂阈值（用户 2026-09-21 指定）。
func TestProbeThresholdDefaultIsThirtySeconds(t *testing.T) {
	if DefaultProbeAfterFirstByteSeconds != 30 {
		t.Errorf("DefaultProbeAfterFirstByteSeconds = %d，期望 30", DefaultProbeAfterFirstByteSeconds)
	}
}
