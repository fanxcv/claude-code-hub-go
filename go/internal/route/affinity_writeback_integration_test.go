package route

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 零值（未参与亲和）与前置条件不满足时一律 no-op：这些分支不碰 Redis，
// 是「开关关闭时绝不产生写入」这条纪律的钉子。
func TestAffinityWritebackSkipsWhenNotApplicable(t *testing.T) {
	ctx := context.Background()
	store := NewAffinityStore(AffinityOptions{Window: 8, SlidingTTLSeconds: 120})

	cases := []struct {
		name      string
		writeback AffinityWriteback
		winner    int64
		failed    int64
	}{
		{
			name:   "零值写回（亲和未参与）",
			winner: 7,
			failed: 7,
		},
		{
			name: "tip 落在系统段（depth 为 0）",
			writeback: AffinityWriteback{store: store, ScopeTag: "s", TipFP: "f", TipDepth: 0,
				IdentityFP: "i", Generation: "v3:g"},
			winner: 7,
		},
		{
			name: "未提名时不写墓碑",
			writeback: AffinityWriteback{store: store, ScopeTag: "s", MatchedFP: "f",
				IdentityFP: "i", Generation: "v3:g"},
			failed: 7,
		},
		{
			name: "失败者不是提名者时不写墓碑",
			writeback: AffinityWriteback{store: store, ScopeTag: "s", MatchedFP: "f",
				IdentityFP: "i", Generation: "v3:g", NominatedProviderID: 8},
			failed: 7,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.winner > 0 && tc.writeback.RecordWinner(ctx, tc.winner) {
				t.Error("RecordWinner 应返回 false")
			}
			if tc.failed > 0 && tc.writeback.TombstoneOnFailure(ctx, tc.failed) {
				t.Error("TombstoneOnFailure 应返回 false")
			}
		})
	}
}

// TestIntegrationAffinityWritebackWinnerAndFence 用真实 Redis 钉住成功写回的三条硬性质：
// 键形制（scope 下的 tip 指纹）、四段值与 generation CAS、以及 Invalidate 之后旧 generation
// 的写回被 fence 拒绝（否则在途旧请求能把已终止的绑定复活）。
func TestIntegrationAffinityWritebackWinnerAndFence(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	scope := affinityScope(t, client)
	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 120,
		GenerationToken:   counterToken(),
	})

	tip, shallow := hash32(scope+"-tip"), hash32(scope+"-shallow")

	// 未命中路径：Lookup 会先 ensure generation，写回必须带它做 CAS。
	lookup, ok := store.Lookup(ctx, scope, []string{tip, shallow})
	if !ok || lookup.Hint != nil {
		t.Fatalf("空库查找应可用且不命中: ok=%v hint=%+v", ok, lookup.Hint)
	}
	writeback := AffinityWriteback{
		store:      store,
		ScopeTag:   scope,
		TipFP:      tip,
		TipDepth:   2,
		IdentityFP: lookup.IdentityFP,
		Generation: lookup.Generation,
	}
	if writeback.IdentityFP != tip {
		t.Fatalf("未命中的 identity root 应为最深指纹 tip，得到 %q", writeback.IdentityFP)
	}

	if !writeback.RecordWinner(ctx, 7) {
		t.Fatalf("成功写回应写入")
	}
	wantValue := "1|7|" + lookup.IdentityFP + "|" + lookup.Generation
	if value := affinityValueAt(t, client, store.bindingKey(scope, tip)); value != wantValue {
		t.Errorf("绑定值 = %q，期望 %q", value, wantValue)
	}
	assertTTLBetween(t, client, store.bindingKey(scope, tip), 100*time.Second, 120*time.Second)

	// generation 推进（管理面终止前缀 Session）后，旧 generation 的写回必须被拒。
	if !store.Invalidate(ctx, scope, lookup.IdentityFP, []string{tip}) {
		t.Fatalf("generation 推进应成功")
	}
	if writeback.RecordWinner(ctx, 9) {
		t.Error("旧 generation 的写回必须被 fence 拒绝")
	}
	if client.Exists(ctx, store.bindingKey(scope, tip)).Val() != 0 {
		t.Error("invalidate 已删除的绑定不得复活")
	}

	// 新 generation 下同一提示位置的写回可成功，且值为新代际。
	next, ok := store.Lookup(ctx, scope, []string{tip, shallow})
	if !ok || next.Hint != nil {
		t.Fatalf("推进后查找应可用且不命中: ok=%v hint=%+v", ok, next.Hint)
	}
	if next.Generation == lookup.Generation {
		t.Fatalf("invalidate 必须推进 generation: %q", next.Generation)
	}
	fresh := writeback
	fresh.Generation = next.Generation
	if !fresh.RecordWinner(ctx, 9) {
		t.Fatalf("新 generation 的写回应写入")
	}
	if value := affinityValueAt(t, client, store.bindingKey(scope, tip)); !strings.HasPrefix(value, "1|9|") {
		t.Errorf("新代际绑定值 = %q，期望以 1|9| 开头", value)
	}
}

// TestIntegrationAffinityWritebackTombstone 用真实 Redis 钉住墓碑：只有「提名者即失败者」
// 才在命中边界写短 TTL 墓碑，且墓碑形制与 Node 一致（0|<reason>|<identity>|<generation>）。
func TestIntegrationAffinityWritebackTombstone(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	scope := affinityScope(t, client)
	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 120,
		GenerationToken:   counterToken(),
	})

	tip, mid := hash32(scope+"-tip"), hash32(scope+"-mid")
	deep, shallow := hash32(scope+"-deep"), hash32(scope+"-shallow")

	// 预置一个活跃绑定：先落 identity generation（写回的 CAS 目标），再写绑定本身。
	const seedGeneration = "v3:seed"
	if err := client.Set(ctx, store.generationKey(scope, mid), seedGeneration, time.Hour).Err(); err != nil {
		t.Fatalf("预置 generation 失败: %v", err)
	}
	if !store.Put(ctx, scope, mid, 7, mid, seedGeneration) {
		t.Fatalf("预置绑定应写入")
	}
	lookup, ok := store.Lookup(ctx, scope, []string{deep, mid, shallow})
	if !ok || lookup.Hint == nil || lookup.Hint.MatchedFP != mid {
		t.Fatalf("预置绑定应被深->浅查找命中: ok=%v hint=%+v", ok, lookup.Hint)
	}
	writeback := AffinityWriteback{
		store:               store,
		ScopeTag:            scope,
		TipFP:               tip,
		TipDepth:            3,
		IdentityFP:          lookup.IdentityFP,
		Generation:          lookup.Generation,
		MatchedFP:           lookup.Hint.MatchedFP,
		NominatedProviderID: lookup.Hint.ProviderID,
	}

	// 失败者不是提名者：不写墓碑，原绑定原样保留。
	if writeback.TombstoneOnFailure(ctx, 9) {
		t.Error("失败者非提名者时不应写墓碑")
	}
	if value := affinityValueAt(t, client, store.bindingKey(scope, mid)); !strings.HasPrefix(value, "1|7|") {
		t.Errorf("原绑定不应被改动，得到 %q", value)
	}

	// 提名者失败：写墓碑并覆盖命中边界键，TTL 缩短到墓碑口径（60s）。
	if !writeback.TombstoneOnFailure(ctx, 7) {
		t.Fatalf("提名者失败应写墓碑")
	}
	wantValue := "0|failover|" + lookup.IdentityFP + "|" + lookup.Generation
	if value := affinityValueAt(t, client, store.bindingKey(scope, mid)); value != wantValue {
		t.Errorf("墓碑值 = %q，期望 %q", value, wantValue)
	}
	assertTTLBetween(t, client, store.bindingKey(scope, mid), 30*time.Second, 60*time.Second)

	// 墓碑生效：同名查找跳过墓碑（同一次查找里更深无绑定，故整体不命中）。
	if next, ok := store.Lookup(ctx, scope, []string{deep, mid, shallow}); !ok || next.Hint != nil {
		t.Errorf("墓碑应被查找跳过: ok=%v hint=%+v", ok, next.Hint)
	}
}
