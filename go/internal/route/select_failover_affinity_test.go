package route

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/redis/go-redis/v9"
)

// 本文件钉住「故障转移 / 重试 / 竞速取候选**绝不咨询亲和**」这条 Node↔Go parity 契约。
//
// 为什么必须单独钉：Node 的亲和提名只在**首次选择**时跑一次
// （`provider-selector.ts:326` 的 `tryPrefixAffinityNomination`，守卫是 `!session.provider`），
// 而故障转移走 `forwarder.ts:4880 selectAlternative` → `provider-selector.ts:620
// pickRandomProviderWithExclusion` → `pickRandomProvider`（`:1168-1515`）——
// 该函数体内**没有一个 affinity 引用**，取候选是纯「`selectTopPriority` 分层 + 同档加权随机」。
//
// Go 此前在故障转移时复用 `Select` 并带上请求正文，于是先做了一次亲和提名：当亲和 hint
// 指向一家「既非当前主选、又不在排除表」的供应商时（典型成因：兄弟会话刚写回更近的前缀记录），
// 会跳去那家，而 Node 会按分层重挑。既有用例看不见它——主选总在排除表里，提名被 `excluded` 挡住。
//
// 判据设计：让 hint 目标落在**最低优先档**、而主选与其同档候选落在最高档。于是
// 「咨询亲和」与「按分层重挑」必然给出**不同**的供应商，判据不依赖随机序列，也不靠
// `selectionMethod` 这类间接信号单独立论。
func TestSelectFailoverDoesNotConsultAffinity(t *testing.T) {
	// hint 目标 7：最低优先档（优先级数值最大 = 最低优先）。
	hintTarget := baseProvider(7, convert.ProviderClaude)
	hintTarget.Priority = intPtr(5)
	// 前序失败的主选 8 与同档健康候选 9：最高优先档。
	primary := baseProvider(8, convert.ProviderClaude)
	primary.Priority = intPtr(1)
	sameTier := baseProvider(9, convert.ProviderClaude)
	sameTier.Priority = intPtr(1)

	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	chain, ok := Fingerprint(body, convert.FormatClaude, 8)
	if !ok {
		t.Fatal("夹具正文应能指纹化")
	}
	// 注入已算好的查找结果：本用例钉的是**是否咨询亲和**，不该顺带依赖 Redis。
	lookup := &AffinityLookup{
		Hint:       &AffinityHint{ProviderID: hintTarget.ID, MatchedFP: chain.Tip().FP, MatchedIndex: 0},
		IdentityFP: "idfp",
		Generation: "v3:gen",
	}
	selector := NewSelector(Options{
		Source: &stubSource{
			providers: []Provider{hintTarget, primary, sameTier},
			byID:      map[int64]Provider{7: hintTarget, 8: primary, 9: sameTier},
		},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
	})

	// 对照臂（两态都必须成立）：**主选**路径仍旧走亲和。亲和是主选独有机制，
	// 修故障转移不能顺手把它关掉——否则粘性（缓存命中）整体失效。
	primaryResult, err := selector.Select(context.Background(), Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42,
		AffinityBody: body, AffinityLookup: lookup,
	})
	if err != nil {
		t.Fatalf("主选失败: %v", err)
	}
	if primaryResult.Provider == nil || primaryResult.Provider.ID != hintTarget.ID {
		t.Fatalf("主选应仍由亲和提名到 %d，实际 %+v", hintTarget.ID, primaryResult.Provider)
	}
	if primaryResult.Method != MethodPrefixAffinity {
		t.Errorf("主选 selectionMethod = %q，期望 %q", primaryResult.Method, MethodPrefixAffinity)
	}

	// 主线：故障转移取候选不得咨询亲和。
	failoverResult, err := selector.SelectFailover(context.Background(), Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42,
		ExcludeIDs:   []int64{primary.ID},
		AffinityBody: body, AffinityLookup: lookup,
	})
	if err != nil {
		t.Fatalf("故障转移选路失败: %v", err)
	}
	if failoverResult.Provider == nil || failoverResult.Provider.ID != sameTier.ID {
		t.Fatalf("故障转移应按分层取到最高档的 %d，实际 %+v（选到 hint 目标即说明咨询了亲和）",
			sameTier.ID, failoverResult.Provider)
	}
	if failoverResult.Method != MethodWeightedRandom {
		t.Errorf("故障转移 selectionMethod = %q，期望 %q——Node 的 pickRandomProvider 只产出加权随机/分组筛选",
			failoverResult.Method, MethodWeightedRandom)
	}
	if failoverResult.Affinity != nil {
		t.Errorf("故障转移不得产生亲和提名: %+v", failoverResult.Affinity)
	}
	// 亲和状态的三件事实在故障转移路径上必须一概不产生：没有查找就没有命中续期
	// （Lookup 的 Lua 在命中时原子续期），没有写回事实就没有终态 generation CAS。
	if failoverResult.AffinityLookup != nil {
		t.Error("故障转移不应做亲和查找（命中续期是亲和状态改写）")
	}
	if failoverResult.AffinityWriteback != nil {
		t.Error("故障转移不应产出亲和写回事实（终态会拿它做 generation CAS）")
	}
	if failoverResult.AffinityIdentity != nil {
		t.Error("故障转移不应产出亲和身份事实（身份形制由主选期的守卫决定）")
	}
}

// countingRedis 只回答一个问题：**这段路径到底有没有访问亲和 Redis**。
// 它不实现真实语义——任何脚本调用都直接报错，于是查找判为不可用（与 Redis 故障同路径），
// 选路回落加权随机。本用例只数调用次数，不看查找结果。
type countingRedis struct {
	redis.UniversalClient
	mu    sync.Mutex
	calls int
}

func (c *countingRedis) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// EvalSha 是拦截点：redis.Script.Run 先走 EVALSHA（go-redis v9 script.go:194），
// 故实现它就能截住整次查找，且返回非 ErrNoScript 的错误不会触发 EVAL 回退。
func (c *countingRedis) EvalSha(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	cmd := redis.NewCmd(ctx)
	cmd.SetErr(errors.New("countingRedis: 本用例只数访问次数"))
	return cmd
}

// TestSelectFailoverSkipsAffinityLookup 从**访问面**钉住同一约束的更强形式：
// 故障转移路径连亲和 Redis 都不该碰。
//
// 为何不能只看结果：Lookup 不只是读。命中时它紧接着跑 validate-hit CAS
// （`affinity_cas_write_v3`，内含 `EXPIRE`/`ZADD`/`SET`）以**滑动续期**；未命中也会跑
// ensureGeneration 写世代键（`affinity_ensure_generation_v4` 的 `SET ... NX`）。
// 即一次查找就是一次亲和状态改写，而「查到了但没用」与「根本没查」在 Result 上可能同形。
// 本用例要的是后者，所以直接数 EVALSHA 次数。
func TestSelectFailoverSkipsAffinityLookup(t *testing.T) {
	providers := []Provider{baseProvider(7, convert.ProviderClaude), baseProvider(8, convert.ProviderClaude)}
	client := &countingRedis{}
	selector := NewSelector(Options{
		Source: &stubSource{
			providers: providers,
			byID:      map[int64]Provider{7: providers[0], 8: providers[1]},
		},
		Affinity: NewAffinityStore(AffinityOptions{Redis: client, Window: 8}),
	})
	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	request := Request{Model: "m", Format: convert.FormatClaude, KeyID: 42, AffinityBody: body}

	// 对照臂：首次选择**会**做一次亲和查找（这里不注入已算好的 lookup）。
	before := client.count()
	if _, err := selector.Select(context.Background(), request); err != nil {
		t.Fatalf("主选失败: %v", err)
	}
	if got := client.count() - before; got != 1 {
		t.Fatalf("首次选择应恰好做 1 次亲和查找，实际 %d 次", got)
	}

	// 主线：故障转移不得新增任何亲和 Redis 访问。
	failoverRequest := request
	failoverRequest.ExcludeIDs = []int64{7}
	after := client.count()
	if _, err := selector.SelectFailover(context.Background(), failoverRequest); err != nil {
		t.Fatalf("故障转移选路失败: %v", err)
	}
	if got := client.count() - after; got != 0 {
		t.Errorf("故障转移不得访问亲和 Redis（命中续期属亲和状态改写），实际新增 %d 次", got)
	}
}
