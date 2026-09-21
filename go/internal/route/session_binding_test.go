package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「会话绑定是选路第一优先级，且不得绕过熔断」这条契约。
//
// 为什么必须单独钉：会话绑定走的是与前缀亲和平行的**另一条**短路路径
// （nominateBySessionBinding → validateAffinityCandidate）。它与 applyFilters 的
// healthRejection 是两处调用——若有人把绑定短路改成「命中直接用」，熔断就被绕过，
// 会话会被钉死在一家坏渠道上，而既有用例（只钉前缀亲和）照绿。

// sessionBindingRequest 构造一次带会话绑定的选路输入（Format 由调用方按需覆盖）。
func sessionBindingRequest(providerID int64) Request {
	return Request{
		Model:     "m",
		Format:    convert.FormatClaude,
		SessionID: "sess_test_1",
		SessionBinding: &SessionBindingSnapshot{
			SessionID:  "sess_test_1",
			KeyID:      42,
			Generation: "gen-1",
			ProviderID: providerID,
		},
	}
}

// TestSessionBindingWinsOverWeightedRandom 是主线：绑定命中即短路，且留痕为 session_reuse。
func TestSessionBindingWinsOverWeightedRandom(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	other := baseProvider(8, convert.ProviderClaude)

	selector := NewSelector(Options{
		Source: &stubSource{
			providers: []Provider{bound, other},
			byID:      map[int64]Provider{7: bound, 8: other},
		},
		// Rand 固定选第一个候选：若无绑定短路，加权随机会在两家间按脚本取。
		Rand: (&scriptedRand{values: []float64{0}}).next,
	})

	request := sessionBindingRequest(7)
	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != bound.ID {
		t.Fatalf("会话绑定应短路选中 id=7，实际 %+v", result.Provider)
	}
	if result.Method != MethodSessionReuse {
		t.Errorf("selectionMethod = %q，期望 %q", result.Method, MethodSessionReuse)
	}
}

// TestSessionBindingRejectsCircuitOpenProvider 反向判据：绑定的供应商熔断开闸时必须被跳过。
//
// 这是设计稿 §8 风险二的缓解：熔断是暂时的，绑定保留待恢复，但本次绝不钉死在它上面。
func TestSessionBindingRejectsCircuitOpenProvider(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	bound := baseProvider(7, convert.ProviderClaude)
	healthy := baseProvider(8, convert.ProviderClaude)

	client := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(bound.ID): {
			"circuitState":     string(StateOpen),
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
	}}
	selector := NewSelector(Options{
		Source: &stubSource{
			providers: []Provider{bound, healthy},
			byID:      map[int64]Provider{7: bound, 8: healthy},
		},
		Health: newTestHealth(t, client, true),
		Now:    func() time.Time { return now },
		Rand:   (&scriptedRand{values: []float64{0}}).next,
	})

	request := sessionBindingRequest(bound.ID)
	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != healthy.ID {
		t.Fatalf("熔断开闸的绑定必须被跳过并回落，实际 %+v", result.Provider)
	}
	if result.Method == MethodSessionReuse {
		t.Error("被熔断拒绝后不应仍记为 session_reuse")
	}
}

// TestSessionBindingEmptyBindingFallsThrough 空绑定必须回落，不得短路。
//
// 新会话（ProviderID == 0）仍从最小的 effectivePriority 档开始选（设计稿裁决 D）。
func TestSessionBindingEmptyBindingFallsThrough(t *testing.T) {
	first := baseProvider(7, convert.ProviderClaude)
	second := baseProvider(8, convert.ProviderClaude)

	selector := NewSelector(Options{
		Source: &stubSource{
			providers: []Provider{first, second},
			byID:      map[int64]Provider{7: first, 8: second},
		},
		Rand: (&scriptedRand{values: []float64{0}}).next,
	})

	request := sessionBindingRequest(0)
	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method == MethodSessionReuse {
		t.Error("空绑定不得短路为 session_reuse")
	}
	if result.Provider == nil {
		t.Fatal("空绑定应正常走加权随机选出供应商")
	}
}

// TestSessionIDSuppressesPrefixAffinity 有 session id 的请求即便无绑定也不查前缀。
//
// 否则「新会话」会被别的会话写下的前缀记录牵走，那就不是会话粘性了。
func TestSessionIDSuppressesPrefixAffinity(t *testing.T) {
	prefixed := baseProvider(7, convert.ProviderClaude)
	other := baseProvider(8, convert.ProviderClaude)

	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	chain, ok := Fingerprint(body, convert.FormatClaude, 8)
	if !ok {
		t.Fatal("夹具正文应能指纹化")
	}
	lookup := &AffinityLookup{
		Hint:       &AffinityHint{ProviderID: prefixed.ID, MatchedFP: chain.Tip().FP, MatchedIndex: 0},
		IdentityFP: "idfp",
		Generation: "v3:gen",
	}

	selector := NewSelector(Options{
		Source: &stubSource{
			providers: []Provider{prefixed, other},
			byID:      map[int64]Provider{7: prefixed, 8: other},
		},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})

	request := Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42,
		AffinityBody: body, AffinityLookup: lookup,
		SessionID: "sess_test_1",
		SessionBinding: &SessionBindingSnapshot{
			SessionID: "sess_test_1", KeyID: 42, Generation: "gen-1", ProviderID: 0,
		},
	}
	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method == MethodPrefixAffinity {
		t.Error("带 session id 的请求不得走前缀亲和（前缀只服务无会话 id 的客户端）")
	}
	if result.AffinityLookup != nil {
		t.Error("跳过前缀层后不应带回 lookup")
	}
}
