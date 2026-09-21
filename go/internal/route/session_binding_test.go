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
		// 总闸开（Affinity 非 nil）：会话绑定是亲和的一层，总闸关时整层不参与（见 resolve）。
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
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

// TestSessionBindingSkippedWhenAffinityGateOff 总闸关（Affinity 未装配）时会话绑定整层不参与。
//
// 为什么必须钉：会话绑定短路只看 req.SessionBinding，不查总闸；若 resolve 不门控，
// 会出现「/readyz 报 disabled，会话粘性却在跑」的矛盾态。
func TestSessionBindingSkippedWhenAffinityGateOff(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	other := baseProvider(8, convert.ProviderClaude)

	selector := NewSelector(Options{
		Source: &stubSource{
			providers: []Provider{bound, other},
			byID:      map[int64]Provider{7: bound, 8: other},
		},
		// Affinity 为 nil = 总闸关。
		Rand: (&scriptedRand{values: []float64{0}}).next,
	})

	request := sessionBindingRequest(bound.ID)
	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method == MethodSessionReuse {
		t.Error("总闸关时不得走会话绑定短路")
	}
	if result.AffinityLookup != nil {
		t.Error("总闸关时不得带回亲和 lookup")
	}
}

// TestSessionBindingSkippedWhenForcePrefix 模式开关为真（强制前缀）时会话绑定整层跳过。
//
// 这是设计 § 5 说的 kill switch：置真必须真的退回前缀行为，
// 否则「强制前缀粘性」只是文案，会话依旧被绑定短路钉住。
func TestSessionBindingSkippedWhenForcePrefix(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	other := baseProvider(8, convert.ProviderClaude)

	selector := NewSelector(Options{
		Source: &stubSource{
			providers: []Provider{bound, other},
			byID:      map[int64]Provider{7: bound, 8: other},
		},
		Affinity:                      NewAffinityStore(AffinityOptions{Window: 8}),
		AffinityIgnoreClientSessionID: true,
		Rand:                          (&scriptedRand{values: []float64{0}}).next,
	})

	request := sessionBindingRequest(bound.ID)
	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method == MethodSessionReuse {
		t.Error("强制前缀模式下不得走会话绑定短路（kill switch 未生效）")
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
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Health:   newTestHealth(t, client, true),
		Now:      func() time.Time { return now },
		Rand:     (&scriptedRand{values: []float64{0}}).next,
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
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
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

// TestSessionBindingStillSuppliesF3bFacts 会话粘性短路时仍交出 F3b 事实（设计稿裁决 C）。
//
// 为何必须钉：F3b 五列靠指纹链**纯计算**供数，而指纹链原先只在前缀提名里算。
// 会话绑定短路返回时若不带事实，五列全空 ⇒ 缓存效果报表失去会话粘性下的全部样本，
// 而这条退化没有任何告警（与「写回静默失效」同型）。
// 反证面：同一短路下 AffinityWriteback 的 store 必须为 nil——本路径**不得**写任何亲和键。
func TestSessionBindingStillSuppliesF3bFacts(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	other := baseProvider(8, convert.ProviderClaude)
	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)

	selector := NewSelector(Options{
		Source: &stubSource{
			providers: []Provider{bound, other},
			byID:      map[int64]Provider{7: bound, 8: other},
		},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})

	request := sessionBindingRequest(bound.ID)
	request.KeyID = 42
	request.AffinityBody = body
	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method != MethodSessionReuse {
		t.Fatalf("前置条件不成立：本次应走会话绑定短路，实际 %q", result.Method)
	}
	if result.AffinityWriteback == nil {
		t.Fatal("会话粘性短路必须交出 F3b 事实（否则五列全空且无告警）")
	}
	scopeTag, matchedFP, tipFP, tipPrefixBytes, hasTip := result.AffinityWriteback.CacheScoreFacts()
	if scopeTag == "" {
		t.Error("F3b 事实的 ScopeTag 不得为空")
	}
	if !hasTip || tipFP == "" {
		t.Errorf("F3b 事实应有 tip 指纹（否则 theoretical_cache_tokens 会写 NULL），实得 hasTip=%v tipFP=%q", hasTip, tipFP)
	}
	if tipPrefixBytes <= 0 {
		t.Errorf("F3b 事实应有正的 tip 前缀字节数，实得 %d", tipPrefixBytes)
	}
	// 会话粘性不看前缀，故没有「命中的指纹」；留空让 cachescore 侧回落 tip。
	if matchedFP != "" {
		t.Errorf("会话粘性下不应有命中的指纹，实得 %q", matchedFP)
	}
}
