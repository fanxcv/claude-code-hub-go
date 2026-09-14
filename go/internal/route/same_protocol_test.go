package route

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件是「同协议优先」的验收测试（用户需求：优先级一致时优先走不需要协议转换的供应商）。
//
// 口径声明（与实现逐条对应，见 sameprotocol.go 与 select.go 的 Options.SameProtocolWeightK）：
//   - 同协议判据 = convert.IsNativePair(客户端格式, 供应商类型)；
//   - 只在同一最高优先组内生效；
//   - 是偏好不是过滤：跨协议候选仍会被选中；
//   - 客户端格式未知时不判定；
//   - 概率（candidatesAtPriority[].probability）按**有效权重**计算，故界面能看出「为什么选中它」。

// sameProtocolFixture 造一个同优先组内「同协议 + 跨协议」各一家的候选集。
//
// 同协议：客户端 claude + 供应商 claude；跨协议：客户端 claude + 供应商 openai-compatible，
// 且**开启协议转换**（否则会被候选期过滤成 protocol_conversion_disabled，压根进不了样本）。
func sameProtocolFixture() (Provider, Provider) {
	same := baseProvider(1, convert.ProviderClaude)
	cross := baseProvider(2, convert.ProviderOpenAICompatible)
	cross.ProtocolConversionEnabled = boolPtr(true)
	return same, cross
}

// ratioOf 跑 n 次选路，返回「选中同协议那家」的比例与「两家都被选中过」的事实。
func ratioOf(t *testing.T, selector *Selector, req Request, n int) (float64, bool) {
	t.Helper()
	sameHits := 0
	crossHits := 0
	for i := 0; i < n; i++ {
		result, err := selector.Select(context.Background(), req)
		if err != nil {
			t.Fatalf("第 %d 次选路失败: %v", i, err)
		}
		if result.Provider == nil {
			t.Fatalf("第 %d 次无可用供应商：夹具应恒有候选", i)
		}
		switch result.Provider.ID {
		case 1:
			sameHits++
		case 2:
			crossHits++
		default:
			t.Fatalf("选中了夹具之外的供应商 %d", result.Provider.ID)
		}
	}
	return float64(sameHits) / float64(n), crossHits > 0
}

const sameProtocolSamples = 20000

// TestSameProtocolPreferenceRaisesSameProtocolOdds 是大样本概率断言（倍率 K=2 → 同协议 2/3）。
//
// 二项分布下 p=2/3、n=20000 的标准差约 0.0033，故 0.02 的容差约 6σ：
// 既不会随机翻绿，也能抓住「倍率没生效（会落到 0.5）」「倍率被误当过滤（会落到 1.0）」两类偏移。
func TestSameProtocolPreferenceRaisesSameProtocolOdds(t *testing.T) {
	same, cross := sameProtocolFixture()
	selector := NewSelector(Options{
		Source:              &stubSource{providers: []Provider{same, cross}},
		SameProtocolWeightK: 2,
		Rand:                rand.New(rand.NewSource(20260913)).Float64,
	})
	req := Request{Model: "m", Format: convert.FormatClaude}

	ratio, crossSeen := ratioOf(t, selector, req, sameProtocolSamples)
	const want = 2.0 / 3.0
	if math.Abs(ratio-want) > 0.02 {
		t.Fatalf("同协议占比 = %.4f，期望约 %.4f（K=2 时同协议权重 2、跨协议 1）", ratio, want)
	}
	if !crossSeen {
		t.Fatalf("跨协议候选在整个样本里一次都没被选中：偏好退化成了硬过滤")
	}
	t.Logf("K=2：同协议占比 %.4f（期望 %.4f，n=%d）", ratio, want, sameProtocolSamples)
}

// TestSameProtocolPreferenceOffKeepsUniformOdds 是反证的另一半：
// 关掉倍率（K=1 与不装配同效）后，占比必须回到等权 1/2。
//
// 这一条同时钉住「未启用时行为逐字节不变」的承诺：若实现把偏好做成了默认开启，
// 这里会红（占比会维持 2/3）。
func TestSameProtocolPreferenceOffKeepsUniformOdds(t *testing.T) {
	for _, k := range []int{0, 1} {
		same, cross := sameProtocolFixture()
		selector := NewSelector(Options{
			Source:              &stubSource{providers: []Provider{same, cross}},
			SameProtocolWeightK: k,
			Rand:                rand.New(rand.NewSource(20260913)).Float64,
		})
		req := Request{Model: "m", Format: convert.FormatClaude}

		ratio, crossSeen := ratioOf(t, selector, req, sameProtocolSamples)
		if math.Abs(ratio-0.5) > 0.02 {
			t.Fatalf("K=%d 时同协议占比 = %.4f，期望约 0.5（不启用偏好即等权）", k, ratio)
		}
		if !crossSeen {
			t.Fatalf("K=%d 时跨协议候选一次都没被选中", k)
		}
		t.Logf("K=%d：同协议占比 %.4f（期望 0.5）", k, ratio)
	}
}

// TestSameProtocolPreferenceKeepsPriorityFirst 钉住「偏好不跳优先级」：
// 高优先组全是跨协议、低优先组有同协议时，仍必须走高优先组。
func TestSameProtocolPreferenceKeepsPriorityFirst(t *testing.T) {
	highCross := baseProvider(1, convert.ProviderOpenAICompatible)
	highCross.ProtocolConversionEnabled = boolPtr(true)
	highCross.Priority = intPtr(1)

	lowSame := baseProvider(2, convert.ProviderClaude)
	lowSame.Priority = intPtr(9)

	selector := NewSelector(Options{
		Source:              &stubSource{providers: []Provider{highCross, lowSame}},
		SameProtocolWeightK: 10,
		Rand:                rand.New(rand.NewSource(7)).Float64,
	})
	for i := 0; i < 200; i++ {
		result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
		if err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		if result.Provider == nil || result.Provider.ID != 1 {
			t.Fatalf("第 %d 次选中 %+v，期望高优先组的跨协议供应商 1（偏好不得跨优先级抢）", i, result.Provider)
		}
		if result.Context.SelectedPriority != 1 {
			t.Fatalf("selectedPriority = %d，期望 1", result.Context.SelectedPriority)
		}
	}
}

// TestSameProtocolPreferenceDisabledWhenFormatUnknown 钉住「无法判定即不判定」：
// 客户端格式未知时不启用偏好（占比仍是 1/2），且候选上不出现同协议事实。
func TestSameProtocolPreferenceDisabledWhenFormatUnknown(t *testing.T) {
	same, cross := sameProtocolFixture()
	selector := NewSelector(Options{
		Source:              &stubSource{providers: []Provider{same, cross}},
		SameProtocolWeightK: 3,
		Rand:                rand.New(rand.NewSource(20260913)).Float64,
	})
	req := Request{Model: "m"} // Format 空：与 guard 的格式兼容过滤同一口径，不判定

	ratio, _ := ratioOf(t, selector, req, sameProtocolSamples)
	if math.Abs(ratio-0.5) > 0.02 {
		t.Fatalf("格式未知时同协议占比 = %.4f，期望约 0.5（不判定）", ratio)
	}

	result, err := selector.Select(context.Background(), req)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	for _, candidate := range result.Context.CandidatesAtPriority {
		if candidate.SameProtocol != nil || candidate.EffectiveWeight != nil {
			t.Fatalf("格式未知时不应输出同协议事实：%+v", candidate)
		}
	}
}

// TestSameProtocolPreferenceZeroWeightFallback 钉住权重全为 0 的兜底路径：
// 开启偏好时只在同协议候选内等概率取；关闭时仍是全体等概率。
func TestSameProtocolPreferenceZeroWeightFallback(t *testing.T) {
	same, cross := sameProtocolFixture()
	same.Weight = 0
	cross.Weight = 0

	selector := NewSelector(Options{
		Source:              &stubSource{providers: []Provider{same, cross}},
		SameProtocolWeightK: 2,
		Rand:                rand.New(rand.NewSource(20260913)).Float64,
	})
	req := Request{Model: "m", Format: convert.FormatClaude}

	ratio, crossSeen := ratioOf(t, selector, req, 500)
	if ratio != 1 {
		t.Fatalf("权重全为 0 且启用偏好时占比 = %.4f，期望 1（只在同协议候选内取）", ratio)
	}
	if crossSeen {
		t.Fatalf("权重全为 0 且启用偏好时跨协议不应被选中")
	}

	// 关闭偏好：回到全体等概率（两家都出现）。
	same2, cross2 := sameProtocolFixture()
	same2.Weight = 0
	cross2.Weight = 0
	offSelector := NewSelector(Options{
		Source:              &stubSource{providers: []Provider{same2, cross2}},
		SameProtocolWeightK: 1,
		Rand:                rand.New(rand.NewSource(20260913)).Float64,
	})
	offRatio, offCrossSeen := ratioOf(t, offSelector, req, 500)
	if math.Abs(offRatio-0.5) > 0.06 {
		t.Fatalf("关闭偏好时占比 = %.4f，期望约 0.5（全体等概率兜底）", offRatio)
	}
	if !offCrossSeen {
		t.Fatalf("关闭偏好时跨协议候选一次都没被选中")
	}
	t.Logf("权重全为 0：启用偏好占比 %.4f，关闭占比 %.4f", ratio, offRatio)
}

// TestSameProtocolPreferenceExcludesNobody 钉住「无可用供应商」判定不变：
// 全组都是跨协议时仍照常选出（不得因偏好而返回 nil）。
func TestSameProtocolPreferenceExcludesNobody(t *testing.T) {
	a := baseProvider(1, convert.ProviderOpenAICompatible)
	a.ProtocolConversionEnabled = boolPtr(true)
	b := baseProvider(2, convert.ProviderOpenAICompatible)
	b.ProtocolConversionEnabled = boolPtr(true)

	selector := NewSelector(Options{
		Source:              &stubSource{providers: []Provider{a, b}},
		SameProtocolWeightK: 5,
		Rand:                rand.New(rand.NewSource(11)).Float64,
	})
	seen := map[int64]bool{}
	for i := 0; i < 400; i++ {
		result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
		if err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		if result.Provider == nil {
			t.Fatalf("全组跨协议时不应返回无可用供应商")
		}
		seen[result.Provider.ID] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("全组跨协议时两家都应可能被选中，实际 %v", seen)
	}

	// 无候选时仍返回空（既有语义）。
	empty := NewSelector(Options{Source: &stubSource{}, SameProtocolWeightK: 5})
	result, err := empty.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("空候选应返回空结果而非错误: %v", err)
	}
	if result.Provider != nil {
		t.Fatalf("空候选时 Provider 应为 nil，实际 %+v", result.Provider)
	}
}

// TestSameProtocolPreferenceObservability 钉住可观测性：
// 启用时每个候选都带 sameProtocol 与 effectiveWeight，概率按有效权重算且合计为 1；
// 关闭时两个键都不出现（JSON 与加该字段之前逐字节一致）。
func TestSameProtocolPreferenceObservability(t *testing.T) {
	same, cross := sameProtocolFixture()
	same.Weight = 3
	cross.Weight = 1

	selector := NewSelector(Options{
		Source:              &stubSource{providers: []Provider{same, cross}},
		SameProtocolWeightK: 2,
		Rand:                rand.New(rand.NewSource(3)).Float64,
	})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	total := 0.0
	byID := map[int64]Candidate{}
	for _, candidate := range result.Context.CandidatesAtPriority {
		byID[candidate.ID] = candidate
		total += candidate.Probability
		if candidate.SameProtocol == nil || candidate.EffectiveWeight == nil {
			t.Fatalf("启用偏好时候选必须带同协议事实：%+v", candidate)
		}
	}
	if math.Abs(total-1) > 1e-9 {
		t.Fatalf("概率合计 = %v，期望 1", total)
	}
	// 同协议：weight 3 × K 2 = 6；跨协议：1。合计 7 → 6/7 与 1/7。
	if got := byID[1]; !*got.SameProtocol || *got.EffectiveWeight != 6 || math.Abs(got.Probability-6.0/7.0) > 1e-9 {
		t.Fatalf("同协议候选的可观测值不符：%+v", got)
	}
	if got := byID[2]; *got.SameProtocol || *got.EffectiveWeight != 1 || math.Abs(got.Probability-1.0/7.0) > 1e-9 {
		t.Fatalf("跨协议候选的可观测值不符：%+v", got)
	}

	// 关闭偏好：两个新键都不出现（对拍与黄金样本不受影响）。
	offSelector := NewSelector(Options{
		Source:              &stubSource{providers: []Provider{same, cross}},
		SameProtocolWeightK: 1,
		Rand:                rand.New(rand.NewSource(3)).Float64,
	})
	offResult, err := offSelector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	encoded, err := json.Marshal(offResult.Context.CandidatesAtPriority)
	if err != nil {
		t.Fatalf("序列化候选失败: %v", err)
	}
	for _, key := range []string{"sameProtocol", "effectiveWeight"} {
		if containsKey(encoded, key) {
			t.Fatalf("关闭偏好时不应输出键 %q：%s", key, encoded)
		}
	}
	if !containsKey(encoded, "probability") {
		t.Fatalf("既有键 probability 必须保留：%s", encoded)
	}
}

// TestSameProtocolJudgementReusesConvertPairing 钉住判据复用：
// 同协议判定必须与 convert 包的配对表逐例一致（自写近似判断会在这里分叉）。
func TestSameProtocolJudgementReusesConvertPairing(t *testing.T) {
	cases := []struct {
		clientFormat convert.ClientFormat
		providerType convert.ProviderType
		want         bool
	}{
		{convert.FormatClaude, convert.ProviderClaude, true},
		{convert.FormatClaude, convert.ProviderClaudeAuth, true},
		{convert.FormatClaude, convert.ProviderOpenAICompatible, false},
		{convert.FormatClaude, convert.ProviderCodex, false},
		{convert.FormatResponse, convert.ProviderCodex, true},
		{convert.FormatResponse, convert.ProviderClaude, false},
		{convert.FormatOpenAI, convert.ProviderOpenAICompatible, true},
		{convert.FormatOpenAI, convert.ProviderClaude, false},
	}
	for _, item := range cases {
		pref := newSameProtocolPreference(item.clientFormat, 2)
		provider := baseProvider(1, item.providerType)
		if got := pref.isSame(item.clientFormat, provider); got != item.want {
			t.Fatalf("isSame(%s, %s) = %v，期望 %v", item.clientFormat, item.providerType, got, item.want)
		}
		if want := convert.IsNativePair(item.clientFormat, item.providerType); want != item.want {
			t.Fatalf("夹具与 convert.IsNativePair 不一致：(%s, %s) = %v", item.clientFormat, item.providerType, want)
		}
	}
	// 未启用时恒为假（防「忘判 enabled 就拿到 true」）。
	if (sameProtocolPreference{}).isSame(convert.FormatClaude, baseProvider(1, convert.ProviderClaude)) {
		t.Fatalf("未启用偏好时 isSame 必须为假")
	}
}

// containsKey 报告 JSON 里是否出现该键（顶层对象数组的浅检查，够本文件用）。
func containsKey(encoded []byte, key string) bool {
	var items []map[string]any
	if err := json.Unmarshal(encoded, &items); err != nil {
		return false
	}
	for _, item := range items {
		if _, ok := item[key]; ok {
			return true
		}
	}
	return false
}
