package route

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「亲和指纹可以由调用方在正文树释放之前算一次」这个契约。
//
// 为什么需要它：正文树是每请求一份与正文同量级的常驻（实测 1.00× 正文字节），dataplane 在转发阶段会把它释放；
// 而指纹按旧口径是每次 Select 都从整棵树重算。预计算槽位一旦失效（被忽略或算错窗口），
// 表现是「树释放后亲和不参与」——静默退化成加权随机，没有任何错误。
//
// 两条断言缺一不可：
//  1. 预计算链与现算链**逐字节一致**（同一窗口、同一算法，JSON 序列化后可直接比字节）；
//  2. 没有正文、只有预计算链时，选路与 F3b 供数仍走与有正文时完全相同的分支。

// affinityPrecomputeBody 是可指纹化的 claude 正文：系统段 + 五条消息，
// 于是 tip 深度为 5、且链条数大于窗口 3（窗口截断因此可被观测）。
func affinityPrecomputeBody(t *testing.T) map[string]any {
	t.Helper()
	return claudeBody(t, `{
		"model": "claude-sonnet-4-5",
		"system": "you are helpful",
		"messages": [
			{"role": "user", "content": "m1"},
			{"role": "assistant", "content": "m2"},
			{"role": "user", "content": "m3"},
			{"role": "assistant", "content": "m4"},
			{"role": "user", "content": "m5"}
		]
	}`)
}

func mustChainJSON(t *testing.T, chain *FingerprintChain) string {
	t.Helper()
	encoded, err := json.Marshal(chain)
	if err != nil {
		t.Fatalf("指纹链序列化失败: %v", err)
	}
	return string(encoded)
}

// TestAffinityFingerprintPrecomputedMatchesRecompute 钉住「同一输入得同一指纹」。
func TestAffinityFingerprintPrecomputedMatchesRecompute(t *testing.T) {
	body := affinityPrecomputeBody(t)
	// 窗口取非默认值：窗口只影响链尾截断，若预计算漏用选器窗口，这条就会红。
	affinity := NewAffinityStore(AffinityOptions{Window: 3})
	selector := NewSelector(Options{Affinity: affinity})

	precomputed, ok := selector.FingerprintOf(body, convert.FormatClaude)
	if !ok || precomputed == nil {
		t.Fatal("亲和已装配时 FingerprintOf 必须给出指纹链")
	}
	recomputed, ok := Fingerprint(body, convert.FormatClaude, 3)
	if !ok {
		t.Fatal("现算指纹失败")
	}
	if got, want := mustChainJSON(t, precomputed), mustChainJSON(t, recomputed); got != want {
		t.Fatalf("预计算链与现算链不一致：\n got=%s\nwant=%s", got, want)
	}
	if precomputed.Tip().Depth != 5 {
		t.Fatalf("指纹链深度应为 5，实际 %d", precomputed.Tip().Depth)
	}
	if len(precomputed.Tail) != 3 {
		t.Fatalf("窗口 3 应把链尾截到 3 个边界，实际 %d", len(precomputed.Tail))
	}

	// 亲和未装配时不给链：窗口就配在亲和存储上，未装配时没有指纹口径。
	if chain, ok := NewSelector(Options{}).FingerprintOf(body, convert.FormatClaude); ok || chain != nil {
		t.Fatal("亲和未装配时不应给出指纹链")
	}
	// 正文为空时不给链（与 Fingerprint 的 nil 语义一致）。
	if chain, ok := selector.FingerprintOf(nil, convert.FormatClaude); ok || chain != nil {
		t.Fatal("空正文不应给出指纹链")
	}
}

// TestAffinityFingerprintPrecomputedSelectsWithoutBody 钉住「树没了仍能按亲和选路」：
// 只有预计算链、AffinityBody 为 nil 时的选路结果必须与带正文时逐字段一致。
func TestAffinityFingerprintPrecomputedSelectsWithoutBody(t *testing.T) {
	target := baseProvider(7, convert.ProviderClaude)
	other := baseProvider(8, convert.ProviderClaude)
	body := affinityPrecomputeBody(t)
	affinity := NewAffinityStore(AffinityOptions{Window: 8})
	source := &stubSource{
		providers: []Provider{target, other},
		byID:      map[int64]Provider{7: target, 8: other},
	}
	selector := NewSelector(Options{
		Source:   source,
		Affinity: affinity,
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})
	chain, ok := selector.FingerprintOf(body, convert.FormatClaude)
	if !ok {
		t.Fatal("构造预计算链失败")
	}
	lookup := &AffinityLookup{
		Hint:       &AffinityHint{ProviderID: 7, MatchedFP: chain.Tip().FP, MatchedIndex: len(chain.Tail) - 1},
		IdentityFP: "idfp",
		Generation: "v3:gen",
	}
	withBody, err := selector.Select(context.Background(), Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42,
		AffinityBody: body, AffinityLookup: lookup,
	})
	if err != nil {
		t.Fatalf("带正文选路失败: %v", err)
	}
	// 正文树已释放：只有预算好的链，没有树。
	withoutBody, err := selector.Select(context.Background(), Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42,
		AffinityFingerprint: chain, AffinityLookup: lookup,
	})
	if err != nil {
		t.Fatalf("仅预计算链选路失败: %v", err)
	}
	if withoutBody.Provider == nil || withoutBody.Provider.ID != 7 {
		t.Fatalf("预计算链应提名供应商 7，实际 %+v", withoutBody.Provider)
	}
	if withoutBody.Method != MethodPrefixAffinity || withoutBody.Reason != ReasonSelectedAffinity {
		t.Fatalf("方法与原因应与带正文时相同，实际 %s / %s", withoutBody.Method, withoutBody.Reason)
	}
	if withBody.Method != withoutBody.Method || withBody.Reason != withoutBody.Reason {
		t.Fatalf("两条路径的选路方式不一致: %s/%s vs %s/%s",
			withBody.Method, withBody.Reason, withoutBody.Method, withoutBody.Reason)
	}
	if withoutBody.Affinity == nil {
		t.Fatal("预计算链必须产出亲和提名事实（写回与落链都靠它）")
	}
	if withoutBody.Affinity.MatchedDepth != withBody.Affinity.MatchedDepth {
		t.Errorf("命中深度不一致: 无正文=%d 带正文=%d",
			withoutBody.Affinity.MatchedDepth, withBody.Affinity.MatchedDepth)
	}
	for name, pair := range map[string][2]string{
		"scopeTag":   {withoutBody.Affinity.ScopeTag, withBody.Affinity.ScopeTag},
		"tipFP":      {withoutBody.Affinity.TipFP, withBody.Affinity.TipFP},
		"identityFP": {withoutBody.Affinity.IdentityFP, withBody.Affinity.IdentityFP},
		"generation": {withoutBody.Affinity.Generation, withBody.Affinity.Generation},
		"matchedFP":  {withoutBody.Affinity.Hint.MatchedFP, withBody.Affinity.Hint.MatchedFP},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s 不一致: 无正文=%q 带正文=%q", name, pair[0], pair[1])
		}
	}
}

// TestAffinityFingerprintPrecomputedFeedsSessionBindingFacts 钉住 F3b 供数走同一条预算链。
func TestAffinityFingerprintPrecomputedFeedsSessionBindingFacts(t *testing.T) {
	body := affinityPrecomputeBody(t)
	selector := NewSelector(Options{Affinity: NewAffinityStore(AffinityOptions{Window: 8})})
	chain, ok := selector.FingerprintOf(body, convert.FormatClaude)
	if !ok {
		t.Fatal("构造预计算链失败")
	}
	facts := selector.f3bCacheScoreFacts(Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42,
		AffinityFingerprint: chain,
	})
	if facts == nil {
		t.Fatal("预计算链应能供 F3b 事实（会话粘性路径的缓存模拟列靠它）")
	}
	if facts.TipFP != chain.Tip().FP || facts.TipDepth != chain.Tip().Depth {
		t.Fatalf("F3b 事实应取自预计算链: %+v", facts)
	}
	if facts.ScopeTag != ScopeTag(42, convert.FormatClaude, "m") {
		t.Fatalf("F3b 事实的 scope tag 不对: %q", facts.ScopeTag)
	}
	// 对照：既没有链也没有正文时不应供数（否则会写出一份空指纹）。
	if facts := selector.f3bCacheScoreFacts(Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42,
	}); facts != nil {
		t.Fatalf("没有指纹来源时不应供数: %+v", facts)
	}
}
