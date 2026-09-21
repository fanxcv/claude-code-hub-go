package route

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住「前缀亲和层提名未被采用」必须分流临时/结构性这条契约。
//
// 为什么必须单独钉：前缀层与会话层一样有 winner 写回——AffinityWriteback.RecordWinner 写 tip
// 绑定，而 terminal 成功侧对非 nil 的 writeback 无条件调用它（settle.go 的 affinityWinner）。
// 若不分流，提名者因**瞬时**原因（读该行失败、熔断、会话冷却）未被采用时，备用胜出就会把
// tip 绑定改写成备用，设计稿 §4「跳过该 provider…不清空绑定；待熔断恢复后会话仍粘回去」即为假。
//
// 分层：本文件钉「分类对不对」（纯内存）；下一条用例（真 Redis）钉「分类对了之后 RecordWinner
// 真的没写」；跨包那条（package route_test）钉完整链（含终态）。

// newPrefixBypassSelector 造一个「亲和命中 7、8 为备用」的选路器。
//
// 备用排首位：加权随机的脚本取首家，便于断言「回落到备用」而非恰好抽中绑定家。
func newPrefixBypassSelector(source Source, health *HealthReader) *Selector {
	return NewSelector(Options{
		Source:   source,
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Health:   health,
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})
}

// TestPrefixAffinityBypassClassifiesRejection 是分类主线：四种「既有绑定未被采用」的形态
// 必须给出正确的临时/结构性判定。
//
// 没有「行已不存在」「渠道停用」这两条反向，把任何失败都判 transient 也能让主线变绿，
// 而后果是会话永远钉在一家已删除/已停用的渠道上（tip 绑定再也改不掉）。
func TestPrefixAffinityBypassClassifiesRejection(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	circuitRedis := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(7): {
			"circuitState":     string(StateOpen),
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
	}}
	disabled := baseProvider(7, convert.ProviderClaude)
	disabled.IsEnabled = false

	cases := []struct {
		name      string
		source    Source
		health    *HealthReader
		wantKeeps bool
	}{
		{
			name: "读该行失败（瞬时）⇒ 保绑定",
			source: &lookupErrorSource{
				providers: []Provider{baseProvider(8, convert.ProviderClaude), baseProvider(7, convert.ProviderClaude)},
				byID:      map[int64]Provider{7: baseProvider(7, convert.ProviderClaude), 8: baseProvider(8, convert.ProviderClaude)},
				err:       errors.New("lookup: 连接池暂时不可用"),
			},
			wantKeeps: true,
		},
		{
			name: "行已不存在（ErrProviderNotFound，结构性）⇒ 允许改写",
			source: &lookupErrorSource{
				providers: []Provider{baseProvider(8, convert.ProviderClaude), baseProvider(7, convert.ProviderClaude)},
				byID:      map[int64]Provider{7: baseProvider(7, convert.ProviderClaude), 8: baseProvider(8, convert.ProviderClaude)},
				err:       providerLookupError(7, store.ErrNotFound),
			},
			wantKeeps: false,
		},
		{
			// 包装两层哨兵：真实读取面若把 store.ErrNotFound 再包一层（驱动/连接池包装），
			// errors.Is 必须仍认得出它——判据用的是 errors.Is 而非 ==，这条就是钉这一点。
			name: "行已不存在（包装两层哨兵）⇒ 允许改写",
			source: &lookupErrorSource{
				providers: []Provider{baseProvider(8, convert.ProviderClaude), baseProvider(7, convert.ProviderClaude)},
				byID:      map[int64]Provider{7: baseProvider(7, convert.ProviderClaude), 8: baseProvider(8, convert.ProviderClaude)},
				err:       fmt.Errorf("pgx: %w", providerLookupError(7, store.ErrNotFound)),
			},
			wantKeeps: false,
		},
		{
			name: "熔断开闸（临时）⇒ 保绑定",
			source: &stubSource{
				providers: []Provider{baseProvider(8, convert.ProviderClaude), baseProvider(7, convert.ProviderClaude)},
				byID:      map[int64]Provider{7: baseProvider(7, convert.ProviderClaude), 8: baseProvider(8, convert.ProviderClaude)},
			},
			health:    newTestHealth(t, circuitRedis, true),
			wantKeeps: true,
		},
		{
			name: "渠道停用（结构性）⇒ 允许改写",
			source: &stubSource{
				providers: []Provider{baseProvider(8, convert.ProviderClaude), disabled},
				byID:      map[int64]Provider{7: disabled, 8: baseProvider(8, convert.ProviderClaude)},
			},
			wantKeeps: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selector := newPrefixBypassSelector(tc.source, tc.health)
			request, _ := affinityNominationRequest(t)
			result, err := selector.Select(context.Background(), request)
			if err != nil {
				t.Fatalf("选路失败: %v", err)
			}
			if result.Provider == nil || result.Provider.ID != 8 {
				t.Fatalf("提名被拒时应回落选备用 8，实际 %+v", result.Provider)
			}
			if result.Method != MethodWeightedRandom {
				t.Errorf("回落后的 selectionMethod = %q，期望 %q", result.Method, MethodWeightedRandom)
			}
			if result.AffinityWriteback == nil {
				t.Fatal("被拒的提名仍应带回写回事实（终态需要它的 identity 与 generation）")
			}
			if got := result.AffinityWriteback.Bypass.KeepsBinding(); got != tc.wantKeeps {
				t.Errorf("KeepsBinding = %v（Bypass=%v），期望 %v", got, result.AffinityWriteback.Bypass, tc.wantKeeps)
			}
		})
	}
}

// TestPrefixAffinityBypassSuppressesWinnerWriteback 钉住**效果**：分类为临时时 RecordWinner
// 必须真的不写（tip 绑定保持原值），结构性时允许改写。两条成对，构成双向分辨。
//
// 为何要真 Redis：nil Redis 的 store 里 Put 本来就返回 false，「返回 false」于是无法归因——
// 只有「本该写得进去却没写」才证明是 Bypass 这道闸拦下的。
func TestPrefixAffinityBypassSuppressesWinnerWriteback(t *testing.T) {
	cases := []struct {
		name        string
		sourceErr   error
		wantWritten bool
		wantWinner  int64
	}{
		{
			name:        "读该行失败（瞬时）⇒ 不得改写，原绑定仍指 7",
			sourceErr:   errors.New("lookup: 连接池暂时不可用"),
			wantWritten: false,
			wantWinner:  7,
		},
		{
			name:        "行已不存在（结构性）⇒ 允许改写为 8",
			sourceErr:   providerLookupError(7, store.ErrNotFound),
			wantWritten: true,
			wantWinner:  8,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := integrationRedis(t)
			ctx := context.Background()
			affinity := NewAffinityStore(AffinityOptions{
				Redis:             client,
				Window:            8,
				SlidingTTLSeconds: 120,
				GenerationToken:   counterToken(),
			})

			body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
			chain, ok := Fingerprint(body, convert.FormatClaude, 8)
			if !ok {
				t.Fatal("夹具正文应能指纹化")
			}
			tip := chain.Tip().FP

			// scope 由 KeyID 派生，故取一个唯一 KeyID 并据此枚举本用例可能写下的键。
			keyID := time.Now().UnixNano()
			scope := ScopeTag(keyID, convert.FormatClaude, "m")
			t.Cleanup(func() {
				keys, _, err := client.Scan(ctx, 0, affinityKeyPrefix+"{"+scope+":*", 64).Result()
				if err == nil && len(keys) > 0 {
					_ = client.Del(ctx, keys...).Err()
				}
			})

			// 先 Lookup 一次：未命中路径会 ensure generation，写回的 CAS 需要它。
			lookup, ok := affinity.Lookup(ctx, scope, chain.DeepestFirst())
			if !ok || lookup.Hint != nil {
				t.Fatalf("空库查找应可用且不命中: ok=%v hint=%+v", ok, lookup.Hint)
			}
			// 预置原绑定：tip 指向 7。本用例要的正是「本可命中 7」。
			if !affinity.Put(ctx, scope, tip, 7, lookup.IdentityFP, lookup.Generation) {
				t.Fatal("预置绑定应写入")
			}
			bindingKey := affinity.bindingKey(scope, tip)
			if value := affinityValueAt(t, client, bindingKey); !strings.HasPrefix(value, "1|7|") {
				t.Fatalf("预置绑定值 = %q，期望以 1|7| 开头", value)
			}

			selector := NewSelector(Options{
				Source: &lookupErrorSource{
					providers: []Provider{baseProvider(8, convert.ProviderClaude), baseProvider(7, convert.ProviderClaude)},
					byID:      map[int64]Provider{7: baseProvider(7, convert.ProviderClaude), 8: baseProvider(8, convert.ProviderClaude)},
					err:       tc.sourceErr,
				},
				Affinity: affinity,
				Rand:     (&scriptedRand{values: []float64{0}}).next,
			})
			request := Request{
				Model: "m", Format: convert.FormatClaude, KeyID: keyID,
				AffinityBody: body,
				AffinityLookup: &AffinityLookup{
					Hint:       &AffinityHint{ProviderID: 7, MatchedFP: tip, MatchedIndex: 0},
					IdentityFP: lookup.IdentityFP,
					Generation: lookup.Generation,
				},
			}
			result, err := selector.Select(ctx, request)
			if err != nil {
				t.Fatalf("选路失败: %v", err)
			}
			if result.Provider == nil || result.Provider.ID != 8 {
				t.Fatalf("提名被拒时应回落选备用 8，实际 %+v", result.Provider)
			}
			if result.AffinityWriteback == nil {
				t.Fatal("被拒的提名仍应带回写回事实")
			}

			// 终态：备用 8 成功胜出。
			written := result.AffinityWriteback.RecordWinner(ctx, 8)
			if written != tc.wantWritten {
				t.Errorf("RecordWinner 返回 %v，期望 %v（Bypass=%v）",
					written, tc.wantWritten, result.AffinityWriteback.Bypass)
			}
			wantPrefix := "1|" + strconv.FormatInt(tc.wantWinner, 10) + "|"
			if value := affinityValueAt(t, client, bindingKey); !strings.HasPrefix(value, wantPrefix) {
				t.Errorf("tip 绑定值 = %q，期望以 %q 开头（%s）", value, wantPrefix, tc.name)
			}
		})
	}
}
