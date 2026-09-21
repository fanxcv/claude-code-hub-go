package route_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件是「前缀亲和层提名因**瞬时**原因未被采用」这条契约的**跨层端到端钉子**：真
// AffinityStore（真 Redis）+ 真选路器 + 真终态结算器，断言最终落到 Redis 的 tip 绑定上。
//
// 为什么必须跨层：判定在选路层（route.AffinityBypass），动作在终态层（settle.go 的
// affinityWinner → AffinityWriteback.RecordWinner），两者隔着 pctx。分层钉子各自全绿仍可能
// 整体失效（判定做对了但没人读）。包内那条钉「分类与 RecordWinner 的返回值」，本文件钉
// 「跑完整条终态之后，Redis 里的 tip 绑定到底指向谁」。
//
// 为何是**外部测试包**（route_test）而不是包内：本文件要 import terminal，而
// session → guard → terminal（guard/adapters.go），包内测试会撞 import cycle。
//
// 真库门控：未设 CCH_TEST_REDIS_URL 时跳过（CI 即跳过）。故 CI 门禁的半边依据在包内。

// prefixBypassBody 造一份可指纹化的 claude 请求体（与包内夹具同形）。
func prefixBypassBody(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(`{"messages": [{"role": "user", "content": "hi"}]}`), &body); err != nil {
		t.Fatalf("构造请求体失败: %v", err)
	}
	return body
}

// runPrefixBypassAcrossLayers 跑完整链：预置 tip 绑定指向 7 → 选路（直读按 sourceErr 的形态
// 返回）→ 备用 8 成功终态 → 读回 Redis 的 tip 绑定。返回终态后 tip 绑定指向的 providerID。
func runPrefixBypassAcrossLayers(t *testing.T, sourceErr error) int64 {
	t.Helper()
	rdb := lookupFailureRedis(t)
	ctx := context.Background()
	affinity := route.NewAffinityStore(route.AffinityOptions{
		Redis:             rdb,
		Window:            8,
		SlidingTTLSeconds: 120,
	})

	body := prefixBypassBody(t)
	chain, ok := route.Fingerprint(body, convert.FormatClaude, 8)
	if !ok {
		t.Fatal("夹具正文应能指纹化")
	}
	tip := chain.Tip().FP

	// scope 由 KeyID 派生，故取唯一 KeyID 并按 scope 前缀清理（键前缀是设计稿写定的契约：
	// cch:pfx:{<scopeTag>}:...）。
	keyID := time.Now().UnixNano()
	scope := route.ScopeTag(keyID, convert.FormatClaude, "m")
	t.Cleanup(func() {
		keys, _, err := rdb.Scan(ctx, 0, "cch:pfx:{"+scope+":*", 64).Result()
		if err == nil && len(keys) > 0 {
			_ = rdb.Del(ctx, keys...).Err()
		}
	})

	// 先 Lookup 一次：未命中路径会 ensure generation，写回的 CAS 需要它。
	lookup, ok := affinity.Lookup(ctx, scope, chain.DeepestFirst())
	if !ok || lookup.Hint != nil {
		t.Fatalf("空库查找应可用且不命中: ok=%v hint=%+v", ok, lookup.Hint)
	}
	if !affinity.Put(ctx, scope, tip, 7, lookup.IdentityFP, lookup.Generation) {
		t.Fatal("预置绑定应写入")
	}

	const (
		boundID  int64 = 7
		backupID int64 = 8
	)
	bound := lookupFailureProvider(boundID)
	backup := lookupFailureProvider(backupID)
	selector := route.NewSelector(route.Options{
		// 备用排首位：加权随机的脚本取首家，便于断言「回落到备用」而非恰好抽中绑定家。
		Source: lookupFailureSource{
			providers: []route.Provider{backup, bound},
			byID:      map[int64]route.Provider{boundID: bound, backupID: backup},
			err:       sourceErr,
		},
		Affinity: affinity,
		Rand:     func() float64 { return 0 },
	})
	request := route.Request{
		Model: "m", Format: convert.FormatClaude, KeyID: keyID,
		AffinityBody: body,
		AffinityLookup: &route.AffinityLookup{
			Hint:       &route.AffinityHint{ProviderID: boundID, MatchedFP: tip, MatchedIndex: 0},
			IdentityFP: lookup.IdentityFP,
			Generation: lookup.Generation,
		},
	}

	result, err := selector.Select(ctx, request)
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != backupID {
		t.Fatalf("提名被拒时应回落选备用 %d，实际 %+v", backupID, result.Provider)
	}
	if result.AffinityWriteback == nil {
		t.Fatal("被拒的提名仍应带回写回事实（终态需要它的 identity 与 generation）")
	}

	// 终态：备用成功。守卫链在这一步把写回事实塞进 pctx（与 guard/adapters_route.go 同源）。
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造请求上下文失败: %v", err)
	}
	if err := pc.SetMessageRequestID(1); err != nil {
		t.Fatalf("写入行标识失败: %v", err)
	}
	pc.SetAffinityWriteback(*result.AffinityWriteback)

	durationMS := 62
	settlement := terminal.Settlement{
		StatusCode:    200,
		DurationMS:    &durationMS,
		ProviderChain: []byte(`[{"id":8,"reason":"request_success"}]`),
		Affinity:      terminal.AffinityDirective{WinnerProviderID: backupID},
	}
	if _, err := terminal.New(lookupFailureWriter{}, terminal.Options{}).
		SettleContext(ctx, pc, settlement, nil); err != nil && !errors.Is(err, terminal.ErrNotSettled) {
		t.Fatalf("终态结算失败: %v", err)
	}

	after, ok := affinity.Lookup(ctx, scope, chain.DeepestFirst())
	if !ok || after.Hint == nil {
		t.Fatalf("终态后 tip 绑定应仍可命中（原绑定或新绑定）: ok=%v hint=%+v", ok, after.Hint)
	}
	return after.Hint.ProviderID
}

// TestPrefixBindingKeptWhenLookupFailsAcrossLayers 主线：提名者**直读失败**（瞬时）⇒ 本次
// 回落选备用 ⇒ 备用**成功**终态跑完 ⇒ Redis 里的 tip 绑定**仍指向原 provider**。
func TestPrefixBindingKeptWhenLookupFailsAcrossLayers(t *testing.T) {
	providerID := runPrefixBypassAcrossLayers(t, errors.New("lookup: 连接池暂时不可用"))
	if providerID != 7 {
		t.Fatalf("提名者直读失败属临时原因，备用成功不得改写 tip 绑定：应仍为 7，实际 %d"+
			"（一次 DB 抖动就会把会话永久改粘到备用）", providerID)
	}
}

// TestPrefixBindingReboundWhenLookupRowGoneAcrossLayers 反向：提名者**行已不存在**
// （route.ErrProviderNotFound，结构性）⇒ 备用成功**允许**改写 ⇒ tip 绑定变为备用。
//
// 没有这条反向，「任何失败都判 transient」也能让主线变绿，而后果是会话永远钉在一家已删除的
// 渠道上（tip 绑定再也改不掉）。两条合起来才证明「瞬时」与「结构性」真的被分开。
func TestPrefixBindingReboundWhenLookupRowGoneAcrossLayers(t *testing.T) {
	providerID := runPrefixBypassAcrossLayers(t, route.ErrProviderNotFound)
	if providerID != 8 {
		t.Fatalf("提名者行已不存在属结构性失效，应允许改写为备用 8，实际绑定为 %d", providerID)
	}
}
