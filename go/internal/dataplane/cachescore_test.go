package dataplane

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// stubSettings 是最小的设置源：只提供门控要读的那一个可空字段。
type stubSettings struct {
	value *bool
	err   error
}

func (s stubSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &store.SystemSettings{CacheEffectivenessEnabled: s.value}, nil
}

// stubAffinityWriteback 同时实现 pctx 的中性句柄与数据面认得的「F3b 事实」本地接口。
type stubAffinityWriteback struct {
	scopeTag       string
	matchedFP      string
	tipFP          string
	tipPrefixBytes int
	hasTip         bool
}

func (stubAffinityWriteback) RecordWinner(context.Context, int64) bool       { return false }
func (stubAffinityWriteback) TombstoneOnFailure(context.Context, int64) bool { return false }

func (s stubAffinityWriteback) CacheScoreFacts() (string, string, string, int, bool) {
	return s.scopeTag, s.matchedFP, s.tipFP, s.tipPrefixBytes, s.hasTip
}

func boolPtr(v bool) *bool { return &v }

// TestApplyCacheScoreFieldsGateOffWritesNothing 是**反证**：开关显式关闭时五列一个都不写。
// 没有这一条，「写了」的证据可以来自「反正都会写」，而不是来自门控真的生效。
func TestApplyCacheScoreFieldsGateOffWritesNothing(t *testing.T) {
	gate := newCacheScoreGate(stubSettings{value: boolPtr(false)}, true)
	settler := &storeSettler{cacheScore: gate, logger: logx.New(nil)}
	pc := newSettlerContext(t)
	setAffinity(t, pc, stubAffinityWriteback{scopeTag: "s", tipFP: "f", tipPrefixBytes: 400, hasTip: true})

	settlement := terminal.Settlement{Usage: terminal.Usage{InputTokens: cacheScoreInt64Ptr(2)}}
	settler.applyCacheScoreFields(context.Background(), &settlement, pc, completedStreamOutcome())

	if settlement.CacheCompatibilityKey != nil || settlement.CacheScoreEligible != nil ||
		settlement.CacheScoreExcludedReason != nil || settlement.TheoreticalCacheTokens != nil ||
		settlement.CacheTTLBucket != nil {
		t.Fatal("开关关闭时 F3b 五列必须一个都不写（否则等于在关闭该功能的部署上凭空造账务数据）")
	}
}

// TestApplyCacheScoreFieldsGateOnWritesDerivedValues 断言门控开启时的全套派生：
// 兼容键、理论缓存量（prefixBytes/4 取整）、TTL 桶、合格标记。
func TestApplyCacheScoreFieldsGateOnWritesDerivedValues(t *testing.T) {
	gate := newCacheScoreGate(stubSettings{value: boolPtr(true)}, false)
	settler := &storeSettler{cacheScore: gate, logger: logx.New(nil)}
	pc := newSettlerContext(t)
	setAffinity(t, pc, stubAffinityWriteback{
		scopeTag: "scope-1", matchedFP: "matched-1", tipFP: "tip-1", tipPrefixBytes: 401, hasTip: true,
	})

	settlement := terminal.Settlement{Usage: terminal.Usage{InputTokens: cacheScoreInt64Ptr(2)}}
	settler.applyCacheScoreFields(context.Background(), &settlement, pc, completedStreamOutcome())

	if settlement.CacheCompatibilityKey == nil || *settlement.CacheCompatibilityKey != "scope-1:matched-1" {
		t.Fatalf("cache_compatibility_key = %v, want scope-1:matched-1（命中指纹优先于 tip）",
			settlement.CacheCompatibilityKey)
	}
	if settlement.TheoreticalCacheTokens == nil || *settlement.TheoreticalCacheTokens != 100 {
		t.Fatalf("theoretical_cache_tokens = %v, want 100（401/4 取整）", settlement.TheoreticalCacheTokens)
	}
	if settlement.CacheTTLBucket == nil || *settlement.CacheTTLBucket != "5m" {
		t.Fatalf("cache_ttl_bucket = %v, want 5m（无 TTL 时归 5m 桶）", settlement.CacheTTLBucket)
	}
	if settlement.CacheScoreEligible == nil || !*settlement.CacheScoreEligible {
		t.Fatalf("cache_score_eligible = %v, want true", settlement.CacheScoreEligible)
	}
	if settlement.CacheScoreExcludedReason != nil {
		t.Fatalf("合格请求的排除原因必须为 NULL，实际 %q", *settlement.CacheScoreExcludedReason)
	}
}

// TestApplyCacheScoreFieldsNoAffinityStillRecords 断言「无亲和」不是「不写」：
// Node 在拿不到指纹时照样写那五列（eligible=false + no_affinity_key），
// 界面上的「缓存评分」区块因此能区分「本次不参与」与「该功能没接」。
func TestApplyCacheScoreFieldsNoAffinityStillRecords(t *testing.T) {
	gate := newCacheScoreGate(stubSettings{value: nil}, true) // 设置行未设置 → 回落 env(true)
	settler := &storeSettler{cacheScore: gate, logger: logx.New(nil)}
	pc := newSettlerContext(t) // 不装亲和写回

	settlement := terminal.Settlement{Usage: terminal.Usage{InputTokens: cacheScoreInt64Ptr(2)}}
	settler.applyCacheScoreFields(context.Background(), &settlement, pc, completedStreamOutcome())

	if settlement.CacheScoreEligible == nil || *settlement.CacheScoreEligible {
		t.Fatalf("无亲和时 eligible 必须是 false（而不是不写），实际 %v", settlement.CacheScoreEligible)
	}
	if settlement.CacheScoreExcludedReason == nil ||
		*settlement.CacheScoreExcludedReason != terminal.CacheScoreExcludedNoAffinityKey {
		t.Fatalf("排除原因 = %v, want %s", settlement.CacheScoreExcludedReason, terminal.CacheScoreExcludedNoAffinityKey)
	}
	if settlement.CacheCompatibilityKey != nil || settlement.CacheTTLBucket != nil {
		t.Fatal("无亲和时兼容键与 TTL 桶必须为 NULL（与 gate.ts 的早返回同形）")
	}
}

// TestApplyCacheScoreFieldsTruncatedStreamIsExcluded 断言截断流被排除，
// 且理由取 **attempt_failed** 而不是 stream_truncated。
//
// 为什么不是 stream_truncated：Node 的门控是短路顺序，截断流在它那里
// `isSuccessfulCompletion` 就已经是 false（streamEndedNormally=false 且没有
// clientAbortCompleteSuccess），先命中的是 attempt_failed 分支。
// Node 的 stream_truncated 只在「客户端在流**已收束**之后才断开」这一支出现
// （`clientAbortCompleteSuccess`），而 Go 的终态词表把这两种断开合成一个
// TerminalClientAborted，无法区分。
//
// 后果与取舍：Go 侧 stream_truncated **当前不可达**（登记在案，不假称已接），
// 两种情形的 eligible 都是 false（都排除在窗口聚合外），差别只在**标签**；
// 选保守侧（一律 attempt_failed）以免把部分交付的流当成合格样本污染缓存评分。
func TestApplyCacheScoreFieldsTruncatedStreamIsExcluded(t *testing.T) {
	gate := newCacheScoreGate(stubSettings{value: boolPtr(true)}, false)
	settler := &storeSettler{cacheScore: gate, logger: logx.New(nil)}
	pc := newSettlerContext(t)
	setAffinity(t, pc, stubAffinityWriteback{scopeTag: "s", tipFP: "f", tipPrefixBytes: 40, hasTip: true})

	outcome := completedStreamOutcome()
	outcome.Kind = forward.TerminalUpstreamTruncated
	settlement := terminal.Settlement{Usage: terminal.Usage{InputTokens: cacheScoreInt64Ptr(2)}}
	settler.applyCacheScoreFields(context.Background(), &settlement, pc, outcome)

	if settlement.CacheScoreEligible == nil || *settlement.CacheScoreEligible {
		t.Fatal("截断流的 eligible 必须为 false")
	}
	if settlement.CacheScoreExcludedReason == nil ||
		*settlement.CacheScoreExcludedReason != terminal.CacheScoreExcludedAttemptFailed {
		t.Fatalf("排除原因 = %v, want %s（Node 的短路顺序：截断流先命中断言失败）",
			settlement.CacheScoreExcludedReason, terminal.CacheScoreExcludedAttemptFailed)
	}
	// 兼容键与理论量仍然落库：Node 在这三个分支里都带上 base 字段。
	if settlement.CacheCompatibilityKey == nil || settlement.TheoreticalCacheTokens == nil {
		t.Fatal("被排除的请求仍应落兼容键与理论缓存量（Node 的 base 字段）")
	}
}

// TestApplyCacheScoreFieldsClientAbortAlsoAttemptFailed 钉住「客户端断开也算未成功交付」。
// 上游可能已报错误、也可能被截断，Go 无从区分；宁保守（不进窗口聚合）。
func TestApplyCacheScoreFieldsClientAbortAlsoAttemptFailed(t *testing.T) {
	gate := newCacheScoreGate(stubSettings{value: boolPtr(true)}, false)
	settler := &storeSettler{cacheScore: gate, logger: logx.New(nil)}
	pc := newSettlerContext(t)
	setAffinity(t, pc, stubAffinityWriteback{scopeTag: "s", tipFP: "f", tipPrefixBytes: 40, hasTip: true})

	outcome := completedStreamOutcome()
	outcome.Kind = forward.TerminalClientAborted
	settlement := terminal.Settlement{Usage: terminal.Usage{InputTokens: cacheScoreInt64Ptr(2)}}
	settler.applyCacheScoreFields(context.Background(), &settlement, pc, outcome)

	if settlement.CacheScoreExcludedReason == nil ||
		*settlement.CacheScoreExcludedReason != terminal.CacheScoreExcludedAttemptFailed {
		t.Fatalf("排除原因 = %v, want %s",
			settlement.CacheScoreExcludedReason, terminal.CacheScoreExcludedAttemptFailed)
	}
}

// TestApplyCostMultipliersFormatsNumbersLikeNode 断言倍率写成 numeric 文本的形态：
// Node 用 Number#toString（1 → "1"、0.3 → "0.3"），Go 侧必须是同一书写，
// 否则同一列在两侧的文本形态不同，对拍与聚合会莫名其妙地不等。
func TestApplyCostMultipliersFormatsNumbersLikeNode(t *testing.T) {
	cases := []struct {
		name            string
		provider, group *float64
		wantProvider    string
		wantGroup       string
	}{
		{name: "常见小数", provider: cacheScoreFloatPtr(0.3), group: cacheScoreFloatPtr(1.5), wantProvider: "0.3", wantGroup: "1.5"},
		{name: "整数不带尾零", provider: cacheScoreFloatPtr(1), group: cacheScoreFloatPtr(2), wantProvider: "1", wantGroup: "2"},
		{name: "零是合法倍率（免费）", provider: cacheScoreFloatPtr(0), group: nil, wantProvider: "0", wantGroup: ""},
		{name: "未提供时不写", provider: nil, group: nil, wantProvider: "", wantGroup: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settlement := terminal.Settlement{}
			applyCostMultipliers(&settlement, &RequestState{
				ProviderMultiplier: tc.provider,
				GroupMultiplier:    tc.group,
			})
			assertOptionalString(t, "cost_multiplier", settlement.CostMultiplier, tc.wantProvider)
			assertOptionalString(t, "group_cost_multiplier", settlement.GroupCostMultiplier, tc.wantGroup)
		})
	}
}

func assertOptionalString(t *testing.T, name string, got *string, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Fatalf("%s = %q，期望不写（NULL）", name, *got)
		}
		return
	}
	if got == nil {
		t.Fatalf("%s = NULL，期望 %q", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %q，期望 %q", name, *got, want)
	}
}

func cacheScoreInt64Ptr(v int64) *int64 { return &v }

func cacheScoreFloatPtr(v float64) *float64 { return &v }

func completedStreamOutcome() forward.StreamOutcome {
	return forward.StreamOutcome{Kind: forward.TerminalCompleted, StatusCode: 200}
}

func newSettlerContext(t *testing.T) *pctx.Context {
	t.Helper()
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造 pctx 失败: %v", err)
	}
	return pc
}

func setAffinity(t *testing.T, pc *pctx.Context, writeback pctx.AffinityWriteback) {
	t.Helper()
	pc.SetAffinityWriteback(writeback)
}
