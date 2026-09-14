package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「亲和提名不得绕过熔断」这条契约——用户实报的那类现象的**头号嫌疑点**。
//
// 为什么必须单独钉：亲和走的是**另一条**候选产生路径（nominateByAffinity → validateAffinityCandidate），
// 与 applyFilters 的 healthRejection 是两处调用。affinity.go 的注释写着「命中后仍须过全套硬校验」，
// 但在本文件之前**没有任何用例检验熔断这一维**（既有用例只钉了「禁用」与「排除列表」）——
// 一旦有人在优化亲和时改成「命中直接用」，熔断就被绕过，而所有既有用例照绿。
//
// 判据取自生产链路形状：命中（AffinityLookup 由调用方注入，不重复访问 Redis）→ 目标供应商熔断开闸
// → 提名必须被拒并静默回落加权随机。反向也要成立：窗口已过期的 open 属半开试探，**必须放行**。

// affinityNominationRequest 构造一次「亲和命中 id=7」的选路输入。
func affinityNominationRequest(t *testing.T) (Request, *AffinityLookup) {
	t.Helper()
	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	chain, ok := Fingerprint(body, convert.FormatClaude, 8)
	if !ok {
		t.Fatal("夹具正文应能指纹化")
	}
	lookup := &AffinityLookup{
		Hint:       &AffinityHint{ProviderID: 7, MatchedFP: chain.Tip().FP, MatchedIndex: 0},
		IdentityFP: "idfp",
		Generation: "v3:gen",
	}
	return Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42,
		AffinityBody: body, AffinityLookup: lookup,
	}, lookup
}

// TestNominateByAffinityRejectsCircuitOpenProvider 是主线：窗口内 open 的亲和目标必须被拒。
func TestNominateByAffinityRejectsCircuitOpenProvider(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	nominated := baseProvider(7, convert.ProviderClaude)
	healthy := baseProvider(8, convert.ProviderClaude)

	client := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(nominated.ID): {
			"circuitState":     string(StateOpen),
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
	}}
	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{nominated, healthy}, byID: map[int64]Provider{7: nominated, 8: healthy}},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Health:   newTestHealth(t, client, true),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})

	request, lookup := affinityNominationRequest(t)
	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != healthy.ID {
		t.Fatalf("熔断开闸的亲和目标必须被拒并回落加权随机，实际选中 %+v", result.Provider)
	}
	if result.Method != MethodWeightedRandom {
		t.Errorf("回落后的 selectionMethod = %q，期望 %q", result.Method, MethodWeightedRandom)
	}
	if result.Affinity != nil {
		t.Errorf("被熔断拒绝的候选不应产生提名: %+v", result.Affinity)
	}
	if result.AffinityLookup != lookup {
		t.Error("被拒绝的提名仍应带回 lookup（终态写回需要它的 generation）")
	}
	if got := filteredReasonOrEmpty(result.Context, nominated.ID); got != ReasonCircuitOpen {
		t.Errorf("被熔断那家的过滤理由应为 %q，实际 %q", ReasonCircuitOpen, got)
	}
}

// TestNominateByAffinityAllowsExpiredCircuitWindow 是反向：窗口已过期属半开试探，必须放行。
//
// 这条与主线同等重要：把过期判定写成「只要 raw=open 就拒」会让熔断**永远无法恢复**
// （半开试探是唯一出口），而那是比「多打一次」严重得多的缺陷。
func TestNominateByAffinityAllowsExpiredCircuitWindow(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	nominated := baseProvider(7, convert.ProviderClaude)
	healthy := baseProvider(8, convert.ProviderClaude)

	client := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(nominated.ID): {
			"circuitState":     string(StateOpen),
			"circuitOpenUntil": strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
		},
	}}
	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{nominated, healthy}, byID: map[int64]Provider{7: nominated, 8: healthy}},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Health:   newTestHealth(t, client, true),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})

	request, _ := affinityNominationRequest(t)
	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != nominated.ID {
		t.Fatalf("窗口已过期的熔断目标应放行试探，实际选中 %+v", result.Provider)
	}
	if result.Method != MethodPrefixAffinity {
		t.Errorf("selectionMethod = %q，期望 %q", result.Method, MethodPrefixAffinity)
	}
	if result.Affinity == nil {
		t.Error("放行时应产生提名（终态墓碑只对提名者写）")
	}
}
