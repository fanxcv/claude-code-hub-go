package route

import (
	"context"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/redis/go-redis/v9"
)

// 本文件是「前缀亲和 = 渠道粘性」的应用级证明：同一前缀连续两次选路，第二次必须**直接回到**
// 第一次那家供应商，而不是重新决策一次。
//
// 为什么单独立用例：亲和存储自身的语义（查找/写回/墓碑/围栏）已由 affinity_integration_test.go
// 覆盖，但那些用例直接调 store。用户报的问题是「同会话没粘住」，落在**选路回路**上：
// 写回 → 下一次查找 → 提名 → 选中。本用例把这条回路整条跑真实 Redis。
//
// 决定性设计：造两个**等价**候选（同优先级、同权重），第二次选路时让脚本化随机数指向另一家。
// 若亲和回路成立，选路结果必须无视随机数回到命中那家（Node 的亲和是「软提名 + 全套硬校验」，
// 提名成功即不进入加权随机）；若回路断了，本用例会选中随机数指示的那家——转红。

// affinityReuseBody 是同一前缀的请求正文（claude 线）。它含 system + 两条会话消息，
// 故 tip 落在第 2 条消息（depth > 0，满足「有会话消息才写绑定」的前提）。
func affinityReuseBody(t *testing.T) map[string]any {
	t.Helper()
	return claudeBody(t, `{
		"system": "you are helpful",
		"messages": [
			{"role": "user", "content": "第一条"},
			{"role": "assistant", "content": [{"type": "text", "text": "好"}]}
		]
	}`)
}

// TestIntegrationAffinitySecondSelectReusesSameProvider 第二次选路命中第一次写回的绑定。
func TestIntegrationAffinitySecondSelectReusesSameProvider(t *testing.T) {
	client := integrationRedis(t)
	// keyID 唯一 → scopeTag 唯一：本用例与其他亲和用例共用一个 Redis 库，必须各扫各的键。
	keyID := time.Now().UnixNano()
	const model = "affinity-reuse-model"
	scope := ScopeTag(keyID, convert.FormatClaude, model)
	cleanAffinityScope(t, client, scope)

	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 300,
	})

	first := baseProvider(1, convert.ProviderClaude)
	second := baseProvider(2, convert.ProviderClaude)
	// 两家等价（baseProvider 同优先级同权重）；正是「重新决策一次」与「粘住」会分岔的形态。
	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{first, second}, byID: map[int64]Provider{1: first, 2: second}},
		Affinity: store,
		// 第一次随机数指向 1；第二次指向 2——若亲和回路生效，结果必须仍是 1。
		Rand: (&scriptedRand{values: []float64{0, 0.99}}).next,
	})

	request := Request{
		Model:        model,
		Format:       convert.FormatClaude,
		KeyID:        keyID,
		Group:        GroupDefault,
		AffinityBody: affinityReuseBody(t),
	}

	// ---- 第一次：应为未命中（initial_selection），并给出写回事实 ----
	firstResult, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("第一次选路失败: %v", err)
	}
	if firstResult.Provider == nil {
		t.Fatalf("第一次选路未选中供应商")
	}
	if firstResult.Reason != ReasonSelectedInitial {
		t.Fatalf("第一次应为首次选择，实际 reason=%q", firstResult.Reason)
	}
	if firstResult.AffinityWriteback == nil {
		t.Fatalf("第一次选路必须给出亲和写回事实（缺了它就没有粘性）")
	}
	// 模拟成功终态提交后的写回（生产由终态层调用同一方法）。
	if !firstResult.AffinityWriteback.RecordWinner(context.Background(), firstResult.Provider.ID) {
		t.Fatalf("写回亲和绑定失败（generation CAS 未通过？）")
	}

	// ---- 第二次：同一前缀，必须命中并回到同一家 ----
	secondResult, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("第二次选路失败: %v", err)
	}
	if secondResult.Provider == nil {
		t.Fatalf("第二次选路未选中供应商")
	}
	if secondResult.Reason != ReasonSelectedAffinity {
		t.Fatalf("第二次应命中前缀亲和（粘住上一家），实际 reason=%q（随机数指示的是 provider 2）",
			secondResult.Reason)
	}
	if secondResult.Provider.ID != firstResult.Provider.ID {
		t.Fatalf("第二次选中 provider %d，第一次是 %d：同一前缀没有粘住",
			secondResult.Provider.ID, firstResult.Provider.ID)
	}
	if secondResult.Method != MethodPrefixAffinity {
		t.Fatalf("命中亲和的 selectionMethod 应为 %q，实际 %q", MethodPrefixAffinity, secondResult.Method)
	}
	// 命中详情要能落链（界面显示「命中哪一段前缀」靠它）。
	if secondResult.Affinity == nil || secondResult.Affinity.Hint.MatchedFP == "" {
		t.Fatalf("命中亲和时应带命中详情：%+v", secondResult.Affinity)
	}
	if secondResult.Affinity.MatchedPrefixByte <= 0 {
		t.Fatalf("命中边界的前缀字节数应 > 0，实际 %d", secondResult.Affinity.MatchedPrefixByte)
	}
}

// cleanAffinityScope 清掉本用例 scope 下的全部亲和键（{fp}、{gen}、{desc-v2}）。
//
// 为何不用 affinityScope 助手：它的 scope 是随机串，而本用例的 scope 由选路器按
// ScopeTag(keyID, format, model) 算出，必须按算出来的那个清。
func cleanAffinityScope(t *testing.T, client redis.UniversalClient, scope string) {
	t.Helper()
	pattern := affinityKeyPrefix + "{" + scope + ":*"
	t.Cleanup(func() {
		ctx := context.Background()
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, pattern, 64).Result()
			if err != nil {
				return
			}
			if len(keys) > 0 {
				_ = client.Del(ctx, keys...).Err()
			}
			if next == 0 {
				return
			}
			cursor = next
		}
	})
}
