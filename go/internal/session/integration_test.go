package session

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// 集成测试门控：CCH_TEST_REDIS_URL 未设置时整组跳过。
// Redis 固定落 DB >= 13（仓库隔离纪律），每个用例自带唯一会话 id，跑完删除自己写的键。
const (
	testRedisEnv   = "CCH_TEST_REDIS_URL"
	testRedisMinDB = 13
	// testTTLSeconds 用短 TTL 让「续期是否真的发生」可在容差内断言。
	testTTLSeconds            = 60
	testTTLToleranceSec       = 3
	testSessionIDPrefix       = "sess_gotest_"
	testKeyID           int64 = 42
)

// testRedis 建一个真实 Redis 连接（DB >= 13）。
func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	raw := os.Getenv(testRedisEnv)
	if raw == "" {
		t.Skipf("未设置 %s，跳过 session 集成测试", testRedisEnv)
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", testRedisEnv, err)
	}
	if options.DB < testRedisMinDB {
		t.Fatalf("%s 必须使用 DB index >= %d，收到 %d", testRedisEnv, testRedisMinDB, options.DB)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("连接 Redis 失败：%v", err)
	}
	return client
}

// newTestBinder 组装「真实 Redis + 真实脚本表」的绑定门面，并登记本用例的键清理。
func newTestBinder(t *testing.T, rdb *redis.Client) *Binder {
	t.Helper()
	registry, err := ratelimit.Load()
	if err != nil {
		t.Fatalf("加载脚本注册表失败: %v", err)
	}
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	return NewBinder(client)
}

// uniqueSessionID 生成用例专属会话 id，避免与上一次运行的残留状态相互污染。
func uniqueSessionID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s%s_%d", testSessionIDPrefix, t.Name(), time.Now().UnixNano())
}

// cleanupSessionKeys 删除本用例可能写下的全部键。
func cleanupSessionKeys(t *testing.T, rdb *redis.Client, sessionID string, keyID int64) {
	t.Helper()
	keys := []string{
		BuildBindingKeys(sessionID, keyID).Canonical,
		BuildBindingKeys(sessionID, keyID).LegacyProvider,
		BuildBindingKeys(sessionID, keyID).LegacyOwner,
		ProviderCooldownKey(sessionID, keyID, 9),
		DiscoveryLeaseKey(sessionID, keyID),
		SeqKey(sessionID),
		LastSeenKey(sessionID),
		InfoKey(sessionID),
		ResponseBodyGenerationKey(sessionID),
		RequestOwnerKey(sessionID, 1),
		RequestOwnerKey(sessionID, 2),
		ObservedConcurrentCountKey(sessionID),
		ObservedGlobalActiveSessionsKey(),
		ActiveSessionsGlobalKey(),
		KeyActiveSessionsKey(keyID),
		UserActiveSessionsKey(7),
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rdb.Del(ctx, keys...).Err()
	})
}

// assertTTLWithin 断言键的剩余 TTL 落在容差内（吸收执行与读 TTL 之间的真实时间流逝）。
func assertTTLWithin(t *testing.T, rdb *redis.Client, key string, wantSeconds int) {
	t.Helper()
	ctx := context.Background()
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读 %s 的 TTL 失败: %v", key, err)
	}
	seconds := int(ttl / time.Second)
	if seconds <= wantSeconds-testTTLToleranceSec || seconds > wantSeconds {
		t.Fatalf("%s 的 TTL 越界: got=%ds want≈%ds", key, seconds, wantSeconds)
	}
}

// TestBindingLifecycle 覆盖「创建 → fence 拒绝迟到写入 → 更新 → 续期 → 清空 → 终结」全链，
// 每步都断言 Redis 侧的键终态，而不只看返回值。
func TestBindingLifecycle(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	keys := BuildBindingKeys(sessionID, testKeyID)

	// 1) 全新会话：创建 canonical 与 legacy owner 镜像，不带 provider。
	created, err := binder.ReadOrReconcile(ctx, sessionID, testKeyID, testTTLSeconds)
	if err != nil {
		t.Fatalf("ReadOrReconcile 失败: %v", err)
	}
	if !created.OK || created.Source != SourceCreated {
		t.Fatalf("全新会话应创建绑定: %+v", created)
	}
	if created.Snapshot.ProviderID != 0 {
		t.Fatalf("全新会话不应带 provider: %+v", created.Snapshot)
	}
	generation := created.Snapshot.Generation
	if generation == "" {
		t.Fatal("创建结果缺少 generation")
	}

	fields, err := rdb.HGetAll(ctx, keys.Canonical).Result()
	if err != nil {
		t.Fatalf("读 canonical 失败: %v", err)
	}
	if fields["key_id"] != strconv.FormatInt(testKeyID, 10) || fields["generation"] != generation {
		t.Fatalf("canonical 内容不符: %+v", fields)
	}
	if _, ok := fields["provider_id"]; ok {
		t.Fatalf("全新会话的 canonical 不应有 provider_id: %+v", fields)
	}
	if owner, err := rdb.Get(ctx, keys.LegacyOwner).Result(); err != nil || owner != strconv.FormatInt(testKeyID, 10) {
		t.Fatalf("legacy owner 镜像不符: owner=%q err=%v", owner, err)
	}
	assertTTLWithin(t, rdb, keys.Canonical, testTTLSeconds)

	// 2) 幂等重读：续期后返回 existing，generation 不变。
	existing, err := binder.ReadOrReconcile(ctx, sessionID, testKeyID, testTTLSeconds)
	if err != nil {
		t.Fatalf("重复 ReadOrReconcile 失败: %v", err)
	}
	if !existing.OK || existing.Source != SourceExisting {
		t.Fatalf("重复读取应命中 existing: %+v", existing)
	}
	if existing.Snapshot.Generation != generation {
		t.Fatalf("幂等读取不应旋转代际: got=%q want=%q", existing.Snapshot.Generation, generation)
	}

	// 3) generation fence：迟到的写入必须被拒，且不得改动任何键。
	stale, err := binder.CompareAndSet(ctx, sessionID, testKeyID, "stale-generation", 9, testTTLSeconds)
	if err != nil {
		t.Fatalf("过期代际的 CAS 不应返回 error: %v", err)
	}
	if stale.OK || stale.ConflictReason != "generation_mismatch" {
		t.Fatalf("过期代际应被 fence 拒绝: %+v", stale)
	}
	afterStale, err := rdb.HGetAll(ctx, keys.Canonical).Result()
	if err != nil {
		t.Fatalf("读 canonical 失败: %v", err)
	}
	if afterStale["generation"] != generation {
		t.Fatalf("被拒绝的写入改动了代际: %+v", afterStale)
	}

	// 4) 正确代际的 CAS：绑定供应商并旋转代际。
	updated, err := binder.CompareAndSet(ctx, sessionID, testKeyID, generation, 9, testTTLSeconds)
	if err != nil {
		t.Fatalf("CAS 失败: %v", err)
	}
	if !updated.OK || updated.Source != SourceUpdated || updated.Snapshot.ProviderID != 9 {
		t.Fatalf("绑定供应商失败: %+v", updated)
	}
	boundGeneration := updated.Snapshot.Generation
	if boundGeneration == generation {
		t.Fatal("CAS 应旋转 generation")
	}
	boundFields, err := rdb.HGetAll(ctx, keys.Canonical).Result()
	if err != nil {
		t.Fatalf("读 canonical 失败: %v", err)
	}
	if boundFields["provider_id"] != "9" || boundFields["generation"] != boundGeneration {
		t.Fatalf("canonical 未记录绑定: %+v", boundFields)
	}
	if provider, err := rdb.Get(ctx, keys.LegacyProvider).Result(); err != nil || provider != "9" {
		t.Fatalf("legacy provider 镜像不符: provider=%q err=%v", provider, err)
	}

	// 5) 续期：不改代际，只刷 TTL；期望供应商不符则冲突。
	mismatch, err := binder.Touch(ctx, sessionID, testKeyID, boundGeneration, 8, testTTLSeconds)
	if err != nil {
		t.Fatalf("期望供应商不符的 Touch 不应返回 error: %v", err)
	}
	if mismatch.OK || mismatch.ConflictReason != "provider_mismatch" {
		t.Fatalf("期望供应商不符应冲突: %+v", mismatch)
	}

	touched, err := binder.Touch(ctx, sessionID, testKeyID, boundGeneration, 9, testTTLSeconds)
	if err != nil {
		t.Fatalf("Touch 失败: %v", err)
	}
	if !touched.OK || touched.Source != SourceTouched {
		t.Fatalf("续期失败: %+v", touched)
	}
	if touched.Snapshot.Generation != boundGeneration || touched.Snapshot.ProviderID != 9 {
		t.Fatalf("续期不应改变绑定: %+v", touched.Snapshot)
	}
	assertTTLWithin(t, rdb, keys.Canonical, testTTLSeconds)

	// 6) 清空绑定并写供应商冷却：provider 消失、冷却键带新代际与独立 TTL。
	cooldownTTL := 30
	cleared, err := binder.Clear(ctx, sessionID, testKeyID, boundGeneration, 9, 9, cooldownTTL, testTTLSeconds)
	if err != nil {
		t.Fatalf("Clear 失败: %v", err)
	}
	if !cleared.OK || cleared.Source != SourceCleared || cleared.Snapshot.ProviderID != 0 {
		t.Fatalf("清空失败: %+v", cleared)
	}
	clearedFields, err := rdb.HGetAll(ctx, keys.Canonical).Result()
	if err != nil {
		t.Fatalf("读 canonical 失败: %v", err)
	}
	if _, ok := clearedFields["provider_id"]; ok {
		t.Fatalf("清空后 canonical 不应有 provider_id: %+v", clearedFields)
	}
	if _, err := rdb.Get(ctx, keys.LegacyProvider).Result(); err != redis.Nil {
		t.Fatalf("清空后 legacy provider 镜像应被删除: %v", err)
	}
	cooldownValue, err := rdb.Get(ctx, ProviderCooldownKey(sessionID, testKeyID, 9)).Result()
	if err != nil {
		t.Fatalf("冷却键应存在: %v", err)
	}
	if cooldownValue != cleared.Snapshot.Generation {
		t.Fatalf("冷却键应记录清空后的代际: got=%q want=%q", cooldownValue, cleared.Snapshot.Generation)
	}
	assertTTLWithin(t, rdb, ProviderCooldownKey(sessionID, testKeyID, 9), cooldownTTL)

	// 7) 终结：清掉绑定，不影响冷却键。
	terminated, err := binder.Terminate(ctx, sessionID, testKeyID, 0, testTTLSeconds)
	if err != nil {
		t.Fatalf("Terminate 失败: %v", err)
	}
	if !terminated.OK || terminated.Source != SourceTerminated {
		t.Fatalf("终结失败: %+v", terminated)
	}
	if _, err := rdb.Get(ctx, ProviderCooldownKey(sessionID, testKeyID, 9)).Result(); err != nil {
		t.Fatalf("终结不应删除冷却键: %v", err)
	}
}

// TestReadOrReconcileUpgradesLegacyMirror 覆盖「只有 legacy 镜像、没有 canonical」的升级路径：
// 切换期 Node 写下的旧键必须能被 Go 接管，否则灰度期间每个会话都会新建绑定。
func TestReadOrReconcileUpgradesLegacyMirror(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	keys := BuildBindingKeys(sessionID, testKeyID)

	// 造出 Node 形态的旧键：只有 legacy owner + provider 镜像。
	if err := rdb.Set(ctx, keys.LegacyOwner, strconv.FormatInt(testKeyID, 10), testTTLSeconds*time.Second).Err(); err != nil {
		t.Fatalf("写 legacy owner 失败: %v", err)
	}
	if err := rdb.Set(ctx, keys.LegacyProvider, "9", testTTLSeconds*time.Second).Err(); err != nil {
		t.Fatalf("写 legacy provider 失败: %v", err)
	}

	upgraded, err := binder.ReadOrReconcile(ctx, sessionID, testKeyID, testTTLSeconds)
	if err != nil {
		t.Fatalf("升级读取失败: %v", err)
	}
	if !upgraded.OK || upgraded.Source != SourceLegacyUpgraded {
		t.Fatalf("应升级旧镜像: %+v", upgraded)
	}
	if upgraded.Snapshot.ProviderID != 9 {
		t.Fatalf("升级应继承原 provider: %+v", upgraded.Snapshot)
	}
	fields, err := rdb.HGetAll(ctx, keys.Canonical).Result()
	if err != nil {
		t.Fatalf("读 canonical 失败: %v", err)
	}
	if fields["provider_id"] != "9" {
		t.Fatalf("升级后 canonical 应带 provider: %+v", fields)
	}
}

// TestReadOrReconcileForeignLegacyOwner 钉住租户隔离：legacy owner 属于别的密钥即冲突，
// 不得因此改写 canonical。
func TestReadOrReconcileForeignLegacyOwner(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	keys := BuildBindingKeys(sessionID, testKeyID)

	if err := rdb.Set(ctx, keys.LegacyOwner, "999", testTTLSeconds*time.Second).Err(); err != nil {
		t.Fatalf("写 legacy owner 失败: %v", err)
	}

	result, err := binder.ReadOrReconcile(ctx, sessionID, testKeyID, testTTLSeconds)
	if err != nil {
		t.Fatalf("外来 owner 不应返回 error: %v", err)
	}
	if result.OK || result.ConflictReason != "foreign_legacy_owner" {
		t.Fatalf("外来 owner 应冲突: %+v", result)
	}
	if exists, err := rdb.Exists(ctx, keys.Canonical).Result(); err != nil || exists != 0 {
		t.Fatalf("冲突时不应创建 canonical: exists=%d err=%v", exists, err)
	}
}

// TestConcurrentCASHasSingleWinner 覆盖并发绑定只一个赢：同一期望代际下 N 个写者只有一个
// 成功，其余必须拿到 generation_mismatch（而非超时或 500）。
func TestConcurrentCASHasSingleWinner(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)

	created, err := binder.ReadOrReconcile(ctx, sessionID, testKeyID, testTTLSeconds)
	if err != nil {
		t.Fatalf("ReadOrReconcile 失败: %v", err)
	}
	generation := created.Snapshot.Generation

	const writers = 16
	results := make([]BindingResult, writers)
	errs := make([]error, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start // 用闭锁让 N 个写者尽量同时进入，而不是靠 sleep 对齐
			results[index], errs[index] = binder.CompareAndSet(
				ctx, sessionID, testKeyID, generation, int64(index+1), testTTLSeconds)
		}(i)
	}
	close(start)
	wg.Wait()

	winners, mismatches := 0, 0
	for i := 0; i < writers; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个写者返回 error: %v", i, errs[i])
		}
		switch {
		case results[i].OK:
			winners++
		case results[i].ConflictReason == "generation_mismatch":
			mismatches++
		default:
			t.Fatalf("第 %d 个写者得到意外结果: %+v", i, results[i])
		}
	}
	if winners != 1 {
		t.Fatalf("并发写入应有且仅有一个赢家: winners=%d", winners)
	}
	if mismatches != writers-1 {
		t.Fatalf("其余写者应全部被 fence 拒绝: mismatches=%d", mismatches)
	}
}

// TestLeaseAcquireRenewRelease 覆盖 discovery 租约的抢占、续期与释放，含 token 不匹配路径。
func TestLeaseAcquireRenewRelease(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	leaseKey := DiscoveryLeaseKey(sessionID, testKeyID)

	acquired, err := binder.AcquireLease(ctx, sessionID, testKeyID, "", testTTLSeconds)
	if err != nil {
		t.Fatalf("抢租约失败: %v", err)
	}
	if !acquired.Acquired || acquired.OwnerToken == "" {
		t.Fatalf("抢租约应成功并给出 token: %+v", acquired)
	}
	if holder, err := rdb.Get(ctx, leaseKey).Result(); err != nil || holder != acquired.OwnerToken {
		t.Fatalf("租约键未记录 owner: holder=%q err=%v", holder, err)
	}
	assertTTLWithin(t, rdb, leaseKey, testTTLSeconds)

	second, err := binder.AcquireLease(ctx, sessionID, testKeyID, "", testTTLSeconds)
	if err != nil {
		t.Fatalf("重复抢租约不应返回 error: %v", err)
	}
	if second.Acquired || !second.Conflict {
		t.Fatalf("租约被持有时应报冲突: %+v", second)
	}

	lost, err := binder.RenewLease(ctx, sessionID, testKeyID, "wrong-token", testTTLSeconds)
	if err != nil {
		t.Fatalf("错误 token 续期不应返回 error: %v", err)
	}
	if lost.OK || !lost.Lost {
		t.Fatalf("错误 token 续期应报丢失: %+v", lost)
	}

	renewed, err := binder.RenewLease(ctx, sessionID, testKeyID, acquired.OwnerToken, testTTLSeconds)
	if err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	if !renewed.OK || renewed.Lost {
		t.Fatalf("续期应成功: %+v", renewed)
	}
	assertTTLWithin(t, rdb, leaseKey, testTTLSeconds)

	if result, err := binder.ReleaseLease(ctx, sessionID, testKeyID, "wrong-token"); err != nil || result.OK {
		t.Fatalf("错误 token 释放应报丢失: %+v err=%v", result, err)
	}

	released, err := binder.ReleaseLease(ctx, sessionID, testKeyID, acquired.OwnerToken)
	if err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if !released.OK || released.Lost {
		t.Fatalf("释放应成功: %+v", released)
	}
	if exists, err := rdb.Exists(ctx, leaseKey).Result(); err != nil || exists != 0 {
		t.Fatalf("释放后租约键应消失: exists=%d err=%v", exists, err)
	}
}

// TestEvalParsersSplitByReplyShape 钉住契约防御：租约脚本的整数回复只能走整数解析，
// 数组解析必须明确报错而不是静默取到零值——否则「续期失败」会被读成「续期成功」。
func TestEvalParsersSplitByReplyShape(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	leaseKey := "session:gotest:reply-shape"
	t.Cleanup(func() { _ = rdb.Del(context.Background(), leaseKey).Err() })

	// 租约不存在：续期脚本返回整数 0。
	value, err := binder.client.evalInt(ctx, "RENEW_SESSION_DISCOVERY_LEASE",
		[]string{leaseKey}, []any{"token", "10"})
	if err != nil {
		t.Fatalf("整数回复应能被整数解析器读出: %v", err)
	}
	if value != 0 {
		t.Fatalf("租约不存在时续期应返回 0: got=%d", value)
	}

	if _, err := binder.client.eval(ctx, "RENEW_SESSION_DISCOVERY_LEASE",
		[]string{leaseKey}, []any{"token", "10"}); err == nil {
		t.Fatal("整数回复不应被当作数组解析成功")
	}
}

// TestTrackerWritesActiveSessions 覆盖活跃会话 ZSET、观测集合与最后活动时间的写入与清理。
func TestTrackerWritesActiveSessions(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)

	if err := binder.TrackSession(ctx, sessionID, testKeyID, 7); err != nil {
		t.Fatalf("TrackSession 失败: %v", err)
	}
	for _, key := range []string{ActiveSessionsGlobalKey(), KeyActiveSessionsKey(testKeyID), UserActiveSessionsKey(7)} {
		if score, err := rdb.ZScore(ctx, key, sessionID).Result(); err != nil || score <= 0 {
			t.Fatalf("活跃集 %s 未加入会话: score=%v err=%v", key, score, err)
		}
		assertTTLWithin(t, rdb, key, int(sessionZSetHostTTL/time.Second))
	}

	// 无用户维度的调用不得写用户键。
	otherSession := uniqueSessionID(t) + "_nouser"
	if err := binder.TrackSession(ctx, otherSession, testKeyID, 0); err != nil {
		t.Fatalf("TrackSession 失败: %v", err)
	}
	if _, err := rdb.ZScore(ctx, UserActiveSessionsKey(7), otherSession).Result(); err != redis.Nil {
		t.Fatalf("无用户维度时不应写用户活跃集: %v", err)
	}
	t.Cleanup(func() {
		_ = rdb.ZRem(ctx, ActiveSessionsGlobalKey(), otherSession).Err()
		_ = rdb.ZRem(ctx, KeyActiveSessionsKey(testKeyID), otherSession).Err()
	})

	if err := binder.MarkLastSeen(ctx, sessionID, time.Duration(testTTLSeconds)*time.Second); err != nil {
		t.Fatalf("MarkLastSeen 失败: %v", err)
	}
	if _, err := rdb.Get(ctx, LastSeenKey(sessionID)).Result(); err != nil {
		t.Fatalf("未写入最后活动时间: %v", err)
	}
	assertTTLWithin(t, rdb, LastSeenKey(sessionID), testTTLSeconds)

	observed := sessionID + "_observed"
	t.Cleanup(func() { _, _ = binder.TerminateObservedSession(ctx, observed) })
	if err := binder.TrackObservedSession(ctx, observed); err != nil {
		t.Fatalf("TrackObservedSession 失败: %v", err)
	}
	if score, err := rdb.ZScore(ctx, ObservedGlobalActiveSessionsKey(), observed).Result(); err != nil || score <= 0 {
		t.Fatalf("观测集未加入会话: score=%v err=%v", score, err)
	}

	// info 键由会话详情路径写入（Node 同样只在此处 EXPIRE，不建键），先用一个短 TTL 造出来，
	// 用来断言续期真的把窗口推到了会话 TTL。
	if err := rdb.Set(ctx, InfoKey(observed), "{}", 5*time.Second).Err(); err != nil {
		t.Fatalf("造 info 键失败: %v", err)
	}
	if err := binder.RefreshObservedSession(ctx, observed, time.Duration(testTTLSeconds)*time.Second); err != nil {
		t.Fatalf("RefreshObservedSession 失败: %v", err)
	}
	assertTTLWithin(t, rdb, InfoKey(observed), testTTLSeconds)

	if err := rdb.Set(ctx, ObservedConcurrentCountKey(observed), "3", time.Minute).Err(); err != nil {
		t.Fatalf("造并发计数失败: %v", err)
	}
	removed, err := binder.TerminateObservedSession(ctx, observed)
	if err != nil {
		t.Fatalf("TerminateObservedSession 失败: %v", err)
	}
	if !removed {
		t.Fatal("终止应移除观测集成员")
	}
	for _, key := range []string{ObservedConcurrentCountKey(observed), InfoKey(observed)} {
		if exists, err := rdb.Exists(ctx, key).Result(); err != nil || exists != 0 {
			t.Fatalf("终止后 %s 应被清理: exists=%d err=%v", key, exists, err)
		}
	}

	// 再次终止：成员已不在，返回 false（Node 语义）。
	if removed, err := binder.TerminateObservedSession(ctx, observed); err != nil || removed {
		t.Fatalf("重复终止应返回未移除: removed=%v err=%v", removed, err)
	}
}

// TestNextSequenceIsUniqueUnderConcurrency 覆盖会话内请求序号的原子分配：
// 并发请求必须拿到互不相同的正数序号，否则请求工件键会互相覆盖。
func TestNextSequenceIsUniqueUnderConcurrency(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	adapter := NewSessionBinderAdapter(BinderOptions{Client: binder, TTL: time.Duration(testTTLSeconds) * time.Second})
	ctx := context.Background()
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)

	const callers = 32
	sequences := make([]int, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			sequences[index], errs[index] = adapter.NextSequence(ctx, sessionID, testKeyID)
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[int]bool, callers)
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 次取序号失败: %v", i, errs[i])
		}
		if sequences[i] <= 0 {
			t.Fatalf("第 %d 次取到非正序号: %d", i, sequences[i])
		}
		if seen[sequences[i]] {
			t.Fatalf("序号重复: %d", sequences[i])
		}
		seen[sequences[i]] = true
	}
	if len(seen) != callers {
		t.Fatalf("序号数不符: got=%d want=%d", len(seen), callers)
	}
}

// TestEnsureReusesClientSessionAndDegradesWithoutRedis 覆盖守卫链适配的两条主路径：
// 有 Redis 时复用客户端携带的会话 id 并推进序号；无 Redis 时降级为每请求独立会话。
func TestEnsureReusesClientSessionAndDegradesWithoutRedis(t *testing.T) {
	ctx := context.Background()
	sessionID := "sess_gotest_ensure_fixed"
	body := map[string]any{
		"metadata": map[string]any{"user_id": `{"session_id":"` + sessionID + `"}`},
	}

	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	adapter := NewSessionBinderAdapter(BinderOptions{Client: binder, TTL: time.Duration(testTTLSeconds) * time.Second})

	first, err := adapter.Ensure(ctx, guardSessionRequest(body))
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if first.SessionID != sessionID {
		t.Fatalf("应复用客户端会话 id: got=%q want=%q", first.SessionID, sessionID)
	}
	second, err := adapter.Ensure(ctx, guardSessionRequest(body))
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if second.Sequence != first.Sequence+1 {
		t.Fatalf("序号应递增: first=%d second=%d", first.Sequence, second.Sequence)
	}
	if _, err := rdb.Get(ctx, LastSeenKey(sessionID)).Result(); err != nil {
		t.Fatalf("应刷新最后活动时间: %v", err)
	}

	// 无 Redis：不得报错，会话退化为每请求独立。
	degraded := NewSessionBinderAdapter(BinderOptions{Client: NewBinder(nil)})
	result, err := degraded.Ensure(ctx, guardSessionRequest(body))
	if err != nil {
		t.Fatalf("降级路径不应返回 error: %v", err)
	}
	if result.SessionID == "" || result.SessionID == sessionID {
		t.Fatalf("降级应生成新会话: %+v", result)
	}
	if result.Sequence <= 0 {
		t.Fatalf("降级应给出正序号: %+v", result)
	}
}

// TestEnsureFallsBackToContentHash 覆盖无会话身份时的降级复用：同一租户下正文哈希相同
// 的第二次请求应复用首次生成的会话，而不是每次新建。
func TestEnsureFallsBackToContentHash(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	adapter := NewSessionBinderAdapter(BinderOptions{Client: binder, TTL: time.Duration(testTTLSeconds) * time.Second})

	messages := []any{map[string]any{"role": "user", "content": "内容哈希降级用例"}}
	body := map[string]any{"messages": messages}
	hash := CalculateMessagesHash(messages)
	if hash == "" {
		t.Fatal("用例正文应有可用哈希")
	}
	mappingKey := TenantContentHashSessionKey(testKeyID, hash)
	t.Cleanup(func() {
		sessionID, _ := rdb.Get(ctx, mappingKey).Result()
		_ = rdb.Del(ctx, mappingKey).Err()
		if sessionID != "" {
			cleanupSessionKeys(t, rdb, sessionID, testKeyID)
		}
	})

	first, err := adapter.Ensure(ctx, guardSessionRequest(body))
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if first.SessionID == "" {
		t.Fatal("应生成会话 id")
	}
	if mapped, err := rdb.Get(ctx, mappingKey).Result(); err != nil || mapped != first.SessionID {
		t.Fatalf("应记录正文哈希映射: mapped=%q err=%v", mapped, err)
	}

	second, err := adapter.Ensure(ctx, guardSessionRequest(body))
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("同正文哈希应复用会话: first=%q second=%q", first.SessionID, second.SessionID)
	}
}

// TestEnsureIgnoresForeignTenantHashMapping 钉住租户隔离：哈希映射存在但 legacy owner
// 属于别的密钥时必须当作未命中并新建会话，绝不能把别的租户的会话复用过来。
func TestEnsureIgnoresForeignTenantHashMapping(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	ctx := context.Background()
	adapter := NewSessionBinderAdapter(BinderOptions{Client: binder, TTL: time.Duration(testTTLSeconds) * time.Second})

	messages := []any{map[string]any{"role": "user", "content": "租户隔离用例"}}
	body := map[string]any{"messages": messages}
	hash := CalculateMessagesHash(messages)
	if hash == "" {
		t.Fatal("用例正文应有可用哈希")
	}

	foreignSession := uniqueSessionID(t) + "_foreign"
	mappingKey := TenantContentHashSessionKey(testKeyID, hash)
	cleanupSessionKeys(t, rdb, foreignSession, testKeyID)
	t.Cleanup(func() {
		_ = rdb.Del(ctx, mappingKey).Err()
		cleanupSessionKeys(t, rdb, foreignSession, 999)
	})

	// 造出「映射指向外来会话」的状态：owner 镜像写另一个密钥 id。
	if err := rdb.Set(ctx, mappingKey, foreignSession, time.Duration(testTTLSeconds)*time.Second).Err(); err != nil {
		t.Fatalf("写哈希映射失败: %v", err)
	}
	if err := rdb.Set(ctx, BuildBindingKeys(foreignSession, 999).LegacyOwner,
		"999", time.Duration(testTTLSeconds)*time.Second).Err(); err != nil {
		t.Fatalf("写外来 owner 失败: %v", err)
	}

	result, err := adapter.Ensure(ctx, guardSessionRequest(body))
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if result.SessionID == foreignSession {
		t.Fatalf("不得复用别的租户的会话: %q", result.SessionID)
	}
	if result.SessionID == "" || result.Sequence <= 0 {
		t.Fatalf("应新建会话并给出序号: %+v", result)
	}
	cleanupSessionKeys(t, rdb, result.SessionID, testKeyID)
}
