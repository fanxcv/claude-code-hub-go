package route

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// affinityScope 造一个本次运行独有的 scope tag 并登记清理：
// 亲和的 gen/desc-v2 键由脚本在 Redis 内创建，无法靠「记住写过的键」穷举，
// 故按前缀清扫自己这一个 scope（scope 唯一，不会碰到别的键空间）。
func affinityScope(t *testing.T, client redis.UniversalClient) string {
	t.Helper()
	scope := "routeit" + strconv.FormatInt(time.Now().UnixNano(), 10)
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
	return scope
}

// counterToken 返回按序递增的 generation token，让测试能断言「哪一次查找用了哪个 generation」。
func counterToken() func() string {
	var mu sync.Mutex
	index := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		index++
		return "v3:test-" + strconv.Itoa(index)
	}
}

func affinityValueAt(t *testing.T, client redis.UniversalClient, key string) string {
	t.Helper()
	value, err := client.Get(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("读取键 %s 失败: %v", key, err)
	}
	return value
}

func assertTTLBetween(t *testing.T, client redis.UniversalClient, key string, low, high time.Duration) {
	t.Helper()
	ttl, err := client.TTL(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("读取键 %s 的 TTL 失败: %v", key, err)
	}
	if ttl < low || ttl > high {
		t.Errorf("键 %s 的 TTL = %s，期望落在 (%s, %s]", key, ttl, low, high)
	}
}

// 命中路径的完整语义：墓碑跳过、旧格式迁移、generation 落键、滑动续期。
func TestIntegrationAffinityLookupSkipsTombstoneAndRenewsTTL(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	scope := affinityScope(t, client)
	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 120,
		GenerationToken:   counterToken(),
	})

	deepest, middle, shallow := hash32(scope+"-deep"), hash32(scope+"-mid"), hash32(scope+"-shallow")

	// 最深是墓碑（failover 后短 TTL），中间是两段旧格式的活跃绑定，最浅为空。
	if err := client.Set(ctx, store.bindingKey(scope, deepest), "0|failover|idfp|v3:old", time.Minute).Err(); err != nil {
		t.Fatalf("写入墓碑失败: %v", err)
	}
	if err := client.Set(ctx, store.bindingKey(scope, middle), "1|7", 5*time.Second).Err(); err != nil {
		t.Fatalf("写入旧格式绑定失败: %v", err)
	}

	lookup, ok := store.Lookup(ctx, scope, []string{deepest, middle, shallow})
	if !ok {
		t.Fatalf("查找应可用")
	}
	if lookup.Hint == nil {
		t.Fatalf("应在跳过墓碑后命中中间前缀")
	}
	if lookup.Hint.ProviderID != 7 || lookup.Hint.MatchedIndex != 1 || lookup.Hint.MatchedFP != middle {
		t.Fatalf("命中结果不符: %+v", lookup.Hint)
	}
	// 旧格式没有 identity/generation：迁移时 identity 取命中指纹，generation 取本次新 token。
	if lookup.IdentityFP != middle || lookup.Generation != "v3:test-1" {
		t.Fatalf("迁移后的 identity/generation 不符: %q / %q", lookup.IdentityFP, lookup.Generation)
	}

	// 迁移值必须落回同一个绑定键且保留原 TTL 口径（KEEPTTL 后再续期）。
	if value := affinityValueAt(t, client, store.bindingKey(scope, middle)); value != "1|7|"+middle+"|v3:test-1" {
		t.Errorf("绑定值未升级为四段格式: %q", value)
	}
	// 命中即续期：原 TTL 是 5s，请求 120s，应被抬到 120s。
	assertTTLBetween(t, client, store.bindingKey(scope, middle), 100*time.Second, 120*time.Second)
	// identity generation 已落键，并带 fence TTL（Node 的 GENERATION_FENCE_TTL_SECONDS = 2 天）。
	if value := affinityValueAt(t, client, store.generationKey(scope, middle)); value != "v3:test-1" {
		t.Errorf("gen 键 = %q，期望 v3:test-1", value)
	}
	assertTTLBetween(t, client, store.generationKey(scope, middle),
		time.Duration(affinityGenerationFenceTTLSeconds-5)*time.Second,
		time.Duration(affinityGenerationFenceTTLSeconds)*time.Second)
	// descendant 有序集合登记了本次绑定的过期时刻（失效路径靠它找回绑定键）。
	members, err := client.ZRange(ctx, store.descendantsV2Key(scope, middle), 0, -1).Result()
	if err != nil {
		t.Fatalf("读取 desc-v2 失败: %v", err)
	}
	if len(members) != 1 || members[0] != store.bindingKey(scope, middle) {
		t.Errorf("desc-v2 成员不符: %v", members)
	}
	// 墓碑与更浅的键都不该被写入。
	if exists, _ := client.Exists(ctx, store.bindingKey(scope, shallow)).Result(); exists != 0 {
		t.Errorf("不应写入未命中的更浅键")
	}
}

// 未命中路径：仍要保证 identity root 有稳定的 generation（否则终态写回无 CAS 依据）。
func TestIntegrationAffinityLookupMissEnsuresStableGeneration(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	scope := affinityScope(t, client)
	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 60,
		GenerationToken:   counterToken(),
	})

	deepest, shallow := hash32(scope+"-deep"), hash32(scope+"-shallow")
	first, ok := store.Lookup(ctx, scope, []string{deepest, shallow})
	if !ok {
		t.Fatalf("查找应可用")
	}
	if first.Hint != nil {
		t.Fatalf("空键空间不应命中: %+v", first.Hint)
	}
	if first.IdentityFP != deepest || first.Generation != "v3:test-1" {
		t.Fatalf("未命中时 identity 应取最深指纹、generation 应为本次 token，实际 %q / %q",
			first.IdentityFP, first.Generation)
	}
	if value := affinityValueAt(t, client, store.generationKey(scope, deepest)); value != "v3:test-1" {
		t.Errorf("gen 键 = %q，期望 v3:test-1", value)
	}

	// 第二次查找必须复用同一 generation：SET NX 保证 identity root 的 generation 稳定，
	// 否则同一次请求在途中的写回会被自己的第二次查找推翻。
	second, ok := store.Lookup(ctx, scope, []string{deepest, shallow})
	if !ok || second.Generation != "v3:test-1" {
		t.Fatalf("第二次查找应复用同一 generation，实际 %+v ok=%v", second, ok)
	}
}

// generation fence 的核心断言：写回必须在 generation 仍有效时成功，旧 generation 一律被拒。
func TestIntegrationAffinityPutHonorsGenerationFence(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	scope := affinityScope(t, client)
	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 90,
		GenerationToken:   counterToken(),
	})

	tip, ancestor := hash32(scope+"-tip"), hash32(scope+"-ancestor")
	lookup, ok := store.Lookup(ctx, scope, []string{tip, ancestor})
	if !ok || lookup.Hint != nil {
		t.Fatalf("查找应可用且未命中: %+v", lookup)
	}

	if !store.Put(ctx, scope, tip, 7, lookup.IdentityFP, lookup.Generation) {
		t.Fatalf("当前 generation 的写回应成功")
	}
	bindingKey := store.bindingKey(scope, tip)
	if value := affinityValueAt(t, client, bindingKey); value != "1|7|"+lookup.IdentityFP+"|"+lookup.Generation {
		t.Errorf("绑定值不符: %q", value)
	}
	assertTTLBetween(t, client, bindingKey, 70*time.Second, 90*time.Second)

	// 落定的绑定可被再次命中，且命中携带同一 identity 与 generation。
	hit, ok := store.Lookup(ctx, scope, []string{tip, ancestor})
	if !ok || hit.Hint == nil || hit.Hint.ProviderID != 7 {
		t.Fatalf("写回后应能命中: %+v ok=%v", hit, ok)
	}
	if hit.IdentityFP != lookup.IdentityFP || hit.Generation != lookup.Generation {
		t.Errorf("命中应沿用 identity/generation，实际 %q / %q", hit.IdentityFP, hit.Generation)
	}

	// 迟到的旧 generation（在途请求带着 invalidate 之前的 generation 回来）必须被拒。
	if store.Put(ctx, scope, tip, 8, lookup.IdentityFP, "v3:stale") {
		t.Errorf("旧 generation 的写回必须被 fence 拒绝")
	}
	if value := affinityValueAt(t, client, bindingKey); value != "1|7|"+lookup.IdentityFP+"|"+lookup.Generation {
		t.Errorf("被拒的写回不得改动绑定: %q", value)
	}

	// 失效推进 generation 并清掉绑定。
	if !store.Invalidate(ctx, scope, lookup.IdentityFP, []string{tip, ancestor}) {
		t.Fatalf("失效应成功")
	}
	bumped := affinityValueAt(t, client, store.generationKey(scope, lookup.IdentityFP))
	if bumped == lookup.Generation {
		t.Fatalf("失效必须推进 generation")
	}
	if exists, _ := client.Exists(ctx, bindingKey).Result(); exists != 0 {
		t.Errorf("失效应删除已登记的绑定键")
	}

	// 失效后在途的写回仍会被拒；重新查找拿到新 generation 后才可写。
	if store.Put(ctx, scope, tip, 7, lookup.IdentityFP, lookup.Generation) {
		t.Errorf("已失效的 generation 不得复活绑定")
	}
	next, ok := store.Lookup(ctx, scope, []string{tip, ancestor})
	if !ok || next.Generation != bumped {
		t.Fatalf("失效后的查找应拿到推进后的 generation，实际 %+v ok=%v", next, ok)
	}
	if !store.Put(ctx, scope, tip, 9, next.IdentityFP, next.Generation) {
		t.Fatalf("新 generation 的写回应成功")
	}
	if value := affinityValueAt(t, client, bindingKey); value != "1|9|"+next.IdentityFP+"|"+next.Generation {
		t.Errorf("新 generation 的绑定值不符: %q", value)
	}
}

// 墓碑写入、短 TTL、以及墓碑对旧 generation 的拒绝。
func TestIntegrationAffinityTombstoneIsShortLivedAndFenced(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	scope := affinityScope(t, client)
	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 600,
		GenerationToken:   counterToken(),
	})

	tip, ancestor := hash32(scope+"-tip"), hash32(scope+"-ancestor")
	lookup, ok := store.Lookup(ctx, scope, []string{tip, ancestor})
	if !ok || lookup.Hint != nil {
		t.Fatalf("查找应可用且未命中: %+v", lookup)
	}
	if !store.Put(ctx, scope, tip, 7, lookup.IdentityFP, lookup.Generation) {
		t.Fatalf("写回应成功")
	}

	if !store.Tombstone(ctx, scope, tip, "failover", lookup.IdentityFP, lookup.Generation) {
		t.Fatalf("墓碑应写入")
	}
	bindingKey := store.bindingKey(scope, tip)
	if value := affinityValueAt(t, client, bindingKey); !strings.HasPrefix(value, "0|failover|") {
		t.Errorf("墓碑值不符: %q", value)
	}
	assertTTLBetween(t, client, bindingKey, 50*time.Second, time.Duration(affinityTombstoneTTLSeconds)*time.Second)

	// 墓碑被查找跳过：没有更浅的绑定，故本次未命中。
	after, ok := store.Lookup(ctx, scope, []string{tip, ancestor})
	if !ok || after.Hint != nil {
		t.Fatalf("墓碑应被跳过并继续向浅，实际 %+v ok=%v", after.Hint, ok)
	}

	// 旧 generation 的墓碑同样被拒（防止迟到的失败请求覆盖新绑定）。
	if store.Tombstone(ctx, scope, tip, "failover", lookup.IdentityFP, "v3:stale") {
		t.Errorf("旧 generation 的墓碑必须被拒")
	}
	// 墓碑原因超长时截断到 32 字符（Node 的 slice(0, 32)）。
	if !store.Tombstone(ctx, scope, tip, strings.Repeat("x", 64), lookup.IdentityFP, lookup.Generation) {
		t.Fatalf("墓碑应再次写入")
	}
	if value := affinityValueAt(t, client, bindingKey); value != "0|"+strings.Repeat("x", 32)+"|"+lookup.IdentityFP+"|"+lookup.Generation {
		t.Errorf("墓碑原因应截断到 32 字符: %q", value)
	}
}

// 并发下 fence 的唯一性：同一 generation 只有持有者能写，旧 generation 并发写回全部被拒。
func TestIntegrationAffinityConcurrentPutOnlyCurrentGenerationWins(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	scope := affinityScope(t, client)
	store := NewAffinityStore(AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 120,
		GenerationToken:   counterToken(),
	})

	tip, ancestor := hash32(scope+"-tip"), hash32(scope+"-ancestor")
	lookup, ok := store.Lookup(ctx, scope, []string{tip, ancestor})
	if !ok || lookup.Hint != nil {
		t.Fatalf("查找应可用且未命中: %+v", lookup)
	}

	const rounds = 8
	var wg sync.WaitGroup
	accepted := make([]bool, rounds*2)
	for index := 0; index < rounds; index++ {
		wg.Add(2)
		go func(slot int) {
			defer wg.Done()
			accepted[slot] = store.Put(ctx, scope, tip, 7, lookup.IdentityFP, lookup.Generation)
		}(index * 2)
		go func(slot int) {
			defer wg.Done()
			accepted[slot] = store.Put(ctx, scope, tip, 8, lookup.IdentityFP, "v3:stale")
		}(index*2 + 1)
	}
	wg.Wait()

	for slot, wrote := range accepted {
		wantAccepted := slot%2 == 0
		if wrote != wantAccepted {
			t.Errorf("第 %d 次写回 accepted=%v，期望 %v（旧 generation 必须被拒）", slot, wrote, wantAccepted)
		}
	}
	if value := affinityValueAt(t, client, store.bindingKey(scope, tip)); value != "1|7|"+lookup.IdentityFP+"|"+lookup.Generation {
		t.Errorf("最终绑定必须来自当前 generation 的写入: %q", value)
	}
}
